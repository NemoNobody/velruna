package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/NemoNobody/velruna/internal/model"
)

func TestExecCaptureWriterUnlimited(t *testing.T) {
	w := newExecCapture(0)
	chunks := []string{"hello ", "wor", "ld"}
	for _, chunk := range chunks {
		if _, err := io.WriteString(w, chunk); err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}
	}
	want := "hello world"
	if got := string(w.bytes()); got != want {
		t.Errorf("bytes = %q, want %q", got, want)
	}
	if w.total != int64(len(want)) {
		t.Errorf("total = %d, want %d", w.total, len(want))
	}
	if w.truncated() {
		t.Error("an unlimited capture must never truncate")
	}
	if got := w.fullRuneCount(); got != utf8.RuneCountInString(want) {
		t.Errorf("rune count = %d, want %d", got, utf8.RuneCountInString(want))
	}
	sum := sha256.Sum256([]byte(want))
	if got := w.hexSHA256(); got != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q, want %q", got, hex.EncodeToString(sum[:]))
	}
}

func TestExecCaptureWriterTruncation(t *testing.T) {
	tests := []struct {
		name      string
		limit     int64
		chunks    []string
		want      string
		total     int64
		truncated bool
	}{
		{"fits exactly", 10, []string{"0123456789"}, "0123456789", 10, false},
		{"one byte over", 10, []string{"0123456789a"}, "012346789a", 11, true},
		{"eight byte budget", 8, []string{"0123456789"}, "01236789", 10, true},
		{"split writes", 8, []string{"0123", "4567", "89ab"}, "012389ab", 12, true},
		{"one byte budget", 1, []string{"abc"}, "c", 3, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newExecCapture(tt.limit)
			var full strings.Builder
			for _, chunk := range tt.chunks {
				full.WriteString(chunk)
				if _, err := io.WriteString(w, chunk); err != nil {
					t.Fatalf("Write(%q): %v", chunk, err)
				}
			}
			if got := string(w.bytes()); got != tt.want {
				t.Errorf("bytes = %q, want %q", got, tt.want)
			}
			if w.total != tt.total {
				t.Errorf("total = %d, want %d", w.total, tt.total)
			}
			if w.truncated() != tt.truncated {
				t.Errorf("truncated = %v, want %v", w.truncated(), tt.truncated)
			}
			// The hash always describes the full stream, not the retained slice.
			sum := sha256.Sum256([]byte(full.String()))
			if got := w.hexSHA256(); got != hex.EncodeToString(sum[:]) {
				t.Errorf("sha256 = %q, want %q", got, hex.EncodeToString(sum[:]))
			}
			if got := w.fullRuneCount(); got != utf8.RuneCountInString(full.String()) {
				t.Errorf("rune count = %d, want %d", got, utf8.RuneCountInString(full.String()))
			}
		})
	}
}

func TestExecCaptureRuneCountAcrossChunks(t *testing.T) {
	w := newExecCapture(4)
	// "é" is two bytes: split the sequence between two writes.
	if _, err := io.WriteString(w, "a\xc3"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "\xa9b"); err != nil {
		t.Fatal(err)
	}
	if got := w.fullRuneCount(); got != 3 {
		t.Errorf("rune count = %d, want 3", got)
	}
	if got := string(w.bytes()); got != "aéb" {
		t.Errorf("bytes = %q, want %q", got, "aéb")
	}
	if w.truncated() {
		t.Error("four bytes in a four-byte budget must not truncate")
	}
}

