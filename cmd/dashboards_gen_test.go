package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gen-fields bakes the field lists of the bundled index patterns from the
// store's live mappings, leaves the patterns it cannot read alone, and keeps
// every other byte of the bundle where it was.
func TestDashboardsGenFieldsBakesAndPreserves(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/velruna/_mapping":
			_, _ = w.Write([]byte(`{"velruna":{"mappings":{"properties":{
				"key": {"type": "keyword"},
				"value": {"type": "text", "fields": {"keyword": {"type": "keyword"}}},
				"created_at": {"type": "date"}
			}}}}`))
		case "/velruna-docs/_mapping":
			_, _ = w.Write([]byte(`{"velruna-docs":{"mappings":{"properties":{
				"hash": {"type": "keyword"}
			}}}}`))
		case "/velruna-history-*/_mapping":
			http.Error(w, `{"error":{"type":"index_not_found_exception"}}`, http.StatusNotFound)
		default:
			http.Error(w, `{"error":"unexpected path `+r.URL.Path+`"}`, http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	conf := filepath.Join(dir, "velruna.conf")
	body, _ := json.Marshal(map[string]string{
		"opensearch_url":  srv.URL,
		"opensearch_user": "u",
	})
	if err := os.WriteFile(conf, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VELRUNA_CONF", conf)
	t.Setenv("OPENSEARCH_PASSWORD", "p")

	// A bundle in the shape the real one has: the three patterns plus a
	// visualization whose embedded JSON string must survive untouched.
	bundlePath := filepath.Join(dir, "bundle.ndjson")
	bundle := `{"id": "p1", "type": "index-pattern", "attributes": {"title": "velruna", "timeFieldName": "created_at", "fields": "[{\"name\": \"old\"}]"}}
{"id": "p2", "type": "index-pattern", "attributes": {"title": "velruna-history-*", "timeFieldName": "created_at", "fields": "[{\"name\": \"oldhist\"}]"}}
{"id": "p3", "type": "index-pattern", "attributes": {"title": "velruna-docs", "fields": "[{\"name\": \"olddocs\"}]"}}
{"id": "v1", "type": "visualization", "attributes": {"title": "Counts", "visState": "{\"aggs\":[{\"id\":\"1\"}]}"}, "references": [{"name": "x", "id": "p1", "type": "index-pattern"}]}
`
	if err := os.WriteFile(bundlePath, []byte(bundle), 0o600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "out.ndjson")

	var out, errOut bytes.Buffer
	cmd := dashboardsGenFieldsCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--conf", conf, "--file", bundlePath, "--out", outPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gen-fields: %v\n%s", err, errOut.String())
	}

	stdout := out.String()
	if !strings.Contains(stdout, "velruna: 5 field(s) baked from velruna") {
		t.Errorf("facts bake line missing:\n%s", stdout)
	}
	if !strings.Contains(stdout, "velruna-docs: 2 field(s) baked from velruna-docs") {
		t.Errorf("docs bake line missing:\n%s", stdout)
	}
	if !strings.Contains(errOut.String(), "velruna-history-*: mapping unavailable") {
		t.Errorf("an unreadable pattern must be reported on stderr:\n%s", errOut.String())
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("rewritten bundle line count = %d:\n%s", len(lines), data)
	}
	if lines[3] != strings.Split(strings.TrimRight(bundle, "\n"), "\n")[3] {
		t.Errorf("visualization line changed:\n%s\n->\n%s", strings.Split(bundle, "\n")[3], lines[3])
	}
	// The pattern that could not be read keeps its old list.
	if !strings.Contains(lines[1], `oldhist`) {
		t.Errorf("history pattern fields must stay:\n%s", lines[1])
	}
	for _, line := range []string{lines[0], lines[2]} {
		var obj struct {
			Attributes struct {
				Fields string `json:"fields"`
			} `json:"attributes"`
		}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("rewritten line does not parse: %v", err)
		}
		var fields []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(obj.Attributes.Fields), &fields); err != nil {
			t.Fatalf("baked fields string does not parse: %v", err)
		}
		if len(fields) == 0 || fields[0].Name != "_index" {
			t.Errorf("baked list wrong: %s", obj.Attributes.Fields)
		}
	}
}

// Every pattern in the bundle failed to resolve: writing a bundle whose lists
// were never read would look like a successful regeneration.
func TestDashboardsGenFieldsRefusesWhenNothingResolved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"type":"index_not_found_exception"}}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	conf := filepath.Join(dir, "velruna.conf")
	body, _ := json.Marshal(map[string]string{"opensearch_url": srv.URL, "opensearch_user": "u"})
	if err := os.WriteFile(conf, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENSEARCH_PASSWORD", "p")
	t.Setenv("VELRUNA_CONF", conf)
	bundlePath := filepath.Join(dir, "bundle.ndjson")
	bundle := `{"id": "p1", "type": "index-pattern", "attributes": {"title": "velruna", "fields": "[{\"name\": \"old\"}]"}}` + "\n"
	if err := os.WriteFile(bundlePath, []byte(bundle), 0o600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "out.ndjson")

	var out, errOut bytes.Buffer
	if err := genFields(&out, &errOut, conf, bundlePath, outPath, ""); err == nil {
		t.Fatal("nothing resolved, but the run reported success")
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Errorf("the output file must not be created when nothing resolved: %v", err)
	}

	// A bundle with no pattern this tool knows how to feed is refused too,
	// before any mapping is read.
	unknown := filepath.Join(dir, "unknown.ndjson")
	if err := os.WriteFile(unknown, []byte(`{"id": "p9", "type": "index-pattern", "attributes": {"title": "other", "fields": "[]"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := genFields(&out, &errOut, conf, unknown, filepath.Join(dir, "out2.ndjson"), ""); err == nil {
		t.Error("an unfeedable bundle must be refused")
	}
}
