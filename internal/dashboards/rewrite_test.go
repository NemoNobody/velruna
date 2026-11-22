package dashboards

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFieldsFromMappingBakesTheBundleShape(t *testing.T) {
	var props map[string]MappingField
	// A mapping fragment in the shape the store answers: a text field with a
	// keyword subfield, an object to walk into (typed and typeless), scalars,
	// and a type this generator has no OSD equivalent for.
	if err := json.Unmarshal([]byte(`{
		"value": {"type": "text", "fields": {"keyword": {"type": "keyword"}}},
		"model": {"properties": {"name": {"type": "keyword"}}},
		"nested_obj": {"type": "object", "properties": {"deep": {"type": "keyword"}}},
		"observed_at": {"type": "date"},
		"ttl_days": {"type": "integer"},
		"confirmed_by": {"type": "object", "enabled": false},
		"dead": {"type": "object", "enabled": false, "properties": {"a": {"type": "keyword"}}},
		"dyn": {"type": "object"},
		"secret_html": {"type": "text", "index": false},
		"vector": {"type": "knn_vector"},
		"floaty": {"type": "float"}
	}`), &props); err != nil {
		t.Fatal(err)
	}

	fields, skipped := FieldsFromMapping(props)
	var names []string
	for _, f := range fields {
		names = append(names, f.Name)
	}
	want := "_index,model.name,nested_obj.deep,observed_at,secret_html,ttl_days,value,value.keyword"
	if got := strings.Join(names, ","); got != want {
		t.Errorf("field names = %s, want %s", got, want)
	}
	// index:false keeps the value stored but out of the index: the field
	// must appear (it exists) but never as searchable.
	for _, f := range fields {
		if f.Name == "secret_html" && f.Searchable {
			t.Error("index:false field baked as searchable")
		}
	}
	// Types without an OSD equivalent are reported, never dropped in
	// silence. Not reported: a deliberately disabled object (even one that
	// still carries properties — baking those would list fields the store
	// refuses to index) and an EMPTY object container, where nothing was
	// lost because there was nothing to bake.
	if strings.Join(skipped, ",") != "float,knn_vector" {
		t.Errorf("skipped = %v, want float and knn_vector reported", skipped)
	}

	rendered, err := RenderFields(fields)
	if err != nil {
		t.Fatalf("RenderFields: %v", err)
	}
	// The exact spacing and key order the bundle stores: a generator that
	// reformats them turns a mapping change into an unreviewable diff.
	wantStart := `[{"name": "_index", "type": "string", "esTypes": ["_index"], "count": 0, ` +
		`"scripted": false, "searchable": true, "aggregatable": true, "readFromDocValues": false}`
	if !strings.HasPrefix(rendered, wantStart) {
		if len(rendered) > 160 {
			t.Errorf("rendered head = %s, want %s", rendered[:160], wantStart)
		} else {
			t.Errorf("rendered head = %s, want %s", rendered, wantStart)
		}
	}
	var back []Field
	if err := json.Unmarshal([]byte(rendered), &back); err != nil {
		t.Fatalf("rendered list is not valid JSON: %v", err)
	}
	if len(back) != len(fields) {
		t.Errorf("round trip lost entries: %d vs %d", len(back), len(fields))
	}
	// text stays non-aggregatable, keyword becomes an aggregatable string.
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if f := byName["value"]; f.Type != "string" || f.Aggregatable || f.ReadFromDocValues {
		t.Errorf("text field baked wrong: %+v", f)
	}
	if f := byName["value.keyword"]; f.Type != "string" || !f.Aggregatable || !f.ReadFromDocValues || len(f.EsTypes) != 1 || f.EsTypes[0] != "keyword" {
		t.Errorf("keyword subfield baked wrong: %+v", f)
	}
	if f := byName["ttl_days"]; f.Type != "number" {
		t.Errorf("integer must normalize to number: %+v", f)
	}
}

// Field names reach the bundle through jsonString: quotes and backslashes
// must survive the bake and the parse-back round trip exactly.
func TestRenderFieldsEscapesHostileNames(t *testing.T) {
	fields := []Field{{Name: `we"ird\name`, Type: "string", EsTypes: []string{"keyword"}, Searchable: true, Aggregatable: true}}
	rendered, err := RenderFields(fields)
	if err != nil {
		t.Fatal(err)
	}
	var back []Field
	if err := json.Unmarshal([]byte(rendered), &back); err != nil {
		t.Fatalf("rendered list is not valid JSON: %v (%s)", err, rendered)
	}
	if len(back) != 1 || back[0].Name != `we"ird\name` {
		t.Errorf("escape round trip wrong: %+v (%s)", back, rendered)
	}
}

