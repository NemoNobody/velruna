package dashboards

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// OSD 3.x has no externally callable field-introspection API, so the bundle
// carries the fields up front: a pattern with fields:[] cannot resolve its
// timeFieldName and every visualization dies with "Could not locate that
// index-pattern-field". `velruna dashboards gen-fields` bakes those lists from
// a live store after a mapping change; the rules below are what it bakes.

// fieldType is one entry of the es-type → OSD-field-type table.
type fieldType struct {
	osdType string
	aggs    bool
	docs    bool
}

// fieldTypes maps an OpenSearch mapping type to the OpenSearch Dashboards field
// type plus its aggregatable/readFromDocValues flags. The "type" must be the
// OSD field type (the OSD_FIELD_TYPES enum), not the raw es type:
// terms.buckets filterFieldTypes match it exactly, and a saved field of type
// "keyword" fails with "is invalid for use with the Terms aggregation" — es
// type keyword normalizes to OSD "string".
var fieldTypes = map[string]fieldType{
	"text":    {"string", false, false},
	"keyword": {"string", true, true},
	"date":    {"date", true, true},
	"boolean": {"boolean", true, true},
	"long":    {"number", true, true},
	"integer": {"number", true, true},
	"short":   {"number", true, true},
	"byte":    {"number", true, true},
}

// Field is one entry of a saved index pattern's field list, in the key order
// OSD itself writes.
type Field struct {
	Name              string   `json:"name"`
	Type              string   `json:"type"`
	EsTypes           []string `json:"esTypes"`
	Count             int      `json:"count"`
	Scripted          bool     `json:"scripted"`
	Searchable        bool     `json:"searchable"`
	Aggregatable      bool     `json:"aggregatable"`
	ReadFromDocValues bool     `json:"readFromDocValues"`
}

// metaFields are fields OSD exposes without them being in the mapping.
// _index must be in the saved field list or aggregations on it fail with
// "Could not locate that index-pattern-field".
var metaFields = []Field{
	{
		Name:              "_index",
		Type:              "string",
		EsTypes:           []string{"_index"},
		Searchable:        true,
		Aggregatable:      true,
		ReadFromDocValues: false,
	},
}

// MappingField is one property in an index mapping: either a leaf (with a
// "type"), an object (with nested "properties"), a deliberately disabled
// object ("enabled": false — not a field, and not a missing type), or a leaf
// with multi-fields (with "fields"). Exported so the store layer can decode
// mappings into it.
type MappingField struct {
	Type       string                  `json:"type"`
	Enabled    *bool                   `json:"enabled"`
	Properties map[string]MappingField `json:"properties"`
	Fields     map[string]MappingField `json:"fields"`
}

// FieldsFromMapping walks the merged mapping properties of the concrete
// indices behind a pattern and returns the OSD field list, sorted by name,
// plus the mapping types it had to skip (present in the mapping, absent from
// the OSD table — the generator reports them rather than dropping fields in
// silence). The sort keeps the bundle diff stable across runs.
func FieldsFromMapping(props map[string]MappingField) ([]Field, []string) {
	skipped := map[string]bool{}
	fields := flattenFields(props, "", skipped)
	fields = append(fields, metaFields...)
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
	names := make([]string, 0, len(skipped))
	for t := range skipped {
		names = append(names, t)
	}
	sort.Strings(names)
	return fields, names
}

