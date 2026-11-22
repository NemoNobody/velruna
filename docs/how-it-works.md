# How velruna works

## Storage model

Facts are stored in an OpenSearch index (`velruna` by default). Each fact is a
document keyed by its `key` (the key is the document ID).

### Index schema

| Field | Type | Purpose |
|-------|------|---------|
| `fact_id` | keyword | stable doc id (UUID) |
| `key` | keyword | stable key, e.g. `proj:foo:loc:bar.py` |
| `value` | text | fact text (full-text searchable) |
| `category` | keyword | `location \| config \| endpoint \| behavior \| context_map \| recipe \| rule` |
| `project` | keyword | `global` or project name |
| `status` | keyword | `verified \| obsolete` |
| `created_at` | date | creation timestamp |
| `updated_at` | date | last write timestamp (moves on any change, incl. notes) |
| `observed_at` | date | when the current value was seen (moves only on value change) |
| `ttl_days` | integer | optional per-fact staleness override, capped by the category TTL |
| `keywords` | keyword[] | normalized tags: lowercase single tokens, deduped, max 10 |
| `source` | keyword | `tool:read_file \| experiment \| user:confirmed \| doc:*` |
| `notes` | text | free-text qualifiers (omitted when empty; obsolete reasons appended here) |
| `summary` | text | short table-of-contents rendering of the value; required when the value is over `summary_min_len` (see below) |
| `author` | keyword | who recorded the fact (self-reported label, write-once) |
| `author_source` | keyword | which chain link named the author: `security` / `config` / `os` / `none` |
| `model` | keyword | model label of the writer at creation (write-once, self-reported) |
| `host` | keyword | host label of the writer at creation (write-once, self-reported) |
| `session` | keyword | agent session label at creation (write-once, self-reported) |
| `updated_by` | keyword | self-reported label of the last writer (moves on every write) |
| `current_ref` | keyword | id of the journal entry that carries the current value |
| `history` | object (`enabled: false`) | **legacy** pre-journal array `[{value, observed_at, source}]` — stored, never indexed; `velruna migrate-history` moves it into the journal and removes the field |

The catalog is applied twice: on index creation, and additively on an existing
index (`PUT _mapping`, once per process). Without the second pass a field added
after the index existed would be typed by the first write's dynamic mapping —
a `keywords` array would become `text` with a `keyword` subfield, and an exact
tag filter would silently stop matching tags the analyzer splits.

### Key conventions

- `key` is the document ID. Re-adding the same key is an idempotent upsert:
  a new value is appended to the journal and the fact points at the entry
  (`current_ref`); the same value updates notes only.
- `key` should be stable, unique per fact, and written in **predictable English**
  (`project:area:location:detail`) — a later session must be able to guess it
  without searching.
- Keys are **trimmed at every edge** (add, patch, get, journal, obsolete,
  prune, keys; the batch dupe check runs on the trimmed key): `" k "` and
  `"k"` must never become two docs with diverging history. Documents written
  with padded keys before this rule are not auto-merged — `velruna keys
  --match` finds them; merging is an admin call.
- `project` labels are trimmed at every edge (write and read) and, like the
  birth authorship, **write-once**: re-adding a fact never migrates its
  project label (an empty project at add time means `global`).
- `value` is the human-readable fact.

## Documents (the docs index)

Fetched pages live in a separate index (`docs_index_name`, `velruna-docs` by
default): the raw HTML is kept write-only for re-extraction, and BM25 runs over
the extracted text, never over markup.

Extraction splits text into **blocks**; it never cuts a block in the middle.
The natural unit is a heading-delimited section. A section longer than
`fetch_block_chars` (default 8000 runes) is split at a line boundary, then at a
space or punctuation, and only then hard at the count — joining the parts gives
the text back unchanged, so a split loses nothing. Split parts are labelled
`heading (2/5)`, and every block is addressable by `order`, the unit
`velruna_get_doc` reads. `block_chars` is an extraction setting: it applies
whenever text is extracted, so a truncated snapshot re-extracted for a larger
`max_chars` is re-blocked at the requested size when that budget actually
stores more text, while a cached snapshot whose text is complete keeps the
split it was stored with (a revalidating `304` included).