func TestRewriteFieldsPreservesBlankLines(t *testing.T) {
	// The O(1) sawContent fix must not change behavior: leading blank lines
	// are dropped, interior and trailing ones are kept. The committed bundle
	// itself has no blank lines, so this pins the generator's blank-line
	// contract for any future bundle that does.
	bundle := []byte("\n\n{\"id\": \"p1\", \"type\": \"index-pattern\", \"attributes\": {\"title\": \"velruna\", \"timeFieldName\": \"created_at\", \"fields\": \"[{\\\"name\\\": \\\"old\\\"}]\"}}\n\n{\"id\": \"v1\", \"type\": \"visualization\", \"attributes\": {\"title\": \"Facts\"}}\n\n")
	newFields := `[{"name": "value", "type": "string", "esTypes": ["text"], "count": 0, "scripted": false, "searchable": true, "aggregatable": false, "readFromDocValues": false}]`

	out, err := RewriteFields(bundle, map[string]string{"velruna": newFields})
	if err != nil {
		t.Fatalf("RewriteFields: %v", err)
	}
	want := "{\"id\": \"p1\", \"type\": \"index-pattern\", \"attributes\": {\"title\": \"velruna\", \"timeFieldName\": \"created_at\", \"fields\": \"" + strings.ReplaceAll(newFields, `"`, `\"`) + "\"}}\n\n{\"id\": \"v1\", \"type\": \"visualization\", \"attributes\": {\"title\": \"Facts\"}}\n\n"
	if string(out) != want {
		t.Errorf("output =\n%q\nwant\n%q", string(out), want)
	}
}

func TestRewriteFieldsTouchesOnlyTheTarget(t *testing.T) {
	bundle := []byte(`{"id": "p1", "type": "index-pattern", "attributes": {"title": "velruna", "timeFieldName": "created_at", "fields": "[{\"name\": \"old\"}]"}}
{"id": "v1", "type": "visualization", "attributes": {"title": "Facts \u0026 notes", "visState": "{\"aggs\":[{\"id\":\"1\"}]}"}, "references": [{"name": "x", "id": "p1", "type": "index-pattern"}]}
`)
	newFields := `[{"name": "value", "type": "string", "esTypes": ["text"], "count": 0, "scripted": false, "searchable": true, "aggregatable": false, "readFromDocValues": false}]`

	out, err := RewriteFields(bundle, map[string]string{"velruna": newFields})
	if err != nil {
		t.Fatalf("RewriteFields: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("line count = %d, want 2:\n%s", len(lines), out)
	}
	original := strings.Split(strings.TrimRight(string(bundle), "\n"), "\n")
	// The untouched line keeps every byte: the visualization's embedded JSON
	// string, its escaped ampersand and the references array must not drift.
	if lines[1] != original[1] {
		t.Errorf("untouched line changed:\n%s\n->\n%s", original[1], lines[1])
	}
	var obj struct {
		Attributes struct {
			Title         string `json:"title"`
			TimeFieldName string `json:"timeFieldName"`
			Fields        string `json:"fields"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &obj); err != nil {
		t.Fatalf("rewritten line does not parse: %v", err)
	}
	if obj.Attributes.Title != "velruna" || obj.Attributes.TimeFieldName != "created_at" {
		t.Errorf("attributes rewritten wrongly: %+v", obj.Attributes)
	}
	if obj.Attributes.Fields != newFields {
		t.Errorf("fields not replaced: %s", obj.Attributes.Fields)
	}
	// Key order of the target line survives too.
	if !strings.HasPrefix(lines[0], `{"id": "p1", "type": "index-pattern", "attributes": {"title": "velruna", "timeFieldName": "created_at", "fields": `) {
		t.Errorf("key order changed:\n%s", lines[0])
	}
}

func TestRewriteFieldsRefusesABundleWithoutFields(t *testing.T) {
	bundle := []byte(`{"id": "p1", "type": "index-pattern", "attributes": {"title": "velruna"}}` + "\n")
	if _, err := RewriteFields(bundle, map[string]string{"velruna": `[]`}); err == nil {
		t.Fatal("a bundle without attributes.fields must be refused, not silently inserted into")
	}
}
