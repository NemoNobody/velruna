package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/keyring"
)

// fakeKeyringCmd replaces the OS keyring for cmd tests: stored entries are
// recorded in memory, the real wallet is never touched.
func fakeKeyringCmd(t *testing.T) (stored map[string]string) {
	t.Helper()
	stored = map[string]string{}
	oldGet, oldSet, oldDel := keyring.Get, keyring.Set, keyring.Delete
	t.Cleanup(func() {
		keyring.Get, keyring.Set, keyring.Delete = oldGet, oldSet, oldDel
	})
	keyring.Get = func(service, user string) (string, error) {
		pw, ok := stored[service+"/"+user]
		if !ok {
			return "", keyring.ErrNotFound
		}
		return pw, nil
	}
	keyring.Set = func(service, user, value string) error {
		stored[service+"/"+user] = value
		return nil
	}
	keyring.Delete = func(service, user string) error {
		if _, ok := stored[service+"/"+user]; !ok {
			return keyring.ErrNotFound
		}
		delete(stored, service+"/"+user)
		return nil
	}
	return stored
}

// credsTestEnv isolates config resolution into a temp dir.
func credsTestEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	conf := filepath.Join(dir, "velruna.conf")
	if err := os.WriteFile(conf, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VELRUNA_CONF", conf)
	t.Setenv("OPENSEARCH_PASSWORD", "")
	t.Setenv("OPENSEARCH_CLUSTER", "")
	t.Setenv("OPENSEARCH_USER", "")
}

// withStdin replaces os.Stdin with a piped file for the duration of fn.
func withStdin(t *testing.T, content string, fn func()) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; _ = f.Close() })
	fn()
}

// TestCredsSetFallsBackToLegacyHostWhenStoreSilent: an unreachable store
// cannot confirm its cluster_uuid, so the entry is keyed by host — and the
// operator is told that happened, not left to discover it later.
func TestCredsSetFallsBackToLegacyHostWhenStoreSilent(t *testing.T) {
	credsTestEnv(t)
	stored := fakeKeyringCmd(t)

	// A closed server answers instantly and deterministically; port 1 can be
	// firewalled into a slow timeout on some hosts.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	host := strings.TrimPrefix(deadURL, "http://")

	cmd := CredsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	withStdin(t, "sekret pw\n", func() {
		cmd.SetArgs([]string{"set", "--url", deadURL})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("creds set: %v", err)
		}
	})
	if got := out.String(); !strings.Contains(got, "account="+host) {
		t.Errorf("set output: %q", got)
	}
	if !strings.Contains(out.String(), "legacy account") {
		t.Errorf("the silent-store fallback must be reported:\n%s", out.String())
	}
	if stored["velruna/"+host] != "sekret pw" {
		t.Errorf("keyring after set: %v", stored)
	}

	out.Reset()
	cmd = CredsCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"rm", "--url", deadURL})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("creds rm: %v", err)
	}
	if _, ok := stored["velruna/"+host]; ok {
		t.Error("entry survived rm")
	}
	if !strings.Contains(out.String(), "removed") {
		t.Errorf("rm output: %q", out.String())
	}

	// rm on a missing entry is an answer, not a crash.
	out.Reset()
	cmd = CredsCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"rm", "--url", deadURL})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("creds rm (absent): %v", err)
	}
	if !strings.Contains(out.String(), "no entry") {
		t.Errorf("rm absent output: %q", out.String())
	}
}