`fetch_doc_chars` (default 100000; `0` = the whole document) is the storage
budget for one document. When the budget runs out, whole blocks are dropped
from the end, `truncated: true` is set, and the answer carries `chars_stored` /
`chars_total`. No block is cut to fit. `truncated` therefore marks a real loss
only: the storage budget or dropped image references. A page over the
`fetch_max_bytes` limit (2 MB) is refused with an error, not trimmed, so it
never stores a partial document. Blocks skipped by the chrome filter are not a
loss.

`content_hash` is sha256 over the full extracted text **before** the budget is
applied: it does not depend on `max_chars` or `block_chars`, so the same page
keeps its identity regardless of how much of it was stored. `full_chars` is the
length of that full text.

When a stored copy was cut by the budget and a larger `max_chars` is requested,
the text is re-extracted from the stored raw HTML with no network. The guard
runs again over the re-extracted text — a credential split by markup is
invisible to the HTML pass and is reconstructed by extraction — and its audit
rows are merged with the earlier ones; the stored raw HTML and the cache
metadata (`etag`, `project`, `expires_at`) are not rewritten. A smaller request
never shrinks the stored copy for unchanged content. Response caps — 32000
runes per `velruna_get_doc` answer (one block is returned whole even when it
alone exceeds the cap), a partial outline (the first 60 lines plus `more N
blocks, orders X..Y`) on a huge document — are read limits, not storage limits.

## Commands

### add

```
velruna add <key> <value> <category> [--project <p>] [--source <s>] [--notes <n>] [--summary <s>]
```

