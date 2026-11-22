package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/keyring"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var credsFlags struct {
	url string
}

// CredsCmd returns `velruna creds`: the OpenSearch credential lives in the OS
// keyring, keyed by the store's identity rather than by a host; these commands
// manage it without ever letting the secret pass through argv (arguments end
// up in shell history) or printing it back.
func CredsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "creds",
		Short: "Manage the OpenSearch password in the OS keyring",
		Long: `The credential resolves in this order: OPENSEARCH_PASSWORD env, the OS
keyring, then the plaintext config file — only when allow_plaintext is on.
Inside the keyring the entry is named after the store, not after one node: the
cluster account "velruna/cluster/<opensearch_cluster>" is tried first, then the
legacy "velruna/<host[:port]>" entry that predates cluster naming.

No command here prints the secret. Tooling that needs it reads it back through
the keyring; scripts pass it in their own environment with OPENSEARCH_PASSWORD.`,
	}
	cmd.PersistentFlags().StringVar(&credsFlags.url, "url", "",
		"target OpenSearch URL (default: the effective opensearch_url)")
	cmd.AddCommand(credsSetCmd(), credsStatusCmd(), credsRmCmd())
	return cmd
}

// accountFor derives the legacy keyring account (host[:port]) from a URL. Thin
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

// recordedClusterName is the store name the config already declares
// (opensearch_cluster / OPENSEARCH_CLUSTER). The runtime, creds status and
// creds rm all resolve the candidate account from it the same way.
func recordedClusterName(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.OpenSearchCluster)
}

// credsLoadTarget returns the effective config (nil when --url overrides the
// endpoint, because then no config's cluster name or user context applies) and
// the URL to probe/store against.
func credsLoadTarget() (*config.Config, string, error) {
	if credsFlags.url != "" {
		if err := refuseUserinfo(credsFlags.url); err != nil {
			return nil, "", err
		}
		return nil, credsFlags.url, nil
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, "", err
	}
	return cfg, targetFromConfig(cfg), nil
}

// targetFromConfig is the endpoint whose host keys the legacy keyring entry:
// the first effective node, exactly what the runtime derives the account from.
// A list-only config leaves opensearch_url at its default, so storing the
// secret under that default would never be found again.
func targetFromConfig(cfg *config.Config) string {
	return cfg.EffectiveURLs()[0]
}

func credsSetCmd() *cobra.Command {
	// The label lives in the constructor, not in credsFlags: only 'set'
	// interprets it, and a shared struct would leak it into other subcommands.
	var name string
	set := &cobra.Command{
		Use:   "set",
		Short: "Store the OpenSearch password in the OS keyring",
		Long: `Read the password from an interactive prompt (hidden input) or from piped
stdin (for scripted migration), then store it in the OS keyring. The password
is never an argument: arguments persist in shell history.

The entry is named after the store, not after one node: 'set' asks the cluster
who it is (its cluster_uuid) and stores under "cluster/<uuid>", recording the
same name in the config file as opensearch_cluster. A node can then be
replaced, moved to another port or dropped from opensearch_urls without the
password going missing. --name records an explicit label instead. When nothing
answers at the configured endpoint, the entry falls back to the legacy
host[:port] account and the output says so. When the store answers but rejects
the password (HTTP 401), nothing is stored: a credential the store itself
refuses cannot work in the runtime either.`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			cfg, target, err := credsLoadTarget()
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

			cluster := strings.TrimSpace(name)
			if cluster == "" {
				uuid, perr := probeClusterUUID(target, cfg, pw)
				if perr != nil {
					if probeRejectedPassword(perr) {
						// The store ANSWERED and refused this credential (401).
						// Storing it — especially over the named entry the runtime
						// reads first — would swap a working credential for one
						// the runtime cannot use, and "stored:" would be a lie.
						fatalErr(fmt.Errorf("the store at %s rejected the new password (HTTP 401): nothing stored — "+
							"the runtime authenticates the same way; re-run with the right password "+
							"(the probe authenticated as user %q — set OPENSEARCH_USER or opensearch_user if the store uses another)",
							config.RedactURL(target), probeUser(cfg)))
					}
					if recorded := recordedClusterName(cfg); recorded != "" {
						// The config already names this store (a previous
						// successful set). Reuse that name: the runtime reads
						// cluster/<name> BEFORE the legacy host entry, so a
						// rotated password stored only under the host account
						// would sit under an entry nothing reads first — the
						// rotation would report success and change nothing.
						cluster = recorded
						if _, werr := fmt.Fprintf(errOut, "warning: the store at %s did not confirm its "+
							"identity (%v): reusing the store name from the config, storing under %s\n",
							config.RedactURL(target), perr, config.ClusterAccount(cluster)); werr != nil {
							fatalErr(werr)
						}
					} else {
						// The store did not confirm its identity: keying by host is
						// the only name left. Saying so keeps a later "entry
						// disappeared after the node moved" explainable.
						if _, werr := fmt.Fprintf(errOut, "warning: the store at %s did not confirm its "+
							"identity (%v): storing under the legacy account %s\n",
							config.RedactURL(target), perr, host); werr != nil {
							fatalErr(werr)
						}
					}
				} else {
					cluster = uuid
				}
			}

			account := host
			if cluster != "" {
				account = config.ClusterAccount(cluster)
			}
			if err := keyring.Set(config.KeyringService, account, pw); err != nil {
				fatalErr(err)
			}
			if err := writeLine(out, "stored: service="+config.KeyringService+" account="+account); err != nil {
				fatalErr(err)
			}
			reportClusterName(out, errOut, cfg, cluster)
		},
	}
	set.Flags().StringVar(&name, "name", "",
		"cluster label for the keyring account (default: the cluster_uuid the store reports)")
	return set
}

