package client

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NemoNobody/velruna/internal/config"
)

// probeCfg is the test config: a short timeout and the loopback opt-in the
// httptest servers need (the SSRF test builds its own config with the opt-in
// off).
func probeCfg() *config.Config {
	return &config.Config{FetchTimeoutSeconds: 5, FetchAllowPrivate: true}
}

// TestProbeHeadContentLength pins the cheap path: when HEAD answers 2xx with
// a positive Content-Length the probe reports it and reads nothing — no body
// bytes, no fallback GET.
func TestProbeHeadContentLength(t *testing.T) {
	var heads, gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			gets.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		heads.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", "1234")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res, err := ProbeURL(context.Background(), probeCfg(), srv.URL)
	if err != nil {
		t.Fatalf("ProbeURL: %v", err)
	}
	if res.Method != "head" {
		t.Errorf("method = %q, want head", res.Method)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", res.Status)
	}
	if res.BytesTotal != 1234 {
		t.Errorf("bytes_total = %d, want 1234", res.BytesTotal)
	}
	if res.BytesSampled != 0 {
		t.Errorf("bytes_sampled = %d, want 0 (HEAD reads no body)", res.BytesSampled)
	}
	if res.ContentType != "text/html" {
		t.Errorf("content_type = %q, want text/html", res.ContentType)
	}
	if res.URL != srv.URL+"/" || res.FinalURL != srv.URL+"/" {
		t.Errorf("url/final_url = %q/%q, want %q", res.URL, res.FinalURL, srv.URL+"/")
	}
	if heads.Load() != 1 || gets.Load() != 0 {
		t.Errorf("requests = %d HEAD, %d GET; want 1/0", heads.Load(), gets.Load())
	}
}

// TestProbeHeadWithoutLengthUsesRange pins the fallback: a 2xx HEAD with no
// length forces one Range GET, and the total comes from Content-Range.
func TestProbeHeadWithoutLengthUsesRange(t *testing.T) {
	var gotRange atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK) // no Content-Length: size unknown
			return
		}
		gotRange.Store(r.Header.Get("Range"))
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Range", "bytes 0-99/500")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusPartialContent)
		writeBody(t, w, strings.Repeat("a", 100))
	}))
	defer srv.Close()

	res, err := ProbeURL(context.Background(), probeCfg(), srv.URL)
	if err != nil {
		t.Fatalf("ProbeURL: %v", err)
	}
	if res.Method != "range" {
		t.Errorf("method = %q, want range", res.Method)
	}
	if res.Status != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", res.Status)
	}
	if res.BytesTotal != 500 {
		t.Errorf("bytes_total = %d, want 500 (from Content-Range)", res.BytesTotal)
	}
	if res.BytesSampled != 100 {
		t.Errorf("bytes_sampled = %d, want 100", res.BytesSampled)
	}
	if got, _ := gotRange.Load().(string); got != "bytes=0-65535" {
		t.Errorf("Range header = %q, want bytes=0-65535", got)
	}
	if res.Note != "" {
		t.Errorf("note = %q, want empty on a 206", res.Note)
	}
}

// TestProbeRangeIgnored pins the 200 answer to a Range request: the size is
// the declared Content-Length, the sample is bounded, and the note warns that
// a later full fetch would pay for the whole entity.
func TestProbeRangeIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		writeBody(t, w, strings.Repeat("x", 1000))
	}))
	defer srv.Close()

	res, err := ProbeURL(context.Background(), probeCfg(), srv.URL)
	if err != nil {
		t.Fatalf("ProbeURL: %v", err)
	}
	if res.Method != "range" || res.Status != http.StatusOK {
		t.Errorf("method/status = %q/%d, want range/200", res.Method, res.Status)
	}
	if res.BytesTotal != 1000 {
		t.Errorf("bytes_total = %d, want 1000 (declared Content-Length)", res.BytesTotal)
	}
	if res.BytesSampled != 1000 {
		t.Errorf("bytes_sampled = %d, want 1000", res.BytesSampled)
	}
	if !strings.Contains(res.Note, "ignored Range") {
		t.Errorf("note = %q, want it to say the server ignored Range", res.Note)
	}
}

