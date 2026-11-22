# Dashboard

A ready-made OpenSearch Dashboards overview of the store — "Velruna — store
overview". It covers facts, the journal (history) and cached documents:

| Panel | Answers |
|-------|---------|
| Facts / Journal entries / Docs cached / Facts with history / Obsolete facts / Docs past TTL (KPI row) | how much is stored, how live the journal is |
| Facts updates over time / Journal writes over time / Docs fetched over time | activity trends (area charts over the time field) |
| Journal ops | writes/rediscoveries/retires/supersedes per bucket (histogram over `op`) |
| Journal entries per period index | how the monthly history indices grow |
| By project / category / status (donuts) | what kind of knowledge is in the store |
| Top fact keys by journal writes | the hottest facts (re-verification candidates) |
| Docs by via / fetched_by / project (donuts) | which route fetched the cache (plain, proxy, renderer), which host, for which project |
| Docs past TTL | snapshots whose `expires_at` has passed: re-fetch candidates (the cached content is still readable) |

## Install

The bundle (`internal/dashboards/objects.ndjson`: 3 index patterns, 18
visualizations, the dashboard) is embedded in the binary and pushed with:

```bash
velruna dashboards push                       # resolves the .kibana write index
velruna dashboards push --conf /path/velruna.conf
velruna dashboards push --index .kibana_1     # pin one index, skip the alias probe
```

On the dev stack `make dev-opensearch-dashboards` does both at once — it
starts the UI, waits for it and pushes the bundle automatically.

It resolves cluster credentials with the same chain as the server
(`OPENSEARCH_PASSWORD` → the OS keyring → the config file when `allow_plaintext`
is on) and writes the saved objects **directly into the global tenant storage**
(the `.kibana` alias backing index). Dashboards must have started at least once
against the cluster: the saved-objects index only exists after that, and the
command says so when the alias is missing.

After the push: open Dashboards, make sure the tenant switcher (avatar menu →
Switch tenants) shows **Global**, and hard-reload the tab — the SPA caches
the index-pattern list per session. A quick start for the whole stack:
`make dev-opensearch && make dev-opensearch-dashboards` (see
`docker/dashboards/README.md` for the bundle's invariants).

### Privileges

The push never touches the facts or docs indices, but it needs:

- **default path**: `indices:admin/aliases/get` (resolve `.kibana`), write
  access to the `.kibana*` indices, and `indices:admin/refresh` for the final
  refresh (a write-only role writes everything and then exits non-zero on the
  refresh);
- **`--index` path**: `indices:admin/exists` (the existence probe) and
  `view_index_metadata` / `indices:admin/mappings/get` to verify the mapping,
  plus the same write and refresh rights. Role with neither can still write,
  but the command then warns that the target could not be verified — and a
  wrong-but-existing index would look green.

## How it reads the store

> An older store that still uses the pre-rename index names
> (`index_name`/`docs_index_name` pinned to `memento*`) needs the index
> patterns edited in the bundle before pushing, and the `memento-store-*`
> saved objects from an earlier push removed.

- Three index patterns with time fields: `velruna` (facts, `updated_at`),
  `velruna-history-*` (journal, `created_at`), `velruna-docs` (doc snapshots,
  `fetched_at`). The dashboard pins a `now-90d` window (`timeRestore`).
- The index patterns carry their field lists inside the bundle (OSD cannot
  introspect them over the API). After a mapping change regenerate and push:

  ```bash
  velruna dashboards gen-fields --conf <store.conf>   # rewrites the embedded bundle
  make build && ./velruna dashboards push            # rebuild: the bundle is embedded
  ```

- Journal entries are also readable without Dashboards: `velruna journal
  <key>` (entries) and `velruna history-info <key>` (counts).

## Troubleshooting

| Symptom | Cause / fix |
|---------|-------------|
| `Trying to initialize aggs without index pattern` | the objects came through `_import` — push with `velruna dashboards push` instead |
| `Could not locate that index-pattern-field (id: …)` | stale/empty field list in a pattern — re-run `velruna dashboards gen-fields` + push |
| Dashboard opens completely empty (no panels) | hand-edited `panelsJSON` in map form — it must be a JSON array (the shipped bundle is correct) |
| `AnalyticEngineError: does not support DSL queries` | stale SPA cache — hard reload the tab |
| Dashboard not in the list | you are in another tenant — avatar → Switch tenants → Global |