// errPasswordRejected marks a probe failure where the store ANSWERED and
// refused the credential itself (HTTP 401). Silence, timeouts and permission
// refusals (403: a valid user without cluster-info rights — the password may
// be perfectly fine) are not rejections.
var errPasswordRejected = errors.New("password rejected")

func probeRejectedPassword(err error) bool {
	return errors.Is(err, errPasswordRejected)
}

// probeUser is the identity the probe authenticates with — the config's
// user, else OPENSEARCH_USER, else the runtime's default. An anonymous
// probe against a secured store would answer 401 and read as a rejected
// password, so probe and runtime must authenticate alike.
func probeUser(cfg *config.Config) string {
	if cfg != nil {
		if u := strings.TrimSpace(cfg.OpenSearchUser); u != "" {
			return u
		}
	}
	if u := strings.TrimSpace(os.Getenv("OPENSEARCH_USER")); u != "" {
		return u
	}
	return config.DefaultConfig().OpenSearchUser
}

// probeClusterUUID asks the store who it is, authenticating with the password
// being stored: the cluster name keys the new entry, so the answer has to come
// from a request that can already succeed with that credential.
func probeClusterUUID(target string, base *config.Config, pw string) (string, error) {
	insecure := false
	addresses := []string{target}
	if base != nil {
		insecure = base.InsecureSkipVerify
		// The store may be alive behind a node that is not the first in the
		// list: hand the probe every configured node so the client can fail
		// over instead of declaring a reachable store silent.
		addresses = base.EffectiveURLs()
	}
	user := probeUser(base)
	// One deadline covers all attempts; both it and the per-attempt timeout
	// scale with the node list so a slow node — even one that never answers
	// — cannot eat the probe before a live node gets its turn. The probe
	// client is read-only (Info only), so unlike the runtime client a
	// timed-out attempt is safe to retry on the next node.
	budget, attempt := 15*time.Second, 15*time.Second
	if n := len(addresses); n > 1 {
		budget = time.Duration(n) * 10 * time.Second
		if budget > 90*time.Second {
			budget = 90 * time.Second
		}
		if attempt = budget / time.Duration(n); attempt < 5*time.Second {
			attempt = 5 * time.Second
		}
	}
	api, err := opensearchapi.NewClient(opensearchapi.Config{
		Client: opensearch.Config{
			Addresses:          addresses,
			Username:           user,
			Password:           pw,
			InsecureSkipVerify: insecure,
			// One retry budget per node. For a single node the transport
			// still applies its default retries after transient faults —
			// safe here because Info is read-only, and the scaled deadline
			// bounds the whole probe either way.
			MaxRetries:           len(addresses) - 1,
			EnableRetryOnTimeout: true,
			RequestTimeout:       attempt,
		},
	})
	if err != nil {
		return "", fmt.Errorf("create probe client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	info, err := api.Info(ctx, nil)
	if err != nil {
		// The HTTP answer — not the body shape — is the proof: a store
		// behind a proxy that returns 401 with an HTML page says the same
		// thing as one that returns the OpenSearch error JSON.
		if info != nil && info.Inspect().Response != nil &&
			info.Inspect().Response.StatusCode == http.StatusUnauthorized {
			return "", fmt.Errorf("%w: %w", errPasswordRejected, err)
		}
		return "", err
	}
	if info == nil || strings.TrimSpace(info.ClusterUUID) == "" {
		return "", errors.New("the store answered without a cluster_uuid")
	}
	return strings.TrimSpace(info.ClusterUUID), nil
}

// reportClusterName records opensearch_cluster in the config file that supplied
// the endpoint, so the runtime picks the cluster account on the next command.
// An env-only setup (no config file) or a --url override cannot be recorded
// automatically; the operator gets the exact line to add instead of a silent
// mismatch between the entry and what the config asks for.
func reportClusterName(out, errOut io.Writer, cfg *config.Config, cluster string) {
	if cluster == "" {
		return
	}
	if cfg == nil {
		// --url override: the entry belongs to whichever store answered, but
		// writing the config would rename a different endpoint's identity.
		if err := writeLine(errOut, "note: --url does not touch the config; to use this entry for the "+
			"configured store, record \"opensearch_cluster\": \""+cluster+"\" in it (or OPENSEARCH_CLUSTER)"); err != nil {
			fatalErr(err)
		}
		return
	}
	path := config.FindConfigPath()
	if path == "" {
		if err := writeLine(errOut, "note: no config file in use; set OPENSEARCH_CLUSTER="+cluster+
			" or add \"opensearch_cluster\": \""+cluster+"\" to a config file so the entry is found again"); err != nil {
			fatalErr(err)
		}
		return
	}
	prev, err := config.SetConfigValue(path, "opensearch_cluster", cluster)
	if err != nil {
		if _, werr := fmt.Fprintf(errOut, "warning: could not record opensearch_cluster in %s (%v): "+
			"add \"opensearch_cluster\": \"%s\" by hand or export OPENSEARCH_CLUSTER\n", path, err, cluster); werr != nil {
			fatalErr(werr)
		}
		return
	}
	if err := writeLine(out, "config: opensearch_cluster="+cluster+" recorded in "+path); err != nil {
		fatalErr(err)
	}
	if prev != "" && prev != cluster {
		// The store this config pointed at before keeps its keyring entry:
		// nothing reads it any more, and silence would leak a secret slot.
		if err := writeLine(out, "note: the config previously named "+prev+": its keyring entry (account "+
			config.ClusterAccount(prev)+") stays until removed with 'velruna creds rm --name "+prev+"'"); err != nil {
			fatalErr(err)
		}
	}
}

// accountProbe is one candidate keyring account and what reading it answered.
type accountProbe struct {
	account string
	kind    string // "cluster" | "legacy"
	err     error
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
			cfg, target, err := credsLoadTarget()
			if err != nil {
				fatalErr(err)
			}
			probes := probeAccounts(cfg, target)
			text := renderCredsStatus(cfg, probes, keyring.Backend())
			if _, err := io.WriteString(out, text); err != nil {
				fatalErr(err)
			}
		},
	}
}