// The entry is keyed by the store's identity: 'set' asks the cluster its
// cluster_uuid, stores under account "cluster/<uuid>" and records the same
// name in the config file, so the node list can change without orphaning the
// password.
func TestCredsSetNamesClusterAndRecordsConfig(t *testing.T) {
	credsTestEnv(t)
	stored := fakeKeyringCmd(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.Error(w, `{"error":"unexpected path `+r.URL.Path+`"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cluster_name":"t","cluster_uuid":"AbCd123","version":{"number":"3.8.0"}}`))
	}))
	t.Cleanup(srv.Close)

	conf := os.Getenv("VELRUNA_CONF")
	body, _ := json.Marshal(map[string]string{"opensearch_url": srv.URL, "opensearch_user": "u"})
	if err := os.WriteFile(conf, body, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := CredsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	withStdin(t, "cluster pw\n", func() {
		cmd.SetArgs([]string{"set"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("creds set: %v", err)
		}
	})
	if stored["velruna/cluster/AbCd123"] != "cluster pw" {
		t.Errorf("keyring after set: %v", stored)
	}
	if got := out.String(); !strings.Contains(got, "account=cluster/AbCd123") {
		t.Errorf("set output: %q", got)
	}
	if !strings.Contains(out.String(), "opensearch_cluster=AbCd123 recorded") {
		t.Errorf("the config must record the cluster name:\n%s", out.String())
	}

	data, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("config after set does not parse: %v", err)
	}
	if doc["opensearch_cluster"] != "AbCd123" || doc["opensearch_url"] != srv.URL {
		t.Errorf("config rewritten wrong: %s", data)
	}
}

// --name labels the entry without probing, and a --url override must not
// rewrite the config's identity (it belongs to another endpoint).
func TestCredsSetNameFlagAndUrlOverride(t *testing.T) {
	credsTestEnv(t)
	stored := fakeKeyringCmd(t)

	cmd := CredsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	withStdin(t, "labeled pw\n", func() {
		cmd.SetArgs([]string{"set", "--url", "http://127.0.0.1:1", "--name", "lab"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("creds set --name: %v", err)
		}
	})
	if stored["velruna/cluster/lab"] != "labeled pw" {
		t.Errorf("keyring after set --name: %v", stored)
	}
	if !strings.Contains(out.String(), "--url does not touch the config") {
		t.Errorf("--url must not write the config identity silently:\n%s", out.String())
	}
	data, err := os.ReadFile(os.Getenv("VELRUNA_CONF"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "opensearch_cluster") {
		t.Errorf("--url rewrote the config identity: %s", data)
	}
}

// rm removes the entry the configuration resolves to; --legacy also clears the
// host entry the migration to cluster naming left behind.
func TestCredsRmClusterAndLegacy(t *testing.T) {
	credsTestEnv(t)
	stored := fakeKeyringCmd(t)
	stored["velruna/cluster/uuid-1"] = "cluster pw"
	stored["velruna/127.0.0.1:1"] = "legacy pw"

	conf := os.Getenv("VELRUNA_CONF")
	if err := os.WriteFile(conf, []byte(`{"opensearch_url":"http://127.0.0.1:1","opensearch_cluster":"uuid-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := CredsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"rm"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("creds rm: %v", err)
	}
	if _, ok := stored["velruna/cluster/uuid-1"]; ok {
		t.Error("cluster entry survived rm")
	}
	if _, ok := stored["velruna/127.0.0.1:1"]; !ok {
		t.Errorf("rm must not clear the legacy entry without --legacy: %v", stored)
	}

	out.Reset()
	cmd = CredsCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"rm", "--legacy"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("creds rm --legacy: %v", err)
	}
	if _, ok := stored["velruna/127.0.0.1:1"]; ok {
		t.Error("--legacy must clear the legacy entry")
	}
	if !strings.Contains(out.String(), "no entry") {
		t.Errorf("rm --legacy output: %q", out.String())
	}
}

// A rotated password must land under the account the runtime reads first. When
// the store is silent but the config already names it (a previous successful
// set), 'set' must reuse that name instead of falling back to the legacy host
// account, where the rotation would report success and change nothing.
func TestCredsSetRotationReusesRecordedCluster(t *testing.T) {
	credsTestEnv(t)
	stored := fakeKeyringCmd(t)
	stored["velruna/cluster/lab"] = "old pw"

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	host := strings.TrimPrefix(deadURL, "http://")

	conf := os.Getenv("VELRUNA_CONF")
	if err := os.WriteFile(conf, []byte(`{"opensearch_url":"`+deadURL+`","opensearch_cluster":"lab"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := CredsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	withStdin(t, "new pw\n", func() {
		cmd.SetArgs([]string{"set"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("creds set: %v", err)
		}
	})
	if got := out.String(); !strings.Contains(got, "reusing the store name") {
		t.Errorf("the name reuse must be reported:\n%s", got)
	}
	if stored["velruna/cluster/lab"] != "new pw" {
		t.Errorf("cluster entry after rotation: %v", stored)
	}
	if _, ok := stored["velruna/"+host]; ok {
		t.Errorf("rotation must not create a legacy entry beside the named one: %v", stored)
	}
}

// An answer without a usable cluster_uuid is not an identity: storing under an
// empty name would create an entry nothing reads back.
func TestProbeClusterUUIDBlankIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cluster_name":"x","cluster_uuid":"  "}`))
	}))
	defer srv.Close()
	if _, err := probeClusterUUID(srv.URL, nil, "pw"); err == nil {
		t.Error("a blank cluster_uuid must be reported as an error")
	}
}

// The store may be alive behind a node that is not first in the list: the probe
// must fail over across every configured node before declaring it silent.
func TestProbeClusterUUIDFailsOverAcrossNodes(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cluster_name":"x","cluster_uuid":"uuid-live"}`))
	}))
	defer live.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	base := config.DefaultConfig()
	base.OpenSearchURLs = []string{deadURL, live.URL}
	uuid, err := probeClusterUUID(base.EffectiveURLs()[0], base, "pw")
	if err != nil {
		t.Fatalf("the probe must fail over to the live node: %v", err)
	}
	if uuid != "uuid-live" {
		t.Errorf("probed uuid = %q", uuid)
	}
}

