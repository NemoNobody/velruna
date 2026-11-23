# exec

Run a long-output command **inside velruna** so the full output lands in the
docs index and only a short summary enters the agent's context. Test runs,
build logs, log sweeps — commands whose output is the task but does not fit a
context window — get stored once, block by block, like a fetched page, and
read on demand afterwards.

```
velruna exec [--timeout 120s] [--max-bytes 4194304] [--force] [--propagate-exit]
             [--project P] [--key K] -- cmd [args...]
```

## Behavior

- **No shell by default.** Everything after `--` is argv, executed directly:
  no quoting games, no shell-injection surface. A shell is an explicit choice
  the caller names itself (`sh -c ...`).
- **stdout and stderr are captured merged**, in stream order.
- **Streaming capture.** Output is consumed as it arrives; memory is
  O(`--max-bytes`), not O(output). A 2 GB log never needs 2 GB of heap.
- The stored text becomes **blocks in the docs index** — the same machinery
  fetched pages use — and the answer prints the document hash, the total byte
  count and the number of sections.

## Deduplication

The document key is `sha256(resolved cwd NUL argv[0] NUL argv[1] ...)`: the
resolved directory and every argv element are joined with NUL separators, so
`["echo hi"]` and `["echo", "hi"]` are different commands. Re-running the same
command in the same directory targets the same document. `--key K` pins an
explicit key instead of the derived one; `--project P` tags the document the
way fetched docs are tagged.

Dedupe compares `ContentHash`, the sha256 of the **full raw stream, before
redaction and before truncation**. A change in the dropped middle of a
truncated capture is still a change, and a rewritten title or redacted span
does not move the hash. If that hash is unchanged and `--force` is absent,
exec prints `unchanged` and rewrites nothing — the document stays exactly as
it was, and the summary reports the stored copy's size and truncation state
(this run's `--max-bytes` may differ).

Known limitation: environment- and time-dependent commands (`date`, build
durations, hostnames) legitimately report `changed` on every run. Dedup is
content equality, not a semantic "nothing important changed".

## Limits

- `--max-bytes` caps the **stored text**, never the accounting: the total
  bytes the command produced are always reported. When the cap bites, velruna
  stores head 50% + tail 50% and sets `truncated: true` — build and test logs
  usually fail at the tail and explain themselves at the head; the middle is
  what goes.
- `--max-bytes 0` means unlimited. That is an explicit choice, not the
  default: an unbounded capture is an unbounded document.
- `--timeout` (default 120s) bounds the run. On expiry the process is killed,
  the partial output is kept and stored, and the answer carries
  `timed_out: true` (plus `signal: killed` and `exit_code: timeout`). A hung
  command still leaves its evidence behind.

## Security

- The captured output and title pass the **same secret guard as fact text**,
  before the store write: high-confidence credential shapes and
  medium-confidence generic tokens (a long hex or base64 blob) are replaced
  in place and reported as `redactions: N`. There is no write refusal for body
  text; only the identity fields can refuse (`--project` at any confidence,
  `--key` at high confidence), because they are stored as identity and cannot
  be rewritten. The guard policy (the same `secret_guard` config) is described
  in [docs/mcp.md](mcp.md).
- The command runs with the **invoking user's permissions and environment**.
  velruna adds no sandbox and promises none — isolation is the caller's
  problem.
- **There is no MCP tool for exec, on purpose.** The MCP server must not
  spawn processes; exec is a CLI command, and an agent that needs it runs it
  the way it runs any other command.

## Exit codes

| Case | Code |
|---|---|
| Capture succeeded (whatever the child exited with) | `0` |
| `--propagate-exit`: the child's exit code | `1..255` |
| `--propagate-exit`: timeout | `124` |

Without `--propagate-exit` the command reports whether the *capture* worked —
a failed build captured cleanly is still a successful exec. With it, the exit
code becomes the child's, so a script can branch on it.

## Reading the output back

The same tools that read a fetched page:

```bash
velruna doc <hash>            # the outline
velruna doc <hash> 3          # block 3
velruna docs "FAIL"           # BM25 over stored documents
```

and `velruna_search_docs` over MCP. The intended loop: run exec, read the
outline, read the one block that matters — usually the tail where it failed —
and leave the rest in the index.

How the docs index and its blocks work: [docs/how-it-works.md](how-it-works.md).
