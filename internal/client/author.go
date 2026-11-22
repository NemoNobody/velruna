package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/user"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/model"
)

// author_source values: which link of the chain produced the label.
const (
	AuthorSourceSecurity = "security" // the store's authenticated user
	AuthorSourceConfig   = "config"   // config author / VELRUNA_AUTHOR
	AuthorSourceOS       = "os"       // the OS user running velruna
	AuthorSourceNone     = "none"     // nothing resolved
)

// AuthorInfo is the writer-label set carried by facts and journal entries
// (self-reported by design; see resolveAuthor).
type AuthorInfo struct {
	Author       string
	AuthorSource string
	Model        string
	Host         string
	Session      string
}

// authorRetryBackoff bounds how often resolveAuthor re-probes the security
// plugin after a failed probe: a long-lived MCP server should not cache a
// transient outage (plugin not up yet, network blip) as the writer identity
// forever, and it should not hammer authinfo on every write either.
const authorRetryBackoff = 5 * time.Minute

// resolveAuthor resolves the writer labels and caches them in the client:
// every caller (journal append, correction script, create stamp) reads the
// same labels. Chain: store-authenticated user (security plugin) → config
// author → OS user. A resolved security label is cached for the process
// lifetime; a degraded chain (security probe failed) is retried after
// authorRetryBackoff, so the store-authenticated name takes over as soon as
// the plugin answers.
//
// Authorship is SELF-REPORTED by design: the chain is best-effort audit
// metadata for shared stores, not an authentication factor (the threat
// model is a local/trusted contour, not an opponent with store access).
// With the security plugin off — the common single-node case — authinfo is
// unavailable and the chain falls to config/OS silently.
// authorCache is the writer-label cache for one (store, user) pair. It lives in
// the process, not on the Client: the MCP server builds a client per tool call,
// and a per-client cache would probe GET /_security/authinfo on every write.
type authorCache struct {
	mu       sync.Mutex
	resolved bool
	retryAt  time.Time
	info     AuthorInfo
}

// authorCaches holds one cache per (store URL, user).
var authorCaches sync.Map

// authorCacheFor returns the cache for a store and account: the security label
// is what authinfo answers for that account, so two configs sharing a URL but
// not a user must not share it.
func authorCacheFor(storeURL, user string) *authorCache {
	key := storeURL + "\x00" + user
	if v, ok := authorCaches.Load(key); ok {
		return v.(*authorCache)
	}
	v, _ := authorCaches.LoadOrStore(key, &authorCache{})
	return v.(*authorCache)
}

func (c *Client) resolveAuthor(ctx context.Context) AuthorInfo {
	cache := authorCacheFor(c.storeKey(), c.cfg.OpenSearchUser)
	cache.mu.Lock()
	now := time.Now()
	if cache.resolved &&
		(cache.info.AuthorSource == AuthorSourceSecurity || now.Before(cache.retryAt)) {
		defer cache.mu.Unlock()
		return cache.info
	}
	cache.mu.Unlock()
	// The chain (including the security probe, up to its own 5 s deadline)
	// runs OUTSIDE the mutex: holding it across a network round-trip would
	// serialize every writer for the probe's duration.
	// Concurrent cold starts may each pay one probe — a rare, harmless
	// duplicate GET, and the double-check below keeps the better result.
	info := c.buildAuthor(ctx)

	cache.mu.Lock()
	defer cache.mu.Unlock()
	// Double-check: a concurrent resolve may have already produced a cached
	// answer; if it found the security label, that beats our fallback chain.
	if cache.resolved && cache.info.AuthorSource == AuthorSourceSecurity && info.AuthorSource != AuthorSourceSecurity {
		return cache.info
	}
	cache.info = info
	cache.resolved = true
	if info.AuthorSource != AuthorSourceSecurity {
		cache.retryAt = time.Now().Add(authorRetryBackoff)
		if ctx.Err() != nil {
			// The probe died with the CALLER's context (an abandoned
			// request), not with the plugin: do not punish the next healthy
			// call with the full backoff.
			cache.retryAt = time.Now()
		}
	}
	return cache.info
}

// buildAuthor walks the authorship chain (security probe → config → OS
// user) without touching the cache; safe to run concurrently.
func (c *Client) buildAuthor(ctx context.Context) AuthorInfo {
	info := AuthorInfo{
		Model:   c.cfg.Model,
		Session: c.cfg.Session,
		Host:    c.cfg.Host,
	}
	if info.Host == "" {
		if h, err := os.Hostname(); err == nil {
			info.Host = sanitizeLabel(h)
		}
	}
	if u := c.securityUser(ctx); u != "" {
		info.Author, info.AuthorSource = u, AuthorSourceSecurity
	} else if c.cfg.Author != "" {
		info.Author, info.AuthorSource = c.cfg.Author, AuthorSourceConfig
	} else if u, err := user.Current(); err == nil && u.Username != "" {
		info.Author, info.AuthorSource = sanitizeLabel(u.Username), AuthorSourceOS
		if info.Author == "" {
			info.AuthorSource = AuthorSourceNone
		}
	} else {
		info.AuthorSource = AuthorSourceNone
	}
	return info
}

// AuthorLabels returns the effective authorship labels for writes made
// through this client (self-reported audit metadata, not authentication).
func (c *Client) AuthorLabels(ctx context.Context) AuthorInfo {
	return c.resolveAuthor(ctx)
}

// securityUser asks the store's security plugin who the authenticated user
// is. Every non-success means "no label": security off (404/400), missing
// privilege (403), unreachable (error) — authorship degrades down the
// chain, writes never fail on this probe. The probe carries a short
// deadline so a stalled plugin cannot slow every first write.
func (c *Client) securityUser(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// The transport rewrites scheme/host to the pool node; the path is what
	// travels.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/_security/authinfo", nil)
	if err != nil {
		return ""
	}
	resp, err := c.api.Client.Stream(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		UserName string `json:"user_name"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body) != nil {
		return ""
	}
	return sanitizeLabel(body.UserName)
}

// sanitizeLabel bounds a label that came from outside (server-provided
// usernames, OS usernames, hostnames): non-printable runes stripped — C0/C1
// control bytes, bidi overrides and zero-width format runes would forge
// terminal output in listings (they land in get/journal/status output),
// over-long labels dropped whole (a truncated name reads as a real name; a
// rejected one reads as absent). The config edge enforces the same length
// bound on config-supplied labels.
func sanitizeLabel(v string) string {
	s := strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(v))
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > config.LabelMaxRunes {
		return ""
	}
	return s
}

// applyTo stamps the entry with the writer labels: every journal entry names
// its own writer (the journal is a log; "who wrote this version" is a
// per-entry fact).
func (a AuthorInfo) applyTo(e *model.JournalEntry) {
	e.Author, e.AuthorSource = a.Author, a.AuthorSource
	e.Model, e.Host, e.Session = a.Model, a.Host, a.Session
}

// stampFact sets the birth labels on a new fact (write-once: who/what/where
// created it). Corrections never rewrite these; they move updated_by only.
func (a AuthorInfo) stampFact(f *model.Fact) {
	f.Author, f.AuthorSource = a.Author, a.AuthorSource
	f.Model, f.Host, f.Session = a.Model, a.Host, a.Session
}

// confirmer turns this writer into one verification of a fact's current value:
// the user label and its source (identity), the machine (a multi-user store
// identifies a confirmer as user + host) and the moment.
func (a AuthorInfo) confirmer(at time.Time) model.Confirmer {
	return model.Confirmer{Author: a.Author, AuthorSource: a.AuthorSource, Host: a.Host, At: at.UTC()}
}