// A node that accepts the connection and then never answers must be bounded
// per attempt and rotated away by the retry (Info is read-only, so a
// timed-out attempt is safe to retry). The pool starts at a random node, so
// the other node answers 502 — a retryable status — which makes the round
// visit BOTH nodes whatever the starting point is: either attempt 1 hangs
// and the retry lands on the 502 node, or attempt 1 gets the 502 and the
// retry hangs. The probe must then fail — no node answers usefully — inside
// the per-attempt bound, having genuinely attempted the silent node. With
// no per-attempt bound the hanging node would instead burn the whole scaled
// budget and trip the elapsed check.
func TestProbeClusterUUIDRetriesPastHangingNode(t *testing.T) {
	var silentHits atomic.Int32
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		silentHits.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
			_, _ = w.Write([]byte(`{"cluster_uuid":"never"}`))
		}
	}))
	defer silent.Close()
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"temporary"}`, http.StatusBadGateway)
	}))
	defer flaky.Close()

	base := config.DefaultConfig()
	base.OpenSearchURLs = []string{silent.URL, flaky.URL}
	start := time.Now()
	_, err := probeClusterUUID(base.EffectiveURLs()[0], base, "pw")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("no node answers usefully: the probe must fail")
	}
	if probeRejectedPassword(err) {
		t.Errorf("a hanging node plus a 502 must not read as a rejected password: %v", err)
	}
	if silentHits.Load() == 0 {
		t.Fatal("the retry never routed an attempt to the silent node")
	}
	if elapsed > 15*time.Second {
		t.Errorf("the probe waited %v, want the ~10s per-attempt bound, not the whole scaled budget", elapsed)
	}
}

// Entries created for a store the current config no longer points at are only
// reachable through an explicit label; rm must not touch the configured store's
// legacy entry while cleaning up an orphan by --name.
func TestCredsRmByName(t *testing.T) {
	credsTestEnv(t)
	stored := fakeKeyringCmd(t)
	stored["velruna/cluster/lab"] = "orphan pw"
	stored["velruna/127.0.0.1:9"] = "current legacy pw"

	cmd := CredsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"rm", "--name", "lab", "--url", "http://127.0.0.1:9"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("creds rm --name: %v", err)
	}
	if _, ok := stored["velruna/cluster/lab"]; ok {
		t.Error("--name must remove the named cluster entry")
	}
	if _, ok := stored["velruna/127.0.0.1:9"]; !ok {
		t.Errorf("rm --name must not clear the legacy entry without --legacy: %v", stored)
	}
	if !strings.Contains(out.String(), "removed") {
		t.Errorf("rm --name output: %q", out.String())
	}
}

// --name beats the config's recorded name: cleaning an orphan must not
// touch the live store's entry — the whole point of the explicit label.
func TestCredsRmByNameBeatsRecorded(t *testing.T) {
	credsTestEnv(t)
	stored := fakeKeyringCmd(t)
	stored["velruna/cluster/lab"] = "orphan pw"
	stored["velruna/cluster/live"] = "live pw"

	conf := os.Getenv("VELRUNA_CONF")
	if err := os.WriteFile(conf, []byte(`{"opensearch_url":"http://127.0.0.1:9","opensearch_cluster":"live"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := CredsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"rm", "--name", "lab"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("creds rm --name: %v", err)
	}
	if _, ok := stored["velruna/cluster/lab"]; ok {
		t.Error("the named orphan must be removed")
	}
	if _, ok := stored["velruna/cluster/live"]; !ok {
		t.Errorf("the config's own entry must survive: %v", stored)
	}
}

