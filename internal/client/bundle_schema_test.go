package client

import (
	"encoding/json"
	"testing"

	"github.com/NemoNobody/velruna/internal/dashboards"
)

// currentBakedFields renders the field list the CURRENT schema would bake
// for each known pattern, keyed by pattern title. (The live generator needs
// a store; this pins the same contract from the mapping bodies the client
// itself writes.)
func currentBakedFields(t *testing.T) map[string]string {
	t.Helper()
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
	return want
}

// The baked field lists in the embedded bundle must equal what the CURRENT
// schema would bake: a mapping change with a forgotten
// `velruna dashboards gen-fields` fails here instead of surprising the
// operator with empty panels later.
func TestBundleFieldListsMatchTheCurrentSchema(t *testing.T) {
	want := currentBakedFields(t)

	objs, err := dashboards.Parse(dashboards.Bundle())
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
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
			t.Errorf("bundle carries an unknown index-pattern title %q", attrs.Title)
			continue
		}
		found[attrs.Title] = true
		if attrs.Fields != w {
			t.Errorf("bundle field list for %s is stale: re-run 'velruna dashboards gen-fields' and commit the bundle", attrs.Title)
		}
	}
	if len(found) != len(want) {
		// Count distinct titles: two objects sharing one title must not
		// mask a pattern the bundle lost.
		t.Errorf("bundle carried %d of %d known pattern titles", len(found), len(want))
	}
}

