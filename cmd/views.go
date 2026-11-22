package cmd

import (
	"strings"
	"time"

	"github.com/NemoNobody/velruna/internal/model"
)

// ── store tools ──────────────────────────────────────────────────────────────

// historyView is one past observation in the MCP-facing shape.
type historyView struct {
	Value      string `json:"value" jsonschema:"the earlier observation"`
	ObservedAt string `json:"observed_at" jsonschema:"RFC3339 when this observation was seen"`
	Source     string `json:"source" jsonschema:"where this observation came from"`
}

// confirmerView is one verification of a fact's current value.
type confirmerView struct {
	Author       string `json:"author" jsonschema:"self-reported label of the confirmer (security user, config label or OS user)"`
	AuthorSource string `json:"author_source,omitempty" jsonschema:"security | config | os | none — which chain produced the label"`
	Host         string `json:"host,omitempty" jsonschema:"machine the confirmation came from (user + host identifies a confirmer in a shared store)"`
	At           string `json:"at" jsonschema:"RFC3339 when the confirmation happened"`
}

// newConfirmerViews maps stored confirmations for the MCP contract.
func newConfirmerViews(in []model.Confirmer) []confirmerView {
	if len(in) == 0 {
		return nil
	}
	out := make([]confirmerView, 0, len(in))
	for _, c := range in {
		out = append(out, confirmerView{
			Author: c.Author, AuthorSource: c.AuthorSource, Host: c.Host, At: rfc3339OrEmpty(c.At),
		})
	}
	return out
}

// factView is the MCP-facing shape of a fact: a stable contract for clients
// (RFC3339 timestamps, no internal fields) rather than the internal model.
// In compact mode most fields carry omitempty so the JSON drops them: the
// compact view is a table of contents (key, summary, state, age), the full
// value is one velruna_get away.
type factView struct {
	Key          string            `json:"key" jsonschema:"stable fact key, e.g. proj:foo:loc:bar.py"`
	Value        string            `json:"value,omitempty" jsonschema:"the fact itself (compact mode: only a 100-rune head when the fact has no summary)"`
	Category     string            `json:"category" jsonschema:"location | config | endpoint | behavior | context_map | recipe | rule"`
	Project      string            `json:"project" jsonschema:"project name, or 'global'"`
	Status       string            `json:"status" jsonschema:"verified | obsolete"`
	Source       string            `json:"source" jsonschema:"where it came from, e.g. tool:read_file, doc:README, user:confirmed"`
	Summary      string            `json:"summary,omitempty" jsonschema:"short table-of-contents rendering of the value; required for values over summary_min_len"`
	SupersededBy string            `json:"superseded_by,omitempty" jsonschema:"key of the fact that replaces this one: the successor a reader should trust instead (one link per fact, set by velruna_patch; the target is verified and the chain is cycle-free)"`
	Author       string            `json:"author,omitempty" jsonschema:"who recorded the fact (self-reported label, not authentication); the author_source field names the chain that produced it"`
	AuthorSource string            `json:"author_source,omitempty" jsonschema:"security | config | os | none — which chain link named the author"`
	UpdatedBy    string            `json:"updated_by,omitempty" jsonschema:"self-reported label of the last writer; the birth author fields are write-once"`
	Notes        string            `json:"notes,omitempty" jsonschema:"qualifiers: what was checked, how far, caveats (compact mode: first 100 runes)"`
	Keywords     []string          `json:"keywords,omitempty" jsonschema:"normalized tags (lowercase single tokens); the keyword filter of velruna_search matches these exactly"`
	Redactions   []model.Redaction `json:"redactions,omitempty" jsonschema:"spans the write-time secret guard replaced before storing (field, pattern, chars — never the text): the stored value is SHORTER than the one written, so a reader must not take it for the complete claim"`
	ObservedAt   string            `json:"observed_at" jsonschema:"RFC3339 when the current value was observed"`
	TTLDays      int               `json:"ttl_days,omitempty" jsonschema:"per-fact staleness override in days, when the writer chose one (otherwise the category default: config/endpoint 14, location/behavior 30, recipe 180, rule 90, context_map never)"`
	AgeDays      int               `json:"age_days" jsonschema:"whole days since the observation; -1 when observed_at is unknown"`
	AgeHours     int               `json:"age_hours" jsonschema:"whole hours since the observation; -1 when observed_at is unknown (0 age_days can mean 1 hour or 23 — hours disambiguate fresh facts)"`
	Stale        bool              `json:"stale" jsonschema:"true when age passes the effective TTL (the per-fact ttl_days override when set, otherwise the category default: config/endpoint 14d, location/behavior 30d, recipe 180d, rule 90d, context_map never) — re-verify against the source before trusting"`
	History      []historyView     `json:"history,omitempty" jsonschema:"previous observations, oldest first; present only when include_history=true"`
	ConfirmedBy  []confirmerView   `json:"confirmed_by,omitempty" jsonschema:"who has verified the CURRENT value, creator first (author+host identifies the confirmer; at is when). A value change resets the set; the journal has every confirmation event"`
	CreatedAt    string            `json:"created_at,omitempty" jsonschema:"RFC3339 creation time (dropped in compact mode)"`
	UpdatedAt    string            `json:"updated_at" jsonschema:"RFC3339 last update time; a same-value touch moves it (age/stale do not)"`
}

