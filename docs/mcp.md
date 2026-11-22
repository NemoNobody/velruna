# MCP server

`velruna` can run as an MCP server, so an agent calls the store directly —
searching before re-verifying something and storing what it verified — instead of
you running the CLI by hand.

```
velruna mcp [--conf <path>]
```

It speaks MCP over **stdio**: JSON-RPC on stdin/stdout, diagnostics on stderr.
The client (VS Code) starts it; you never run it manually except to debug.

## Registering it

velruna is meant to be available in **every** project (it is memory, not a project
setting), so prefer user-wide registration. How a config is picked up depends on
the session type:

| Scope | File | Read by |
|---|---|---|
| User, portable | `~/.copilot/mcp-config.json` (`$COPILOT_HOME` if set) | the Agent Host and the Copilot CLI **natively**; regular chat only when `chat.mcp.discovery.enabled` enables the `copilot` source — all discovery sources are **off by default** |
| VS Code user profile | `mcp.json` via `MCP: Open User Configuration`, or `code --add-mcp <json>` | regular chat natively; VS Code marks this destination as deprecated for compatibility |
| Workspace, portable | `.mcp.json` at the project root | regular chat, that project only |
| Workspace, VS Code | `.vscode/mcp.json` | deprecated for compatibility |

**A registered server is not necessarily a running one** — a server that never
started is invisible to the agent. Verify with `MCP: List Servers`, or read the
per-server log `~/.config/Code/logs/<stamp>/window1/mcpServer.<source>.velruna.log`
(`workspace-dot-mcp` = workspace `.mcp.json`; `copilot` = the discovered user
config). Newly registered servers start on the next chat message
(`chat.mcp.autostart`) or on a window reload — and non-workspace servers also
need a **trust approval** (approve the trust dialog / add the server to the
trusted list): until it is approved, the server stays stopped and its log stays
empty. VS Code asks again whenever the server definition changes — expect a
prompt while iterating on the config; rebuilding the `velruna` binary alone does
not change the definition.

A registration in `~/.copilot/mcp-config.json`:

```json
{
  "mcpServers": {
    "velruna": {
      "type": "stdio",
      "command": "<path-to-binary>/velruna",
      "args": ["mcp", "--conf", "<path-to-binary>/velruna.conf"]
    }
  }
}
```

For regular chat to read that file, discovery must be enabled for the `copilot`
source (VS Code 1.138+):

```json
// user settings.json
"chat.mcp.discovery.enabled": {
    "claude-desktop": false,
    "copilot": true,
    "windsurf": false,
    "cursor-global": false,
    "cursor-workspace": false
}
```

Register the server in exactly **one** scope: on a name collision VS Code applies
`chat.mcp.collisionBehavior` (default `disable`; set it to `suffix` to keep both
under distinct names) — do not keep a workspace `.mcp.json` and the user-wide
file for the same server name.

Two details that matter:

- **Absolute path to the binary.** MCP clients do not run your shell profile, so
  `PATH` is not a reliable place to look. If you install `velruna` somewhere else,
  update this path.
- **`--conf` with an absolute path.** The server is started from an arbitrary
  working directory, so `./velruna.conf` is unreliable. Without `--conf` the MCP
  lookup chain is `VELRUNA_CONF` → next to the binary → `~/.velruna.conf`; the
  working-directory entry that the plain CLI uses is skipped on purpose — a cloned
  repository's `velruna.conf` must never hijack where your facts and credentials
  are sent. `--conf` removes the guesswork.

Do not put the OpenSearch password in the MCP config: it resolves from the OS
keyring (`velruna creds set`), `OPENSEARCH_PASSWORD`, or `velruna.conf` (the
latter only when `allow_plaintext` is on).

### Running from the container image

`ghcr.io/nemonobody/velruna` carries the server and the CLI. stdio is the
transport, so the client starts the container per session and no port is
published. A container has no session bus, which means the OS keyring is
unreachable inside it: the password comes from `OPENSEARCH_PASSWORD` or from a
mounted config that carries it.

