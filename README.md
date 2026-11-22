# velruna

[![CI](https://github.com/NemoNobody/velruna/actions/workflows/ci.yml/badge.svg)](https://github.com/NemoNobody/velruna/actions/workflows/ci.yml)

Persistent, provenance-carrying memory for AI agents (and humans).

> **Before the name misleads you:** this is not a memory that recalls what you
> cannot name. It is a catalogue of facts a session has already verified —
> found by text (BM25) or by key, and read in pieces. Matching meaning is the
> job of the model holding the context, not of the store.
>
> The name is coined: Old Norse *vél* "device, craft" (modern Icelandic
> "machine") + *rún* "secret; carved mark". Runestones and the Bergen trade
> tags were durable authored records — written once, read by strangers long
> after — and that is the model here. In Younger Futhark, ᚢᛁᛚᚱᚢᚾᛅ (ᚢ writes
> /v/). The word is invented; the metaphor is not. Up to v0.8.6 the project
> was published as `memento`.

## What it is

`velruna` stores **facts you or an agent have verified** about your projects —
file locations, endpoint and config values, behavior — in an OpenSearch index.
Every agent session relearns the same paths, ports and quirks; this stores them
once, with the evidence and an age. The aim is to stop re-verifying the same
path in every session: an agent that keeps the store in the loop (the rule file
is [rules/velruna.md](rules/velruna.md)) reads the fact with its observation
date and source, and goes back to the source only when the age says it may have
moved. The store records that evidence, who wrote it and when; it cannot verify
anything itself. It is an advisory memory with an audit trail, not an oracle.
Why OpenSearch and not files or SQL: [Storage](#storage).

Who it is for: one person or a small team working the same repositories
session after session, who want one shared, inspectable store instead of a
memory file per clone — and who already run (or don't mind running) an
OpenSearch node. Sharing one store between sessions, machines and agents is the
point: keys are namespaces, every write carries an author label, so two agents
on the same project can pick up each other's facts without a human relaying
them. If a flat `AGENTS.md` is enough, it is enough.

Each fact carries evidence, not verdicts:

- **provenance** (`source`): where it came from — `tool:read_file`,
  `experiment`, `user:confirmed`, `doc:*`;
- **observation time** (`observed_at`): when the value was actually seen;
- **history**: every accepted value is appended to an append-only journal
  index (`<index>-history-<period-date>`, monthly by default) instead of
  overwriting the fact; entries are never rewritten and the journal is not
  pruned by default (write-forever), so history shows how old it is;
- **notes**: free-text qualifiers, e.g. "checked the dev config only";
- **authorship**: every write is stamped with who recorded it (the store's
  `security` user when the cluster has one, else config/OS user — a
  self-reported audit label, not authentication) and which model/host/session
  produced it; the birth labels are write-once, corrections move `updated_by`;
- **re-verification**: `confirmed_by` lists who verified the *current* value
  (creator first, user+host per entry); `velruna_patch reverify` moves
  `observed_at` and journals an `op=rediscovery` entry, so a stale fact can
  actually become fresh again;
- **summary**: a value over `summary_min_len` must ship a short table-of-
  contents rendering (kept under `summary_max_len`), so store listings stay
  skimmable without pulling full values into context;
- **keywords**: up to 10 normalized tags per fact (lowercase single tokens),
  exact-match filterable — cross-project slices (`perf`, `security`, `infra`)
  without a graph backlog, and recipes/rules stay findable by topic;
- **successor** (`superseded_by`): at most one link per fact to the record
  that replaces it, set by `velruna_patch` (a later patch may repoint it;
  both events are journaled as `op=supersede`).
  The target must be a verified fact and the chain must stay cycle-free; a
  link pointing back in time is legal but flagged in the journal. It answers
  "this key is stale garbage, read that one instead" without copying the value;
- **secret guard**: writes scan for credential-like text (the index is
  plaintext) — a real credential refuses the write, a hit in an identity field
  (key, project, source, keywords) also refuses, while a heuristic hit (a long
  hash, a base64 blob, an opaque token, a weak assignment) is replaced in
  place and the answer lists what was cut; `reject` by default, `ask` lets a
  human confirm a false positive, `allow` opts out;
- **staleness**: reads compute `age_days` and `stale` from `observed_at` and a
  per-category TTL (`config`/`endpoint` 14 days, `location`/`behavior` 30 days,
  `rule` 90, `recipe` 180, `context_map` never). A writer
  may shorten the window per fact (`ttl_days`), never lengthen it past the
  category cap. `stale: true` is an instruction to re-verify against the source,
  not to discard the fact.

Two statuses: `verified` (the subject exists) and `obsolete` (the subject is
gone — file deleted, endpoint removed). The store holds the evidence; the agent
judges trust and staleness in context.

Authorship is audit metadata, not access control: a write records the store's
security user when the cluster exposes one, otherwise the configured
(`author`) or OS user, plus `model`/`host`/`session` labels. These birth labels
are write-once; every later write moves `updated_by`. The journal carries the
same per-entry, so a shared store shows who saw what when. Labels are
self-reported — a client can set any `author`; the field supports review and
attribution, not trust.

## Reading is selective

A session pays only for what it reads. `velruna_list` and `velruna_search`
return compact rows — key, summary, category, status, age — and the full value
is one `velruna_get` away; a body over `summary_min_len` must ship a summary,
so a listing stays skimmable; a fetched page is split into blocks — a section too
long for one block is split, not cut — and read one block at a time. The store
never decides to push a document into the context: that is what lets it work
from a small window — a local model with a modest budget — without dragging
whole pages along.

## Non-goals

Deliberate, and worth knowing before filing an issue:

- **semantic / vector search** — the LLM already does semantics over a summary
  and an outline; embeddings inside the store are complexity without a reader;
- **storing secrets** — the index is plaintext, and the guard is a safety net,
  not a vault;
- **authentication of its own** — a shared store runs on OpenSearch's security
  plugin (per-user accounts and roles, document-level security; the recipes are
  in [docs/setup.md](docs/setup.md));
- **deciding for you** — a fact is evidence with an age, never a verdict;
- **another storage backend behind the same tools** — the queries are
  OpenSearch's (see [Storage](#storage)).

## Quick start

A 20-second tour (`hack/quickstart-demo.sh` against a throwaway node — add a
fact with provenance and a tag, read it back, correct it, and watch the journal
keep both values):

![velruna in a terminal: add, get, correct, journal](docs/media/quickstart.gif)

Build (Go 1.26+):

```
go build -o velruna .
```

Or install directly:

```
go install github.com/NemoNobody/velruna@latest
```

Or download the binary for your platform from
[Releases](https://github.com/NemoNobody/velruna/releases); with the repo
cloned, `scripts/install.sh` picks it, verifies the sha256 and puts it in
`~/.local/bin`.

Or install the MCP bundle from [Releases](https://github.com/NemoNobody/velruna/releases)
(`velruna-<version>.mcpb`): clients that speak the MCP Bundle format show a
configuration form for the OpenSearch URL, user and password — no JSON editing.

Or run the container (release tags only, `linux/amd64` + `linux/arm64`):

```
docker run -i --rm --user "$(id -u):$(id -g)" \
  -v ~/.velruna.conf:/etc/velruna/velruna.conf:ro \
  -e VELRUNA_CONF=/etc/velruna/velruna.conf -e OPENSEARCH_PASSWORD \
  ghcr.io/nemonobody/velruna:0.8.9 mcp
```

`--user` keeps a `0600` config readable (without it the image's uid 65532 needs
a `0644`/`chown 65532` file — see [docs/setup.md](docs/setup.md) §2).

The image is distroless and runs as uid 65532; a container has no session bus,
so the password comes from the environment or the mounted config, not from the
OS keyring — [docs/setup.md](docs/setup.md) §2 and
[docs/mcp.md](docs/mcp.md#running-from-the-container-image).

You also need a reachable OpenSearch (a single local node is enough). Quickest:
`make dev-opensearch` runs one in a container (localhost-only, security plugin
off, 512 MB heap, named volume — [docs/setup.md](docs/setup.md) §2). A system install follows
the [official documentation](https://docs.opensearch.org/latest/install-and-configure/install-opensearch/index/);
[docs/setup.md](docs/setup.md) covers what `velruna` expects.

Configure: copy `velruna.conf.example` to `velruna.conf` (or `~/.velruna.conf`).
Against the dev node — no TLS, security off, reachable only from this machine:

```json
{
  "opensearch_url": "http://127.0.0.1:9200",
  "opensearch_user": "dev",
  "opensearch_password": "dev",
  "allow_plaintext": true
}
```

Against a real cluster: `https://…`, drop `allow_plaintext`, and keep the
password out of the file — in the OS keyring (`velruna creds set`) or in
`OPENSEARCH_PASSWORD` for containers and headless setups. A plaintext file
password is only read when `allow_plaintext` is on, which is off by default.
The full annotated example (index names, TTLs, fetch options) is
`velruna.conf.example`; `insecure_skip_verify: true` is for a self-signed
local node only — verification is on by default.

The index is created automatically on first use. Env vars override the file:
`OPENSEARCH_URL`, `OPENSEARCH_USER`, `OPENSEARCH_PASSWORD`,
`OPENSEARCH_INSECURE_SKIP_VERIFY`, `VELRUNA_INDEX`, `VELRUNA_HISTORY_PERIOD`,
`VELRUNA_HISTORY_LIMIT`, `VELRUNA_SUMMARY_MIN_LEN`, `VELRUNA_SUMMARY_MAX_LEN`,
`VELRUNA_SECRET_GUARD`, `VELRUNA_ALLOW_PLAINTEXT`,
`VELRUNA_AUTHOR`, `VELRUNA_MODEL`, `VELRUNA_HOST`, `VELRUNA_SESSION`;
`VELRUNA_CONF` points at an explicit config file. Fetch
options (`velruna fetch`): `VELRUNA_DOCS_INDEX`, `VELRUNA_DOCS_TTL_DAYS`,
`VELRUNA_FETCH_TIMEOUT`, `VELRUNA_FETCH_MAX_BYTES`, `VELRUNA_FETCH_BLOCK_CHARS`,
`VELRUNA_FETCH_DOC_CHARS`, `VELRUNA_FETCH_HOSTS`, `VELRUNA_FETCH_USER_AGENT`,
`VELRUNA_FETCH_PROXY_URL`,
`VELRUNA_FETCH_PROXY_FALLBACK`, `VELRUNA_FETCH_RESPECT_ROBOTS`,
`VELRUNA_FETCH_ALLOW_PRIVATE`, `VELRUNA_FETCH_RENDER`,
`VELRUNA_FETCH_OBSCURA_PATH`, `VELRUNA_FETCH_OBSCURA_STEALTH`,
`VELRUNA_LOCAL_REPO_ROOTS`,
`VELRUNA_REPO_SCAN_DEPTH`.

The password resolves from `OPENSEARCH_PASSWORD`, then the OS keyring (service
`velruna`, account `host[:port]`), then the config file — the file only when
`allow_plaintext` is on. `velruna creds set` reads a hidden prompt (the password
is never taken from argv). Check the connection:

```
velruna status
```

Store and read back a fact:

```
velruna add demo:api:port "API port is 8080" endpoint -p demo -s tool:read_file
velruna get demo:api:port
velruna search "api port"
```

Fetch a web page into the docs index and read it back block by block:

```
velruna fetch https://www.postgresql.org/docs/current/tutorial.html
velruna doc <hash-from-output> 1      # the "Part I. Tutorial" block
velruna docs "tutorial"
```

The page is parsed with the HTML5 tokenizer (`golang.org/x/net/html`), split on
its headings, and each heading-delimited section is split into blocks: a section
longer than `fetch_block_chars` (default 8000 runes) is split at a line boundary,
never truncated. Blocks are cheap to read, so open the one you want instead of the
whole document — `--blocks` reads a run of them. `fetch_doc_chars` (default
100000; `0` = the whole document) is the storage budget: when it runs out, whole
blocks are dropped from the end and the answer says so (`truncated`,
`chars_stored`/`chars_total`) — a block is never cut in the middle. Code blocks
(`<pre>`) are stored with their newlines and indentation: a code sample whose
braces are missing is not a code sample. Site chrome is filtered, not stored:
when the markup names the article (`<main>`, `role="main"`, `<article>`) only
that container is read, `nav`/`aside`/`footer` are removed, and a short block of
at least two links with no letter outside them (a rail, a breadcrumb) is
dropped. The rule is deliberately strict: one word outside the links makes the
block prose, so a labelled menu survives — cutting a paragraph to remove a menu
is the worse mistake. A page that is nothing but links keeps its blocks — an
empty document would hide the loss. Prose outside any of these markers can still
carry a banner; the loss is reported rather than silent: `truncated` covers the
storage budget and dropped image references, never the chrome filter. A page over
the 2 MB `fetch_max_bytes` limit is refused with an error, not trimmed.

## Commands

| Command | Description |
|---------|-------------|
| `velruna add <key> <value> <category> [--project <p>] [--source <s>] [--notes <n>] [--summary <s>] [--allow-secret]` | Add or correct a fact (prints `added`/`updated`/`touched (same value)`; the accepted value is appended to the journal; values over `summary_min_len` require `--summary`) |
| `velruna get <key> [--history]` | Get a fact by key (history is off by default; `--history` prints the legacy in-doc chain, or the newest journal entries for journal-era facts — the full journal is one `velruna journal` away) |
| `velruna search <query> [--project <p>] [--category <c>] [--status <s>] [--limit N]` | Full-text search over values, notes and summaries (`--status any` disables the status filter; `--limit 0` = up to 1000, larger values are clamped to it) |
| `velruna keys [prefix] [--match <s>] [--project <p>] [--status <s>]` | Find facts by key: prefix and/or substring (`*`, `?` wildcards in `--match`), across projects |
| `velruna list <project> [--prefix <p>]` | List all facts for a project, optionally narrowed to a key prefix (trailing `*` allowed) |
| `velruna journal <key> [--limit N]` | Read the journal: the append-only history of a key, newest first |
| `velruna history-info <key>` | Compact journal overview: writes, rediscoveries, time range (no entry text) |
| `velruna prune <key> [--keep N] [--yes]` | Drop old journal entries, keeping the newest N (manual admin tool: destructive, without `--yes` only the plan is printed) |
| `velruna migrate-history [--project <p>] [--yes]` | Move pre-journal in-doc history arrays into the journal (one-time; plan first, `--yes` to write) |
| `velruna obsolete <key> [reason] [--allow-secret]` | Mark a fact obsolete because its subject is gone (reason appended to notes) |
| `velruna status [--conf <path>]` | Check connectivity and show the effective configuration (env overrides are listed by name, never values) |
| `velruna creds set\|get\|status\|rm [--url <u>]` | Manage the OpenSearch password in the OS keyring: `set` reads a hidden prompt (piped stdin for scripts), `get` prints the stored password (for scripts that need it), `status` shows only the source (`env\|keyring\|file\|none`), `rm` deletes the entry |
| `velruna fetch <url> [--refresh] [-p <p>] [--allow-secret] [--max-chars N] [--block-chars N]` | Fetch a page into the docs index and print its outline: cache-aware, blocks are read on demand; `--max-chars 0` stores the whole document (default `fetch_doc_chars`), `--block-chars` sets the target block size; github URLs that match a local clone print the file path |
| `velruna doc <hash> [n\|--section N] [--blocks N] [--full]` | Read a stored document: the outline (default), block `n`, `--blocks N` consecutive blocks, or every block with `--full` (answers are capped at 32000 runes; `next` names the next block; one block is always returned whole even when it alone exceeds the cap) |
| `velruna docs <query> [--limit N] [-p <p>]` | BM25 search over stored documents (the extracted blocks, never the raw page) |
| `velruna mcp` | Run as an MCP server over stdio |

Exit codes: `0` success, `1` any error, `3` the named key does not exist
(`get`/`obsolete`/`prune`) — scripts can tell a missing fact from an
unreachable store without parsing stderr.

Key convention: the key names a **stable subject**, not its current value —
`demo:api:port`, not `demo:api:v1`. Re-using the same key with a new value is a
correction: the new value is journaled and the fact points at it; re-adding the
same value updates notes only and leaves the journal untouched.

## MCP server

`velruna mcp` speaks MCP over stdio, so agents (VS Code, others) call
`velruna_status`, `velruna_search`, `velruna_get`, `velruna_add`,
`velruna_add_batch`, `velruna_patch`, `velruna_patch_batch`, `velruna_list`,
`velruna_keys`, `velruna_journal`, `velruna_history_info`, `velruna_prune`,
`velruna_obsolete`, `velruna_fetch_doc`, `velruna_get_doc` and
`velruna_search_docs` directly. Reads omit
history unless
`include_history=true` is passed (that chain is the legacy pre-journal
in-doc array; journal history is read with `velruna_journal` /
`velruna_history_info`), and carry `age_days`/`stale`. `velruna_list` is
compact by default (a table of contents: key/summary/state/age — the full
value is one `velruna_get` away) and newest-activity-first; cheap partial
updates (dated note, status flip) go through `velruna_patch` without
re-sending the value; `velruna_add_batch` packs the session-end ritual into
one round-trip. Registration and
troubleshooting: [docs/mcp.md](docs/mcp.md).

## Use with agents

An importable rule lives in [rules/velruna.md](rules/velruna.md): copy it — or
just its compact block — into `AGENTS.md`, `.github/copilot-instructions.md`,
`.cursor/rules/`, or your editor's always-on instructions (drop the YAML
frontmatter when pasting into a rule file). The file is also shaped as a skill:
put it at `<skills-dir>/velruna-memory/SKILL.md` to have an agent load it on
demand. It tells the agent when to consult and update memory, and how to keep it
honest.

## Dashboards

`internal/dashboards/objects.ndjson` (embedded in the binary) — a ready-made
OpenSearch Dashboards overview ("Velruna — store overview"): KPI row for
facts/journal/docs, activity trends, breakdowns by project/category/status,
journal ops and the top fact keys. `make dev-opensearch-dashboards` pushes it
automatically on the dev stack; elsewhere run `velruna dashboards push`.
Install instructions and caveats: [docs/dashboard.md](docs/dashboard.md).

## Storage

### Why OpenSearch (and not files or SQL)

The store has to answer four questions at once: full-text lookup over values and
summaries, aggregation over projects/tags/operations, an append-only history
you can query by time, and safe concurrent access for more than one machine or
user. Options we weighed:

- **Files (one JSON/YAML per fact)** — simplest to read by eye, but every
  property above would be rebuilt by hand: search, locking, partial-write
  recovery, a history that is queryable rather than a pile of files, and
  per-project access control. The filesystem becomes the schema.
- **A wiki-style backend (files + a search index, the MediaWiki shape)** — it
  works and it is fast, but the index is a second system to operate, a fact
  becomes page text, and history is page revisions instead of typed
  observations. Same operational surface as OpenSearch, less query power.
- **SQLite/Postgres** — transactions are genuinely better than what we have;
  we trade them for BM25, aggregations, index-per-period history and a query
  language that fits "find by summary, group by project, count by op". Value
  corrections here are last-writer-wins, not CAS ([docs/how-it-works.md](docs/how-it-works.md)).
- **OpenSearch** (chosen) — one engine for all four questions above, and
  comfortable as a shared, inspectable catalogue rather than a private blob.
  One local node is enough to start.

The cost is honest: a JVM service to run and a plaintext store to protect. That
is the deliberate trade; the security posture is below and in
[docs/setup.md](docs/setup.md).

Facts live in a single OpenSearch index (default `velruna`). One node is enough
to start; a proper cluster works the same way. How to point `velruna` at a
cluster: [docs/setup.md](docs/setup.md). Schema and design:
[docs/how-it-works.md](docs/how-it-works.md). Backup and restore (the standard
OpenSearch snapshot API): [docs/backup.md](docs/backup.md).

**Secrets do not belong in facts** (passwords, tokens, private keys) — a rule for
agents and humans, not a guarantee: the store is plaintext, and the
best-effort secret guard (default `reject`) scans writes. A high-confidence
credential refuses the write; a heuristic hit (an unnamed 40-character hash, a
base64 blob, an opaque token, a weak assignment) is replaced with
`[velruna-redacted:pattern]` before the fact is stored and listed in the
answer's `redactions`, so a legitimate fact is not lost to a false positive —
but the stored value is then shorter than the one written; a hit in an identity
field (`key`, `project`, `source`, `keywords`) refuses, because a rewritten key
is a different fact. A secret with no
known shape passes. The rule stays the first line
of defense. Fetched pages get the same guard: high-confidence credential
shapes are redacted out of the stored snapshot; on documentation pages the
unnamed token shapes are ordinary content and are kept (counted in the
answer as `guard_medium_ignored`), while API payloads (JSON/XML) refuse the
write. Details: [docs/how-it-works.md](docs/how-it-works.md).

## JS rendering (obscura, opt-in)

Some documentation sites answer a plain fetch with 403/422/429 — they gate
on the TLS/HTTP2 fingerprint, not the content. `fetch_render: "obscura"`
enables a fallback: when a plain fetch fails the way bot walls fail (network
walls, IP-ban statuses — not 404/401, not the allowlist, not oversize), the
the URL goes to [obscura](https://github.com/h4ckf0r0day/obscura) (Apache-2.0),
a lightweight headless browser with a Chrome-shaped stealth fingerprint. The rendered HTML
flows through the exact same extract + guard + store pipeline (`via:
obscura`).

It is opt-in by design: obscura is a third-party binary (~75 MB, linux
x86_64, glibc 2.35+) that **executes page JS locally**. Install it pinned:

```
make obscura   # sha256-pinned v0.2.6 stealth build → ~/.local/bin
```

then set `fetch_render: "obscura"` (env `VELRUNA_FETCH_RENDER`).
`fetch_obscura_path` points at the binary (default: `obscura` via PATH);
`fetch_obscura_stealth` (default `true`) passes `--stealth`.

Egress rules: the proxy (`fetch_proxy_url`, validated at config load — every
command, not just fetch) and the environment proxy are honored by the
renderer; a proxy it cannot forward fails the render loudly, never a direct
render behind it. A direct render resolves the target through the same
public-address rule as the plain fetch (no internal addresses). See
docs/setup.md §8 for the full egress and SSRF model.

v1 limits, stated plainly: the dump mode does not report the post-redirect
URL, so a rendered doc stores `final_url` = the requested URL; the renderer
is serialized (one render at a time); `robots.txt` (honored by default via
`fetch_respect_robots`) gates both the plain and the render path; the
URL is visible in the renderer's argv (`/proc`) — pages fetched with secrets
in query strings stay a plain-fetch concern.

## Platform notes

The Linux keyring provider (native D-Bus Secret Service) is the daily driver
and is exercised live. The darwin/windows providers (system Keychain /
Credential Manager via zalando/go-keyring) are exercised by CI too: the
`windows` job runs the keyring round-trip on every push and the `macos` job
runs it against the runner's login Keychain on tags and manual dispatch
(macOS runners bill 10x minutes, so a release check is the right cadence).
Neither has been smoke-tested by the author on real Apple/Windows hardware.
Release binaries cover linux/amd64, linux/arm64, darwin/amd64, darwin/arm64
and windows/amd64; only linux/amd64 is smoke-tested by the author — reports
from the other machines are welcome and move this section. If the keyring
misbehaves on your platform, open an issue.

## License

[Apache-2.0](LICENSE)

