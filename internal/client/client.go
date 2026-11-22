package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NemoNobody/velruna/internal/config"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// defaultRequestTimeout bounds each HTTP round-trip so the CLI fails fast
// instead of hanging forever when OpenSearch is unreachable.
const defaultRequestTimeout = 30 * time.Second

// ErrNotFound is returned when a fact with the given key does not exist.
var ErrNotFound = errors.New("fact not found")

// ErrContract marks a rejection that never touched the wire: the request
// violates the local contract (summary bounds, re-retiring with a reason)
// no matter how healthy the store is. Edge layers must answer with the
// message itself, never wrap it in availability advice.
var ErrContract = errors.New("rejected before store contact")

// ErrConflict reports that a concurrent writer changed the fact between
// this call's read and its conditional write (if_seq_no/if_primary_term
// CAS). The operation was valid; the snapshot it was computed from is
// stale. The agent's contract is to re-read and retry — NOT to stop (as
// store-availability advice would say).
var ErrConflict = errors.New("concurrent write conflict")

type contractError struct{ err error }

func (e *contractError) Error() string { return e.err.Error() }
func (e *contractError) Unwrap() error { return ErrContract }

// contractErrorf wraps a pre-transport rejection so edges can classify it
// without string-matching the message.
func contractErrorf(format string, args ...interface{}) error {
	return &contractError{fmt.Errorf(format, args...)}
}

// Client wraps the OpenSearch client.
type Client struct {
	api *opensearchapi.Client
	cfg *config.Config

	// The process-wide dedup for the replica reconcile, the author label and the
	// infrastructure passes lives in package variables (replicas.go, author.go,
	// journal.go, mapping.go): the MCP server builds a fresh client per tool
	// call, so per-client state would repeat the work on every call. The fields
	// below only cache a probe for this client's lifetime.

	// replicasOnce/replicasVal cache the auto replica-count probe (see
	// autoReplicas) for this client; the process-wide progress is claimed through
	// reconciledStores in replicas.go. replicasProbeOK distinguishes "probed,
	// lone node" (reconcile may act) from "probe failed" (reconcile must not
	// guess). replicasFixOnce makes the drift correction run at most once for
	// this client (see reconcileReplicas).
	replicasOnce    sync.Once
	replicasVal     int
	replicasProbeOK bool
	replicasFixOnce sync.Once
}

