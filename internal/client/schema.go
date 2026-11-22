package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// SchemaVersion is the mapping/settings contract this binary reads and writes.
//
// It is stored as mappings._meta.velruna_schema on the facts index and checked
// once per process. Two rules:
//
//   - a store stamped with a LOWER version is upgraded in place: every change so
//     far is additive (new fields, new views), and the mapping reconcile next to
//     this check writes the new shape;
//   - a store stamped with a HIGHER version is refused by name. A newer binary
//     may rely on fields this one does not know, and "keep writing and hope" is
//     how a store gets corrupted. The fix is to upgrade the binary.
//
// A store created before versioning (v0.8.x) has no marker: it is adopted at the
// current version, because 0.9 changed nothing that would break older data.
const SchemaVersion = 1

// schemaMetaKey is the mapping metadata key the version lives under.
const schemaMetaKey = "velruna_schema"

// ensureSchemaVersion checks (once per process) that this binary may use the
// store. It returns an error only for a store written by a newer binary.
func (c *Client) ensureSchemaVersion(ctx context.Context) error {
	gate := infraGate("schema|" + c.cfg.OpenSearchURL + "|" + c.cfg.IndexName)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.done {
		return nil
	}

	status, body, err := c.doSavedObjectRequest(ctx, http.MethodGet,
		"/"+c.cfg.IndexName+"/_mapping", nil)
	if err != nil {
		// The store is unreachable: EnsureIndex's caller reports that; this
		// check must not turn a network error into a schema verdict.
		return nil
	}
	switch status {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusUnauthorized:
		// A role that may write but not read mappings: the version cannot be
		// checked, and refusing would break a setup that worked before.
		gate.done = true
		return nil
	default:
		return nil
	}

	var res map[string]struct {
		Mappings struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return nil // an unparsable mapping is the reconcile's problem, not ours
	}

	found := 0
	for _, idx := range res {
		if raw, ok := idx.Mappings.Meta[schemaMetaKey]; ok {
			var v int
			if err := json.Unmarshal(raw, &v); err == nil {
				found = v
			}
		}
	}
	if found > SchemaVersion {
		return fmt.Errorf("the store at %s was written by a newer velruna (schema %d, this binary "+
			"understands %d): upgrade the binary before writing; reads are not blocked by this check "+
			"for older schemas only", c.cfg.OpenSearchURL, found, SchemaVersion)
	}
	gate.done = true
	return nil
}

// schemaMeta is the _meta block stamped on every mapping write.
func schemaMeta() map[string]interface{} {
	return map[string]interface{}{schemaMetaKey: SchemaVersion}
}
