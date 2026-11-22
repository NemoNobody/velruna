package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/model"

	"github.com/google/uuid"
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
		return nil, fmt.Errorf("reach OpenSearch at %s: %w", config.RedactURL(c.cfg.OpenSearchURL), err)
	}
	return resp, nil
}

// URL returns the configured OpenSearch URL.
func (c *Client) URL() string { return c.cfg.OpenSearchURL }

// IndexName returns the configured index name.
func (c *Client) IndexName() string { return c.cfg.IndexName }

// indexReplicas is the replica count stamped on newly created velruna
// indices. With the auto default (config -1) the cluster is asked once per
// process: a single data node can never assign a copy (the index would sit
// yellow forever), so it gets 0; a real cluster gets 1 so losing a node
// loses neither facts nor history. A health probe failure must not fail a
// write — it falls back to 0. Existing indices are realigned separately by
// reconcileReplicas (see replicas.go).
func (c *Client) indexReplicas(ctx context.Context) int {
	if c.cfg.IndexReplicas >= 0 {
		return c.cfg.IndexReplicas
	}
	v, _ := c.autoReplicas(ctx)
	return v
}

// buildIndexBody returns the index creation body (mappings) used by EnsureIndex.
func buildIndexBody(replicas int) map[string]interface{} {
	return map[string]interface{}{
		"settings": map[string]interface{}{
			"index.number_of_replicas": replicas,
		},
		"mappings": map[string]interface{}{
			"properties": buildIndexProperties(),
			"_meta":      schemaMeta(),
		},
	}
}

// buildIndexProperties is the single field catalog: index creation and the
// additive mapping reconcile share it, so a field can never exist in one and
// be forgotten in the other.
func buildIndexProperties() map[string]interface{} {
	return map[string]interface{}{
		"fact_id":     map[string]interface{}{"type": "keyword"},
		"key":         map[string]interface{}{"type": "keyword"},
		"value":       map[string]interface{}{"type": "text"},
		"category":    map[string]interface{}{"type": "keyword"},
		"project":     map[string]interface{}{"type": "keyword"},
		"status":      map[string]interface{}{"type": "keyword"},
		"created_at":  map[string]interface{}{"type": "date"},
		"updated_at":  map[string]interface{}{"type": "date"},
		"observed_at": map[string]interface{}{"type": "date"},
		"ttl_days":    map[string]interface{}{"type": "integer"},
		"keywords":    map[string]interface{}{"type": "keyword"},
		// Verifications of the current value: stored for reads, not
		// indexed (the journal is where confirmations are queried).
		"confirmed_by": map[string]interface{}{"type": "object", "enabled": false},
		"source":       map[string]interface{}{"type": "keyword"},
		"notes":        map[string]interface{}{"type": "text"},
		// Summary is the table-of-contents rendering of value: searchable
		// text, so listings and BM25 can work from it.
		"summary": map[string]interface{}{"type": "text"},
		// Stored, never indexed: legacy history arrays predate the journal
		// and are read with the fact, not searched.
		"history": map[string]interface{}{"type": "object", "enabled": false},
		// Journal entry id of the current value (see model.Fact.CurrentRef).
		"current_ref": map[string]interface{}{"type": "keyword"},
		// Authorship labels (self-reported; see model.Fact): exact-match
		// fields for "who wrote what" queries, never full-text.
		"author":        map[string]interface{}{"type": "keyword"},
		"author_source": map[string]interface{}{"type": "keyword"},
		"model":         map[string]interface{}{"type": "keyword"},
		"host":          map[string]interface{}{"type": "keyword"},
		"session":       map[string]interface{}{"type": "keyword"},
		"updated_by":    map[string]interface{}{"type": "keyword"},
	}
}

// EnsureIndex creates the index when it is missing.
//
// Existence is checked with a read-only request first, and the create is skipped
// when the index is already there. Creating unconditionally would need a write
// and would therefore fail on a read-only index — breaking read commands
// (`get`, `search`, `list`), which must keep working even when the cluster has
// blocked writes.
func (c *Client) EnsureIndex(ctx context.Context) error {
	existsReq := opensearchapi.IndicesExistsReq{Indices: []string{c.cfg.IndexName}}
	resp, err := c.api.Indices.Exists(ctx, existsReq)
	switch {
	case err == nil:
		// A store written by a newer binary is refused before anything is
		// written; a store from an older one is upgraded in place by the
		// mapping reconcile below.
		if serr := c.ensureSchemaVersion(ctx); serr != nil {
			return serr
		}
		c.reconcileReplicas(ctx)
		// Best effort, like the replica reconcile: a mapping the cluster
		// refuses (a field already exists with another type) must not break
		// reads, and the once-guard stays open so a later run retries.
		_ = c.reconcileMapping(ctx)
		return nil
	case resp != nil && resp.StatusCode == http.StatusNotFound:
		// The index is missing: create it below. The status code is checked
		// instead of the error type because a HEAD response has no body, so the
		// client cannot parse it into a typed error.
	default:
		return c.storeHint(fmt.Errorf("check index %s: %w", c.cfg.IndexName, err))
	}

	body, _ := json.Marshal(buildIndexBody(c.indexReplicas(ctx)))
	if _, err := c.api.Indices.Create(ctx, opensearchapi.IndicesCreateReq{
		Index: c.cfg.IndexName,
		Body:  bytes.NewReader(body),
	}); err != nil {
		if isAlreadyExists(err) {
			return nil // lost the create race to a concurrent process: the index is there
		}
		return wrapWriteError("create index", err)
	}
	return nil
}

// addCorrectionScript records a value change on the main fact doc: the new
// value replaces the old, and current_ref points at the journal entry that
// was appended for it (the journal append happens BEFORE this script: the
// journal is the log, the main doc is the projection, and an orphan journal
// entry from a failed main write is a harmless truth, never a lost value).
// updated_by moves to the last toucher; the birth author labels are
// write-once and deliberately absent from this script.
//
// The in-doc history array is no longer maintained: the journal index is the
// history. Facts created before the journal keep their array until
// 'velruna migrate-history' moves it.
const addCorrectionScript = `
ctx._source.value = params.value;
// The guard audit describes the value: a clean correction clears the rows of
// the value it replaced (a stale audit would claim a loss that did not happen).
if (params.redactions != null) { ctx._source.redactions = params.redactions; }
else { ctx._source.remove("redactions"); }
ctx._source.category = params.category;
if (params.ttl_days != null) { ctx._source.ttl_days = params.ttl_days; }
if (params.keywords != null) { ctx._source.keywords = params.keywords; }
if (params.confirmed_by != null) { ctx._source.confirmed_by = params.confirmed_by; }
ctx._source.observed_at = params.observed_at;
if (params.source != null && params.source != "") { ctx._source.source = params.source; }
if (params.notes != null && params.notes != "") { ctx._source.notes = params.notes; }
ctx._source.status = "verified";
ctx._source.summary = params.summary;
ctx._source.current_ref = params.current_ref;
ctx._source.updated_by = params.updated_by;
ctx._source.updated_at = params.updated_at;
`

// AddResult reports what an accepted Add did — the answer to "what happened
// to my write", which {created} alone cannot tell (a replacement and a
// same-value touch both report created=false, and on a critical fact the
// difference is the whole point).
type AddResult struct {
	// Created: the fact did not exist and this write gave it birth.
	Created bool
	// Changed: a new version was accepted (birth, value correction, status
	// transition). false means a same-value touch: notes/source moved,
	// observed_at and the journal untouched.
	Changed bool
	// PreviousSummary is the summary the correction replaced ("" when the
	// prior fact had none) — names the superseded version cheaply.
	PreviousSummary string
	// Resurrected: the fact was obsolete and this write re-declared it
	// alive (status back to verified, observed_at moved, the event is
	// journaled). Without it a same-value resurrect reads as a value
	// correction (changed=true), and a correction over an obsolete fact
	// hides the status transition entirely.
	Resurrected bool
}