// NewClient creates a new OpenSearch client.
func NewClient(cfg *config.Config) (*Client, error) {
	if cfg.OpenSearchPassword == "" {
		// Fail fast with the migration path spelled out: a 401 from the server
		// would not tell the operator where credentials come from now. When the
		// keyring itself is unusable, say so — "store one with creds set" would
		// send the user into the same failure again.
		if cfg.KeyringError != "" {
			return nil, fmt.Errorf("OpenSearch password is empty: the keyring is unavailable (%s). "+
				"Fix the keyring backend, export OPENSEARCH_PASSWORD, or set allow_plaintext=true in the config", cfg.KeyringError)
		}
		return nil, fmt.Errorf("OpenSearch password is empty: store one with 'velruna creds set' " +
			"(OS keyring), export OPENSEARCH_PASSWORD, or set allow_plaintext=true in the config")
	}
	api, err := opensearchapi.NewClient(opensearchapi.Config{
		Client: opensearch.Config{
			Addresses:          cfg.EffectiveURLs(),
			Username:           cfg.OpenSearchUser,
			Password:           cfg.OpenSearchPassword,
			InsecureSkipVerify: cfg.InsecureSkipVerify,
			RequestTimeout:     defaultRequestTimeout,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create opensearch client: %w", err)
	}
	return &Client{api: api, cfg: cfg}, nil
}

// storeHint turns the first-run failure ("nothing is listening") into an
// instruction. Every other error already names what it failed at; this one is
// the one a new user hits, and the bare dial error does not say how to get a
// store.
func (c *Client) storeHint(err error) error {
	if err == nil {
		return nil
	}
	var netErr net.Error
	if !errors.As(err, &netErr) && !errors.Is(err, syscall.ECONNREFUSED) {
		return err
	}
	return fmt.Errorf("%w\nno OpenSearch answered at %s. Start one with `make dev-opensearch` "+
		"(this repository) or\n  docker run -d -p 9200:9200 -e discovery.type=single-node "+
		"-e DISABLE_SECURITY_PLUGIN=true opensearchproject/opensearch:3.8.0\n"+
		"then point opensearch_url / opensearch_urls at it (docs/setup.md, section 1-2)",
		err, strings.Join(c.cfg.EffectiveURLs(), ", "))
}

// isNotFound reports whether err is an HTTP 404 from OpenSearch. The client
// returns different error types depending on the shape of the response body:
// a plain string error, or a structured one with type/reason/root_cause.
func isNotFound(err error) bool {
	var se *opensearch.StringError
	if errors.As(err, &se) {
		return se.Status == http.StatusNotFound
	}
	var ste *opensearch.StructError
	if errors.As(err, &ste) {
		return ste.Status == http.StatusNotFound
	}
	return false
}

// isAlreadyExists reports OpenSearch's "the index you are creating is already
// there". Two velruna processes on a fresh cluster (two Zed windows, CLI +
// MCP) both pass the HEAD check and race to create the index; the loser must
// treat the collision as success, not as a tool failure.
func isAlreadyExists(err error) bool {
	var ste *opensearch.StructError
	return errors.As(err, &ste) && ste.Err.Type == "resource_already_exists_exception"
}

// isVersionConflict reports OpenSearch's optimistic-concurrency rejection:
// op_type=create on an existing document (or an if_seq_no mismatch). Like
// isNotFound, it answers from either error shape the client can produce.
// The predicate also DECIDES SUCCESS (the deterministic-id journal
// collision), and a middlebox-rewritten 409 must not fake an append that
// never happened — so both shapes delegate to ONE conflictAnswer gate
// (kept shared on purpose: whenever the two branches were patched apart,
// one of them kept re-opening a hole) that requires, before any acceptor
// answers:
//   - the body's declared 409. For classified bodies opensearch-go
//     takes the status from the body (the typed errors carry no
//     transport code; only its final unclassified fallback uses the
//     HTTP code), so the body's self-reported status is the gate — a
//     gateway that rewrites the HTTP code but passes a genuine
//     conflict body through still classifies as conflict (pinned);
//   - no index-creation-race signature anywhere in the body: the race
//     writes NO document, so reading it as a doc collision would fake a
//     journal append and dangle current_ref. The race names
//     resource_already_exists_exception (type or reason, at
//     the top level or in the nested root_cause/caused_by detail) or
//     carries the "index ["+"already exists"
//     marker pair; doc-level collisions name the document id, never
//     "index [".
//
// Acceptors per shape: struct answers from error.type (canonical
// version_conflict_engine_exception) or the reason wording; string
// shape (error.type is gone there) from the wording only. Genuine
// engine collisions name themselves in either shape, across 2.x/3.x
// wording (the acceptor vocabulary — "version conflict", "already
// exists", the exception names — has been stable since 6.x).
func isVersionConflict(err error) bool {
	var ste *opensearch.StructError
	if errors.As(err, &ste) {
		return conflictAnswer(ste.Status, ste.Err.Type, ste.Err.Reason, rootCauseHasRace(ste.Err))
	}
	var se *opensearch.StringError
	return errors.As(err, &se) && conflictAnswer(se.Status, "", se.Err, false)
}

// conflictAnswer is the single shared gate both shapes flow through —
// see isVersionConflict for the why.
func conflictAnswer(status int, typ, text string, raceInNested bool) bool {
	return status == http.StatusConflict &&
		!raceSignature(typ, text) &&
		!raceInNested &&
		(typ == "version_conflict_engine_exception" ||
			strings.Contains(text, "version_conflict") ||
			strings.Contains(text, "version conflict") ||
			strings.Contains(text, "already exists"))
}

// rootCauseHasRace scans the engine's nested error detail — the
// root_cause array and every caused_by chain — for the race
// signature. A middlebox may rewrite the top-level type and reason
// off it while leaving the engine naming resource_already_exists or
// carrying the marker pair in the nested detail.
func rootCauseHasRace(err opensearch.Err) bool {
	for _, rc := range err.RootCause {
		if raceSignature(rc.Type, rc.Reason) {
			return true
		}
	}
	for cb := err.CausedBy; cb != nil; cb = cb.CausedBy {
		if raceSignature(cb.Type, cb.Reason) {
			return true
		}
	}
	return false
}

func raceSignature(typ, reason string) bool {
	return typ == "resource_already_exists_exception" ||
		strings.Contains(reason, "resource_already_exists") ||
		(strings.Contains(reason, "index [") && strings.Contains(reason, "already exists"))
}

// isClusterBlock reports whether the error is OpenSearch refusing a write
// because of a cluster or index block — typically the disk flood stage turning
// indices read-only, or an explicit read-only block.
func isClusterBlock(err error) bool {
	var ste *opensearch.StructError
	if errors.As(err, &ste) {
		return ste.Err.Type == "cluster_block_exception" ||
			strings.Contains(ste.Err.Reason, "read-only")
	}
	var se *opensearch.StringError
	if errors.As(err, &se) {
		return strings.Contains(se.Err, "cluster_block_exception") ||
			strings.Contains(se.Err, "read-only")
	}
	return false
}

// wrapWriteError turns a cluster-side block into an actionable message: the
// caller must know this is not a velruna bug and that retrying will not help. The
// guidance comes first and the raw cluster error last, because the raw dump is
// long (type + reason + root_cause) and would otherwise bury the one thing the
// reader needs to act on.
func wrapWriteError(op string, err error) error {
	if isClusterBlock(err) {
		return fmt.Errorf("%s: the cluster is blocking writes. This is a cluster-side "+
			"block — usually the disk flood stage (a data node above the flood "+
			"watermark) or an explicit read-only block. Free space on the data nodes "+
			"or ask the cluster owner; retrying will not help. Cluster said: %w", op, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// Info checks connectivity and returns cluster information. It is the cheapest
// way to tell whether the store is usable.
func (c *Client) Info(ctx context.Context) (*opensearchapi.InfoResp, error) {
	resp, err := c.api.Info(ctx, nil)
	if err != nil {
		urls := c.cfg.EffectiveURLs()
		redacted := make([]string, 0, len(urls))
		for _, u := range urls {
			redacted = append(redacted, config.RedactURL(u))
		}
		return nil, fmt.Errorf("reach OpenSearch at %s: %w", strings.Join(redacted, ", "), err)
	}
	return resp, nil
}

// URL returns the configured OpenSearch URL.
func (c *Client) URL() string { return c.cfg.OpenSearchURL }

// IndexName returns the configured index name.
func (c *Client) IndexName() string { return c.cfg.IndexName }
