package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// SchemaReport is what the store's own mapping says about this binary. It is
// diagnostics only: reading it never changes the store.
type SchemaReport struct {
	// Schema names the schema marker: "v1", "unversioned (store created before
	// schema markers)", "newer (vN)", or an "unreadable"/"absent" verdict.
	Schema string
	// Mapping is "ok" when the stored field types match what this binary
	// writes, otherwise a short drift summary. Empty when the mapping could
	// not be read (the Schema line says why).
	Mapping string
	// Hint is what to do about a non-clean verdict.
	Hint string
	// IndexAbsent reports that the facts index does not exist yet.
	IndexAbsent bool
}

// storedMappingField is the part of a stored mapping property this check
// cares about. A disabled object carries no "type" in the response.
type storedMappingField struct {
	Type    string `json:"type"`
	Enabled *bool  `json:"enabled"`
}

// SchemaCheck reads the facts index mapping once and reports the schema
// marker plus any field-type drift. OpenSearch cannot change a field's type in
// place, so a store whose field was pinned by a dynamic mapping (a keywords
// array that became text+keyword before the mapping was made explicit) keeps
// that type forever: the only fix is a re-index, and the operator has to see
// it rather than have the writer silently work around it.
func (c *Client) SchemaCheck(ctx context.Context) *SchemaReport {
	path := "/" + url.PathEscape(c.cfg.IndexName) + "/_mapping"
	status, body, err := c.doSavedObjectRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return &SchemaReport{Schema: "unreadable: " + err.Error(),
			Hint: "the store did not answer the mapping read: check opensearch_url and the network"}
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return &SchemaReport{Schema: "absent (no facts index yet)", IndexAbsent: true,
			Hint: "the index is created with the current schema on the first write"}
	case http.StatusForbidden:
		return &SchemaReport{Schema: "unreadable (HTTP 403)",
			Hint: "this role cannot read mappings: the schema and field types are not verified. " +
				"Grant the store user the monitor/mapping read permission, or run 'velruna status' " +
				"once with an account that can"}
	case http.StatusUnauthorized:
		return &SchemaReport{Schema: "unreadable (HTTP 401)",
			Hint: "the credentials were rejected: check opensearch_user and the password source"}
	default:
		return &SchemaReport{Schema: fmt.Sprintf("unreadable (HTTP %d)", status),
			Hint: "the mapping read failed: see the store's response in the server logs"}
	}

	var res map[string]struct {
		Mappings struct {
			Meta       map[string]json.RawMessage    `json:"_meta"`
			Properties map[string]storedMappingField `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return &SchemaReport{Schema: "unreadable (the mapping response did not parse)",
			Hint: "the store answered something unexpected: check the OpenSearch version"}
	}

	names := make([]string, 0, len(res))
	for name := range res {
		names = append(names, name)
	}
	sort.Strings(names)

	// An alias can resolve to several backing indices: the newest marker wins,
	// and the properties merge in sorted-name order (first definition wins).
	found := 0
	markerSeen := false
	markerNotAnInt := false
	props := map[string]storedMappingField{}
	for _, name := range names {
		m := res[name].Mappings
		if raw, ok := m.Meta[schemaMetaKey]; ok {
			var v int
			if err := json.Unmarshal(raw, &v); err != nil {
				markerNotAnInt = true
			} else if v > found {
				found = v
				markerSeen = true
			}
		}
		for field, spec := range m.Properties {
			if _, seen := props[field]; !seen {
				props[field] = spec
			}
		}
	}

	rep := &SchemaReport{}
	switch {
	case markerNotAnInt && !markerSeen:
		rep.Schema = "unreadable (the schema marker is not a number)"
		rep.Hint = "the mapping was edited by hand or written by a future scheme: this binary " +
			"will not stamp a version it cannot read"
	case !markerSeen:
		rep.Schema = "unversioned (store created before schema markers)"
		rep.Hint = "adopted at v" + fmt.Sprint(SchemaVersion) + ": the additive fields land on the next write"
	case found > SchemaVersion:
		rep.Schema = fmt.Sprintf("newer (v%d)", found)
		rep.Hint = fmt.Sprintf("this binary understands v%d: upgrade velruna before using this store", SchemaVersion)
	case found < SchemaVersion:
		rep.Schema = fmt.Sprintf("v%d", found)
		rep.Hint = fmt.Sprintf("additive upgrade to v%d happens on the next write", SchemaVersion)
	default:
		rep.Schema = fmt.Sprintf("v%d", found)
	}

	// Field-type drift: the types this binary writes versus what the store
	// pinned. A missing field is not drift (the additive reconcile adds it),
	// but it is worth naming when the mapping cannot be updated.
	expected := expectedFieldTypes()
	var drift, missing []string
	for field, want := range expected {
		have, ok := props[field]
		if !ok {
			missing = append(missing, field)
			continue
		}
		if storedType(have) != want {
			drift = append(drift, fmt.Sprintf("%s: %s, want %s", field, storedType(have), want))
		}
	}
	sort.Strings(drift)
	sort.Strings(missing)
	switch {
	case len(drift) > 0:
		parts := drift
		truncated := false
		if len(parts) > 6 {
			parts = parts[:6]
			truncated = true
		}
		rep.Mapping = strings.Join(parts, "; ")
		if truncated {
			rep.Mapping += fmt.Sprintf(" (+%d more)", len(drift)-6)
		}
		// A drift verdict outranks the additive hint: the field types are the
		// part an operator has to act on.
		rep.Hint = "stored field types cannot be changed in place: re-index into a fresh index " +
			"(docs/how-it-works.md, Re-indexing a drifted store). Until then exact filters on the " +
			"drifted fields can miss values."
	case len(missing) > 0:
		parts := missing
		truncated := false
		if len(parts) > 6 {
			parts = parts[:6]
			truncated = true
		}
		rep.Mapping = "not yet mapped: " + strings.Join(parts, ", ")
		if truncated {
			rep.Mapping += fmt.Sprintf(" (+%d more)", len(missing)-6)
		}
		if rep.Hint == "" {
			rep.Hint = "these fields are added by an additive mapping update on the next write; " +
				"if they stay missing, the store role cannot update mappings (docs/setup.md, roles)"
		}
	default:
		rep.Mapping = "ok"
	}
	return rep
}

// storedType renders a stored property the way expectedFieldTypes labels it.
func storedType(f storedMappingField) string {
	if f.Enabled != nil && !*f.Enabled {
		return "object(disabled)"
	}
	if f.Type == "" {
		return "text" // OpenSearch defaults a bare {fields:...} leaf to text
	}
	return f.Type
}
