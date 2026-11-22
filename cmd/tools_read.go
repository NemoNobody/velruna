package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/model"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type searchInput struct {
	Query          string `json:"query" jsonschema:"text to look for in fact values, notes and summaries (keys are not searched)"`
	Project        string `json:"project,omitempty" jsonschema:"limit results to this project"`
	Category       string `json:"category,omitempty" jsonschema:"limit results to this category"`
	Keyword        string `json:"keyword,omitempty" jsonschema:"limit results to facts carrying this exact tag (keywords are stored as lowercase single tokens)"`
	Status         string `json:"status,omitempty" jsonschema:"limit results to this status: verified (default), obsolete, or any"`
	Limit          int    `json:"limit,omitempty" jsonschema:"max hits to return (default 10, cap 50): the context budget for search results"`
	IncludeHistory bool   `json:"include_history,omitempty" jsonschema:"include the full history chain (default: current value only); ships only on full rows (compact=false)"`
	// Compact defaults to true (the velruna_list rule): a search hit is
	// navigation — the summary is the table of contents, the full value is
	// one velruna_get away. compact=false restores full rows.
	Compact *bool `json:"compact,omitempty" jsonschema:"compact rows (no value/authorship; notes truncated): DEFAULT true — velruna_get reads the value; compact=false restores full rows (~5x the tokens)"`
}

func (s *mcpServer) search(ctx context.Context, req *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, factsOutput, error) {
	// Shape before store contact (the search_docs contract): an empty query
	// answers a silent zero — navigation is what list/keys are for.
	if strings.TrimSpace(in.Query) == "" {
		return nil, factsOutput{}, errors.New("velruna_search: query is required")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	// Write edges store the lowercase canonical category; the term filter is
	// case-sensitive, so canonicalize the filter too. Project labels are
	// stored trimmed (write edges); the term filter is whitespace-exact, so
	// the filter trims too — " proj " must not answer a silent zero.
	in.Category = strings.ToLower(strings.TrimSpace(in.Category))
	in.Project = strings.TrimSpace(in.Project)
	// An unknown category filter answers a silent zero forever (the term is
	// exact on the stored canonical): refuse the typo at the edge instead.
	if in.Category != "" && !model.ValidCategory(in.Category) {
		return nil, factsOutput{}, fmt.Errorf("velruna_search: unknown category %q (filters match the stored canonical: location | config | endpoint | behavior | context_map | recipe | rule)", in.Category)
	}
	status, serr := resolveStatus(in.Status)
	if serr != nil {
		return nil, factsOutput{}, serr
	}
	// A keyword filter is a term on the stored canonical tags: normalize the
	// same way the write edge does, or " Perf " answers a silent zero.
	keyword := ""
	if in.Keyword != "" {
		kw, kerr := model.NormalizeKeywords([]string{in.Keyword})
		if kerr != nil {
			return nil, factsOutput{}, fmt.Errorf("velruna_search: %v", kerr)
		}
		if len(kw) == 0 {
			return nil, factsOutput{}, errors.New("velruna_search: keyword must not be blank")
		}
		keyword = kw[0]
	}
	compact := true
	if in.Compact != nil {
		compact = *in.Compact
	}
	// Shape before store contact (the list/keys contract): an invalid status
	// must not pay for a client build and an index ensure.
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, factsOutput{}, storeError("velruna_search", err)
	}
	facts, meta, err := c.Search(ctx, in.Query, in.Project, in.Category, status, keyword, limit)
	if err != nil {
		return nil, factsOutput{}, storeError("velruna_search", err)
	}
	out := factsOutput{
		Count:     len(facts),
		Truncated: meta.Total > len(facts)+meta.Skipped,
		Skipped:   meta.Skipped,
		Facts:     newFactViews(facts, in.IncludeHistory, compact),
	}
	if out.Count == 0 {
		// A bare 0 invites URL-style guessing and tool churn; name the next
		// right move instead.
		out.Hint = "0 hits: this search reads values/notes/summaries, not keys — for key lookup use velruna_keys (prefix/match); for a project overview velruna_list; try shorter terms"
	} else if compact {
		// Compact is the default now: say where the full value lives.
		out.Hint = "compact rows: velruna_get(key) reads the full value; compact=false restores full rows"
	}
	return nil, out, nil
}

