# Testing — what is actually verified

This project treats tests as the agent's self-control: claims of "works" are
backed by executed suites, and this page records what those suites cover and
what was run live against real clusters.

## Unit & integration (mock transport)

- `make ci` = `fmt-check` + `build` + `vet` + `go test ./...` + `golangci-lint` —
  the local gate; CI runs it plus `make test-keyring`, the darwin/windows
  cross-compile step, and native windows and (gated) macOS jobs that build,
  vet and run the platform test suites (`.github/workflows/ci.yml`).
- more than 400 test functions exercise the HTTP contract against a fake
  OpenSearch (`httptest`): which endpoints are called, with what parameters and bodies —
  index creation, composable journal templates, the ingest pipeline,
  snapshot-derived writes (touch, resurrect, retire, status flip) guarded by
  optimistic concurrency (`seq_no`/`primary_term`) while value corrections are
  deliberately last-writer-wins, the prune
  scripts, wildcard reads with `ignore_unavailable`, search bodies.
- The fake transport is deliberate: the contract, not OpenSearch itself, is
  where the historical bugs lived (read-after-write, read-only stores).
- **Doc/code drift guards** — `docsync_test.go` (root package) pins the
  surfaces that review rounds kept re-finding stale: the README command/flag
  table against the cobra tree (subcommand alternatives and short flags
  included), CLI command and MCP tool names mentioned inside user-facing
  strings against the real trees, env-var
  lists in README/setup.md against what our Go code (config, cmd, internal)
  and `docker/opensearch.yml` actually consume, ghcr image
  pins agreeing everywhere, compose profiles all named in setup.md, and a
  small retired-phrase list. They run in `make ci`; when one fails after an
  intentional change, update the doc (or the curated list) so the change
  stays a decision, not silent drift.

## Live CLI smoke — `hack/cli-smoke.sh`

28 checks against one live store (throwaway `smoke:<runid>:` keys): status,
add/correction/get, journal writes, history-info, list/keys/search, obsolete,
secret-guard rejection AND redaction (a high-confidence credential refuses the
write, a heuristic hit is stored with a marker + audit), prune dry-run +
`--yes`, migrate-history, docs search on a missing index. Exit 0 only when
everything passes.

## Version matrix — `hack/version-matrix.sh` (2026-11-11)

Brings the compose dev node up per image tag (fresh volume each time, so the
full infra-creation path runs) and executes the smoke above against it.

| OpenSearch | Result |
|---|---|
| 2.19.7 (last 2.x) | PASS 23/23 |
| 3.0.0 | PASS 23/23 |
| 3.3.2 | PASS 23/23 |
| 3.8.0 (production) | PASS 23/23 |
| 3.9.0 | PASS 23/23 |

The run predates the guard's redaction path and the extractor rework: the suite
had 23 checks then and has 28 now (`hack/cli-smoke.sh` is the live count). The
matrix has not been re-run since; treat it as the compatibility floor (these
versions work), not as a per-release verification. The unit and contract suites
run against the mock transport on every push — that is the per-commit gate.

Run it yourself: `hack/version-matrix.sh` (sequential; needs docker/podman).
The images can also be pinned for everyday dev via `OPENSEARCH_IMAGE` in
`docker/opensearch.yml`.

## Topology — 3-node cluster (2026-11-11)

`make dev-opensearch-cluster` (three nodes on 127.0.0.1:9400–9402): CRUD +
prune against real shard allocation, node kill (primary failover, writes
continue while yellow), node return (back to green, data intact). Automatic
replica stamping (`index_replicas: -1`) verified: 0 replicas on a lone data
node, 1 on a multi-node cluster.

## Keyring — real providers (`make test-keyring`)

The secret store is the one dependency a mock cannot vouch for: the wire
protocol, the default-collection resolution and the "missing entry" sentinel
are provider behavior. `make test-keyring` starts a disposable session bus
(`dbus-run-session`) and a `gnome-keyring-daemon` with a throwaway `HOME`, then
runs `internal/keyringnative` against it — the developer's real wallet is never
touched. The tests also exist behind `VELRUNA_KEYRING_TEST=1`, which is what CI
turns on:

- Linux: `make test-keyring` in the `build-test` job (Secret Service over D-Bus).
- macOS: the `macos` job runs `VELRUNA_KEYRING_TEST=1 go test ./internal/keyring/`
  against the runner's login Keychain (the zalando provider: `security` CLI). It is
  gated to tags and `workflow_dispatch`: a macOS runner bills 10x minutes, so a
  release check is the right cadence (`gh workflow run ci.yml` forces it).
- Windows: the `windows` job runs `vet`, `build`, the platform-neutral packages'
  tests and `VELRUNA_KEYRING_TEST=1 go test ./internal/keyring/` on every push —
  the Credential Manager is exercised for real (zalando -> wincred), not just
  compiled. The job already earned its keep: it caught a HOME-vs-USERPROFILE
  assumption in a config test the Linux suite could never see.

A plain `go test ./...` never writes to a live keychain: the integration tests
skip unless the flag is set.

## Not yet covered (known gaps)

- **secure profile** (`make dev-opensearch-secure`, https + demo certs) —
  exercised by the dispatch-only smoke workflow: the secured stack boots, the
  Dashboards endpoint answers 401 without credentials, and a `dashboards push`
  succeeds. The CLI smoke suite runs only against the plain dev node, and
  CI-on-push starts neither.
- **Windows Credential Manager policy** (locked store, dismissed prompt) — the
  `windows` CI job does create/read/delete on every push, but these paths are
  unexplored.
- The OpenSearch **Dashboards bundle** targets the current OSD line; older OSD
  versions are out of scope by decision: adapting saved objects to every old
  OSD line is a maintenance tax with no paying user.