// "the store is silent" and "the store rejected the password" are different
// verdicts: only the first may fall back to the recorded-name reuse, the
// second must stop the write (the rejected password cannot work in the
// runtime either). 403 sits on the silent side: a valid user without
// cluster-info rights gets a permission refusal, not a credential refusal.
func TestProbeRejectedPassword(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"security_exception","reason":"unable to authenticate user"},"status":401}`))
	}))
	defer auth.Close()
	html401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A store behind a proxy that answers 401 with an HTML page says the
		// same thing as the OpenSearch error JSON: the HTTP answer, not the
		// body shape, is the proof the credential was refused.
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("<html><body>401 Unauthorized</body></html>"))
	}))
	defer html401.Close()
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"type":"permission_denied_exception","reason":"no cluster rights"},"status":403}`))
	}))
	defer forbidden.Close()

	if _, err := probeClusterUUID(auth.URL, nil, "bad pw"); err == nil {
		t.Fatal("a 401 answer must fail the probe")
	} else if !probeRejectedPassword(err) {
		t.Errorf("a 401 must read as a rejected password: %v", err)
	}

	if _, err := probeClusterUUID(html401.URL, nil, "bad pw"); err == nil {
		t.Fatal("a 401 answer with a non-OpenSearch body must fail the probe")
	} else if !probeRejectedPassword(err) {
		t.Errorf("a 401 with an HTML body must read as a rejected password: %v", err)
	}

	if _, err := probeClusterUUID(forbidden.URL, nil, "pw"); err == nil {
		t.Fatal("a 403 answer must fail the probe")
	} else if probeRejectedPassword(err) {
		t.Errorf("a 403 is a permission refusal, not a rejected password: %v", err)
	}

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	if _, err := probeClusterUUID(deadURL, nil, "pw"); err == nil {
		t.Fatal("an unreachable store must fail the probe")
	} else if probeRejectedPassword(err) {
		t.Errorf("silence is not a rejection: %v", err)
	}
}

