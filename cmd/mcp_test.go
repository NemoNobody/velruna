package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NemoNobody/velruna/internal/keyring"
	"github.com/NemoNobody/velruna/internal/model"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverEnvVar makes the test binary act as the MCP server child process.
const serverEnvVar = "VELRUNA_TEST_MCP_SERVER"

// TestMain makes the cmd suite hermetic against the machine's keyring:
// after a real `velruna creds set` the dev entry (127.0.0.1:9200) exists in
// the user's wallet, and a Go test binary asking for it triggers a desktop
// access prompt that hangs the suite. The dispatcher default is a fake; the
// creds tests install their own fakes via fakeKeyringCmd. In-process code
// paths must never call the real keyring provider.
func TestMain(m *testing.M) {
	if os.Getenv(serverEnvVar) == "1" {
		// Child process: serve MCP over stdio until the client disconnects.
		if err := runMCPServer(""); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	get, set, del := keyring.Get, keyring.Set, keyring.Delete
	keyring.Get = func(string, string) (string, error) { return "", keyring.ErrNotFound }
	keyring.Set = func(string, string, string) error { return nil }
	keyring.Delete = func(string, string) error { return keyring.ErrNotFound }
	code := m.Run()
	keyring.Get, keyring.Set, keyring.Delete = get, set, del
	cleanupCLITrimBuild() // the once-built CLI subprocess binary (cli_trim_test)
	os.Exit(code)
}

// connectTestServer spawns this test binary as an MCP server and connects to it
// over stdio, exercising the real handshake. extraEnv entries override the
// inherited environment.
func connectTestServer(t *testing.T, extraEnv ...string) *mcp.ClientSession {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// The child is a separate process: the parent's dispatcher fake does not
	// reach it. Pin env so the child resolves the password from env (never
	// the keyring) and fails fast on store access (port 9 = discard).
	cmd := exec.Command(exe)
	cmd.Env = append(childEnv(append([]string{
		"OPENSEARCH_PASSWORD=testpw",
		"OPENSEARCH_URL=https://127.0.0.1:9",
	}, extraEnv...)), serverEnvVar+"=1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	client := mcp.NewClient(&mcp.Implementation{Name: "velruna-test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to MCP server: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// childEnv returns the parent environment with the overridden variables
// removed, so the last value always wins in the child.
func childEnv(overrides []string) []string {
	names := map[string]bool{}
	for _, kv := range overrides {
		if i := strings.IndexByte(kv, '='); i > 0 {
			names[kv[:i]] = true
		}
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && names[kv[:i]] {
			continue
		}
		env = append(env, kv)
	}
	return append(env, overrides...)
}

var wantTools = []string{
	"velruna_status", "velruna_search", "velruna_get",
	"velruna_add", "velruna_add_batch", "velruna_patch", "velruna_patch_batch", "velruna_list", "velruna_keys",
	"velruna_prune", "velruna_obsolete",
	"velruna_journal", "velruna_history_info",
	"velruna_fetch_doc", "velruna_get_doc", "velruna_search_docs",
}

func TestMCPServerExposesAllTools(t *testing.T) {
	session := connectTestServer(t)
	ctx := context.Background()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	got := map[string]string{}
	for _, tool := range tools.Tools {
		got[tool.Name] = tool.Description
	}
	for _, name := range wantTools {
		desc, ok := got[name]
		if !ok {
			t.Errorf("tool %q not advertised", name)
			continue
		}
		if desc == "" {
			t.Errorf("tool %q has an empty description", name)
		}
	}
	if len(got) != len(wantTools) {
		t.Errorf("got %d tools, want %d: %v", len(got), len(wantTools), got)
	}
}

// TestMCPServerStatusNeverFails checks the status contract:
// velruna_status must report availability instead of failing, so an agent can
// explain the situation to the user even when OpenSearch is missing.
func TestMCPServerStatusNeverFails(t *testing.T) {
	session := connectTestServer(t)
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "velruna_status"})
	if err != nil {
		t.Fatalf("CallTool(velruna_status): %v", err)
	}
	if res.IsError {
		t.Fatalf("velruna_status reported a tool error: %+v", res.Content)
	}

	// The structured result must carry the ok flag and a URL.
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out struct {
		OK   bool   `json:"ok"`
		URL  string `json:"url"`
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal structured content %s: %v", raw, err)
	}
	if out.URL == "" {
		t.Errorf("url is empty in status output: %s", raw)
	}
	if !out.OK && out.Hint == "" {
		t.Errorf("ok=false but no hint given: %s", raw)
	}
}

// TestMCPServerStoreToolsFailWhenUnreachable is the counterpart of the status
// contract: when the store is unreachable, the store tools must fail loudly.
// An agent that receives an empty list instead of an error would conclude
// "there is no such fact" — which is exactly the wrong lesson.
func TestMCPServerStoreToolsFailWhenUnreachable(t *testing.T) {
	session := connectTestServer(t, "OPENSEARCH_URL=https://127.0.0.1:9")
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"velruna_search", map[string]any{"query": "anything"}},
		{"velruna_get", map[string]any{"key": "any:key"}},
		{"velruna_list", map[string]any{"project": "any"}},
		{"velruna_prune", map[string]any{"key": "any:key", "confirm": true}},
		{"velruna_fetch_doc", map[string]any{"url": "https://example.com/x"}},
		{"velruna_get_doc", map[string]any{"hash": "abc123"}},
		{"velruna_search_docs", map[string]any{"query": "anything"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			if err != nil {
				t.Fatalf("CallTool(%s): %v", tc.name, err)
			}
			if !res.IsError {
				t.Errorf("%s reported success with an unreachable store; "+
					"an empty result would be mistaken for a real answer", tc.name)
			}
		})
	}
}