// redactionsForFields keeps the audit rows of the fields this write stores. A
// field the write drops (a summary on a same-value touch) must not claim a loss
// that never landed, and a field the caller did not touch must not inherit
// another field's rows.
func redactionsForFields(rows []model.Redaction, fields ...string) []model.Redaction {
	if len(rows) == 0 || len(fields) == 0 {
		return nil
	}
	keep := make(map[string]bool, len(fields))
	for _, f := range fields {
		keep[f] = true
	}
	out := make([]model.Redaction, 0, len(rows))
	for _, r := range rows {
		if keep[r.Field] {
			out = append(out, r)
		}
	}
	return out
}

// Add stores a fact under its key. Three cases: the key is new (created, then
// journaled and linked); the value is unchanged (only notes/source/status
// move — observed_at must not, the observation is old and only the qualifier
// is new, and the journal is untouched; the exception is a dead fact: an
// unchanged re-add of an obsolete fact is a fresh verification, journaled
// like the Patch resurrect, and observed_at moves); the value changed (a
// journal entry is appended for the new value, then the main doc is
// corrected to point at it — this also resurrects an obsolete fact).
//
// The journal append precedes the main write: the journal is the accepted
// log, the main doc is the projection. A failed main write leaves an orphan
// entry (truth: the value was accepted, the projection lags and is fixed by
// the next write); the reverse order could lose values from the log.
//
// Notes are only replaced by a non-empty new note: the CLI flag and the MCP
// field default to empty, and a correction without --notes must not silently
// erase the qualifiers (they are not kept in history, so erasure is irreversible).
//
// The category moves with the correction on both update paths: the write edge
// validates and canonicalizes it, and a fact stored with the wrong category
// must not keep the wrong staleness TTL forever (category drives stale).
//
// The summary moves with the value unconditionally: the summary describes the
// current value, and a correction to a short value (no summary required)
// erasing the old summary is the honest state — an old table of contents for
// a new value is a lie.
//
// It reports the accepted outcome (see AddResult). Writes wait for a refresh,
// so the fact is immediately visible to subsequent Search/List calls.
func (c *Client) Add(ctx context.Context, f *model.Fact) (AddResult, error) {
	if err := c.checkSummaryCap(f); err != nil {
		return AddResult{}, err
	}
	// Shape before store: keyword normalization is shared with the MCP edge,
	// and storing the raw form would split one tag across two spellings.
	if kw, err := model.NormalizeKeywords(f.Keywords); err != nil {
		return AddResult{}, contractErrorf("add fact: %v", err)
	} else {
		f.Keywords = kw
	}
	// Shape before store (the summary-cap rule): a writer-chosen staleness
	// window must fit the category cap even before we know whether this is a
	// create, a touch, or a correction.
	if f.TTLDays != 0 {
		if err := model.ValidTTLOverride(f.Category, f.TTLDays); err != nil {
			return AddResult{}, contractErrorf("add fact: %v", err)
		}
	}
	existing, seqNo, primaryTerm, err := c.getFactVersion(ctx, f.Key)
	if errors.Is(err, ErrNotFound) {
		// Summary bounds are enforced on accepted values only (create and
		// correction): a same-value notes touch must not fail over a summary
		// the pre-summary fact never had.
		if err := c.checkSummaryRequired(f); err != nil {
			return AddResult{}, err
		}
		// Birth labels are stamped once (write-once): who/what/where created
		// the fact. Later writes move only updated_by. Adding a fact IS the
		// first verification of its value, so the creator opens confirmed_by.
		author := c.resolveAuthor(ctx)
		author.stampFact(f)
		f.UpdatedBy = f.Author
		f.ConfirmedBy = []model.Confirmer{author.confirmer(f.ObservedAt)}
		// Distinct names: the create pair belongs to the link only. The outer
		// seqNo/primaryTerm must keep receiving the create-loser re-read below —
		// the touch path conditions on those.
		created, birthSeq, birthTerm, err := c.createFact(ctx, f)
		if err != nil {
			return AddResult{}, err
		}
		if created {
			// Journal the birth value under a deterministic id (the same
			// contract as corrections and the migration), then link the fact
			// to it. A journal failure leaves the fact in the pre-journal
			// shape (migrate-history catches it), not a broken fact. A link
			// failure (a blackout between the append and the link) is also
			// repairable: a same-value retry is a touch, not a relink, but
			// migrate-history re-appends the current entry under the SAME
			// deterministic id and links it — the collision is success, so
			// the repair needs no old auto id. (The id collision assumes
			// notes/source unchanged since birth; a touch that moved them
			// hashes a new id and appends the newer state as its own entry —
			// a second snapshot, not a duplicate of the birth.)
			ts := f.ObservedAt
			if ts.IsZero() {
				ts = f.UpdatedAt
			}
			ref := journalEntryID(f.Key, model.OpWrite, f.Value, f.Notes, f.Source, ts)
			if _, jErr := c.appendJournalID(ctx, c.newJournalEntryFor(ctx, f, f.FactID, ts), ref, "", true); jErr != nil {
				return AddResult{Created: true, Changed: true}, fmt.Errorf("add: the fact doc was stored, the journal entry lagged: %w", jErr)
			}
			if lErr := c.linkCurrentRef(ctx, f.Key, ref, birthSeq, birthTerm); lErr != nil {
				return AddResult{Created: true, Changed: true}, fmt.Errorf("add: the fact and its birth journal entry were stored, the link to the journal failed (migrate-history repairs the link): %w", lErr)
			}
			return AddResult{Created: true, Changed: true}, nil
		}
		// op_type=create lost the race to a concurrent Add of the same key
		// (a plain index would last-write-win and drop the winner's value
		// with no journal entry): re-read and apply as a correction below.
		existing, seqNo, primaryTerm, err = c.getFactVersion(ctx, f.Key)
		if err != nil {
			return AddResult{}, fmt.Errorf("add fact: %w", err)
		}
	} else if err != nil {
		return AddResult{}, fmt.Errorf("add fact: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// A write may move the category; the ttl_days that survives this write —
	// including a stored override the request does not mention — must fit the
	// incoming category's cap, or the fact would outlive what the category
	// promises auditors.
	effTTL := f.TTLDays
	if effTTL == 0 {
		effTTL = existing.TTLDays
	}
	if effTTL > 0 {
		if err := model.ValidTTLOverride(f.Category, effTTL); err != nil {
			if f.TTLDays == 0 {
				return AddResult{}, contractErrorf("add fact: the stored ttl_days=%d exceeds the max for category %q (%dd): re-add with a smaller ttl_days to move the category", effTTL, f.Category, model.TTLDays(f.Category))
			}
			return AddResult{}, contractErrorf("add fact: %v", err)
		}
	}

	if existing.Value == f.Value {
		// Re-declaring a dead fact alive IS a fresh verification (the Patch
		// resurrect rule, kept symmetric): flipping obsolete→verified on a
		// same-value add without a journal entry left the audit asymmetry
		// half-closed — "when was it last seen alive" would go unrecorded.
		// Only the explicit dead state resurrects (a status-less legacy doc
		// is not "dead": it stays a plain touch).
		resurrect := existing.Status == "obsolete"
		// The audit follows the fields this write actually stores. A same-value
		// touch never writes the summary (it is dropped, see the write edge), so
		// only notes can contribute rows; and because the touch OVERWRITES notes,
		// the stored notes row is dropped even when the new note is clean (a stale
		// row would claim a loss that no longer exists). Rows of the value survive.
		var touchFields []string
		if f.Notes != "" {
			touchFields = append(touchFields, "notes")
		}
		touchRows := redactionsForFields(f.Redactions, touchFields...)
		mergedRows := model.MergeRedactions(existing.Redactions, touchFields, touchRows)
		var entry *model.JournalEntry
		var ref string
		doc := map[string]interface{}{
			"category":   f.Category,
			"status":     "verified",
			"updated_by": c.resolveAuthor(ctx).Author,
			"updated_at": now,
		}
		if f.TTLDays != 0 {
			// A touch may retune the staleness window (qualifier-like, not
			// journaled); omitted means "keep the stored one".
			doc["ttl_days"] = f.TTLDays
		}
		if len(f.Keywords) > 0 {
			// Same rule as ttl_days: keywords are a stored qualifier, not a
			// journaled observation; an empty list means "leave them alone".
			doc["keywords"] = f.Keywords
		}
		if resurrect {
			observed := time.Now().UTC()
			stated := *existing
			// The touch doc moves category unconditionally (the write edge
			// canonicalizes it); the journaled resurrect must state the same.
			stated.Category = f.Category
			if f.Notes != "" {
				stated.Notes = f.Notes
			}
			if len(touchFields) > 0 || len(touchRows) > 0 {
				stated.Redactions = mergedRows
			}
			if len(f.Keywords) > 0 {
				stated.Keywords = f.Keywords
			}
			if f.Source != "" {
				stated.Source = f.Source
			}
			stated.Status = "verified"
			stated.ObservedAt = observed
			// A resurrection is a fresh verification by the writer: the prior
			// confirmations belong to a fact that was declared dead.
			stated.ConfirmedBy = []model.Confirmer{c.resolveAuthor(ctx).confirmer(observed)}
			doc["confirmed_by"] = stated.ConfirmedBy
			// Doc-first: the entry is built and its id preallocated here, the
			// entry itself is indexed only after the update is accepted.
			entry = c.newJournalEntryFor(ctx, &stated, existing.FactID, observed)
			ref = uuid.New().String()
			doc["observed_at"] = observed.Format(time.RFC3339)
			doc["current_ref"] = ref
		}
		// source follows the notes rule: empty means "not given", never
		// "erase" (Patch's contract). The qualifier is not kept in history on
		// the touch path, so an unconditional write of an empty source would
		// be an irreversible silent wipe.
		if f.Source != "" {
			doc["source"] = f.Source
		}
		if f.Notes != "" {
			doc["notes"] = f.Notes
		}
		if len(touchFields) > 0 || len(touchRows) > 0 {
			// The note was rewritten (or a span in it was cut): the stored notes
			// row must follow, and rows of the value it sits next to stay.
			doc["redactions"] = mergedRows
		}
		body, _ := json.Marshal(map[string]interface{}{
			"doc": doc,
		})
		// CAS: the touch moves status/updated_by/updated_at (and on a
		// resurrect current_ref/observed_at) from the snapshot read above.
		// A concurrent correction or retirement that landed since that read
		// makes this write wrong: the conflict is honest ("re-read and
		// retry") instead of silently clobbering the other writer's state.
		_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
			Index:      c.cfg.IndexName,
			DocumentID: f.Key,
			Body:       strings.NewReader(string(body)),
			Params: opensearchapi.UpdateParams{
				Refresh:       "wait_for",
				IfSeqNo:       &seqNo,
				IfPrimaryTerm: &primaryTerm,
			},
		})
		if err != nil {
			if isVersionConflict(err) {
				return AddResult{}, conflictError("add fact", f.Key, err)
			}
			return AddResult{}, wrapWriteError("add fact", err)
		}
		if entry != nil {
			if _, jErr := c.appendJournalID(ctx, entry, ref, "", false); jErr != nil {
				return AddResult{Changed: true}, fmt.Errorf("add: resurrection accepted, the journal entry lagged: %w", jErr)
			}
		}
		// Same value: notes/source moved, observed_at must not (the
		// observation is old), the journal is untouched — the resurrection
		// exception above is the one same-value write that IS a new
		// observation, and it reports Changed=true and Resurrected=true.
		return AddResult{Changed: resurrect, Resurrected: resurrect}, nil
	}

	if err := c.checkSummaryRequired(f); err != nil {
		return AddResult{}, err
	}

	// The journal entry carries the doc's fact_id (not the caller's fresh
	// uuid): entries group by the identity that survives, and the doc's
	// fact_id is that identity. The qualifiers are MERGED onto the incoming
	// value before journaling: the script keeps the stored source/notes when
	// the incoming ones are empty, and the journal entry must state the
	// accepted state exactly as the doc will read after the write — an entry
	// with an empty source next to a doc that kept "doc:README" is a small
	// audit fiction. (The entry body is value/notes/source only — see
	// journalAppend — so the merge stays three fields.)
	stated := *f
	// project is write-once (the correction script never moves it): the
	// journal entry of a correction states the doc's project, not the
	// request's — the CLI/MCP default "global" would otherwise journal a
	// project the fact never had.
	stated.Project = existing.Project
	if stated.Source == "" {
		stated.Source = existing.Source
	}
	if stated.Notes == "" {
		stated.Notes = existing.Notes
	}
	if len(stated.Keywords) == 0 {
		// Tags follow the notes/source rule: omitted means "keep the stored
		// set", and the journaled snapshot must state what the doc will hold.
		stated.Keywords = existing.Keywords
	}
	entry := c.newJournalEntryFor(ctx, &stated, existing.FactID, f.ObservedAt)
	ref := uuid.New().String()

	// ttl_days follows the patch convention: nil keeps the stored window, a
	// positive value replaces it (the shape was already checked above).
	var ttlParam interface{}
	if f.TTLDays != 0 {
		ttlParam = f.TTLDays
	}
	var kwParam interface{}
	if len(f.Keywords) > 0 {
		kwParam = f.Keywords
	}
	// nil when the guard cut nothing: the script removes the field, so a clean
	// value never inherits the audit of the value it replaced.
	var redactionsParam interface{}
	if len(f.Redactions) > 0 {
		redactionsParam = f.Redactions
	}

	body, _ := json.Marshal(map[string]interface{}{
		"script": map[string]interface{}{
			"lang":   "painless",
			"source": addCorrectionScript,
			"params": map[string]interface{}{
				"value":    f.Value,
				"category": f.Category,
				"ttl_days": ttlParam,
				"keywords": kwParam,
				// Redactions of the value being stored: nil clears the audit of the
				// value it replaces.
				"redactions": redactionsParam,
				// A new value is verified only by its writer: the set resets.
				"confirmed_by": []model.Confirmer{c.resolveAuthor(ctx).confirmer(f.ObservedAt)},
				"observed_at":  f.ObservedAt.UTC().Format(time.RFC3339),
				"source":       f.Source,
				"notes":        f.Notes,
				"summary":      f.Summary,
				"updated_at":   now,
				"current_ref":  ref,
				"updated_by":   c.resolveAuthor(ctx).Author,
			},
		},
	})
	_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: f.Key,
		Body:       strings.NewReader(string(body)),
		Params:     opensearchapi.UpdateParams{Refresh: "wait_for"},
	})
	if err != nil {
		// Nothing was accepted: no journal entry, Changed stays false (the
		// doc-first order is what makes this answer truthful).
		return AddResult{}, wrapWriteError("add fact", err)
	}
	if _, jErr := c.appendJournalID(ctx, entry, ref, "", false); jErr != nil {
		return AddResult{Changed: true, PreviousSummary: existing.Summary}, fmt.Errorf("add: correction accepted, the journal entry lagged: %w", jErr)
	}
	// The value was accepted as a new version (journal entry + correction):
	// changed=true, and the summary the correction is replacing is reported
	// so the caller can name the superseded version without reading the
	// journal. A correction over an obsolete fact also resurrects it (the
	// script writes status=verified): the transition is named explicitly.
	return AddResult{Changed: true, PreviousSummary: existing.Summary, Resurrected: existing.Status == "obsolete"}, nil
}

