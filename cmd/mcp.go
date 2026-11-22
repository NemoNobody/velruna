package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/model"
	"github.com/NemoNobody/velruna/internal/version"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

var mcpFlags struct {
	conf string
}

// McpCmd returns the `velruna mcp` command: an MCP server speaking over stdio.
func McpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run as an MCP server over stdio (for agents: VS Code and others)",
		Long: `Run as an MCP server over stdio.

The server exposes the facts store to MCP clients (VS Code, agents) as tools.
It speaks JSON-RPC on stdin/stdout, so it is normally started by the client,
not by hand. Diagnostics go to stderr.

Config resolution: --conf flag, then VELRUNA_CONF, then next to the binary,
then ~/.velruna.conf. The velruna.conf in the working directory is ignored on
purpose: the editor starts this server from the cwd of an arbitrary project,
and a cloned repository's config must not hijack the store endpoint.`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			if err := runMCPServer(mcpFlags.conf); err != nil {
				_, _ = io.WriteString(os.Stderr, "velruna mcp: "+err.Error()+"\n")
				os.Exit(1)
			}
		},
	}
	cmd.Flags().StringVar(&mcpFlags.conf, "conf", "",
		"config file to use (overrides VELRUNA_CONF and the default lookup)")
	return cmd
}

// mcpServer holds the state shared by all tool handlers.
//
// The config is loaded lazily on every call on purpose: the server must start
// even when OpenSearch is unreachable or the config file is broken. A server
// that dies on startup is invisible to the client, so it cannot even report
// the problem. Instead, every tool describes the failure in actionable terms.
type mcpServer struct {
	confPath string
}

// runMCPServer builds the server and serves it over stdio until the client
// disconnects.
func runMCPServer(confPath string) error {
	s := &mcpServer{confPath: confPath}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "velruna",
		Version: version.Version,
	}, &mcp.ServerOptions{
		Instructions: "Verified facts about the user's projects (file locations, " +
			"config and endpoint values, behavior). Consult facts before " +
			"re-verifying something the user may have already confirmed, and store " +
			"newly verified facts so they are not re-derived. Never store secrets " +
			"(passwords, tokens) — the store is plaintext. Call velruna_status first " +
			"if you are unsure whether the store is available.",
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_status",
		Description: "Check whether the facts store (OpenSearch) is reachable and " +
			"report the effective configuration, plus storage state: cluster and " +
			"index health, disk usage of the busiest data node, and whether the index " +
			"is read-only. Never fails: returns ok=false plus an explanation when the " +
			"store is unavailable, so you can tell the user what to fix instead of " +
			"retrying.",
	}, s.status)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_search",
		Description: "Search stored facts by text. Looks in fact values, notes and summaries " +
			"only — it does NOT match keys, so use velruna_get for an exact key " +
			"(velruna_keys navigates keys by pattern). By default returns only " +
			"trusted ('verified') facts; obsolete facts are superseded and must not " +
			"be treated as current. History ships only on full rows " +
			"(include_history=true with compact=false); velruna_journal is the " +
			"history tool. " +
			"compact is TRUE by default (rows scan cheap; velruna_get reads the value); compact=false restores full values. " +
			"Answers carry age_days/age_hours/stale: a stale fact " +
			"must be re-verified against the source before being trusted.",
	}, s.search)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_get",
		Description: "Get facts by exact key: key=... for one fact (found=false when " +
			"there is no such fact — that is not an error), keys=[...] for a batch of " +
			"up to 10 (items[] with per-key found). History is omitted unless " +
			"include_history=true. Answers carry age_days/age_hours/stale: a stale fact must be " +
			"re-verified against the source before being trusted.",
	}, s.get)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_add",
		Description: "Store a verified fact: file locations, config and endpoint " +
			"values, behavior. Re-use the same key to correct a fact — the " +
			"accepted value is appended to the history journal and the fact " +
			"points at it. Re-adding the same value is a notes/source touch: no " +
			"journal entry, observed_at stays (staleness keeps aging — a re-read " +
			"is not a re-verification), status returns to verified " +
			"(resurrects an obsolete fact), updated_by moves. The answer says " +
			"what happened: created/changed (changed=false is a same-value " +
			"touch) and previous_summary names the superseded version. Values longer " +
			"than summary_min_len " +
			"require a summary (kept under summary_max_len): it is the " +
			"store's table of contents. For cheap partial updates (dated note, " +
			"status flip) use velruna_patch — no value resend. Never store " +
			"secrets (passwords, tokens, keys): the store is plaintext and the " +
			"secret_guard refuses credential shapes and redacts heuristic hits " +
			"(the answer lists redactions) on writes.",
	}, s.add)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_add_batch",
		Description: "Store 1..10 facts in one call (the session-end ritual: " +
			"open-items, git-state, ... in a single round-trip). Per-item guard " +
			"and confirm_secret (one approved false positive never whitewashes " +
			"the rest); duplicate keys are rejected; partial success is legal — " +
			"the per-item answer says created/changed/error for each. Same " +
			"semantics as velruna_add.",
	}, s.addBatch)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_patch",
		Description: "Partial update of an EXISTING fact (never creates — use " +
			"velruna_add): change value/summary/source, replace notes, append a " +
			"dated note line (notes_append — the cheap qualifier update, atomic " +
			"server-side), flip status (verified|obsolete; a transition is " +
			"journaled, obsolete→verified sets observed_at=now; reason goes with " +
			"obsolete). Omitted fields are untouched. Journal semantics match " +
			"velruna_add: value changes and status transitions append entries, " +
			"notes/summary/source moves are touches. On a value change the old " +
			"summary is CLEARED unless you pass a new one (a stale table of " +
			"contents is a lie). superseded_by sets the successor link: the fact " +
			"key that replaces this one, which a reader should trust instead " +
			"(one link per fact, never repointed; the target must exist and be " +
			"verified; cycles refused). The link is journaled as op=supersede.",
	}, s.patch)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_patch_batch",
		Description: "Patch 1..10 existing facts in one call (the session-end " +
			"ritual when the facts already exist: dated notes, status flips, " +
			"git-state corrections in a single round-trip). Per-item guard and " +
			"confirm_secret (one approved false positive never whitewashes the " +
			"rest); duplicate keys are rejected; partial success is legal — the " +
			"per-item answer says changed/status/error for each. Same semantics as " +
			"velruna_patch.",
	}, s.patchBatch)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_list",
		Description: "List the facts stored for a project. Defaults to trusted " +
			"('verified') facts only. prefix keeps only keys starting with it " +
			"(trailing * allowed; keys conventionally start with the project " +
			"name, e.g. prefix '<project>:state:'). COMPACT by default: key/summary/" +
			"category/status/notes-head/age rows (a project listing is the most " +
			"expensive agent call otherwise — full values cost ~5x); compact=false " +
			"restores full rows, velruna_get reads any single fact. SORT by default " +
			"updated (newest activity first: 'what moved since last session'; note " +
			"a same-value touch moves updated_at without changing age/stale); " +
			"sort=key for stable navigation. History ships only on full rows " +
			"(include_history: true with compact: false); velruna_journal is the " +
			"history tool. A zero-hit answer carries a hint naming the next " +
			"right move.",
	}, s.list)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_keys",
		Description: "Find facts by key across all projects: prefix (key start, " +
			"e.g. 'proj:foo:loc:') and/or match (key substring; * and ? wildcards). " +
			"At least one is required; results are sorted by key. The key-level " +
			"navigation tool — use it to scope before velruna_list/get; project is " +
			"an optional extra filter. Rows are compact (key/summary/age: " +
			"navigation is scanned, not read). Defaults to trusted ('verified') " +
			"facts only.",
	}, s.keys)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_prune",
		Description: "Drop old history entries under a key, keeping the newest " +
			"'keep' (the current value is never touched). DESTRUCTIVE: dropped " +
			"observations are gone for good. Without confirm=true nothing is " +
			"written — the call returns the plan. Show the plan to the user and " +
			"re-call with confirm=true only after their explicit approval.",
	}, s.prune)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_obsolete",
		Description: "Mark a fact obsolete because its subject is gone (file " +
			"deleted, endpoint removed). Use velruna_add when the value " +
			"changed, velruna_patch to flip status — the transition is journaled " +
			"either way. The value is kept for reference; the reason is appended " +
			"to notes.",
	}, s.obsolete)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_journal",
		Description: "Read the journal for a key: the append-only history of " +
			"accepted values (newest first, bounded by limit; changes = writes-1). " +
			"op=write is an accepted value; op=rediscovery is a re-confirmation of " +
			"the same value (velruna_patch reverify); op=retire marks a " +
			"verified→obsolete transition; op=supersede records a superseded_by " +
			"link (a relation between records, not a value change). Counts after an " +
			"admin deleted old journal indices are lower bounds. Use " +
			"velruna_history_info first for the cheap overview, the full " +
			"journal only when the history matters.",
	}, s.journal)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_history_info",
		Description: "Compact per-key history overview: writes, rediscoveries, " +
			"first/last timestamp, current_ref — without the entry bodies. " +
			"Answers 'did this fact change, how often, when was the last time' " +
			"for fast scouting before a full journal read; a context-cheap " +
			"summary for reports and trust judgments. Counts are LOWER BOUNDS " +
			"(admin index pruning is invisible to the app).",
	}, s.historyInfo)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_fetch_doc",
		Description: "Fetch a web page, store its snapshot in the docs index as " +
			"blocks (headings/sections), and return the outline (hash, title, " +
			"sections). max_chars caps the stored text in runes (0 = the whole " +
			"page; default fetch_doc_chars), block_chars sets the target block size " +
			"(1000..64000; default fetch_block_chars); the budget drops whole " +
			"blocks, never cuts one. A second fetch of the " +
			"same URL returns the stored snapshot (cached=true) until its TTL " +
			"expires; refresh=true forces a new download. block_chars applies when " +
			"the page is extracted: a cached snapshot with all its text keeps its " +
			"stored split (a revalidating 304 included), while a truncated snapshot " +
			"re-extracted for a larger max_chars is re-blocked at the requested " +
			"size when that budget stores more text. Section text is read with " +
			"velruna_get_doc; the raw page is never returned (context economy). " +
			"Requests send no cookies and no credentials; a host outside " +
			"fetch_allow_hosts is rejected before the request. GitHub URLs that " +
			"match a local clone resolve to a local path (local=true) with no " +
			"network and no store write. Bot-walled sites (403/422/429) may be " +
			"retried through the opt-in obscura renderer (via=obscura).",
	}, s.fetchDoc)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_get_doc",
		Description: "Read a stored document snapshot by hash: the outline by " +
			"default, section=N for one block or a run of them (blocks=N, default " +
			"1, max 10), full=true for every block. One answer is capped at " +
			"32000 runes of block text; when the cap cuts the read, next names " +
			"the section to continue from. The outline in the answer is capped too " +
			"(sections_hidden counts the blocks it left out, hint summarizes their " +
			"range); block_chars is applied at fetch/re-extraction, so a cached " +
			"snapshot keeps its own split unless more text was requested. Prefer one section: full bloats the " +
			"context. Never touches the " +
			"network: stale=true means the snapshot is past its TTL — refresh it " +
			"with velruna_fetch_doc before quoting.",
	}, s.getDoc)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_search_docs",
		Description: "BM25 search over stored page snapshots (section text only; " +
			"the raw html is write-only and never searched). Returns hash+url+title+" +
			"project per doc, with the MATCHED section (order, heading, a snippet " +
			"centered on the match): velruna_get_doc(hash, section=order) reads that " +
			"section in full. Complements velruna_search, which searches verified " +
			"facts, not pages.",
	}, s.searchDocs)

	err := srv.Run(context.Background(), &mcp.StdioTransport{})
	if errors.Is(err, io.EOF) {
		// Normal shutdown: the client closed the connection.
		return nil
	}
	return err
}

