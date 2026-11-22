package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// mappingBodyFrom renders a store mapping response with the wanted types, so
// the test states the drift instead of hand-writing 25 fields.
func mappingBodyFrom(t *testing.T, marker *int, overrides map[string]string) string {
	t.Helper()
	types := expectedFieldTypes()
	for name, ty := range overrides {
		types[name] = ty
	}
	return mappingBody(t, map[string]map[string]any{"velruna": mappingIndex(marker, propsFromTypes(types))})
}

func propsFromTypes(types map[string]string) map[string]map[string]any {
	props := map[string]map[string]any{}
	for name, ty := range types {
		if ty == "?" {
			continue
		}
		if ty == "object(disabled)" {
			props[name] = map[string]any{"enabled": false}
			continue
		}
		props[name] = map[string]any{"type": ty}
	}
	return props
}

// mappingIndex renders one index's mappings block.
func mappingIndex(marker *int, props map[string]map[string]any) map[string]any {
	meta := map[string]any{}
	if marker != nil {
		meta[schemaMetaKey] = *marker
	}
	return map[string]any{"mappings": map[string]any{"_meta": meta, "properties": props}}
}

// mappingBody renders a GET /_mapping response over several indices (an alias
// resolves to more than one).
func mappingBody(t *testing.T, indices map[string]map[string]any) string {
	t.Helper()
	body, err := json.Marshal(indices)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestSchemaCheck_Clean(t *testing.T) {
	v := SchemaVersion
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusOK, mappingBodyFrom(t, &v, nil)
	})
	rep := c.SchemaCheck(context.Background())
	if rep.Schema != "v1" || rep.Mapping != "ok" || rep.Hint != "" {
		t.Errorf("clean store reported wrongly: %+v", rep)
	}
}

// A keywords array that a dynamic mapping pinned as text before the mapping
// declared it: reads keep working, exact tag filters silently miss. The check
// has to name it and point at the re-index recipe.
func TestSchemaCheck_NamesFieldTypeDrift(t *testing.T) {
	v := SchemaVersion
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusOK, mappingBodyFrom(t, &v, map[string]string{"keywords": "text"})
	})
	rep := c.SchemaCheck(context.Background())
	if !strings.Contains(rep.Mapping, "keywords: text, want keyword") {
		t.Errorf("drift not named: %q", rep.Mapping)
	}
	if !strings.Contains(rep.Hint, "re-index") {
		t.Errorf("the drift hint must name the fix: %q", rep.Hint)
	}
}

func TestSchemaCheck_MissingFieldsAreNotDrift(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		// A store adopted from 0.8: only the old fields are mapped.
		return http.StatusOK, `{"velruna":{"mappings":{"_meta":{"` + schemaMetaKey + `":1},"properties":{
			"key": {"type": "keyword"}, "value": {"type": "text"}, "category": {"type": "keyword"},
			"project": {"type": "keyword"}, "status": {"type": "keyword"}, "created_at": {"type": "date"},
			"updated_at": {"type": "date"}, "observed_at": {"type": "date"}, "source": {"type": "keyword"},
			"notes": {"type": "text"}, "history": {"enabled": false}}}}}`
	})
	rep := c.SchemaCheck(context.Background())
	if !strings.HasPrefix(rep.Mapping, "not yet mapped:") {
		t.Errorf("missing additive fields: %q", rep.Mapping)
	}
	if strings.Contains(rep.Mapping, "keywords: ") {
		t.Errorf("a missing field must not be reported as a type drift: %q", rep.Mapping)
	}
	if !strings.Contains(rep.Hint, "additive mapping update") {
		t.Errorf("hint should name the additive update: %q", rep.Hint)
	}
}

func TestSchemaCheck_UnversionedAndNewer(t *testing.T) {
	two := SchemaVersion + 1
	cases := []struct {
		name       string
		marker     *int
		wantSchema string
		wantHint   string
	}{
		{"no marker", nil, "unversioned (store created before schema markers)", "adopted"},
		{"newer store", &two, "newer (v2)", "upgrade velruna"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
				return http.StatusOK, mappingBodyFrom(t, tc.marker, nil)
			})
			rep := c.SchemaCheck(context.Background())
			if rep.Schema != tc.wantSchema {
				t.Errorf("schema = %q, want %q", rep.Schema, tc.wantSchema)
			}
			if !strings.Contains(rep.Hint, tc.wantHint) {
				t.Errorf("hint = %q, want it to mention %q", rep.Hint, tc.wantHint)
			}
		})
	}
}