```json
{
  "mcpServers": {
    "velruna": {
      "type": "stdio",
      "command": "docker",
      "args": [
        "run", "-i", "--rm",
        "--user", "1000:1000",
        "--network", "velruna-os_default",
        "-e", "VELRUNA_CONF=/etc/velruna/velruna.conf",
        "-e", "OPENSEARCH_PASSWORD",
        "-v", "/home/me/.velruna.conf:/etc/velruna/velruna.conf:ro",
        "ghcr.io/nemonobody/velruna:0.8.9",
        "mcp"
      ]
    }
  }
}
```

- `--network` is only needed when the store is the compose service (the stack
  puts it on `velruna-os_default`); for a store on the host use
  `--network host` on Linux, or the host's LAN address.
- the mount source must exist: Docker creates a **directory** for a missing
  host path and the server then fails with `is a directory`;
- the image runs as uid 65532, so a `chmod 600` config owned by you is
  unreadable inside — the example passes `--user 1000:1000` to reuse your own
  uid; without it use `chmod 644` (the file holds no password when the password
  comes from the environment) or `chown 65532`;
- `-e OPENSEARCH_PASSWORD` forwards the variable from the environment the MCP
  client was started from — a desktop-launched editor inherits the login
  session, not your shell exports, so the variable may simply be absent. When
  that is unclear, use `--env-file` with an owner-only file (mode `600`);
- `docker inspect` shows the variable to anyone who can reach the daemon, so
  keep the socket private (the default);
- a config file is not mandatory: `-e OPENSEARCH_URL`, `-e OPENSEARCH_USER` and
  `-e OPENSEARCH_PASSWORD` (or an `--env-file`) are enough for a
  container-only setup;
- pin a version (`:0.9.0`), not `:latest`;
- facts written from a container carry the container's OS user as author
  (`nonroot`) — set `VELRUNA_AUTHOR` / `VELRUNA_HOST` to keep provenance
  readable for the other sessions reading the store.

### Installing from the MCP bundle

Each release also ships MCP Bundles (`.mcpb`) — the MCP Bundle format: a zip
holding `manifest.json`, the builds and a launcher. Clients that speak the
format (Claude Desktop and others) install one from the file and show a
configuration form instead of asking you to edit JSON:

| Asset | Size | Contents |
|---|---|---|
| `velruna-<version>.mcpb` | ~22 MB | every build (universal; the registry entry points here) |
| `velruna-<version>-darwin.mcpb` | ~9 MB | macOS amd64 + arm64 |
| `velruna-<version>-linux.mcpb` | ~9 MB | linux amd64 + arm64 |
| `velruna-<version>-windows.mcpb` | ~5 MB | the Windows build |

A per-OS bundle declares only its own platform and drops the other launch
descriptions; the universal one exists because a registry package entry has no
platform field, so a client cannot choose between several artifacts.

The form asks for:

| Field | Goes to |
|---|---|
| OpenSearch URL (required) | `OPENSEARCH_URL` |
| OpenSearch user (default `admin`) | `OPENSEARCH_USER` |
| OpenSearch password (masked) | `OPENSEARCH_PASSWORD` |
| Skip TLS verification (default off) | `OPENSEARCH_INSECURE_SKIP_VERIFY` |

The server still needs a reachable OpenSearch — the bundle does not embed one.
The same artifact is what the official MCP registry points at, so a client that
resolves the registry entry downloads exactly this file.

## Tools

