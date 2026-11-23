package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/config"

	"github.com/spf13/cobra"
)

var execFlags struct {
	timeout       time.Duration
	maxBytes      int64
	force         bool
	propagateExit bool
	project       string
	key           string
	allowSecret   bool
}

// ExecCmd returns the `velruna exec` command: run a command, capture merged
// stdout+stderr into the docs index, print only a short summary. The output
// never enters the caller's context unless it is read back with `velruna doc`.
//
// Put child flags after `--`: cobra parses its own flags everywhere else.
func ExecCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec [flags] -- <cmd> [args...]",
		Short: "Capture a command's output into the docs index and print a summary",
		Long: `Run a command without a shell, capture its merged stdout+stderr into the
docs index, and print only a short summary (status, exit code, size, hash,
doc id). The output itself is read back with 'velruna doc <hash> <n>', so a
long build or test log never enters an agent's context.

The capture is keyed by the resolved working directory and the exact argv,
so running the same command twice is a dedupe hit (use --force to rewrite).
At most --max-bytes are retained (head and tail halves); the summary marks
a truncated stream and reports the total size. The stored output and title
pass the full write guard: high- and medium-confidence credential shapes are
redacted, never refused. A credential shape in --project refuses the capture
(any confidence) and a high-confidence one in --key does too, because those
fields are stored as identity (doc.project and the doc id behind
exec://<key>).

Put child flags after --, e.g. 'velruna exec -- go test -run TestX ./...'.`,
		Args: cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			if execFlags.maxBytes < 0 {
				fatalErr(errors.New("--max-bytes must be 0 or greater"))
			}
			if execFlags.timeout <= 0 {
				fatalErr(errors.New("--timeout must be greater than zero"))
			}
			project := strings.TrimSpace(execFlags.project)
			key := strings.TrimSpace(execFlags.key)
			// Guard the stored identity fields before the child runs: a rejected
			// secret must cost nothing, and these fields cannot be rewritten
			// (a redacted project tag or doc id would change what is matched on).
			cfg, err := config.LoadConfig()
			if err != nil {
				fatalErr(err)
			}
			if err := guardExecIdentity(cfg, project, key, execFlags.allowSecret); err != nil {
				fatalErr(err)
			}
			c, err := newClientFromConfig(cfg)
			if err != nil {
				fatalErr(err)
			}
			res, err := c.CaptureExec(context.Background(), client.ExecRequest{
				Argv:     args,
				Timeout:  execFlags.timeout,
				MaxBytes: execFlags.maxBytes,
				Force:    execFlags.force,
				Project:  project,
				Key:      key,
			})
			if err != nil {
				fatalErr(err)
			}
			if err := printExecSummary(os.Stdout, res); err != nil {
				fatalErr(err)
			}
			if code := execExitCode(res, execFlags.propagateExit); code != 0 {
				os.Exit(code)
			}
		},
	}
	cmd.Flags().DurationVar(&execFlags.timeout, "timeout", client.DefaultExecTimeout,
		"kill the command after this long; partial output is still stored")
	cmd.Flags().Int64Var(&execFlags.maxBytes, "max-bytes", client.DefaultExecMaxBytes,
		"bytes of output to retain (head and tail halves; 0 = unlimited)")
	cmd.Flags().BoolVar(&execFlags.force, "force", false,
		"rewrite the stored capture even when the output hash is unchanged")
	cmd.Flags().BoolVar(&execFlags.propagateExit, "propagate-exit", false,
		"exit with the child's status (clamped to 1..255; a timeout exits 124)")
	cmd.Flags().StringVar(&execFlags.project, "project", "",
		"tag the captured document with this project")
	cmd.Flags().StringVar(&execFlags.key, "key", "",
		"override the capture's dedupe key (also its doc id)")
	cmd.Flags().BoolVar(&execFlags.allowSecret, "allow-secret", false,
		"confirm a secret-guard finding in --project/--key is a false positive (secret_guard=ask only)")
	return cmd
}

// guardExecIdentity applies fetch's no-rewrite guard to the two fields exec
// stores as identity: the project tag and the captured doc's id (exec://<key>).
// Rewriting them would silently change what the stored capture is matched on,
// so a finding refuses the capture (the key at high confidence only: the
// natural key is a 64-hex digest, which the medium long-hex shape matches, and
// refusing it would make --key unusable).
func guardExecIdentity(cfg *config.Config, project, key string, allowSecret bool) error {
	return guardCheck(cfg.SecretGuard, allowSecret,
		guardField{Name: "project", Text: project},
		guardField{Name: "key", Text: key, HighOnly: true})
}

// execExitCode maps a capture to the CLI's exit status. Without
// --propagate-exit a successful capture always exits 0; with it, a child
// status is clamped to 1..255 (0 would claim an empty result) and a timeout
// is the conventional 124.
func execExitCode(res *client.ExecResult, propagate bool) int {
	if !propagate {
		return 0
	}
	if res.TimedOut {
		return 124
	}
	if res.ExitCode == nil {
		return 1
	}
	code := *res.ExitCode
	if code < 1 {
		return 1
	}
	if code > 255 {
		return 255
	}
	return code
}

// printExecSummary renders the short answer: everything an agent needs to
// decide what to read next, and nothing that could be long.
func printExecSummary(w io.Writer, res *client.ExecResult) error {
	exit := "none"
	if res.ExitCode != nil {
		exit = strconv.Itoa(*res.ExitCode)
	}
	if res.TimedOut {
		exit = "timeout"
	}
	sha := res.SHA256
	if len(sha) > 12 {
		sha = sha[:12]
	}
	var b strings.Builder
	b.WriteString("status: ")
	b.WriteString(res.Status)
	b.WriteString("\nexit_code: ")
	b.WriteString(exit)
	b.WriteString("\ntimed_out: ")
	b.WriteString(strconv.FormatBool(res.TimedOut))
	b.WriteString("\nbytes: ")
	b.WriteString(strconv.FormatInt(res.TotalBytes, 10))
	if res.Truncated {
		b.WriteString(" (truncated)")
	}
	// Redaction can make the stored text longer than the raw stream (a marker
	// may exceed the span it replaced), so the two counts are reported apart
	// instead of as "stored of total", which reads like a bug when stored > total.
	if res.Bytes != res.TotalBytes {
		b.WriteString(" (stored ")
		b.WriteString(strconv.FormatInt(res.Bytes, 10))
		b.WriteString(")")
	}
	b.WriteString("\nsha256: ")
	b.WriteString(sha)
	b.WriteString("\ndoc: ")
	b.WriteString(res.DocHash)
	b.WriteString("\nsections: ")
	b.WriteString(strconv.Itoa(res.Sections))
	b.WriteString("\nelapsed: ")
	b.WriteString(res.Elapsed.String())
	b.WriteByte('\n')
	if res.Signal != "" {
		b.WriteString("signal: ")
		b.WriteString(res.Signal)
		b.WriteByte('\n')
	}
	b.WriteString("truncated: ")
	b.WriteString(strconv.FormatBool(res.Truncated))
	b.WriteString("\nredactions: ")
	b.WriteString(strconv.Itoa(res.Redactions))
	b.WriteByte('\n')
	_, err := io.WriteString(w, b.String())
	return err
}