// newJournalEntryFor builds — without writing — the entry for a state the
// caller is about to accept. The scripted write paths preallocate the entry
// id, carry it into the update as current_ref, and index the entry under
// that id only after the update is accepted: a refused update (the cluster
// vetoes the script, a shard refuses the write) must leave no journal
// trace — an entry for a state that never landed is an audit lie. The
// residual the other way (update accepted, entry write lost) leaves the doc
// right and the log lagging one entry: current_ref dangles, the fact keeps
// working.
func (c *Client) newJournalEntryFor(ctx context.Context, f *model.Fact, factID string, ts time.Time) *model.JournalEntry {
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	e := model.NewJournalEntry(f.Key, factID, f.Project, f.Category, model.OpWrite, f.Value, f.Notes, f.Source, ts)
	e.Keywords = f.Keywords
	e.Redactions = f.Redactions
	c.resolveAuthor(ctx).applyTo(e)
	return e
}

// linkCurrentRef points a fact at its journal entry. Used on the create
// path (the fact doc is written before the entry id is committed); the
// correction script sets current_ref inside the same write.
// The link is a snapshot-derived write: it is conditioned on the version
// pair the create itself reported — "nothing has touched this doc since I
// wrote it". A concurrent correction landing after the create bumps the
// version, the link conflicts honestly, and the winner's link survives: a
// correction or resurrect sets its own current_ref (pointing at the entry
// that carries the live value) inside its script. A plain same-value touch
// bumps the version without setting a ref — the conflict then leaves the
// fact created-but-unlinked, which migrate-history genuinely repairs
// (deterministic-id re-append, collision-as-success). An unconditional
// link here would instead overwrite the winner's ref with the birth one,
// leaving current_ref pointing at an entry that does not carry the live
// value — a later prune --keep could then delete the entry that does.
// Re-reading the pair at link time would NOT close that window (the
// correction would be inside the snapshot); the pair must come from the
// create response.
func (c *Client) linkCurrentRef(ctx context.Context, key, ref string, seqNo, primaryTerm int) error {
	body, _ := json.Marshal(map[string]interface{}{
		"doc": map[string]interface{}{"current_ref": ref},
	})
	if _, err := c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: key,
		Body:       strings.NewReader(string(body)),
		Params: opensearchapi.UpdateParams{
			Refresh:       "wait_for",
			IfSeqNo:       &seqNo,
			IfPrimaryTerm: &primaryTerm,
		},
	}); err != nil {
		if isVersionConflict(err) {
			return conflictError("link current_ref", key, err)
		}
		return wrapWriteError("link current_ref", err)
	}
	return nil
}