// TestMCPServerDocToolsValidateOffline pins the offline validation contract:
// a bad input is the caller's problem, not an availability problem. The store
// is unreachable on purpose: the tools must reject the input BEFORE touching
// it, so the agent gets an actionable message ("the host is not allowed"),
// not a store timeout.
func TestMCPServerDocToolsValidateOffline(t *testing.T) {
	session := connectTestServer(t,
		"OPENSEARCH_URL=https://127.0.0.1:9",
		"VELRUNA_FETCH_HOSTS=allowed.example.com",
	)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"velruna_fetch_doc", map[string]any{"url": ""}, "empty url"},
		{"velruna_fetch_doc", map[string]any{"url": "ftp://allowed.example.com/x"}, "scheme"},
		{"velruna_fetch_doc", map[string]any{"url": "https://denied.example.com/x"}, "fetch_allow_hosts"},
		// The project tag is stored text: it is guard-scanned at the call,
		// before the store (the cache-hit re-tag path never sees the content
		// guard, so the gate lives here).
		{"velruna_fetch_doc", map[string]any{"url": "https://allowed.example.com/x", "project": "p " + guardSentinel}, "secret-guard"},
		{"velruna_search_docs", map[string]any{"query": ""}, "query"},
		{"velruna_get_doc", map[string]any{"hash": ""}, "hash"},
		// A negative section would silently read block 0 (readDocBlocks
		// skips orders below start): refuse it before the store.
		{"velruna_get_doc", map[string]any{"hash": "abcd1234abcd1234", "section": -1}, "section must be 0 or greater"},
	} {
		t.Run(tc.name+" "+tc.want, func(t *testing.T) {
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			if err != nil {
				t.Fatalf("CallTool(%s): %v", tc.name, err)
			}
			if !res.IsError {
				t.Fatalf("%s accepted invalid input: %+v", tc.name, tc.args)
			}
			text := toolText(res)
			if !strings.Contains(text, tc.want) {
				t.Errorf("error must mention %q, got: %s", tc.want, text)
			}
			if strings.Contains(text, "127.0.0.1:9") {
				t.Errorf("validation ran after the store was reached: %s", text)
			}
		})
	}
}

// TestMCPServerAddValidatesShapeOffline pins that velruna_add rejects a blank
// key, a blank value, and an unknown category before touching the store: an
// empty key produces a cryptic store error, and an unknown category never
// ages (silent rot). The store is a discard port: any text mentioning it
// means validation ran too late.
func TestMCPServerAddValidatesShapeOffline(t *testing.T) {
	session := connectTestServer(t, "OPENSEARCH_URL=https://127.0.0.1:9")
	ctx := context.Background()

	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"key": "", "value": "v", "category": "config"}, "key is required"},
		{map[string]any{"key": "  ", "value": "v", "category": "config"}, "key is required"},
		{map[string]any{"key": "k", "value": "", "category": "config"}, "value is required"},
		{map[string]any{"key": "k", "value": "  ", "category": "config"}, "value is required"},
		{map[string]any{"key": "k", "value": "v", "category": "secrets"}, "category"},
		{map[string]any{"key": "k", "value": "v", "category": ""}, "category"},
		// The tag array is agent-supplied text in the same plaintext index:
		// the guard must scan it too (a credential-shaped tag used to slip
		// through because only key/value/notes/summary/source were scanned).
		{map[string]any{"key": "k", "value": "v", "category": "config", "keywords": []any{"token=AKIAIOSFODNN7EXAMPLE"}}, "keywords"},
	} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "velruna_add", Arguments: tc.args})
		if err != nil {
			t.Fatalf("CallTool(velruna_add): %v", err)
		}
		if !res.IsError {
			t.Fatalf("velruna_add accepted invalid input: %+v", tc.args)
		}
		text := toolText(res)
		if !strings.Contains(text, tc.want) {
			t.Errorf("error must mention %q, got: %s", tc.want, text)
		}
		if strings.Contains(text, "127.0.0.1:9") {
			t.Errorf("validation ran after the store was reached: %s", text)
		}
	}
}

// TestMCPServerListRequiresProjectOffline pins that velruna_list refuses a
// blank project before store contact. The CLI demands a project for the same
// reason: a project-less list silently returns nothing on a multi-project
// store, and an agent reads that silence as "no facts" and re-derives work
// that is already stored. velruna_keys is the cross-project tool.
func TestMCPServerListRequiresProjectOffline(t *testing.T) {
	session := connectTestServer(t, "OPENSEARCH_URL=https://127.0.0.1:9")
	ctx := context.Background()

	for _, project := range []string{"", "   "} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "velruna_list",
			Arguments: map[string]any{"project": project},
		})
		if err != nil {
			t.Fatalf("CallTool(velruna_list): %v", err)
		}
		if !res.IsError {
			t.Fatalf("velruna_list accepted blank project %q", project)
		}
		text := toolText(res)
		if !strings.Contains(text, "project is required") {
			t.Errorf("error must demand a project, got: %s", text)
		}
		if !strings.Contains(text, "velruna_keys") {
			t.Errorf("error must point at the cross-project tool, got: %s", text)
		}
		if strings.Contains(text, "127.0.0.1:9") {
			t.Errorf("validation ran after the store was reached: %s", text)
		}
	}
}

