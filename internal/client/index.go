package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

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
		// Successor link (see model.Fact.SupersededBy): exact-match navigation.
		"superseded_by": map[string]interface{}{"type": "keyword"},
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
		return c.adoptExistingIndex(ctx)
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
			// Lost the create race to a concurrent process: the index is there,
			// but it was created by someone else — adopt it exactly as if the
			// HEAD above had seen it (a newer creator's schema must still be
			// refused; an older one's mapping must still be upgraded).
			return c.adoptExistingIndex(ctx)
		}
		return wrapWriteError("create index", err)
	}
	return nil
}

// adoptExistingIndex runs the checks that belong to a pre-existing index.
// The mapping reconcile is skipped when the schema verdict is unknown: the
// reconcile PUT also stamps _meta, and a blind stamp would silently downgrade
// a store written by a newer binary.
func (c *Client) adoptExistingIndex(ctx context.Context) error {
	checked, err := c.ensureSchemaVersion(ctx)
	if err != nil {
		return err
	}
	c.reconcileReplicas(ctx)
	if checked {
		// Best effort, like the replica reconcile: a mapping the cluster
		// refuses (a field already exists with another type) must not break
		// reads, and the once-guard stays open so a later run retries.
		_ = c.reconcileMapping(ctx)
	}
	return nil
}
