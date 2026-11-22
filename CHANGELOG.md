# Changelog

Notable changes per release tag. `v0.8.6` is the first release cut for public
consumption; the entries below it are kept as history. Work in progress lands
under `## Unreleased`; the release commit moves that section under its version
number (`.github/workflows/release.yml` reads the heading to build the release
notes).

## Unreleased

### Added

- MCP bundle (`.mcpb`): every release now attaches a universal
  `velruna-<version>.mcpb` plus per-OS `-darwin`, `-linux` and `-windows`
  bundles. A desktop client installs one with a configuration dialog —
  OpenSearch URL, user, password and the TLS-skip flag, no JSON editing. The
  universal bundle carries all five builds and a launcher that picks the right
  one; a per-OS bundle carries only that OS's builds and declares just that
  platform. `make mcpb` builds the universal one locally.
- `superseded_by`: a fact can name the record that replaces it (one link per
  fact; a later patch may repoint it — each event is journaled), target
  verified, cycle-free. A link-only patch journals `op=supersede`; a patch
  that also changes the value or retires the fact keeps that event's op and
  its entry states the link. Returned by `get` and `journal`; a link that
  points back in time is flagged in the journal. See docs/how-it-works.md.
- several store nodes: `opensearch_urls` / `OPENSEARCH_URLS` accepts a comma
  separated list and validates every node, so a client can reach a cluster
  without a proxy in front of it. `status` reports the list; an unreachable
  store now suggests where to look.
- a schema version stamped into the facts index (`mappings._meta`: `SchemaVersion = 1`):
  a store written by a newer binary is refused before anything is written,
  while a store from an older one is upgraded in place (additive mapping).
- deployment: a secured stack profile with Dashboards (`secure-dash`), server
  knobs (`VELRUNA_OS_HEAP`, `VELRUNA_CLUSTER_SEEDS`), and a
  `workflow_dispatch` smoke pipeline that boots the single-node stack, runs
  `cli-smoke`, pushes the dashboard bundle and checks every panel through the
  cluster.

## v0.8.10 — 2026-11-21

### Changed

- license: **Apache-2.0** replaces GPL-3.0. Releases up to and including
  v0.8.9 remain under GPL-3.0; from this release on the project is permissive
  (attribution + an explicit patent grant), which also unblocks the Docker MCP
  Catalog, whose registry rejects copyleft licenses outright. Copyright stays
  with the author; `LICENSE` carries the full text.


## v0.8.9 — 2026-11-21

### Added

- `velruna dashboards push`: installs the bundled "Velruna — store overview"
  (3 index patterns, 18 visualizations, the dashboard) straight from the
  binary — the bundle is embedded, credentials come from the same chain as the
  MCP server. Flags: `--conf`, `--index`, `--file`.
- container image `ghcr.io/nemonobody/velruna` (distroless, nonroot, linux
  amd64 + arm64), published by `.github/workflows/image.yml` for release tags;
  compose profile `app` and the `make stack-up` / `make stack-down` shortcuts.

### Changed

- the dashboards push no longer needs python3, bash or a repository checkout:
  the saved-objects write lives in `internal/client`, the bundle in
  `internal/dashboards` (regenerate with `docker/dashboards/gen_fields.py`,
  then rebuild — the bundle is compiled in).

### Removed

- `scripts/push-dashboards.sh`, superseded by `velruna dashboards push`; its
  `VELRUNA_OS_URL` / `VELRUNA_OS_USER` variables are replaced by the standard
  `OPENSEARCH_URL` / `OPENSEARCH_USER`. The script's `VELRUNA_KIBANA_INDEX` is
  now `--index`, and its `VELRUNA_DASHBOARDS_ENV` / `DASHBOARDS_USER` /
  `DASHBOARDS_PASSWORD` fallback is gone: credentials come from
  `OPENSEARCH_PASSWORD`, the OS keyring, or the config file.

## v0.8.8 — 2026-11-21

Rename only; behaviour is unchanged. Published as `memento` up to v0.8.6, and
as `velruna` from this release.

- module path: `github.com/NemoNobody/memento` -> `github.com/NemoNobody/velruna`.
- MCP server name and all 16 tools: `memento_*` -> `velruna_*`.
- environment variables: `MEMENTO_*` -> `VELRUNA_*`; the config lookup now
  finds `velruna.conf` in the working directory, next to the executable, or
  `~/.velruna.conf`.
- keyring: the service name and the KWallet application id are `velruna` now,
  so an existing credential is not found — run `velruna creds set` once.
- default index names: `velruna`, `velruna-docs`, `velruna-history-<period>`.
  The binary does not migrate an existing store: point `index_name` and
  `docs_index_name` at the old names, or copy the indices to the new ones.
- release assets and the installer: `velruna-<os>-<arch>`.
- the redaction marker is `[velruna-redacted:...]`.
  Markers written before the rename (`[memento-redacted:...]`) are still
  recognized as placeholders.
- local paths: the dashboards credential file moves from
  `~/.config/memento/dashboards.env` to `~/.config/velruna/dashboards.env`, and
  the dev compose project and volume names gain the `velruna` prefix, so a dev
  stack starts on a fresh volume.

## v0.8.6 — 2026-11-20

- keyring: provider detection on Linux — the bus owner of
  `org.freedesktop.secrets` is probed, and an unusable provider is a named
  refusal instead of a place to keep credentials: KDE with gnome-keyring
  owning the name (ephemeral master key, credentials die at logout) and no
  owner at all are both refused.