// TestProbeHeadMethodNotAllowed pins the 405 fallback: a server that does not
// implement HEAD is probed with the bounded GET instead.
func TestProbeHeadMethodNotAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Allow", "GET")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Range", "bytes 0-99/700")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusPartialContent)
		writeBody(t, w, strings.Repeat("b", 100))
	}))
	defer srv.Close()

	res, err := ProbeURL(context.Background(), probeCfg(), srv.URL)
	if err != nil {
		t.Fatalf("ProbeURL: %v", err)
	}
	if res.Method != "range" {
		t.Errorf("method = %q, want range after HEAD 405", res.Method)
	}
	if res.Status != http.StatusPartialContent || res.BytesTotal != 700 || res.BytesSampled != 100 {
		t.Errorf("status/total/sampled = %d/%d/%d, want 206/700/100",
			res.Status, res.BytesTotal, res.BytesSampled)
	}
}

// TestProbeContentRangeWildcard pins the unknown-size answer: "/_*" must
// become -1, not 0 and not a guess.
func TestProbeContentRangeWildcard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Range", "bytes 0-99/*")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusPartialContent)
		writeBody(t, w, strings.Repeat("c", 100))
	}))
	defer srv.Close()

	res, err := ProbeURL(context.Background(), probeCfg(), srv.URL)
	if err != nil {
		t.Fatalf("ProbeURL: %v", err)
	}
	if res.BytesTotal != -1 {
		t.Errorf("bytes_total = %d, want -1 for an unknown total", res.BytesTotal)
	}
	if res.BytesSampled != 100 {
		t.Errorf("bytes_sampled = %d, want 100", res.BytesSampled)
	}
}

// TestProbeRedirect pins the per-hop policy: an allowed hop is followed and
// recorded, a hop into a host outside fetch_allow_hosts is refused with the
// policy sentinel.
func TestProbeRedirect(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Length", "42")
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()
	finalURL := final.URL + "/final"

	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, finalURL, http.StatusFound)
	}))
	defer hop.Close()

	cfg := probeCfg()
	cfg.FetchAllowHosts = []string{"127.0.0.1"}

	res, err := ProbeURL(context.Background(), cfg, hop.URL)
	if err != nil {
		t.Fatalf("ProbeURL through an allowed redirect: %v", err)
	}
	if res.FinalURL != finalURL {
		t.Errorf("final_url = %q, want %q", res.FinalURL, finalURL)
	}
	if res.Method != "head" || res.Status != http.StatusOK || res.BytesTotal != 42 {
		t.Errorf("method/status/total = %q/%d/%d, want head/200/42",
			res.Method, res.Status, res.BytesTotal)
	}

	port := strings.TrimPrefix(final.URL, "http://127.0.0.1:")
	badHop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// "localhost" is outside the allowlist ("127.0.0.1" only), so the hop
		// must be refused before any connection is made.
		http.Redirect(w, r, "http://localhost:"+port+"/denied", http.StatusFound)
	}))
	defer badHop.Close()

	if _, err := ProbeURL(context.Background(), cfg, badHop.URL); err == nil {
		t.Fatal("redirect to a host outside fetch_allow_hosts must be refused")
	} else if !errors.Is(err, ErrFetchPolicy) || !strings.Contains(err.Error(), "not in fetch_allow_hosts") {
		t.Errorf("want a policy refusal naming the allowlist, got: %v", err)
	}
}

// TestProbeSSRFRefused pins the guard boundary: without fetch_allow_private a
// literal loopback target is refused during validation, before any dial, and
// the documented opt-in makes it probeable.
func TestProbeSSRFRefused(t *testing.T) {
	cfg := &config.Config{FetchTimeoutSeconds: 5}

	if _, err := ProbeURL(context.Background(), cfg, "http://127.0.0.1/"); err == nil {
		t.Fatal("loopback literal must be refused")
	} else if !strings.Contains(err.Error(), "not a public address") {
		t.Errorf("want the public-address refusal, got: %v", err)
	}

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := ProbeURL(context.Background(), cfg, srv.URL); err == nil {
		t.Fatal("a loopback httptest server must be refused without fetch_allow_private")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("refused target received %d requests, want 0 (refusal precedes the dial)", n)
	}

	priv := probeCfg()
	if _, err := ProbeURL(context.Background(), priv, srv.URL); err != nil {
		t.Errorf("fetch_allow_private must make the target probeable: %v", err)
	}
}

// TestProbeContentTypeNormalized pins the media-type reduction: parameters
// (charset, quoted values) and letter case never leak into the result.
func TestProbeContentTypeNormalized(t *testing.T) {
	cases := []struct{ header, want string }{
		{"text/html; charset=UTF-8", "text/html"},
		{`application/json; charset="utf-8"`, "application/json"},
		{"Application/PDF", "application/pdf"},
		{"text/plain", "text/plain"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", tc.header)
			w.Header().Set("Content-Length", "10")
			w.WriteHeader(http.StatusOK)
		}))
		res, err := ProbeURL(context.Background(), probeCfg(), srv.URL)
		srv.Close()
		if err != nil {
			t.Fatalf("ProbeURL(%q): %v", tc.header, err)
		}
		if res.ContentType != tc.want {
			t.Errorf("Content-Type %q -> %q, want %q", tc.header, res.ContentType, tc.want)
		}
	}
}

// TestProbeRangeSamplingLimit pins the 64 KiB cap and the close: the handler
// unblocks on connection close only, so a probe that drained the 200 KiB body
// instead of closing it would fail the wait.
func TestProbeRangeSamplingLimit(t *testing.T) {
	const total = 200 << 10
	var once sync.Once
	closed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		chunk := bytes.Repeat([]byte("x"), 4<<10)
		for written := 0; written < total; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				break
			}
		}
		// A keep-alive client that closed the body drops the connection; the
		// request context then cancels. Without the close this blocks and the
		// test reports a leaked body.
		<-r.Context().Done()
		once.Do(func() { close(closed) })
	}))
	// Defers are LIFO: CloseClientConnections must run first, or a regression
	// that leaves the body open makes srv.Close block after the wait below
	// times out (and the test hangs instead of reporting).
	defer srv.Close()
	defer srv.CloseClientConnections()

	res, err := ProbeURL(context.Background(), probeCfg(), srv.URL)
	if err != nil {
		t.Fatalf("ProbeURL: %v", err)
	}
	if res.Method != "range" {
		t.Errorf("method = %q, want range", res.Method)
	}
	if res.Status != http.StatusOK || res.BytesTotal != total {
		t.Errorf("status/total = %d/%d, want 200/%d", res.Status, res.BytesTotal, total)
	}
	if res.BytesSampled != probeSampleBytes {
		t.Errorf("bytes_sampled = %d, want exactly %d", res.BytesSampled, probeSampleBytes)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("probe left the response body open: the server connection never closed")
	}
}

// TestProbeFallbackUsesFinalURL pins the second leg's target: when HEAD was
// redirected, the fallback GET must ask the final hop directly instead of
// walking the chain again (double traffic, possibly a different landing).
func TestProbeFallbackUsesFinalURL(t *testing.T) {
	var hopHits, finalHits atomic.Int32
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		finalHits.Add(1)
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK) // no length: force the fallback
			return
		}
		w.Header().Set("Content-Range", "bytes 0-99/500")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusPartialContent)
		writeBody(t, w, strings.Repeat("d", 100))
	}))
	defer final.Close()
	finalURL := final.URL + "/final"

	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hopHits.Add(1)
		http.Redirect(w, r, finalURL, http.StatusFound)
	}))
	defer hop.Close()

	res, err := ProbeURL(context.Background(), probeCfg(), hop.URL)
	if err != nil {
		t.Fatalf("ProbeURL: %v", err)
	}
	if res.FinalURL != finalURL {
		t.Errorf("final_url = %q, want %q", res.FinalURL, finalURL)
	}
	if res.Method != "range" || res.Status != http.StatusPartialContent || res.BytesTotal != 500 {
		t.Errorf("method/status/total = %q/%d/%d, want range/206/500",
			res.Method, res.Status, res.BytesTotal)
	}
	if got := hopHits.Load(); got != 1 {
		t.Errorf("redirect hop hit %d times, want 1 (the GET must target the final url)", got)
	}
	if got := finalHits.Load(); got != 2 {
		t.Errorf("final hop hit %d times, want 2 (HEAD + one fallback GET)", got)
	}
}

// TestProbeFallbackKeepsHeadContentType pins the media-type handover: a 206
// without Content-Type must not erase the type HEAD declared.
func TestProbeFallbackKeepsHeadContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK) // no length: force the fallback
			return
		}
		// No Content-Type (and no body, so the server does not sniff one).
		w.Header().Set("Content-Range", "bytes 0-99/500")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusPartialContent)
	}))
	defer srv.Close()

	res, err := ProbeURL(context.Background(), probeCfg(), srv.URL)
	if err != nil {
		t.Fatalf("ProbeURL: %v", err)
	}
	if res.ContentType != "text/html" {
		t.Errorf("content_type = %q, want the HEAD type text/html", res.ContentType)
	}
	if res.BytesTotal != 500 {
		t.Errorf("bytes_total = %d, want 500 from Content-Range", res.BytesTotal)
	}
}

// TestProbeEmptyEntity416 pins the empty-entity answer: HEAD declares 0 bytes,
// the range GET is refused with 416 and "Content-Range: bytes */0" — the total
// is known (0) and must not degrade to unknown.
func TestProbeEmptyEntity416(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Range", "bytes */0")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer srv.Close()

	res, err := ProbeURL(context.Background(), probeCfg(), srv.URL)
	if err != nil {
		t.Fatalf("ProbeURL: %v", err)
	}
	if res.Status != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("status = %d, want 416", res.Status)
	}
	if res.BytesTotal != 0 {
		t.Errorf("bytes_total = %d, want 0 from Content-Range", res.BytesTotal)
	}
	if res.Note == "" {
		t.Error("the empty entity must carry a note")
	}
}