// checkSummaryCap rejects an over-long summary in any write: a provided
// summary that is longer than the cap stops being a summary. The cap lives
// on the shape, not on the accepted value, so it runs before the store
// read.
func (c *Client) checkSummaryCap(f *model.Fact) error {
	if c.cfg.SummaryMaxLen > 0 {
		if n := utf8.RuneCountInString(f.Summary); n > c.cfg.SummaryMaxLen {
			return contractErrorf("add fact: summary is %d characters, over summary_max_len=%d: keep it a table of contents, not a second value", n, c.cfg.SummaryMaxLen)
		}
	}
	return nil
}

// checkSummaryRequired enforces the table-of-contents contract on an
// accepted value (create and correction paths): a value over summary_min_len
// must ship a summary, so store listings stay skimmable. Lengths count
// runes: the store is text-first and byte counts would punish non-Latin
// scripts.
func (c *Client) checkSummaryRequired(f *model.Fact) error {
	if c.cfg.SummaryMinLen > 0 && f.Summary == "" {
		if n := utf8.RuneCountInString(f.Value); n > c.cfg.SummaryMinLen {
			return contractErrorf("add fact: value is %d characters, over summary_min_len=%d: a summary is required (CLI --summary, MCP summary) — it keeps store listings skimmable", n, c.cfg.SummaryMinLen)
		}
	}
	return nil
}

// createFact stores a brand-new fact with op_type=create: OpenSearch refuses
// the write when the document exists, so two concurrent creators of the same
// key cannot silently overwrite each other (last-write-wins would lose the
// first value with no history entry). It reports created=true on success
// (with the document version pair the create itself wrote — the birth link
// is conditioned on exactly that state) and created=false on a version
// conflict, leaving the caller to re-read the winner and apply the fact as a
// correction.
func (c *Client) createFact(ctx context.Context, f *model.Fact) (bool, int, int, error) {
	body, _ := json.Marshal(f)
	resp, err := c.api.Index(ctx, opensearchapi.IndexReq{
		Index:      c.cfg.IndexName,
		DocumentID: f.Key,
		Body:       bytes.NewReader(body),
		Params:     opensearchapi.IndexParams{Refresh: "wait_for", OpType: "create"},
	})
	if err == nil {
		return true, resp.SeqNo, resp.PrimaryTerm, nil
	}
	if isVersionConflict(err) {
		return false, 0, 0, nil
	}
	return false, 0, 0, wrapWriteError("add fact", err)
}

// Get gets a fact by key. It uses the realtime document GET API, so it sees
// the fact immediately after Add (unlike a search, which waits for a refresh).
func (c *Client) Get(ctx context.Context, key string) (*model.Fact, error) {
	f, _, _, err := c.getFactVersion(ctx, key)
	return f, err
}

// getFactVersion reads a fact together with OpenSearch's document version
// pair (seq_no/primary_term). Snapshot-derived writes — touches,
// resurrects, retirements, status transitions — carry it into the update
// as if_seq_no/if_primary_term: without the condition, a concurrent write
// landing between our read and the update lets current_ref be rewritten to
// a journal entry carrying the OLD value, breaking the invariant that
// current_ref journals the doc's current value (and lets a plain touch
// silently un-verify a fact another writer just retired).
func (c *Client) getFactVersion(ctx context.Context, key string) (*model.Fact, int, int, error) {
	resp, err := c.api.Document.Get(ctx, opensearchapi.DocumentGetReq{
		Index:      c.cfg.IndexName,
		DocumentID: key,
	})
	if err != nil {
		if isNotFound(err) {
			return nil, 0, 0, fmt.Errorf("%w: %q", ErrNotFound, key)
		}
		return nil, 0, 0, fmt.Errorf("get fact: %w", err)
	}
	if !resp.Found {
		return nil, 0, 0, fmt.Errorf("%w: %q", ErrNotFound, key)
	}
	var f model.Fact
	if err := json.Unmarshal(resp.Source, &f); err != nil {
		return nil, 0, 0, fmt.Errorf("get fact: decode %q: %w", key, err)
	}
	return &f, resp.SeqNo, resp.PrimaryTerm, nil
}

// conflictError names a failed CAS condition as what it is: a valid
// operation over a stale snapshot. The message orders the remedy (re-read,
// retry) first because that is the whole answer; the store's own reason is
// chained too (two %w keeps errors.Is(err, ErrConflict) true while a
// non-standard 409 payload still reaches logs instead of being discarded).
func conflictError(op, key string, err error) error {
	return fmt.Errorf("%s: fact %q changed under this write (a concurrent writer won the race): re-read and retry: %w; store: %w", op, key, ErrConflict, err)
}

// Search performs a keyword search, capped to limit hits. limit <= 0 means
// "no caller cap": the size is then NavSizeCap (1000), not the server's
// default of 10, which would silently truncate. Relevance order is
// OpenSearch's.
func (c *Client) Search(ctx context.Context, query, project, category, status, keyword string, limit int) ([]model.Fact, SearchMeta, error) {
	return c.search(ctx, buildSearchBody(query, project, category, status, keyword, limit))
}

// List returns all facts for a project whose key starts with prefix.
// An empty prefix lists the whole project. A prefix may carry the trailing
// '*' sugar ("state:*" == "state:"); a '*' anywhere else is rejected.
//
// sort: "updated" (newest activity first — the session-start use-case: what
// moved since last time) or "key" (stable navigation order). Facts written
// before updated_at existed sort last under "updated" (missing:_last).
func (c *Client) List(ctx context.Context, project, prefix, sort string) ([]model.Fact, SearchMeta, error) {
	p, err := NormalizePrefix(prefix)
	if err != nil {
		return nil, SearchMeta{}, err
	}
	return c.search(ctx, buildProjectBody(project, p, sort))
}

// Keys returns facts whose key matches a prefix and/or a substring, across
// all projects (project == "" keeps the scope global). It is the key-level
// navigation tool: prefix answers "what is under this subtree", match
// answers "which keys mention this token". At least one of them is required:
// an unbounded key query is a context flood, not a search.
//
// match is a Lucene keyword pattern: '*' and '?' inside it are wildcards.
func (c *Client) Keys(ctx context.Context, prefix, match, project, status string) ([]model.Fact, SearchMeta, error) {
	p, err := NormalizePrefix(prefix)
	if err != nil {
		return nil, SearchMeta{}, err
	}
	if p == "" && match == "" {
		return nil, SearchMeta{}, fmt.Errorf("keys: provide a key prefix and/or a match pattern")
	}
	return c.search(ctx, buildKeysBody(p, match, project, status))
}