- keyring: a KWallet-native backend (`org.kde.kwalletd6`, wallet `kdewallet`,
  folder = service, key = account) when no usable Secret Service provider
  exists.
- keyring: every call is bounded by a deadline, so a locked wallet's GUI
  prompt ends as a "wallet locked" error instead of hanging the server.
- `memento creds set` refuses on an unusable backend with a hint
  (`OPENSEARCH_PASSWORD` or a plaintext config), instead of reporting a
  successful write.
- keyring: the KWallet D-Bus methods take the caller's appId as their **last**
  argument (`writePassword(handle, folder, key, value, appid)`), not the wallet
  name. Passing the wallet name there makes kwalletd return an empty folder
  list and `-1` on every write, and after a few such calls it warns about
  repeated failed attempts to unlock the wallet.
- keyring: the session-bus connection is never closed. `dbus.SessionBus`
  returns a process-wide connection, so closing it after one call broke every
  later call in the same process (an MCP server reads the password repeatedly).
- client: the additive mapping reconcile, the journal infrastructure install,
  the journal index pipeline pinning and the writer-label probe
  (`GET /_security/authinfo`) run once per store per process instead of once
  per client. The MCP server builds a client per tool call, so every call paid
  a `PUT _mapping`, every append paid two or three extra requests and every
  write re-probed the label. Concurrent callers wait for the first pass rather than
  skipping it (a write racing the old code could pin a field with the dynamic
  mapping, or write a journal entry before the ingest pipeline was pinned).
  The work is assumed stable for the process lifetime: restart after a cluster
  swap, a deleted template or a topology change (the journal template's replica
  drift check and the "externally managed" verdict are made once per process).
  A read-only or DLS role cannot manage mappings: the 403 counts as
  "externally managed" and closes the pass, so reads do not pay a refused PUT
  on every call.
- docs: on KF 6.13 KWallet can serve `org.freedesktop.secrets` itself when
  kwalletd is started by PAM and the bus name is free, so `ksecretd` is
  optional there; gnome-keyring must stay out of the session, otherwise it
  takes the name and the ephemeral-keyring refusal applies.

## v0.8.5 — 2026-11-20

Second pass over v0.8.4: three of the five fixes there were too coarse.

- `<template>` inside `<head>` was inert content, but closing the head popped it
  and un-skipped its text: a template's content leaked into the page. The head
  now closes only when no dropped subtree (template, script, style) is open above
  it;
- text after an implicitly-ended `<head>` did not close it, so `<head>…</head
  missing>VISIBLE` lost the text (and an `<img>` in that spot was not collected).
  Both text and start tags close the head now, and the head check runs before
  image collection;
- `math` is not opaque after all: a `<title>` inside MathML is an ordinary HTML
  element, so the scan follows document order instead of jumping to `</math>`;
- opaque containers that nest (`svg`, `template`) are skipped to their matching
  close tag, not the first one, and constructs inside them are not markup: a
  commented-out `<svg>`, a CDATA block or a `</svg>` inside a script no longer
  ends the skip early;
- closing an implicit head is O(1) when no head is open: the check runs on every
  start tag, and scanning the stack there made a 2 MB page of open tags take
  minutes;
- the head closes before image collection, so an `<img>` right after `<head>`
  (or after `<meta>`/`<link>`) is collected instead of skipped with the head;
- tests: text closing an implicit head, an image right after head elements, a
  template in the head, nested templates, comments and CDATA inside `svg`, the
  image cap on the tokenizer path, the MathML title, and a start-tag run sized to
  catch the head check going linear; the v0.8.4 changelog date now matches its tag.

A second review of the same scans found more edges of that class:

- a tag name runs to whitespace, `/` or `>`: `<title-x>` and `<script-widget>`
  are custom elements, not `title` and `script`, so the scanner no longer reads a
  title out of them;
- a CDATA section inside `svg` is text up to `]]>`, not a bogus comment that ends
  at the first `>`: a `<title>` inside it is SVG content, not the page title;
- `noscript` is raw text (the tokenizer reads it that way with scripting on), so
  a `</template>` inside it does not close the enclosing inert template;
- `plaintext` never ends: the rest of the page is text, so a document that opens
  it has no title, and its `</plaintext>` is not a close tag;
- form feed is whitespace, so it terminates a tag name and an attribute value;
- a quote inside an unquoted attribute value (`href=/x?q="`) is part of the
  value: the tag ends at the first `>`, as in the tokenizer;
- a processing instruction, DOCTYPE or other bogus comment ends at the first
  `>`, so a `<title>` inside one is not the page title;
- a mismatched close tag inside `svg` closes the elements it names — the tree
  parser's recovery — instead of being ignored, so an svg ends where a browser
  ends it;
- in the fallback, a close tag for an element below an open `<template>` is
  ignored (template content is inert until `</template>`), and a repeated
  `<head>` is ignored the way the parser ignores it;
- tests: custom-element names, CDATA and mismatched closes inside `svg`, raw-text
  `noscript` and `plaintext`, form feed, a quote in an unquoted value, a title
  inside a processing instruction, and the two fallback cases.

Documents are stored as blocks, not truncated prefixes:

- a section longer than `fetch_block_chars` (default 8000 runes) is split into
  blocks at a line boundary (then at a space or punctuation, then hard), and the
  parts carry a `(2/5)` suffix; concatenating the blocks gives the section back,
  so a split loses nothing;
- `fetch_doc_chars` (default 100000, `0` = whole document) caps the stored text;
  the budget drops whole blocks from the end and sets `truncated`, never cutting
  a block in half (a first block larger than the budget is stored whole: an
  empty document would hide the loss);
- `memento_fetch` takes `max_chars` and `block_chars` per call; asking for more
  than was stored re-extracts from the saved html without the network (the guard
  runs again over the re-extracted text — a credential split by markup is only
  visible there — and the audit lists are merged), and asking for less never
  shrinks a stored copy whose content is unchanged;
- `memento_get_doc` takes `blocks` (1..10) and caps one answer at 32000 runes,
  returning `next` when more remains; one block is always returned whole even
  when it alone exceeds the budget; `full` hits the same budget with an explicit
  `next` instead of silently returning megabytes;
- the outline lists the first 60 blocks and summarizes the rest with their
  orders and size: a response limit, not a storage limit;
- `content_hash` now covers the page's full extracted text before the budget
  (`full_chars` records its length), so it does not depend on `max_chars` or the
  block size;
- the old section/document caps (16 / 4000 / 24000) are gone: a section count is
  not a limit, and `truncated` means a real loss (the budget or dropped image
  refs; a page over `fetch_max_bytes` is refused, not trimmed);
- `memento fetch` gains `--max-chars`/`--block-chars`, `memento doc` gains
  `--blocks`, and the new keys are in the config example and environment
  (`MEMENTO_FETCH_BLOCK_CHARS`, `MEMENTO_FETCH_DOC_CHARS`).

## v0.8.4 — 2026-11-20

Extraction edge cases found by the release review.

- the title scan skips three containers it missed: `noframes` (raw text like
  `script`), `math` (foreign content, like `svg`), and `<![CDATA[...]]>`, which
  in HTML is a bogus comment and ends at the first `>`;