// statusInput is empty: velruna_status takes no arguments.
type statusInput struct{}

// status reports connectivity and effective configuration. It is deliberately
// error-free: it returns ok=false instead of failing, so an agent can relay the
// problem to the user in words. The payload is shared with `velruna status`,
// but resolved through the MCP NoCWD chain: the report must describe the
// config the store tools actually use, never a cwd file the tools ignore.
func (s *mcpServer) status(ctx context.Context, req *mcp.CallToolRequest, _ statusInput) (*mcp.CallToolResult, statusReport, error) {
	return nil, collectStatus(ctx, s.confPath, true), nil
}

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

// loadConfig resolves the config for a tool call: the explicit --conf wins;
// without one the working directory is ignored (the editor opens MCP with
// the cwd of an arbitrary, possibly untrusted, project — its velruna.conf
// must not hijack the endpoint).
func (s *mcpServer) loadConfig() (*config.Config, error) {
	if s.confPath != "" {
		return config.LoadConfigFrom(s.confPath)
	}
	return config.LoadConfigNoCWD()
}

// newClient returns a client for the configured store. Config is reloaded on
// every call so that a broken file or an unreachable server cannot kill the
// server process (see mcpServer). The index is ensured like the CLI does.
func (s *mcpServer) newClient(ctx context.Context) (*client.Client, error) {
	c, _, err := s.newClientWithConfig(ctx)
	return c, err
}

