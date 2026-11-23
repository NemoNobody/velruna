package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/NemoNobody/velruna/internal/model"
	"github.com/NemoNobody/velruna/internal/secretguard"
)

const (
	// DefaultExecTimeout bounds one captured command. A capture is a note,
	// not a job runner: a process that outlives the budget is killed and its
	// partial output is stored (the summary says so).
	DefaultExecTimeout = 120 * time.Second
	// DefaultExecMaxBytes is the CLI's default storage budget: the head and
	// tail halves are kept, the middle is dropped.
	DefaultExecMaxBytes int64 = 4 << 20
	// execSectionChars is the target block size for stored command output.
	// It matches the docs default so a captured run reads like any other doc.
	execSectionChars = 8000
	// execWaitDelay bounds the post-exit drain of the stdout/stderr pipes. A
	// command may leave a grandchild holding the write end; without this the
	// capture would hang after the command itself has exited.
	execWaitDelay = 5 * time.Second
)

// ViaExec marks a document captured from a command's output rather than
// fetched from a URL (see the model.Doc Via enum).
const ViaExec = "exec"

// ExecRequest describes one captured command.
type ExecRequest struct {
	Argv     []string      // Argv[0] is the binary; no shell
	Dir      string        // cwd; normalized with filepath.EvalSymlinks
	Timeout  time.Duration // default 120s
	MaxBytes int64         // default 4 MiB; 0 = unlimited
	Force    bool          // rewrite even when the output hash is unchanged
	Project  string        // optional doc tag
	Key      string        // optional dedupe key override
}

// ExecResult is the short summary of a capture. The output itself lives in the
// docs index; nothing that could be long travels back to the caller.
type ExecResult struct {
	Status     string // "changed" | "unchanged"
	ExitCode   *int   // nil when the process was killed/timed out
	TimedOut   bool
	Signal     string // e.g. "killed" when timed out
	Bytes      int64  // bytes of stored (redacted) output
	TotalBytes int64  // bytes of the full stream before truncation
	SHA256     string // sha256 of the full RAW stream, before redaction/truncation (ContentHash)
	DocHash    string // docs-index id
	Sections   int
	Truncated  bool
	Elapsed    time.Duration
	Redactions int
}