// TestMCPServerAddBatchSurfacesPartialSuccess pins the batch contract the
// single-tool simplification almost broke: when the fact doc landed and
// only the journal append failed, the batch item must report created=true
// beside its error — a retry that treats the item as "nothing stored" would
// issue a correction over a fact that exists.
func TestMCPServerAddBatchSurfacesPartialSuccess(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/velruna":
			w.WriteHeader(http.StatusOK) // the facts index exists
		case r.Method == http.MethodGet && (r.URL.Path == "/velruna/_doc/k" || r.URL.Path == "/velruna/_doc/k2"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"type":"document_missing_exception","reason":"no such document","index":"velruna"},"status":404}`)
		case r.Method == http.MethodGet && r.URL.Path == "/velruna/_doc/t":
			_, _ = io.WriteString(w, `{"_index":"velruna","_id":"t","_version":1,"_seq_no":3,"_primary_term":1,"found":true,"_source":{"key":"t","value":"v","category":"config","project":"p","status":"verified"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/velruna/_doc/t2":
			_, _ = io.WriteString(w, `{"_index":"velruna","_id":"t2","_version":1,"_seq_no":3,"_primary_term":1,"found":true,"_source":{"key":"t2","value":"v","category":"config","project":"p","status":"verified"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/velruna/_doc/t3":
			_, _ = io.WriteString(w, `{"_index":"velruna","_id":"t3","_version":1,"_seq_no":3,"_primary_term":1,"found":true,"_source":{"key":"t3","value":"v","category":"config","project":"p","status":"verified"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/velruna/_doc/t4":
			_, _ = io.WriteString(w, `{"_index":"velruna","_id":"t4","_version":1,"_seq_no":3,"_primary_term":1,"found":true,"_source":{"key":"t4","value":"v","category":"config","project":"p","status":"verified"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/velruna/_doc/c":
			_, _ = io.WriteString(w, `{"_index":"velruna","_id":"c","_version":1,"_seq_no":4,"_primary_term":1,"found":true,"_source":{"key":"c","value":"old","category":"config","project":"p","status":"verified"}}`)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "-history-") && strings.Contains(r.URL.Path, "/_doc/"):
			// journal appends go PUT /<hist>/_doc/<id> (opensearch-go
			// Index with a DocumentID). Fail only the k and c entries: the
			// fixture also needs a CLEAN birth row — without one, a create
			// wrongly counted as touched changes no assertion.
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"fact_key":"k"`) || strings.Contains(string(body), `"fact_key":"c"`) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":{"type":"cluster_block_exception","reason":"journal write blocked"},"status":500}`)
				return
			}
			_, _ = io.WriteString(w, `{"_index":"velruna-history","_id":"journal-entry-1","_version":1,"_seq_no":0,"_primary_term":1,"result":"created"}`)
		default:
			_, _ = io.WriteString(w, `{"_index":"velruna","_id":"k","_version":1,"_seq_no":0,"_primary_term":1,"result":"created"}`)
		}
	}))
	t.Cleanup(fake.Close)
	session := connectTestServer(t, "OPENSEARCH_URL="+fake.URL)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "velruna_add_batch",
		Arguments: map[string]any{"items": []any{
			map[string]any{"key": "k", "value": "v", "category": "config"},
			// same-value re-declare of an existing fact: a clean touch —
			// the counter case a one-item partial fixture can never show.
			map[string]any{"key": "t", "value": "v", "category": "config"},
			// clean value correction: created=false changed=true, the row
			// shape that separates the created and changed counters (a birth
			// always has both, so without it swapped counters pass).
			map[string]any{"key": "c", "value": "new", "category": "config"},
			// clean birth (its append succeeds): the row that pins the
			// touched guard — err==nil && !changed — because dropping
			// !changed would count this create as a touch too.
			map[string]any{"key": "k2", "value": "v2", "category": "config"},
			// three more touches: with the clean birth all counters are
			// pairwise distinct (2/3/4), so swapping ANY pair of increments
			// flips an assertion.
			map[string]any{"key": "t2", "value": "v", "category": "config"},
			map[string]any{"key": "t3", "value": "v", "category": "config"},
			map[string]any{"key": "t4", "value": "v", "category": "config"},
		}},
	})
	if err != nil {
		t.Fatalf("CallTool(velruna_add_batch): %v", err)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out struct {
		Created int `json:"created"`
		Changed int `json:"changed"`
		Touched int `json:"touched"`
		Items   []struct {
			Key     string `json:"key"`
			Created bool   `json:"created"`
			Changed bool   `json:"changed"`
			Note    string `json:"note"`
			Error   string `json:"error"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("structured content: %v (%s)", err, raw)
	}
	if len(out.Items) != 7 {
		t.Fatalf("items = %d, want 7: %s", len(out.Items), raw)
	}
	it := out.Items[0]
	if it.Error == "" {
		t.Fatalf("the journal failure must be reported per item: %s", raw)
	}
	if strings.Contains(it.Error, "velruna_add failed") {
		t.Errorf("the item error must carry only the batch label, not the single-add store label: %s", it.Error)
	}
	if !it.Created {
		t.Errorf("item.created = false, want true (the fact doc stored; only the journal lagged): %s", raw)
	}
	if out.Items[1].Error != "" || out.Items[1].Created || out.Items[1].Changed {
		t.Errorf("the same-value item must be a clean non-created touch, got %+v: %s", out.Items[1], raw)
	}
	if out.Items[2].Error == "" || out.Items[2].Created || !out.Items[2].Changed {
		// the journal hook blocks every append, so the correction is a
		// partial success too: the doc correction landed, its entry lagged
		t.Errorf("the correction item must be created=false changed=true beside its lag error, got %+v: %s", out.Items[2], raw)
	}
	if out.Items[3].Error != "" || !out.Items[3].Created || !out.Items[3].Changed {
		t.Errorf("the clean birth must be created=true changed=true with no error, got %+v: %s", out.Items[3], raw)
	}
	for _, touch := range out.Items[4:7] {
		if touch.Error != "" || touch.Created || touch.Changed {
			t.Errorf("the same-value items must be clean non-created touches, got %+v: %s", touch, raw)
		}
		// The per-item note is the batch twin of the singular velruna_add
		// note: a batch caller must see same-value honesty too (round-21).
		if !strings.Contains(touch.Note, "same-value touch") {
			t.Errorf("the same-value item must carry the same-value note, got %+v: %s", touch, raw)
		}
	}
	if !strings.Contains(out.Items[1].Note, "same-value touch") {
		t.Errorf("the clean touch row must carry the note, got %+v: %s", out.Items[1], raw)
	}
	if out.Items[3].Note != "" {
		t.Errorf("a clean birth must not carry a same-value note, got %q", out.Items[3].Note)
	}
	// The aggregate must agree with the rows (the patch twin pins the same):
	// a summary that contradicts what the items say is the audit lie the
	// counters exist to prevent. Partial-birth/touch/correction/clean-birth/
	// touch rows give pairwise distinct populations (created=2, changed=3,
	// touched=4), and the clean birth pins the touched guard (dropping the
	// !changed condition would count it as a fifth touch).
	if out.Created != 2 || out.Changed != 3 || out.Touched != 4 {
		t.Errorf("counters = created:%d changed:%d touched:%d, want 2/3/4: %s", out.Created, out.Changed, out.Touched, raw)
	}
}

// TestMCPServerPatchBatchSurfacesPartialSuccess is the patch twin of
// TestMCPServerAddBatchSurfacesPartialSuccess: an accepted value change
// whose journal entry lagged must surface changed=true beside the item
// error — Patch returns the partial PatchResult with that error.
func TestMCPServerPatchBatchSurfacesPartialSuccess(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/velruna":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/velruna/_doc/"):
			_, _ = io.WriteString(w, `{"_index":"velruna","_id":"k","_version":1,"_seq_no":3,"_primary_term":1,"found":true,"_source":{"key":"k","value":"old","category":"config","project":"p","status":"verified"}}`)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "-history-") && strings.Contains(r.URL.Path, "/_doc/"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"type":"cluster_block_exception","reason":"journal write blocked"},"status":500}`)
		default:
			_, _ = io.WriteString(w, `{"_index":"velruna","_id":"k","result":"updated"}`)
		}
	}))
	t.Cleanup(fake.Close)
	session := connectTestServer(t, "OPENSEARCH_URL="+fake.URL)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "velruna_patch_batch",
		Arguments: map[string]any{"items": []any{
			map[string]any{"key": "k", "value": "new value"},
		}},
	})
	if err != nil {
		t.Fatalf("CallTool(velruna_patch_batch): %v", err)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out struct {
		Changed int `json:"changed"`
		Items   []struct {
			Key     string `json:"key"`
			Changed bool   `json:"changed"`
			Error   string `json:"error"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("structured content: %v (%s)", err, raw)
	}
	if len(out.Items) != 1 {
		t.Fatalf("items = %d, want 1: %s", len(out.Items), raw)
	}
	it := out.Items[0]
	if it.Error == "" {
		t.Fatalf("the journal failure must be reported per item: %s", raw)
	}
	if strings.Contains(it.Error, "velruna_patch failed") {
		t.Errorf("item error must not carry the single-patch failure label, only the batch one: %s", raw)
	}
	if !it.Changed {
		t.Errorf("item.changed = false, want true (the value change landed; only the journal lagged): %s", raw)
	}
	if out.Changed != 1 {
		t.Errorf("batch changed counter = %d, want 1 (the write landed): %s", out.Changed, raw)
	}
}

// TestMCPServerObsoleteFailsOnBrokenConfig pins that a config load failure
// cannot silently disable the secret guard: the guard policy lives in the
// config, so an unloadable policy must fail the write loudly — never proceed
// with an unscanned reason into the plaintext store.
func TestMCPServerObsoleteFailsOnBrokenConfig(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "velruna.conf")
	if err := os.WriteFile(conf, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := connectTestServer(t, "VELRUNA_CONF="+conf)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "velruna_obsolete",
		Arguments: map[string]any{"key": "k", "reason": "the file was deleted"},
	})
	if err != nil {
		t.Fatalf("CallTool(velruna_obsolete): %v", err)
	}
	if !res.IsError {
		t.Fatal("obsolete wrote with an unloadable guard policy")
	}
	text := toolText(res)
	if !strings.Contains(text, "velruna_obsolete") {
		t.Errorf("error must name the tool: %s", text)
	}
	if strings.Contains(text, "127.0.0.1:9") {
		t.Errorf("the write reached store contact despite the broken config: %s", text)
	}
}

// TestMCPGetDocMissingIsFoundFalse pins the tool's contract: a document that
// does not exist is an empty result (found:false), not a store-failure report.
// An agent that is taught "the store is down" by a missing doc starts
// reporting phantom outages.
func TestMCPGetDocMissingIsFoundFalse(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK) // the facts index "exists"
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"type":"document_missing_exception","reason":"Document with id abcd not found.","index":"velruna-docs"},"status":404}`)
	}))
	t.Cleanup(fake.Close)

	session := connectTestServer(t, "OPENSEARCH_URL="+fake.URL)
	ctx := context.Background()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "velruna_get_doc",
		Arguments: map[string]any{"hash": "abcd1234abcd1234"},
	})
	if err != nil {
		t.Fatalf("CallTool(velruna_get_doc): %v", err)
	}
	if res.IsError {
		t.Fatalf("missing doc must be found:false, got tool error: %s", toolText(res))
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), `"found":false`) {
		t.Fatalf("want found:false, got: %s", raw)
	}
}

