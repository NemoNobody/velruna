package cmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/model"
)

func testDoc() *model.Doc {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	return &model.Doc{
		URL:       "https://example.com/guide",
		URLHash:   "abc123def4567890",
		Title:     "Example Guide",
		Project:   "velruna",
		FetchedAt: now.Add(-24 * time.Hour),
		ExpiresAt: now.Add(24 * time.Hour),
		Sections: []model.Section{
			{Order: 0, Heading: "Intro", Text: "first section text"},
			{Order: 1, Heading: "Usage", Text: "second section text"},
		},
	}
}

func TestPrintDocOutline(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if err := printDocOutline(&buf, testDoc(), true, false, now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"url: https://example.com/guide",
		"hash: abc123def4567890",
		"title: Example Guide",
		"project: velruna",
		"cached: true",
		"[0] Intro",
		"[1] Usage",
		"velruna doc abc123def4567890 <n>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("outline lacks %q:\n%s", want, out)
		}
	}
	// The outline never carries section bodies.
	if strings.Contains(out, "first section text") {
		t.Errorf("outline leaked section text:\n%s", out)
	}
}

func TestPrintDocOutlineStale(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	d := testDoc()
	d.ExpiresAt = now.Add(-time.Hour) // past TTL
	if err := printDocOutline(&buf, d, false, false, now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "stale: true") {
		t.Errorf("expired doc must be marked stale:\n%s", buf.String())
	}
}

func TestPrintDocFull(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if err := printDocFull(&buf, testDoc(), now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"first section text",
		"second section text",
		"--- [0] Intro ---",
		"--- [1] Usage ---",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--full lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "next:") {
		t.Errorf("a small document needs no continuation:\n%s", out)
	}
	if strings.Contains(out, "stale: fetched") {
		t.Errorf("a fresh document must not carry the stale note:\n%s", out)
	}
}

func TestPrintDocFullBudget(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	d := testDoc()
	d.ExpiresAt = now.Add(-time.Hour) // stale: the note must precede the text
	d.Sections = nil
	for i := 0; i < 3; i++ {
		d.Sections = append(d.Sections, model.Section{
			Order:   i,
			Heading: "Block " + strconv.Itoa(i),
			Text:    strings.Repeat("x", docReadBudget),
		})
	}
	var buf bytes.Buffer
	if err := printDocFull(&buf, d, now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "--- [0] Block 0 ---") {
		t.Errorf("the first block must always be included:\n%s", out)
	}
	if strings.Contains(out, "--- [1] Block 1 ---") {
		t.Errorf("a block past the budget must be deferred:\n%s", out)
	}
	if !strings.Contains(out, "next: continue with --section 1") {
		t.Errorf("a capped read must name the next block:\n%s", out)
	}
	stale := strings.Index(out, "stale: fetched ")
	body := strings.Index(out, "--- [0] Block 0 ---")
	if stale < 0 || !strings.Contains(out, "re-fetch before quoting") {
		t.Errorf("a stale read lacks the re-fetch warning:\n%s", out)
	} else if stale > body {
		t.Errorf("the stale note must precede the text:\n%s", out)
	}
}

func TestResolveSection(t *testing.T) {
	tests := []struct {
		name    string
		flag    int
		args    []string
		want    int
		wantErr string
	}{
		{"flag not given", -1, nil, -1, ""},
		{"flag given", 3, nil, 3, ""},
		{"positional", -1, []string{"2"}, 2, ""},
		{"positional outline", -1, []string{"-1"}, -1, ""},
		{"positional negative", -1, []string{"-2"}, 0, "--section must be 0 or greater, got -2"},
		{"flag negative", -2, nil, 0, "--section must be 0 or greater, got -2"},
		{"positional not a number", -1, []string{"x"}, 0, "invalid syntax"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveSection(tt.flag, tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveSection(%d, %v) error = %v, want %q", tt.flag, tt.args, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSection(%d, %v): %v", tt.flag, tt.args, err)
			}
			if got != tt.want {
				t.Errorf("resolveSection(%d, %v) = %d, want %d", tt.flag, tt.args, got, tt.want)
			}
		})
	}
}

func TestPrintDocOutlineCapsRows(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	d := testDoc()
	d.Sections = nil
	for i := 0; i < outlineRows+5; i++ {
		d.Sections = append(d.Sections, model.Section{
			Order:   i,
			Heading: "Section " + strconv.Itoa(i),
			Text:    "body",
		})
	}
	if err := printDocOutline(&buf, d, false, false, now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "  [0] Section 0") {
		t.Errorf("outline lacks the first row:\n%s", out)
	}
	if strings.Contains(out, "  ["+strconv.Itoa(outlineRows)+"] ") {
		t.Errorf("outline listed a row past the cap:\n%s", out)
	}
	if !strings.Contains(out, "more 5 blocks, orders 60..64") {
		t.Errorf("outline lacks the hidden-block summary:\n%s", out)
	}
}

func TestPrintDocOutlineTextVolume(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	d := testDoc()
	d.Size = 120
	d.FullChars = 400
	d.Truncated = true
	if err := printDocOutline(&buf, d, false, false, now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "text: 120 of 400 chars stored") {
		t.Errorf("outline lacks the stored/total volume:\n%s", out)
	}
	if !strings.Contains(out, "store the whole page with --max-chars 0") {
		t.Errorf("truncated outline lacks the whole-page hint:\n%s", out)
	}
}