| Tool | Purpose | Notes |
|---|---|---|
| `velruna_status` | Is the store reachable? What is the effective config and storage state? | **Never fails** — `ok=false` plus a hint instead, so the agent can explain the problem; includes `password_from` (`env`/`keyring`/`file`/`none`, never the value), `opensearch_version`, and the running binary's `mcp_version`/`commit`/`started_at` (an agent can verify which build answers — the repo on disk is not the running server); `storage.indices` lists every velruna index (facts, docs, journal periods) with documents, exact bytes and shard layout from one `_cat/indices` call, and `desired_replicas`/`actual_replicas`/`replica_drift` name a replica reconcile that did not converge; `fetch` reports the effective document-fetch setup (docs index, TTL, renderer, proxy presence — never the URL — allowlist size, timeout) |
| `velruna_search` | Full-text search over fact values, notes and summaries | Trusted facts only by default; `status: "any"` includes obsolete ones; `keyword` restricts to facts carrying that exact tag; history off unless `include_history: true`; **compact by default** (`compact: false` restores full rows): key/summary/category/status/notes-head/age rows, the full value is one `velruna_get` away; a non-compact-zero answer names that in `hint`, and a zero-hit answer carries a navigation `hint` |
| `velruna_get` | Exact fact by key, or a batch via `keys: [...]` (up to 10, `items[]` with per-key `found`) | `found: false` when absent — not an error; history off unless `include_history: true`; carries `summary`, `keywords`, `confirmed_by` (who verified the current value), `author`/`author_source`, `updated_by` when present |
| `velruna_add` | Store a verified fact | Shape is validated before the guard and the store: non-empty `key`/`value`, and `category` one of `location`/`config`/`endpoint`/`behavior`/`context_map`/`recipe`/`rule`, stored lowercase (an unknown category never ages — silent rot). Optional `ttl_days` shortens the category's staleness window for this fact (it never lengthens it past the cap: config/endpoint 14, location/behavior 30, recipe 180, rule 90; `context_map` is uncapped). Corrects by key: the accepted value is appended to the journal and the fact points at the entry; the same value updates notes/source only and leaves the journal untouched. Optional `keywords` (up to 10 normalized tags, exact-match filterable via `velruna_search`). The answer says what happened: `created`, `changed` (false = same-value touch), `previous_summary` (the superseded table of contents). `notes` are replaced only by a non-empty new note (an omitted `notes` leaves the stored qualifiers alone — they never enter history, so erasing them is irreversible). A value over `summary_min_len` requires `summary` (under `summary_max_len`) — the store's table of contents. Writes are stamped with authorship labels (`author`/`author_source`/`model`/`host`/`session` — self-reported audit metadata, birth labels write-once; later writes move `updated_by`). `secret_guard` runs before any store contact: a high-confidence credential refuses the write, a heuristic hit in a text field is replaced and reported in `redactions`, and a hit in an identity field (key/project/source/keywords) refuses. For cheap partial updates (dated note, status flip) use `velruna_patch` — no value resend |
| `velruna_add_batch` | Store 1..10 facts in one call (the session-end ritual in a single round-trip) | Per-item guard and `confirm_secret` (one approved false positive never whitewashes the rest); duplicate keys are rejected (the whole batch — a dupe makes created/changed order-dependent and ambiguous); partial success is legal: per-item `{key, created, changed, status, error?}` |
| `velruna_patch` | Partial update of an existing fact (never creates) | Fields: `value`, `summary`, `notes` (replace), `notes_append` (dated line appended atomically server-side — a concurrent add cannot drop it), `source`, `status` (`verified`\|`obsolete`; a transition is journaled; obsolete→verified sets `observed_at=now`), `reason` (with obsolete), `ttl_days` (retunes the staleness window, bounded by the category cap; a qualifier touch, not journaled), `keywords` (replaces the tag set; same normalization as add — a qualifier touch, the tags appear in the history with the next accepted value), `reverify` (re-confirm the current value: `observed_at` moves to now, `op=rediscovery` is journaled, the writer joins `confirmed_by`; refused together with value/summary/status, and on an obsolete fact). Omitted fields are untouched. On a value change the old summary is CLEARED unless a new one is provided (a stale table of contents is a lie); a long new value requires a new summary (the add rule) |
| `velruna_patch_batch` | Patch 1..10 existing facts in one call (the session-end ritual when the facts already exist) | Per-item guard and `confirm_secret` (one approved false positive never whitewashes the rest); duplicate keys rejected (the whole batch); partial success legal: per-item `{key, changed, reverified?, status, error?}`, with `changed`/`reverified` counters on the answer. Same field semantics as `velruna_patch`; never creates (a missing key is a per-item store error) |
| `velruna_list` | Facts of a project | Same trust default as search; **compact by default** (`compact: false` restores full rows): key/summary/category/status/notes-head/age rows — full values cost ~5x tokens and a project listing is the most expensive agent call; **sorted by `updated_at` desc** by default (`sort: "key"` for stable navigation; a same-value touch moves `updated_at` without changing `age`/`stale`); `prefix` keeps only keys starting with it (trailing `*` allowed); history ships only on full rows (`include_history: true` with `compact: false` — compact rows are navigation; `velruna_journal` reads history); a zero-hit answer carries a navigation `hint` |
| `velruna_keys` | Find facts by key across all projects | `prefix` (key start) and/or `match` (key substring; `*`, `?` wildcards); at least one required; sorted by key; `project` is an optional filter; key matching is case-sensitive (keys conventionally lowercase); rows are always compact (navigation is scanned, not read) |
| `velruna_journal` | Read the journal for a key: the append-only history of accepted values, newest first (`limit`, default 20; `since`/`until` bound `created_at` as RFC3339 UTC) | `op: write` is an accepted value, `op: rediscovery` is a re-confirmation of the same value (`reverify` on `velruna_patch`), `op: retire` marks a verified→obsolete transition; counts after an admin deleted old journal indices are lower bounds. Prefer `velruna_history_info` first, the full journal only when the history matters |
| `velruna_history_info` | Compact per-key history overview: writes, rediscoveries, first/last timestamp, `current_ref` — no entry bodies | Answers "did this fact change, how often, when last" cheaply; counts are LOWER BOUNDS (admin index pruning is invisible to the app) |
| `velruna_prune` | Drop old journal entries under a key, keeping the newest `keep` | **DESTRUCTIVE** — without `confirm: true` nothing is written, the call returns the plan; show it to the user and re-call only after their approval. Journal-first; keys that predate the journal fall back to the legacy in-doc array. The journal is write-forever by design: pruning is a manual admin decision, never automatic |
| `velruna_obsolete` | Mark a fact obsolete because its subject is gone | Value is kept for reference; the reason is appended to notes; the transition is journaled. The same `secret_guard` policy as `velruna_add` applies: a credential refuses, a heuristic hit in the reason is redacted with a marker and reported in `redactions` (the rows merge with the current value's audit). Use `velruna_add` when the value changed, `velruna_patch` to flip status |
| `velruna_fetch_doc` | Fetch a web page into the docs index, return its outline (hash + blocks) | Cache-aware: a second fetch of the same URL returns the stored snapshot (`cached: true`) until its TTL; `refresh: true` forces a check; the stored ETag/Last-Modified go out as `If-None-Match`/`If-Modified-Since`, and a `304` answer keeps the snapshot and moves only `fetched_at`/`expires_at` (`revalidated: true`) — no body download; validators are only reused for the same retrieval path, so a renderer-captured copy re-fetches in full. Passing a different `project` re-tags the stored snapshot (the tag is guard-scanned at the call, both paths). Requests send no cookies and no credentials; a host outside `fetch_allow_hosts` is rejected before the request. Internal addresses (loopback, private, link-local incl. cloud metadata) are refused at every hop and at dial time — the SSRF guard; `fetch_allow_private: true` is the opt-in for self-hosted docs on a LAN. GitHub URLs that match a local clone resolve to a local path (`local: true`) with no fetch and no store write. With `fetch_render: "obscura"` (opt-in), a fetch refused by bot walls (network failure, 403/422/429) is retried through the obscura headless browser and stored `via: obscura` (v1 limit: `final_url` = the requested URL); non-eligible failures (404/401, allowlist, redirect limits, oversize) stay plain. The `secret_guard` runs before the doc is finalized: high-confidence credential shapes in the agent-visible fields, the stored raw HTML and image hrefs are redacted in place (the answer lists `redactions` + a `warning`, no previews; redaction precedes `size`/`content_hash`). Medium findings route on the stored `content_type`: documentation types (html/markdown/plain) do not refuse the write — the shapes are ordinary content there — instead the answer surfaces `guard_medium_ignored: N` when N>0; API types (`application/json\|xml`, `+json`/`+xml`) keep refusing (confirmable at `ask`). The URL and the post-redirect `final_url` are identity — scanned high-confidence only, and a credential-bearing redirect target refuses the doc. A direct render (no proxy) resolves the target and refuses non-public addresses (the render is the SSRF gate for its own route; with a proxy the proxy enforces the policy). **Blocks and budgets:** the extracted text is split into blocks, never cut — a heading-delimited section is the natural unit, and a section longer than `block_chars` (optional; defaults to `fetch_block_chars`, 8000 runes) is split at a line boundary (then at a space or punctuation, and only then hard at the count) without loss; split parts are labelled `heading (2/5)`, and every block is addressable by `order`. Optional `max_chars` (0 = the whole document; defaults to `fetch_doc_chars`, 100000) is the storage budget for one document: when it runs out, whole blocks are dropped from the end, `truncated: true` is set, and the answer carries `chars_stored`/`chars_total`. `content_hash` is computed over the full extracted text before the budget (it does not move with `max_chars`/`block_chars`), and `full_chars` is that text's length. When a stored copy was cut and a larger `max_chars` is requested, the text is re-extracted from the stored HTML with no network; the guard runs again over the re-extracted text (a credential split by markup is only visible there) and the audit lists are merged with the earlier ones — the stored `html`, `etag`, `project` and `expires_at` are not rewritten. A smaller request never shrinks the stored copy for unchanged content. `block_chars` applies whenever text is extracted: a cached snapshot whose text is complete keeps its stored split (a revalidating `304` included); a truncated snapshot re-extracted for a larger `max_chars` is re-blocked at the requested size when that budget actually stores more text. `truncated` marks a real loss only — the budget or dropped image references; a page over `fetch_max_bytes` (2 MB) is refused with an error, not trimmed — blocks skipped by the chrome filter are not a loss. A large outline is shown partially (the first 60 lines plus `more N blocks, orders X..Y`) — a response limit, not a storage limit |
| `velruna_get_doc` | Read a stored snapshot by hash: outline / block(s) from `section: N` / `full: true` | `blocks` (default 1, max 10) returns that many consecutive blocks; with `section: N` the run starts at block N. The answer is capped at 32000 runes — if the requested blocks do not fit, fewer are returned and `next` names the next block to read; one block is always returned whole even when it alone exceeds the cap (an empty answer would hide the document). `full: true` hits the same cap with an explicit marker, never silently. Never touches the network; `found: false` is an answer, not an error; `stale: true` means the snapshot is past its TTL — refresh with `velruna_fetch_doc` before quoting. Prefer one block or a few: `full` bloats the context |
| `velruna_search_docs` | BM25 search over stored page blocks | Returns hash+url+title+project per doc with the MATCHED block (reported in `section: {order, heading, snippet}` — `order` is the block number, the snippet is centered on the match; `velruna_get_doc(section=order)` reads that block, add `blocks` for the following ones); doc-level snippet is the fallback when no block is identified; default limit 5, cap 20; the raw html is write-only and never searched |

Note: `velruna_search` does **not** match keys — use `velruna_get` for an exact
key, `velruna_keys` for key navigation (prefix/substring across projects).
`include_history: true` on fact reads returns the legacy in-doc history array
(pre-journal facts); journal-era history is read with `velruna_journal` /
`velruna_history_info`.

Facts tools answer with `count` and the honest truncation signal: `truncated:
true` means the cluster matched more facts than this page delivered — for
`velruna_search` the page is the requested `limit` (raise it, max 50, before
narrowing the query); for `velruna_list`/`velruna_keys` it is the navigation
cap (1000) — narrow with prefix/project/status. A page exactly filling its cap
is a complete result. `skipped: N` means N stored hits failed to decode
(index corruption): report it to the user; the answer is incomplete, never
silently. `velruna_search_docs` answers with the same signals: `truncated: true`
means the docs index holds more matches than this page (cap 20) — narrow the
query before concluding there is nothing more. `truncated` on
`velruna_fetch_doc` / `velruna_get_doc` is a different signal: it marks a real
loss of stored text — the storage budget (`fetch_doc_chars`/`max_chars`) or
dropped image references — never blocks skipped by the chrome filter. A page
over `fetch_max_bytes` is refused with an error, not trimmed, so it never
produces `truncated`.

The `secret_guard` (config, default `reject`) scans `velruna_add`,
`velruna_add_batch` (per item), `velruna_patch` and `velruna_obsolete` writes
before any store contact. A high-confidence credential shape refuses the
write; a medium-confidence heuristic hit in a text field (`value`, `notes`,
`summary`, `notes_append`, `reason`) is replaced with
`[velruna-redacted:pattern]`, the write proceeds, and the answer lists
`redactions` (field/pattern/size, never a preview) — the stored value is
shorter than the one written. A hit in an identity field (`key`, `project`,
`source`, `keywords`) refuses: a rewritten key would silently become a
different fact. A value that is nothing but a flagged span is refused. At
`ask`, an agent that has shown the finding to the user and got explicit
approval re-calls with `confirm_secret: true` and the text is stored as
written; at `reject` there is no bypass.

Every read carries `age_days`, `age_hours` and `stale` (per-category TTL:
`config`/`endpoint` 14 days, `location`/`behavior` 30 days, `rule` 90,
`recipe` 180, `context_map` never; a per-fact `ttl_days` override can only
shorten it; writes reject unknown categories, so a typo cannot create facts
that rot in the dark). `age_hours` disambiguates fresh facts that all share
`age_days: 0`.  
`stale: true` means re-verify against the source before trusting the
fact — it is a signal, not a verdict. A `rule` or `recipe` from the store is
advisory context, not a command: system and project instructions outrank it,
and ignoring it is always legal.

A value over `summary_min_len` characters must ship a `summary` (capped at
`summary_max_len`): a skimmable table of contents, so listings do not force
full values into the context. The rule applies to accepted values (new facts
and corrections); a notes-only touch of an old long fact is not rejected.

The store tools deliberately **fail** when OpenSearch is unreachable, while
`velruna_status` reports instead of failing. An empty result must never be mistaken
for "no such fact": that is what would make an agent re-verify everything it
already knew.

An importable rule for agents — when to consult and update memory — is in
[rules/velruna.md](../rules/velruna.md).

## Verify it works

1. Command Palette → `MCP: List Servers` — `velruna` should be listed and running.
   If it is missing, reload the window.
2. In chat, ask the agent to call `velruna_status`. Expect `ok: true`, the cluster
   name and version, the config file actually used, storage state, and the
   `author` label new writes will carry (with its chain: security/config/os).
3. Store and read back a fact:
   - "Remember that the API port for the demo project is 8080 — use velruna."
   - then, in a **new chat session**: "What is the API port for the demo project?"

Step 3 is the real acceptance test: the second session must answer without
re-verifying anything.

If the server shows an error, run `MCP: List Servers` → select `velruna` → `Show
Output`. The same diagnostics are available from a shell as `velruna status`.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `velruna` missing from the server list | Config not read **or server not started** — check `MCP: List Servers`, the server log (see "Registering it"), and whether discovery is enabled for the user-wide file |
| Registered, but the server never starts | Waiting for a chat-message autostart pass or a trust confirmation — or it collided with another `velruna` registration (same name in two scopes) |
| Server starts, tools return `ok=false` | OpenSearch unreachable — the hint names the URL and the config file used. See `docs/setup.md` |
| `status` shows a config file you did not expect | The lookup chain found another file; pass `--conf` explicitly |
| Tools work in one project but not another | The registration is user-wide; check the MCP output log for a start-up error |
| Writes fail with "the cluster is blocking writes" | Cluster-side block (disk flood stage or a read-only index). Free space or ask the cluster owner; see `docs/setup.md` |
