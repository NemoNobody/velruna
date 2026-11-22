package cmd

import (
	"context"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/version"
	"github.com/spf13/cobra"
)

// statusEnvVars are the environment variables that override the config file.
// Every env override config.go reads is listed (names only, never values:
// a proxy URL can embed credentials). Kept in sync with the env parsing in
// LoadConfigFrom.
var statusEnvVars = []string{
	"OPENSEARCH_URL",
	"OPENSEARCH_URLS",
	"OPENSEARCH_USER",
	"OPENSEARCH_PASSWORD",
	"OPENSEARCH_INSECURE_SKIP_VERIFY",
	"VELRUNA_INDEX",
	"VELRUNA_HISTORY_LIMIT",
	"VELRUNA_HISTORY_PERIOD",
	"VELRUNA_SUMMARY_MIN_LEN",
	"VELRUNA_SUMMARY_MAX_LEN",
	"VELRUNA_AUTHOR",
	"VELRUNA_MODEL",
	"VELRUNA_HOST",
	"VELRUNA_SESSION",
	"VELRUNA_SECRET_GUARD",
	"VELRUNA_ALLOW_PLAINTEXT",
	"VELRUNA_CONF",
	"VELRUNA_DOCS_INDEX",
	"VELRUNA_DOCS_TTL_DAYS",
	"VELRUNA_FETCH_TIMEOUT",
	"VELRUNA_FETCH_MAX_BYTES",
	"VELRUNA_FETCH_BLOCK_CHARS",
	"VELRUNA_FETCH_DOC_CHARS",
	"VELRUNA_FETCH_HOSTS",
	"VELRUNA_FETCH_USER_AGENT",
	"VELRUNA_FETCH_PROXY_URL",
	"VELRUNA_FETCH_PROXY_FALLBACK",
	"VELRUNA_FETCH_RESPECT_ROBOTS",
	"VELRUNA_FETCH_ALLOW_PRIVATE",
	"VELRUNA_FETCH_RENDER",
	"VELRUNA_FETCH_OBSCURA_PATH",
	"VELRUNA_FETCH_OBSCURA_STEALTH",
	"VELRUNA_LOCAL_REPO_ROOTS",
	"VELRUNA_REPO_SCAN_DEPTH",
}

// permWarning flags a plaintext password living in a file that group/others
// can read. The store is plaintext by design; the file must not be.
func permWarning(cfg *config.Config, configFile string) string {
	if !cfg.FileHasPlaintext || cfg.FilePerm&0o077 == 0 || configFile == "" {
		return ""
	}
	return "warning: the config file is readable by group/others (mode " +
		strconv.FormatInt(int64(cfg.FilePerm.Perm()), 8) +
		"): run chmod 600 " + configFile
}

// processStart is when this binary process booted: velruna_status carries it
// (RFC3339) so an agent can tell a long-lived MCP server from one that
// restarted after an update — the "is the tool I'm talking to the build I
// just installed?" question.
var processStart = time.Now().UTC()

