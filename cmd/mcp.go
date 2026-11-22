package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/config"
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
			"(one link per fact; a later patch may repoint it; the target must " +
			"exist and be verified; cycles refused). A link-only patch is " +
			"journaled as op=supersede.",
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