func flattenFields(props map[string]MappingField, prefix string, skipped map[string]bool) []Field {
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	// The store's response order is not defined; walking in name order makes
	// the baked list reproducible.
	sort.Strings(names)

	var out []Field
	for _, name := range names {
		spec := props[name]
		path := prefix + name
		if spec.Enabled != nil && !*spec.Enabled {
			// A deliberately disabled object: the store chose not to index
			// anything under it — absence is the design, not a generator
			// gap. Checked before the container walk: a disabled object can
			// still carry properties, and baking them would list fields the
			// store refuses to index.
			continue
		}
		_, isLeaf := fieldTypes[spec.Type]
		if len(spec.Properties) > 0 && !isLeaf {
			// A container (typeless object, "object", "nested") is walked
			// into: OSD lists its leaves, not the container. velruna's own
			// mappings use typeless objects; the general case must not lose
			// fields from foreign mappings either.
			out = append(out, flattenFields(spec.Properties, path+".", skipped)...)
		}
		if isLeaf {
			ft := fieldTypes[spec.Type]
			out = append(out, Field{
				Name:              path,
				Type:              ft.osdType,
				EsTypes:           []string{spec.Type},
				Searchable:        true,
				Aggregatable:      ft.aggs,
				ReadFromDocValues: ft.docs,
			})
		} else if spec.Type != "" && len(spec.Properties) == 0 && spec.Type != "object" && spec.Type != "nested" {
			// A leaf type the OSD table does not know (float, ip,
			// date_nanos, ...): a field was lost, so it is reported, never
			// silently dropped. An EMPTY object/nested container is not a
			// lost field — there was nothing to bake — so it stays quiet.
			skipped[spec.Type] = true
		}
		// Multi-fields (e.g. a text field with a .keyword subfield) are
		// separate entries in OSD.
		if len(spec.Fields) > 0 {
			subs := make([]string, 0, len(spec.Fields))
			for sub := range spec.Fields {
				subs = append(subs, sub)
			}
			sort.Strings(subs)
			for _, sub := range subs {
				ft, ok := fieldTypes[spec.Fields[sub].Type]
				if !ok {
					if spec.Fields[sub].Type != "" {
						skipped[spec.Fields[sub].Type] = true
					}
					continue
				}
				out = append(out, Field{
					Name:              path + "." + sub,
					Type:              ft.osdType,
					EsTypes:           []string{spec.Fields[sub].Type},
					Searchable:        true,
					Aggregatable:      ft.aggs,
					ReadFromDocValues: ft.docs,
				})
			}
		}
	}
	return out
}

// RenderFields serializes a field list the way the bundle stores it: a JSON
// string with ", " and ": " separators and the keys in the order OSD writes.
// The spacing is deliberate — it is what the committed bundle already holds,
// so re-running the generator changes only the lists that really changed.
func RenderFields(fields []Field) (string, error) {
	var b []byte
	b = append(b, '[')
	for i, f := range fields {
		if i > 0 {
			b = append(b, ',', ' ')
		}
		b = append(b, '{')
		for k, kv := range []struct {
			key string
			raw []byte
		}{
			{"name", jsonString(f.Name)},
			{"type", jsonString(f.Type)},
			{"esTypes", stringSlice(f.EsTypes)},
			{"count", jsonInt(f.Count)},
			{"scripted", jsonBool(f.Scripted)},
			{"searchable", jsonBool(f.Searchable)},
			{"aggregatable", jsonBool(f.Aggregatable)},
			{"readFromDocValues", jsonBool(f.ReadFromDocValues)},
		} {
			if k > 0 {
				b = append(b, ',', ' ')
			}
			b = append(b, jsonString(kv.key)...)
			b = append(b, ':', ' ')
			b = append(b, kv.raw...)
		}
		b = append(b, '}')
	}
	b = append(b, ']')
	if !json.Valid(b) {
		return "", fmt.Errorf("rendered field list is not valid JSON")
	}
	return string(b), nil
}

// jsonString escapes like json.dumps(ensure_ascii=False): no HTML escaping,
// non-ASCII stays raw.
func jsonString(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// A Go string cannot fail to encode as JSON; keep the output sane.
		return []byte(`""`)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func stringSlice(items []string) []byte {
	var b []byte
	b = append(b, '[')
	for i, s := range items {
		if i > 0 {
			b = append(b, ',', ' ')
		}
		b = append(b, jsonString(s)...)
	}
	b = append(b, ']')
	return b
}

func jsonBool(v bool) []byte {
	if v {
		return []byte("true")
	}
	return []byte("false")
}

// jsonInt renders a field count the way the bundle stores it (always 0 in a
// freshly baked list; OSD rewrites it at runtime).
func jsonInt(v int) []byte {
	return []byte(strconv.Itoa(v))
}