// A role that may write but not read mappings must get an explicit "we cannot
// verify", not a clean bill of health.
func TestSchemaCheck_UnreadableIsExplicit(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusForbidden, `{"status":403,"error":{"type":"security_exception"}}`
	})
	rep := c.SchemaCheck(context.Background())
	if rep.Schema != "unreadable (HTTP 403)" {
		t.Errorf("schema = %q", rep.Schema)
	}
	if rep.Mapping != "" {
		t.Errorf("no mapping verdict when it could not be read: %q", rep.Mapping)
	}
	if !strings.Contains(rep.Hint, "cannot read mappings") {
		t.Errorf("hint should say what to fix: %q", rep.Hint)
	}
}

func TestSchemaCheck_AbsentIndex(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusNotFound, `{"error":{"type":"index_not_found_exception"}}`
	})
	rep := c.SchemaCheck(context.Background())
	if !rep.IndexAbsent || !strings.HasPrefix(rep.Schema, "absent") {
		t.Errorf("absent index: %+v", rep)
	}
}

// A marker that exists but is not an integer is "unknown", never "unversioned":
// stamping over it would overwrite a newer scheme's marker with a lie.
func TestSchemaCheck_NonNumericMarkerIsUnknown(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusOK, `{"velruna":{"mappings":{"_meta":{"` + schemaMetaKey + `":"future"},"properties":{"key":{"type":"keyword"}}}}}`
	})
	rep := c.SchemaCheck(context.Background())
	if !strings.Contains(rep.Schema, "not a number") {
		t.Errorf("schema = %q, want the unknown-marker verdict", rep.Schema)
	}
}

// HTTP 200 with an empty object means the name resolved to no indices at all
// (an alias without backing). That is absence, not "unversioned": the storage
// line in the same status output already says index=absent, and claiming
// otherwise prints two contradictory rows.
func TestSchemaCheck_EmptyBodyIsAbsent(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusOK, `{}`
	})
	rep := c.SchemaCheck(context.Background())
	if !rep.IndexAbsent || !strings.HasPrefix(rep.Schema, "absent") || rep.Mapping != "" {
		t.Errorf("empty mapping body reported wrongly: %+v", rep)
	}
}

// Fields the store carries that this binary does not know are NOT drift: the
// opposite direction (store older than binary) is additive, and a store from
// a newer scheme legitimately has extra fields.
func TestSchemaCheck_ExtraStoreFieldsAreNotDrift(t *testing.T) {
	v := SchemaVersion
	props := propsFromTypes(expectedFieldTypes())
	props["future_field"] = map[string]any{"type": "keyword"}
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusOK, mappingBody(t, map[string]map[string]any{"velruna": mappingIndex(&v, props)})
	})
	rep := c.SchemaCheck(context.Background())
	if rep.Mapping != "ok" {
		t.Errorf("extra store field reported as drift: %q", rep.Mapping)
	}
}

// An alias over two backing indices: the newest marker wins, and properties
// merge in sorted-name order (first definition wins). This pins that merge
// semantics so a future change is a decision, not an accident.
func TestSchemaCheck_AliasMerge(t *testing.T) {
	v := SchemaVersion
	clean := propsFromTypes(expectedFieldTypes())
	old := propsFromTypes(expectedFieldTypes())
	old["keywords"] = map[string]any{"type": "text"}
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusOK, mappingBody(t, map[string]map[string]any{
			"a_old": mappingIndex(nil, old),
			"z_new": mappingIndex(&v, clean),
		})
	})
	rep := c.SchemaCheck(context.Background())
	if rep.Schema != "v1" {
		t.Errorf("marker across aliases = %q, want v1 (newest wins)", rep.Schema)
	}
	if !strings.Contains(rep.Mapping, "keywords: text") {
		t.Errorf("merge should surface the old backing's pinned type; mapping = %q", rep.Mapping)
	}
}

// A typeless property with children is an object, not a text leaf: naming the
// drift label wrongly sends the operator chasing the wrong field shape.
func TestSchemaCheck_TypelessObjectIsLabelledObject(t *testing.T) {
	v := SchemaVersion
	props := propsFromTypes(expectedFieldTypes())
	delete(props, "confirmed_by")
	props["confirmed_by"] = map[string]any{"properties": map[string]any{"author": map[string]any{"type": "keyword"}}}
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusOK, mappingBody(t, map[string]map[string]any{"velruna": mappingIndex(&v, props)})
	})
	rep := c.SchemaCheck(context.Background())
	if !strings.Contains(rep.Mapping, "confirmed_by: object, want object(disabled)") {
		t.Errorf("drift label = %q, want the object verdict for confirmed_by", rep.Mapping)
	}
}