// TestProbeEmptyEntity200 pins the 200-with-no-body case: nothing was sampled,
// so the "ignored Range" note would be a lie.
func TestProbeEmptyEntity200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res, err := ProbeURL(context.Background(), probeCfg(), srv.URL)
	if err != nil {
		t.Fatalf("ProbeURL: %v", err)
	}
	if res.BytesTotal != 0 || res.BytesSampled != 0 {
		t.Errorf("total/sampled = %d/%d, want 0/0", res.BytesTotal, res.BytesSampled)
	}
	if strings.Contains(res.Note, "ignored Range") {
		t.Errorf("note = %q; an empty entity ignored nothing", res.Note)
	}
	if !strings.Contains(res.Note, "empty") {
		t.Errorf("note = %q, want a precise empty-entity note", res.Note)
	}
}

// TestProbeIgnoresEnvProxy pins the direct-only transport: even with
// HTTP_PROXY/HTTPS_PROXY set, a probe must not route through the proxy. The
// DefaultTransport is pinned because net/http caches the env proxy decision
// once per process (an earlier test's env would otherwise win); the pinned
// transport returns the recording proxy, so dropping tr.Proxy = nil makes the
// probe hit it and fails here.
func TestProbeIgnoresEnvProxy(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	preq, err := http.NewRequest(http.MethodGet, proxy.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	orig := http.DefaultTransport
	http.DefaultTransport = &http.Transport{Proxy: http.ProxyURL(preq.URL)}
	t.Cleanup(func() { http.DefaultTransport = orig })
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)

	cfg := probeCfg()
	cfg.FetchTimeoutSeconds = 2
	// .invalid is reserved: a direct attempt can never reach a server, so the
	// proxy's hit counter is the only observable.
	if _, err := ProbeURL(context.Background(), cfg, "http://probe-env-proxy.invalid/"); err == nil {
		t.Error("a direct probe of a reserved host must fail")
	}
	if n := proxyHits.Load(); n != 0 {
		t.Errorf("probe sent %d requests through the proxy, want 0 (direct-only)", n)
	}
}