type getInput struct {
	Key            string   `json:"key,omitempty" jsonschema:"exact fact key (single-key contract)"`
	Keys           []string `json:"keys,omitempty" jsonschema:"batch lookup: up to 10 exact keys; returns items[] with found per key (mutually exclusive with key)"`
	IncludeHistory bool     `json:"include_history,omitempty" jsonschema:"include the full history chain (default: current value only)"`
}

type getOutput struct {
	// Found is the single-key answer; it is a pointer so batch mode stays
	// clean: getOutput{Items: ...} used to emit a top-level "found": false
	// that means nothing there (every item carries its own flag).
	Found *bool     `json:"found,omitempty" jsonschema:"false when no fact has this key (single-key mode only; absent in batch mode — items carry their own found flag)"`
	Fact  *factView `json:"fact,omitempty" jsonschema:"the fact, when found (single-key mode)"`
	Items []itemGet `json:"items,omitempty" jsonschema:"batch mode (keys param): one item per requested key, in request order"`
}

// boolPtr exists for one reason: getOutput.Found must be omittable without
// conflating "absent" with "false".
func boolPtr(b bool) *bool { return &b }

// itemGet is one batch-lookup result: found=false is an answer, not an error.
type itemGet struct {
	Key   string    `json:"key" jsonschema:"the requested key"`
	Found bool      `json:"found" jsonschema:"false when no fact has this key"`
	Fact  *factView `json:"fact,omitempty" jsonschema:"the fact, when found"`
}

func (s *mcpServer) get(ctx context.Context, req *mcp.CallToolRequest, in getInput) (*mcp.CallToolResult, getOutput, error) {
	// Validate the shape before any store contact: exactly one of key/keys.
	// Keys are trimmed at this edge: the store holds trimmed keys, and a
	// lookup/echo on " k " would miss the fact it names.
	in.Key = strings.TrimSpace(in.Key)
	for i, k := range in.Keys {
		in.Keys[i] = strings.TrimSpace(k)
	}
	// Shape first (exactly one of key/keys, then the cap): those refusals
	// describe the call as a whole and must not be preempted by a per-key
	// complaint. Only then the per-key non-empty check — every key validated
	// before any store contact: a keys list carrying " " must fail without
	// touching the store.
	hasKey := in.Key != ""
	if hasKey == (len(in.Keys) > 0) {
		return nil, getOutput{}, fmt.Errorf("velruna_get: provide exactly one of key or keys (up to %d)", maxGetKeys)
	}
	if len(in.Keys) > maxGetKeys {
		return nil, getOutput{}, fmt.Errorf("velruna_get: keys is capped at %d (context economy)", maxGetKeys)
	}
	for i, k := range in.Keys {
		if k == "" {
			return nil, getOutput{}, fmt.Errorf("velruna_get: empty key in keys at index %d", i)
		}
	}
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, getOutput{}, storeError("velruna_get", err)
	}
	if len(in.Keys) > 0 {
		items := make([]itemGet, 0, len(in.Keys))
		for _, k := range in.Keys {
			f, err := c.Get(ctx, k)
			if errors.Is(err, client.ErrNotFound) {
				items = append(items, itemGet{Key: k, Found: false})
				continue
			}
			if err != nil {
				return nil, getOutput{}, storeError("velruna_get", err)
			}
			v := newFactView(*f, in.IncludeHistory, false)
			items = append(items, itemGet{Key: k, Found: true, Fact: &v})
		}
		return nil, getOutput{Items: items}, nil
	}
	f, err := c.Get(ctx, in.Key)
	if errors.Is(err, client.ErrNotFound) {
		// Not an error: "there is no such fact" is a legitimate answer.
		return nil, getOutput{Found: boolPtr(false)}, nil
	}
	if err != nil {
		return nil, getOutput{}, storeError("velruna_get", err)
	}
	v := newFactView(*f, in.IncludeHistory, false)
	return nil, getOutput{Found: boolPtr(true), Fact: &v}, nil
}