// rfc3339OrEmpty formats t for the MCP contract, or returns "" when t is the
// zero time — facts created before observed_at existed carry no date.
func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// truncRunes keeps the first n runes of s, marking a real cut with an
// ellipsis. Compact views trade text for context: a reader that needs the
// rest calls velruna_get.
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n]), " \t\n") + "…"
}

// newFactView renders a fact for the MCP contract. History ships only when
// asked for: corrections are frequent (state keys get re-observed daily) and
// shipping every past observation bloats the agent's context on every read.
// Staleness is computed at read time — the store provides the evidence
// (observed_at), the signal (stale/age_days) tells the agent to re-verify.
//
// compact keeps the fields an agent navigates by (key, summary, category,
// status, source, notes head, age/stale, updated_at) and drops the bulk
// (value, authorship labels, created_at, history): full facts are ~5x the
// tokens of a compact one, and list/search over a whole project is the most
// expensive call in a session. A fact without a summary keeps a 100-rune
// head of its value so it is not an empty row. History is a full-row-only
// option: compact rows are navigation; velruna_journal reads the history.
func newFactView(f model.Fact, includeHistory, compact bool) factView {
	v := factView{
		Key:          f.Key,
		Value:        f.Value,
		Category:     f.Category,
		Project:      f.Project,
		Status:       f.Status,
		Source:       f.Source,
		Summary:      f.Summary,
		SupersededBy: f.SupersededBy,
		Author:       f.Author,
		AuthorSource: f.AuthorSource,
		UpdatedBy:    f.UpdatedBy,
		Notes:        f.Notes,
		Keywords:     f.Keywords,
		Redactions:   f.Redactions,
		ConfirmedBy:  newConfirmerViews(f.ConfirmedBy),
		ObservedAt:   rfc3339OrEmpty(f.ObservedAt),
		TTLDays:      f.TTLDays,
		AgeDays:      f.AgeDays(time.Now().UTC()),
		AgeHours:     f.AgeHours(time.Now().UTC()),
		Stale:        f.Stale(time.Now().UTC()),
		CreatedAt:    rfc3339OrEmpty(f.CreatedAt),
		UpdatedAt:    rfc3339OrEmpty(f.UpdatedAt),
	}
	if compact {
		v.Author, v.AuthorSource, v.UpdatedBy, v.CreatedAt = "", "", "", ""
		v.Notes = truncRunes(v.Notes, 100)
		if v.Summary == "" {
			v.Value = truncRunes(v.Value, 100)
		} else {
			v.Value = ""
		}
	}
	if includeHistory && !compact {
		history := make([]historyView, 0, len(f.History))
		for _, h := range f.History {
			history = append(history, historyView{
				Value:      h.Value,
				ObservedAt: rfc3339OrEmpty(h.ObservedAt),
				Source:     h.Source,
			})
		}
		v.History = history
	}
	return v
}

func newFactViews(facts []model.Fact, includeHistory, compact bool) []factView {
	views := make([]factView, 0, len(facts))
	for _, f := range facts {
		views = append(views, newFactView(f, includeHistory, compact))
	}
	return views
}

// factsOutput is the list-shaped result shared by velruna_search and velruna_list.
type factsOutput struct {
	Count int        `json:"count" jsonschema:"number of facts returned"`
	Facts []factView `json:"facts" jsonschema:"matching facts (order: search = relevance; list = sort parameter, updated by default; keys = by key)"`
	// Hint is navigation advice for the zero-hit case: an agent that gets a
	// bare 0 starts guessing; a pointer to the right tool ends the loop.
	Hint string `json:"hint,omitempty" jsonschema:"present when count=0: which tool/shape to try next"`
	// Truncated is the honest page signal: the store matched more than the
	// page delivered. For list/keys the page is the navigation cap (1000),
	// for search it is the requested limit — the advice differs, and an
	// agent told to "narrow the query" when the limit cut the output would
	// answer from a subset it cannot see.
	Truncated bool `json:"truncated,omitempty" jsonschema:"true when the store matched more facts than this page (the requested limit for search; the navigation cap 1000 for list/keys): for search raise the limit (max 50) before narrowing, for list/keys narrow the query (prefix/project/status); absent means complete"`
	Skipped   int  `json:"skipped,omitempty" jsonschema:"hits the store returned whose content failed to decode (index corruption): report to the user, never silently drop"`
}