// TestExecCaptureCutsAtRuneBoundary: when the middle is dropped, the head and
// tail must not keep half a rune each; the stored text has to stay valid UTF-8
// (otherwise JSON marshaling turns the fragments into U+FFFD and SizeRaw no
// longer describes what a read returns).
func TestExecCaptureCutsAtRuneBoundary(t *testing.T) {
	w := newExecCapture(4) // head 2 bytes + tail 2 bytes
	// "a" + "é" + "é" = 5 bytes: the two-byte head budget lands inside the
	// first é, the two-byte tail budget holds the second one whole.
	if _, err := io.WriteString(w, "a\u00e9\u00e9"); err != nil {
		t.Fatal(err)
	}
	if !w.truncated() {
		t.Fatal("five bytes in a four-byte budget must truncate")
	}
	got := string(w.bytes())
	if !utf8.ValidString(got) {
		t.Fatalf("truncated capture is not valid UTF-8: %q", got)
	}
	if got != "aé" {
		t.Errorf("bytes = %q, want the head plus the whole tail rune", got)
	}
}

func TestExecDedupeKeySymlink(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	argv := []string{"echo", "hi"}
	a, err := execDedupeKey(real, argv)
	if err != nil {
		t.Fatalf("execDedupeKey(real): %v", err)
	}
	b, err := execDedupeKey(link, argv)
	if err != nil {
		t.Fatalf("execDedupeKey(link): %v", err)
	}
	if a != b {
		t.Errorf("symlinked dir key %q != real dir key %q", b, a)
	}
	if len(a) != 64 {
		t.Errorf("key length = %d, want 64 hex chars", len(a))
	}
}

func TestExecDedupeKeyArgBoundaries(t *testing.T) {
	dir := t.TempDir()
	one, err := execDedupeKey(dir, []string{"echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	two, err := execDedupeKey(dir, []string{"echo", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if one == two {
		t.Error("one argv element and two elements must not share a key")
	}
}

func TestExecKeyOverride(t *testing.T) {
	req := ExecRequest{Argv: []string{"echo", "hi"}, Key: "fixed-key"}
	if got := resolveExecKey(req, "/tmp"); got != "fixed-key" {
		t.Errorf("resolved key = %q, want the override", got)
	}
	req.Key = ""
	got := resolveExecKey(req, "/tmp")
	if got == "fixed-key" || len(got) != 64 {
		t.Errorf("resolved key = %q, want the computed hash", got)
	}
}

func TestRedactExecText(t *testing.T) {
	red, rows := redactExecText("output", "x AKIAABCDEFGHIJKLMNOP y")
	if len(rows) != 1 {
		t.Fatalf("redactions = %+v, want 1", rows)
	}
	if rows[0].Field != "output" || rows[0].Pattern != "aws-access-key" || rows[0].Chars == 0 {
		t.Errorf("redaction row = %+v", rows[0])
	}
	if strings.Contains(red, "AKIAABCDEFGHIJKLMNOP") {
		t.Errorf("redacted text still carries the key: %q", red)
	}

	// A capture uses the full fact-text policy: medium-confidence generic
	// shapes (a long hex here) are redacted too, not tolerated as page noise.
	// The context must not name a hash: a commit/checksum hint deliberately
	// exempts the medium hex detector.
	hexToken := strings.Repeat("a1b2c3d4", 8)
	red, rows = redactExecText("output", "deployment id "+hexToken)
	if len(rows) != 1 || rows[0].Pattern != "long-hex" {
		t.Fatalf("medium redactions = %+v, want one long-hex", rows)
	}
	if strings.Contains(red, hexToken) {
		t.Errorf("medium shape survived the redaction: %q", red)
	}
}

// execStore is a minimal docs index behind the mock transport.
type execStore struct {
	index  bool
	stored string
	writes int
}

func (s *execStore) respond(r recordedRequest) (int, string) {
	switch {
	case r.Path == "/velruna-docs" && r.Method == http.MethodHead:
		if s.index {
			return http.StatusOK, ""
		}
		return http.StatusNotFound, "{}"
	case r.Path == "/velruna-docs" && r.Method == http.MethodPut:
		s.index = true
		return http.StatusOK, `{"acknowledged":true}`
	case strings.HasPrefix(r.Path, "/velruna-docs/_doc/"):
		if r.Method == http.MethodGet {
			if s.stored == "" {
				return http.StatusNotFound, "{}"
			}
			return http.StatusOK, `{"found":true,"_source":` + s.stored + `}`
		}
		s.stored = r.Body
		s.writes++
		return http.StatusOK, `{"result":"created"}`
	}
	return http.StatusInternalServerError, `{"status":500}`
}

func execPrintfPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("printf")
	if err != nil {
		t.Skipf("printf unavailable: %v", err)
	}
	return p
}