// CaptureExec runs one command, captures merged stdout+stderr into the docs
// index, and returns a summary. The stored text is redacted with the write
// guard; the full stream is never held in memory beyond the head/tail budget.
//
// The command's exit status is data, not an error: a non-zero code is reported
// in the result (unless --propagate-exit asks the CLI to adopt it). Only a
// failure to start the command, or a store failure, returns an error.
func (c *Client) CaptureExec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return nil, errors.New("exec: no command given")
	}
	if req.MaxBytes < 0 {
		return nil, fmt.Errorf("exec: max bytes must be 0 or greater, got %d", req.MaxBytes)
	}
	dir, err := normalizeExecDir(req.Dir)
	if err != nil {
		return nil, err
	}
	// Fail before a long command runs when the store is unreachable; the same
	// call also makes the dedupe read below valid.
	if err := c.EnsureDocsIndex(ctx); err != nil {
		return nil, err
	}
	key := resolveExecKey(req, dir)

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultExecTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	capture := newExecCapture(req.MaxBytes)
	cmd := exec.CommandContext(runCtx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	// One writer for both streams: os/exec reuses the same pipe when Stdout and
	// Stderr are the same value, so the merged order is the child's own.
	cmd.Stdout = capture
	cmd.Stderr = capture
	cmd.WaitDelay = execWaitDelay
	// Put the child in its own process group so a timeout kills the whole tree
	// (a daemonized grandchild must not outlive the capture). On Windows this is
	// a no-op (the platform kills the process tree differently).
	configureProcessGroup(cmd)

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	var exitCode *int
	var exitErr *exec.ExitError
	switch {
	case timedOut:
		// Killed by the context: no exit code, partial output is still stored.
	case runErr == nil:
		code := 0
		exitCode = &code
	case errors.Is(runErr, exec.ErrWaitDelay):
		// The command itself exited, but a child kept the pipes open past
		// WaitDelay. Wait closed them, so the capture is complete: a WaitDelay
		// expiry is not a capture failure. The exit status is the command's
		// own, taken from the process state.
		if cmd.ProcessState != nil {
			if code := cmd.ProcessState.ExitCode(); code >= 0 {
				exitCode = &code
			}
		}
	case errors.As(runErr, &exitErr):
		if code := exitErr.ExitCode(); code >= 0 {
			exitCode = &code
		}
	default:
		return nil, fmt.Errorf("exec %q: %w", req.Argv[0], runErr)
	}
	signal := ""
	if timedOut {
		signal = "killed"
	}

	// The command line is data too: a credential in argv must not land in the
	// plaintext store through the title. Both fields go through the same guard.
	raw := string(capture.bytes())
	text, outputRows := redactExecText("output", raw)
	title, titleRows := redactExecText("title", "exec: "+strings.Join(req.Argv, " "))
	redactions := make([]model.Redaction, 0, len(titleRows)+len(outputRows))
	redactions = append(redactions, titleRows...)
	redactions = append(redactions, outputRows...)

	// The content hash covers every raw byte the command streamed, before
	// redaction and before truncation. It is the dedupe identity: a rewritten
	// title or redacted span must not move it, and a change anywhere in a
	// stream whose middle was dropped must still be detected. A hash of raw
	// bytes reveals nothing about their content.
	sha := capture.hexSHA256()

	storedRunes := utf8.RuneCountInString(text)
	// SizeRaw describes the STORED text: measured after the rune-boundary cut
	// and the redaction, so it matches the bytes a read returns.
	storedBytes := len(text)
	fullChars := storedRunes
	if capture.truncated() {
		// The dropped middle was never redacted, so adjust the full rune count
		// by the redaction delta of the part that was retained.
		fullChars = capture.fullRuneCount() + storedRunes - utf8.RuneCountInString(raw)
		if fullChars < storedRunes {
			fullChars = storedRunes
		}
	}

	blocks := splitBlocks(text, execSectionChars)
	sections := make([]model.Section, 0, len(blocks))
	for k, block := range blocks {
		sections = append(sections, model.Section{
			Order:   k,
			Heading: partHeading("Part", k, len(blocks)),
			Text:    block,
		})
	}

	host, _ := os.Hostname()
	doc := &model.Doc{
		URL:         "exec://" + key,
		URLHash:     key,
		Title:       title,
		Project:     req.Project,
		FetchedAt:   time.Now().UTC(),
		Size:        storedRunes,
		SizeRaw:     storedBytes,
		FullChars:   fullChars,
		Truncated:   capture.truncated(),
		FetchedBy:   host,
		Via:         ViaExec,
		Sections:    sections,
		Redactions:  redactions,
		ContentHash: sha,
	}

	prior, gerr := c.GetDocByHash(ctx, key)
	switch {
	case gerr == nil && !req.Force && prior.ContentHash == sha:
		// Nothing is rewritten, so the answer describes the STORED document:
		// this run's --max-bytes may differ from the one the copy was written
		// with, and reporting this run's size/truncation would lie about what
		// a reader will find.
		return &ExecResult{
			Status:     "unchanged",
			ExitCode:   exitCode,
			TimedOut:   timedOut,
			Signal:     signal,
			Bytes:      int64(prior.SizeRaw),
			TotalBytes: capture.total,
			SHA256:     sha,
			DocHash:    key,
			Sections:   len(prior.Sections),
			Truncated:  prior.Truncated,
			Elapsed:    elapsed,
			Redactions: len(redactions),
		}, nil
	case gerr != nil && !errors.Is(gerr, ErrNoDoc):
		return nil, fmt.Errorf("exec: read stored capture: %w", gerr)
	}

	if err := c.PutDoc(ctx, doc); err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}
	return &ExecResult{
		Status:     "changed",
		ExitCode:   exitCode,
		TimedOut:   timedOut,
		Signal:     signal,
		Bytes:      int64(storedBytes),
		TotalBytes: capture.total,
		SHA256:     sha,
		DocHash:    key,
		Sections:   len(sections),
		Truncated:  capture.truncated(),
		Elapsed:    elapsed,
		Redactions: len(redactions),
	}, nil
}

