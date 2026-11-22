package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// reconcileMapping adds mapping properties an existing index was created
// without. PUT _mapping is additive and idempotent for fields that are already
// there with the same type; the point is the fields added after the index
// existed (ttl_days, keywords), which would otherwise be typed by the first
// write's dynamic mapping — a `keywords` array would become text+keyword and
// an exact tag filter would silently stop matching tags the analyzer splits.
//
// It runs once per store per process. The MCP server builds a fresh client per
// tool call, so a per-client flag would pay the PUT on every call. The gate
// also makes a concurrent caller WAIT for the pass: a write racing the first
// PUT would otherwise let the dynamic mapping pin a field (a `keywords` array
// becomes text+keyword) and the later PUT would conflict on every retry,
// leaving the degraded type silently in place.
//
// The failure is deliberately not fatal, matching the replica reconcile: reads
// and writes stay correct, only the type best-effort degrades, and an operator
// can see the real types in GET _mapping.
func (c *Client) reconcileMapping(ctx context.Context) error {
	gate := infraGate("mapping|" + c.cfg.OpenSearchURL + "|" + c.cfg.IndexName)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.done {
		return nil
	}
	body, err := json.Marshal(map[string]interface{}{
		"properties": buildIndexProperties(),
		// Stamps the schema this binary writes: a newer binary refuses a store
		// it cannot understand, and this is what it reads.
		"_meta": schemaMeta(),
	})
	if err != nil {
		return fmt.Errorf("reconcile index mapping %s: %w", c.cfg.IndexName, err)
	}
	if _, err := c.api.Indices.Mapping.Put(ctx, opensearchapi.MappingPutReq{
		Indices: []string{c.cfg.IndexName},
		Body:    bytes.NewReader(body),
	}); err != nil {
		if statusIs(err, 403) {
			// A read-only or DLS role cannot manage mappings: the index is
			// managed externally, which is not a retryable failure. Closing the
			// gate keeps every read from paying a refused PUT.
			gate.done = true
			return nil
		}
		// The gate stays open: the next caller retries.
		return fmt.Errorf("reconcile index mapping %s: %w", c.cfg.IndexName, err)
	}
	gate.done = true
	return nil
}