// statusReport is the shared diagnostics payload behind `velruna status` and the
// velruna_status MCP tool. It never carries an error: it describes the situation
// so both a human and an agent can act on it.
type statusReport struct {
	OK                 bool                 `json:"ok" jsonschema:"true when OpenSearch is reachable and the store is usable"`
	MCPVersion         string               `json:"mcp_version" jsonschema:"velruna MCP server version (dev for source builds; the git tag for releases) — NOT the OpenSearch version"`
	Commit             string               `json:"commit,omitempty" jsonschema:"git commit this binary was stamped from (release/dev builds); empty for plain source builds"`
	StartedAt          string               `json:"started_at" jsonschema:"RFC3339 when this MCP server process started (a restart after an update moves it)"`
	URL                string               `json:"url" jsonschema:"the OpenSearch URL that was tried"`
	URLs               []string             `json:"urls,omitempty" jsonschema:"every node address the client dials (a cluster may list several); absent when only one is configured"`
	Index              string               `json:"index" jsonschema:"the index name used for facts"`
	HistoryIndexPeriod string               `json:"history_index_period" jsonschema:"date granularity of the journal indices: day | week | month | quarter | year"`
	HistoryLimit       int                  `json:"history_limit" jsonschema:"default keep for 'prune' (the journal itself is write-forever); 0 demands an explicit keep"`
	SecretGuard        string               `json:"secret_guard,omitempty" jsonschema:"secret guard level in effect on writes: reject | ask | allow"`
	Author             string               `json:"author,omitempty" jsonschema:"effective author label stamped on new facts (self-reported: security | config | os chain); empty when nothing resolves"`
	AuthorSource       string               `json:"author_source,omitempty" jsonschema:"which chain link named the author: security | config | os | none"`
	PasswordFrom       string               `json:"password_from,omitempty" jsonschema:"where the OpenSearch password resolves from: env | keyring | file | none; the value is never shown"`
	PermWarning        string               `json:"perm_warning,omitempty" jsonschema:"plaintext password in a group/world-readable config file: the user should chmod 600 it"`
	KeyringError       string               `json:"keyring_error,omitempty" jsonschema:"keyring provider failure (not 'entry not found'): a bus/wallet outage, not a missing entry"`
	ConfigFile         string               `json:"config_file" jsonschema:"config file actually used, empty when none was found"`
	EnvOverrides       []string             `json:"env_overrides,omitempty" jsonschema:"environment variables that override the config file"`
	ClusterName        string               `json:"cluster_name,omitempty" jsonschema:"OpenSearch cluster name, when reachable"`
	OpenSearchVersion  string               `json:"opensearch_version,omitempty" jsonschema:"OpenSearch version, when reachable"`
	Storage            *client.StorageState `json:"storage,omitempty" jsonschema:"cluster health, disk usage and read-only state; absent when the store is unreachable"`
	StorageNote        string               `json:"storage_note,omitempty" jsonschema:"what the health status means for this index, in words"`
	Fetch              *fetchReport         `json:"fetch,omitempty" jsonschema:"document-fetch configuration: TTL, renderer, proxy presence (never the URL), allowlist size — the \"why does fetch fail\" answer"`
	Error              string               `json:"error,omitempty" jsonschema:"why the store is not usable"`
	Hint               string               `json:"hint,omitempty" jsonschema:"what the user should do about it"`
}

// fetchReport is the effective document-fetch configuration. The proxy URL is
// reported as a boolean on purpose: it may carry credentials in userinfo, and
// status output is agent-visible.
type fetchReport struct {
	DocsIndex       string `json:"docs_index" jsonschema:"index holding fetched page snapshots"`
	DocsTTLDays     int    `json:"docs_ttl_days" jsonschema:"snapshot TTL in days; 0 = never expires"`
	Render          string `json:"render,omitempty" jsonschema:"JS renderer for walled pages (empty = plain fetch only; obscura = opt-in renderer)"`
	ProxyConfigured bool   `json:"proxy_configured" jsonschema:"true when a fetch proxy is configured (the URL is never shown)"`
	ProxyFallback   bool   `json:"proxy_fallback" jsonschema:"true when a refused direct fetch retries through the proxy"`
	AllowPrivate    bool   `json:"allow_private" jsonschema:"true when the SSRF guard is relaxed for self-hosted targets"`
	RespectRobots   bool   `json:"respect_robots" jsonschema:"true when robots.txt is honored"`
	AllowHosts      int    `json:"allow_hosts" jsonschema:"number of hosts in the privacy allowlist (0 = no allowlist)"`
	TimeoutSeconds  int    `json:"timeout_seconds" jsonschema:"per-fetch timeout"`
	MaxBytes        int    `json:"max_bytes" jsonschema:"body size cap per fetch"`
	BlockChars      int    `json:"block_chars" jsonschema:"target size of one stored block, in runes (a longer section is split, never truncated)"`
	DocChars        int    `json:"doc_chars" jsonschema:"stored-text cap per document, in runes; 0 = the whole document"`
	UserAgent       string `json:"user_agent,omitempty" jsonschema:"User-Agent sent on fetches (not a secret: every target sees it)"`
}

