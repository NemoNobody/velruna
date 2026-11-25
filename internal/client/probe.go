package client

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/NemoNobody/velruna/internal/config"
)

// probeSampleBytes is the body window a range probe reads. 64 KiB is enough
// to tell an HTML page from a PDF, a login wall from an article, and to see
// the first bytes of a binary; it is small enough that probing a huge file
// costs the target (and the link) almost nothing.
const probeSampleBytes int64 = 64 << 10

// ProbeResult is what a pre-fetch probe learned about a URL: identity of the
// final hop, the status, the media type, and what the origin says about its
// size versus how much of it was actually read. BytesTotal is -1 whenever the
// size is genuinely unknown (chunked response, "Content-Range: .../*", a
// non-2xx answer), never a guess.
type ProbeResult struct {
	URL          string
	FinalURL     string
	Status       int
	ContentType  string
	BytesTotal   int64 // -1 when unknown
	BytesSampled int64
	Method       string // "head" | "range"
	Note         string
}

// ProbeURL inspects a URL without storing anything. It normalizes the input,
// applies the fetch allowlist and the public-address guard, then asks the
// origin for its headers with HEAD. When HEAD is refused (405/501) or the
// length is unknown, it samples the first 64 KiB with one bounded GET
// carrying "Range: bytes=0-65535" and reads the size from Content-Range.
//
// The probe is a plain direct HTTP client on purpose: no browser renderer, no
// proxy (neither fetch_proxy_url nor the HTTP_PROXY environment), no cookies,
// no credentials — a probe must be as unremarkable as a link preview. It
// never touches the store: no docs, no facts, no Client. The SSRF defense is
// the same guardedDialContext the fetcher uses, plus the per-hop allowlist
// check in the redirect chain. robots.txt is deliberately not consulted: a
// probe is a metadata preview, not a fetch, and the real fetch enforces it.
//
// A non-2xx status is a result, not an error: the caller reports a bot wall.
// Only transport failures and URL/allowlist/redirect-policy refusals come
// back as errors (fetch policy sentinels are wrapped, so errors.Is works).
func ProbeURL(ctx context.Context, cfg *config.Config, rawURL string) (*ProbeResult, error) {
	if cfg == nil {
		return nil, fmt.Errorf("probe: nil config")
	}
	normalized, err := NormalizeURL(rawURL, cfg.FetchAllowPrivate)
	if err != nil {
		return nil, err
	}
	if err := HostAllowed(cfg, normalized); err != nil {
		return nil, err
	}
	// A probe is a metadata preview, not a fetch: robots.txt is deliberately
	// not consulted. The real fetch enforces it; a probe adds no hidden
	// request (and no proxied robots lookup, which would break the direct-only
	// contract above).

	timeout := time.Duration(cfg.FetchTimeoutSeconds) * time.Second
	if timeout <= 0 {
		// A zero timeout would mean "unbounded" in net/http: a probe must
		// never park the caller on a silent socket.
		timeout = 30 * time.Second
	}
	tr := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	}
	// Direct only. A nil Proxy ignores the ambient HTTP_PROXY/HTTPS_PROXY, so
	// the probe routes exactly like the operator's fetch_allow_hosts intends.
	tr.Proxy = nil
	if !cfg.FetchAllowPrivate {
		tr.DialContext = guardedDialContext(nil)
	}
	defer tr.CloseIdleConnections()

	httpc := &http.Client{Timeout: timeout, Transport: tr}
	httpc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > maxFetchRedirects {
			return fmt.Errorf("probe: more than %d redirects: %w", maxFetchRedirects, ErrRedirectLimit)
		}
		// The allowlist and the public-address rule hold for EVERY hop: an
		// allowed host that bounces to a denied or loopback target must not
		// be probed (redirect-based SSRF, the same hole the fetcher closes).
		if err := HostAllowed(cfg, req.URL.String()); err != nil {
			return fmt.Errorf("probe: redirect chain: %v: %w", err, ErrFetchPolicy)
		}
		if !cfg.FetchAllowPrivate && blockedHost(strings.ToLower(req.URL.Hostname())) {
			return fmt.Errorf("probe: redirect chain: %s is not a public address: %w", req.URL.Hostname(), ErrFetchPolicy)
		}
		return nil
	}

	res := &ProbeResult{URL: normalized, FinalURL: normalized, BytesTotal: -1, Method: "head"}

	headReq, err := probeRequest(ctx, cfg, http.MethodHead, normalized)
	if err != nil {
		return nil, err
	}
	head, err := httpc.Do(headReq)
	if err != nil {
		return nil, fmt.Errorf("probe %s: %w", normalized, err)
	}
	_ = head.Body.Close()
	// resp.Request.URL is the last hop the client actually asked (CheckRedirect
	// only sees the hops in between).
	res.FinalURL = head.Request.URL.String()
	res.Status = head.StatusCode
	res.ContentType = normalizeContentType(head.Header.Get("Content-Type"))

	headOK := head.StatusCode >= 200 && head.StatusCode < 300
	if headOK && head.ContentLength > 0 {
		res.BytesTotal = head.ContentLength
		return res, nil
	}
	// 405/501 means "HEAD not supported"; a 2xx with no positive length means
	// "size unknown". Both are answered by sampling the body. Every other
	// non-2xx (403, 404, ...) is the origin's verdict: report it, do not fall
	// back into a GET the caller did not ask for.
	if !headOK && head.StatusCode != http.StatusMethodNotAllowed && head.StatusCode != http.StatusNotImplemented {
		return res, nil
	}
	return probeRange(ctx, cfg, httpc, res)
}