type listInput struct {
	Project        string `json:"project" jsonschema:"project name to list facts for (required, never blank; velruna_keys navigates across projects)"`
	Prefix         string `json:"prefix,omitempty" jsonschema:"keep only keys starting with this prefix (trailing * allowed), e.g. '<project>:state:'"`
	Status         string `json:"status,omitempty" jsonschema:"limit results to this status: verified (default), obsolete, or any"`
	IncludeHistory bool   `json:"include_history,omitempty" jsonschema:"include the full history chain (default: current value only)"`
	// Compact defaults to true: list is a table of contents, and full values
	// for a whole project are the single most expensive agent call
	// (observed ~15k tokens for 32 facts). compact=false restores the full
	// view; a listed fact's full text is one velruna_get away.
	Compact *bool `json:"compact,omitempty" jsonschema:"compact rows: key/summary/category/status/source/notes-head/age (DEFAULT true); false = full values, ~5x the tokens"`
	// Sort defaults to updated: the dominant list use-case is session start
	// ("what moved since last time"); velruna_keys covers key-order navigation.
	Sort string `json:"sort,omitempty" jsonschema:"'updated' (default: newest activity first; a touch moves updated_at, not age/stale) or 'key' (stable navigation order)"`
}

func (s *mcpServer) list(ctx context.Context, req *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, factsOutput, error) {
	// Project labels are stored trimmed; the term filter is whitespace-exact.
	in.Project = strings.TrimSpace(in.Project)
	// Shape before store contact: a blank project would run term{"project":""}
	// — a silent zero dressed up as "no data" (the keys/search edges refuse
	// malformed shapes the same way).
	if in.Project == "" {
		return nil, factsOutput{}, errors.New("velruna_list: project is required (velruna_keys navigates across projects)")
	}
	// The prefix normalizes before any store contact (the keys contract):
	// a bad prefix is the caller's problem, and its shape error must not come
	// back wrapped in the availability advice — or never come back at all when
	// the store is unreachable. NormalizePrefix is idempotent, so the store
	// sees exactly what was checked.
	normPrefix, pErr := client.NormalizePrefix(in.Prefix)
	if pErr != nil {
		return nil, factsOutput{}, fmt.Errorf("velruna_list: %w", pErr)
	}
	in.Prefix = normPrefix
	compact := true
	if in.Compact != nil {
		compact = *in.Compact
	}
	sort := strings.ToLower(strings.TrimSpace(in.Sort))
	if sort == "" {
		sort = "updated"
	}
	if sort != "updated" && sort != "key" {
		return nil, factsOutput{}, fmt.Errorf("velruna_list: sort must be updated|key, got %q", in.Sort)
	}
	// Shape before store contact (the keys/search edges validate the same
	// way): an invalid status must not pay for a full navigation query
	// before failing. The filter itself stays client-side — List has no
	// server-side status term — but validation does not wait for it.
	status, serr := resolveStatus(in.Status)
	if serr != nil {
		return nil, factsOutput{}, serr
	}
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, factsOutput{}, storeError("velruna_list", err)
	}
	facts, meta, err := c.List(ctx, in.Project, in.Prefix, sort)
	if err != nil {
		return nil, factsOutput{}, storeError("velruna_list", err)
	}
	// The truncation signal is the cluster-side total: the status filter
	// below shrinks the page, and truncation must survive the filtering.
	// rawLen >= cap used to fire on an exactly-full page (a complete result).
	rawLen := meta.Total
	if status != "" {
		kept := facts[:0]
		for _, f := range facts {
			if f.Status == status {
				kept = append(kept, f)
			}
		}
		facts = kept
	}
	out := factsOutput{Count: len(facts), Truncated: rawLen > client.NavSizeCap, Skipped: meta.Skipped, Facts: newFactViews(facts, in.IncludeHistory, compact)}
	if out.Count == 0 {
		// Same contract as search: a bare 0 invites guessing; name the next
		// right move. The status filter runs client-side, so an all-obsolete
		// project legitimately lands here at the verified default.
		out.Hint = "0 facts: the default hides obsolete facts — try status:'any'; drop or widen the prefix; velruna_keys navigates keys across projects; a brand-new project is a legitimate 0"
	}
	return nil, out, nil
}

type keysInput struct {
	Prefix  string `json:"prefix,omitempty" jsonschema:"key prefix, e.g. 'proj:foo:loc:'; trailing * allowed"`
	Match   string `json:"match,omitempty" jsonschema:"key substring; * and ? work as wildcards"`
	Project string `json:"project,omitempty" jsonschema:"limit to this project (default: all projects)"`
	Status  string `json:"status,omitempty" jsonschema:"limit results to this status: verified (default), obsolete, or any"`
}