// clientFromConfig builds the client for an already-loaded config, so a tool
// that needs config values (the guard, prune's default keep) loads the config
// once: the keyring behind a locked wallet is prompted at most once per call.
func (s *mcpServer) clientFromConfig(ctx context.Context, cfg *config.Config) (*client.Client, error) {
	c, err := client.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if err := c.EnsureIndex(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// newClientWithConfig is newClient plus the effective config, for tools that
// need config values beyond the connection (velruna_prune's default keep).
func (s *mcpServer) newClientWithConfig(ctx context.Context) (*client.Client, *config.Config, error) {
	cfg, err := s.loadConfig()
	if err != nil {
		return nil, nil, err
	}
	c, err := s.clientFromConfig(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return c, cfg, nil
}

// storeError marks a failure as an availability problem rather than an empty
// result, so an agent reports it instead of treating "nothing" as the answer.
func storeError(op string, err error) error {
	return fmt.Errorf("%s failed: %w. If the facts store is unreachable, call "+
		"velruna_status and tell the user; do not retry in a loop", op, err)
}

// writeFailure classifies a failed write at the MCP edge. A local contract
// rejection (summary bounds, re-retiring) never reached the store: wrapping
// it in availability advice would send the agent chasing velruna_status
// while the request itself is what needs fixing. A CAS conflict is neither:
// the client's message already orders "re-read and retry", and store
// advice ("do not retry") would contradict it in the other direction.
func writeFailure(op string, err error) error {
	if errors.Is(err, client.ErrContract) || errors.Is(err, client.ErrConflict) {
		return err
	}
	return storeError(op, err)
}

// resolveStatus maps the status input to a filter. Empty means "trusted facts
// only" — obsolete facts are superseded and must not be mistaken for current;
// "any" disables the filter. Statuses are stored lowercase, so the input is
// canonicalized; an unknown spelling is a loud error, because a case-sensitive
// term filter silently matching zero facts reads to the agent as "no data".
func resolveStatus(status string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(status))
	switch s {
	case "":
		return "verified", nil
	case "any":
		return "", nil
	case "verified", "obsolete":
		return s, nil
	default:
		return "", fmt.Errorf("unknown status %q: use verified, obsolete, or any", status)
	}
}

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

// maxGetKeys and maxAddBatchItems cap the batch tools: a batch is a
// round-trip saving, not a context flood.
const (
	maxGetKeys       = 10
	maxAddBatchItems = 10
)

type addInput struct {
	Key      string   `json:"key" jsonschema:"stable fact key, e.g. proj:foo:loc:bar.py — re-use the same key to update"`
	Value    string   `json:"value" jsonschema:"the fact text"`
	Category string   `json:"category" jsonschema:"location | config | endpoint | behavior | context_map | recipe | rule"`
	Project  string   `json:"project,omitempty" jsonschema:"project name; defaults to 'global'"`
	Source   string   `json:"source,omitempty" jsonschema:"where the fact came from (tool:read_file, doc:README, user:confirmed); defaults to tool:mcp"`
	Notes    string   `json:"notes,omitempty" jsonschema:"qualifiers: what was checked, how far, caveats — carried to later sessions"`
	Keywords []string `json:"keywords,omitempty" jsonschema:"tags for cross-project slices and filtered search (normalized to lowercase single tokens, max 10, deduplicated); an empty list leaves the stored tags alone on a same-value re-add"`
	TTLDays  int      `json:"ttl_days,omitempty" jsonschema:"optional staleness window in days, set it only when you know this fact rots faster than its category default; it may only shorten the window (cap: config/endpoint 14, location/behavior 30, recipe 180, rule 90; context_map uncapped)"`
	Summary  string   `json:"summary,omitempty" jsonschema:"short table-of-contents rendering of the value (under summary_max_len chars); required when the value is longer than summary_min_len; not applied on same-value re-adds, but an over-cap summary is still refused at every edge"`

	// ConfirmSecret bypasses a guard hit at secret_guard=ask. It is the agent's
	// barrier: show the masked finding to the user first, set this only after
	// their explicit approval. At reject it has no effect.
	ConfirmSecret bool `json:"confirm_secret,omitempty" jsonschema:"confirm a guard hit is a false positive (secret_guard=ask only); set only after the user approved the exact finding"`
}

type addOutput struct {
	Key     string `json:"key" jsonschema:"the stored key"`
	Created bool   `json:"created" jsonschema:"true when the fact was newly created, false when an existing fact was updated"`
	Changed bool   `json:"changed" jsonschema:"true when a new version was accepted (birth, value correction); false = same-value touch: notes/source moved, observed_at and the journal untouched"`
	Status  string `json:"status" jsonschema:"status of the stored fact"`
	// PreviousSummary names the superseded version at zero journal cost:
	// the summary the accepted correction replaced ("" when the prior fact
	// had none). It is what closes the "did I replace the critical value or
	// just touch it?" ambiguity.
	PreviousSummary string `json:"previous_summary,omitempty" jsonschema:"summary of the value this write replaced (empty when the fact was created fresh or the prior version had no summary)"`
	// Note explains the least-obvious answer (a same-value touch): without
	// it, created=false+changed=false reads like a no-op and agents mis-age
	// the fact.
	Note string `json:"note,omitempty" jsonschema:"what exactly this write did when the shape alone does not say (same-value touch: observed_at and age keep counting, notes/source moved only; resurrect: an obsolete fact re-declared alive)"`
	// Redactions names every span the secret guard replaced before storing
	// (field, detector pattern, size — never the text). Non-empty means the
	// stored value is SHORTER than the one written: the flag is not noise, it
	// tells the caller the claim lost a piece.
	Redactions []model.Redaction `json:"redactions,omitempty" jsonschema:"spans the secret guard replaced before storing (field, pattern, chars); the stored value is shorter than the one written"`
}

func (s *mcpServer) add(ctx context.Context, req *mcp.CallToolRequest, in addInput) (*mcp.CallToolResult, addOutput, error) {
	cfg, cerr := s.loadConfig()
	if cerr != nil {
		return nil, addOutput{}, storeError("velruna_add", cerr)
	}
	out, err := s.addOne(ctx, cfg, nil, nil, in)
	if err != nil {
		return nil, addOutput{}, err
	}
	return nil, out, nil
}

// addOne is the add contract for one item: validate → guard → write. The
// config arrives loaded (the guard and the client share it; a locked wallet
// is not prompted twice), so velruna_add_batch runs the identical pipeline
// per item without re-reading config per call.
// addOne is the shared write path of velruna_add and velruna_add_batch.
// c is the shared client (a batch reuses one connection and one EnsureIndex);
// nil means "create one for this single call". cErr is a client-creation
// failure the batch already paid for: the item runs validation and guard and
// reports it WITHOUT a store attempt (a slow store must not cost the batch
// ten timeouts and ten identical per-item client creations). The key is
// trimmed at this edge: "k " and "k" must not become two docs with
// diverging history.
func (s *mcpServer) addOne(ctx context.Context, cfg *config.Config, c *client.Client, cErr error, in addInput) (addOutput, error) {
	// Validate the shape before guard and store: an empty key produces a
	// cryptic store error, and an unknown category never ages (silent rot).
	in.Key = strings.TrimSpace(in.Key)
	if in.Key == "" {
		return addOutput{}, errors.New("velruna_add: key is required")
	}
	if strings.TrimSpace(in.Value) == "" {
		return addOutput{}, errors.New("velruna_add: value is required")
	}
	if !model.ValidCategory(strings.ToLower(in.Category)) {
		return addOutput{}, fmt.Errorf("velruna_add: unknown category %q (want location | config | endpoint | behavior | context_map | recipe | rule)", in.Category)
	}
	// Canonicalize for storage: the TTL table and the search category filter
	// are case-sensitive, so a mixed-case write would silently never age and
	// never match category filters.
	in.Category = strings.ToLower(in.Category)
	in.Project = strings.TrimSpace(in.Project)
	if in.Project == "" {
		in.Project = "global"
	}
	if in.Source == "" {
		in.Source = "tool:mcp"
	}
	// The guard runs before any store contact: a rejected write must be
	// rejected even when OpenSearch is down. The project label is agent-supplied
	// identity text stored in the shared index (the fetch path gates it the same
	// way) — the default "global" is inert.
	//
	// Identity fields (key, project, source, tags) can only refuse: rewriting a
	// span there would silently change what the fact IS. Text fields may come
	// back shortened (a medium-confidence heuristic hit is replaced, not fatal),
	// and the audit of what was cut travels with the stored fact.
	kwForGuard := strings.Join(in.Keywords, " ")
	redactions, gErr := guardSanitize(cfg.SecretGuard, in.ConfirmSecret,
		identityField("key", &in.Key),
		textField("value", &in.Value),
		identityField("project", &in.Project),
		textField("notes", &in.Notes),
		textField("summary", &in.Summary),
		identityField("keywords", &kwForGuard),
		identityField("source", &in.Source))
	if gErr != nil {
		return addOutput{}, gErr
	}
	if cErr != nil {
		return addOutput{}, cErr
	}
	if c == nil {
		var cerr error
		c, cerr = s.clientFromConfig(ctx, cfg)
		if cerr != nil {
			return addOutput{}, storeError("velruna_add", cerr)
		}
	}
	f := model.NewFact(in.Key, in.Value, in.Category, in.Project, in.Source, in.Notes)
	f.Summary = in.Summary
	f.TTLDays = in.TTLDays
	f.Redactions = redactions
	// Normalized at the edge as well as in the client: the error must name the
	// tool call, and client.Add re-normalizes for the CLI path.
	kw, kwErr := model.NormalizeKeywords(in.Keywords)
	if kwErr != nil {
		return addOutput{}, fmt.Errorf("velruna_add: %v", kwErr)
	}
	f.Keywords = kw
	res, err := c.Add(ctx, f)
	if err != nil {
		// Partial success (the fact doc landed, the journal/link follow-up
		// failed): Add reports what landed in res. The single velruna_add edge
		// cannot surface the flags — go-sdk discards the typed output when the
		// handler returns an error, so the honest state travels in the error
		// text — but velruna_add_batch copies these flags into its per-item
		// output next to item.Error, where they are visible. Do not "simplify"
		// this away: the batch answer would silently zero what did store.
		if res.Created || res.Changed {
			return addOutput{Key: in.Key, Created: res.Created, Changed: res.Changed, Status: f.Status, Redactions: redactions}, writeFailure("velruna_add", err)
		}
		return addOutput{}, writeFailure("velruna_add", err)
	}
	out := addOutput{Key: in.Key, Created: res.Created, Changed: res.Changed,
		Status: f.Status, PreviousSummary: res.PreviousSummary, Redactions: redactions}
	if !res.Created && !res.Changed {
		out.Note = "same-value touch: observed_at and the journal untouched, the fact keeps aging (re-verify when stale); notes/source moved only"
		if in.Summary != "" {
			// A passed summary would rewrite the CURRENT value's table of
			// contents without a new version — silently misleading in a
			// shared store. It is dropped; say so instead of hiding it.
			out.Note += "; the passed summary was ignored on a same-value touch (correct the value to change its summary)"
		}
	} else if res.Resurrected {
		out.Note = "resurrected: the fact was obsolete; status is back to verified, observed_at moved, and the event is journaled"
	}
	return out, nil
}

// addBatchInput carries add-shaped items; confirm_secret is per item, so a
// user-approved false positive on one fact never whitewashes the next.
type addBatchInput struct {
	Items []addInput `json:"items" jsonschema:"1..10 add-shaped items (key, value, category, ...); duplicate keys are rejected"`
}

type addBatchOutput struct {
	Count int `json:"count" jsonschema:"number of items processed"`
	// Summary counters: the whole-session ritual without re-reading items.
	Created int            `json:"created" jsonschema:"items newly created"`
	Changed int            `json:"changed" jsonschema:"items that accepted a new version (includes created)"`
	Touched int            `json:"touched" jsonschema:"items touched on the same value (notes/source only)"`
	Items   []addBatchItem `json:"items" jsonschema:"per-item outcome in request order; a failed item does not roll back the others"`
}

type addBatchItem struct {
	Key     string `json:"key" jsonschema:"the item's key"`
	Created bool   `json:"created" jsonschema:"see velruna_add"`
	Changed bool   `json:"changed" jsonschema:"see velruna_add"`
	Status  string `json:"status,omitempty" jsonschema:"status of the stored fact"`
	// Error carries the item's rejection (guard hit, validation, store
	// failure). Partial success is legal: OpenSearch writes are per-doc, and
	// a batch transaction would be a fiction.
	Error string `json:"error,omitempty" jsonschema:"this item's failure (guard/validation/store); other items were processed regardless"`
	// Redactions is velruna_add's audit per item: the stored value lost a
	// flagged span. The batch answer must carry it — the per-item outcome is
	// the only place a caller can see that a write landed shortened.
	Redactions []model.Redaction `json:"redactions,omitempty" jsonschema:"spans the secret guard replaced before storing (see velruna_add)"`
}

func (s *mcpServer) addBatch(ctx context.Context, req *mcp.CallToolRequest, in addBatchInput) (*mcp.CallToolResult, addBatchOutput, error) {
	if len(in.Items) == 0 || len(in.Items) > maxAddBatchItems {
		return nil, addBatchOutput{}, fmt.Errorf("velruna_add_batch: items must be 1..%d", maxAddBatchItems)
	}
	// Duplicate keys in one batch are ambiguous by construction: the second
	// item silently corrects the first (order-dependent created/changed and
	// summary requirements). Reject the shape instead of picking a winner —
	// on the TRIMMED key, which is what addOne stores.
	seen := make(map[string]bool, len(in.Items))
	for _, it := range in.Items {
		k := strings.TrimSpace(it.Key)
		if seen[k] {
			return nil, addBatchOutput{}, fmt.Errorf("velruna_add_batch: duplicate key %q in one batch", k)
		}
		seen[k] = true
	}
	cfg, cerr := s.loadConfig()
	if cerr != nil {
		return nil, addBatchOutput{}, storeError("velruna_add_batch", cerr)
	}
	// One client for the batch: a shared connection and a single EnsureIndex
	// (the per-item version made the "one round-trip" tool ten round-trips).
	c, cErr := s.clientFromConfig(ctx, cfg)
	if cErr != nil {
		cErr = storeError("velruna_add_batch", cErr)
	}
	out := addBatchOutput{Items: make([]addBatchItem, 0, len(in.Items))}
	for i, it := range in.Items {
		res, err := s.addOne(ctx, cfg, c, cErr, it)
		// The echo carries the TRIMMED key: it is what the store holds, and a
		// caller reconciling answers against the store must not miss on " k ".
		item := addBatchItem{Key: strings.TrimSpace(it.Key), Created: res.Created, Changed: res.Changed, Status: res.Status, Redactions: res.Redactions}
		if err != nil {
			// Label the item, not the single-add tool: an agent reading a
			// batch answer must not conclude velruna_add itself broke.
			item.Error = "velruna_add_batch item " + strconv.Itoa(i) + ": " + strings.TrimPrefix(err.Error(), "velruna_add: ")
		}
		// The counters read what LANDED, not only what completed cleanly: a
		// partial success stored the fact (item.Error names the lagged
		// follow-up), and a summary that contradicts the per-item truth is
		// exactly the audit lie the counters exist to prevent.
		if res.Created {
			out.Created++
		}
		if res.Changed {
			out.Changed++
		}
		if err == nil && !res.Changed {
			out.Touched++
		}
		out.Items = append(out.Items, item)
	}
	out.Count = len(out.Items)
	// A store-wide failure (config/client) surfaces as per-item errors: the
	// batch itself answers "what happened to each write", which is the point.
	return nil, out, nil
}

type patchInput struct {
	Key          string   `json:"key" jsonschema:"exact fact key of an EXISTING fact (patch never creates: use velruna_add)"`
	Value        string   `json:"value,omitempty" jsonschema:"new value: journaled, observed_at moves; the summary resets with it (pass a new summary for a long value)"`
	Summary      string   `json:"summary,omitempty" jsonschema:"new table of contents (with a value change: omitting it CLEARS the old summary — an old summary for a new value is a lie)"`
	Notes        string   `json:"notes,omitempty" jsonschema:"replaces notes (non-empty = provided, add's convention)"`
	NotesAppend  string   `json:"notes_append,omitempty" jsonschema:"append a dated line 'YYYY-MM-DD: text' to notes (atomic server-side; the cheap qualifier update — no need to resend the value)"`
	Source       string   `json:"source,omitempty" jsonschema:"replaces source when non-empty"`
	Status       string   `json:"status,omitempty" jsonschema:"verified | obsolete: a transition is journaled; obsolete→verified sets observed_at=now (re-declaring alive is a fresh verification)"`
	Reason       string   `json:"reason,omitempty" jsonschema:"why it became obsolete (only with status=obsolete; folded into notes)"`
	Reverify     bool     `json:"reverify,omitempty" jsonschema:"re-confirm the current value: observed_at moves to now, the journal gains op=rediscovery, the writer joins confirmed_by; cannot be combined with value/summary/status"`
	TTLDays      int      `json:"ttl_days,omitempty" jsonschema:"retune the staleness window in days (bounded by the category cap: config/endpoint 14, location/behavior 30, recipe 180, rule 90; context_map uncapped); omitted = untouched; a qualifier touch, not journaled"`
	Keywords     []string `json:"keywords,omitempty" jsonschema:"replaces the tag set when non-empty (same normalization as add: lowercase single tokens, max 10); a qualifier touch, not journaled — the tags appear in the history with the next accepted value"`
	SupersededBy string   `json:"superseded_by,omitempty" jsonschema:"key of the fact that replaces this one (the successor to trust instead): set once, never repointed; the target must exist and be verified and the chain must stay cycle-free; journaled as op=supersede"`

	// ConfirmSecret bypasses a guard hit at secret_guard=ask (see addInput).
	ConfirmSecret bool `json:"confirm_secret,omitempty" jsonschema:"confirm a guard hit is a false positive (secret_guard=ask only); set only after the user approved the exact finding"`
}

type patchOutput struct {
	Key        string `json:"key" jsonschema:"the patched key"`
	Changed    bool   `json:"changed" jsonschema:"true = a journaled event landed (new value, status transition, or a re-confirmation); false = qualifier-only touch"`
	Status     string `json:"status" jsonschema:"status of the fact after the patch"`
	Reverified bool   `json:"reverified,omitempty" jsonschema:"true when the patch re-confirmed the value in place (observed_at moved, op=rediscovery journaled, value unchanged)"`
	Note       string `json:"note,omitempty" jsonschema:"set on a qualifier-only touch: observed_at and age untouched, the journal not appended"`
	// Redactions: see velruna_add — the stored value lost a flagged span.
	Redactions []model.Redaction `json:"redactions,omitempty" jsonschema:"spans the secret guard replaced before storing (see velruna_add)"`
}

func (s *mcpServer) patch(ctx context.Context, req *mcp.CallToolRequest, in patchInput) (*mcp.CallToolResult, patchOutput, error) {
	// Shape first: a malformed patch is the caller's problem, answered before
	// config, guard and any store contact (the list/search/keys contract).
	if strings.TrimSpace(in.Key) == "" {
		return nil, patchOutput{}, errors.New("velruna_patch: key is required")
	}
	if err := client.ValidatePatchFields(patchFields(in)); err != nil {
		return nil, patchOutput{}, err
	}
	cfg, cerr := s.loadConfig()
	if cerr != nil {
		return nil, patchOutput{}, storeError("velruna_patch", cerr)
	}
	out, err := s.patchOne(ctx, cfg, nil, nil, in)
	if err != nil {
		return nil, patchOutput{}, err
	}
	return nil, out, nil
}

// patchFields maps the MCP input onto the client's patch shape (one place,
// shared by the single and batch paths).
func patchFields(in patchInput) client.PatchFields {
	return client.PatchFields{
		Value:        in.Value,
		Summary:      in.Summary,
		Notes:        in.Notes,
		NotesAppend:  in.NotesAppend,
		Source:       in.Source,
		Status:       in.Status,
		Reason:       in.Reason,
		TTLDays:      in.TTLDays,
		Keywords:     in.Keywords,
		Reverify:     in.Reverify,
		SupersededBy: in.SupersededBy,
	}
}

// patchOne is the shared write path of velruna_patch and velruna_patch_batch
// (the addOne contract): validate → guard → write. c is the shared client
// (a batch reuses one connection); nil means "create one". cErr is a
// client-creation failure the batch already paid for: the item still runs
// validation and guard and reports the failure WITHOUT a store attempt.
func (s *mcpServer) patchOne(ctx context.Context, cfg *config.Config, c *client.Client, cErr error, in patchInput) (patchOutput, error) {
	in.Key = strings.TrimSpace(in.Key)
	if in.Key == "" {
		return patchOutput{}, errors.New("velruna_patch: key is required")
	}
	// Shape first, on the text as written: a malformed patch is the caller's
	// problem and must be answered before the guard rewrites anything.
	if err := client.ValidatePatchFields(patchFields(in)); err != nil {
		return patchOutput{}, err
	}
	// Every provided field is user-supplied text on a write path — the guard
	// covers all of them, notes_append and reason included (a secret can
	// arrive in a dated note as easily as in a value). Same split as add:
	// identity fields refuse, text fields may come back shortened.
	kwForGuard := strings.Join(in.Keywords, " ")
	redactions, gErr := guardSanitize(cfg.SecretGuard, in.ConfirmSecret,
		identityField("key", &in.Key),
		textField("value", &in.Value),
		textField("summary", &in.Summary),
		textField("notes", &in.Notes),
		textField("notes_append", &in.NotesAppend),
		identityField("source", &in.Source),
		identityField("keywords", &kwForGuard),
		identityField("superseded_by", &in.SupersededBy),
		textField("reason", &in.Reason))
	if gErr != nil {
		return patchOutput{}, gErr
	}
	fields := patchFields(in)
	fields.Redactions = redactions
	if cErr != nil {
		return patchOutput{}, cErr
	}
	if c == nil {
		var cerr error
		c, cerr = s.clientFromConfig(ctx, cfg)
		if cerr != nil {
			return patchOutput{}, storeError("velruna_patch", cerr)
		}
	}
	res, err := c.Patch(ctx, in.Key, fields)
	if errors.Is(err, client.ErrNotFound) {
		return patchOutput{}, fmt.Errorf("velruna_patch: no fact with key %q: nothing to patch (velruna_add creates)", in.Key)
	}
	if err != nil {
		// Partial success (the patch landed, the journal follow-up lagged):
		// Patch reports what landed in res. velruna_patch_batch copies the
		// flags into its per-item output; do not drop them (see addOne's note —
		// the single edge cannot show them, the batch edge must).
		if res.Changed || res.Status != "" {
			return patchOutput{Key: in.Key, Changed: res.Changed, Status: res.Status, Reverified: res.Reverified}, writeFailure("velruna_patch", err)
		}
		return patchOutput{}, writeFailure("velruna_patch", err)
	}
	out := patchOutput{Key: in.Key, Changed: res.Changed, Status: res.Status, Reverified: res.Reverified, Redactions: redactions}
	if !res.Changed {
		out.Note = "qualifier-only touch: observed_at and the journal untouched, the fact keeps aging (re-verify when stale)"
	}
	return out, nil
}

// patchBatchInput carries patch-shaped items; confirm_secret is per item
// (the add_batch rule): one approved false positive never whitewashes the
// next item.
type patchBatchInput struct {
	Items []patchInput `json:"items" jsonschema:"1..10 patch-shaped items (key, value/notes_append/status, ...); duplicate keys are rejected"`
}

type patchBatchOutput struct {
	Count int `json:"count" jsonschema:"number of items processed"`
	// Summary counters (the add_batch rule): what the batch actually did.
	Changed    int              `json:"changed" jsonschema:"items that journaled an event (value change, status transition, re-confirmation)"`
	Reverified int              `json:"reverified,omitempty" jsonschema:"subset of changed: items that re-confirmed the value in place"`
	Touched    int              `json:"touched" jsonschema:"items patched on qualifiers only (no journal entry)"`
	Items      []patchBatchItem `json:"items" jsonschema:"per-item outcome in request order; a failed item does not roll back the others"`
}

type patchBatchItem struct {
	Key        string            `json:"key" jsonschema:"the item's key"`
	Changed    bool              `json:"changed" jsonschema:"see velruna_patch"`
	Reverified bool              `json:"reverified,omitempty" jsonschema:"see velruna_patch"`
	Status     string            `json:"status,omitempty" jsonschema:"status of the fact after the patch"`
	Error      string            `json:"error,omitempty" jsonschema:"this item's failure (guard/validation/store); other items were processed regardless"`
	Redactions []model.Redaction `json:"redactions,omitempty" jsonschema:"spans the secret guard replaced before storing (see velruna_add)"`
}

func (s *mcpServer) patchBatch(ctx context.Context, req *mcp.CallToolRequest, in patchBatchInput) (*mcp.CallToolResult, patchBatchOutput, error) {
	if len(in.Items) == 0 || len(in.Items) > maxAddBatchItems {
		return nil, patchBatchOutput{}, fmt.Errorf("velruna_patch_batch: items must be 1..%d", maxAddBatchItems)
	}
	// Duplicate keys are ambiguous in a patch batch too: the second item
	// patches the first item's fresh state (order-dependent journaled
	// transitions). Reject the shape — on the TRIMMED key.
	seen := make(map[string]bool, len(in.Items))
	for _, it := range in.Items {
		k := strings.TrimSpace(it.Key)
		if seen[k] {
			return nil, patchBatchOutput{}, fmt.Errorf("velruna_patch_batch: duplicate key %q in one batch", k)
		}
		seen[k] = true
	}
	cfg, cerr := s.loadConfig()
	if cerr != nil {
		return nil, patchBatchOutput{}, storeError("velruna_patch_batch", cerr)
	}
	c, cErr := s.clientFromConfig(ctx, cfg)
	if cErr != nil {
		cErr = storeError("velruna_patch_batch", cErr)
	}
	out := patchBatchOutput{Items: make([]patchBatchItem, 0, len(in.Items))}
	for i, it := range in.Items {
		res, err := s.patchOne(ctx, cfg, c, cErr, it)
		item := patchBatchItem{Key: strings.TrimSpace(it.Key), Changed: res.Changed, Status: res.Status, Reverified: res.Reverified, Redactions: res.Redactions}
		if err != nil {
			// Batch label, same as velruna_add_batch: the item failed, not
			// the single-patch tool.
			item.Error = "velruna_patch_batch item " + strconv.Itoa(i) + ": " + strings.TrimPrefix(err.Error(), "velruna_patch: ")
		}
		// Counters read what landed (the add_batch rule): a partial success
		// did change the fact; the error names its lagged follow-up.
		if res.Changed {
			out.Changed++
			if res.Reverified {
				out.Reverified++
			}
		} else if err == nil {
			out.Touched++
		}
		out.Items = append(out.Items, item)
	}
	out.Count = len(out.Items)
	return nil, out, nil
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

type obsoleteInput struct {
	Key    string `json:"key" jsonschema:"key of the fact whose subject is gone"`
	Reason string `json:"reason,omitempty" jsonschema:"why it is gone (file deleted, endpoint removed); appended to notes"`

	// ConfirmSecret bypasses a guard hit at secret_guard=ask (see addInput).
	ConfirmSecret bool `json:"confirm_secret,omitempty" jsonschema:"confirm a guard hit is a false positive (secret_guard=ask only); set only after the user approved the exact finding"`
}

type obsoleteOutput struct {
	Key    string `json:"key" jsonschema:"the key that was marked obsolete"`
	Status string `json:"status" jsonschema:"the new status"`
	// Redactions: see velruna_add — the stored reason text lost a flagged span.
	Redactions []model.Redaction `json:"redactions,omitempty" jsonschema:"spans the secret guard replaced before storing (see velruna_add)"`
}

func (s *mcpServer) obsolete(ctx context.Context, req *mcp.CallToolRequest, in obsoleteInput) (*mcp.CallToolResult, obsoleteOutput, error) {
	// The config is the guard's policy: a load failure must fail the write
	// (as in add), never silently skip the scan of a credential-like reason.
	// Loaded once and reused: a locked wallet is not prompted twice per write.
	in.Key = strings.TrimSpace(in.Key)
	if in.Key == "" {
		return nil, obsoleteOutput{}, errors.New("velruna_obsolete: key is required")
	}
	cfg, cerr := s.loadConfig()
	if cerr != nil {
		return nil, obsoleteOutput{}, storeError("velruna_obsolete", cerr)
	}
	// The CLI edge scans key+reason; the MCP edge must not be the narrower
	// gate (a credential-shaped key reaches the shared index via MCP only).
	// Key is identity (rename it), the reason is text: a heuristic hit there is
	// redacted and the retirement proceeds, the same policy as add/patch.
	redactions, gErr := guardSanitize(cfg.SecretGuard, in.ConfirmSecret,
		identityField("key", &in.Key),
		textField("reason", &in.Reason))
	if gErr != nil {
		return nil, obsoleteOutput{}, gErr
	}
	c, err := s.clientFromConfig(ctx, cfg)
	if err != nil {
		return nil, obsoleteOutput{}, storeError("velruna_obsolete", err)
	}
	err = c.Obsolete(ctx, in.Key, in.Reason, redactions)
	if errors.Is(err, client.ErrNotFound) {
		return nil, obsoleteOutput{}, fmt.Errorf("no fact with key %q: nothing to mark obsolete", in.Key)
	}
	if err != nil {
		return nil, obsoleteOutput{}, writeFailure("velruna_obsolete", err)
	}
	return nil, obsoleteOutput{Key: in.Key, Status: "obsolete", Redactions: redactions}, nil
}

type pruneInput struct {
	Key     string `json:"key" jsonschema:"key of the fact whose history to prune"`
	Keep    int    `json:"keep,omitempty" jsonschema:"number of newest history entries to keep; 0 = history_limit from config"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"must be true to write; without it only the plan is returned"`
}

type pruneOutput struct {
	Key          string `json:"key" jsonschema:"the pruned key"`
	HistoryTotal int    `json:"history_total" jsonschema:"history entries before the call"`
	Keep         int    `json:"keep" jsonschema:"history entries that survive"`
	Removed      int    `json:"removed" jsonschema:"entries deleted (0 when nothing was written)"`
	Pruned       bool   `json:"pruned" jsonschema:"true when history was actually rewritten"`
	Plan         string `json:"plan,omitempty" jsonschema:"no-write preview: what confirm=true would destroy. Relay it to the user and re-call with confirm=true only after their approval"`
}

// prune is destructive, so it carries the same barrier as the CLI: without
// confirm=true the plan is returned and nothing is written. An agent must
// relay the plan to the user and re-call only after explicit approval.
func (s *mcpServer) prune(ctx context.Context, req *mcp.CallToolRequest, in pruneInput) (*mcp.CallToolResult, pruneOutput, error) {
	in.Key = strings.TrimSpace(in.Key)
	if in.Key == "" {
		return nil, pruneOutput{}, errors.New("velruna_prune: key is required")
	}
	cfg, cerr := s.loadConfig()
	if cerr != nil {
		return nil, pruneOutput{}, storeError("velruna_prune", cerr)
	}
	// keep resolves from local config BEFORE any store contact: the refusal
	// needs no store, and it must not pay for an index ensure.
	keep := in.Keep
	if keep < 0 {
		// A negative keep used to collapse silently into the configured
		// default: the operator asked to prune and got a different plan.
		return nil, pruneOutput{}, fmt.Errorf("velruna_prune: keep must not be negative (got %d); pass keep=0 for the configured history_limit", in.Keep)
	}
	if keep <= 0 {
		keep = cfg.HistoryLimit
	}
	if keep <= 0 {
		// history_limit: 0 means "keep everything": silently collapsing it
		// to 1 would destroy every history entry. Demand an explicit keep.
		return nil, pruneOutput{}, fmt.Errorf("velruna_prune: history_limit is 0 (keep everything) and no keep value was given: pass an explicit keep (e.g. keep: 5)")
	}
	c, err := s.clientFromConfig(ctx, cfg)
	if err != nil {
		return nil, pruneOutput{}, storeError("velruna_prune", err)
	}
	f, err := c.Get(ctx, in.Key)
	if errors.Is(err, client.ErrNotFound) {
		return nil, pruneOutput{}, fmt.Errorf("no fact with key %q: nothing to prune", in.Key)
	}
	if err != nil {
		return nil, pruneOutput{}, storeError("velruna_prune", err)
	}
	total := len(f.History)
	// Journal-first: once a key has journal entries, the journal IS the
	// history and the legacy in-doc array is only counted pre-migration.
	if jt, jErr := c.JournalCount(ctx, in.Key); jErr != nil {
		return nil, pruneOutput{}, storeError("velruna_prune", jErr)
	} else if jt > 0 {
		total = jt
	}
	if total <= keep {
		return nil, pruneOutput{Key: in.Key, HistoryTotal: total, Keep: total, Removed: 0}, nil
	}
	if !in.Confirm {
		// The barrier: report the plan as a normal result and write nothing.
		// The plan is the deliverable of this call, not a failure: error-shaped
		// results get dropped by clients that swallow tool errors, silently
		// removing the safety barrier the agent must relay to the user.
		return nil, pruneOutput{
			Key:          in.Key,
			HistoryTotal: total,
			Keep:         keep,
			Removed:      0,
			Pruned:       false,
			Plan: fmt.Sprintf("key %q has %d history entries; confirm=true keeps the newest %d and drops at most %d observations for good (the entry that carries the current value is always spared). Nothing was written. Show this plan to the user and re-call with confirm=true only after their approval",
				in.Key, total, keep, total-keep),
		}, nil
	}
	removed, err := c.Prune(ctx, in.Key, keep)
	if err != nil {
		return nil, pruneOutput{}, storeError("velruna_prune", err)
	}
	return nil, pruneOutput{Key: in.Key, HistoryTotal: total, Keep: keep, Removed: removed, Pruned: true}, nil
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

// --- page snapshots (docs index) ---

type fetchDocInput struct {
	URL     string `json:"url" jsonschema:"page url to fetch; a missing scheme becomes https, utm_* parameters are stripped"`
	Project string `json:"project,omitempty" jsonschema:"tag the document with this project"`
	Refresh bool   `json:"refresh,omitempty" jsonschema:"force a fresh download even when a stored snapshot exists"`

	// MaxChars and BlockChars override the configured extraction limits for
	// this call (fetch_doc_chars, fetch_block_chars). Pointers: 0 is a
	// meaningful MaxChars (store the whole page).
	MaxChars   *int `json:"max_chars,omitempty" jsonschema:"characters to store; 0 = the whole document; default: fetch_doc_chars"`
	BlockChars *int `json:"block_chars,omitempty" jsonschema:"target block size in runes (1000..64000); default: fetch_block_chars"`

	// ConfirmSecret bypasses a guard hit at secret_guard=ask, the same
	// contract as velruna_add: page text is untrusted and can quote
	// credential-like strings.
	ConfirmSecret bool `json:"confirm_secret,omitempty" jsonschema:"confirm a guard hit is a false positive (secret_guard=ask only); set only after the user approved the exact finding"`
}

type docSectionOutline struct {
	Order   int    `json:"order" jsonschema:"section number for velruna_get_doc"`
	Heading string `json:"heading"`
	Chars   int    `json:"chars" jsonschema:"text length in runes"`
}

func newDocOutline(d *model.Doc) []docSectionOutline {
	out := make([]docSectionOutline, 0, len(d.Sections))
	for _, s := range d.Sections {
		out = append(out, docSectionOutline{Order: s.Order, Heading: s.Heading, Chars: len([]rune(s.Text))})
	}
	return out
}

type fetchDocOutput struct {
	Hash      string `json:"hash,omitempty" jsonschema:"document id for velruna_get_doc"`
	URL       string `json:"url,omitempty"`
	Title     string `json:"title,omitempty"`
	Project   string `json:"project,omitempty"`
	FetchedAt string `json:"fetched_at,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Stale     bool   `json:"stale,omitempty" jsonschema:"past TTL: re-fetch with velruna_fetch_doc before quoting"`
	Cached    bool   `json:"cached,omitempty" jsonschema:"true when the stored snapshot was returned (no new content was downloaded)"`
	// Revalidated names the 304 path: the origin confirmed the stored copy
	// with HTTP validators, so the content is unchanged and only fetched_at /
	// expires_at moved. It is a network request, not a cache hit — an agent
	// asking "did the page change?" needs the difference.
	Revalidated bool                `json:"revalidated,omitempty" jsonschema:"true when the origin answered 304 Not Modified and the stored snapshot was kept (timestamps moved; a larger max_chars may have re-extracted more text from the saved html)"`
	Truncated   bool                `json:"truncated,omitempty" jsonschema:"extraction hit a limit: the stored text is a prefix"`
	Via         string              `json:"via,omitempty" jsonschema:"route that served the fetch when not direct: proxy | obscura (audit; obscura means the content was browser-rendered)"`
	Sections    []docSectionOutline `json:"sections,omitempty" jsonschema:"outline: pull a section with velruna_get_doc(hash, section)"`

	// Chars is the stored text and CharsTotal the text extracted before the
	// budget: N of M tells the agent how much of the page it has. The outline
	// is capped at outlineRows lines; SectionsHidden counts the rest.
	Chars          int    `json:"chars_stored,omitempty" jsonschema:"characters stored in this snapshot (runes)"`
	CharsTotal     int    `json:"chars_total,omitempty" jsonschema:"characters extracted from the whole page before the storage budget (runes)"`
	SectionsHidden int    `json:"sections_hidden,omitempty" jsonschema:"outline blocks beyond the listed ones (hint summarizes their range)"`
	Hint           string `json:"hint,omitempty" jsonschema:"navigation advice: more blocks, or a truncated snapshot and how to store the rest"`

	// Redactions: high-confidence credential spans edited out of the stored
	// snapshot (pattern/field/size, no preview): the secret never entered the
	// store, and the answer is transparent about the surgery.
	Redactions []redactionView `json:"redactions,omitempty" jsonschema:"credential spans redacted out of the stored snapshot (no preview)"`
	Warning    string          `json:"warning,omitempty" jsonschema:"set when redactions happened: the stored text is edited, quote from it with that in mind"`

	// GuardMediumIgnored surfaces the doc-route leniency: medium-confidence
	// token shapes left in documentation content on purpose (asset hashes,
	// quoted SHAs). High-confidence secrets are redacted, never ignored.
	GuardMediumIgnored int `json:"guard_medium_ignored,omitempty" jsonschema:"medium-confidence shapes left in documentation content on purpose (documentation route only); high-confidence secrets are always redacted"`

	// GitHub local-clone resolution: the answer is a file on this machine.
	Local bool   `json:"local,omitempty" jsonschema:"true when the url resolved to a file in a local git clone: Path is the file; nothing was fetched"`
	Path  string `json:"path,omitempty" jsonschema:"local clone file path when local=true"`
}

// viaOut hides the default route (direct) from the contract: it carries
// information only when the fetch did NOT go straight to the target.
func viaOut(doc *model.Doc) string {
	if doc.Via == client.ViaDirect {
		return ""
	}
	return doc.Via
}

type redactionView struct {
	Field   string `json:"field" jsonschema:"doc field the span was redacted from"`
	Pattern string `json:"pattern" jsonschema:"detector pattern name"`
	Chars   int    `json:"chars" jsonschema:"length of the redacted span"`
}

func (s *mcpServer) fetchDoc(ctx context.Context, req *mcp.CallToolRequest, in fetchDocInput) (*mcp.CallToolResult, fetchDocOutput, error) {
	// Validate before any store contact: a bad url, a denied host, a robots
	// verdict is the caller's problem, not an availability problem. Config is
	// local, and normalization needs fetch_allow_private.
	cfg, err := s.loadConfig()
	if err != nil {
		return nil, fetchDocOutput{}, err
	}
	rawURL, err := client.NormalizeURL(in.URL, cfg.FetchAllowPrivate)
	if err != nil {
		return nil, fetchDocOutput{}, err
	}
	if err := client.HostAllowed(cfg, rawURL); err != nil {
		return nil, fetchDocOutput{}, err
	}
	if p, ok := client.GitHubLocal(cfg, rawURL); ok {
		// The content is in a local clone: hand out the path. No fetch, no
		// store write: the repo mapping is machine-specific and must never
		// enter the shared index.
		return nil, fetchDocOutput{Local: true, Path: p}, nil
	}
	// The project tag is agent-supplied text that lands in a shared index,
	// and the cache-hit path re-tags a stored doc WITHOUT the content
	// guard (guardFetch gates only the fresh path): scan the tag here.
	// Trim first: stored labels are trimmed, and the guard must see the
	// value that will be stored.
	in.Project = strings.TrimSpace(in.Project)
	if err := guardCheck(cfg.SecretGuard, in.ConfirmSecret,
		guardField{Name: "project", Text: in.Project}); err != nil {
		return nil, fetchDocOutput{}, err
	}
	limits, err := resolveExtractLimits(cfg, in.BlockChars, in.MaxChars)
	if err != nil {
		return nil, fetchDocOutput{}, err
	}
	c, err := client.NewClient(cfg) // FetchDoc ensures the docs index itself
	if err != nil {
		return nil, fetchDocOutput{}, storeError("velruna_fetch_doc", err)
	}
	doc, cached, revalidated, err := c.FetchDocWithin(ctx, rawURL, in.Project, in.Refresh, limits,
		func(d *model.Doc) ([]model.Redaction, error) {
			// Guard after extraction, before the doc is finalized.
			return guardFetch(d, cfg.SecretGuard, in.ConfirmSecret)
		})
	if err != nil {
		return nil, fetchDocOutput{}, err
	}
	outline := newDocOutline(doc)
	shown := len(outline)
	if shown > outlineRows {
		shown = outlineRows
	}
	hint := outlineSummary(doc, shown)
	if doc.Truncated && (doc.FullChars == 0 || doc.Size < doc.FullChars) {
		// A truncation with size == full_chars came from dropped image refs (or
		// a single oversized block): more text is not available, so the budget
		// advice would send the caller after nothing.
		truncHint := "stored a prefix of the page; call again with max_chars=0 to store the whole page"
		if doc.FullChars > 0 {
			truncHint = fmt.Sprintf("stored the first %d of %d chars; call again with max_chars=0 to store the whole page", doc.Size, doc.FullChars)
		}
		if hint != "" {
			hint += " "
		}
		hint += truncHint
	}
	now := time.Now().UTC()
	out := fetchDocOutput{
		Hash:           doc.URLHash,
		URL:            doc.URL,
		Title:          doc.Title,
		Project:        doc.Project,
		FetchedAt:      rfc3339OrEmpty(doc.FetchedAt),
		ExpiresAt:      rfc3339OrEmpty(doc.ExpiresAt),
		Stale:          doc.Stale(now),
		Cached:         cached,
		Revalidated:    revalidated,
		Truncated:      doc.Truncated,
		Via:            viaOut(doc),
		Sections:       outline[:shown],
		Chars:          doc.Size,
		CharsTotal:     doc.FullChars,
		SectionsHidden: len(outline) - shown,
		Hint:           hint,

		GuardMediumIgnored: doc.GuardMediumIgnored,
	}
	if len(doc.Redactions) > 0 {
		out.Redactions = make([]redactionView, 0, len(doc.Redactions))
		for _, r := range doc.Redactions {
			out.Redactions = append(out.Redactions, redactionView{Field: r.Field, Pattern: r.Pattern, Chars: r.Chars})
		}
		out.Warning = fmt.Sprintf("%d high-confidence credential span(s) were redacted out of the stored snapshot; the page is stored edited (the secret never entered the store)", len(doc.Redactions))
	}
	return nil, out, nil
}

type getDocInput struct {
	Hash string `json:"hash" jsonschema:"document id from velruna_fetch_doc/velruna_search_docs"`

	// Section is a pointer: 0 is a valid section number, and its absence
	// must mean "outline only".
	Section *int `json:"section,omitempty" jsonschema:"section number to return in full (from the outline); omit for the outline only"`
	// Blocks is how many consecutive blocks a section read returns: 1 keeps
	// the answer small, more saves round-trips.
	Blocks *int `json:"blocks,omitempty" jsonschema:"how many consecutive blocks to return (default 1, max 10)"`
	Full   bool `json:"full,omitempty" jsonschema:"return every section (bloats the context; prefer one section)"`
}

type getDocOutput struct {
	Found     bool   `json:"found" jsonschema:"false when no stored doc has this hash"`
	Hash      string `json:"hash,omitempty"`
	URL       string `json:"url,omitempty"`
	Title     string `json:"title,omitempty"`
	Stale     bool   `json:"stale,omitempty" jsonschema:"past TTL: refresh with velruna_fetch_doc before quoting"`
	Truncated bool   `json:"truncated,omitempty" jsonschema:"extraction hit a limit: the stored text is a prefix"`
	// GuardMediumIgnored surfaces the doc-route leniency recorded at fetch
	// time: the leniency must be visible on every read path, not only the
	// outline printed by velruna_fetch_doc.
	GuardMediumIgnored int                 `json:"guard_medium_ignored,omitempty" jsonschema:"medium-confidence shapes left in documentation content on purpose (documentation route only); high-confidence secrets are always redacted"`
	Sections           []docSectionOutline `json:"sections,omitempty" jsonschema:"outline: heading + length per section"`

	// The outline is capped at outlineRows lines, like the fetch answer:
	// SectionsHidden counts the rest and Hint summarizes their range.
	SectionsHidden int    `json:"sections_hidden,omitempty" jsonschema:"outline blocks beyond the listed ones (hint summarizes their range)"`
	Hint           string `json:"hint,omitempty" jsonschema:"how to continue: more outline blocks, or a capped read"`

	SectionText string `json:"section_text,omitempty" jsonschema:"the text of the requested section"`
	// BlocksIncluded counts the blocks in SectionText; Next is where a capped
	// read continues (absent when the read reached the end).
	BlocksIncluded int    `json:"blocks_included,omitempty" jsonschema:"blocks returned in section_text"`
	FullText       string `json:"full_text,omitempty" jsonschema:"every section, delimited by headers (full=true)"`
	Next           *int   `json:"next,omitempty" jsonschema:"section to continue from when the read was capped"`
}

// mergeHint joins two navigation hints without losing either: one answer
// carries at most one hint field.
func mergeHint(existing, extra string) string {
	switch {
	case existing == "":
		return extra
	case extra == "":
		return existing
	}
	return existing + "; " + extra
}

func (s *mcpServer) getDoc(ctx context.Context, req *mcp.CallToolRequest, in getDocInput) (*mcp.CallToolResult, getDocOutput, error) {
	in.Hash = strings.TrimSpace(in.Hash)
	if in.Hash == "" {
		return nil, getDocOutput{}, errors.New("velruna_get_doc: hash is required")
	}
	// Validate before the read: readDocBlocks skips orders below start, so a
	// negative section would silently return block 0.
	if in.Section != nil && *in.Section < 0 {
		return nil, getDocOutput{}, fmt.Errorf("velruna_get_doc: section must be 0 or greater, got %d", *in.Section)
	}
	if in.Blocks != nil && (*in.Blocks < 1 || *in.Blocks > maxReadBlocks) {
		return nil, getDocOutput{}, fmt.Errorf("velruna_get_doc: blocks must be between 1 and %d, got %d", maxReadBlocks, *in.Blocks)
	}
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, getDocOutput{}, storeError("velruna_get_doc", err)
	}
	doc, err := c.GetDocByHash(ctx, in.Hash)
	if errors.Is(err, client.ErrNoDoc) {
		return nil, getDocOutput{Found: false}, nil
	}
	if err != nil {
		return nil, getDocOutput{}, storeError("velruna_get_doc", err)
	}
	// The outline is capped like the fetch answer; the hint carries the range
	// of the blocks it left out and is merged with the read hints below.
	outline := newDocOutline(doc)
	shown := len(outline)
	if shown > outlineRows {
		shown = outlineRows
	}
	now := time.Now().UTC()
	out := getDocOutput{
		Found:              true,
		Hash:               doc.URLHash,
		URL:                doc.URL,
		Title:              doc.Title,
		Stale:              doc.Stale(now),
		Truncated:          doc.Truncated,
		GuardMediumIgnored: doc.GuardMediumIgnored,
		Sections:           outline[:shown],
		SectionsHidden:     len(outline) - shown,
		Hint:               outlineSummary(doc, shown),
	}
	if in.Full {
		text, _, next := readDocBlocks(doc, 0, len(doc.Sections), docReadBudget)
		out.FullText = text
		if next >= 0 {
			out.Next = &next
			out.Hint = mergeHint(out.Hint, fmt.Sprintf("full text capped at %d runes; continue with section=%d", docReadBudget, next))
		}
	}
	if in.Section != nil && !in.Full {
		want := 1
		if in.Blocks != nil {
			want = *in.Blocks
		}
		text, included, next := readDocBlocks(doc, *in.Section, want, docReadBudget)
		if included == 0 {
			return nil, getDocOutput{}, fmt.Errorf("velruna_get_doc: doc %s has no section %d (the outline lists %d sections)",
				doc.URLHash, *in.Section, len(doc.Sections))
		}
		out.SectionText = text
		out.BlocksIncluded = included
		if next >= 0 {
			out.Next = &next
			out.Hint = mergeHint(out.Hint, fmt.Sprintf("continue with section=%d", next))
		}
		return nil, out, nil
	}
	return nil, out, nil
}

type searchDocsInput struct {
	Query   string `json:"query" jsonschema:"text to search in stored document sections (the raw html is never searched)"`
	Project string `json:"project,omitempty" jsonschema:"limit to documents tagged with this project"`
	Limit   int    `json:"limit,omitempty" jsonschema:"max documents to return (default 5, cap 20): the context budget for search results"`
}

// docSectionHit is the section that matched the query: get_doc(section=order)
// returns the full text around the match.
type docSectionHit struct {
	Order   int    `json:"order" jsonschema:"section number: velruna_get_doc(hash, section=order) reads the full section"`
	Heading string `json:"heading,omitempty" jsonschema:"section heading (table of contents)"`
	Snippet string `json:"snippet" jsonschema:"match-centered text (±160 runes), whitespace-collapsed"`
}

type docHitView struct {
	Hash    string `json:"hash" jsonschema:"document id for velruna_get_doc"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Project string `json:"project,omitempty" jsonschema:"project tag of the stored document"`
	Stale   bool   `json:"stale" jsonschema:"past TTL: refresh with velruna_fetch_doc before quoting"`
	// Section is the matched section (all-terms scan in Go); Snippet is the
	// doc-level fallback (first section) when no section is identified.
	Section *docSectionHit `json:"section,omitempty" jsonschema:"the section that matched the query"`
	Snippet string         `json:"snippet,omitempty" jsonschema:"first section, whitespace-collapsed and capped (fallback when no section matched)"`
}

type docsHitsOutput struct {
	Count     int          `json:"count"`
	Docs      []docHitView `json:"docs"`
	Truncated bool         `json:"truncated,omitempty" jsonschema:"true when the store matched more documents than this page (cap 20): narrow the query before concluding there is nothing more; absent means complete"`
	Skipped   int          `json:"skipped,omitempty" jsonschema:"stored documents whose content failed to decode (index corruption): report to the user"`
}

func (s *mcpServer) searchDocs(ctx context.Context, req *mcp.CallToolRequest, in searchDocsInput) (*mcp.CallToolResult, docsHitsOutput, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, docsHitsOutput{}, errors.New("velruna_search_docs: query is required")
	}
	in.Project = strings.TrimSpace(in.Project)
	limit := in.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > client.DocsSearchCap {
		limit = client.DocsSearchCap
	}
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, docsHitsOutput{}, storeError("velruna_search_docs", err)
	}
	docs, meta, err := c.SearchDocs(ctx, in.Query, in.Project, limit)
	if err != nil {
		return nil, docsHitsOutput{}, storeError("velruna_search_docs", err)
	}
	now := time.Now().UTC()
	terms := queryTerms(in.Query)
	hits := make([]docHitView, 0, len(docs))
	for _, d := range docs {
		h := docHitView{Hash: d.URLHash, URL: d.URL, Title: d.Title, Project: d.Project, Stale: d.Stale(now)}
		if i := pickMatchedSection(d.Sections, terms); i >= 0 {
			sec := d.Sections[i]
			h.Section = &docSectionHit{Order: sec.Order, Heading: sec.Heading, Snippet: centerSnippet(sec.Text, terms)}
		} else if len(d.Sections) > 0 {
			h.Snippet = snippet(d.Sections[0].Text, 300)
		}
		hits = append(hits, h)
	}
	return nil, docsHitsOutput{Count: len(hits), Truncated: meta.Total > len(hits)+meta.Skipped, Skipped: meta.Skipped, Docs: hits}, nil
}

// queryTerms approximates OpenSearch's standard tokenizer: lowercased
// alphanumeric runs of length >= 3, deduplicated. Stopwords need no list:
// the all-terms section scan punishes them uniformly.
func queryTerms(query string) []string {
	var out []string
	seen := map[string]bool{}
	var cur []rune
	flush := func() {
		w := string(cur)
		cur = cur[:0]
		if len(w) >= 3 && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	for _, r := range strings.ToLower(query) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// pickMatchedSection picks the section containing ALL query terms — a direct
// match beats any frequency-based pick (frequency overvalues long sections).
// Fallback: the section with the most distinct terms; -1 = no match.
// This is the cheap Go-side scan the design chose over nested inner_hits.
func pickMatchedSection(sections []model.Section, terms []string) int {
	if len(terms) == 0 {
		return -1
	}
	best, bestHits := -1, 0
	for i, sec := range sections {
		lower := strings.ToLower(sec.Text)
		hits := 0
		for _, t := range terms {
			if strings.Contains(lower, t) {
				hits++
			}
		}
		if hits == len(terms) {
			return i
		}
		if hits > bestHits {
			best, bestHits = i, hits
		}
	}
	if bestHits > 0 {
		return best
	}
	return -1
}

// centerSnippet windows the text around the first term hit (±160 runes) so
// the answer is visible in the snippet, not just the section opening.
func centerSnippet(text string, terms []string) string {
	collapsed := []rune(strings.Join(strings.Fields(text), " "))
	lower := strings.ToLower(string(collapsed))
	pos := -1
	for _, t := range terms {
		if p := strings.Index(lower, t); p >= 0 && (pos < 0 || p < pos) {
			pos = p
		}
	}
	const win = 160
	if pos < 0 {
		return snippet(text, 2*win)
	}
	// pos is a byte index into the lowercased string; rune-align it onto the
	// collapsed text (case folding can shift byte lengths; snippets tolerate).
	r := utf8.RuneCountInString(lower[:pos])
	start, end := r-win, r+win
	if start < 0 {
		start = 0
	}
	if end > len(collapsed) {
		end = len(collapsed)
	}
	out := string(collapsed[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(collapsed) {
		out += "…"
	}
	return out
}