// normalizeExecDir turns the requested working directory into an absolute,
// symlink-free path. Stable identity is the point: the same directory named
// through two symlinks must produce one dedupe key.
func normalizeExecDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("exec: resolve working directory: %w", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("exec: resolve %q: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("exec: resolve %q: %w", dir, err)
	}
	return resolved, nil
}

// execKeyHash is the identity of one command in one directory: the resolved
// directory separated from every argv element by NUL. NUL joining keeps
// ["echo hi"] and ["echo", "hi"] distinct, and it is the same key for the doc
// id and the dedupe lookup.
func execKeyHash(resolvedDir string, argv []string) string {
	sum := sha256.Sum256([]byte(resolvedDir + "\x00" + strings.Join(argv, "\x00")))
	return hex.EncodeToString(sum[:])
}

// execDedupeKey normalizes dir and hashes the command identity.
func execDedupeKey(dir string, argv []string) (string, error) {
	resolved, err := normalizeExecDir(dir)
	if err != nil {
		return "", err
	}
	return execKeyHash(resolved, argv), nil
}

// resolveExecKey applies the caller's override, if any.
func resolveExecKey(req ExecRequest, resolvedDir string) string {
	if req.Key != "" {
		return req.Key
	}
	return execKeyHash(resolvedDir, req.Argv)
}

// redactExecText applies the project's full write guard (RedactAll, the same
// policy fact text fields use): high-confidence credential shapes AND the
// medium-confidence generic tokens (a long hex or base64 blob) are replaced.
// A capture is a short, writer-authored artifact rather than page markup, so
// there is no "medium shapes are probably asset hashes" tolerance here; the
// audit rows say exactly what was cut. The second pass catches a span that
// starts inside an earlier replacement and was dropped by the first pass'
// fold.
func redactExecText(field, text string) (string, []model.Redaction) {
	red, findings := secretguard.RedactAll(field, text)
	if len(findings) > 0 {
		var more []secretguard.Finding
		red, more = secretguard.RedactAll(field, red)
		findings = append(findings, more...)
	}
	rows := make([]model.Redaction, 0, len(findings))
	for _, f := range findings {
		rows = append(rows, model.Redaction{Field: field, Pattern: f.Pattern, Chars: f.Length})
	}
	return red, rows
}

// execCapture is the streaming sink: every byte feeds the hash and the total,
// while memory stays O(MaxBytes) by keeping a head prefix and a tail ring. When
// MaxBytes is zero the whole stream is kept.
type execCapture struct {
	mu      sync.Mutex
	limit   int64
	head    []byte
	headMax int
	tail    []byte
	tailMax int
	tailPos int // next slot to overwrite once the ring is full
	tailLen int
	total   int64
	all     []byte
	hash    hash.Hash
	runes   runeCounter
}

func newExecCapture(limit int64) *execCapture {
	w := &execCapture{limit: limit, hash: sha256.New()}
	if limit > 0 {
		w.headMax = int(limit / 2)
		w.tailMax = int(limit) - w.headMax
		w.tail = make([]byte, w.tailMax)
	}
	return w
}

func (w *execCapture) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	_, _ = w.hash.Write(p)
	w.total += int64(n)
	w.runes.add(p)
	if w.limit <= 0 {
		w.all = append(w.all, p...)
		return n, nil
	}
	if len(w.head) < w.headMax {
		k := w.headMax - len(w.head)
		if k > len(p) {
			k = len(p)
		}
		w.head = append(w.head, p[:k]...)
		p = p[k:]
	}
	if len(p) > 0 {
		w.writeTail(p)
	}
	return n, nil
}