// NormalizePrefix validates a key prefix and strips the optional trailing
// '*'. It is exported so CLI and MCP can reject bad prefixes before touching
// the store. It is idempotent: the result is trimmed again after the strip
// ("k *" normalizes to "k", not "k "), so what a guard-checked edge accepts
// is exactly the term the store sees on the wire.
func NormalizePrefix(prefix string) (string, error) {
	p := strings.TrimSpace(prefix)
	p = strings.TrimSpace(strings.TrimSuffix(p, "*"))
	if strings.Contains(p, "*") {
		return "", fmt.Errorf("key prefix %q: only a trailing * is supported", prefix)
	}
	return p, nil
}

// Obsolete marks a fact obsolete because its subject is gone. The value and
// observed_at are intentionally left untouched — there is no new observation.
// The reason is appended to Notes so earlier qualifiers are not lost.
//
// The transition is journaled (the journal records accepted state, and a
// status flip is state): the audit trail is symmetric — going obsolete and
// coming back verified both leave entries. The journal entry carries the
// current value with the reason folded into notes, so "when did this die and
// why" is answerable from the log.
// appendNote joins a line into notes without leading/trailing blank lines
// (the shared join for retire reasons and dated note appends).
func appendNote(base, line string) string {
	if line == "" {
		return base
	}
	if base == "" {
		return line
	}
	return base + "\n" + line
}

// obsoleteScript retires a fact atomically: the reason appends INSIDE the
// script (read-modify-write of notes from the Go side would race a
// concurrent notes_append and silently drop it — the same contract as
// patchScript), and status/current_ref/updated_* land in the same write.
const obsoleteScript = `
if (params.reason_append != null) {
  String n = ctx._source.notes == null ? "" : ctx._source.notes.toString();
  ctx._source.notes = n.isEmpty() ? params.reason_append : n + params.nsep + params.reason_append;
}
if (params.redactions != null) { ctx._source.redactions = params.redactions; }
ctx._source.status = params.status;
ctx._source.current_ref = params.current_ref;
ctx._source.updated_at = params.updated_at;
ctx._source.updated_by = params.updated_by;
`

// Obsolete retires a fact. redactions is the write-time secret-guard audit of
// the reason text: a redacted reason lands in notes, and the rows travel with
// the fact (merged with the current value's audit, which the retirement does
// not rewrite).
func (c *Client) Obsolete(ctx context.Context, key, reason string, redactions []model.Redaction) error {
	existing, seqNo, primaryTerm, err := c.getFactVersion(ctx, key)
	if err != nil {
		return fmt.Errorf("obsolete fact: %w", err)
	}
	reason = strings.TrimSpace(reason)
	if reason != "" && existing.Status == "obsolete" {
		// Mirror Patch's refusal: a reason belongs to the verified→obsolete
		// transition. Re-retiring with a reason would append a duplicate notes
		// line while the journal stays untouched (it fires only on status
		// changes) — notes would grow audit lines the journal never recorded.
		return contractErrorf("obsolete: fact %q is already obsolete (a reason belongs to the verified→obsolete transition)", key)
	}
	// notes is one line per qualifier (see Patch): fold any whitespace run a
	// caller passes into single spaces before the reason joins the notes.
	reason = strings.Join(strings.Fields(reason), " ")
	// The journal snapshot carries the notes as of journal time (a concurrent
	// atomic append that lands after this Get lags one line; the doc stays
	// right — see the Patch journal contract). The already-obsolete refusal
	// above is computed from the stale read, exactly like Patch's: two racing
	// retires both pass the check, so each journals an entry and each appends
	// the reason — at worst a duplicated notes line and an extra journal entry
	// per race (the journal records every accepted write, so the audit trail
	// stays truthful about the events even then).
	notes := appendNote(existing.Notes, reason)
	currentRef := existing.CurrentRef
	// The audit of the stored value survives a retirement: only the reason text
	// may add rows (an appended reason accumulates — the earlier text is still
	// in the notes).
	mergedRedactions := model.MergeRedactions(existing.Redactions, nil, redactions)
	var entry *model.JournalEntry
	if existing.Status != "obsolete" {
		stated := *existing
		stated.Notes = notes
		stated.Redactions = mergedRedactions
		// Doc-first: the entry is built and its id preallocated here, the
		// entry itself is indexed only after the transition is accepted.
		entry = c.newJournalEntryFor(ctx, &stated, existing.FactID, time.Now().UTC())
		// The ops marker names the event: a retirement entry is not a value
		// write, and "who retired this, when" must not be inferred from notes.
		entry.Op = model.OpRetire
		currentRef = uuid.New().String()
	}
	params := map[string]interface{}{
		"reason_append": nil,
		"status":        "obsolete",
		"current_ref":   currentRef,
		"updated_at":    time.Now().UTC().Format(time.RFC3339),
		"updated_by":    c.resolveAuthor(ctx).Author,
		// The newline separator is a param, never "\n" in the script source:
		// painless string literals on this cluster process no escapes beyond
		// \\ and \" and refuse the script at compile time otherwise.
		"nsep": "\n",
	}
	if reason != "" {
		params["reason_append"] = reason
	}
	if len(redactions) > 0 {
		params["redactions"] = mergedRedactions
	}
	body, _ := json.Marshal(map[string]interface{}{
		"script": map[string]interface{}{
			"lang":   "painless",
			"source": obsoleteScript,
			"params": params,
		},
	})
	_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: key,
		Body:       strings.NewReader(string(body)),
		Params:     obsoleteUpdateParams(entry, seqNo, primaryTerm),
	})
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("obsolete %w: %q", ErrNotFound, key)
		}
		if isVersionConflict(err) {
			return conflictError("obsolete fact", key, err)
		}
		return wrapWriteError("obsolete fact", err)
	}
	if entry != nil {
		if _, jErr := c.appendJournalID(ctx, entry, currentRef, "", false); jErr != nil {
			return fmt.Errorf("obsolete: the transition was accepted, the journal entry lagged: %w", jErr)
		}
	}
	return nil
}

// obsoleteUpdateParams makes a retirement that journals a snapshot
// (entry != nil) a CAS write: if a concurrent correction or retirement
// landed after the read, the entry being journaled states a value the doc
// no longer holds and current_ref would point the doc at it. A
// re-retirement that journals nothing stays unconditional.
func obsoleteUpdateParams(entry *model.JournalEntry, seqNo, primaryTerm int) opensearchapi.UpdateParams {
	p := opensearchapi.UpdateParams{Refresh: "wait_for"}
	if entry != nil {
		p.IfSeqNo, p.IfPrimaryTerm = &seqNo, &primaryTerm
	}
	return p
}

// PatchFields is the partial-update payload: an empty string means "not
// provided, leave the field alone" (the same presence convention as add).
// Erasing a field to empty is not a patch operation — corrections carry
// content, not deletions.
type PatchFields struct {
	Value       string   // new value: journaled, observed_at moves, summary resets with it
	Summary     string   // new table of contents; with a value change, "" clears the old one
	Notes       string   // replaces notes when non-empty (add's rule)
	NotesAppend string   // dated "YYYY-MM-DD: text" line appended atomically (painless)
	Source      string   // replaces source when non-empty
	Status      string   // verified | obsolete — a transition is journaled (audit symmetry)
	Reason      string   // why it became obsolete (folded into notes; guard-checked)
	TTLDays     int      // retunes the staleness window (bounded by the category cap); 0 = untouched
	Keywords    []string // replaces the tag set when non-empty (tags are journaled with each accepted value)
	// Reverify re-confirms the current value: observed_at moves to now, the
	// journal gains an op=rediscovery entry, and the writer joins confirmed_by.
	// It cannot be combined with a value or status change — those are other
	// events (and both reset the confirmation set).
	Reverify bool
	// Redactions is the write-time secret-guard audit for the value this patch
	// stores (see model.Fact). A value change replaces the audit: the old rows
	// described the old value. Non-value patches leave the stored audit alone.
	Redactions []model.Redaction
}