- the tokenizer fallback (markup nested past the parser's stack cap) closes an
  implicitly-ended `<head>` on the first element that cannot live in it, not only
  on `<body>`: a head left open used to swallow the page;
- a link that wraps block content through an inline wrapper
  (`<a><span><div>…</div></span></a>`) is formatting like a direct wrap, not a
  menu label;
- tests pin the image cap (one over the cap reports truncation, exactly the cap
  does not), the three title containers, the nested-block link, and a head left
  open without a `<body>`. The linearity test comment now says what it can and
  cannot catch: the parser's 512-level stack cap bounds the depth-times-candidates
  shape, so that one is pinned by behaviour tests instead.

## v0.8.3 — 2026-11-19

Extraction runs on the HTML5 parser, and code blocks survive it.

The hand-written scanner that grew out of the first `fetch` had produced a
steady stream of bug classes — tags inside comments, quotes inside attributes,
`<` in prose, CRLF separators, unclosed elements — each one a silent difference
from what a reader sees. The page is now parsed with `golang.org/x/net/html`
(BSD-3) and read as a tree. One dependency replaces four hundred lines of
almost-HTML.

- `<pre>` text is stored verbatim: newlines and indentation are the content of a
  code block, and the old line filter used to delete every `}`;
- inline elements no longer split a sentence: text nodes are joined before a
  line is folded, so `In Go a simple <code>if</code> looks like this:` reads as
  written instead of losing short words to the junk filter;
- sections take their heading from the heading element, not from the first line
  of text (`If`, not "In Go a simple if looks like this:");
- the chrome filter tells a menu from prose by the letters outside the links: a
  block of two or more links with no word outside them is a rail and is dropped;
  any word outside a link makes it prose and it stays. The rule is deliberately
  strict — a labelled menu (a version picker, a "Help improve" block) survives,
  because cutting a paragraph to remove a menu is the worse mistake. A link that
  wraps block content is formatting, not a menu label;
- a content root that yields nothing falls through to the next candidate
  (`<main>` → `role="main"` → a lone `<article>` → the page);
- markup nested deeper than the parser's 512-element stack cap takes a
  tokenizer fallback: the text is kept in one section, headings are lost.

Accepted, and visible in tests: an unclosed `<nav>`/`<aside>`/`<footer>` owns
the rest of the page in the tree (browsers agree), so its content is treated as
chrome, and a page that is nothing but chrome still keeps its sections.

Checked against the Python, Go, PostgreSQL, MDN and Kubernetes documentation:
code examples keep their braces and indentation, headings are the real headings,
and no prose section is dropped — a labelled picker or footer line may now be
stored where a looser rule would have removed it.

## v0.8.2 — 2026-11-19

Extraction stores the article, not the site around it.

A fetched documentation page kept its sidebar as the first sections: the markup
that says "this is the article" was ignored, so a link rail that comes first in
the source ate the section and character budgets before the real text started,
and the outline read as navigation.

- a page that names its content container (`<main>`, `role="main"`,
  `<article>`) is read from there, falling back to the whole page when there is
  none or when it yields nothing;
- `nav`, `aside` and `footer` are dropped with the other non-text elements;
- a short block that is mostly anchor text is treated as a link rail. The ratio
  is computed over non-whitespace runes: block tags inject a newline per
  element, and counting those padded a rail until it read as prose;
- a page that is nothing but link blocks keeps its sections — an empty document
  would hide the loss instead of showing it;
- a dropped element that is left unclosed loses only its tag, and leaves a line
  break behind: its text stays readable instead of merging into the paragraph
  before it.

Checked against the Python, Go and PostgreSQL documentation: no navigation
sections, and a page with a content container is no longer truncated by its own
sidebar.

## v0.8.1 — 2026-11-18

Secret guard: a heuristic hit no longer costs the whole write.

`secret_guard=reject` (the default) refused any write whose text matched a
medium-confidence heuristic — an unnamed 40-character hash, a base64 blob, a
weak assignment. Those shapes are ordinary, legitimate content (commit hashes,
checksums, config names), so a valid fact could not be stored at all, and the
only escape was to turn the guard off globally.

- a **high-confidence** credential (vendor prefixes, PEM blocks, JWT, Bearer
  headers, basic-auth URLs, strong assignments) still refuses the write:
  storing a marker in its place would let a writer believe the token was saved;
- a **medium-confidence** hit in a text field (`value`, `notes`, `summary`,
  `notes_append`, `reason`) is replaced with `[memento-redacted:pattern]` and
  the write proceeds; the answer lists `redactions` (field, pattern, size —
  never the text), and the rows are stored on the fact and in the journal
  entry of the accepted value;
- a hit in an **identity** field (`key`, `project`, `source`, `keywords`)
  refuses: a rewritten key would silently become a different fact;
- a text field that is nothing but a flagged span is refused — a fact whose
  value is a bare marker would read as a complete claim;
- a slash-joined list of word-like identifiers (`contrastControl/colorControl/
  sharpnessControl/tintControl`) is no longer read as an opaque token: the
  heuristic matched it only because `/` belongs to the base64 alphabet. This
  narrows the false-positive class; a secret made only of mixed-case letters
  and `/` still passes, which is why "never store secrets" remains the rule;
- `ask` with confirmation stores the text as written (a human judged the
  finding a false positive); `reject` still has no confirmation flag.

Docs fetching honors `robots.txt` by default (`fetch_respect_robots: true`):
the store fetches on behalf of every session, a per-site opt-out is one config
line, so there is no reason to ship the rude default. The User-Agent stays a
browser-shaped string (many documentation sites 403 known bot tokens); the
identity is config, not a fingerprint.

`memento_patch` records the audit with the value it stores and clears it when a
clean value replaces a redacted one; rows cut from qualifiers (a note, a retire
reason) merge with the current value's rows. The audit is part of every read
shape — `memento_get`, `memento_search`, `memento_list`, `memento_journal`,
CLI `get` — so a shortened value never reads as the complete claim. Fetched
pages are unchanged: the guard there is route-based (documentation keeps medium
shapes and counts them in `guard_medium_ignored`, API payloads refuse), because
page text is not authored by the writer.

## v0.8.0 — 2026-11-11

First public baseline. Everything below the fold landed in the same cycle:
append-only journal history, facts/docs split, dashboard bundle, keyring
providers, Windows/macOS CI. Headlines of the cycle that ended here:

- **categories** `recipe` (TTL 180d) and `rule` (90d); the category TTL is
  also the cap for a writer-chosen per-fact `ttl_days`;
- **keywords** on facts (normalized tags, exact-term filter) and an additive
  mapping reconcile that upgrades existing indices on first use;
- **re-verification**: `confirmed_by` (who verified the *current* value,
  user+host per entry) plus `memento_patch reverify` → `observed_at` moves and
  the journal gains `op=rediscovery`, so a stale fact can become fresh again;
- **`op=retire`** marks the verified→obsolete transition in the journal;
- **journal time windows** (`since`/`until`), `keywords` on patch;
- **conditional GET** for the docs cache (ETag/Last-Modified → 304 keeps the
  snapshot and moves only the timestamps);
- **status** reports per-index sizes, replica drift and the effective fetch
  configuration;
- **dashboard**: docs-by-via/host/project donuts and a TTL KPI on `expires_at`;
- **CI**: real keyring tests (Secret Service, Keychain, Credential Manager),
  Windows job on every push, macOS job on tags/dispatch;
- docs: advisory contract and precedence, "Not yet covered" test inventory.

History journal: the in-doc `history` array is replaced by an append-only
journal of dated indices (`<index>-history-<period-date>`, monthly by default;
`history_index_period` config, `MEMENTO_HISTORY_PERIOD` env).

- Doc-first journaling: the fact doc update lands first, the journal entry
  is appended only once the cluster accepted the write — a refused update
  leaves no journal trace (the entry id is preallocated and carried into the
  update as `current_ref`). Script sources stay escape-free to match the
  cluster painless dialect (separators arrive as params; a lint test pins
  it). The journal is write-forever
  (entries are never rewritten, no CAS); `history_limit` is now only the
  default `--keep` for the manual `memento prune`.
- New commands: `memento journal <key> [--limit N]`, `memento history-info
  <key>`, `memento migrate-history [--project p] [--yes]` (one-time move of
  pre-journal in-doc arrays); new MCP tools `memento_journal`,
  `memento_history_info` (13 tools).
- Journal infrastructure (server-clock ingest pipeline, index template) is
  created automatically on the first accepted write. Dated indices keep old
  periods cheap to inspect, snapshot, or delete; after an admin deletes old
  journal indices, read counts are lower bounds.
- Backups: snapshots must include `memento-history-*` (docs/backup.md).
- Optional UI: `make dev-opensearch-dashboards` adds OpenSearch Dashboards to
  the dev compose (profile-gated); `scripts/push-dashboards.sh` imports a
  ready-made bundle (index patterns for facts/journal/docs, visualizations,
  an overview dashboard) — idempotent. Credentials resolve wallet-first via
  the new `memento creds get` (prints the resolved password to stdout for
  scripted use; never argv), overridable by `DASHBOARDS_USER`/
  `DASHBOARDS_PASSWORD`, with `~/.config/memento/dashboards.env` as the
  headless fallback.

Authorship and summary:
- Every write is stamped with self-reported audit labels: `author` (resolved
  from the store's security plugin, else config `author`, else the OS user),
  `author_source` (`security`/`config`/`os`/`none`), `model`, `host`,
  `session`. Birth labels are write-once; later writes move `updated_by`.
  Journal entries carry the writer who appended them.
- Long values stay skimmable: a value over `summary_min_len` (default 2000)
  requires a `summary` (CLI `--summary`, MCP `summary`) capped at
  `summary_max_len` (default 400). The rule covers accepted values; notes
  touches of old long facts are never rejected. `memento_search` matches
  summaries: the search fields are value, notes and summary.
- Config keys `author`/`model`/`host`/`session`/`summary_min_len`/
  `summary_max_len`; env `MEMENTO_AUTHOR`/`MEMENTO_MODEL`/`MEMENTO_HOST`/
  `MEMENTO_SESSION`/`MEMENTO_SUMMARY_MIN_LEN`/`MEMENTO_SUMMARY_MAX_LEN`.
  `memento status` reports the effective author and its chain.

MCP ergonomics (context economy, partial updates, batch):
- `memento_list` is **compact by default** (`compact: false` restores full
  rows) and **sorted by `updated_at` desc** by default (`sort: "key"` for the
  old stable order). A compact row carries key/summary/category/status/
  notes-head/age; the full value is one `memento_get` away (~5x tokens saved
  on the most expensive call in a session). `memento_keys` is always compact.
  **Breaking** for clients relying on full rows / key order.
- New `memento_patch`: partial update of an existing fact — value, summary,
  source, notes replace, atomic `notes_append` (dated line, applied
  server-side so concurrent writers cannot drop a note), status flip
  (a transition is journaled; obsolete→verified sets `observed_at=now`;
  `reason` goes with obsolete). On a value change the old summary is cleared
  unless a new one is provided (a stale table of contents is a lie).
- New `memento_add_batch` (1..10 facts per call, the session-end ritual in
  one round-trip): per-item guard and `confirm_secret`, duplicate keys
  rejected, partial success reported per item.
- New `memento_patch_batch` (1..10 patches per call): the same batch shape
  and per-item contract as `memento_add_batch`, `memento_patch` field
  semantics per item (never creates).
- `memento_get` accepts `keys: [...]` (up to 10) for batch reads
  (`items[]` with per-key `found`).
- `memento_add` answers say what happened: `created`, `changed` (false =
  same-value touch) and `previous_summary` (the superseded table of
  contents). `memento_obsolete`/status flips are journaled (audit symmetry).
- `memento_search_docs` hits carry the MATCHED section (`section: {order,
  heading, snippet}` centered on the match) — `memento_get_doc(section=order)`
  reads it in full; doc-level snippet remains the fallback.
- `memento_search`/`memento_list` zero-hit answers carry a navigation `hint`;
  `memento_search` is **compact by default** (`compact: false` restores full
  rows; a hit's summary is the table of contents, the value is one
  `memento_get` away). **Breaking** for clients relying on full search rows.
- `memento_status` reports `mcp_version`, `commit`, `started_at` (the running
  binary, not the repo on disk); `version` renamed to `opensearch_version`.
  **Breaking** for clients reading `version`.
- Keys are trimmed at every edge (add/patch/get/keys/journal/obsolete/prune;
  the batch dupe check uses the trimmed key): `" k "` and `"k"` never become
  two docs. A compact answer never ships history (full rows only).
- Answers say what the write did: a same-value touch (`memento_add`/
  `memento_patch` with `changed: false`) carries a `note` spelling out that
  only notes/source moved — `observed_at`, age and the journal untouched. The
  batch answers add summary counters (`created`/`changed`/`touched`).
- Reads carry `age_hours` next to `age_days`: a fresh fact with `age_days: 0`
  can be 1 hour or 23 hours old, and the trust decision depends on it.
- 16 MCP tools.

Fetch guard triage and fetch v1 fixes:
- The secret guard now grades findings: `high` (vendor prefixes, PEM blocks,
  basic-auth URLs, `Bearer`, assignments with a 20+ char multi-class secret) vs
  `medium` (long hex/base64, weak assignments like `password = someVarName`).
  On fetched pages: high-confidence shapes in agent-visible fields are REDACTED
  in place (marker `[memento-redacted:pattern]`) and the page is stored — the
  answer lists `redactions` (field/pattern/size, no preview) plus a `warning`;
  redaction runs before `size`/`content_hash`, so metadata matches the stored
  text. Medium findings in visible text route on the stored `content_type`:
  documentation types (html/markdown/plain) keep the page and surface
  `guard_medium_ignored: N` (CLI outline, MCP field) — refusing on asset
  hashes and quoted SHAs made documentation unreadable; API types
  (`application/json|xml`, `+json`/`+xml`) still refuse the write (confirmable
  at `ask`, no bypass at `reject`).
  The URL and the post-redirect `final_url` are scanned high-confidence only
  (identity fields: a git SHA in a GitHub path is ordinary; a
  credential-bearing redirect target refuses the doc); image hrefs are
  redacted in place; the credential-assignment rule reads quoted JSON/YAML
  config lines (`"api_key": "S3cr3t"`); the agent-supplied `project` tag is
  guard-scanned on every write path (facts and docs — for cached docs at the
  call, the re-tag path never sees the content guard).
- The stored raw HTML is redacted with the high-confidence set, twice: the
  second pass closes the common overlap-tail case (a span starting inside an
  earlier replacement; the marker rule refuses a right-hand side starting with
  `[`, so the pair is a fixed point). A greedy span can still swallow a scheme
  prefix — the residual exposure is disk-only, the stored HTML is never served
  to agents. Its redactions join the audit trail as field `html`.
- `memento obsolete` refuses a reason on an already-obsolete fact (mirrors
  `memento_patch`): the journal skips a re-retire, so notes must not grow an
  audit line the journal never recorded.
- Keys, project labels and doc hashes are trimmed at every CLI and MCP edge
  (`add`, `get`, `obsolete`, `prune`, `journal`, `history-info`, `list`,
  `search`, `keys --match/--project`, `doc`; MCP read filters and the MCP
  project default included) — a padded filter never answers a silent zero.
- A same-value `add` on an obsolete fact resurrects it with a journal entry
  and moves `observed_at` (the Patch resurrect rule kept symmetric): the
  status flip is never silent. The MCP `memento_obsolete` guard now scans the
  key as well as the reason (CLI parity).
- Retire reasons fold to one line in notes (whitespace runs → single spaces):
  the audit trail stays one event per line.
- Render env-proxy hardening: the proxy Go's environment machinery produced
  is validated as a parsed struct — a malformed env proxy normalized so the
  credentials hide in the path is refused, and no `@` ever reaches obscura's
  argv (`/proc`-visible): the `@`/`%40` path check guards both the config and
  the env branch, and the SSRF pre-check is documented as guarding the
  requested URL only (obscura follows redirects itself — run it behind a
  proxy for per-hop policy).
- `add` never erases a stored `source` with an empty one (the notes rule):
  the qualifier is not kept in history on the touch path, so the wipe would
  be irreversible; a correction replaces source only when one is given.
- The guard's `redactions` audit lists only the spans the text actually lost
  (a match folded into an overlapping one is no longer reported), and the
  double redaction pass now covers every agent-visible field, not just the
  stored HTML.
- SSRF tunnels: the NAT64 well-known prefix (`64:ff9b::/96`, and its local
  `/48` variant `64:ff9b:1::/48`) is decoded to its embedded IPv4 like 6to4
  and Teredo already were; arbitrary RFC 6052 prefixes stay a documented
  residual.
- A correction journals the MERGED accepted state (the new value with the
  stored `source`/`notes` when the request left them empty): the journal
  entry and the doc never disagree about the qualifiers. The entry also
  repeats the doc's write-once `project` (a correction with the default
  `global` no longer journals a project the fact never had), and a
  resurrected fact journals the canonicalized category the doc received.
  `memento keys "k *"` normalizes to `k` once (the guard-checked prefix is
  what the wire sees), `memento_list`/`memento_search`/`memento_keys` and the
  CLI `search`/`keys` validate status (and `search --limit >= 0`) before any
  store contact (`prune` resolves `keep` from the local config before building
  the client too), and `memento status`'s env list is now pinned against the
  loader's source (the "kept in sync" comment is a test, not a promise).
  `memento_patch` answers its shape errors (nothing to change, a bogus
  `status`, a `reason` without `status=obsolete`) before any store contact
  too — one exported validator, one contract on both edges. Fact search
  (`memento_search`, `memento search`) refuses an empty query up-front like
  `memento_search_docs` does: a silent zero is not an answer.
- `memento keys` shape errors answer before any store contact: a prefix that
  normalizes to empty (" " or "*") with no `--match` is a caller error, not
  a wrapped "the store is unreachable" advisory. `memento fetch --project`
  and `memento docs --project` are trimmed like every other edge, `memento
  status` lists the render env overrides (`MEMENTO_FETCH_RENDER`,
  `MEMENTO_FETCH_OBSCURA_PATH`, `MEMENTO_FETCH_OBSCURA_STEALTH`), and
  `memento prune` on legacy keys reports the actually removed count instead
  of a pre-read guess.

- Default fetch User-Agent is a desktop Chrome string (sites 403 known bot
  UAs; override with `fetch_user_agent`/`MEMENTO_FETCH_USER_AGENT`).
- 429 is treated as an IP ban: with `fetch_proxy_fallback` the proxy is a new
  IP; without a proxy, one polite retry after a short pause. `Retry-After` is
  not honored (a hostile server could park the client).
- `text/markdown` / `text/x-markdown` responses extract as plain text (raw
  README endpoints are documentation); the request Accept advertises it.
- New config keys `fetch_render` (`off`|`obscura`, default `off`),
  `fetch_obscura_path` (default `obscura`), `fetch_obscura_stealth`
  (default `true`); env `MEMENTO_FETCH_RENDER`, `MEMENTO_FETCH_OBSCURA_PATH`,
  `MEMENTO_FETCH_OBSCURA_STEALTH`.

Named-hash context exemption:
- The three generic token shapes (`long-hex`, `long-base64`,
  `long-opaque-token`) are skipped when a word NAMES them within 40 bytes
  (`sha256:`, `md5`, `hash`, `digest`, `checksum`, `revision`, `rev`,
  `commit`, `hex`, `git`, `blob`, `tree`, `tag`, `sum`): a hash the author
  labels is a content fingerprint, not a credential. Vendor-prefixed shapes
  and the assignment rule never take the exemption (a labeled secret stays
  a secret; a real credential beside "commit" still meets the assignment
  rule). Applies uniformly to fact writes and fetched pages.
- Documents store `content_type` (the server media type), the input the
  fetch guard routes on.

JS rendering (opt-in, obscura):
- `fetch_render: "obscura"` adds a browser-render fallback for fetches refused
  the way bot walls refuse (network failures, 403/422/429 — NOT 404/401, not
  allowlist refusals, redirect limits, oversize): the URL goes to the
  [obscura](https://github.com/h4ckf0r0day/obscura) headless browser
  (Chrome-shaped stealth TLS/HTTP2 fingerprint), and the rendered HTML flows
  through the same extract + guard + store pipeline; the doc is stored
  `via: obscura`. Render failures surface BOTH reasons (plain + renderer).
- Third-party by design: ~75 MB binary, executes page JS locally, linux
  x86_64 glibc 2.35+. `make obscura` installs the sha256-pinned 0.2.6
  stealth build into `~/.local/bin`. The renderer is serialized (one render
  at a time, cancel-aware); `robots.txt` (when respected) gates both paths;
  the renderer keeps the operator's egress — `fetch_proxy_url` is validated
  at config load (every command fails on a bad value: the proxy is a route,
  not a fetch decoration), the environment proxy (`HTTP_PROXY`) is consulted
  for the render target too, and a proxy that cannot be forwarded
  (credentials, a scheme-less value) is a load error: the render never runs
  direct behind it (argv hygiene: a credential proxy is never forwarded to
  the subprocess); a typo in `fetch_render` is a loud startup error.
- SSRF: a **direct** render resolves the target through the same public-address
  rule as the plain fetch and refuses internal addresses (the render is the
  SSRF gate for its own route; `fetch_allow_private` opens it). With a proxy
  the proxy enforces the policy. Known limit: obscura follows redirects on
  its own, and a DNS-rebinding window between the pre-check and obscura's
  dial remains — run the renderer behind a proxy for policy-enforced egress.
- v1 limits: rendered docs store `final_url` = the requested URL (the dump
  mode does not report redirects); success = exit 0 + non-empty dump.
  README “JS rendering” and docs/setup.md §8.
- Docs: least-privilege store user guide for clusters with the security
  plugin (role/user/role-mapping recipes; the app role cannot delete the
  facts index; `indices:admin/get` + per-pattern create/settings/update
  actions included, with an upgrade note for roles provisioned before
  this release), linked from SECURITY.md.

Fixed:
- `memento_obsolete` (CLI and MCP) writes are scripted: the reason appends
  inside the painless script, so a concurrent atomic `notes_append` can no
  longer be dropped by the read-modify-write from the stale read (the
  contract `memento_patch` already had). A `reason` with `memento_patch` on
  an already-obsolete fact is refused (it would append a duplicate line with
  no journal entry).
- Journal infrastructure: a failed first install (403, network blip) is
  no longer remembered as "installed" — appends retry it until it lands,
  so a long-lived MCP server never journals without the template and the
  server-clock pipeline. Journal indices created while that was broken are
  healed: before appending, the app pins the ts pipeline on the index
  (idempotent settings PUT), so entries never sort as the oldest and
  become the first prune victims.
- `memento migrate-history` reruns are safe after a partial failure:
  entries are appended under content-derived ids, so the rerun collides
  with (and skips) what the previous run wrote instead of duplicating;
  a racing write's `current_ref` is never overwritten.
- `prune`: a transient fact-read failure aborts the prune instead of
  silently deleting the entry that carries the current value.
- Journal reads order by the store clock (`ingest_at`), not the writers'.
- Correction journal entries carry the fact's real `fact_id`.
- Negative `MEMENTO_SUMMARY_MIN_LEN`/`MEMENTO_SUMMARY_MAX_LEN` are
  rejected (they silently disabled the corresponding check); blank
  author labels are a loud config error, not a silent OS-user fallback.
- `list`/`search` lines show the summary (the table of contents
  contract); `get` shows a summary line; author labels are quoted in
  `get`/`journal`/`status` and stripped of non-printable characters.

Hardening (pre-release):
- SSRF: IPv4-compatible `::/96` addresses (`::127.0.0.1`, `::169.254.169.254`)
  are decoded by hand and blocked (`To4()` only understands `::ffff/96`); a
  policy refusal (allowlist / public-address rule) is FINAL — the proxy
  fallback never re-tries a refused URL through `fetch_proxy_url`;
  stored docs drop image references aimed at internal hosts (self-hosted
  docs keep them under `fetch_allow_private`); `robots.txt` rules honor
  RFC 9309 `*` and `$`; the obscura child environment drops every
  `MEMENTO_*`/`OPENSEARCH_*` variable and sandboxes HOME.
- Config refuses silent erosion: `MEMENTO_FETCH_HOSTS` set but naming zero
  hosts, `docs_ttl_days < 0`, and an `opensearch_url` without a host are
  load errors.
- Keyring (native Linux provider): upsert is create-then-delete — a
  cancelled prompt or dropped RPC can no longer destroy the stored
  credential; duplicate entries are picked deterministically and healed by
  the next write.
- Author labels are stripped of invisible Unicode (C1 controls, zero-width
  spaces, bidi overrides), not just C0/DEL.
- `memento get --history` fetches the newest journal entries for
  journal-era facts (the flag used to print nothing); `memento search
  --limit` clamps to the navigation cap with a note; every rendered
  timestamp says UTC (`Z`), unset times say `unknown`; a same-value touch
  says the passed summary was ignored.
- Exit codes: missing key is 3 (`get`/`obsolete`/`prune`), general errors
  stay 1 — scripts can tell "no such fact" from "store down".
- MCP write failures classify honestly: local contract rejections
  (summary bounds, re-retiring with a reason) and a missing key answer as
  themselves, no fake availability advice over a healthy store; search and
  navigation answers carry exact match counts (`track_total_hits`);
  journal reads count undecodable entries (`skipped`) instead of dropping
  them silently.
- Docs: clock synchronization is stated as the operational prerequisite of
  period-based journal pruning (entry routing follows the writer clock).
- `memento migrate-history` converges on stores past the navigation cap:
  the scan filter (needs-migration) moved server-side, so capped pages no
  longer walk past already-migrated facts and re-report the same head.
- Concurrent-write honesty: snapshot-derived writes (the same-value touch,
  the resurrect, the retirement, the status flip) are conditioned on the
  document version pair (`if_seq_no`/`if_primary_term`); a concurrent write
  that landed since the read fails as a named conflict — "re-read and
  retry" — instead of silently clobbering it. Value corrections stay
  last-write-wins (both writers journal; the audit stays truthful).
- Keyring upsert no longer sweeps the just-saved secret away when the kept
  item carries a bogus resource path (multiple matches now say so).
- A JSON body served with no declared Content-Type is refused, not parsed
  as a page (an API error blob would have been stored as a document).
- `memento_list` requires a project (cross-project navigation is
  `memento_keys`); stored text can no longer forge terminal layout —
  control and invisible format characters are stripped when CLI output
  prints fact values, summaries and notes.
- `robots.txt` groups with several consecutive `User-agent:` lines are
  kept whole, and when one agent appears in several groups their rules are
  COMBINED per RFC 9309 2.2.1 (first-wins and last-wins both silently
  dropped half of the file's stated policy).
- The birth link (`current_ref` for a freshly created fact) is CAS-conditioned
  on the version pair returned by the create itself — no re-read window where
  a concurrent writer could slip between the read and the link.
- Batch answers are honest about partial success: per-item flags carry the
  accepted write beside its lagged-journal error, and the aggregate counters
  (created/changed/touched) count exactly what landed — a summary can no
  longer contradict the rows it summarizes.
- Conflict classification answers from either error shape the client can produce
  (a middlebox-rewritten 409 included), is gated to real 409s, and refuses the
  index-creation-race signature wherever the engine leaves it — top level,
  root_cause, or a caused_by chain; success-by-collision is limited to
  content-derived deterministic journal ids, so a collision over a fresh uuid
  ref surfaces as a lagged entry, never as a clean success with a dangling
  `current_ref`.

- `memento docs` / `memento_search_docs` on a store without any fetched page
  returned an `index_not_found` error; the docs index is created lazily on the
  first fetch, so an absent index now reads as an empty result (same posture as
  journal wildcard reads).
- Replica counts now follow the topology as a steady-state property, not just
  at index creation: on startup the client re-checks the facts/docs indices,
  every existing history period index and the journal template, and corrects
  drift (`PUT _settings` / template re-PUT).
  A single-node install that later grows into a cluster self-heals instead of
  staying unreplicated. In auto mode nothing is written when the health probe
  fails — an unknown topology is never "fixed" toward 0; an explicit
  `index_replicas` value is now enforced on existing indices too. The
  journal-template reader parses the nested settings shape OpenSearch actually
  returns (the flat-key form never matched, so the template re-PUT never fired),
  reconcile now claims one pass per store per process (a long-lived MCP server
  builds a fresh client per call), a health probe reporting zero data nodes is
  treated as unknown, and the shared-store DLS recipe is honored end to end: a
  403 reading the pipeline/template or pinning the journal pipeline is taken as
  "externally managed infra" instead of failing the write.
- Verified version matrix: the live CLI smoke (`hack/cli-smoke.sh`, 23 checks)
  passes on OpenSearch 2.19.7, 3.0.0, 3.3.2, 3.8.0 and 3.9.0
  (`hack/version-matrix.sh`); `OPENSEARCH_IMAGE`/`OSD_IMAGE` can pin the dev
  stack images. What is covered is now written down in `docs/testing.md`. The
  matrix runs under its own compose project (`-p memento-matrix`) and asserts
  the reported version matches the tag, so it can neither wipe a developer's
  `memento-os` data volume nor score a PASS against the wrong image.
- `scripts/install.sh`: release installer (latest or `--version`, os/arch
  detection, sha256 verification against the release `checksums.txt`, installs
  into `~/.local/bin`).
- Community files: `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, issue templates.