// resolvedConfigPath returns the config file that would be used, given an
// optional explicit path. noCWD matches the caller's lookup chain: the MCP
// server ignores the working directory (see FindConfigPathNoCWD).
func resolvedConfigPath(explicit string, noCWD bool) string {
	if explicit != "" {
		return explicit
	}
	if noCWD {
		return config.FindConfigPathNoCWD()
	}
	return config.FindConfigPath()
}

// setEnvOverrides records which environment variables are in effect, so
// diagnostics do not blame the config file for a value that came from the
// environment.
func setEnvOverrides(rep *statusReport) {
	for _, name := range statusEnvVars {
		if os.Getenv(name) != "" {
			rep.EnvOverrides = append(rep.EnvOverrides, name)
		}
	}
}

// envNote returns a human-readable suffix naming active env overrides.
func envNote(rep statusReport) string {
	if len(rep.EnvOverrides) == 0 {
		return ""
	}
	return " Note: " + listNote(rep.EnvOverrides) + " are set in the environment and override the file."
}

// listNote renders a list of names the way fmt's %v does, without the
// reflection cost.
func listNote(names []string) string {
	return "[" + strings.Join(names, " ") + "]"
}

// collectStatus gathers connectivity and configuration diagnostics. It never
// returns an error: an unavailable store is reported, not raised. noCWD must
// match the caller's config chain: when the MCP server reports the CWD chain
// while its store tools use the NoCWD chain, the report is not the effective
// configuration — and with a password in the environment the status request
// sends the credentials to a cloned repo's hijack endpoint.
func collectStatus(ctx context.Context, confPath string, noCWD bool) statusReport {
	rep := statusReport{
		ConfigFile: resolvedConfigPath(confPath, noCWD),
		MCPVersion: version.Version,
		Commit:     version.Commit,
		StartedAt:  processStart.Format(time.RFC3339),
	}
	setEnvOverrides(&rep)

	var cfg *config.Config
	var err error
	if confPath == "" && noCWD {
		cfg, err = config.LoadConfigNoCWD()
	} else {
		cfg, err = config.LoadConfigFrom(confPath)
	}
	if err != nil {
		rep.Error = err.Error()
		rep.Hint = "Fix the config file (or point --conf / VELRUNA_CONF at a valid one). See docs/setup.md."
		return rep
	}
	urls := cfg.EffectiveURLs()
	rep.URL = config.RedactURL(urls[0])
	if len(urls) > 1 {
		rep.URLs = make([]string, 0, len(urls))
		for _, u := range urls {
			rep.URLs = append(rep.URLs, config.RedactURL(u))
		}
	}
	rep.Index = cfg.IndexName
	rep.HistoryIndexPeriod = cfg.HistoryIndexPeriod
	rep.HistoryLimit = cfg.HistoryLimit
	rep.SecretGuard = cfg.SecretGuard
	rep.Fetch = &fetchReport{
		DocsIndex:       cfg.DocsIndexName,
		DocsTTLDays:     cfg.DocsTTLDays,
		Render:          cfg.FetchRender,
		ProxyConfigured: cfg.FetchProxyURL != "",
		ProxyFallback:   cfg.FetchProxyFallback,
		AllowPrivate:    cfg.FetchAllowPrivate,
		RespectRobots:   cfg.FetchRespectRobots,
		AllowHosts:      len(cfg.FetchAllowHosts),
		TimeoutSeconds:  cfg.FetchTimeoutSeconds,
		MaxBytes:        cfg.FetchMaxBytes,
		BlockChars:      cfg.FetchBlockChars,
		DocChars:        cfg.FetchDocChars,
		UserAgent:       cfg.FetchUserAgent,
	}
	rep.PasswordFrom = cfg.PasswordSource
	rep.KeyringError = cfg.KeyringError
	rep.PermWarning = permWarning(cfg, rep.ConfigFile)
	rep.Author, rep.AuthorSource = cfg.Author, "config"
	if cfg.Author == "" {
		if u, err := user.Current(); err == nil && u.Username != "" {
			// Same bound as the client's chain: a hostile-length username must
			// not reach the terminal unfiltered.
			if s, nErr := config.NormalizeLabel(u.Username); nErr == nil && s != "" {
				rep.Author, rep.AuthorSource = s, "os"
			}
		}
	}

	if cfg.OpenSearchPassword == "" {
		// The config is fine, the credential is missing: say where it should
		// come from instead of sending a guaranteed 401.
		rep.Error = "no OpenSearch password (env, keyring and file sources all empty or refused)"
		var b strings.Builder
		b.WriteString("Store it in the OS keyring with 'velruna creds set'.")
		if cfg.FileHasPlaintext {
			b.WriteString(" The config file carries a plaintext password and allow_plaintext is off: migrate, then delete opensearch_password from the file (or set allow_plaintext=true).")
		}
		b.WriteString(" In containers/headless, export OPENSEARCH_PASSWORD (for the Zed MCP server: set it in the server's env).")
		rep.Hint = b.String()
		return rep
	}

	c, err := client.NewClient(cfg)
	if err != nil {
		rep.Error = err.Error()
		rep.Hint = "The OpenSearch client could not be created; check opensearch_url. See docs/setup.md."
		return rep
	}

	info, err := c.Info(ctx)
	if err != nil {
		rep.Error = err.Error()
		var b strings.Builder
		b.WriteString("OpenSearch is not reachable at ")
		b.WriteString(config.RedactURL(cfg.OpenSearchURL))
		b.WriteString(". Check that the server is running and that opensearch_url / " +
			"opensearch_user / opensearch_password are correct (config file: ")
		b.WriteString(configPathOrNone(rep.ConfigFile))
		b.WriteString(").")
		b.WriteString(envNote(rep))
		b.WriteString(" If OpenSearch is not set up yet, see docs/setup.md — a single node is " +
			"enough to start, and nothing in velruna assumes one or the other. " +
			"Report this to the user; do not retry in a loop.")
		rep.Hint = b.String()
		return rep
	}

	rep.OK = true
	rep.ClusterName = info.ClusterName
	rep.OpenSearchVersion = info.Version.Number
	rep.Storage = c.StorageState(ctx)
	rep.StorageNote = storageNote(rep.Storage)
	// With a live client the security probe can resolve the store user: the
	// effective author is the client's chain answer, not the config guess.
	labels := c.AuthorLabels(ctx)
	rep.Author, rep.AuthorSource = labels.Author, labels.AuthorSource
	return rep
}