// PatchResult reports what the patch did.
type PatchResult struct {
	// Changed: a new version was accepted (value change, status transition) or
	// a re-confirmation was journaled (Reverified).
	Changed bool
	Status  string
	// Reverified: the value was re-confirmed in place (observed_at moved, the
	// journal gained op=rediscovery, the value itself did not change).
	Reverified bool
}

// patchScript applies only the provided params (null = untouched). The notes
// append runs inside the script: read-modify-write of notes from the Go side
// would race with a concurrent add/patch and silently drop one of the notes
// (the correction script's notes handling has the same property by design).
const patchScript = `
if (params.value != null) { ctx._source.value = params.value; }
if (params.redactions != null) { ctx._source.redactions = params.redactions; }
else if (params.redactions_clear) { ctx._source.remove("redactions"); }
if (params.summary != null) { ctx._source.summary = params.summary; }
if (params.notes != null) { ctx._source.notes = params.notes; }
if (params.notes_append != null) {
  String n = ctx._source.notes == null ? "" : ctx._source.notes.toString();
  ctx._source.notes = n.isEmpty() ? params.notes_append : n + params.nsep + params.notes_append;
}
if (params.reason_append != null) {
  String n = ctx._source.notes == null ? "" : ctx._source.notes.toString();
  ctx._source.notes = n.isEmpty() ? params.reason_append : n + params.nsep + params.reason_append;
}
if (params.source != null) { ctx._source.source = params.source; }
if (params.ttl_days != null) { ctx._source.ttl_days = params.ttl_days; }
if (params.keywords != null) { ctx._source.keywords = params.keywords; }
if (params.confirmed_by != null) { ctx._source.confirmed_by = params.confirmed_by; }
if (params.confirmer != null) {
  if (ctx._source.confirmed_by == null) { ctx._source.confirmed_by = new ArrayList(); }
  boolean found = false;
  for (int i = 0; i < ctx._source.confirmed_by.size(); i++) {
    def c = ctx._source.confirmed_by.get(i);
    if (c.author == params.confirmer.author && c.host == params.confirmer.host) {
      ctx._source.confirmed_by.set(i, params.confirmer);
      found = true;
    }
  }
  if (!found) { ctx._source.confirmed_by.add(params.confirmer); }
}
if (params.status != null) { ctx._source.status = params.status; }
if (params.observed_at != null) { ctx._source.observed_at = params.observed_at; }
if (params.current_ref != null) { ctx._source.current_ref = params.current_ref; }
ctx._source.updated_at = params.updated_at;
ctx._source.updated_by = params.updated_by;
`

// ValidatePatchFields is the shape contract of a partial update, exported so
// the MCP edge can refuse a malformed patch before any store contact (the
// same "shape before availability" rule as list/search/keys: a bogus status
// must not pay for an index ensure, and on an unreachable store the shape
// error would drown in the "do not retry" advice).
func ValidatePatchFields(p PatchFields) error {
	if p.Value == "" && p.Summary == "" && p.Notes == "" && p.NotesAppend == "" &&
		p.Source == "" && p.Status == "" && p.Reason == "" && p.TTLDays == 0 && len(p.Keywords) == 0 && !p.Reverify {
		return fmt.Errorf("patch: nothing to change (provide value, summary, notes, notes_append, source, status, reason, ttl_days, keywords, or reverify)")
	}
	if p.Reverify && (p.Value != "" || p.Summary != "" || p.Status != "") {
		return fmt.Errorf("patch: reverify cannot be combined with a value, summary or status change (a new value is verified by its writer; a status transition is its own event)")
	}
	if p.TTLDays < 0 {
		return fmt.Errorf("patch: ttl_days must be positive, got %d", p.TTLDays)
	}
	if len(p.Keywords) > 0 {
		// Shape only: the length/count bounds are the shared normalizer's job
		// (the client runs it before the write, like add).
		for _, k := range p.Keywords {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("patch: keywords must not contain blank entries")
			}
		}
	}
	if p.Reason != "" && p.Status != "obsolete" {
		return fmt.Errorf("patch: reason is only accepted with status=obsolete")
	}
	if p.Status != "" && p.Status != "verified" && p.Status != "obsolete" {
		return fmt.Errorf("patch: status must be verified|obsolete, got %q", p.Status)
	}
	return nil
}