// keys is the key-level navigation tool: prefix/match on the key field, with
// project as an optional filter. The empty-query guard lives in the client so
// CLI and MCP share one contract.
func (s *mcpServer) keys(ctx context.Context, req *mcp.CallToolRequest, in keysInput) (*mcp.CallToolResult, factsOutput, error) {
	// Validate the query shape before any store contact: a bad query is the
	// caller's problem, not an availability problem. The match is trimmed the
	// same way the prefix is: store keys are trimmed, and " k " as a wildcard
	// over trimmed keys answers a silent zero.
	in.Match = strings.TrimSpace(in.Match)
	in.Project = strings.TrimSpace(in.Project)
	// Normalize the prefix before the empty-query check: " " and "*"
	// normalize to "" and must count as "no query given" — otherwise the
	// call reaches the store and its shape error comes back wrapped in the
	// availability advice ("do not retry"), contradicting the contract.
	normPrefix, pErr := client.NormalizePrefix(in.Prefix)
	if pErr != nil {
		return nil, factsOutput{}, fmt.Errorf("velruna_keys: %w", pErr)
	}
	if normPrefix == "" && in.Match == "" {
		return nil, factsOutput{}, fmt.Errorf("velruna_keys: provide a key prefix and/or a match pattern")
	}
	status, serr := resolveStatus(in.Status)
	if serr != nil {
		return nil, factsOutput{}, serr
	}
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, factsOutput{}, storeError("velruna_keys", err)
	}
	facts, meta, err := c.Keys(ctx, normPrefix, in.Match, in.Project, status)
	if err != nil {
		return nil, factsOutput{}, storeError("velruna_keys", err)
	}
	// Keys is key-level navigation: the rows are scanned, not read — the
	// compact view is always the right shape here (no parameter: the full
	// value of a key-matched fact is a velruna_get away, the history is a
	// velruna_journal away; an include_history knob would be a no-op in
	// compact rows, so the schema does not offer it).
	return nil, factsOutput{Count: len(facts), Truncated: meta.Total > client.NavSizeCap, Skipped: meta.Skipped, Facts: newFactViews(facts, false, true)}, nil
}

// --- journal (history log index) ---

type journalInput struct {
	Key   string `json:"key" jsonschema:"exact fact key"`
	Limit int    `json:"limit,omitempty" jsonschema:"newest entries to return (default 20, cap 1000)"`
	Since string `json:"since,omitempty" jsonschema:"only entries at or after this RFC3339 timestamp (UTC), e.g. 2026-11-10T00:00:00Z — answers \"what changed since last session\""`
	Until string `json:"until,omitempty" jsonschema:"only entries at or before this RFC3339 timestamp (UTC)"`
}

type journalEntryView struct {
	FactKey      string            `json:"fact_key" jsonschema:"the fact key this entry observes"`
	FactID       string            `json:"fact_id,omitempty" jsonschema:"stable fact identity the entry belongs to"`
	Project      string            `json:"project,omitempty" jsonschema:"project of the entry"`
	Category     string            `json:"category,omitempty" jsonschema:"category at the time of the entry"`
	Op           string            `json:"op" jsonschema:"write (an accepted value) | rediscovery (a re-confirmation of the same value) | retire (verified→obsolete transition)"`
	Value        string            `json:"value" jsonschema:"the accepted value at that moment"`
	Notes        string            `json:"notes,omitempty" jsonschema:"notes shipped with the write"`
	Source       string            `json:"source,omitempty" jsonschema:"source shipped with the write"`
	Keywords     []string          `json:"keywords,omitempty" jsonschema:"tag set as of this accepted value (a tag-only change is a touch, so the previous set stays in the last entry that carried it)"`
	Redactions   []model.Redaction `json:"redactions,omitempty" jsonschema:"spans the write-time secret guard replaced in this version (see velruna_add)"`
	CreatedAt    time.Time         `json:"created_at" jsonschema:"writer-side observation time (UTC)"`
	Author       string            `json:"author,omitempty" jsonschema:"who appended this entry (self-reported label)"`
	AuthorSource string            `json:"author_source,omitempty" jsonschema:"security | config | os | none for this writer"`
	Model        string            `json:"model,omitempty" jsonschema:"model label of this writer"`
	Host         string            `json:"host,omitempty" jsonschema:"host label of this writer"`
	Session      string            `json:"session,omitempty" jsonschema:"agent session label of this writer"`
}