func TestPrintDocHits(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	docs := []model.Doc{*testDoc()}
	docs[0].ExpiresAt = now.Add(-time.Hour)
	if err := printDocHits(&buf, docs, now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"abc123def4567890", "Example Guide", "[stale]", "first section text"} {
		if !strings.Contains(out, want) {
			t.Errorf("hits lack %q:\n%s", want, out)
		}
	}
}

func TestSnippetCapsAndCollapses(t *testing.T) {
	got := snippet("a\n\n  b   c", 10)
	if got != "a b c" {
		t.Errorf("snippet collapse = %q", got)
	}
	long := strings.Repeat("x", 50)
	got = snippet(long, 10)
	if !strings.HasPrefix(got, strings.Repeat("x", 10)) || !strings.HasSuffix(got, "…") {
		t.Errorf("snippet cap = %q", got)
	}
	if len([]rune(got)) != 11 { // 10 + ellipsis
		t.Errorf("snippet len = %d, want 11", len([]rune(got)))
	}
}

func TestPrintProbe(t *testing.T) {
	var buf bytes.Buffer
	// A probe note only ever comes from the 200 fallback (the server ignored
	// Range); a 206 leaves Note empty, so the binding is tested on 200.
	r := &client.ProbeResult{
		URL:          "https://example.com/doc",
		FinalURL:     "https://example.com/doc",
		Status:       200,
		ContentType:  "text/html",
		BytesTotal:   123456,
		BytesSampled: 65536,
		Method:       "range",
		Note:         "server ignored Range; sampled first 65536 bytes",
	}
	if err := printProbe(&buf, r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"status: 200",
		"content_type: text/html",
		"bytes_total: 123456",
		"bytes_sampled: 65536",
		"method: range",
		"final_url: https://example.com/doc",
		"note: server ignored Range",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("probe output lacks %q:\n%s", want, out)
		}
	}
	// A probe never stores anything: no doc line and no hash.
	if strings.Contains(out, "doc:") || strings.Contains(out, "hash:") {
		t.Errorf("probe output must not carry doc/hash lines:\n%s", out)
	}
}

func TestProbeUnknownSize(t *testing.T) {
	var buf bytes.Buffer
	if err := printProbe(&buf, &client.ProbeResult{Status: 200, BytesTotal: -1, Method: "head"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "bytes_total: unknown") {
		t.Errorf("negative size must render as unknown:\n%s", buf.String())
	}
	// An empty note must not print an empty "note:" line.
	if strings.Contains(buf.String(), "note:") {
		t.Errorf("an empty note must not be printed:\n%s", buf.String())
	}
	if got := bytesTotalLabel(0); got != "0" {
		t.Errorf("bytesTotalLabel(0) = %q, want 0", got)
	}
	if got := bytesTotalLabel(42); got != "42" {
		t.Errorf("bytesTotalLabel(42) = %q, want 42", got)
	}
	if got := bytesTotalLabel(-1); got != "unknown" {
		t.Errorf("bytesTotalLabel(-1) = %q, want unknown", got)
	}
}

func TestFetchCmdProbeFlag(t *testing.T) {
	saved := fetchFlags
	t.Cleanup(func() { fetchFlags = saved })

	cmd := FetchCmd()
	f := cmd.Flags().Lookup("probe")
	if f == nil {
		t.Fatal("fetch has no --probe flag")
	}
	if !strings.Contains(f.Usage, "ignores") || !strings.Contains(f.Usage, "--max-chars") {
		t.Errorf("probe usage must say it ignores the extraction flags: %q", f.Usage)
	}
	// --project and --allow-secret do not reach a probe either: they must be
	// named in the ignored set, not silently accepted.
	for _, flag := range []string{"--project", "--allow-secret"} {
		if !strings.Contains(f.Usage, flag) {
			t.Errorf("probe usage must list %s as ignored: %q", flag, f.Usage)
		}
	}
	if err := cmd.ParseFlags([]string{"--probe"}); err != nil {
		t.Fatal(err)
	}
	if !fetchFlags.probe {
		t.Error("--probe did not set the flag")
	}
}

func TestProbeOrLocalLocalClone(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "checkout", "velruna")
	newProbeRepo(t, repoDir, "https://github.com/NemoNobody/velruna.git", "README.md")
	cfg := &config.Config{LocalRepoRoots: []string{root}, LocalRepoScanDepth: 3}

	var buf bytes.Buffer
	url := "https://github.com/NemoNobody/velruna/blob/master/README.md"
	if err := probeOrLocal(context.Background(), &buf, cfg, url); err != nil {
		t.Fatalf("probeOrLocal: %v", err)
	}
	want := "local: " + filepath.Join(repoDir, "README.md")
	if got := strings.TrimSpace(buf.String()); got != want {
		t.Errorf("local clone path = %q, want %q", got, want)
	}
}

// newProbeRepo builds a throwaway repo the GitHub-local resolver accepts:
// a fixed branch, an origin remote, one commit.
func newProbeRepo(t *testing.T, dir, remote string, files ...string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		full := append([]string{"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", full, err, out)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run("init", "-q", "-b", "master", dir)
	run("-C", dir, "remote", "add", "origin", remote)
	for _, f := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("content of "+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("-c", "user.email=t@t", "-c", "user.name=t", "-C", dir, "add", "-A")
	run("-c", "user.email=t@t", "-c", "user.name=t", "-C", dir, "commit", "-q", "-m", "init")
}