// With --url there is no config user context, but the probe must still
// authenticate (as the runtime's default user): an anonymous request
// against a secured store would answer 401 and read as "your password is
// wrong", blocking a perfectly legitimate creds set.
func TestProbeClusterUUIDAuthenticatesOnURLOverride(t *testing.T) {
	t.Setenv("OPENSEARCH_USER", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"type":"security_exception","reason":"no credentials"},"status":401}`))
			return
		}
		// Pin WHO the fallback authenticates as: the 401 message names this
		// user, so a silent change of the default would make it a lie.
		if u != config.DefaultConfig().OpenSearchUser {
			t.Errorf("probe authenticated as %q, want the default user %q", u, config.DefaultConfig().OpenSearchUser)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cluster_name":"x","cluster_uuid":"uuid-ok"}`))
	}))
	defer srv.Close()

	uuid, err := probeClusterUUID(srv.URL, nil, "pw")
	if err != nil {
		t.Fatalf("the probe must authenticate even on the --url path: %v", err)
	}
	if uuid != "uuid-ok" {
		t.Errorf("probed uuid = %q", uuid)
	}
}

// The secret never leaves the keyring through the CLI: 'creds get' was removed
// precisely so no caller can print the password, and stdout of the remaining
// subcommands must stay secret-free.
func TestCredsGetIsGone(t *testing.T) {
	credsTestEnv(t)
	fakeKeyringCmd(t)
	for _, sub := range CredsCmd().Commands() {
		if sub.Name() == "get" {
			t.Fatal("'creds get' must not exist: the password is read back through the keyring, never printed")
		}
	}
}

// The credential-leak guards: a --url carrying userinfo must be refused, and
// no code path may echo the password back (prompts, "stored:" lines, status).
func TestCredsRefuseUserinfo(t *testing.T) {
	credsTestEnv(t)
	fakeKeyringCmd(t)

	for _, raw := range []string{
		"https://admin:hunter2@os.test:9200",
		"https://admin:hunter2@os%test:9200", // parse failure with "@": no echo
	} {
		if err := refuseUserinfo(raw); err == nil {
			t.Errorf("refuseUserinfo(%q): want error, got nil", raw)
		} else if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("refuseUserinfo echoes the password: %v", err)
		}
	}
	for _, ok := range []string{"https://os.test:9200", "http://localhost:9200"} {
		if err := refuseUserinfo(ok); err != nil {
			t.Errorf("refuseUserinfo(%q): %v, want nil", ok, err)
		}
	}

	// accountFor must never return a string carrying the credentials.
	if got := accountFor("https://admin:hunter2@os.test:9200"); got != "os.test:9200" {
		t.Errorf("accountFor userinfo url = %q, want the bare host", got)
	}
	if got := accountFor("https://admin:hunter2@os%test:9200"); strings.Contains(got, "hunter2") {
		t.Errorf("accountFor echoes the password on parse failure: %q", got)
	}

	// credsLoadTarget refuses the userinfo flag.
	old := credsFlags.url
	t.Cleanup(func() { credsFlags.url = old })
	credsFlags.url = "https://admin:hunter2@os.test:9200"
	if _, _, err := credsLoadTarget(); err == nil {
		t.Error("credsLoadTarget: want refusal for userinfo --url, got nil")
	} else if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("credsLoadTarget echoes the password: %v", err)
	}
	credsFlags.url = "https://os.test:9200"
	if _, got, err := credsLoadTarget(); err != nil || got != "https://os.test:9200" {
		t.Errorf("credsLoadTarget clean url = %q, %v", got, err)
	}
}

func TestCredsSetEmptyPasswordAborts(t *testing.T) {
	credsTestEnv(t)
	fakeKeyringCmd(t)
	// fatalErr exits the process on empty input, so exercise readSecret
	// directly: the guard under test is the empty-result contract.
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	pw, err := readSecret("prompt: ", f, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("readSecret: %v", err)
	}
	if pw != "" {
		t.Errorf("pw = %q, want empty (aborted by the command)", pw)
	}
}