Adds a fact. If the key exists: a different value corrects it (the accepted
value is appended to the journal, then the main doc is updated to point at the
entry); the same value updates `notes`/`source` only (no journal entry,
`observed_at` stays). A correction's journal entry carries the merged accepted
state — the new value with the stored `source`/`notes` when the request left
them empty — so the log and the doc never disagree about the qualifiers. The
entry also repeats the doc's write-once `project` (a correction cannot move
it) and the canonicalized `category` the write edge applied. The exception: a same-value `add` on an **obsolete**
fact is a fresh verification — it resurrects the fact to `verified`, moves
`observed_at` to now, and the transition is journaled (the Patch resurrect
rule kept symmetric, so "when was it last seen alive" is always answerable
from the log). `--notes` carries qualifiers ("checked the dev config
only") to later sessions. An empty `--source` or `--notes` means "not given",
never "erase": neither overwrites a stored value (the Patch contract) — on
the touch path the qualifiers are not kept in history, so such a wipe would be
irreversible. A value over `summary_min_len` requires `--summary`
(the store's table of contents, see below).

### get

```
velruna get <key> [--history]
```

Returns the fact for a key, plus the freshness signal (`age_days`, `stale`).
The history chain is **off by default** (it bloats the context of agents that
read facts on every turn); `--history` prints the legacy in-doc array of
pre-journal facts (a half-migrated fact keeps showing the array until
`migrate-history` retires it), and for facts without a legacy array the
newest journal entries (up to 20). The full journal is one
`velruna journal <key> --limit <N>` away (counts at a glance:
`velruna history-info`).

### search

```
velruna search <query> [--project <p>] [--category <c>] [--status <s>]
```

Full-text search over `value`, `notes` and `summary`, filtered by project/category/status.
Default status is `verified`. Stale facts are marked `[stale: Nd]` in the
listing.

### list

```
velruna list <project> [--prefix <p>]
```

All facts for a project, optionally narrowed to a key prefix (navigation cap
1000; the CLI says when the answer was truncated). Stale facts are marked
`[stale: Nd]`.

### journal / history-info

```
velruna journal <key> [--limit N] [--since RFC3339] [--until RFC3339]
velruna history-info <key>
```

`journal` reads the journal for a key: every accepted value, newest first
(`--limit` caps the page; `--since`/`--until` bound `created_at`, so "what
changed since my last session" is a query, not a scan — dates are RFC3339 in
UTC on purpose: `2026-11-10` would silently mean local midnight).
`history-info` is the cheap overview: writes,
rediscoveries, first/last timestamps, `current_ref` — no entry bodies. Use it
to answer "did this fact change, how often, when last" before pulling the
full journal.

### prune

```
velruna prune <key> [--keep N] [--yes]
```

Drops journal entries beyond the newest `N` (journal-first; keys that predate
the journal fall back to the legacy in-doc array), keeping the current value
untouched. `N` defaults to `history_limit` from the config. The journal is
write-forever by design, so pruning is a manual admin tool, never automatic.

Pruning is destructive (dropped observations have no undo), so it sits behind a
barrier: **without `--yes` nothing is written** — the command prints the plan
(how many entries exist, how many survive, how many are lost) and exits. An
agent must show that plan to the user and re-run with `--yes` only after their
explicit approval.

### obsolete

```
velruna obsolete <key> [reason]
```

Marks the fact `obsolete` because its subject is gone (file deleted, endpoint
removed). The value and `observed_at` are kept; the reason is appended to
`notes`. Use `add` instead when the value merely changed — corrections journal
the new value.

The write is scripted (painless): the reason appends **inside** the script, so
a concurrent `notes_append` cannot be dropped by a read-modify-write from the
stale read, and status/`current_ref` land in the same atomic update. The
journal entry carries the reason in notes (audit symmetry with
`velruna_patch`). A reason with `velruna_patch` on an already-obsolete fact is
refused — the reason belongs to the transition. That refusal is computed from
the read taken before the scripted update: a retire racing in between the read
and the script slips past the check (the journal entry documents that event, so
the audit trail never lies about the state — at worst a notes line duplicates).
`velruna_obsolete` refuses the same way, from the same kind of pre-read: two
racing retires can duplicate the notes line and journal one entry each — the
journal records every accepted write, so the events stay truthful.

## The journal (history)

History is an append-only **journal** of dated indices, not an array inside
the fact doc:

- One entry per accepted value: `fact_key`, `op` (`write`; `rediscovery` — a
  re-confirmation of the same value; `retire` marks the verified→obsolete
  transition, so "when and by whom was this retired" is a query; `supersede`
  records a successor link — a relation, not a value), `value`,
  `notes`, `source`, `keywords`, `created_at`, author fields. `op_type=create`: appends
  never overwrite each other, there is no CAS.
- Entries live in `<index>-history-<period-date>`: the period granularity is
  `history_index_period` (config; env `VELRUNA_HISTORY_PERIOD`; default
  `month` — `2026.11`; also `day`, `week` (`2026-w46`), `quarter`
  (`2026-q4`), `year`). Dated indices keep old periods small, inspectable,
  and deletable (the admin pruning path).
  Which period an entry lands in is decided by its client-side `created_at`
  (the writer's clock), while `ingest_at` is the server clock: on a shared
  store, a writer whose clock is skewed past a period boundary files
  entries into an older period index — and an admin cleaning up "old"
  periods by date can delete them as if they were stale. Keep NTP (or
  systemd-timesyncd) on every writer machine; this is the operational
  prerequisite of period-based journal pruning, chosen over per-write
  round-trips to the server clock.
- The journal infrastructure (ingest pipeline `<index>-journal-ts` stamping
  entries with the **server clock** — writer clocks skew on a shared store;
  index template `<index>-journal` with explicit mappings) is created
  automatically on the first accepted write.
- Write order is **doc-first**: the journal entry id is preallocated into
  `current_ref`, the fact doc is updated, then the entry is indexed. A lost
  entry append leaves `current_ref` dangling (repairable: `migrate-history`
  re-appends the current state under the same deterministic id); the reverse
  order could journal a value that never reached the doc.
  Value corrections are last-writer-wins, not CAS: two concurrent
  corrections both land in the journal and the doc keeps the later one, so
  the journal is the log of *accepted writes*, not a sequence guaranteed to
  match "what was current at each instant". An entry that was immediately
  overwritten is history, not a lie.
- The journal is never pruned automatically (write-forever): history shows
  how old it is. `history_limit` is only the default `--keep` for the manual
  `velruna prune`.
- After an admin deleted old journal indices, read counts are lower bounds —
  the app cannot see deletions it did not perform.

### migrate-history

```
velruna migrate-history [--project <p>] [--yes]
```

One-time upgrade step from pre-journal releases: moves each fact's in-doc
`history` array into the journal (entries land in the index of their
`observed_at`), appends the current value, sets `current_ref`, and removes
the array. It prints the plan first; `--yes` writes. Unmigrated facts keep
working; their history just stays in-doc until the next correction.
Reruns after a partial failure are safe: entries are appended under ids
derived from their content, so the rerun collides with what the previous
run wrote (collision = success) and never duplicates.

## History retention

Corrections are frequent (state keys get re-observed daily), so history is
kept out of the fact doc entirely: the journal carries it and fact reads stay
cheap. The journal is write-forever; `history_limit` (config key
`history_limit`, env `VELRUNA_HISTORY_LIMIT`, default 20) is the default
`--keep` for `velruna prune`, which applies it on demand, behind a
confirmation barrier. Dated journal indices are the retention lever: an admin
can close, snapshot, or delete whole periods with standard OpenSearch tools.

## Staleness

The store provides the evidence (`observed_at`); the signal is computed at read
time, so it never rots in the index. Every read (`get`, `search`, `list`, and
the MCP tools) carries `age_days` and `stale`:

| Category | TTL (days) | Rationale |
|----------|-----------|-----------|
| `config`, `endpoint` | 14 | values move with deploys and config edits |
| `location`, `behavior` | 30 | files and code behavior move slower |
| `rule` | 90 | cross-project constraints outlive a sprint, but not a year of drift |
| `recipe` | 180 | reusable procedures survive small refactors; tooling and hardware do not |
| `context_map` | never | decisions and design notes do not rot by age |

The category TTL is also a **cap**: a writer may set a shorter per-fact window
(`ttl_days` on `velruna_add`/`velruna_patch`, `--ttl-days` on the CLI) when it
knows the fact rots faster. Lengthening past the category cap is refused at the
write edge — a window that outlives the category's promise would quietly
mislead every later reader. Categories with no default TTL are uncapped
precisely because they have no promise to break.

Rules:

- `stale` is `age_days > ttl`, where `ttl` is the per-fact override when one is
  set and the category default otherwise; at the exact TTL a fact is still
  fresh.
- A zero `observed_at` (facts predating the field) means `age_days: -1`; for
  TTL'd categories that is **stale** — the age cannot be trusted, so re-check
  the source. Categories without a TTL stay `stale: false`.
- `stale: true` is an instruction to re-verify, not a verdict to discard. The
  store provides the signal; the agent judges.
- Nothing in `add`/`obsolete` moves `observed_at` except a real value change, so
  a stale fact cannot be silently "re-freshed" by a notes touch.

## Keywords

A fact may carry up to 10 tags (`keywords` on `velruna_add`, `--keyword` on the
CLI). Tags are normalized at the write edge — trimmed, lowercased, one token
each, deduplicated in first-seen order — because the filter is a plain term on
a `keyword` array: exact match, no analysis. That makes tags the cheapest
cross-project slice (`velruna_search` with `keyword: "perf"` over all projects)
and the association layer an agent needs without a graph database. An empty
list on a re-add leaves the stored tags alone (the notes/source convention);
keywords are a stored qualifier, not a journaled observation. Every accepted
value's journal entry carries the tag set as of that moment, so re-tagging is
safe: the previous set stays recoverable from the entry that carried it.

## Re-verification (`confirmed_by`)

`stale` is an instruction to re-check, and a re-check has to leave a trace.
Without one, a fact could never become fresh again (a same-value re-add is a
touch: it deliberately does not move `observed_at`).

`velruna_patch {reverify: true}` re-confirms the current value in place:
`observed_at` moves to now, the journal gains an `op=rediscovery` entry (who,
when, model, session), and the writer joins the fact's `confirmed_by`. The
value, notes, tags and summary are untouched; a `reverify` cannot ride along
with a value or status change — those are different events.

`confirmed_by` lists who has verified the **current** value, creator first, as
`{author, author_source, host, at}`: the user label and the chain that produced
it (`security` | `config` | `os`), the machine (user + host identifies a
confirmer in a multi-user, multi-machine store), and when. A value change or a
resurrection resets the set: those confirmations were about a different value
(or a fact that was declared dead) and must not count. Repeats are
deduplicated per user+host (latest time wins); every event stays in the journal.
Who may confirm is a permissions question, and permissions live in OpenSearch
roles — the store records, it does not gate.

## Succession (`superseded_by`)

Some records are replaced rather than corrected: an old key dies, and the
truth moves to a new one. Copying the value would create two claims to keep in
sync, so the store keeps a **link** instead — `superseded_by` names the fact a
reader should trust instead. `velruna_patch {superseded_by: "other:key"}` sets
it and journals an `op=supersede` entry; `velruna_get` returns the field, so a
reader who lands on the dead key is sent to the live one.

The field is a chain, not a graph, and the invariants make it walkable:

- one outgoing link per fact — a later patch may repoint it (a successor can
  itself be retired or merged again, so the chain must stay repairable), and
  every repoint is its own `op=supersede` entry;
- no self-reference and no cycles — the client walks the target's own chain
  (bounded) before accepting a link;
- every fact on the chain must be `verified` **at the time the link is
  written**: a reader sent through an obsolete hop would land on a record the
  store itself says is dead. A target retired later is visible in one `get`
  (the link is not rewritten by the retirement) — repoint the link if the
  successor died;
- a link pointing back in time (a fact created later declaring an earlier
  successor — a key merged into a record that already existed) is legal, but
  its journal entry carries `marker=superseded-newer-with-older`, so a
  reversed claim is a query, not a surprise.

The link is not part of the value: a value correction keeps it and touches
neither `observed_at` nor `confirmed_by` — succession states a relation
between records, not a re-observation of one.

## Advisory contract and precedence

The store is **context, not commands**. A fact — including a `rule` or a
`recipe` — is something a previous session verified, not an instruction this
one must obey. Precedence, highest first:

1. system prompt and the current session's user instructions;
2. personal/global agent rules (`~/.agents`, user config);
3. the project's own `AGENTS.md` / config;
4. **the shared store, last**.

The store sits last because its metadata is unsigned: `author` and
`author_source` are self-reported labels written by the caller, and a plaintext
store cannot verify them. Identity is not authority. So an agent may silently
ignore a stored rule that contradicts a higher layer, and it must never treat
a stored rule as a hard requirement — an infeasible rule is reported (at most
one dated `notes_append` flag per key) and the task proceeds. Retiring a
negative rule is a human/author decision: nothing auto-obsoletes a fact.

This is also why **skills and other artifacts are not stored as content**. The
store cannot sign a bundle, cannot verify authorship, and cannot be a release
or activation channel — a registry built on it would be a supply-chain hole
dressed as convenience. A fact may *reference* a skill (repository URL, version,
content hash) the way it references any other location; the code itself lives
in git and on disk, where signatures and review actually exist.

A long fact is useless in a listing: pulling every full value to find one is
exactly the context cost the store exists to avoid. So an accepted value over
`summary_min_len` runes (default 2000) must ship a `summary` — a short
table-of-contents rendering — and the summary itself is capped at
`summary_max_len` runes (default 400; ~100 tokens, about a 10x compression).
Both knobs are config keys (env `VELRUNA_SUMMARY_MIN_LEN` /
`VELRUNA_SUMMARY_MAX_LEN`); `0` disables the respective check.

The rule applies to the accepted value: create and correction. A notes-only
touch of a long fact that predates the rule is not rejected (the value is
unchanged), and a rejected correction never reaches the journal. The summary
is searchable text (`text`, BM25): `velruna_search` can match it, which turns
the store into a skimmable index — find by summary, read the full value by
key.

## Authorship

Every write is stamped with self-reported labels: `author` (plus
`author_source` naming the chain link), `model`, `host`, `session`. The birth
labels are write-once; later writes move only `updated_by`, and every journal
entry carries the labels of the writer who appended it.

The author resolves once per process, down this chain:

1. `security` — the store's security plugin: `GET /_security/authinfo` (the
   name the *server* authenticated, so it cannot be forged by the client).
   Any non-success (security off, 403, timeout) silently degrades to the next
   link; a stalled plugin never slows writes (5s probe deadline).
2. `config` — the `author` config key / `VELRUNA_AUTHOR`.
3. `os` — the OS username of the process.
4. `none` — nothing resolved; writes carry no author labels.

`model`/`host`/`session` come from config keys / env (`VELRUNA_MODEL`,
`VELRUNA_HOST`, `VELRUNA_SESSION`); `host` falls back to the OS hostname
(sanitized), `model` and `session` are never guessed.

**Threat model:** these labels are audit metadata, not authentication. A
client can set any `author` it likes (config), and the `security` label is
only as strong as the cluster's per-user credentials. The purpose is
accountability in a cooperative shared store: who recorded what, when, which
model produced it — for review, drift analysis, and noticing that one source
is consistently wrong. On a hostile shared store, author labels inform a
human; they do not stop the hostile client (that is the store's security
plugin's job: per-user accounts and index permissions).

## Secret guard

The index is plaintext: a secret written once is exposed in every later read
(and in every agent context that reads it). The write paths (`add`,
`obsolete`, and the MCP tools) scan the user-supplied text — key, value,
summary, notes, source (for `obsolete`: key and reason) — for credential-like
shapes: vendor token
prefixes (`AKIA…`, `ghp_…`, `xox…`, `sk-…`, JWT, PEM headers), `Bearer`
headers, `password = …` assignments, and long opaque strings (hex/base64).

The level is `secret_guard` in the config (env `VELRUNA_SECRET_GUARD`). Every
finding carries a confidence level — `high` for deterministic credential
shapes (vendor prefixes, PEM key blocks, `Bearer` headers, basic-auth URLs,
assignments whose right-hand side is 20+ characters spanning 3+ character
classes) and `medium` for the generic shapes that also fire on ordinary
content (long hex/base64 like git SHAs and asset hashes, weak assignments
such as `password = someVarName`):

| Level | Behavior |
|-------|----------|
| `reject` (default) | a high-confidence credential refuses the write; a medium-confidence heuristic hit in a TEXT field (`value`, `notes`, `summary`, `notes_append`, `reason`) is replaced in place — the write proceeds and the answer lists `redactions`; a hit in an IDENTITY field (`key`, `project`, `source`, `keywords`) refuses, because a rewritten key would silently become a different fact; there is no confirmation flag |
| `ask` | the same, except an explicit confirmation stores the text as written: `--allow-secret` (CLI) / `confirm_secret: true` (MCP) — an agent must show the masked finding to the user first |
| `allow` | scanning is off; a deliberate opt-out for shared stores |

Why the split: the high-confidence shapes are deterministic vendor prefixes and
key blocks — near-certainly a real credential, and storing a marker in its
place would let a writer believe the token was saved. The medium shapes are
heuristics that fire on ordinary, legitimate content (a commit hash, a
checksum, a config name); refusing those made a perfectly good fact
unstorable at `reject` with no escape, so the flagged span is replaced and the
loss is stated. The same asymmetry does NOT apply to fetched pages: there the
medium leniency is route-based (documentation vs API payload), because page
content is not authored by the writer.

A redaction is never silent: the fact carries `redactions` (field, pattern,
size — never the text, so the removed span cannot leak back through a read),
the journal entry of the accepted value carries the same rows, and the write
answer repeats them. The rows reach every read shape (`velruna_get`,
`velruna_search`, `velruna_list`, `velruna_journal`, CLI `get`), so a value
that quietly shrank never reads as a complete claim. The rows follow the text
they describe: a field the write overwrites drops its stored row even when the
new text is clean, appending a line accumulates (the earlier line is still in
`notes`) while replacing `notes` clears the rows of the lines folded into it,
and only a value change replaces the whole set (a clean value clears it).

Guarantees and limits:

- The guard runs **before any store contact**: a rejected write is rejected
  even when OpenSearch is down.
- The guard's message carries masked previews only (first 4 characters and
  the length) — the matched text is never echoed back into the caller's
  context. Echoing it would leak the secret a second time.
- Detection is regex-based best effort: a secret without a recognizable shape
  passes. A false positive is possible too — an unnamed 40-character hex
  string in notes matches `long-hex` (medium); on fact writes it is replaced
  rather than fatal, and at `ask` the confirmation flag stores the text as
  written. A text field that is nothing but a flagged span is refused: a fact
  whose value is a bare marker would read as a complete claim, so store it
  under `allow` if it is genuinely a hash. A generic token shape that sits
  next to a word that
  NAMES it (`sha256:`, `revision`, `commit`, `checksum`, SRI `=sha384,…`) is
  a content fingerprint, not a credential: the three generic shapes
  (long-hex/long-base64/long-opaque) are skipped inside such a hash context
  (±40 bytes of a hint word). Vendor-prefixed shapes and assignments never
  take the exemption: a real credential beside the word "commit" is still
  caught by the assignment rule.
- The fetched-page path uses the confidence split as policy and routes on the
  server's content type: HTML/markdown/plain is DOCUMENTATION, `application/
  json|xml` and `+json`/`+xml` types are API payloads. High-confidence
  shapes in the agent-visible fields (title, section text/headings, image
  alt/title) and in the stored raw HTML are REDACTED in place (`[velruna-
  redacted:pattern]`) and the page is stored — the answer lists `redactions`
  (field/pattern/size, never a preview). Medium findings in the agent-visible
  text: on the DOCUMENTATION route they do not refuse the write (documentation
  is full of asset hashes, quoted SHAs and config names — refusing made pages
  unusable); they are counted and surfaced (`guard: N` in the CLI outline,
  `guard_medium_ignored: N` in the MCP answer) so the leniency is visible, not
  silent. On the API route medium findings still REFUSE the write (an API body
  quoting a token is a real leak class): at `ask` a finding can be confirmed,
  at `reject` there is no bypass. Redaction runs before the stored `size` and
  `content_hash` are computed, so the metadata describes exactly the text
  that ships. Every redacted field (agent-visible text and the stored raw
  HTML) is redacted in a high-confidence pass applied twice: the second pass
  closes the common overlap-tail case (a span starting inside an earlier
  replacement); a greedy span can still swallow a scheme prefix, so a
  credential glued directly to a long token can survive into the stored
  HTML — the raw HTML is never served to agents, so the residual exposure is
  disk-only. Its redactions join the audit trail as field `html`: the
  surgery on the store is reported, and the trail lists only the spans the
  text actually lost (a match folded into an overlapping one is not claimed). The document URL is scanned high-confidence only:
  it is identity (redaction is impossible) and medium shapes are ordinary in
  it (git SHAs in GitHub paths). The post-redirect `final_url` carries the
  same identity role: scanned high-confidence only, and a credential-bearing
  redirect target (admin:secret@host) refuses the doc — a URL with credentials
  is never a document. Image hrefs are documentation: the high-confidence
  shapes are redacted in place (a credential-bearing src is a leak, not a
  link). The agent/user-supplied `project` tag is scanned at the call sites,
  before the store (the cache-hit re-tag path never sees the content guard),
  on fact writes (`velruna_add`/`velruna_add_batch`/CLI add) exactly as on the
  fetch path. The project is graded like the visible text (not HighOnly) and
  stays strict on every content route: labels are short identity text where
  a credential has no business appearing;
  a SHA-named project refuses its own fetch — keep labels human-readable.
  The `redactions` field is dynamically mapped, not filtered — the docs
  index is small and the audit trail is worth the mapping.
- Deliberate tradeoff: a high-confidence secret on a fetched page ends up
  stored in redacted form — the redaction must therefore out-cover a reject.
  For the shared plaintext docs store this is the accepted product decision:
  the secret never ships to agents, the documentation survives.
- The guard gates every route equally: a page rendered through the obscura
  fallback (README, "JS rendering") flows through the same hook — the
  renderer changes how the HTML arrives, not what is allowed into the store.

## Auth

The CLI uses **Basic auth** (user + password). Connection and auth settings
come from a config file:

- `--conf <path>` flag (`velruna mcp` and `velruna status`)
- `VELRUNA_CONF` env var
- `velruna.conf` (current dir)
- `velruna.conf` next to the binary
- `~/.velruna.conf`

Env vars override the file: `OPENSEARCH_URL`, `OPENSEARCH_USER`,
`OPENSEARCH_PASSWORD`, `OPENSEARCH_INSECURE_SKIP_VERIFY`, `VELRUNA_CONF`,
`VELRUNA_INDEX`, `VELRUNA_HISTORY_PERIOD`, `VELRUNA_HISTORY_LIMIT`,
`VELRUNA_SECRET_GUARD`,
`VELRUNA_ALLOW_PLAINTEXT`, `VELRUNA_SUMMARY_MIN_LEN`,
`VELRUNA_SUMMARY_MAX_LEN`, `VELRUNA_AUTHOR`, `VELRUNA_MODEL`,
`VELRUNA_HOST`, `VELRUNA_SESSION`, plus the `VELRUNA_FETCH_*` /
`VELRUNA_DOCS_*` / `VELRUNA_LOCAL_REPO_ROOTS` / `VELRUNA_REPO_SCAN_DEPTH`
fetch options (the full list lives in README and docs/setup.md).

OpenSearch also offers API keys, mTLS, and enterprise SSO (Kerberos / LDAP /
JWT), but `velruna` speaks **Basic auth only** — user and password in the
config.

## Notes

- `insecure_skip_verify` defaults to `false`: TLS certificates are verified.
  The local OpenSearch uses a self-signed cert, so a local setup sets it to
  `true` explicitly. Never enable it for a shared network.
- The index is created automatically on first run. The journal infrastructure
  (pipeline, template, dated journal indices) is created on the first
  accepted write.
