package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NemoNobody/velruna/internal/client"
)

func TestFetchDocInputProbeJSON(t *testing.T) {
	var on fetchDocInput
	if err := json.Unmarshal([]byte(`{"url":"https://example.com","probe":true}`), &on); err != nil {
		t.Fatal(err)
	}
	if !on.Probe {
		t.Error("probe:true must set Probe")
	}

	var off fetchDocInput
	if err := json.Unmarshal([]byte(`{"url":"https://example.com"}`), &off); err != nil {
		t.Fatal(err)
	}
	if off.Probe {
		t.Error("an omitted probe must stay false")
	}
}

func TestFetchDocOutputProbeShape(t *testing.T) {
	out := fetchDocOutput{
		URL: "https://example.com/doc",
		Probe: probeFromResult(&client.ProbeResult{
			URL:          "https://example.com/doc",
			FinalURL:     "https://example.com/doc",
			Status:       403,
			ContentType:  "text/html",
			BytesTotal:   -1,
			BytesSampled: 128,
			Method:       "range",
			Note:         "server ignored Range; sampled first 128 bytes",
		}),
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["hash"]; ok {
		t.Errorf("a probe answer must not carry a hash: %s", raw)
	}
	p, ok := m["probe"].(map[string]any)
	if !ok {
		t.Fatalf("answer has no probe object: %s", raw)
	}
	for _, k := range []string{
		"url", "final_url", "status", "content_type", "bytes_total", "bytes_sampled", "method", "note",
	} {
		if _, ok := p[k]; !ok {
			t.Errorf("probe object lacks %q: %s", k, raw)
		}
	}
	// Unknown stays visible as -1 rather than disappearing behind omitempty.
	if got, _ := p["bytes_total"].(float64); got != -1 {
		t.Errorf("bytes_total = %v, want -1", p["bytes_total"])
	}
	if got, _ := p["status"].(float64); got != 403 {
		t.Errorf("status = %v, want 403 (a bot wall is a result, not an error)", p["status"])
	}
	if !strings.Contains(string(raw), `"method":"range"`) {
		t.Errorf("method missing from wire shape: %s", raw)
	}
}

func TestFetchDocOutputNoProbeFieldOnFetch(t *testing.T) {
	raw, err := json.Marshal(fetchDocOutput{Hash: "abc", URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"probe"`) {
		t.Errorf("a normal fetch answer must not claim a probe: %s", raw)
	}
}

// TestFetchDocProbeHandler drives the real handler: a probe answers with the
// target's status/content_type, returns no hash, and never contacts the store
// (the config points at a dead endpoint, so any store call would fail).
func TestFetchDocProbeHandler(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", "11")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("hello world"))
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	conf := filepath.Join(dir, "velruna.conf")
	body := `{
		"opensearch_url": "http://127.0.0.1:9",
		"fetch_allow_private": true,
		"fetch_respect_robots": false
	}`
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Isolate from the caller's environment: the file is the source of truth.
	t.Setenv("OPENSEARCH_URL", "")
	t.Setenv("OPENSEARCH_PASSWORD", "testpw") // skip the keyring
	t.Setenv("VELRUNA_FETCH_ALLOW_PRIVATE", "true")
	t.Setenv("VELRUNA_FETCH_RESPECT_ROBOTS", "false")

	s := &mcpServer{confPath: conf}
	_, out, err := s.fetchDoc(context.Background(), nil, fetchDocInput{URL: srv.URL, Probe: true})
	if err != nil {
		t.Fatalf("fetchDoc(probe): %v", err)
	}
	if out.Probe == nil {
		t.Fatal("probe answer has no probe object")
	}
	if out.Hash != "" {
		t.Errorf("probe must not carry a hash, got %q", out.Hash)
	}
	if out.Probe.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", out.Probe.Status)
	}
	if out.Probe.ContentType != "text/html" {
		t.Errorf("content_type = %q, want text/html", out.Probe.ContentType)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"hash"`) {
		t.Errorf("probe answer must not serialize a hash: %s", raw)
	}
	// A HEAD fast-path answer has no qualifier, but "note" is part of the
	// documented contract: an empty string is the honest value, not an omission.
	if !strings.Contains(string(raw), `"note":""`) {
		t.Errorf("probe answer must carry an empty note on the HEAD fast path: %s", raw)
	}
}
