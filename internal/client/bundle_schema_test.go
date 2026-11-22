package client

import (
	"encoding/json"
	"testing"

	"github.com/NemoNobody/velruna/internal/dashboards"
)

// The baked field lists in the embedded bundle must equal what the CURRENT
// schema would bake: a mapping change with a forgotten
// `velruna dashboards gen-fields` fails here instead of surprising the
// operator with empty panels later. (The live generator needs a store; this
// test pins the same contract from the mapping bodies the client itself
// writes.)
func TestBundleFieldListsMatchTheCurrentSchema(t *testing.T) {
	propsOf := func(body map[string]interface{}) map[string]dashboards.MappingField {
		mappings, _ := body["mappings"].(map[string]interface{})
		raw, err := json.Marshal(mappings["properties"])
		if err != nil {
			t.Fatal(err)
		}
		var props map[string]dashboards.MappingField
		if err := json.Unmarshal(raw, &props); err != nil {
			t.Fatal(err)
		}
		return props
	}
	journal := buildJournalTemplateBody("velruna-history-", 0)
	tmpl, _ := journal["template"].(map[string]interface{})

	want := map[string]string{}
	for title, body := range map[string]map[string]interface{}{
		"velruna":           buildIndexBody(0),
		"velruna-history-*": tmpl,
		"velruna-docs":      buildDocsIndexBody(0),
	} {
		fields, skipped := dashboards.FieldsFromMapping(propsOf(body))
		if len(skipped) > 0 {
			t.Errorf("%s: current schema bakes skipped types %v — the OSD table or the mapping moved", title, skipped)
		}
		rendered, err := dashboards.RenderFields(fields)
		if err != nil {
			t.Fatalf("%s: render: %v", title, err)
		}
		want[title] = rendered
	}

	objs, err := dashboards.Parse(dashboards.Bundle())
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, o := range objs {
		if o.Type != "index-pattern" {
			continue
		}
		var attrs struct {
			Title  string `json:"title"`
			Fields string `json:"fields"`
		}
		if err := json.Unmarshal(o.Attributes, &attrs); err != nil {
			t.Fatal(err)
		}
		w, ok := want[attrs.Title]
		if !ok {
			continue
		}
		found++
		if attrs.Fields != w {
			t.Errorf("bundle field list for %s is stale: re-run 'velruna dashboards gen-fields' and commit the bundle", attrs.Title)
		}
	}
	if found != len(want) {
		t.Errorf("bundle carried %d of %d known patterns", found, len(want))
	}
}