func TestCaptureExecStoresAndDeduplicates(t *testing.T) {
	dir := t.TempDir()
	st := &execStore{}
	c, _ := newFakeClientCfg(t, docsMutate, st.respond)
	ctx := context.Background()
	req := ExecRequest{
		Argv:    []string{execPrintfPath(t), "%s", "hello-exec"},
		Dir:     dir,
		Timeout: 10 * time.Second,
		Project: "proj",
	}

	res, err := c.CaptureExec(ctx, req)
	if err != nil {
		t.Fatalf("CaptureExec: %v", err)
	}
	if res.Status != "changed" {
		t.Errorf("status = %q, want changed", res.Status)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", res.ExitCode)
	}
	if res.Bytes != int64(len("hello-exec")) || res.TotalBytes != int64(len("hello-exec")) {
		t.Errorf("bytes = %d/%d, want %d", res.Bytes, res.TotalBytes, len("hello-exec"))
	}
	if res.Truncated {
		t.Error("small output must not be truncated")
	}
	if res.Sections != 1 {
		t.Errorf("sections = %d, want 1", res.Sections)
	}
	if len(res.SHA256) != 64 {
		t.Errorf("sha256 = %q, want 64 hex chars", res.SHA256)
	}
	if st.writes != 1 {
		t.Fatalf("writes = %d, want 1", st.writes)
	}

	var doc model.Doc
	if err := json.Unmarshal([]byte(st.stored), &doc); err != nil {
		t.Fatalf("stored doc not decodable: %v", err)
	}
	if doc.URL != "exec://"+res.DocHash || doc.URLHash != res.DocHash {
		t.Errorf("url/url_hash = %q/%q, want exec://%s", doc.URL, doc.URLHash, res.DocHash)
	}
	if doc.ContentHash != res.SHA256 {
		t.Errorf("content_hash = %q, want %q", doc.ContentHash, res.SHA256)
	}
	if doc.Via != "exec" || doc.Project != "proj" {
		t.Errorf("audit fields = via %q project %q", doc.Via, doc.Project)
	}
	if len(doc.Sections) != 1 || doc.Sections[0].Text != "hello-exec" {
		t.Errorf("sections = %+v, want the captured text", doc.Sections)
	}
	if doc.Size != len("hello-exec") || doc.SizeRaw != len("hello-exec") {
		t.Errorf("size = %d/%d, want %d", doc.Size, doc.SizeRaw, len("hello-exec"))
	}

	res2, err := c.CaptureExec(ctx, req)
	if err != nil {
		t.Fatalf("CaptureExec(second): %v", err)
	}
	if res2.Status != "unchanged" {
		t.Errorf("second status = %q, want unchanged", res2.Status)
	}
	if res2.DocHash != res.DocHash || res2.Sections != len(doc.Sections) {
		t.Errorf("unchanged result = %+v, want doc %s with %d sections", res2, res.DocHash, len(doc.Sections))
	}
	if st.writes != 1 {
		t.Errorf("an unchanged capture wrote again (writes = %d)", st.writes)
	}

	req.Force = true
	res3, err := c.CaptureExec(ctx, req)
	if err != nil {
		t.Fatalf("CaptureExec(force): %v", err)
	}
	if res3.Status != "changed" {
		t.Errorf("forced status = %q, want changed", res3.Status)
	}
	if st.writes != 2 {
		t.Errorf("forced capture writes = %d, want 2", st.writes)
	}
}