// probeAccounts reads every candidate entry, in the order the runtime would
// try them, and reports the outcome per entry (never the value).
func probeAccounts(cfg *config.Config, target string) []accountProbe {
	var probes []accountProbe
	if name := recordedClusterName(cfg); name != "" {
		probes = append(probes, accountProbe{account: config.ClusterAccount(name), kind: "cluster"})
	}
	probes = append(probes, accountProbe{account: accountFor(target), kind: "legacy"})
	for i := range probes {
		_, probes[i].err = keyring.Get(config.KeyringService, probes[i].account)
	}
	return probes
}

// renderCredsStatus explains the credential resolution for the candidate
// accounts. The password value is never part of the output. backend is the
// provider name the next operation would use (empty when none is reachable),
// so a machine with both Secret Service and a KWallet can tell where the entry
// lives.
func renderCredsStatus(cfg *config.Config, probes []accountProbe, backend string) string {
	var b strings.Builder
	b.WriteString("accounts:\n")
	for _, p := range probes {
		b.WriteString("  ")
		b.WriteString(config.KeyringService)
		b.WriteString("/")
		b.WriteString(p.account)
		b.WriteString(" (")
		b.WriteString(p.kind)
		switch {
		case p.err == nil:
			b.WriteString(", entry present)")
		case errors.Is(p.err, keyring.ErrNotFound):
			b.WriteString(", no entry)")
		default:
			b.WriteString(", unavailable)")
		}
		b.WriteString("\n")
	}
	b.WriteString("source: ")
	if cfg == nil {
		b.WriteString("n/a (--url overrides the configured endpoint)")
	} else {
		b.WriteString(cfg.PasswordSource)
		if cfg.PasswordSource == "keyring" && cfg.KeyringAccount != "" {
			b.WriteString(" (")
			b.WriteString(cfg.KeyringAccount)
			b.WriteString(")")
		}
	}
	if backend != "" {
		b.WriteString("\nbackend: ")
		b.WriteString(backend)
	}
	b.WriteString("\nkeyring: ")
	outage := probeOutage(probes)
	switch {
	case outage != nil:
		b.WriteString("unavailable: ")
		b.WriteString(outage.Error())
		// An ErrBackend text already names its own fix (env var, ksecretd,
		// KWallet-native); adding the container hint would contradict it on
		// a desktop that simply has the wrong provider.
		if !errors.Is(outage, keyring.ErrBackend) {
			b.WriteString("\n  (no session bus in containers/ssh: use OPENSEARCH_PASSWORD)")
		}
	case probesAllMissing(probes):
		b.WriteString("no entry — store one: velruna creds set")
	default:
		b.WriteString("entry present (value not shown)")
	}
	b.WriteString("\n")
	if cfg != nil {
		b.WriteString("config file plaintext: ")
		switch {
		case !cfg.FileHasPlaintext:
			b.WriteString("absent\n")
		case cfg.AllowPlaintext:
			b.WriteString("present, in use (allow_plaintext on)\n")
		case probesBackendUnusable(probes):
			b.WriteString("present, REFUSED (allow_plaintext off): fix the keyring backend above, or export OPENSEARCH_PASSWORD\n")
		default:
			b.WriteString("present, REFUSED (allow_plaintext off): migrate with 'velruna creds set', then remove opensearch_password from the file\n")
		}
		if cfg.PasswordSource == "none" {
			// A refused backend already prints its own fix above; the generic
			// "velruna creds set" advice would point at a store that cannot be
			// written to.
			if probesBackendUnusable(probes) {
				b.WriteString("hint: fix the keyring backend listed above, or export OPENSEARCH_PASSWORD\n")
			} else {
				b.WriteString("hint: store with 'velruna creds set' (keyring) or export OPENSEARCH_PASSWORD (containers, headless)\n")
			}
		}
	}
	return b.String()
}

