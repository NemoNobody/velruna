# Worksets

A workset is a **named, ordered list of references to verified facts**, shared
between agents working on one project. It answers "what is the current line of
work on this project, in what order, who holds which item" — and nothing else.
It is deliberately **not a tracker**: no priorities, no deadlines, no subtasks,
no comment threads. The work units are facts the store already carries (with
their own status, age and history); a workset only orders them, marks where
each one stands, and says who moved it last.

Because it stores references, not copies, it stays cheap: a fact correction
never has to touch the worksets that list it.

## The model

One document per workset, in the worksets index:

| Field | Purpose |
|---|---|
| `id` | stable UUID |
| `name` | unique slug — the handle the CLI and MCP tools use; names are **globally unique** (the project is a label, not a namespace) |
| `aliases` | former names, collected by `rename` — a session holding the old handle still finds the workset |
| `project` | label, default `global` |
| `entries` | the ordered list (below); order is the reading order |
| `cursors` | per-reader reading positions (below); at most 20, no time-based expiry |
| `created_at` / `updated_at` | timestamps |
| `created_by` / `updated_by` | authorship labels — the same self-reported audit metadata facts carry |
| `version` | human-readable write counter; the compare-and-swap itself is conditioned on `_seq_no`/`_primary_term` |

## Entries

An entry is a reference to one fact plus its work state:

| Field | Purpose |
|---|---|
| `fact_id` | the fact's stable id (UUID) |
| `note` | optional free text for the entry — why it is here, how to approach it |
| `phase` | `todo \| in_progress \| blocked \| testing \| done \| cancelled` |
| `phase_by` | who set the current phase (the owner while a lock is held) |
| `phase_at` | when the phase was set |
| `added_at` / `added_by` | when and by whom the entry was added |

### fact_state

At read time every entry resolves its fact against the store and carries
`fact_state`: `verified | obsolete | missing | unknown`. It is **computed,
never stored** — a fact's own status moves independently of the workset, and a
copy in the entry would only be a stale lie.

`missing` means the fact document itself is gone — documents are deleted by
an admin, nothing else removes them. When an entry stops resolving to a live
fact, the usual case is simply that the fact went `obsolete`; treat
`missing` as "someone deleted the record", not as a broken reference.
`unknown` means the state read did not answer for that id: a partial store
answer, or an entry beyond the per-call state-read cap (200 ids). It is never
treated as `verified`; `--live-only` drops it.
`velruna workset show --live-only` hides entries whose fact is not
`verified` — what is usually wanted when a new session re-reads a workset.

## Phases

| Phase | Meaning |
|---|---|
| `todo` | not started |
| `in_progress` | actively worked; `phase_by` owns it |
| `blocked` | cannot continue until an external condition clears |
| `testing` | declared done and under verification |
| `done` | verified and accepted |
| `cancelled` | closed without a result — not needed, duplicate, withdrawn |

### Transitions

| from \ to | todo | in_progress | blocked | testing | done | cancelled |
|---|---|---|---|---|---|---|
| todo | no-op | yes | yes | yes | no | yes |
| in_progress | yes | takeover/no-op | yes | yes | no | yes |
| blocked | yes | yes | no-op | yes | no | yes |
| testing | yes | yes | yes | no-op | yes | yes |
| done | yes | yes | no | yes | no-op | no |
| cancelled | yes | yes | no | no | no | no-op |

The rules the matrix encodes:

- **`done` is reachable only from `testing`.** Nobody jumps to done: the
  `testing` phase is the verification step, and skipping it is the whole
  failure mode this state machine exists to prevent.
- **A `cancelled` item reopens only through `todo`/`in_progress`** — back
  through the queue, not straight into work.
- **`done -> cancelled` and `cancelled -> done` are forbidden.** A finished
  item that turns out to be wrong reopens to `testing` (or `todo`); the
  path keeps the audit honest about what happened.
- **Self-transition is a no-op.** Re-applying a phase is idempotent, so
  retrying a rejected call is safe.
- `in_progress -> in_progress` by the same author is a no-op; by another
  author it is a **takeover** (see Ownership).

### Ownership

While an entry is `in_progress` or `blocked`, its phase is locked to
`phase_by`: only the owner may change it. Another author may always move it
to `todo` (handing it back to the queue) or take it explicitly with
`--takeover`.

The lock is not a reservation: it **expires after `workset_takeover_seconds`**
(default 3600) without a phase change. A takeover after expiry is journaled
with `takeover: stale` — the holder went silent, and that is the expected, not
an error. A move that needed the explicit flag is journaled with
`takeover: explicit` (see Audit).

`testing`, `done` and `cancelled` do not lock: they are claims and handovers,
not an exclusive hold on the work.

## Cursors and presence

Each author has a **reading position** in the ordered list, keyed by
`author@host` (the author label is self-reported; the host separates two
machines under one label). An empty author names no reader and is refused.
At most 20 cursors per workset, least-recently-used evicted; there is **no
time-based expiry** in v1 — a cursor lives until eviction or `--clear`.

Cursor moves are **never journaled** — that would drown the audit in traffic;
phase changes and composition changes (add/remove/rename/create/delete) are.

`show` draws two separate lines:

- `in_progress: ...` — the **claims**: which entry each author currently owns,
  rendered from the entry's `phase_by` as `author@host@index`;
