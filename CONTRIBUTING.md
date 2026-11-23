# Contributing to velruna

Thanks for your interest! velruna is a small, focused tool: an MCP server for
verified agent memory on top of OpenSearch. Changes that keep that focus are
the most welcome.

## Ground rules

- **Fix the root cause, not the symptom.** Every "it works" claim needs an
  executed test or a live run behind it — see `docs/testing.md` for what is
  already verified and how to extend it.
- Scope discipline: bug fixes and focused improvements are easier to land than
  sweeping refactors. For anything larger, open an issue first to agree on the
  shape.
- The store is plaintext by design: never add code that puts secrets into
  OpenSearch, and keep the secret-guard behaviour intact.

## Development

```
go build -o velruna .     # build
make ci                   # fmt-check + build + vet + go test ./... + golangci-lint (the local gate; CI adds test-keyring and cross-compiles)
make dev-opensearch       # throwaway single-node OpenSearch (docker/podman)
```

Point a throwaway config at the dev node, e.g.
`/tmp/velruna-dev.conf` (the dev stack runs with the security plugin off, so
the credentials are ignored — `dev`/`dev` matches the README):

```
{"opensearch_url":"http://127.0.0.1:9200","opensearch_user":"dev","opensearch_password":"dev","allow_plaintext":true}
```

Then `VELRUNA_CONF=/tmp/velruna-dev.conf ./velruna status`. The dev stack has
security off and binds to loopback only — never expose it.

Useful suites:

- `hack/cli-smoke.sh` — 28 live checks against one store (safe on any store you
  allow a few KB of throwaway keys in);
- `hack/version-matrix.sh` — the same smoke across the verified OpenSearch
  version matrix;
- `make dev-opensearch-cluster` — 3-node topology playground.

## Pull requests

- One logical unit per commit; commit messages in English, explaining *why*.
- `make ci` must pass; add or adjust tests when behaviour changes — tests here
  assert the HTTP contract against the fake cluster (`internal/client/contract_test.go`)
  and the client logic around it.
- User-visible changes: update `docs/` in the same PR and add a `CHANGELOG.md`
  entry under "Unreleased".
- Breaking changes to the stored schema or the MCP tool surface are acceptable
  pre-1.0 but must be called out in the PR and the changelog (with a migration
  note when one is needed).

## Releases

`make release VERSION=vX.Y.Z` is the only supported path: it runs the tests,
requires no modified or staged tracked changes, creates an annotated tag and
pushes it. The `Release` workflow then builds the binaries for every supported
platform and publishes them as a GitHub release whose notes come from the
matching `CHANGELOG.md` section of that tag (`## vX.Y.Z — ...`). Do not create
the release by hand.

Repair path: if a release exists without binaries, or a publish run failed,
re-run the workflow by hand (`Actions → Release → Run workflow`) with the tag
as `version` (it must look like `vX.Y.Z`, or `vX.Y.Z-suffix` for a
pre-release, which is published as a pre-release). The job reuses the existing
release, refreshes its notes from the tag's changelog and re-uploads the
binaries. `dist/checksums.txt` is authoritative for what was built; an asset
left over from an older run is not covered by it.

## License

License note: the project is Apache-2.0; contributions fall under the same license.
