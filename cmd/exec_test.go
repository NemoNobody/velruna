package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/config"
)

func TestGuardExecIdentity(t *testing.T) {
	const secret = "AKIAABCDEFGHIJKLMNOP"
	reject := &config.Config{SecretGuard: config.GuardReject}

	if err := guardExecIdentity(reject, "proj", "key", false); err != nil {
		t.Errorf("clean identity refused: %v", err)
	}
	err := guardExecIdentity(reject, "proj "+secret, "key", false)
	if err == nil {
		t.Fatal("a credential in --project must refuse the capture")
	}
	if !strings.Contains(err.Error(), "field project") {
		t.Errorf("refusal must name the project field: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("refusal echoed the secret: %v", err)
	}
	err = guardExecIdentity(reject, "proj", "key-"+secret, false)
	if err == nil {
		t.Fatal("a credential in --key must refuse the capture")
	}
	if !strings.Contains(err.Error(), "field key") {
		t.Errorf("refusal must name the key field: %v", err)
	}

	// ask needs the confirmation fetch's --allow-secret provides.
	ask := &config.Config{SecretGuard: config.GuardAsk}
	if err := guardExecIdentity(ask, "proj", secret, false); err == nil {
		t.Error("ask without confirmation must refuse")
	}
	if err := guardExecIdentity(ask, "proj", secret, true); err != nil {
		t.Errorf("ask with confirmation must proceed: %v", err)
	}

	// A 64-hex key is the natural shape (the derived key has it). The medium
	// long-hex detector must not refuse it: only high-confidence shapes gate
	// an identity field, otherwise every ordinary --key would be rejected.
	hexKey := strings.Repeat("ab", 32)
	for _, level := range []string{config.GuardReject, config.GuardAsk} {
		if err := guardExecIdentity(&config.Config{SecretGuard: level}, "proj", hexKey, false); err != nil {
			t.Errorf("%s: a 64-hex --key was refused: %v", level, err)
		}
	}
}

func TestExecExitCode(t *testing.T) {
	code := func(n int) *int { return &n }
	tests := []struct {
		name      string
		res       *client.ExecResult
		propagate bool
		want      int
	}{
		{"capture success", &client.ExecResult{ExitCode: code(0)}, false, 0},
		{"capture failure of child", &client.ExecResult{ExitCode: code(3)}, false, 0},
		{"propagate success clamps to 1", &client.ExecResult{ExitCode: code(0)}, true, 1},
		{"propagate child code", &client.ExecResult{ExitCode: code(7)}, true, 7},
		{"propagate 255", &client.ExecResult{ExitCode: code(255)}, true, 255},
		{"propagate clamps high", &client.ExecResult{ExitCode: code(300)}, true, 255},
		{"timeout", &client.ExecResult{TimedOut: true}, true, 124},
		{"timeout wins over a code", &client.ExecResult{TimedOut: true, ExitCode: code(2)}, true, 124},
		{"no code", &client.ExecResult{}, true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := execExitCode(tt.res, tt.propagate); got != tt.want {
				t.Errorf("execExitCode(%+v, %v) = %d, want %d", tt.res, tt.propagate, got, tt.want)
			}
		})
	}
}

func TestPrintExecSummary(t *testing.T) {
	code := 0
	res := &client.ExecResult{
		Status:     "changed",
		ExitCode:   &code,
		Bytes:      12,
		TotalBytes: 20,
		SHA256:     strings.Repeat("ab", 32),
		DocHash:    "doc-key",
		Sections:   2,
		Truncated:  true,
		Elapsed:    3 * time.Millisecond,
		Redactions: 1,
	}
	var buf bytes.Buffer
	if err := printExecSummary(&buf, res); err != nil {
		t.Fatalf("printExecSummary: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"status: changed",
		"exit_code: 0",
		"timed_out: false",
		"bytes: 20 (truncated) (stored 12)",
		"sha256: " + strings.Repeat("ab", 6),
		"doc: doc-key",
		"sections: 2",
		"elapsed: 3ms",
		"truncated: true",
		"redactions: 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, strings.Repeat("ab", 32)) {
		t.Errorf("summary leaked the full hash:\n%s", out)
	}
}

func TestPrintExecSummaryTimeout(t *testing.T) {
	res := &client.ExecResult{
		Status:   "changed",
		TimedOut: true,
		Signal:   "killed",
		SHA256:   "0123456789abcdef",
		DocHash:  "k",
		Elapsed:  time.Second,
	}
	var buf bytes.Buffer
	if err := printExecSummary(&buf, res); err != nil {
		t.Fatalf("printExecSummary: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"exit_code: timeout", "timed_out: true", "signal: killed", "sha256: 0123456789ab"} {
		if !strings.Contains(out, want) {
			t.Errorf("timeout summary lacks %q:\n%s", want, out)
		}
	}
}