// TestMCPGetDocOutlineCapped pins the read-side outline budget: velruna_get_doc
// caps the outline at outlineRows lines exactly like velruna_fetch_doc, so a
// long page cannot flood the answer. sections_hidden counts the blocks left
// out and hint names their range; a capped read adds its own hint without
// losing the outline one.
func TestMCPGetDocOutlineCapped(t *testing.T) {
	const total = outlineRows + 5
	sections := make([]model.Section, 0, total)
	for i := 0; i < total; i++ {
		sections = append(sections, model.Section{
			Order:   i,
			Heading: fmt.Sprintf("section %d", i),
			// 1000 runes per block: full=true must exceed docReadBudget.
			Text: strings.Repeat("x", 1000),
		})
	}
	source, err := json.Marshal(model.Doc{
		URL:      "https://allowed.example.com/page",
		URLHash:  "abcd1234abcd1234",
		Title:    "page",
		Sections: sections,
	})
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}
	body := fmt.Sprintf(`{"_index":"velruna-docs","_id":"abcd1234abcd1234","found":true,"_source":%s}`, source)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK) // the docs index "exists"
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(fake.Close)

	session := connectTestServer(t, "OPENSEARCH_URL="+fake.URL)
	ctx := context.Background()
	call := func(args map[string]any) map[string]any {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "velruna_get_doc", Arguments: args})
		if err != nil {
			t.Fatalf("CallTool(velruna_get_doc): %v", err)
		}
		if res.IsError {
			t.Fatalf("velruna_get_doc failed: %s", toolText(res))
		}
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshal structured content: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode structured content: %v (%s)", err, raw)
		}
		return out
	}

	out := call(map[string]any{"hash": "abcd1234abcd1234"})
	lines, _ := out["sections"].([]any)
	if len(lines) != outlineRows {
		t.Errorf("outline lines = %d, want %d", len(lines), outlineRows)
	}
	if got := out["sections_hidden"]; got != float64(total-outlineRows) {
		t.Errorf("sections_hidden = %v, want %d", got, total-outlineRows)
	}
	hint, _ := out["hint"].(string)
	if !strings.Contains(hint, fmt.Sprintf("more %d blocks", total-outlineRows)) {
		t.Errorf("hint must summarize the hidden blocks, got: %q", hint)
	}

	full := call(map[string]any{"hash": "abcd1234abcd1234", "full": true})
	hint, _ = full["hint"].(string)
	if !strings.Contains(hint, fmt.Sprintf("more %d blocks", total-outlineRows)) ||
		!strings.Contains(hint, "full text capped") {
		t.Errorf("a capped full read must keep both hints, got: %q", hint)
	}
}