// Patch applies a partial update to an existing fact. It exists so an agent
// can extend qualifiers (append a dated note) or flip status without
// re-sending — and re-paying context for — the whole value. Journal
// semantics mirror add: the journal records accepted state, so a value
// change and a status transition both append entries; notes/source/summary
// moves are touches (no entry, observed_at stays).
//
// Summary invalidation: the summary describes the current value. On a value
// change the summary is replaced by the provided one ("" clears it — an old
// table of contents for a new value is a lie). A long new value requires a
// new summary (the add rule).
//
// Resurrection (obsolete → verified) sets observed_at=now: re-declaring a
// dead fact alive IS a fresh verification, and a fact that resurrects while
// staying stale would contradict its own staleness signal.
func (c *Client) Patch(ctx context.Context, key string, p PatchFields) (PatchResult, error) {
	if err := ValidatePatchFields(p); err != nil {
		return PatchResult{}, err
	}

	existing, seqNo, primaryTerm, err := c.getFactVersion(ctx, key)
	if errors.Is(err, ErrNotFound) {
		// The chain must survive: edges classify this as "no such fact"
		// (a clean answer), not as a store failure (Obsolete does the same).
		return PatchResult{}, fmt.Errorf("patch: %w: fact %q not found — use add to create it", ErrNotFound, key)
	}
	if err != nil {
		return PatchResult{}, fmt.Errorf("patch: %w", err)
	}

	// The cap belongs to the fact's stored category: a writer may shorten the
	// window, never lengthen it past what the category promises.
	if p.TTLDays > 0 {
		if err := model.ValidTTLOverride(existing.Category, p.TTLDays); err != nil {
			return PatchResult{}, contractErrorf("patch: %v", err)
		}
	}
	if len(p.Keywords) > 0 {
		kw, err := model.NormalizeKeywords(p.Keywords)
		if err != nil {
			return PatchResult{}, contractErrorf("patch: %v", err)
		}
		p.Keywords = kw
	}

	valueChanged := p.Value != "" && p.Value != existing.Value
	statusChanged := p.Status != "" && p.Status != existing.Status
	resurrect := statusChanged && p.Status == "verified"

	// notes for the doc: replace wins, then append, then the retire reason.
	newNotes := existing.Notes
	if p.Notes != "" {
		newNotes = p.Notes
	}
	appendLine := ""
	if p.NotesAppend != "" {
		appendLine = time.Now().UTC().Format("2006-01-02") + ": " + p.NotesAppend
		newNotes = appendNote(newNotes, appendLine)
	}
	journalNotes := newNotes
	retireReason := ""
	if p.Reason != "" && p.Status == "obsolete" && !statusChanged {
		// The reason belongs to the transition. Re-retiring an already
		// obsolete fact would append a duplicate notes line with no journal
		// entry (no state was accepted) — a silent audit lie.
		return PatchResult{}, contractErrorf("patch: fact %q is already obsolete (a reason belongs to the verified→obsolete transition)", key)
	}
	if statusChanged && p.Status == "obsolete" && p.Reason != "" {
		// notes is one line per qualifier: a reason with newlines would
		// embed a multi-line block and break the "one line, one event"
		// reading of the audit trail.
		retireReason = strings.Join(strings.Fields(p.Reason), " ")
		journalNotes = appendNote(journalNotes, retireReason)
	}

	if valueChanged {
		probe := model.NewFact(key, p.Value, existing.Category, existing.Project, existing.Source, newNotes)
		probe.Summary = p.Summary
		if err := c.checkSummaryRequired(probe); err != nil {
			return PatchResult{}, err
		}
		if err := c.checkSummaryCap(probe); err != nil {
			return PatchResult{}, err
		}
	} else if p.Summary != "" {
		probe := model.NewFact(key, existing.Value, existing.Category, existing.Project, existing.Source, existing.Notes)
		probe.Summary = p.Summary
		if err := c.checkSummaryCap(probe); err != nil {
			return PatchResult{}, err
		}
	}

	now := time.Now().UTC()
	currentRef := existing.CurrentRef
	// The audit rows this write lands. A value change REPLACES the set (a clean
	// value clears it — stale rows would describe text that no longer exists);
	// a qualifier-only write MERGES its rows in, so the audit of the value it
	// sits next to stays true.
	var qualifierRedactions []model.Redaction
	var replacedFields []string
	if !valueChanged {
		if p.Summary != "" {
			replacedFields = append(replacedFields, "summary")
		}
		if p.Notes != "" {
			replacedFields = append(replacedFields, "notes")
		}
		// Appended text adds rows instead of replacing them, but it is scanned
		// only when the patch actually appends it.
		scanFields := append([]string(nil), replacedFields...)
		if p.NotesAppend != "" {
			scanFields = append(scanFields, "notes_append")
		}
		if p.Reason != "" {
			scanFields = append(scanFields, "reason")
		}
		qualifierRedactions = redactionsForFields(p.Redactions, scanFields...)
	}
	effectiveRedactions := existing.Redactions
	if valueChanged {
		effectiveRedactions = p.Redactions
	} else if len(replacedFields) > 0 || len(qualifierRedactions) > 0 {
		effectiveRedactions = model.MergeRedactions(existing.Redactions, replacedFields, qualifierRedactions)
	}
	var entry *model.JournalEntry
	if valueChanged || statusChanged || p.Reverify {
		stated := *existing
		if valueChanged {
			stated.Value = p.Value
		}
		// The journal carries the notes as of journal time — a concurrent
		// atomic append that landed after this Get is not in the snapshot (the
		// doc stays right, the log lags by one append; the journal is a
		// history of accepted state, not a changelog of every byte).
		stated.Notes = journalNotes
		if p.Source != "" {
			stated.Source = p.Source
		}
		if len(p.Keywords) > 0 {
			// The snapshot states the tag set the doc will hold.
			stated.Keywords = p.Keywords
		}
		if valueChanged || len(replacedFields) > 0 || len(qualifierRedactions) > 0 {
			// The entry states the text it records, audit included.
			stated.Redactions = effectiveRedactions
		}
		// Doc-first: the entry is built and its id preallocated here, the
		// entry itself is indexed only after the update is accepted.
		entry = c.newJournalEntryFor(ctx, &stated, existing.FactID, now)
		if statusChanged && p.Status == "obsolete" {
			// Only the transition is a retirement. A value correction on an
			// already-obsolete fact (status unchanged, the script leaves it
			// dead) is a write: marking it retire would date the burial wrong.
			entry.Op = model.OpRetire
		}
		if p.Reverify {
			// Same value, freshly observed: the reserved rediscovery marker is
			// exactly this event, and the entry names who re-confirmed it.
			entry.Op = model.OpRediscovery
		}
		currentRef = uuid.New().String()
	}

	newStatus := existing.Status
	if p.Status != "" {
		newStatus = p.Status
	}
	if p.Reverify && existing.Status == "obsolete" {
		return PatchResult{}, contractErrorf("patch: fact %q is obsolete — re-declaring it alive is a resurrection (status=verified or add), not a re-verification", key)
	}

	params := map[string]interface{}{
		"value":            nil,
		"redactions":       nil,
		"redactions_clear": false,
		"summary":          nil,
		"notes":            nil,
		"notes_append":     nil,
		"reason_append":    nil,
		"source":           nil,
		"ttl_days":         nil,
		"keywords":         nil,
		"confirmed_by":     nil,
		"confirmer":        nil,
		"status":           nil,
		"observed_at":      nil,
		"current_ref":      nil,
		"updated_at":       now.Format(time.RFC3339),
		"updated_by":       c.resolveAuthor(ctx).Author,
		// Notes-line separator is data, not source: painless string literals
		// refuse "\n" at compile time (only \\ and \" escapes are valid).
		"nsep": "\n",
	}
	if valueChanged {
		params["value"] = p.Value
		// The summary moves with the value unconditionally: the provided one
		// (possibly "" = clear) replaces the old table of contents.
		params["summary"] = p.Summary
		params["observed_at"] = now.Format(time.RFC3339)
		params["current_ref"] = currentRef
		// A new value is verified only by its writer: the set resets.
		params["confirmed_by"] = []model.Confirmer{c.resolveAuthor(ctx).confirmer(now)}
	} else if statusChanged {
		params["current_ref"] = currentRef
	}
	// The guard audit describes the stored text: a value change sets or clears
	// the rows; a qualifier write drops the rows of the fields it overwrote and
	// merges the new ones in.
	if valueChanged || len(replacedFields) > 0 || len(qualifierRedactions) > 0 {
		if len(effectiveRedactions) > 0 {
			params["redactions"] = effectiveRedactions
		} else {
			params["redactions_clear"] = true
		}
	}
	if resurrect {
		params["observed_at"] = now.Format(time.RFC3339)
		// A resurrection is a fresh verification by the resurrector: the prior
		// confirmations belong to a fact that was declared dead.
		params["confirmed_by"] = []model.Confirmer{c.resolveAuthor(ctx).confirmer(now)}
	}
	if p.Reverify {
		// observed_at moves (that is the point), and the confirmer is appended
		// INSIDE the script: a Go-side read-modify-write of the list would drop
		// a concurrent confirmation.
		params["observed_at"] = now.Format(time.RFC3339)
		params["current_ref"] = currentRef
		params["confirmer"] = c.resolveAuthor(ctx).confirmer(now)
	}
	if p.Summary != "" && !valueChanged {
		params["summary"] = p.Summary
	}
	if p.Notes != "" {
		params["notes"] = newNotes
	} else if appendLine != "" {
		params["notes_append"] = appendLine
	}
	if retireReason != "" {
		// The reason appends INSIDE the script: folding it into a Go-side
		// notes replace would be a read-modify-write of the stale Get, which
		// silently drops a concurrent atomic notes_append — the exact race
		// the scripted patch exists to prevent.
		params["reason_append"] = retireReason
	}
	if p.Source != "" {
		params["source"] = p.Source
	}
	if p.TTLDays > 0 {
		params["ttl_days"] = p.TTLDays
	}
	if len(p.Keywords) > 0 {
		params["keywords"] = p.Keywords
	}
	if p.Status != "" {
		params["status"] = p.Status
	}

	body, _ := json.Marshal(map[string]interface{}{
		"script": map[string]interface{}{
			"lang":   "painless",
			"source": patchScript,
			"params": params,
		},
	})
	_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: key,
		Body:       strings.NewReader(string(body)),
		Params:     patchUpdateParams(entry, valueChanged, seqNo, primaryTerm),
	})
	if err != nil {
		if isNotFound(err) {
			return PatchResult{}, fmt.Errorf("patch %w: %q", ErrNotFound, key)
		}
		if isVersionConflict(err) {
			return PatchResult{}, conflictError("patch fact", key, err)
		}
		return PatchResult{}, wrapWriteError("patch fact", err)
	}
	if entry != nil {
		if _, jErr := c.appendJournalID(ctx, entry, currentRef, "", false); jErr != nil {
			return PatchResult{Changed: valueChanged || statusChanged || p.Reverify, Status: newStatus, Reverified: p.Reverify}, fmt.Errorf("patch: accepted, the journal entry lagged: %w", jErr)
		}
	}
	return PatchResult{Changed: valueChanged || statusChanged || p.Reverify, Status: newStatus, Reverified: p.Reverify}, nil
}