func TestCaptureExecTruncates(t *testing.T) {
	st := &execStore{}
	c, _ := newFakeClientCfg(t, docsMutate, st.respond)
	res, err := c.CaptureExec(context.Background(), ExecRequest{
		Argv:     []string{execPrintfPath(t), "%s", "0123456789"},
		Dir:      t.TempDir(),
		Timeout:  10 * time.Second,
		MaxBytes: 8,
	})
	if err != nil {
		t.Fatalf("CaptureExec: %v", err)
	}
	if !res.Truncated {
		t.Error("10 bytes in an 8-byte budget must truncate")
	}
	if res.TotalBytes != 10 {
		t.Errorf("total = %d, want 10", res.TotalBytes)
	}
	if res.Bytes != 8 {
		t.Errorf("stored bytes = %d, want 8", res.Bytes)
	}
	var doc model.Doc
	if err := json.Unmarshal([]byte(st.stored), &doc); err != nil {
		t.Fatalf("stored doc not decodable: %v", err)
	}
	if len(doc.Sections) != 1 || doc.Sections[0].Text != "01236789" {
		t.Errorf("stored text = %+v, want head 4 + tail 4", doc.Sections)
	}
	if doc.FullChars != 10 {
		t.Errorf("full_chars = %d, want the full stream's 10 runes", doc.FullChars)
	}
	if !doc.Truncated {
		t.Error("the stored doc must carry truncated=true")
	}
	// ContentHash covers the raw full stream, not the retained head+tail: a
	// change in the dropped middle must still be a dedupe miss.
	sum := sha256.Sum256([]byte("0123456789"))
	wantSHA := hex.EncodeToString(sum[:])
	if res.SHA256 != wantSHA {
		t.Errorf("sha256 = %q, want the raw stream hash %q", res.SHA256, wantSHA)
	}
	if doc.ContentHash != wantSHA {
		t.Errorf("content_hash = %q, want the raw stream hash %q", doc.ContentHash, wantSHA)
	}
}