// storageNote explains the index health in words. It states what is true without
// assuming a cluster shape: whether "yellow" matters depends on the deployment,
// and that judgement belongs to the reader.
func storageNote(st *client.StorageState) string {
	if st == nil {
		return ""
	}
	if st.IndexAbsent {
		return "index does not exist yet: it is created on the first write"
	}
	if st.IndexHealth == "" {
		return ""
	}
	switch st.IndexHealth {
	case "red":
		return "some primary shards are unassigned: parts of the index are unavailable"
	case "yellow":
		return "all primary shards are active; some replica shards are unassigned, so redundancy is reduced"
	case "green":
		return "all shards are active"
	}
	return ""
}

// configPathOrNone makes an empty path readable in messages.
func configPathOrNone(path string) string {
	if path == "" {
		return "none found"
	}
	return path
}

var statusFlags struct {
	conf string
}

// StatusCmd returns the `velruna status` command: the same diagnostics as the
// velruna_status MCP tool, for use from a shell.
func StatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Check that OpenSearch is reachable and show the effective configuration",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			rep := collectStatus(context.Background(), statusFlags.conf, false)
			if err := printStatus(os.Stdout, rep); err != nil {
				fatalErr(err)
			}
			if !rep.OK {
				os.Exit(1)
			}
		},
	}
	cmd.Flags().StringVar(&statusFlags.conf, "conf", "",
		"config file to use (overrides VELRUNA_CONF and the default lookup)")
	return cmd
}