// TestMCPPruneKeepRefusalOffline pins the prune edge: with history_limit=0
// and no explicit keep, the refusal is a config-state answer that must not
// touch the store (the round-10 shape-before-store contract).
func TestMCPPruneKeepRefusalOffline(t *testing.T) {
	session := connectTestServer(t, "OPENSEARCH_URL=https://127.0.0.1:9", "VELRUNA_HISTORY_LIMIT=0")
	ctx := context.Background()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "velruna_prune",
		Arguments: map[string]any{"key": "k"},
	})
	if err != nil {
		t.Fatalf("CallTool(velruna_prune): %v", err)
	}
	if !res.IsError {
		t.Fatal("history_limit=0 without keep must be refused")
	}
	text := toolText(res)
	if !strings.Contains(text, "history_limit is 0") {
		t.Errorf("error must name the config state, got: %s", text)
	}
	if strings.Contains(text, "127.0.0.1:9") {
		t.Errorf("the refusal ran after the store was reached: %s", text)
	}
}

func TestResolveStatus(t *testing.T) {
	cases := map[string]string{
		"":         "verified", // default: only trusted facts
		"any":      "",         // explicit: no filter
		"obsolete": "obsolete",
		"VERIFIED": "verified", // stored statuses are lowercase: canonicalize
		"Obsolete": "obsolete",
		"Any":      "",
		"  ":       "verified", // whitespace-only means "no filter given"
	}
	for in, want := range cases {
		got, err := resolveStatus(in)
		if err != nil {
			t.Errorf("resolveStatus(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("resolveStatus(%q) = %q, want %q", in, got, want)
		}
	}
	// An unknown spelling must be a loud error, not a silent zero-match filter.
	if _, err := resolveStatus("bogus"); err == nil {
		t.Error("resolveStatus(bogus): want error, got nil")
	}
}

// toolText flattens the text content of a tool result for assertions.
func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// TestMCPServerAddSecretGuard pins the write-time guard rail end to end. The
// store is unreachable on purpose: a rejected write must be rejected by the
// guard before any store contact, and the guard's message must carry masked
// previews only (the whole point is not to echo the secret into the agent's
// context).
func TestMCPServerAddSecretGuard(t *testing.T) {
	const sentinel = "AKIAIOSFODNN7EXAMPLE" // the AWS documentation example key
	storeDown := "OPENSEARCH_URL=https://127.0.0.1:9"
	secretArgs := map[string]any{
		"key":      "guard:test",
		"value":    "the admin key is " + sentinel,
		"category": "config",
	}

	t.Run("reject blocks before the store", func(t *testing.T) {
		session := connectTestServer(t, "VELRUNA_SECRET_GUARD=reject", storeDown)
		res, err := session.CallTool(context.Background(),
			&mcp.CallToolParams{Name: "velruna_add", Arguments: secretArgs})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !res.IsError {
			t.Fatal("reject mode stored credential-like text")
		}
		text := toolText(res)
		if !strings.Contains(text, "secret-guard") {
			t.Errorf("want a secret-guard error, got: %s", text)
		}
		if strings.Contains(text, sentinel) {
			t.Errorf("guard message echoes the secret: %s", text)
		}
	})

	t.Run("confirm does not bypass reject", func(t *testing.T) {
		session := connectTestServer(t, "VELRUNA_SECRET_GUARD=reject", storeDown)
		args := map[string]any{"confirm_secret": true}
		for k, v := range secretArgs {
			args[k] = v
		}
		res, err := session.CallTool(context.Background(),
			&mcp.CallToolParams{Name: "velruna_add", Arguments: args})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !res.IsError || !strings.Contains(toolText(res), "secret-guard") {
			t.Errorf("reject must ignore confirm_secret; got: %s", toolText(res))
		}
	})

	t.Run("ask needs the confirmation flag", func(t *testing.T) {
		session := connectTestServer(t, "VELRUNA_SECRET_GUARD=ask", storeDown)
		res, err := session.CallTool(context.Background(),
			&mcp.CallToolParams{Name: "velruna_add", Arguments: secretArgs})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !res.IsError {
			t.Fatal("ask mode stored credential-like text without confirmation")
		}
		text := toolText(res)
		if !strings.Contains(text, "confirm_secret") {
			t.Errorf("ask message must name the confirmation parameter: %s", text)
		}
		if strings.Contains(text, sentinel) {
			t.Errorf("guard message echoes the secret: %s", text)
		}
	})

	t.Run("ask with confirmation reaches the store", func(t *testing.T) {
		session := connectTestServer(t, "VELRUNA_SECRET_GUARD=ask", storeDown)
		args := map[string]any{"confirm_secret": true}
		for k, v := range secretArgs {
			args[k] = v
		}
		res, err := session.CallTool(context.Background(),
			&mcp.CallToolParams{Name: "velruna_add", Arguments: args})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !res.IsError {
			t.Fatal("the store is unreachable; a success means the test wired the env wrong")
		}
		text := toolText(res)
		if strings.Contains(text, "secret-guard") {
			t.Errorf("confirmed ask must pass the guard; got: %s", text)
		}
		if !strings.Contains(text, "velruna_add failed") {
			t.Errorf("want the store error after the guard passed; got: %s", text)
		}
	})
}

// TestNewFactViewCarriesTimeAndHistory pins the MCP contract: the view exposes
// the observation time and the staleness signal; history ships only when
// include_history is set (default off: state keys get corrected daily and every
// past observation bloats the agent's context). A zero timestamp (facts created
// before observed_at existed) must map to "".
func TestNewFactViewCarriesTimeAndHistory(t *testing.T) {
	observed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := model.Fact{
		Key: "k", Value: "v", Category: "config", Project: "p",
		Status:     "obsolete",
		Source:     "experiment",
		Notes:      "note",
		ObservedAt: observed,
		History: []model.HistoryEntry{
			{Value: "old", ObservedAt: observed.Add(-time.Hour), Source: "tool:read_file"},
			{Value: "older", Source: "doc:README"}, // zero time → ""
		},
	}

	v := newFactView(f, true, false)
	if v.ObservedAt != "2026-01-02T03:04:05Z" {
		t.Errorf("ObservedAt = %q, want RFC3339", v.ObservedAt)
	}
	if v.Status != "obsolete" || v.Notes != "note" {
		t.Errorf("status/notes not carried: %+v", v)
	}
	if len(v.History) != 2 {
		t.Fatalf("history len = %d, want 2", len(v.History))
	}
	if v.History[0].Value != "old" || v.History[0].ObservedAt != "2026-01-02T02:04:05Z" ||
		v.History[0].Source != "tool:read_file" {
		t.Errorf("history[0] = %+v", v.History[0])
	}
	if v.History[1].ObservedAt != "" {
		t.Errorf("zero observed_at must map to empty string, got %q", v.History[1].ObservedAt)
	}

	off := newFactView(f, false, false)
	if len(off.History) != 0 {
		t.Errorf("history must be stripped by default, got %v", off.History)
	}
	// History is a full-row-only option: compact=true never ships it, even
	// when include_history asks (compact rows are navigation; the journal
	// tools read history).
	compact := newFactView(f, true, true)
	if len(compact.History) != 0 {
		t.Errorf("compact must strip history, got %v", compact.History)
	}
	if off.Value != "v" {
		t.Errorf("stripping history must not strip the current value, got %q", off.Value)
	}

	// A config observation from 2026-01-02 is far past the 14-day TTL by any
	// plausible test date, so the staleness signal must be on.
	if v.AgeDays <= model.TTLDays("config") {
		t.Errorf("AgeDays = %d, want past the config TTL", v.AgeDays)
	}
	if !v.Stale {
		t.Error("Stale = false for a long-past-TTL fact; the agent would trust it blindly")
	}

	// Fresh facts must not raise the signal.
	now := time.Now().UTC()
	fresh := model.Fact{Key: "k", Value: "v", Category: "location", ObservedAt: now}
	fv := newFactView(fresh, false, false)
	if fv.AgeDays != 0 || fv.Stale {
		t.Errorf("fresh fact: age_days = %d, stale = %v, want 0/false", fv.AgeDays, fv.Stale)
	}

	// Pre-observed_at facts: age is unknown (-1) and TTL'd categories are stale.
	empty := newFactView(model.Fact{Key: "k", Value: "v"}, false, false)
	if empty.ObservedAt != "" {
		t.Errorf("zero ObservedAt must map to empty string, got %q", empty.ObservedAt)
	}
	if len(empty.History) != 0 {
		t.Errorf("empty history must stay empty, got %v", empty.History)
	}
	if empty.AgeDays != -1 {
		t.Errorf("zero observed_at: age_days = %d, want -1", empty.AgeDays)
	}
	if empty.Stale {
		t.Error("category with no TTL must not be stale when observed_at is unknown")
	}
	legacyConfig := newFactView(model.Fact{Key: "k", Value: "v", Category: "config"}, false, false)
	if !legacyConfig.Stale {
		t.Error("unknown observation time must be stale for TTL'd categories")
	}
}

// TestMCPKeysEmptyQuery pins the velruna_keys guard: an empty key query is a
// context flood, rejected before the store is touched. The store is
// unreachable on purpose: the rejection must not depend on it.
func TestMCPKeysEmptyQuery(t *testing.T) {
	session := connectTestServer(t)
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "velruna_keys",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError || !strings.Contains(toolText(res), "prefix") {
		t.Errorf("empty keys query must be rejected with a prefix hint; got: %s", toolText(res))
	}
}

// TestQueryTerms pins the tokenizer approximation: lowercased alphanumeric
// runs of length >= 3, deduplicated. The section pick depends on it.
func TestQueryTerms(t *testing.T) {
	got := queryTerms("OpenSearch, opensearch URL https://x")
	want := []string{"opensearch", "url", "https", "x"}
	if strings.Join(got, ",") != strings.Join(want[:3], ",") {
		// "x" is length 1 and must be dropped; order must follow first use.
		t.Errorf("terms = %v, want %v", got, want[:3])
	}
	if len(got) != 3 {
		t.Errorf("len(terms) = %d, want 3 (short runs dropped, dupe collapsed): %v", len(got), got)
	}
	if queryTerms("") != nil {
		t.Error("empty query must yield no terms")
	}
	if terms := queryTerms("ключ ключ项目2"); len(terms) != 2 {
		t.Errorf("unicode terms = %v, want 2 (cyrillic and CJK survive)", terms)
	}
}

// TestPickMatchedSection pins the all-terms rule: a section containing every
// term wins even when it is not first; frequency alone never wins outright;
// no match is -1 (doc-level snippet fallback).
func TestPickMatchedSection(t *testing.T) {
	sections := []model.Section{
		{Order: 1, Text: "alpha beta gamma epsilon"},
		{Order: 2, Text: "zeta eta theta"},
		{Order: 3, Text: "gamma ... delta ... alpha"},
	}
	terms := queryTerms("alpha gamma delta")
	if i := pickMatchedSection(sections, terms); i != 2 {
		t.Errorf("pick = %d, want 2 (section 3 has ALL terms; section 1 lacks delta)", i)
	}
	// No section has all terms: the most distinct terms wins; ties go to the
	// first section (pickMatchedSection is deterministic).
	if i := pickMatchedSection(sections, queryTerms("alpha zeta")); i != 0 {
		t.Errorf("fallback pick = %d, want 0 (one term each, first wins)", i)
	}
	if i := pickMatchedSection(sections, queryTerms("nonexistent")); i != -1 {
		t.Errorf("no-match pick = %d, want -1", i)
	}
	if i := pickMatchedSection(sections, nil); i != -1 {
		t.Errorf("empty terms pick = %d, want -1", i)
	}
}

// TestCenterSnippet checks the match-centered window: the term is inside the
// snippet, the window is bounded, and edges carry ellipsis markers.
func TestCenterSnippet(t *testing.T) {
	text := strings.Repeat("filler ", 200) + " KEYWORD tail"
	s := centerSnippet(text, []string{"keyword"})
	if !strings.Contains(strings.ToLower(s), "keyword") {
		t.Errorf("snippet must contain the matched term: %q", s)
	}
	if !strings.HasPrefix(s, "…") {
		t.Errorf("mid-text window must open with an ellipsis: %q", s)
	}
	if r := []rune(s); len(r) > 2*160+10 {
		t.Errorf("snippet too wide: %d runes", len(r))
	}
	// Term at the very start: no leading ellipsis.
	head := centerSnippet("KEYWORD "+strings.Repeat("x ", 300), []string{"keyword"})
	if !strings.HasPrefix(head, "KEYWORD") {
		t.Errorf("head term must start the snippet without a leading ellipsis: %q", head[:20])
	}
	// No hit at all: plain capped snippet, no crash.
	if s := centerSnippet("nothing here", []string{"absent"}); s == "" {
		t.Error("no-term fallback must still produce a snippet")
	}
}

// TestNewFactViewCompact pins the compact shape: the bulk fields (value with
// a summary, authorship labels, created_at) are dropped, notes keep a head,
// and a summary-less fact keeps a 100-rune value head so the row is not empty.
func TestNewFactViewCompact(t *testing.T) {
	now := time.Now().UTC()
	long := strings.Repeat("д", 150)
	f := model.Fact{
		Key: "k", Value: long, Category: "config", Project: "p", Status: "verified",
		Source: "tool:read_file", Author: "a1", AuthorSource: "security", UpdatedBy: "a2",
		Notes: strings.Repeat("n", 150), Summary: "sum",
		ObservedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	c := newFactView(f, false, true)
	if c.Value != "" {
		t.Errorf("compact row with a summary must drop the value, got %d runes", len([]rune(c.Value)))
	}
	if c.Author != "" || c.AuthorSource != "" || c.UpdatedBy != "" || c.CreatedAt != "" {
		t.Errorf("compact must drop authorship/created_at: %+v", c)
	}
	if len([]rune(c.Notes)) != 101 { // 100 runes + ellipsis
		t.Errorf("notes head = %d runes, want 101", len([]rune(c.Notes)))
	}
	if c.Summary != "sum" || c.Key != "k" || c.Status != "verified" {
		t.Errorf("compact keeps the navigation fields: %+v", c)
	}

	noSum := f
	noSum.Summary = ""
	c2 := newFactView(noSum, false, true)
	if len([]rune(c2.Value)) != 101 {
		t.Errorf("summary-less compact row keeps a 100-rune head, got %d", len([]rune(c2.Value)))
	}
	// Short values survive untouched.
	short := model.Fact{Key: "k", Value: "tiny", Category: "location", Project: "p", Status: "verified"}
	if c3 := newFactView(short, false, true); c3.Value != "tiny" {
		t.Errorf("short value in compact mode = %q, want tiny", c3.Value)
	}
}

// TestMCPServerM1ValidationOffline pins the new tools' input contracts:
// batch, patch, and get reject bad shapes BEFORE touching the store.
func TestMCPServerM1ValidationOffline(t *testing.T) {
	session := connectTestServer(t, "OPENSEARCH_URL=https://127.0.0.1:9")
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"velruna_add_batch", map[string]any{"items": []any{}}, "1..10"},
		{"velruna_add_batch", map[string]any{"items": []any{
			map[string]any{"key": "k", "value": "v", "category": "config"},
			map[string]any{"key": "k", "value": "w", "category": "config"},
		}}, "duplicate key"},
		// The dupe check runs on the TRIMMED key (what the store gets):
		// " k " and "k" are one fact, and a batch of two is the silent
		// correction the contract refuses.
		{"velruna_add_batch", map[string]any{"items": []any{
			map[string]any{"key": " k ", "value": "v", "category": "config"},
			map[string]any{"key": "k", "value": "w", "category": "config"},
		}}, "duplicate key"},
		{"velruna_patch_batch", map[string]any{"items": []any{}}, "1..10"},
		{"velruna_patch_batch", map[string]any{"items": []any{
			map[string]any{"key": "k", "notes_append": "a"},
			map[string]any{"key": " k ", "notes_append": "b"},
		}}, "duplicate key"},
		// The project label is agent-supplied identity text in the shared
		// index: the fact path gates it exactly like the fetch path.
		{"velruna_add", map[string]any{"key": "k", "value": "v", "category": "config", "project": "rel " + guardSentinel}, "secret-guard"},
		{"velruna_patch", map[string]any{"key": ""}, "key is required"},
		// The patch shape (nothing-to-change, bogus status, orphan reason) is
		// validated before config/guard/store: a shape error must never arrive
		// wrapped in the availability advice (round-8 parity with list/keys).
		{"velruna_patch", map[string]any{"key": "k", "status": "bogus"}, "status must be verified|obsolete"},
		{"velruna_patch", map[string]any{"key": "k", "reason": "gone"}, "reason is only accepted"},
		// The list prefix normalizes before the store too (the keys contract).
		{"velruna_list", map[string]any{"project": "p", "prefix": "a*b"}, "trailing *"},
		{"velruna_get", map[string]any{"key": "k", "keys": []any{"k"}}, "exactly one"},
		{"velruna_get", map[string]any{}, "exactly one"},
		// The fact-search edge refuses an empty query up-front (the
		// search_docs contract): a silent zero is not an answer.
		{"velruna_search", map[string]any{"query": "  "}, "query is required"},
	} {
		t.Run(tc.name+" "+tc.want, func(t *testing.T) {
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			if err != nil {
				t.Fatalf("CallTool(%s): %v", tc.name, err)
			}
			if !res.IsError {
				t.Fatalf("%s accepted invalid input: %+v", tc.name, tc.args)
			}
			text := toolText(res)
			if !strings.Contains(text, tc.want) {
				t.Errorf("error must mention %q, got: %s", tc.want, text)
			}
			if strings.Contains(text, "127.0.0.1:9") {
				t.Errorf("validation ran after the store was reached: %s", text)
			}
		})
	}
}