- `cursors: ...` — the per-reader **positions** recorded by
  `workset cursor --at N`, rendered `author@host@N`, newest first.

```
in_progress: codex@server@4, me@laptop@7
cursors: codex@server@4, me@laptop@1
```

Cursors answer "what has already been looked at"; the in_progress line answers
"who is on what right now" and whether two agents are about to start the same
entry. Both describe the whole workset even when the table is filtered by
`--phase`/`--live-only`.

## Audit

An append-only journal (one entry per event, never rewritten) records:

```
create | add | remove | rename | delete | phase | takeover | note
```

each with the author, the time and an optional note. Rows are ordered by the
store-stamped ingest time (`ingest_at`), not the writer clock; the entry's own
`at` is the fallback for rows written before the pipeline existed. A row that
broke another author's claim also carries a `takeover` field:

- `takeover: explicit` (op `takeover`) — the move needed `--takeover`;
- `takeover: stale` (op stays `phase`) — the claim had gone quiet longer than
  `workset_takeover_seconds`, so no flag was needed.
- `op: note` — only the entry note changed (the phase stayed the same); the
  reason is recorded instead of being dropped silently.

Every other row has no `takeover` field. `workset show --journal N` flags an
entry **`self_verified`** when the same author moved it through
`in_progress`/`testing` and `done`: the "verified and accepted" verdict was
never checked by anyone else. The marker is computed only from the loaded
journal rows, and only when a `testing` (or `in_progress`) row by that author
was recorded before the `done` transition. It is therefore absent with
`--journal 0`, and absent when the proving row falls outside the `N`-row
window: a missing row proves nothing, so it is never guessed from the live
document. It is a reading aid, not a verdict — solo work is legal — but the
next session should know the `done` is one agent's word.

## CLI

```
velruna workset create <name> [--project <p>]
velruna workset list [--project <p>]
velruna workset show <name> [--after N] [--before N] [--limit N] [--phase <p>,...] [--live-only] [--journal N]
velruna workset add <name> <fact-id>... [--phase <p>] [--note <s>]
velruna workset remove <name> <fact-id>...
velruna workset phase <name> <fact-id>... --to <p> [--note <s>] [--takeover] [--takeover-after 90s]
velruna workset rename <name> <new-name>
velruna workset delete <name> --yes
velruna workset cursor <name> [--at N | --clear] [--author A] [--host H]
```

`add`, `remove` and `phase` accept several fact ids in one call, so a batch
composition change is one document write, not N. `delete` refuses without
`--yes`; `cursor` without `--at`/`--clear` only prints the current position.

A new entry starts at `todo`. **`add --phase` accepts every phase**
(`todo | in_progress | blocked | testing | done | cancelled`): it is the
entry's initial state, so the transition matrix does not apply to it. The
`done` case is a documented exception to the testing-only entry rule: it is
for reference-only links — facts that belong on the list for orientation but
need no work here, so they never pass through `testing` on this workset.

```bash
velruna workset create checkout-flow --project store
velruna workset add checkout-flow <fact-id> <fact-id> --note "queue for review"
velruna workset phase checkout-flow <fact-id> --to in_progress
velruna workset cursor checkout-flow --at 3
velruna workset show checkout-flow --live-only --journal 20
velruna workset delete checkout-flow --yes
```

## MCP tools

| Tool | Purpose |
|---|---|
| `velruna_workset_create` | Create a workset |
| `velruna_workset_list` | List worksets (by name/project) |
| `velruna_workset_show` | Read one workset: entries, computed `fact_state`, phases, cursors |
| `velruna_workset_add` | Add fact references (several per call; optional `phase`) |
| `velruna_workset_remove` | Remove references |
| `velruna_workset_phase` | Move an entry's phase (`takeover` when required) |
| `velruna_workset_rename` | Rename (old name goes to `aliases`) |
| `velruna_workset_delete` | Delete a workset (requires `confirm: true`) |
| `velruna_workset_cursor` | Set the calling author's reading position |

Registration and general MCP behavior: [docs/mcp.md](mcp.md).

## Config

| Key | Default | Purpose |
|---|---|---|
| `worksets_index_name` | `velruna-worksets` | The workset documents index |
| `worksets_history_index_period` | *(empty — inherits `history_index_period`)* | Journal granularity (`day\|week\|month\|quarter\|year`); one setting aligns every journal by default, a dedicated value may differ |
| `workset_takeover_seconds` | `3600` | Ownership-lock lifetime; must be > 0 (a zero window would defeat the lock entirely) |

## Storage

Two indices, same shape as the facts journal: `velruna-worksets` holds the
workset documents, `velruna-worksets-history-<period>` holds the audit journal
(one index per period, e.g. `...-history-2026-11`).

Every write to a workset document is a **compare-and-swap conditioned on
`_seq_no` and `_primary_term`** (the pair read with the document; `version` is
only a human-readable counter): two agents editing the same workset do not
silently overwrite each other. The losing write is refused and the conflict
names the other writer — re-read the fresh document and re-apply. Cursor moves
are retried up to three times because they are idempotent. Fact documents and
workset documents are never written in each other's paths: a workset does not
own the facts it references.

Storage model background: [docs/how-it-works.md](how-it-works.md).