// probeOutage returns the first error that is not "entry not found": the
// provider itself is unusable (bus down, wallet closed), as opposed to the
// entries simply not existing.
func probeOutage(probes []accountProbe) error {
	for _, p := range probes {
		if p.err != nil && !errors.Is(p.err, keyring.ErrNotFound) {
			return p.err
		}
	}
	return nil
}

// probesBackendUnusable reports whether the outage is a provider verdict
// (ErrBackend): the fix belongs to the wallet, not to the store.
func probesBackendUnusable(probes []accountProbe) bool {
	return errors.Is(probeOutage(probes), keyring.ErrBackend)
}

// probesAllMissing reports that no entry was found, so the right advice is
// "store one". An outage is handled by the caller before this is asked.
func probesAllMissing(probes []accountProbe) bool {
	for _, p := range probes {
		if p.err == nil {
			return false
		}
	}
	return true
}

func credsRmCmd() *cobra.Command {
	var legacy bool
	var name string
	rm := &cobra.Command{
		Use:   "rm",
		Short: "Remove the keyring entry for the OpenSearch store",
		Long: `Removes the entry the current configuration resolves to: the cluster
account when opensearch_cluster names the store, otherwise the legacy
host[:port] one. --legacy also removes the legacy host[:port] entry left behind
by the migration to cluster naming. --name removes the entry for one explicit
store label (cluster/<name>) — the only way to clean up entries created with
'velruna creds set --name' against a store the current config no longer points
at.`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			out := cmd.OutOrStdout()
			cfg, target, err := credsLoadTarget()
			if err != nil {
				fatalErr(err)
			}
			var accounts []string
			if explicit := strings.TrimSpace(name); explicit != "" {
				accounts = append(accounts, config.ClusterAccount(explicit))
			} else if recorded := recordedClusterName(cfg); recorded != "" {
				accounts = append(accounts, config.ClusterAccount(recorded))
			}
			if len(accounts) == 0 || legacy {
				accounts = append(accounts, accountFor(target))
			}
			for _, account := range accounts {
				derr := keyring.Delete(config.KeyringService, account)
				switch {
				case derr == nil:
					if err := writeLine(out, "removed: service="+config.KeyringService+" account="+account); err != nil {
						fatalErr(err)
					}
				case errors.Is(derr, keyring.ErrNotFound):
					if err := writeLine(out, "no entry for service="+config.KeyringService+" account="+account); err != nil {
						fatalErr(err)
					}
				default:
					fatalErr(derr)
				}
			}
		},
	}
	rm.Flags().BoolVar(&legacy, "legacy", false,
		"also remove the legacy host[:port] entry")
	rm.Flags().StringVar(&name, "name", "",
		"remove the entry for this explicit store label (cluster/<name>) instead of the configured one")
	return rm
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