// TestMCPServerSearchDocsSectionShapeOffline pins the get-keys contract
// end-to-end on the validation layer: keys over the cap is rejected with an
// actionable message before the store is reached.
func TestMCPServerGetKeysCapOffline(t *testing.T) {
	session := connectTestServer(t, "OPENSEARCH_URL=https://127.0.0.1:9")
	keys := make([]any, 11)
	for i := range keys {
		keys[i] = "k"
	}
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "velruna_get", Arguments: map[string]any{"keys": keys}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError || !strings.Contains(toolText(res), "capped at 10") {
		t.Errorf("keys cap not enforced: %s", toolText(res))
	}
}

// TestNewFactViewCompactDropsHistory pins the context-economy rule: compact
// rows are a table of contents; the history chain is the most expensive
// field in the payload, so compact implies no history even when the caller
// asked for it.
func TestNewFactViewCompactDropsHistory(t *testing.T) {
	now := time.Now().UTC()
	f := model.Fact{
		Key: "k", Value: "v", Category: "config", Project: "p", Status: "verified",
		CreatedAt: now, UpdatedAt: now, ObservedAt: now,
	}
	v := newFactView(f, true, true)
	if v.History != nil {
		t.Errorf("compact row must carry no history, got %+v", v.History)
	}
	full := newFactView(f, true, false)
	if full.History == nil {
		t.Error("full row with include_history must carry the history block")
	}
}