// writeTail keeps the newest tailMax bytes in a ring. A chunk at least as
// large as the ring replaces it whole, which keeps bulk output copy-bound
// instead of byte-loop-bound.
func (w *execCapture) writeTail(p []byte) {
	if w.tailMax == 0 {
		return
	}
	if len(p) >= w.tailMax {
		copy(w.tail, p[len(p)-w.tailMax:])
		w.tailPos = 0
		w.tailLen = w.tailMax
		return
	}
	copy(w.tail[w.tailPos:], p)
	if w.tailPos+len(p) > w.tailMax {
		copy(w.tail, p[w.tailMax-w.tailPos:])
	}
	w.tailPos = (w.tailPos + len(p)) % w.tailMax
	if w.tailLen < w.tailMax {
		w.tailLen += len(p)
		if w.tailLen > w.tailMax {
			w.tailLen = w.tailMax
		}
	}
}

// bytes returns the retained text: head then tail in stream order. When the
// middle was dropped, the head/tail junction is moved to a rune boundary so a
// cut UTF-8 sequence cannot become U+FFFD on marshal (which would make the
// stored text disagree with SizeRaw/Bytes). An untruncated capture is the
// original byte stream and is returned untouched.
func (w *execCapture) bytes() []byte {
	if w.limit <= 0 {
		return w.all
	}
	if w.tailLen == 0 {
		return w.head
	}
	head := w.head
	tail := w.tail[:w.tailLen]
	if w.tailLen == w.tailMax {
		tail = append(append([]byte(nil), w.tail[w.tailPos:]...), w.tail[:w.tailPos]...)
	}
	if w.truncated() {
		head = trimIncompleteRuneEnd(head)
		tail = trimIncompleteRuneStart(tail)
	}
	out := make([]byte, 0, len(head)+len(tail))
	out = append(out, head...)
	return append(out, tail...)
}

// trimIncompleteRuneEnd drops a trailing incomplete UTF-8 sequence from b
// (a rune whose lead byte is kept but whose continuation bytes fell into the
// dropped middle). Invalid bytes are left alone: they came from the command,
// not from the cut.
func trimIncompleteRuneEnd(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if !utf8.RuneStart(b[i]) {
			continue
		}
		if utf8.FullRune(b[i:]) {
			return b
		}
		return b[:i]
	}
	return b
}

// trimIncompleteRuneStart drops leading continuation bytes of a tail that
// begins in the middle of a rune.
func trimIncompleteRuneStart(b []byte) []byte {
	i := 0
	for i < len(b) && i < utf8.UTFMax && !utf8.RuneStart(b[i]) {
		i++
	}
	return b[i:]
}

func (w *execCapture) truncated() bool {
	return w.limit > 0 && w.total > w.limit
}

// hexSHA256 is the hash of every byte the command wrote, before redaction and
// before truncation.
func (w *execCapture) hexSHA256() string {
	return hex.EncodeToString(w.hash.Sum(nil))
}

// fullRuneCount is the rune count of the full stream. An incomplete UTF-8
// sequence at EOF counts one rune per byte, exactly as utf8.RuneCount does.
func (w *execCapture) fullRuneCount() int {
	return w.runes.n + len(w.runes.carry)
}

// runeCounter counts runes across chunk boundaries: a Write may split a
// multi-byte sequence, so the incomplete tail is carried to the next chunk.
type runeCounter struct {
	n     int
	carry []byte
}

func (r *runeCounter) add(p []byte) {
	if len(r.carry) > 0 {
		p = append(r.carry, p...)
		r.carry = nil
	}
	for len(p) > 0 {
		need := execRuneLen(p[0])
		if need <= 1 {
			r.n++
			p = p[1:]
			continue
		}
		if len(p) < need {
			r.carry = append(r.carry[:0], p...)
			return
		}
		_, size := utf8.DecodeRune(p)
		r.n++
		p = p[size:]
	}
}

// execRuneLen returns the length a valid sequence starting with b would have.
// A continuation byte or an invalid lead is one rune (the RuneError case).
func execRuneLen(b byte) int {
	switch {
	case b < 0x80:
		return 1
	case b < 0xC0:
		return 1
	case b < 0xE0:
		return 2
	case b < 0xF0:
		return 3
	case b < 0xF8:
		return 4
	default:
		return 1
	}
}
