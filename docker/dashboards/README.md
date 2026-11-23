# Dashboards bundle

`internal/dashboards/objects.ndjson` — source of the saved-object bundle for
the "Velruna — store overview" dashboard: 3 index patterns, 18 visualizations,
the dashboard itself. The file is embedded into the binary (`go:embed`), so the
push needs nothing but `velruna` itself. Stable `velruna-store-*` ids make the
push idempotent (update in place).

Push with `velruna dashboards push` (the same credential chain as the MCP
server: `OPENSEARCH_PASSWORD`, then the OS keyring, then the config file when
`allow_plaintext` is on; the cluster address comes from the config,
`OPENSEARCH_URL` or the default). The command talks to the CLUSTER, not the Dashboards port: it indexes
the objects directly into the `.kibana` alias backing index, which IS the
global tenant's storage. The saved-objects REST API is not used on purpose —
the `security_tenant: global` header is ignored for basic-auth requests
(verified on OSD 3.8): REST writes always land in the authenticated user's
personal tenant index (`.kibana_<hash>_<user>_1`) and never appear in the
global UI. Direct indexing writes the canonical OSD 3.x typed shape: doc id
`<type>:<id>`, attributes nested under a `"<type>"` field, pinned
`migrationVersion` (index-pattern 7.6.0, visualization 7.10.0,
dashboard 7.9.3). After a push, switch the UI to the Global tenant and hard
reload (the SPA caches the index-pattern list per session).
Do NOT use the OSD `_import` endpoint: OSD 3.x strips the
visualization's index reference during import and the panels then fail
with "Trying to initialize aggs without index pattern".

Registered classic visualization types in OSD 3.8 (from the source):
pie/line/area/histogram/horizontal_bar/heatmap/gauge/goal (vislib),
metric (vis_type_metric), table, markdown, tagcloud, vega, timelion,
metrics (TSVB — needs panel config, do not hand-write), tile_map,
region_map, input_control_vis. `basic` was REMOVED in 3.x.
`params: {}` is valid — vislib merges its defaults.

Bundle invariants (OSD 3.x quirks, all learned live):

- visualizations reference their index pattern via
  `"indexRefName": "kibanaSavedObjectMeta.searchSourceJSON.index"` inside
  `searchSourceJSON` plus a matching `references[]` entry — the old
  `"index": "<name>"` placeholder form is treated as a literal id and
  fails with "The index pattern ... no longer exists";
- index patterns must carry a non-empty `fields` list up front: an empty
  list cannot resolve the `timeFieldName` and every query dies with
  "Could not locate that index-pattern-field";
- index-pattern field descriptors use the OSD field-type enum, not the raw
  es type: a saved field of type "keyword" fails Terms aggs ("is invalid
  for use with the Terms aggregation") — keyword/text map to "string",
  long/integer to "number" (`internal/dashboards/fields.go` encodes this);
- visualization `attributes` must not contain `optionsJSON` (strict
  saved-object mapping rejects it; options live inside `visState`);
- the dashboard's `panelsJSON` must be a JSON **array** of panel entries
  (`{"version","panelIndex":N,"panelRefName":"panel_N","gridData":
  {"x","y","w","h","i"},"embeddableConfig":{}}`);
  the map form `{"p1":{...}}` saves without complaint but the dashboard
  renders completely empty; entries must NOT carry a `"type"` field —
  with `"type":"visualization"` present OSD 3.x collapses every panel to
  a 1x1 tile (the UI itself writes type-less entries);
- the classic grid is **48 columns** wide in OSD 3.x (ground truth: the UI
  saved `w=48` for a panel stretched to full width; the old bundle maxed at
  x+w=48 too): full width = 48, halves = 24, quarters = 12;
- `gridData.i` must equal `String(panelIndex)` — with a mismatched `i` the
  layout cannot be matched to the panel and every tile collapses to 1x1;
- do NOT hand-write terms `orderBy: {"<metricId>":"desc"}` — OSD 3.x can
  translate it into a malformed ES order path ("failed to parse field
  [order]"); vislib already orders by the metric descending by default;
- patterns must list the `_index` meta field explicitly (aggregatable
  "string", esType `_index`) for aggs on it (`metaFields` in
  `internal/dashboards/fields.go`);
- `visState` aggs live at the TOP level (`{"title","type","params",
  "aggs":[...]}`), not under `params`;
- the dashboard pins `timeRestore` + `now-90d..now`, otherwise the
  default "last 15 minutes" renders it empty.

Panel inventory (18 visualizations + the dashboard): 6 KPI tiles (facts,
journal entries, docs cached, facts with history, obsolete facts, Docs past
TTL — a filtered metric over `expires_at`; three of the six are filtered:
facts with history, obsolete facts and Docs past TTL),
3 time-area charts (facts updates / journal writes /
docs fetched over time), journal ops histogram, journal entries per period
index, three donuts (project/category/status), three docs breakdowns (by
via / fetched_by / project), top fact keys horizontal bar.
Index SIZES are deliberately absent: sizes live outside searchable data and
Vega url data sources are GET-only (the OSD console proxy is POST-only).
A size TREND needs a self-monitoring metric (planned, not built yet).
Live sizes meanwhile:
Stack Management -> Index Management, or `velruna status` for the cluster view.

After a mapping change (new fact/journal/doc fields) regenerate the field
lists from a live cluster, rebuild the binary and push again:

```
velruna dashboards gen-fields --conf <store.conf>  # rewrites internal/dashboards/objects.ndjson
make build && ./velruna dashboards push
```

The generator rewrites only the `fields` list of the patterns it can read: a
pattern whose index does not exist keeps its list and is reported, and every
other byte of the bundle stays where it was.