// TestMCPServerObsoleteGuardCoversKey pins the guard parity between the CLI
// edge (key+reason) and the MCP edge: a credential-shaped KEY must not reach
// the shared index via MCP only.
func TestMCPServerObsoleteGuardCoversKey(t *testing.T) {
	const sentinel = "AKIAIOSFODNN7EXAMPLE"
	storeDown := "OPENSEARCH_URL=https://127.0.0.1:9"
	session := connectTestServer(t, "VELRUNA_SECRET_GUARD=reject", storeDown)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "velruna_obsolete",
		Arguments: map[string]any{
			"key":    "leak-" + sentinel,
			"reason": "file deleted",
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("reject mode accepted a credential-shaped key")
	}
	if text := toolText(res); !strings.Contains(text, "secret-guard") {
		t.Errorf("want a secret-guard error, got: %s", text)
	}
}

// TestMCPGetKeysEmptyPreValidated pins that every key is validated before
// any store contact: the empty entry fails the whole call without touching
// the store, and the shape refusals (exactly-one, cap) keep priority over
// the per-key complaint.
func TestMCPGetKeysEmptyPreValidated(t *testing.T) {
	session := connectTestServer(t, "OPENSEARCH_URL=https://127.0.0.1:9")
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "velruna_get",
		Arguments: map[string]any{"keys": []any{" k ", " "}},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	text := toolText(res)
	if !strings.Contains(text, "empty key in keys at index 1") {
		t.Errorf("want the indexed empty-key refusal, got: %s", text)
	}
	if strings.Contains(text, "store") {
		t.Errorf("the call touched the store before validating: %s", text)
	}
}

