package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/keyring"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var credsFlags struct {
	url string
}

// CredsCmd returns `velruna creds`: the OpenSearch credential lives in the OS
// keyring (service=velruna, account=host[:port]); these commands manage it
// without ever letting the secret pass through argv (arguments end up in
// shell history) or printing it back.
func CredsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "creds",
		Short: "Manage the OpenSearch password in the OS keyring",
		Long: `The credential resolves in this order: OPENSEARCH_PASSWORD env, the OS
keyring (service "velruna", account host[:port]), then the plaintext config
file — only when allow_plaintext is on. These commands write and inspect the
keyring entry; status never prints the value. The exception is 'creds get',
which prints the resolved password to stdout for scripted use (curl recipes,
dashboard pushes): treat its stdout as a secret.`,
	}
	cmd.PersistentFlags().StringVar(&credsFlags.url, "url", "",
		"target OpenSearch URL (default: the effective opensearch_url)")
	cmd.AddCommand(credsSetCmd(), credsGetCmd(), credsStatusCmd(), credsRmCmd())
	return cmd
}

// accountFor derives the keyring account (host[:port]) from a URL. Thin
// alias for config.KeyringAccount: the CLI and the config loader must never
// drift on which keyring entry a password lives under.
func accountFor(rawURL string) string { return config.KeyringAccount(rawURL) }

// refuseUserinfo rejects a url that carries credentials: the prompt, the
// "stored: account=..." line and creds status all quote the target, so a
// password in the url would leak to the terminal and shell history. The
// error text never quotes the url itself.
func refuseUserinfo(raw string) error {
	u, err := url.Parse(raw)
	if err == nil {
		if u.User != nil {
			return errors.New("the url carries user info; remove credentials from the url")
		}
		return nil
	}
	if strings.Contains(raw, "@") {
		// Parse failure: config.RedactURL returns the raw text back, and the
		// parse error echoes the input, so report a fixed non-echoing message.
		return errors.New("the url carries user info; remove credentials from the url")
	}
	return nil
}

// credsTarget resolves the URL whose host keys the keyring entry.
func credsTarget() (string, error) {
	if credsFlags.url != "" {
		if err := refuseUserinfo(credsFlags.url); err != nil {
			return "", err
		}
		return credsFlags.url, nil
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		return "", err
	}
	return targetFromConfig(cfg), nil
}

// targetFromConfig is the endpoint whose host keys the keyring entry: the
// first effective node, exactly what the runtime derives the password account
// from. A list-only config leaves opensearch_url at its default, so storing
// the secret under that default would never be found again.
func targetFromConfig(cfg *config.Config) string {
	return cfg.EffectiveURLs()[0]
}

func credsSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set",
		Short: "Store the OpenSearch password in the OS keyring",
		Long: `Read the password from an interactive prompt (hidden input) or from piped
stdin (for scripted migration), then store it in the OS keyring. The password
is never an argument: arguments persist in shell history.`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			target, err := credsTarget()
			if err != nil {
				fatalErr(err)
			}
			host := accountFor(target)
			pw, err := readSecret("password for "+host+" (input hidden): ", os.Stdin, errOut)
			if err != nil {
				fatalErr(err)
			}
			if pw == "" {
				fatalErr(errors.New("empty password: nothing stored"))
			}
			if err := keyring.Set(config.KeyringService, host, pw); err != nil {
				fatalErr(err)
			}
			if err := writeLine(out, "stored: service="+config.KeyringService+" account="+host); err != nil {
				fatalErr(err)
			}
		},
	}
}

// credsGetCmd prints the resolved password to stdout so scripts can pipe it
// (curl --config, env assignment) without the secret ever living in argv or
// shell history — the same discipline as "set" on the write side. With no
// --url it mirrors the client's own chain (env, keyring, plaintext config);
// with --url it reads only the keyring entry for that host (the effective
// config's password belongs to the configured endpoint, not to --url).
func credsGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "Print the resolved password to stdout (for piping in scripts)",
		Long: `Prints the password resolved by the client's own chain. With --url, prints
the keyring entry for that host instead. stdout is the secret: never redirect
it to a file or a log; consume it in a pipe or a shell assignment.`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			out := cmd.OutOrStdout()
			if credsFlags.url != "" {
				target, err := credsTarget()
				if err != nil {
					fatalErr(err)
				}
				host := accountFor(target)
				pw, err := keyring.Get(config.KeyringService, host)
				if err != nil {
					if errors.Is(err, keyring.ErrNotFound) {
						fatalErr(fmt.Errorf("no keyring entry for service=%s account=%s: store one with 'velruna creds set --url %s'", config.KeyringService, host, target))
					}
					fatalErr(err)
				}
				if pw == "" {
					fatalErr(fmt.Errorf("keyring entry for account=%s is empty", host))
				}
				if _, err := io.WriteString(out, pw+"\n"); err != nil {
					fatalErr(err)
				}
				return
			}
			cfg, err := config.LoadConfig()
			if err != nil {
				fatalErr(err)
			}
			if cfg.OpenSearchPassword == "" {
				// fatalErr writes to stderr: stdout must carry only the secret.
				fatalErr(errors.New("no password resolves: store one with 'velruna creds set' or export OPENSEARCH_PASSWORD (see 'velruna creds status')"))
			}
			if _, err := io.WriteString(out, cfg.OpenSearchPassword+"\n"); err != nil {
				fatalErr(err)
			}
		},
	}
}

func credsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show how the OpenSearch password resolves (never its value)",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			out := cmd.OutOrStdout()
			// One config load per command. With --url the endpoint is
			// overridden: the effective config's password source is about
			// a different host, so it is not shown.
			var cfg *config.Config
			target := credsFlags.url
			if target != "" {
				if err := refuseUserinfo(target); err != nil {
					fatalErr(err)
				}
			} else {
				c, cerr := config.LoadConfig()
				if cerr != nil {
					fatalErr(cerr)
				}
				cfg = c
				target = targetFromConfig(c)
			}
			host := accountFor(target)
			_, kerr := keyring.Get(config.KeyringService, host)
			text := renderCredsStatus(host, cfg, kerr, keyring.Backend())
			if _, err := io.WriteString(out, text); err != nil {
				fatalErr(err)
			}
		},
	}
}

// renderCredsStatus explains the credential resolution for one host. The
// password value is never part of the output. backend is the provider name the
// next operation would use (empty when none is reachable), so a machine with
// both Secret Service and a KWallet can tell where the entry lives.
func renderCredsStatus(host string, cfg *config.Config, keyringErr error, backend string) string {
	var b strings.Builder
	b.WriteString("account: ")
	b.WriteString(config.KeyringService)
	b.WriteString("/")
	b.WriteString(host)
	b.WriteString("\nsource: ")
	if cfg == nil {
		b.WriteString("n/a (--url overrides the configured endpoint)")
	} else {
		b.WriteString(cfg.PasswordSource)
	}
	if backend != "" {
		b.WriteString("\nbackend: ")
		b.WriteString(backend)
	}
	b.WriteString("\nkeyring: ")
	switch {
	case keyringErr == nil:
		b.WriteString("entry present (value not shown)\n")
	case errors.Is(keyringErr, keyring.ErrNotFound):
		b.WriteString("no entry — store one: velruna creds set\n")
	default:
		b.WriteString("unavailable: ")
		b.WriteString(keyringErr.Error())
		// An ErrBackend text already names its own fix (env var, ksecretd,
		// KWallet-native); adding the container hint would contradict it on a
		// desktop that simply has the wrong provider.
		if !errors.Is(keyringErr, keyring.ErrBackend) {
			b.WriteString("\n  (no session bus in containers/ssh: use OPENSEARCH_PASSWORD)")
		}
		b.WriteString("\n")
	}
	if cfg != nil {
		b.WriteString("config file plaintext: ")
		switch {
		case !cfg.FileHasPlaintext:
			b.WriteString("absent\n")
		case cfg.AllowPlaintext:
			b.WriteString("present, in use (allow_plaintext on)\n")
		case errors.Is(keyringErr, keyring.ErrBackend):
			b.WriteString("present, REFUSED (allow_plaintext off): fix the keyring backend above, or export OPENSEARCH_PASSWORD\n")
		default:
			b.WriteString("present, REFUSED (allow_plaintext off): migrate with 'velruna creds set', then remove opensearch_password from the file\n")
		}
		if cfg.PasswordSource == "none" {
			// A refused backend already prints its own fix above; the generic
			// "velruna creds set" advice would point at a store that cannot be
			// written to.
			if errors.Is(keyringErr, keyring.ErrBackend) {
				b.WriteString("hint: fix the keyring backend listed above, or export OPENSEARCH_PASSWORD\n")
			} else {
				b.WriteString("hint: store with 'velruna creds set' (keyring) or export OPENSEARCH_PASSWORD (containers, headless)\n")
			}
		}
	}
	return b.String()
}

func credsRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm",
		Short: "Remove the keyring entry for the OpenSearch host",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			out := cmd.OutOrStdout()
			target, err := credsTarget()
			if err != nil {
				fatalErr(err)
			}
			host := accountFor(target)
			derr := keyring.Delete(config.KeyringService, host)
			switch {
			case derr == nil:
				if err := writeLine(out, "removed: service="+config.KeyringService+" account="+host); err != nil {
					fatalErr(err)
				}
			case errors.Is(derr, keyring.ErrNotFound):
				if err := writeLine(out, "no entry for service="+config.KeyringService+" account="+host); err != nil {
					fatalErr(err)
				}
			default:
				fatalErr(derr)
			}
		},
	}
}

// readSecret reads a one-line secret: hidden input on a terminal, a plain
// line when stdin is piped (scripted migration). Trailing newlines are cut;
// interior whitespace is part of the secret.
func readSecret(prompt string, in *os.File, errOut io.Writer) (string, error) {
	fd := int(in.Fd())
	if term.IsTerminal(fd) {
		if _, err := io.WriteString(errOut, prompt); err != nil {
			return "", err
		}
		b, err := term.ReadPassword(fd)
		if _, werr := io.WriteString(errOut, "\n"); werr != nil {
			return "", werr
		}
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if line == "" && err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