func TestCaptureExecRedactsOutput(t *testing.T) {
	st := &execStore{}
	c, _ := newFakeClientCfg(t, docsMutate, st.respond)
	// Build the key in the output, not in argv: the title must stay clean so
	// this test counts exactly one output redaction.
	res, err := c.CaptureExec(context.Background(), ExecRequest{
		Argv:    []string{execPrintfPath(t), "AKIA%s", "ABCDEFGHIJKLMNOP"},
		Dir:     t.TempDir(),
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("CaptureExec: %v", err)
	}
	if res.Redactions != 1 {
		t.Errorf("redactions = %d, want 1", res.Redactions)
	}
	if strings.Contains(st.stored, "AKIAABCDEFGHIJKLMNOP") {
		t.Error("the stored document carries the unredacted key")
	}
	var doc model.Doc
	if err := json.Unmarshal([]byte(st.stored), &doc); err != nil {
		t.Fatalf("stored doc not decodable: %v", err)
	}
	if len(doc.Redactions) != 1 || doc.Redactions[0].Pattern != "aws-access-key" {
		t.Errorf("redaction rows = %+v, want one aws-access-key", doc.Redactions)
	}
	if len(doc.Sections) != 1 || !strings.Contains(doc.Sections[0].Text, "velruna-redacted") {
		t.Errorf("stored text = %+v, want a redaction marker", doc.Sections)
	}
	// Redaction rewrites the stored text but must not move the content hash:
	// dedupe follows the raw stream.
	sum := sha256.Sum256([]byte("AKIAABCDEFGHIJKLMNOP"))
	wantSHA := hex.EncodeToString(sum[:])
	if res.SHA256 != wantSHA {
		t.Errorf("sha256 = %q, want the raw stream hash %q", res.SHA256, wantSHA)
	}
	if doc.ContentHash != wantSHA {
		t.Errorf("content_hash = %q, want the raw stream hash %q", doc.ContentHash, wantSHA)
	}
}

func TestCaptureExecSplitsLongOutput(t *testing.T) {
	// Words separated by whitespace: a single 8000-char alphanumeric run would
	// now be one medium-confidence token and redacted, not split. The length
	// still exceeds execSectionChars, which is what this test needs.
	long := strings.Repeat("line of test output\n", 600)
	st := &execStore{}
	c, _ := newFakeClientCfg(t, docsMutate, st.respond)
	res, err := c.CaptureExec(context.Background(), ExecRequest{
		Argv:    []string{execPrintfPath(t), "%s", long},
		Dir:     t.TempDir(),
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("CaptureExec: %v", err)
	}
	if res.Sections != 2 {
		t.Errorf("sections = %d, want 2", res.Sections)
	}
	var doc model.Doc
	if err := json.Unmarshal([]byte(st.stored), &doc); err != nil {
		t.Fatalf("stored doc not decodable: %v", err)
	}
	if len(doc.Sections) != 2 || doc.Sections[0].Order != 0 || doc.Sections[1].Order != 1 {
		t.Fatalf("sections = %+v, want orders 0 and 1", doc.Sections)
	}
	if got := len(doc.Sections[0].Text) + len(doc.Sections[1].Text); got != len(long) {
		t.Errorf("block lengths sum to %d, want %d", got, len(long))
	}
}

// TestCaptureExecUnchangedReportsStoredDoc: on a dedupe hit nothing is
// rewritten, so the summary must describe the STORED copy, not this run's
// would-be truncation (--max-bytes may differ between runs).
func TestCaptureExecUnchangedReportsStoredDoc(t *testing.T) {
	st := &execStore{}
	c, _ := newFakeClientCfg(t, docsMutate, st.respond)
	dir := t.TempDir()
	argv := []string{execPrintfPath(t), "%s", "0123456789"}
	ctx := context.Background()

	first, err := c.CaptureExec(ctx, ExecRequest{Argv: argv, Dir: dir, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("CaptureExec(first): %v", err)
	}
	if first.Bytes != 10 || first.Truncated {
		t.Fatalf("first result = %+v, want the full 10-byte copy", first)
	}

	// Same raw stream, a budget that would truncate THIS run: the stored copy
	// (full, untruncated) is returned unchanged and must be reported as such.
	second, err := c.CaptureExec(ctx, ExecRequest{Argv: argv, Dir: dir, Timeout: 10 * time.Second, MaxBytes: 4})
	if err != nil {
		t.Fatalf("CaptureExec(second): %v", err)
	}
	if second.Status != "unchanged" {
		t.Fatalf("second status = %q, want unchanged", second.Status)
	}
	if second.Bytes != 10 {
		t.Errorf("unchanged bytes = %d, want the stored copy's 10", second.Bytes)
	}
	if second.Truncated {
		t.Error("unchanged result claims truncation the stored copy does not have")
	}
	if st.writes != 1 {
		t.Errorf("writes = %d, want the dedupe hit to write nothing", st.writes)
	}
}

// TestCaptureExecWaitDelayKeepsCapture: a command that exits 0 while a child
// still holds the pipes must not lose the capture. WaitDelay closes the pipes;
// the exit status comes from the process state and the output is stored.
func TestCaptureExecWaitDelayKeepsCapture(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the pipe-drain budget")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh unavailable: %v", err)
	}
	st := &execStore{}
	c, _ := newFakeClientCfg(t, docsMutate, st.respond)
	res, err := c.CaptureExec(context.Background(), ExecRequest{
		Argv:    []string{sh, "-c", "sleep 7 & echo done"},
		Dir:     t.TempDir(),
		Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("CaptureExec: %v", err)
	}
	if res.Status != "changed" {
		t.Errorf("status = %q, want changed (the capture succeeded)", res.Status)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want the shell's 0", res.ExitCode)
	}
	if !strings.Contains(st.stored, "done") {
		t.Error("the captured output was lost")
	}
}