// TestMCPAddBatchEnsuresIndexOnce: the batch reuses one client, so the index
// check (and the mapping/infra passes behind it) happens once for the whole
// batch. Before the batch shared a client it was one check per item.
func TestMCPAddBatchEnsuresIndexOnce(t *testing.T) {
	var heads atomic.Int64
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/velruna":
			heads.Add(1)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/velruna/_doc/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"type":"document_missing_exception","reason":"no such document"},"status":404}`)
		default:
			_, _ = io.WriteString(w, `{"_index":"velruna","_id":"k","_version":1,"_seq_no":0,"_primary_term":1,"result":"created"}`)
		}
	}))
	t.Cleanup(fake.Close)

	session := connectTestServer(t, "OPENSEARCH_URL="+fake.URL)
	items := make([]any, 0, 6)
	for i := 0; i < 6; i++ {
		items = append(items, map[string]any{"key": "k" + strconv.Itoa(i), "value": "v", "category": "config"})
	}
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "velruna_add_batch",
		Arguments: map[string]any{"items": items},
	})
	if err != nil {
		t.Fatalf("CallTool(velruna_add_batch): %v", err)
	}
	if res.IsError {
		t.Fatalf("tool reported an error: %v", res.Content)
	}
	if got := heads.Load(); got != 1 {
		t.Fatalf("index checks = %d, want 1 (one client per batch)", got)
	}
}
