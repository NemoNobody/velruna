package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/NemoNobody/velruna/internal/keyring"
)

// writePlainConf writes a config file with a plaintext password.
func writePlainConf(t *testing.T, dir string, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "velruna.conf"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPasswordPrecedence(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()

	fileBody := `{"opensearch_url": "https://os.internal:9200", "opensearch_password": "filepw"}`

	cases := []struct {
		name       string
		keyringPw  string
		keyringErr error
		envPw      string
		plaintext  bool
		wantPw     string
		wantSource string
	}{
		{"env wins over keyring and file", "keyringpw", nil, "envpw", true, "envpw", "env"},
		{"keyring wins over file", "keyringpw", nil, "", false, "keyringpw", "keyring"},
		{"file only when allow_plaintext", "", keyring.ErrNotFound, "", true, "filepw", "file"},
		{"file refused by default", "", keyring.ErrNotFound, "", false, "", "none"},
		{"keyring outage falls back to file", "", errors.New("no session bus"), "", true, "filepw", "file"},
		{"keyring outage, no plaintext: none", "", errors.New("no session bus"), "", false, "", "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			if err := os.WriteFile(filepath.Join(dir, "velruna.conf"), []byte(fileBody), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { removeFile(t, filepath.Join(dir, "velruna.conf")) })
			fakeKeyring(t, tc.keyringPw, tc.keyringErr)
			if tc.envPw != "" {
				t.Setenv("OPENSEARCH_PASSWORD", tc.envPw)
			}
			if tc.plaintext {
				t.Setenv("VELRUNA_ALLOW_PLAINTEXT", "true")
			}
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.OpenSearchPassword != tc.wantPw {
				t.Errorf("password = %q, want %q", cfg.OpenSearchPassword, tc.wantPw)
			}
			if cfg.PasswordSource != tc.wantSource {
				t.Errorf("source = %q, want %q", cfg.PasswordSource, tc.wantSource)
			}
		})
	}
}