// TestProbeDialGuardRefusesLoopbackName pins the resolved-address guard: a
// hostname that resolves to loopback is refused at dial time, and the guard
// is wired into the probe transport. "[::1%25lo]" is a loopback literal in a
// form the name-level pre-guard does not recognize (net.ParseIP rejects the
// zone), so only the resolved-address dial guard can stop it; dropping
// tr.DialContext = guardedDialContext(nil) lets the request through and fails
// here.
func TestProbeDialGuardRefusesLoopbackName(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := srv.Listener.Addr().String()
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		t.Fatalf("unexpected listener address %q", addr)
	}
	port := addr[i+1:]

	// A hostname resolving to loopback is refused by the guard itself.
	if _, err := guardedDialContext(nil)(context.Background(), "tcp", "localhost:"+port); err == nil {
		t.Error("guarded dial of localhost must be refused")
	} else if !strings.Contains(err.Error(), "refusing connection to non-public address") {
		t.Errorf("want the resolved-address refusal, got: %v", err)
	}

	cfg := &config.Config{FetchTimeoutSeconds: 5} // fetch_allow_private off
	target := "http://[::1%25lo]:" + port + "/"
	if _, err := ProbeURL(context.Background(), cfg, target); err == nil {
		t.Fatal("a loopback target must be refused without fetch_allow_private")
	} else if !strings.Contains(err.Error(), "refusing connection to non-public address") {
		t.Errorf("want the resolved-address dial refusal, got: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("guarded target received %d requests, want 0", n)
	}
}
