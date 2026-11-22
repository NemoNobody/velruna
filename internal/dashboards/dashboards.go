// Package dashboards carries the ready-made OpenSearch Dashboards bundle
// (index patterns, visualizations and the "Velruna — store overview"
// dashboard) together with the rules that turn a bundle line into a saved
// object document.
//
// The bundle is embedded in the binary on purpose: installing it must work
// from the single executable, including a distroless container where no
// python, jq or bundle file is available. The field lists are baked by
// `velruna dashboards gen-fields` (cmd/dashboards.go), a development tool that
// reads a live store's mappings.
package dashboards

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"
)

// objectsNDJSON is the generator output; keep internal/dashboards/objects.ndjson
// in sync by re-running `velruna dashboards gen-fields` after a mapping change.
//
//go:embed objects.ndjson
var objectsNDJSON []byte

// Bundle returns the embedded saved-object bundle, one JSON object per line.
func Bundle() []byte { return objectsNDJSON }

// MigrationVersion is the per-type migrationVersion OpenSearch Dashboards 3.x
// writes itself when the UI saves an object. The values are pinned per type
// rather than computed: the bundle goes straight into the .kibana index, so
// nothing else fills them in, and a wrong value makes the UI drop the object.
var MigrationVersion = map[string]string{
	"index-pattern": "7.6.0",
	"visualization": "7.10.0",
	"dashboard":     "7.9.3",
}

// Object is one saved object as it appears in the bundle file.
type Object struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Attributes json.RawMessage `json:"attributes"`
	References json.RawMessage `json:"references,omitempty"`
}

// DocumentID is the _id the object takes in the saved-objects index: the
// "<type>:<id>" shape the UI itself writes.
func (o Object) DocumentID() string { return o.Type + ":" + o.ID }

// Parse reads a bundle: one JSON object per line, blank lines ignored.
func Parse(bundle []byte) ([]Object, error) {
	sc := bufio.NewScanner(bytes.NewReader(bundle))
	// Attributes carry the baked field lists (tens of KB per index pattern),
	// so the default 64 KB line limit is too small.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var objs []Object
	seen := make(map[string]struct{})
	line := 0
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var o Object
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, fmt.Errorf("bundle line %d: %w", line, err)
		}
		if o.ID == "" || o.Type == "" {
			return nil, fmt.Errorf("bundle line %d: object without id or type", line)
		}
		if len(o.Attributes) == 0 {
			return nil, fmt.Errorf("bundle line %d (%s): object without attributes", line, o.DocumentID())
		}
		// "attributes": null or an array passes a length check but serialises to
		// a document the UI silently drops.
		if attr := bytes.TrimSpace(o.Attributes); len(attr) == 0 || attr[0] != '{' {
			return nil, fmt.Errorf("bundle line %d (%s): attributes must be a JSON object", line, o.DocumentID())
		}
		// A duplicate would overwrite the earlier object: "indexed 22" would
		// then mean 21 installed, and a generator bug would look like a clean run.
		if _, dup := seen[o.DocumentID()]; dup {
			return nil, fmt.Errorf("bundle line %d: duplicate object %s", line, o.DocumentID())
		}
		seen[o.DocumentID()] = struct{}{}
		objs = append(objs, o)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read bundle: %w", err)
	}
	if len(objs) == 0 {
		return nil, fmt.Errorf("bundle is empty")
	}
	return objs, nil
}

// Document renders the document to index for one saved object: the OSD 3.x
// typed shape, with the attributes nested under a field named after the type.
func (o Object) Document(now time.Time) ([]byte, error) {
	ver, ok := MigrationVersion[o.Type]
	if !ok {
		return nil, fmt.Errorf("unsupported saved object type %q (%s): the bundle was generated for a "+
			"different OpenSearch Dashboards version", o.Type, o.DocumentID())
	}
	refs := []json.RawMessage{}
	if len(o.References) > 0 {
		if err := json.Unmarshal(o.References, &refs); err != nil {
			return nil, fmt.Errorf("%s: references must be an array: %w", o.DocumentID(), err)
		}
		if refs == nil {
			// A "references": null decodes to a nil slice, and the UI then sees
			// no relation to the index pattern; the document needs [].
			refs = []json.RawMessage{}
		}
	}
	refsJSON, err := json.Marshal(refs)
	if err != nil {
		return nil, fmt.Errorf("encode references of %s: %w", o.DocumentID(), err)
	}
	doc := make(map[string]json.RawMessage, 5)
	set := func(key string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("encode %s of %s: %w", key, o.DocumentID(), err)
		}
		doc[key] = b
		return nil
	}
	if err := set("type", o.Type); err != nil {
		return nil, err
	}
	// The UI stores millisecond precision; second precision would make an
	// object written today look older than one written by the UI a moment ago.
	if err := set("updated_at", now.UTC().Format("2006-01-02T15:04:05.000Z")); err != nil {
		return nil, err
	}
	if err := set("migrationVersion", map[string]string{o.Type: ver}); err != nil {
		return nil, err
	}
	doc["references"] = refsJSON
	doc[o.Type] = o.Attributes
	return json.Marshal(doc)
}