func TestKeyringServiceAndAccount(t *testing.T) {
	if KeyringService != "velruna" {
		t.Errorf("service = %q, want velruna", KeyringService)
	}
	cases := map[string]string{
		"https://127.0.0.1:9200":              "127.0.0.1:9200",
		"https://os.internal":                 "os.internal",
		"http://search.lan:8080/x":            "search.lan:8080",
		"not a url":                           "not a url",
		"https://admin:hunter2@os.test:9200":  "os.test:9200",
		"https://admin:hunter2@os%2test:9200": "<url with user info>", // unparseable: the account name must not echo the password
		// The default port is the same endpoint: the entry must not vanish when
		// someone types it out.
		"https://os.internal:443": "os.internal",
		"http://os.internal:80":   "os.internal",
		// A non-default port stays: it is a different endpoint.
		"http://os.internal:443": "os.internal:443",
		"https://os.internal:80": "os.internal:80",
	}
	for in, want := range cases {
		if got := KeyringAccount(in); got != want {
			t.Errorf("KeyringAccount(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestClusterAccountNaming pins the two account shapes and the order the
// runtime tries them in: the cluster entry first, the legacy host entry kept
// readable behind it.
func TestClusterAccountNaming(t *testing.T) {
	if got := ClusterAccount(" uuid-1 "); got != "cluster/uuid-1" {
		t.Errorf("ClusterAccount = %q, want cluster/uuid-1", got)
	}
	cfg := DefaultConfig()
	cfg.OpenSearchURL = "https://a.internal:9200"
	if got := KeyringAccounts(cfg); len(got) != 1 || got[0] != "a.internal:9200" {
		t.Errorf("accounts without a cluster name = %q, want [a.internal:9200]", got)
	}
	cfg.OpenSearchCluster = "uuid-1"
	if got := KeyringAccounts(cfg); len(got) != 2 || got[0] != "cluster/uuid-1" || got[1] != "a.internal:9200" {
		t.Errorf("accounts with a cluster name = %q, want [cluster/uuid-1 a.internal:9200]", got)
	}
}

// TestPasswordClusterThenLegacy checks the read-through: a cluster entry wins,
// and a missing cluster entry still finds the legacy host entry — a store
// configured before cluster naming keeps working without re-entering a
// password, and the account that answered is recorded for status.
func TestPasswordClusterThenLegacy(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)

	conf := filepath.Join(dir, "velruna.conf")
	if err := os.WriteFile(conf, []byte(`{"opensearch_url": "https://os.internal:9200", "opensearch_cluster": "uuid-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeFile(t, conf) })

	entries := map[string]string{"velruna/os.internal:9200": "legacypw"}
	oldGet, oldSet, oldDel := keyring.Get, keyring.Set, keyring.Delete
	t.Cleanup(func() { keyring.Get, keyring.Set, keyring.Delete = oldGet, oldSet, oldDel })
	keyring.Get = func(service, user string) (string, error) {
		pw, ok := entries[service+"/"+user]
		if !ok {
			return "", keyring.ErrNotFound
		}
		return pw, nil
	}
	keyring.Set = func(service, user, value string) error { return nil }
	keyring.Delete = func(service, user string) error { return nil }

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OpenSearchPassword != "legacypw" || cfg.PasswordSource != "keyring" {
		t.Errorf("legacy read-through failed: pw=%q source=%q", cfg.OpenSearchPassword, cfg.PasswordSource)
	}
	if cfg.KeyringAccount != "os.internal:9200" {
		t.Errorf("KeyringAccount = %q, want the legacy entry that answered", cfg.KeyringAccount)
	}

	entries["velruna/cluster/uuid-1"] = "clusterpw"
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OpenSearchPassword != "clusterpw" {
		t.Errorf("cluster entry must win: pw=%q", cfg.OpenSearchPassword)
	}
	if cfg.KeyringAccount != "cluster/uuid-1" {
		t.Errorf("KeyringAccount = %q, want cluster/uuid-1", cfg.KeyringAccount)
	}
}

// TestClusterNameFromEnvAndFile: opensearch_cluster is a config key and an
// environment override like any other credential-adjacent setting.
func TestClusterNameFromEnvAndFile(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)

	conf := filepath.Join(dir, "velruna.conf")
	if err := os.WriteFile(conf, []byte(`{"opensearch_cluster": "from-file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeFile(t, conf) })

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OpenSearchCluster != "from-file" {
		t.Errorf("OpenSearchCluster = %q, want from-file", cfg.OpenSearchCluster)
	}
	t.Setenv("OPENSEARCH_CLUSTER", "from-env")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig with env: %v", err)
	}
	if cfg.OpenSearchCluster != "from-env" {
		t.Errorf("env must override the file: %q", cfg.OpenSearchCluster)
	}
}

// TestSetConfigValuePreservesTheFile: recording the cluster name must not
// rewrite or lose anything else the operator put in the config.
func TestSetConfigValuePreservesTheFile(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "velruna.conf")
	body := `{"opensearch_url": "https://os.internal:9200", "history_limit": 5, "fetch_allow_hosts": ["a.example"]}`
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetConfigValue(conf, "opensearch_cluster", "uuid-1"); err != nil {
		t.Fatalf("SetConfigValue: %v", err)
	}
	data, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("rewritten config does not parse: %v", err)
	}
	if doc["opensearch_cluster"] != "uuid-1" || doc["history_limit"] != float64(5) ||
		doc["opensearch_url"] != "https://os.internal:9200" ||
		len(doc["fetch_allow_hosts"].([]any)) != 1 {
		t.Errorf("rewritten config lost something: %s", data)
	}
	if fi, err := os.Stat(conf); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v (%v), want 600", fi.Mode(), err)
	}
	// Idempotent: a second write of the same value is a no-op.
	before, _ := os.ReadFile(conf)
	if err := SetConfigValue(conf, "opensearch_cluster", "uuid-1"); err != nil {
		t.Fatalf("SetConfigValue (again): %v", err)
	}
	after, _ := os.ReadFile(conf)
	if string(before) != string(after) {
		t.Errorf("re-writing the same value changed the file:\n%s\n->\n%s", before, after)
	}
}

func TestAllowPlaintextDefaultsOff(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	writePlainConf(t, dir, `{"opensearch_url": "https://127.0.0.1:9200"}`)
	t.Cleanup(func() { removeFile(t, filepath.Join(dir, "velruna.conf")) })

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowPlaintext {
		t.Error("allow_plaintext must default to false")
	}
	if cfg.PasswordSource != "none" {
		t.Errorf("source = %q, want none", cfg.PasswordSource)
	}
}

func TestAllowPlaintextFileField(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	writePlainConf(t, dir, `{"allow_plaintext": true, "opensearch_password": "filepw"}`)
	t.Cleanup(func() { removeFile(t, filepath.Join(dir, "velruna.conf")) })

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.AllowPlaintext || cfg.PasswordSource != "file" || cfg.OpenSearchPassword != "filepw" {
		t.Errorf("allow_plaintext file field not applied: %+v", cfg)
	}
}
