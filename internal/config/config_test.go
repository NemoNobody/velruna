package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NemoNobody/velruna/internal/keyring"
)

// TestMain makes the keyring dispatcher safe for every test in this package:
// the default fake never touches the real wallet. A real provider call from a
// test binary can block on a desktop access prompt (a new app id asking the
// wallet for an existing secret) — the suite must never depend on the
// machine's keyring state. Keyring-specific tests install their own fakes
// via fakeKeyring.
func TestMain(m *testing.M) {
	get, set, del := keyring.Get, keyring.Set, keyring.Delete
	keyring.Get = func(string, string) (string, error) { return "", keyring.ErrNotFound }
	keyring.Set = func(string, string, string) error { return nil }
	keyring.Delete = func(string, string) error { return keyring.ErrNotFound }
	code := m.Run()
	keyring.Get, keyring.Set, keyring.Delete = get, set, del
	os.Exit(code)
}

// removeFile deletes path for test cleanup. A real removal failure fails the
// test (it can leave a stray velruna.conf next to the test binary and skew
// later tests); a file that is already gone is fine.
func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove %s: %v", path, err)
	}
}

// chdir switches the working directory and returns a cleanup function that
// restores it. Call the returned func at the end of each test (or via t.Cleanup).
func chdir(t *testing.T, dir string) func() {
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getcwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	return func() {
		if err := os.Chdir(orig); err != nil {
			t.Errorf("restore cwd: %v", err)
		}
	}
}

// fakeKeyring replaces the OS keyring calls: config tests must resolve
// credentials only from env/file, never from the developer's real wallet.
func fakeKeyring(t *testing.T, pw string, err error) {
	t.Helper()
	oldGet, oldSet, oldDel := keyring.Get, keyring.Set, keyring.Delete
	keyring.Get = func(service, user string) (string, error) { return pw, err }
	keyring.Set = func(service, user, value string) error { return nil }
	keyring.Delete = func(service, user string) error { return nil }
	t.Cleanup(func() { keyring.Get, keyring.Set, keyring.Delete = oldGet, oldSet, oldDel })
}

// clearEnv resets the config-related env vars to empty so tests are
// deterministic regardless of the surrounding environment, and isolates the
// keyring (the real service is never consulted from tests).
func clearEnv(t *testing.T) {
	fakeKeyring(t, "", keyring.ErrNotFound)
	for _, k := range []string{
		"VELRUNA_CONF",
		"OPENSEARCH_URL",
		"OPENSEARCH_USER",
		"OPENSEARCH_PASSWORD",
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
		"VELRUNA_DOCS_INDEX",
		"VELRUNA_DOCS_TTL_DAYS",
		"VELRUNA_FETCH_TIMEOUT",
		"VELRUNA_FETCH_MAX_BYTES",
		"VELRUNA_FETCH_BLOCK_CHARS",
		"VELRUNA_FETCH_DOC_CHARS",
		"VELRUNA_FETCH_ALLOW_PRIVATE",
		"VELRUNA_FETCH_HOSTS",
		"OPENSEARCH_INSECURE_SKIP_VERIFY",
		"VELRUNA_FETCH_USER_AGENT",
		"VELRUNA_FETCH_PROXY_URL",
		"VELRUNA_FETCH_PROXY_FALLBACK",
		"VELRUNA_FETCH_RESPECT_ROBOTS",
		"VELRUNA_FETCH_RENDER",
		"VELRUNA_FETCH_OBSCURA_PATH",
		"VELRUNA_FETCH_OBSCURA_STEALTH",
		"VELRUNA_LOCAL_REPO_ROOTS",
		"VELRUNA_REPO_SCAN_DEPTH",
	} {
		t.Setenv(k, "")
	}
}

func TestRedactURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://admin:secret@127.0.0.1:9200", "https://admin:xxxxx@127.0.0.1:9200"},
		{"https://127.0.0.1:9200", "https://127.0.0.1:9200"},
		{"https://admin@host", "https://admin@host"},
		{"::not a url::", "::not a url::"},
	}
	for _, c := range cases {
		if got := RedactURL(c.in); got != c.want {
			t.Errorf("RedactURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Credentials in opensearch_url leak into every error message and status
// print; the config refuses them at load time (the fetch path does the same
// for user URLs).
func TestLoadConfigRejectsURLUserInfo(t *testing.T) {
	t.Setenv("OPENSEARCH_URL", "https://admin:hunter2@127.0.0.1:9200")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("opensearch_url with user info must be rejected")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the rejection message echoes the password: %v", err)
	}
	if !strings.Contains(err.Error(), "admin:xxxxx") {
		t.Errorf("the rejection message must show a redacted url: %v", err)
	}

	// A credential url that fails url.Parse (broken percent-escape) must be
	// refused too, without echoing the raw text: url.Parse's own error quotes
	// the input, and RedactURL returns raw on parse failure.
	t.Setenv("OPENSEARCH_URL", "https://admin:sec%zzret@os.test:9200")
	_, err = LoadConfig()
	if err == nil {
		t.Fatal("unparseable opensearch_url with credentials must be rejected")
	}
	if strings.Contains(err.Error(), "sec%zzret") || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the rejection message echoes the credentials: %v", err)
	}

	// A plain (credential-free) unparseable url is a config error as well,
	// with the parse error quoted (nothing to hide).
	t.Setenv("OPENSEARCH_URL", "http://[::1")
	_, err = LoadConfig()
	if err == nil {
		t.Fatal("unparseable opensearch_url must be rejected")
	}
}

// FindConfigPathNoCWD must skip a ./velruna.conf: `velruna mcp` runs with
// the cwd of an arbitrary (possibly cloned, untrusted) project.
func TestFindConfigPathNoCWD(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "velruna.conf"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("VELRUNA_CONF", "")
	t.Chdir(dir)

	if got := FindConfigPath(); got != "velruna.conf" {
		t.Errorf("FindConfigPath() = %q, want the cwd entry", got)
	}
	if got := FindConfigPathNoCWD(); got != "" {
		t.Errorf("FindConfigPathNoCWD() = %q, want no cwd entry", got)
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.OpenSearchURL != "https://127.0.0.1:9200" {
		t.Errorf("OpenSearchURL = %q, want default", cfg.OpenSearchURL)
	}
	if cfg.OpenSearchUser != "admin" {
		t.Errorf("OpenSearchUser = %q, want admin", cfg.OpenSearchUser)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must be false by default (TLS is verified)")
	}
	if cfg.IndexName != "velruna" {
		t.Errorf("IndexName = %q, want velruna", cfg.IndexName)
	}
	if cfg.HistoryLimit != 20 {
		t.Errorf("HistoryLimit = %d, want default 20", cfg.HistoryLimit)
	}
	if cfg.HistoryIndexPeriod != PeriodMonth {
		t.Errorf("HistoryIndexPeriod = %q, want default %q", cfg.HistoryIndexPeriod, PeriodMonth)
	}
	if cfg.SecretGuard != GuardReject {
		t.Errorf("SecretGuard = %q, want the safe default %q", cfg.SecretGuard, GuardReject)
	}
	if cfg.FetchBlockChars != 8000 {
		t.Errorf("FetchBlockChars = %d, want default 8000", cfg.FetchBlockChars)
	}
	if cfg.FetchDocChars != 100_000 {
		t.Errorf("FetchDocChars = %d, want default 100000", cfg.FetchDocChars)
	}
}

func TestLoadConfig_NoFileNoEnv(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OpenSearchURL != "https://127.0.0.1:9200" {
		t.Errorf("OpenSearchURL = %q, want default (no file)", cfg.OpenSearchURL)
	}
	if cfg.IndexName != "velruna" {
		t.Errorf("IndexName = %q, want velruna (no file)", cfg.IndexName)
	}
	if cfg.HistoryLimit != 20 {
		t.Errorf("HistoryLimit = %d, want default 20 (no file)", cfg.HistoryLimit)
	}
}

func TestLoadConfig_File(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	fc := filepath.Join(dir, "velruna.conf")
	if err := os.WriteFile(fc, []byte(`{
  "opensearch_url": "https://custom:9200",
  "opensearch_user": "user1",
  "opensearch_password": "pass1",
  "index_name": "myindex"
}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OpenSearchURL != "https://custom:9200" {
		t.Errorf("OpenSearchURL = %q, want file value", cfg.OpenSearchURL)
	}
	if cfg.OpenSearchUser != "user1" {
		t.Errorf("OpenSearchUser = %q, want user1", cfg.OpenSearchUser)
	}
	if cfg.IndexName != "myindex" {
		t.Errorf("IndexName = %q, want myindex", cfg.IndexName)
	}
	// Regression: a file without insecure_skip_verify must keep the default
	// (verified TLS: verification is only ever off by explicit opt-in).
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = true, want default false when key omitted")
	}
}

func TestLoadConfig_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	fc := filepath.Join(dir, "velruna.conf")
	if err := os.WriteFile(fc, []byte(`{
  "opensearch_url": "https://file:9200",
  "index_name": "fileindex"
}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	t.Setenv("OPENSEARCH_URL", "https://env:9200")
	t.Setenv("VELRUNA_INDEX", "envindex")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OpenSearchURL != "https://env:9200" {
		t.Errorf("OpenSearchURL = %q, want env value (override)", cfg.OpenSearchURL)
	}
	if cfg.IndexName != "envindex" {
		t.Errorf("IndexName = %q, want env value (override)", cfg.IndexName)
	}
}

// TestLoadConfig_HistoryLimit pins the per-key history retention knob: an
// explicit file value overrides the default, an explicit 0 means "keep
// everything" and must survive decoding (the *int in fileConfig exists for
// exactly this distinction).
func TestLoadConfig_HistoryLimit(t *testing.T) {
	cases := []struct {
		name string
		file string
		env  string
		want int
	}{
		{"file override", `{"history_limit": 5}`, "", 5},
		{"file zero keeps everything", `{"history_limit": 0}`, "", 0},
		{"file negative disables retention", `{"history_limit": -1}`, "", -1},
		{"env overrides file", `{"history_limit": 5}`, "7", 7},
		{"env zero", `{}`, "0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			restore := chdir(t, dir)
			defer restore()
			clearEnv(t)
			fc := filepath.Join(dir, "velruna.conf")
			if err := os.WriteFile(fc, []byte(tc.file), 0644); err != nil {
				t.Fatalf("write velruna.conf: %v", err)
			}
			if tc.env != "" {
				t.Setenv("VELRUNA_HISTORY_LIMIT", tc.env)
			}
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.HistoryLimit != tc.want {
				t.Errorf("HistoryLimit = %d, want %d", cfg.HistoryLimit, tc.want)
			}
		})
	}
}

func TestLoadConfig_HistoryLimitBadEnvIsAnError(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	t.Setenv("VELRUNA_HISTORY_LIMIT", "not-a-number")
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for VELRUNA_HISTORY_LIMIT=not-a-number, want error")
	}
}

// TestLoadConfig_SecretGuard pins the guard rail: the default is the safe
// level, file and env set it, typos fail loudly (a silently ignored guard
// value is a guard that is not there).
func TestLoadConfig_SecretGuard(t *testing.T) {
	cases := []struct {
		name string
		file string
		env  string
		want string
	}{
		{"file ask", `{"secret_guard": "ask"}`, "", GuardAsk},
		{"file allow", `{"secret_guard": "allow"}`, "", GuardAllow},
		{"file canonicalizes", `{"secret_guard": "  ASK "}`, "", GuardAsk},
		{"env overrides file", `{"secret_guard": "allow"}`, "reject", GuardReject},
		{"env canonicalizes", `{}`, "Allow", GuardAllow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			restore := chdir(t, dir)
			defer restore()
			clearEnv(t)
			fc := filepath.Join(dir, "velruna.conf")
			if err := os.WriteFile(fc, []byte(tc.file), 0644); err != nil {
				t.Fatalf("write velruna.conf: %v", err)
			}
			if tc.env != "" {
				t.Setenv("VELRUNA_SECRET_GUARD", tc.env)
			}
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.SecretGuard != tc.want {
				t.Errorf("SecretGuard = %q, want %q", cfg.SecretGuard, tc.want)
			}
		})
	}
}

func TestLoadConfig_SecretGuardBadValuesAreErrors(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)

	fc := filepath.Join(dir, "velruna.conf")
	if err := os.WriteFile(fc, []byte(`{"secret_guard": "permissive"}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for secret_guard=permissive, want error")
	}

	if err := os.WriteFile(fc, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	t.Setenv("VELRUNA_SECRET_GUARD", "yes-please")
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for VELRUNA_SECRET_GUARD=yes-please, want error")
	}
}

func TestLoadConfig_HistoryIndexPeriod(t *testing.T) {
	cases := []struct {
		name string
		file string
		env  string
		want string
	}{
		{"file quarter", `{"history_index_period": "quarter"}`, "", PeriodQuarter},
		{"file canonicalizes", `{"history_index_period": "  Week "}`, "", PeriodWeek},
		{"env overrides file", `{"history_index_period": "year"}`, "day", PeriodDay},
		{"env canonicalizes", `{}`, "Quarter", PeriodQuarter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			restore := chdir(t, dir)
			defer restore()
			clearEnv(t)
			fc := filepath.Join(dir, "velruna.conf")
			if err := os.WriteFile(fc, []byte(tc.file), 0644); err != nil {
				t.Fatalf("write velruna.conf: %v", err)
			}
			if tc.env != "" {
				t.Setenv("VELRUNA_HISTORY_PERIOD", tc.env)
			}
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.HistoryIndexPeriod != tc.want {
				t.Errorf("HistoryIndexPeriod = %q, want %q", cfg.HistoryIndexPeriod, tc.want)
			}
		})
	}
}

func TestLoadConfig_HistoryIndexPeriodBadValuesAreErrors(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)

	fc := filepath.Join(dir, "velruna.conf")
	if err := os.WriteFile(fc, []byte(`{"history_index_period": "fortnight"}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for history_index_period=fortnight, want error")
	}

	if err := os.WriteFile(fc, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	t.Setenv("VELRUNA_HISTORY_PERIOD", "biweekly")
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for VELRUNA_HISTORY_PERIOD=biweekly, want error")
	}
}

// Author labels ship in every fact, so the config edge is where control
// bytes and runaway lengths must die: they would forge terminal output and
// bloat every later read.
func TestLoadConfig_AuthorLabels(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	fc := filepath.Join(dir, "velruna.conf")
	if err := os.WriteFile(fc, []byte(`{"author": " alice ", "model": "local-llm", "host": "devbox", "session": "s-42"}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Author != "alice" || cfg.Model != "local-llm" || cfg.Host != "devbox" || cfg.Session != "s-42" {
		t.Errorf("labels = %q/%q/%q/%q, want alice/local-llm/devbox/s-42", cfg.Author, cfg.Model, cfg.Host, cfg.Session)
	}

	t.Setenv("VELRUNA_AUTHOR", "bob")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Author != "bob" {
		t.Errorf("Author = %q, want bob (env must override the file)", cfg.Author)
	}
}

func TestLoadConfig_AuthorLabelsBadAreErrors(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	fc := filepath.Join(dir, "velruna.conf")

	long := strings.Repeat("x", 201)
	if err := os.WriteFile(fc, []byte(`{"author": "`+long+`"}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for a 201-rune author, want error")
	}

	if err := os.WriteFile(fc, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	t.Setenv("VELRUNA_MODEL", long)
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for a 201-rune VELRUNA_MODEL, want error")
	}

	// A blank-after-trim label must be loud, not silently "absent": it
	// would fall back to the OS user in the audit chain (file field).
	if err := os.WriteFile(fc, []byte(`{"author": "   "}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for a whitespace-only author, want error")
	}

	// Same bound on the env path: VELRUNA_AUTHOR=" " passes the non-empty
	// env guard and must not overwrite the file author with a blank label.
	if err := os.WriteFile(fc, []byte(`{"author": "alice"}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	t.Setenv("VELRUNA_AUTHOR", "  ")
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for VELRUNA_AUTHOR of only spaces, want error")
	}
}

func TestLoadConfig_SummaryLimits(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	fc := filepath.Join(dir, "velruna.conf")

	// Defaults exist: a fresh config has the table-of-contents contract.
	if err := os.WriteFile(fc, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.SummaryMinLen != 2000 || cfg.SummaryMaxLen != 400 {
		t.Errorf("summary defaults = %d/%d, want 2000/400", cfg.SummaryMinLen, cfg.SummaryMaxLen)
	}

	if err := os.WriteFile(fc, []byte(`{"summary_min_len": 500, "summary_max_len": 100}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.SummaryMinLen != 500 || cfg.SummaryMaxLen != 100 {
		t.Errorf("summary limits = %d/%d, want 500/100", cfg.SummaryMinLen, cfg.SummaryMaxLen)
	}

	t.Setenv("VELRUNA_SUMMARY_MIN_LEN", "0")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.SummaryMinLen != 0 {
		t.Errorf("SummaryMinLen = %d, want 0 (env override: requirement off)", cfg.SummaryMinLen)
	}

	t.Setenv("VELRUNA_SUMMARY_MAX_LEN", "not-a-number")
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for VELRUNA_SUMMARY_MAX_LEN=not-a-number, want error")
	}

	if err := os.WriteFile(fc, []byte(`{"summary_min_len": -1}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	t.Setenv("VELRUNA_SUMMARY_MAX_LEN", "")
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for summary_min_len=-1, want error")
	}

	// The env path carries the same bound as the file path: a negative
	// value silently disables the corresponding check (the checks are "> 0").
	if err := os.WriteFile(fc, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	t.Setenv("VELRUNA_SUMMARY_MIN_LEN", "")
	t.Setenv("VELRUNA_SUMMARY_MAX_LEN", "-5")
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for VELRUNA_SUMMARY_MAX_LEN=-5, want error")
	}
	t.Setenv("VELRUNA_SUMMARY_MAX_LEN", "")
	t.Setenv("VELRUNA_SUMMARY_MIN_LEN", "-5")
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for VELRUNA_SUMMARY_MIN_LEN=-5, want error")
	}
}

func TestLoadConfig_FetchBlocks(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	fc := filepath.Join(dir, "velruna.conf")

	// Defaults exist: 8000-rune blocks, whole docs up to 100k.
	if err := os.WriteFile(fc, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.FetchBlockChars != 8000 || cfg.FetchDocChars != 100_000 {
		t.Errorf("fetch block/doc defaults = %d/%d, want 8000/100000", cfg.FetchBlockChars, cfg.FetchDocChars)
	}

	// File values apply.
	if err := os.WriteFile(fc, []byte(`{"fetch_block_chars": 4000, "fetch_doc_chars": 50000}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.FetchBlockChars != 4000 || cfg.FetchDocChars != 50000 {
		t.Errorf("file fetch block/doc = %d/%d, want 4000/50000", cfg.FetchBlockChars, cfg.FetchDocChars)
	}

	// Env overrides both keys; VELRUNA_FETCH_DOC_CHARS=0 is the documented
	// "store the whole document" value and must survive the override.
	t.Setenv("VELRUNA_FETCH_BLOCK_CHARS", "6000")
	t.Setenv("VELRUNA_FETCH_DOC_CHARS", "0")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.FetchBlockChars != 6000 || cfg.FetchDocChars != 0 {
		t.Errorf("env fetch block/doc = %d/%d, want 6000/0", cfg.FetchBlockChars, cfg.FetchDocChars)
	}

	t.Setenv("VELRUNA_FETCH_DOC_CHARS", "not-a-number")
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for VELRUNA_FETCH_DOC_CHARS=not-a-number, want error")
	}
	t.Setenv("VELRUNA_FETCH_DOC_CHARS", "")
	t.Setenv("VELRUNA_FETCH_BLOCK_CHARS", "not-a-number")
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for VELRUNA_FETCH_BLOCK_CHARS=not-a-number, want error")
	}
	t.Setenv("VELRUNA_FETCH_BLOCK_CHARS", "")

	// Bounds. Block size is mandatory and clamped to 1000..64000 in both
	// directions; doc chars accepts 0 but not negatives.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"fetch_block_chars below min", `{"fetch_block_chars": 999}`},
		{"fetch_block_chars above max", `{"fetch_block_chars": 64001}`},
		{"fetch_block_chars zero", `{"fetch_block_chars": 0}`},
		{"fetch_doc_chars negative", `{"fetch_doc_chars": -1}`},
	} {
		if err := os.WriteFile(fc, []byte(tc.body), 0644); err != nil {
			t.Fatalf("write velruna.conf: %v", err)
		}
		if _, err := LoadConfig(); err == nil {
			t.Errorf("LoadConfig() = nil error for %s, want error", tc.name)
		}
	}

	// The boundary messages name the key and the accepted range: an operator
	// reads them from a startup log.
	if err := os.WriteFile(fc, []byte(`{"fetch_block_chars": 999}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "fetch_block_chars must be between 1000 and 64000") {
		t.Errorf("LoadConfig() error = %v, want the fetch_block_chars range message", err)
	}
	if err := os.WriteFile(fc, []byte(`{"fetch_doc_chars": -1}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "fetch_doc_chars must be 0 or greater") {
		t.Errorf("LoadConfig() error = %v, want the fetch_doc_chars bound message", err)
	}

	// Accepted boundaries, inclusive.
	for _, tc := range []struct {
		name  string
		body  string
		block int
		doc   int
	}{
		{"fetch_block_chars min", `{"fetch_block_chars": 1000}`, 1000, 100_000},
		{"fetch_block_chars max", `{"fetch_block_chars": 64000}`, 64000, 100_000},
		{"fetch_doc_chars zero", `{"fetch_doc_chars": 0}`, 8000, 0},
	} {
		if err := os.WriteFile(fc, []byte(tc.body), 0644); err != nil {
			t.Fatalf("write velruna.conf: %v", err)
		}
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig(%s): %v", tc.name, err)
		}
		if cfg.FetchBlockChars != tc.block || cfg.FetchDocChars != tc.doc {
			t.Errorf("%s: block/doc = %d/%d, want %d/%d", tc.name, cfg.FetchBlockChars, cfg.FetchDocChars, tc.block, tc.doc)
		}
	}
}

func TestLoadConfig_InsecureSkipVerifyExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	fc := filepath.Join(dir, "velruna.conf")
	if err := os.WriteFile(fc, []byte(`{"insecure_skip_verify": false}`), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = true, want explicit false from file")
	}
}

func TestLoadConfig_MalformedFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	fc := filepath.Join(dir, "velruna.conf")
	if err := os.WriteFile(fc, []byte(`{"opensearch_url": `), 0644); err != nil {
		t.Fatalf("write velruna.conf: %v", err)
	}
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for malformed JSON, want error")
	}
}

func TestLoadConfig_MissingExplicitPathIsAnError(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	t.Setenv("VELRUNA_CONF", filepath.Join(dir, "does-not-exist.conf"))
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for missing VELRUNA_CONF path, want error")
	}
}

func TestFindConfigPath(t *testing.T) {
	// case 1: VELRUNA_CONF wins.
	t.Setenv("VELRUNA_CONF", "/explicit/path/conf.json")
	if p := FindConfigPath(); p != "/explicit/path/conf.json" {
		t.Errorf("VELRUNA_CONF path = %q, want explicit", p)
	}

	// case 2: cwd velruna.conf wins.
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	cwdFile := filepath.Join(dir, "velruna.conf")
	if err := os.WriteFile(cwdFile, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write cwd velruna.conf: %v", err)
	}
	if p := FindConfigPath(); p != "velruna.conf" {
		t.Errorf("cwd path = %q, want velruna.conf", p)
	}

	// case 3: executable-dir velruna.conf wins (no cwd file).
	removeFile(t, cwdFile)
	execPath, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	execConf := filepath.Join(filepath.Dir(execPath), "velruna.conf")
	if err := os.WriteFile(execConf, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write exec velruna.conf: %v", err)
	}
	t.Cleanup(func() { removeFile(t, execConf) })
	if p := FindConfigPath(); p != execConf {
		t.Errorf("exec path = %q, want %s", p, execConf)
	}

	// case 4: home .velruna.conf wins (no cwd or exec file).
	removeFile(t, execConf)
	homeDir := t.TempDir()
	setHome(t, homeDir)
	homeFile := filepath.Join(homeDir, ".velruna.conf")
	if err := os.WriteFile(homeFile, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write home .velruna.conf: %v", err)
	}
	t.Cleanup(func() { removeFile(t, homeFile) })
	if p := FindConfigPath(); p != homeFile {
		t.Errorf("home path = %q, want %s", p, homeFile)
	}

	// case 5: nothing found.
	removeFile(t, homeFile)
	setHome(t, t.TempDir())
	if p := FindConfigPath(); p != "" {
		t.Errorf("no-file path = %q, want empty", p)
	}
}

// TestNormalizeRender: typos in fetch_render are refused loudly — silently
// ignoring "obscure" would run plain fetches while the operator believes
// pages are JS-rendered.
func TestNormalizeRender(t *testing.T) {
	for _, ok := range []string{"off", "obscura", " Off ", "OBSCURA"} {
		v, err := normalizeRender(ok)
		if err != nil || (v != RenderOff && v != RenderObscura) {
			t.Errorf("normalizeRender(%q) = %q, %v", ok, v, err)
		}
	}
	for _, bad := range []string{"obscure", "js", "browser", ""} {
		if _, err := normalizeRender(bad); err == nil {
			t.Errorf("normalizeRender(%q) must be refused", bad)
		}
	}
}

// setHome points the user home at dir on every platform: os.UserHomeDir reads
// USERPROFILE on Windows and $HOME elsewhere. Setting only HOME passes on
// Linux and leaves the Windows CI job with an empty home (the job exists to
// catch exactly this class of platform assumption).
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

func TestEffectiveURLs(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.EffectiveURLs(); len(got) != 1 || got[0] != cfg.OpenSearchURL {
		t.Errorf("default EffectiveURLs = %v, want just opensearch_url", got)
	}
	cfg.OpenSearchURLs = []string{"https://a:9200", "https://b:9200"}
	if got := cfg.EffectiveURLs(); len(got) != 2 || got[1] != "https://b:9200" {
		t.Errorf("EffectiveURLs = %v, want the explicit list", got)
	}
}

func TestSplitURLList(t *testing.T) {
	got := splitURLList(" https://a:9200 , https://b:9200,\nhttps://c:9200 ")
	want := []string{"https://a:9200", "https://b:9200", "https://c:9200"}
	if len(got) != len(want) {
		t.Fatalf("splitURLList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitURLList[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// Every node address gets the same credential rules: one bad entry in the list
// must fail the load, not surface as a confusing dial error at request time.
func TestValidateStoreURLRejectsCredentials(t *testing.T) {
	for _, raw := range []string{"https://user:pw@os:9200", "https://", "://nope", "os:9200"} {
		if err := validateStoreURL("opensearch_urls[0]", raw); err == nil {
			t.Errorf("validateStoreURL(%q) accepted an unusable address", raw)
		}
	}
	for _, raw := range []string{"https://os:9200", "http://127.0.0.1:9200"} {
		if err := validateStoreURL("opensearch_url", raw); err != nil {
			t.Errorf("validateStoreURL(%q) = %v, want nil", raw, err)
		}
	}
}