func TestRenderCredsStatus(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.OpenSearchCluster = "uuid-1"
	cfg.PasswordSource = "keyring"
	cfg.KeyringAccount = "cluster/uuid-1"
	probes := []accountProbe{
		{account: "cluster/uuid-1", kind: "cluster"},
		{account: "os.test:9200", kind: "legacy", err: keyring.ErrNotFound},
	}

	// cluster entry present, legacy absent: both are listed, the present one is
	// what resolves.
	text := renderCredsStatus(cfg, probes, "secret-service")
	for _, want := range []string{
		"velruna/cluster/uuid-1 (cluster, entry present)",
		"velruna/os.test:9200 (legacy, no entry)",
		"source: keyring (cluster/uuid-1)",
		"entry present (value not shown)",
		"backend: secret-service",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "sekret") {
		t.Error("the status renderer has no access to values by design")
	}

	// Nothing stored, plaintext refused: the migration hint appears.
	cfg = config.DefaultConfig()
	cfg.PasswordSource = "none"
	cfg.FileHasPlaintext = true
	cfg.AllowPlaintext = false
	text = renderCredsStatus(cfg, []accountProbe{{account: "os.test:9200", kind: "legacy", err: keyring.ErrNotFound}}, "")
	for _, want := range []string{"REFUSED", "no entry — store one", "hint:"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}

	// --url overrides the endpoint: the config's password source belongs to a
	// different host, so it must not be shown, and the config-file section
	// (plaintext status, migration hint) is about the configured host too.
	text = renderCredsStatus(nil, []accountProbe{{account: "os.test:9200", kind: "legacy"}}, "")
	for _, want := range []string{"source: n/a (--url", "entry present"} {
		if !strings.Contains(text, want) {
			t.Errorf("nil-config text lacks %q:\n%s", want, text)
		}
	}
	for _, deny := range []string{"config file plaintext", "hint:"} {
		if strings.Contains(text, deny) {
			t.Errorf("nil-config text must not mention %q:\n%s", deny, text)
		}
	}

	// A refused backend names its own fix: the container hint must not be
	// appended, or it would contradict the reason on a desktop with the wrong
	// provider.
	cfg = config.DefaultConfig()
	cfg.PasswordSource = "none"
	text = renderCredsStatus(cfg, []accountProbe{{account: "os.test:9200", kind: "legacy", err: fmt.Errorf("%w: gnome-keyring on KDE", keyring.ErrBackend)}}, "")
	if !strings.Contains(text, "gnome-keyring on KDE") {
		t.Errorf("refusal reason missing:\n%s", text)
	}
	if strings.Contains(text, "no session bus in containers/ssh") {
		t.Errorf("an ErrBackend refusal must not carry the container hint:\n%s", text)
	}

	// A provider outage that is not a backend verdict (no bus at all) does get
	// the container hint.
	text = renderCredsStatus(cfg, []accountProbe{{account: "os.test:9200", kind: "legacy", err: errors.New("no session bus")}}, "")
	if !strings.Contains(text, "no session bus in containers/ssh") {
		t.Errorf("non-backend outage should carry the container hint:\n%s", text)
	}

	// A refused backend must not be paired with "velruna creds set": neither the
	// hint nor the plaintext-migration line may point at a store that cannot be
	// written to until the backend is fixed.
	cfg = config.DefaultConfig()
	cfg.PasswordSource = "none"
	cfg.FileHasPlaintext = true
	cfg.AllowPlaintext = false
	text = renderCredsStatus(cfg, []accountProbe{{account: "os.test:9200", kind: "legacy", err: fmt.Errorf("%w: no owner", keyring.ErrBackend)}}, "")
	if strings.Contains(text, "velruna creds set") {
		t.Errorf("refused backend still advertises 'creds set':\n%s", text)
	}
	if !strings.Contains(text, "fix the keyring backend") {
		t.Errorf("refused backend hint missing:\n%s", text)
	}
}