// probeRange samples the first probeSampleBytes of the entity with one
// bounded GET. A 206 answers the size via Content-Range; a 200 means the
// server ignored Range, so the whole entity was offered and only the sampled
// window was read — the note says so, because a caller that later downloads
// the body pays for all of it. A 416 answers "bytes */N", the same size
// statement for the empty-entity case (an N of 0).
func probeRange(ctx context.Context, cfg *config.Config, httpc *http.Client, res *ProbeResult) (*ProbeResult, error) {
	// Target the hop HEAD ended on, not the original URL: asking the original
	// again would walk the redirect chain twice (double traffic, and the
	// second walk can land somewhere other than what HEAD saw). The allowlist
	// still holds for this request.
	if err := HostAllowed(cfg, res.FinalURL); err != nil {
		return nil, err
	}
	req, err := probeRequest(ctx, cfg, http.MethodGet, res.FinalURL)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", probeSampleBytes-1))

	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("probe %s: %w", res.FinalURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	res.Method = "range"
	res.FinalURL = resp.Request.URL.String()
	res.Status = resp.StatusCode
	// A 206 without Content-Type must not erase the type HEAD declared.
	if ct := normalizeContentType(resp.Header.Get("Content-Type")); ct != "" {
		res.ContentType = ct
	}

	// LimitReader caps the work; the close above drops the rest of the
	// connection instead of draining a large body nobody asked for.
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, probeSampleBytes))
	res.BytesSampled = n
	if err != nil {
		return nil, fmt.Errorf("probe %s: read body: %w", res.FinalURL, err)
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		res.BytesTotal = parseContentRangeTotal(resp.Header.Get("Content-Range"))
	case http.StatusOK:
		// Range ignored: the size is whatever the server declared, if it did.
		if resp.ContentLength >= 0 {
			res.BytesTotal = resp.ContentLength
		}
		if n > 0 {
			res.Note = fmt.Sprintf("server ignored Range; sampled first %d bytes", n)
		} else {
			// Nothing was ignored: the entity itself is empty.
			res.Note = "empty entity; no bytes sampled"
		}
	default:
		// A non-206 answer can still state the size: 416 replies
		// "Content-Range: bytes */N" (an empty entity is "*/0").
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			res.BytesTotal = parseContentRangeTotal(cr)
		}
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && res.BytesTotal == 0 {
			res.Note = "empty entity (0 bytes); the range is not satisfiable"
		}
	}
	return res, nil
}

// probeRequest builds the uncredentialed request both probe legs share: the
// same identity the fetcher presents, no cookies, and identity encoding so
// Content-Length/Content-Range describe the entity rather than a
// transport-decoded stream.
func probeRequest(ctx context.Context, cfg *config.Config, method, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("probe: new request: %w", err)
	}
	req.Header.Set("Accept", "text/html, text/markdown;q=0.8, text/plain;q=0.6, */*;q=0.5")
	req.Header.Set("Accept-Encoding", "identity")
	ua := cfg.FetchUserAgent
	if ua == "" {
		ua = "velruna/probe (+pre-fetch metadata)"
	}
	req.Header.Set("User-Agent", ua)
	return req, nil
}

// parseContentRangeTotal reads the complete length out of
// "bytes 0-65535/123456". "*"—and a missing, malformed or negative value—
// means the origin did not state one: -1, never a guess.
func parseContentRangeTotal(v string) int64 {
	i := strings.LastIndexByte(v, '/')
	if i < 0 {
		return -1
	}
	total := strings.TrimSpace(v[i+1:])
	if total == "*" {
		return -1
	}
	n, err := strconv.ParseInt(total, 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// normalizeContentType reduces a Content-Type header to its bare media type:
// parameters (charset, boundary) are not part of "what kind of thing is
// this", and a probe used for routing decisions must not treat "text/html;
// charset=utf-8" and "text/html" as different answers.
func normalizeContentType(raw string) string {
	if raw == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(raw)
	if err != nil {
		// Malformed headers are common on the open internet: fall back to the
		// part before the first ';' rather than dropping the signal entirely.
		mt = strings.TrimSpace(strings.SplitN(raw, ";", 2)[0])
	}
	return strings.ToLower(mt)
}