// The pattern time fields, the visualization aggregations and filters, and
// the dashboard panel refs must resolve against the same schema the field
// lists are baked from. Regenerating the field lists after a mapping rename
// would otherwise leave a terms aggregation or a time filter pointing at a
// field OSD cannot find — "Could not locate that index-pattern-field" at
// panel time, with green tests.
func TestBundleObjectsResolveAgainstTheCurrentSchema(t *testing.T) {
	fieldSets := map[string]map[string]dashboards.Field{}
	for title, rendered := range currentBakedFields(t) {
		var fs []dashboards.Field
		if err := json.Unmarshal([]byte(rendered), &fs); err != nil {
			t.Fatalf("%s: baked list does not parse: %v", title, err)
		}
		set := make(map[string]dashboards.Field, len(fs))
		for _, f := range fs {
			set[f.Name] = f
		}
		fieldSets[title] = set
	}

	objs, err := dashboards.Parse(dashboards.Bundle())
	if err != nil {
		t.Fatal(err)
	}

	// The field each pattern is a time-based index on. Changing one is a
	// deliberate, reviewed change: existing saved searches and panel time
	// filters break when it moves.
	wantTime := map[string]string{
		"velruna":           "updated_at",
		"velruna-history-*": "created_at",
		"velruna-docs":      "fetched_at",
	}
	patternTitleByID := map[string]string{}
	visIDs := map[string]bool{}
	for _, o := range objs {
		switch o.Type {
		case "index-pattern":
			var attrs struct {
				Title         string `json:"title"`
				TimeFieldName string `json:"timeFieldName"`
			}
			if err := json.Unmarshal(o.Attributes, &attrs); err != nil {
				t.Fatal(err)
			}
			set, known := fieldSets[attrs.Title]
			if !known {
				// Do not register it: a registered unknown title turns every
				// field check of every visualization hanging on that pattern
				// into "absent from pattern <unknown>" noise.
				t.Errorf("bundle carries an unknown index-pattern title %q", attrs.Title)
				continue
			}
			patternTitleByID[o.ID] = attrs.Title
			if attrs.TimeFieldName != wantTime[attrs.Title] {
				t.Errorf("pattern %s: timeFieldName = %q, want %q", attrs.Title, attrs.TimeFieldName, wantTime[attrs.Title])
				continue
			}
			f, found := set[attrs.TimeFieldName]
			if !found {
				t.Errorf("pattern %s: timeFieldName %q is not in its own field list", attrs.Title, attrs.TimeFieldName)
			} else if f.Type != "date" {
				t.Errorf("pattern %s: timeFieldName %q is type %q, want date", attrs.Title, attrs.TimeFieldName, f.Type)
			}
		case "visualization":
			visIDs[o.ID] = true
		}
	}

	for _, o := range objs {
		if o.Type != "visualization" {
			continue
		}
		var refs []struct {
			Name string `json:"name"`
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(o.References, &refs); err != nil {
			t.Fatalf("visualization %s: references: %v", o.ID, err)
		}
		title := ""
		refNameToTitle := map[string]string{}
		seenPatternIDs := map[string]bool{}
		patternRefs := 0
		for _, r := range refs {
			if r.Type == "index-pattern" {
				if !seenPatternIDs[r.ID] {
					seenPatternIDs[r.ID] = true
					patternRefs++
				}
				if ti, ok := patternTitleByID[r.ID]; ok {
					title = ti
					refNameToTitle[r.Name] = ti
				}
			}
		}
		if patternRefs > 1 {
			// A vis with several distinct pattern refs would make the
			// empty-meta.index fallback below (and the "its own pattern"
			// reading) arbitrary — skip the field checks, one clear error.
			t.Errorf("visualization %s carries %d distinct index-pattern refs; the fallback pattern would be ambiguous", o.ID, patternRefs)
			continue
		}
		if title == "" {
			t.Errorf("visualization %s references no known index-pattern", o.ID)
			continue
		}
		set := fieldSets[title]

		var attrs struct {
			VisState string `json:"visState"`
			Meta     struct {
				SearchSourceJSON string `json:"searchSourceJSON"`
			} `json:"kibanaSavedObjectMeta"`
		}
		if err := json.Unmarshal(o.Attributes, &attrs); err != nil {
			t.Fatalf("visualization %s: attributes: %v", o.ID, err)
		}

		var vis struct {
			Aggs []struct {
				ID     string `json:"id"`
				Type   string `json:"type"`
				Params struct {
					Field string `json:"field"`
				} `json:"params"`
			} `json:"aggs"`
		}
		if err := json.Unmarshal([]byte(attrs.VisState), &vis); err != nil {
			t.Fatalf("visualization %s: visState: %v", o.ID, err)
		}
		for _, agg := range vis.Aggs {
			if agg.Params.Field == "" { // count-style aggs carry no field
				continue
			}
			f, ok := set[agg.Params.Field]
			if !ok {
				t.Errorf("visualization %s: agg %q (%s) points at %q, absent from pattern %s", o.ID, agg.ID, agg.Type, agg.Params.Field, title)
				continue
			}
			if !f.Aggregatable {
				t.Errorf("visualization %s: agg %q (%s) aggregates the non-aggregatable field %q", o.ID, agg.ID, agg.Type, agg.Params.Field)
			}
			if agg.Type == "date_histogram" && f.Type != "date" {
				t.Errorf("visualization %s: date_histogram on %q of type %q — OSD rejects it at panel time", o.ID, agg.Params.Field, f.Type)
			}
		}

		var ss struct {
			Filter []struct {
				Meta struct {
					Index string `json:"index"`
					Key   string `json:"key"`
				} `json:"meta"`
			} `json:"filter"`
		}
		if err := json.Unmarshal([]byte(attrs.Meta.SearchSourceJSON), &ss); err != nil {
			t.Fatalf("visualization %s: searchSourceJSON: %v", o.ID, err)
		}
		for _, fl := range ss.Filter {
			if fl.Meta.Key == "" {
				// A filter without meta.key is a grouped/nested one: its field
				// lives in the inner query. The walker only handles flat filters
				// (the current bundle's shape) — extend it before adding groups.
				t.Errorf("visualization %s: grouped filter with empty meta.key — extend the walker", o.ID)
				continue
			}
			// meta.index names the pattern the filter hangs on: a pattern id
			// directly, or a reference name (the exporter writes the sentinel
			// "kibanaSavedObjectMeta.searchSourceJSON.index" for the vis's own
			// pattern, which resolves through the references).
			fset, ftitle := set, title
			if fl.Meta.Index != "" {
				ft, ok := patternTitleByID[fl.Meta.Index]
				if !ok {
					ft, ok = refNameToTitle[fl.Meta.Index]
				}
				if !ok {
					t.Errorf("visualization %s: filter on %q names pattern %q the test cannot resolve (unknown pattern id or reference name)", o.ID, fl.Meta.Key, fl.Meta.Index)
					continue
				}
				f2, ok := fieldSets[ft]
				if !ok {
					t.Errorf("visualization %s: filter names pattern %q the bundle cannot bake", o.ID, ft)
					continue
				}
				fset, ftitle = f2, ft
			}
			f, ok := fset[fl.Meta.Key]
			if !ok {
				t.Errorf("visualization %s: filter on %q, absent from pattern %s", o.ID, fl.Meta.Key, ftitle)
				continue
			}
			if !f.Searchable {
				t.Errorf("visualization %s: filter on the non-searchable field %q — OSD finds nothing for it", o.ID, fl.Meta.Key)
			}
		}
	}

	for _, o := range objs {
		if o.Type != "dashboard" {
			continue
		}
		var refs []struct {
			Name string `json:"name"`
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(o.References, &refs); err != nil {
			t.Fatalf("dashboard: references: %v", err)
		}
		refNames := map[string]bool{}
		for _, r := range refs {
			refNames[r.Name] = true
			if r.Type == "visualization" && !visIDs[r.ID] {
				t.Errorf("dashboard references visualization %q the bundle does not carry", r.ID)
			}
		}
		var attrs struct {
			PanelsJSON string `json:"panelsJSON"`
		}
		if err := json.Unmarshal(o.Attributes, &attrs); err != nil {
			t.Fatalf("dashboard: attributes: %v", err)
		}
		var panels []struct {
			PanelRefName string `json:"panelRefName"`
		}
		if err := json.Unmarshal([]byte(attrs.PanelsJSON), &panels); err != nil {
			t.Fatalf("dashboard: panelsJSON: %v", err)
		}
		for _, p := range panels {
			if !refNames[p.PanelRefName] {
				t.Errorf("dashboard panel %q references no saved-object ref", p.PanelRefName)
			}
		}
	}
}
