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
// store. checked reports whether the stored version was actually READ: false
// means the verdict is unknown (the store did not answer, the role cannot read
// mappings, or the body did not decode), and the caller must not stamp a
// version it never saw — a blind _meta PUT could downgrade a newer store's
// marker and let this binary write into it.
//
// An error means one thing only: the store carries a newer schema.
func (c *Client) ensureSchemaVersion(ctx context.Context) (checked bool, err error) {
	gate := infraGate("schema|" + c.cfg.OpenSearchURL + "|" + c.cfg.IndexName)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.done {
		return !gate.unreadable, nil
	}

	status, body, gerr := c.doSavedObjectRequest(ctx, http.MethodGet,
		"/"+c.cfg.IndexName+"/_mapping", nil)
	if gerr != nil {
		// The store is unreachable: EnsureIndex's caller reports that; this
		// check must not turn a network error into a schema verdict. The gate
		// stays open so a later call retries the check.
		return false, nil
	}
	switch status {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusUnauthorized:
		// A role that may write but not read mappings: the version cannot be
		// checked, and refusing would break a setup that worked before. Cache
		// the answer so every tool call does not pay a refused GET.
		gate.done, gate.unreadable = true, true
		return false, nil
	default:
		return false, nil
	}

	var res map[string]struct {
		Mappings struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"mappings"`
	}
	if uerr := json.Unmarshal([]byte(body), &res); uerr != nil {
		return false, nil // an unparsable mapping is not a version verdict
	}

	found := 0
	for _, idx := range res {
		if raw, ok := idx.Mappings.Meta[schemaMetaKey]; ok {
			var v int
			if uerr := json.Unmarshal(raw, &v); uerr == nil {
				found = v
			}
		}
	}
	if found > SchemaVersion {
		return false, fmt.Errorf("the store at %s was written by a newer velruna (schema %d, this binary "+
			"understands %d): upgrade the binary before using this store", c.cfg.OpenSearchURL, found, SchemaVersion)
	}
	gate.done = true
	return true, nil
}

// schemaMeta is the _meta block stamped on every mapping write.
func schemaMeta() map[string]interface{} {
	return map[string]interface{}{schemaMetaKey: SchemaVersion}
}