// patchUpdateParams conditions a patch on the document version pair when it
// journals a STATUS transition computed from the snapshot read: a
// concurrent value change that landed after the read would leave current_ref
// pointing at an entry stating the value the doc no longer holds. A patch
// that also changes the value computes current_ref from the new value, so
// the invariant holds under any write order and the update stays
// unconditional (last-write-wins, matching add's correction).
func patchUpdateParams(entry *model.JournalEntry, valueChanged bool, seqNo, primaryTerm int) opensearchapi.UpdateParams {
	p := opensearchapi.UpdateParams{Refresh: "wait_for"}
	if entry != nil && !valueChanged {
		p.IfSeqNo, p.IfPrimaryTerm = &seqNo, &primaryTerm
	}
	return p
}

// pruneHistoryScript keeps only the newest params.keep history entries in
// the legacy in-doc array. It serves facts that predate the journal until
// 'velruna migrate-history' moves their array; journal pruning is
// PruneJournal (the journal index is the history for journal-era facts).
const pruneHistoryScript = `
def h = ctx._source.history;
if (h != null && h.size() > params.keep) {
  ctx._source.history = new ArrayList(h.subList(h.size() - params.keep, h.size()));
}
ctx._source.updated_at = params.updated_at;
`

// JournalEntriesForPrune exposes the journal entry count for a key (the
// prune planning path shows the journal, not the legacy array, once a key
// has entries there).
func (c *Client) JournalCount(ctx context.Context, key string) (int, error) {
	_, meta, err := c.JournalEntries(ctx, JournalQuery{Key: key, Limit: 1})
	return meta.Total, err
}

// Prune drops history beyond the newest keep. It is journal-first: a key
// with journal entries is pruned in the journal (the legacy in-doc array is
// ignored for such keys — migrate-history removes it); keys that predate the
// journal fall back to the legacy array so live stores prune cleanly between
// release and migration. It returns the number of entries removed. Like every
// destructive call it must sit behind an explicit user confirmation — the
// barrier lives in the CLI (--yes) and the MCP tool (confirm=true).
func (c *Client) Prune(ctx context.Context, key string, keep int) (int, error) {
	if keep < 0 {
		return 0, fmt.Errorf("prune: keep must be >= 0, got %d", keep)
	}
	total, err := c.JournalCount(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("prune fact: %w", err)
	}
	if total > 0 {
		return c.PruneJournal(ctx, key, keep)
	}

	existing, err := c.Get(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("prune fact: %w", err)
	}
	removed := len(existing.History) - keep
	if removed <= 0 {
		return 0, nil // nothing beyond the limit: no write, no updated_at move
	}
	body, _ := json.Marshal(map[string]interface{}{
		"script": map[string]interface{}{
			"lang":   "painless",
			"source": pruneHistoryScript,
			"params": map[string]interface{}{
				"keep":       keep,
				"updated_at": time.Now().UTC().Format(time.RFC3339),
			},
		},
	})
	_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: key,
		Body:       bytes.NewReader(body),
		Params:     opensearchapi.UpdateParams{Refresh: "wait_for"},
	})
	if err != nil {
		if isNotFound(err) {
			return 0, fmt.Errorf("prune %w: %q", ErrNotFound, key)
		}
		return 0, wrapWriteError("prune fact", err)
	}
	// The script trims the REAL array; the pre-read snapshot can lag a
	// concurrent write, so the reported count is recomputed from the stored
	// doc ("dropped N" must not be a snapshot guess — legacy path only,
	// journal-backed keys report exactly).
	after, gErr := c.Get(ctx, key)
	if gErr != nil {
		return removed, nil // write succeeded; the recount is best effort here
	}
	if removed = len(existing.History) - len(after.History); removed < 0 {
		removed = 0
	}
	return removed, nil
}

// buildSearchBody builds the search query body for a keyword search.
// The result budget matters as soon as one index holds bulk text (docs):
// an uncapped search ships every hit into the caller's context.
func buildSearchBody(query string, project, category, status, keyword string, limit int) map[string]interface{} {
	filters := []map[string]interface{}{}
	if project != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"project": project}})
	}
	if category != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"category": category}})
	}
	if status != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"status": status}})
	}
	if keyword != "" {
		// A term filter on the keyword array: exact tag match, no analysis.
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"keywords": keyword}})
	}
	boolQuery := map[string]interface{}{
		"must": map[string]interface{}{
			// multi_match treats the query as text. query_string would parse
			// Lucene syntax, so keys like "proj:foo" or "foo(bar" would either
			// match the wrong field or fail with a 400.
			"multi_match": map[string]interface{}{
				"query":  query,
				"fields": []string{"value", "notes", "summary"},
			},
		},
	}
	if len(filters) > 0 {
		boolQuery["filter"] = filters
	}
	body := map[string]interface{}{
		"query": map[string]interface{}{"bool": boolQuery},
		// Exact totals, not the 10000-capped "gte" estimate: the truncation
		// advice ("of N matches") must be true at any store size, and the
		// counting cost is irrelevant at this store's scale.
		"track_total_hits": true,
	}
	if limit > 0 {
		body["size"] = limit
	} else {
		// "no cap": size is mandatory for predictability; the server default
		// of 10 silently truncates limit=0 queries.
		body["size"] = NavSizeCap
	}
	return body
}

// NavSizeCap is the explicit size for navigation queries (list/keys) and for
// "no cap" searches: without it OpenSearch returns 10 hits and callers
// silently receive an arbitrary subset.
const NavSizeCap = 1000

// buildProjectBody builds the search query body for a project lookup,
// optionally narrowed to a key prefix.
func buildProjectBody(project, prefix, sort string) map[string]interface{} {
	filters := []map[string]interface{}{
		{"term": map[string]interface{}{"project": project}},
	}
	if prefix != "" {
		filters = append(filters, map[string]interface{}{
			"prefix": map[string]interface{}{"key": prefix},
		})
	}
	body := map[string]interface{}{
		"query":            map[string]interface{}{"bool": map[string]interface{}{"filter": filters}},
		"size":             NavSizeCap,
		"track_total_hits": true,
	}
	if sort == "updated" {
		body["sort"] = []map[string]interface{}{{
			"updated_at": map[string]interface{}{"order": "desc", "missing": "_last"},
		}}
	} else {
		body["sort"] = []map[string]interface{}{{"key": map[string]interface{}{"order": "asc"}}}
	}
	return body
}

// buildKeysBody builds the key-navigation query: AND of the given filters,
// sorted by key so the navigation output is stable.
func buildKeysBody(prefix, match, project, status string) map[string]interface{} {
	filters := []map[string]interface{}{}
	if prefix != "" {
		filters = append(filters, map[string]interface{}{
			"prefix": map[string]interface{}{"key": prefix},
		})
	}
	if match != "" {
		filters = append(filters, map[string]interface{}{
			"wildcard": map[string]interface{}{"key": "*" + match + "*"},
		})
	}
	if project != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"project": project}})
	}
	if status != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"status": status}})
	}
	return map[string]interface{}{
		"query":            map[string]interface{}{"bool": map[string]interface{}{"filter": filters}},
		"sort":             []map[string]interface{}{{"key": map[string]interface{}{"order": "asc"}}},
		"size":             NavSizeCap,
		"track_total_hits": true,
	}
}

// SearchMeta carries query diagnostics that a facts-only return hides:
// Total is the cluster-side match count (the honest truncation signal: a
// page exactly filling the cap is a complete result), Skipped counts hits
// whose _source failed to decode — index corruption must never be silent.
type SearchMeta struct {
	Total   int
	Skipped int
}

// search executes a search and returns the matching facts with meta.
func (c *Client) search(ctx context.Context, body map[string]interface{}) ([]model.Fact, SearchMeta, error) {
	data, _ := json.Marshal(body)
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{c.cfg.IndexName},
		Body:    bytes.NewReader(data),
	})
	if err != nil {
		return nil, SearchMeta{}, fmt.Errorf("search: %w", err)
	}
	meta := SearchMeta{Total: resp.Hits.Total.Value}
	var results []model.Fact
	for _, hit := range resp.Hits.Hits {
		var f model.Fact
		if err := json.Unmarshal(hit.Source, &f); err != nil {
			meta.Skipped++
			continue
		}
		results = append(results, f)
	}
	return results, meta, nil
}