// printStatus renders a report for humans on w.
func printStatus(w io.Writer, rep statusReport) error {
	var b strings.Builder
	b.WriteString("config file: ")
	b.WriteString(configPathOrNone(rep.ConfigFile))
	b.WriteString("\n")
	if len(rep.EnvOverrides) > 0 {
		b.WriteString("env overrides: ")
		b.WriteString(listNote(rep.EnvOverrides))
		b.WriteString("\n")
	}
	if rep.URL == "" {
		b.WriteString("status: NOT AVAILABLE\n  error: ")
		b.WriteString(rep.Error)
		b.WriteString("\n  hint: ")
		b.WriteString(rep.Hint)
		b.WriteString("\n")
		_, err := io.WriteString(w, b.String())
		return err
	}
	b.WriteString("velruna ")
	b.WriteString(rep.MCPVersion)
	if rep.Commit != "" {
		b.WriteString(" (")
		b.WriteString(rep.Commit)
		b.WriteString(")")
	}
	b.WriteString(", started ")
	b.WriteString(rep.StartedAt)
	b.WriteString("\nurl: ")
	b.WriteString(rep.URL)
	if len(rep.URLs) > 1 {
		b.WriteString("\nurls: ")
		b.WriteString(strings.Join(rep.URLs, ", "))
	}
	b.WriteString("\nindex: ")
	b.WriteString(rep.Index)
	b.WriteString("\nhistory_index_period: ")
	b.WriteString(rep.HistoryIndexPeriod)
	b.WriteString("\nhistory_limit: ")
	b.WriteString(strconv.Itoa(rep.HistoryLimit))
	if f := rep.Fetch; f != nil {
		b.WriteString("\ndocs: index=")
		b.WriteString(f.DocsIndex)
		b.WriteString(" ttl=")
		if f.DocsTTLDays <= 0 {
			b.WriteString("never")
		} else {
			b.WriteString(strconv.Itoa(f.DocsTTLDays))
			b.WriteString("d")
		}
		b.WriteString(" render=")
		if f.Render == "" {
			b.WriteString("off")
		} else {
			b.WriteString(f.Render)
		}
		b.WriteString(" proxy=")
		if f.ProxyConfigured {
			b.WriteString("configured")
			if f.ProxyFallback {
				b.WriteString("(fallback)")
			}
		} else {
			b.WriteString("off")
		}
		b.WriteString(" allow_hosts=")
		b.WriteString(strconv.Itoa(f.AllowHosts))
		if f.AllowPrivate {
			b.WriteString(" allow_private=on")
		}
		if f.RespectRobots {
			b.WriteString(" robots=on")
		}
		b.WriteString(" timeout=")
		b.WriteString(strconv.Itoa(f.TimeoutSeconds))
		b.WriteString("s max=")
		b.WriteString(humanBytes(int64(f.MaxBytes)))
		b.WriteString(" block=")
		b.WriteString(strconv.Itoa(f.BlockChars))
		b.WriteString(" doc=")
		b.WriteString(strconv.Itoa(f.DocChars))
	}
	b.WriteString("\nsecret_guard: ")
	b.WriteString(rep.SecretGuard)
	if rep.Author != "" {
		// Quoted: the label is self-reported (server/OS user) and can contain
		// spaces; strconv.Quote also escapes any control byte that survived.
		b.WriteString("\nauthor: ")
		b.WriteString(strconv.Quote(rep.Author))
		b.WriteString(" (")
		b.WriteString(rep.AuthorSource)
		b.WriteString(")")
	}
	b.WriteString("\npassword: ")
	b.WriteString(rep.PasswordFrom)
	if rep.KeyringError != "" {
		b.WriteString(" (keyring outage: ")
		b.WriteString(rep.KeyringError)
		b.WriteString(")")
	}
	b.WriteString("\n")
	if rep.PermWarning != "" {
		b.WriteString(rep.PermWarning)
		b.WriteString("\n")
	}
	if rep.OK {
		b.WriteString("cluster: ")
		b.WriteString(rep.ClusterName)
		b.WriteString(" (OpenSearch ")
		b.WriteString(rep.OpenSearchVersion)
		b.WriteString(")\n")
		if _, err := io.WriteString(w, b.String()); err != nil {
			return err
		}
		if err := printStorage(w, rep); err != nil {
			return err
		}
		return writeLine(w, "status: OK")
	}
	b.WriteString("status: NOT AVAILABLE\n  error: ")
	b.WriteString(rep.Error)
	b.WriteString("\n  hint: ")
	b.WriteString(rep.Hint)
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// printStorage renders the storage state. Facts only — no verdicts about whether
// a given cluster shape is expected.
func printStorage(w io.Writer, rep statusReport) error {
	st := rep.Storage
	if st == nil {
		return nil
	}
	var b strings.Builder
	b.WriteString("storage: cluster=")
	b.WriteString(st.ClusterHealth)
	b.WriteString(" index=")
	if st.IndexAbsent {
		b.WriteString("absent")
	} else {
		b.WriteString(st.IndexHealth)
	}
	b.WriteString(" unassigned_shards=")
	b.WriteString(strconv.Itoa(st.Unassigned))
	b.WriteString(" data_nodes=")
	b.WriteString(strconv.Itoa(st.DataNodes))
	b.WriteString("\n")
	if rep.StorageNote != "" {
		b.WriteString("  ")
		b.WriteString(rep.StorageNote)
		b.WriteString("\n")
	}
	if st.WorstNode != "" {
		b.WriteString("disk: busiest data node ")
		b.WriteString(st.WorstNode)
		b.WriteString(" at ")
		b.WriteString(strconv.FormatFloat(st.WorstNodeUsedPercent, 'f', 0, 64))
		b.WriteString("% used\n")
	}
	if st.DesiredReplicas != nil && st.ActualReplicas != nil {
		b.WriteString("replicas: facts index has ")
		b.WriteString(strconv.Itoa(*st.ActualReplicas))
		b.WriteString(", topology asks for ")
		b.WriteString(strconv.Itoa(*st.DesiredReplicas))
		if st.ReplicaDrift {
			b.WriteString(" (drift: the replica reconcile did not converge)")
		}
		b.WriteString("\n")
	}
	if len(st.Indices) > 0 {
		b.WriteString("indices:\n")
		for _, idx := range st.Indices {
			b.WriteString("  ")
			b.WriteString(idx.Index)
			b.WriteString("  ")
			b.WriteString(idx.Health)
			b.WriteString("  docs=")
			b.WriteString(strconv.Itoa(idx.Documents))
			b.WriteString("  size=")
			b.WriteString(humanBytes(idx.SizeBytes))
			b.WriteString("  shards=")
			b.WriteString(strconv.Itoa(idx.Primaries))
			b.WriteString("/")
			b.WriteString(strconv.Itoa(idx.Replicas))
			b.WriteString("\n")
		}
		if st.IndicesTruncated {
			b.WriteString("  (more indices exist than the reported page)\n")
		}
	}
	if st.ReadOnlyIndex {
		b.WriteString("warning: the index has a read-only block — writes are rejected until it is cleared\n")
	}
	if st.Unavailable != "" {
		b.WriteString("storage details unavailable: ")
		b.WriteString(st.Unavailable)
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// humanBytes renders a byte count with one decimal in binary units. Storage
// sizes are read by humans deciding whether to act, not by machines.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + "B"
	}
	d := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		d /= unit
		if d < unit {
			return strconv.FormatFloat(d, 'f', 1, 64) + suffix
		}
	}
	return strconv.FormatFloat(d, 'f', 1, 64) + "PiB"
}