func newJournalEntryViews(entries []model.JournalEntry) []journalEntryView {
	views := make([]journalEntryView, 0, len(entries))
	for _, e := range entries {
		views = append(views, journalEntryView{
			FactKey: e.FactKey, FactID: e.FactID, Project: e.Project, Category: e.Category,
			Op: e.Op, Value: e.Value, Notes: e.Notes, Source: e.Source, CreatedAt: e.CreatedAt,
			Keywords:   e.Keywords,
			Redactions: e.Redactions,
			Author:     e.Author, AuthorSource: e.AuthorSource, Model: e.Model,
			Host: e.Host, Session: e.Session,
		})
	}
	return views
}

type journalOutput struct {
	Key       string             `json:"key" jsonschema:"the queried key"`
	Entries   []journalEntryView `json:"entries" jsonschema:"journal entries, newest first (store-clock order)"`
	Total     int                `json:"total" jsonschema:"total entries for the key (lower bound after admin index deletes)"`
	Truncated bool               `json:"truncated" jsonschema:"true when more entries exist than returned (raise limit)"`
	Skipped   int                `json:"skipped,omitempty" jsonschema:"hits whose content failed to decode (index corruption): report to the user, raising the limit does not recover them"`
}

func (s *mcpServer) journal(ctx context.Context, req *mcp.CallToolRequest, in journalInput) (*mcp.CallToolResult, journalOutput, error) {
	in.Key = strings.TrimSpace(in.Key)
	if in.Key == "" {
		return nil, journalOutput{}, errors.New("velruna_journal: key is required")
	}
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, journalOutput{}, storeError("velruna_journal", err)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	since, serr := parseJournalBound("since", in.Since)
	if serr != nil {
		return nil, journalOutput{}, serr
	}
	until, uerr := parseJournalBound("until", in.Until)
	if uerr != nil {
		return nil, journalOutput{}, uerr
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return nil, journalOutput{}, errors.New("velruna_journal: since is after until")
	}
	entries, meta, err := c.JournalEntries(ctx, client.JournalQuery{
		Key: in.Key, Limit: limit, Since: since, Until: until,
	})
	if err != nil {
		return nil, journalOutput{}, storeError("velruna_journal", err)
	}
	if entries == nil {
		entries = []model.JournalEntry{}
	}
	return nil, journalOutput{Key: in.Key, Entries: newJournalEntryViews(entries), Total: meta.Total, Truncated: meta.Truncated, Skipped: meta.Skipped}, nil
}

type historyInfoInput struct {
	Key string `json:"key" jsonschema:"exact fact key"`
}

type historyInfoOutput struct {
	Key           string     `json:"key" jsonschema:"the fact key"`
	Writes        int        `json:"writes" jsonschema:"accepted values journaled (changes = writes-1); LOWER BOUND after admin index deletes"`
	Rediscoveries int        `json:"rediscoveries" jsonschema:"same-value re-confirmations journaled"`
	FirstAt       *time.Time `json:"first_at,omitempty" jsonschema:"earliest surviving entry (UTC)"`
	LastAt        *time.Time `json:"last_at,omitempty" jsonschema:"newest surviving entry (UTC)"`
	Category      string     `json:"category,omitempty" jsonschema:"current category of the fact"`
	CurrentRef    string     `json:"current_ref,omitempty" jsonschema:"journal entry id carrying the current value"`
}

func (s *mcpServer) historyInfo(ctx context.Context, req *mcp.CallToolRequest, in historyInfoInput) (*mcp.CallToolResult, historyInfoOutput, error) {
	in.Key = strings.TrimSpace(in.Key)
	if in.Key == "" {
		return nil, historyInfoOutput{}, errors.New("velruna_history_info: key is required")
	}
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, historyInfoOutput{}, storeError("velruna_history_info", err)
	}
	info, err := c.HistoryInfo(ctx, in.Key)
	if err != nil {
		return nil, historyInfoOutput{}, storeError("velruna_history_info", err)
	}
	return nil, historyInfoOutput{
		Key: info.Key, Writes: info.Writes, Rediscoveries: info.Rediscoveries,
		FirstAt: info.FirstAt, LastAt: info.LastAt,
		Category: info.Category, CurrentRef: info.CurrentRef,
	}, nil
}
