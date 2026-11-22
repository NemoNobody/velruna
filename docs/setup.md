# Setup

## Requirements

- **OpenSearch** — you provide it: `velruna` connects to an existing cluster and
  does not embed a store. Install and configure it per the official
  documentation (see [section 1](#1-opensearch)).
- **Go 1.26+** — only if you build `velruna` from source (`go build -o velruna .`).

`velruna` talks to OpenSearch over HTTPS with basic auth; the tested version
matrix is 2.19.7, 3.0.0, 3.3.2, 3.8.0 and 3.9.0 (see [section 1](#1-opensearch)).

## 1. OpenSearch

`velruna` needs a reachable OpenSearch — it does not embed a store and does not
ship one. Install and configure it per the official documentation; that part is
not covered here:

- **Install** (packages, Docker, Helm): <https://docs.opensearch.org/latest/install-and-configure/install-opensearch/index/>
  — a single local node is enough to start.
- **Configure** (security, TLS, users): <https://docs.opensearch.org/latest/>

What `velruna` expects:

| What | Value |
|---|---|
| Version | OpenSearch 3.8.0; tested matrix 2.19.7 / 3.0.0 / 3.3.2 / 3.8.0 / 3.9.0 — one live smoke run, with its date and caveats in [testing.md](testing.md#version-matrix--hackversion-matrixsh-2026-11-11); rerun locally with `hack/version-matrix.sh` |
| REST API | `https://127.0.0.1:9200` — HTTPS with the bundled demo certificate is fine (`insecure_skip_verify: true`) |
| Auth | basic auth, user `admin` |

Check that it answers before continuing:

```
curl -k -u admin:<your-password> https://127.0.0.1:9200
```

If you get the cluster info JSON back, the cluster is up. Remember the admin
password — you will put it in the `velruna` config below.

## 2. OpenSearch in a container (dev / single-machine recipe)

`make dev-opensearch` starts a single-node OpenSearch in a container
(docker, podman-compose, podman — auto-detected). This is the fastest
onboarding path and the dev cycle recipe:

```
make dev-opensearch          # up; data in the named volume velruna-os-data
make dev-opensearch-logs     # tail the node logs
make dev-opensearch-down     # stop; the named volume keeps the data
```

The node binds **127.0.0.1:9200 only** and runs with the security plugin off —
this is the dev rule: plaintext credentials are acceptable only while the node
cannot be reached off this machine. Snapshots land in `docker/os-snapshots/`
on the host (`path.repo` is configured). If this machine already runs
OpenSearch on 9200, start the dev node beside it: `VELRUNA_OS_PORT=9299 make dev-opensearch`.

For a dev node, `velruna.conf` uses dummy credentials with plaintext allowed
(the file is `chmod 600`, the node is localhost-only):

```json
{
  "opensearch_url": "http://127.0.0.1:9200",
  "opensearch_user": "dev",
  "opensearch_password": "dev",
  "allow_plaintext": true
}
```

Containers love to lose data: before `make dev-opensearch-down` take a
snapshot (repository `velruna-fs` → `/snapshots` → `docker/os-snapshots/`) —
see [backup.md](backup.md) §5.

### Dashboards (optional UI)

`make dev-opensearch-dashboards` adds OpenSearch Dashboards on top of the dev
node (compose profile `dashboards`, so the plain `up`/`down` cycle does not
pull it in). UI: http://127.0.0.1:5601 (`VELRUNA_DASH_PORT=5699` to move it);
with the security plugin off it opens without login. The target then waits
for the UI and pushes the ready-made bundle into it automatically — after it
finishes, open http://127.0.0.1:5601 and look for "Velruna — store overview".

The bundle ships inside the binary (`internal/dashboards/objects.ndjson`,
embedded): index patterns for `velruna`, `velruna-history-*` and
`velruna-docs`, KPI tiles, activity trends and journal panels, and the
dashboard itself. To install it against your own cluster, run the same
command by hand:

```
velruna dashboards push                       # defaults to the config's opensearch_url
OPENSEARCH_URL=http://db-host:9200 velruna dashboards push
velruna dashboards push --conf ~/.velruna.conf
```

Re-runnable: objects carry stable `velruna-store-*` ids and are overwritten
in place (they go straight into the global Dashboards tenant storage — see
docker/dashboards/README.md). Credentials and TLS settings come from the same
chain as the MCP server: `OPENSEARCH_PASSWORD`, then the OS keyring
(`velruna creds set`), then the config file when `allow_plaintext` is on.

### Multi-node and secure topologies (optional)

Two more compose profiles for smoke-testing what a single dev node cannot
show (all loopback-bound, all torn down by `make dev-opensearch-down`):

- `make dev-opensearch-cluster` — three-node cluster (`velruna-cluster`),
  endpoints `http://127.0.0.1:9400/9401/9402`. With more than one data node
  the client automatically stamps `number_of_replicas: 1` on the facts index,
  the docs index and the journal template, so shard/replica placement — and
  survival of one node — can be watched with `_cat/shards/velruna*`. Point a
  config file at any endpoint (`opensearch_url`); the client talks to the
  cluster through one node like any client does.
- `make dev-opensearch-secure VELRUNA_SECURE_DEV_PASSWORD=<pw>` — single node
  WITH the security plugin (demo config and self-signed certs installed by
  the image entry point). Endpoint `https://127.0.0.1:9443`; connect with
  `opensearch_user: "admin"`, the password via keyring/env, and
  `insecure_skip_verify: true` for the demo certs.

### velruna itself in a container

The image workflow (`.github/workflows/image.yml`) publishes
`ghcr.io/nemonobody/velruna` for `linux/amd64`
and `linux/arm64`, tagged `X.Y.Z`, plus `X.Y` and `latest` for a final release
(never for a pre-release). The image is distroless (no shell,
~14 MB) and runs as uid 65532; it holds the binary only — nothing secret is
baked in.

The package is created **private** on the first push: set it to public once in
the repository's package settings (`Packages` → the package → *Package
settings* → *Change visibility* → public) and link it to the repository.
Without that step an anonymous `docker run` fails with
`denied: requested access to the resource is denied`. Until a release image is
published, `make docker-build` side-steps the registry entirely:
`VELRUNA_IMAGE=ghcr.io/nemonobody/velruna:dev make stack-up`.

```bash
# CLI against a store you already have (config path: --conf, else VELRUNA_CONF)
docker run --rm -i --user "$(id -u):$(id -g)" \
  -v ~/.velruna.conf:/etc/velruna/velruna.conf:ro \
  -e VELRUNA_CONF=/etc/velruna/velruna.conf \
  -e OPENSEARCH_PASSWORD \
  ghcr.io/nemonobody/velruna:0.8.9 status

# No config file at all: the connection can come from the environment
OPENSEARCH_URL=https://os.example:9200 OPENSEARCH_USER=admin docker run --rm -i \
  -e OPENSEARCH_URL -e OPENSEARCH_USER -e OPENSEARCH_PASSWORD \
  ghcr.io/nemonobody/velruna:0.8.9 status

# MCP over stdio: the client starts the process, so the container is run with -i
docker run -i --rm --user "$(id -u):$(id -g)" \
  -v ~/.velruna.conf:/etc/velruna/velruna.conf:ro \
  -e VELRUNA_CONF=/etc/velruna/velruna.conf -e OPENSEARCH_PASSWORD \
  ghcr.io/nemonobody/velruna:0.8.9 mcp
```

Or bring up OpenSearch and run velruna against it (the Dashboards UI stays
opt-in):

```bash
make stack-up        # opensearch up -d, then one velruna status run
make stack-down
```

`docker/velruna.container.conf` is the config the stack mounts: it points at
`http://opensearch:9200` and carries no credentials — the dev node runs with
the security plugin off, and the compose service supplies the dummy `dev/dev`
pair (override with `OPENSEARCH_USER`/`OPENSEARCH_PASSWORD`). A restrictive
umask (077) checks this file out as 0600, which uid 65532 cannot read: `chmod
644 docker/velruna.container.conf` if `make stack-up` fails on the config. Point
`VELRUNA_CONF_FILE=<path>` at your own config or `VELRUNA_IMAGE=<tag>` at a
pinned image. A relative `VELRUNA_CONF_FILE` resolves from the compose file's
directory (`docker/`), not from the shell's working directory.

A mounted config must be **readable by uid 65532**, so `chmod 600` on the host
owner does not work — either `chmod 644` it (it holds no password if the
password comes from the environment), or `chown 65532:65532` it, or run the
container with `--user "$(id -u):$(id -g)"`. Docker creates a *directory* at
the mount source when the host path does not exist, and the container then
fails with `is a directory` — check the path before blaming the image.

Two differences from a host install:

- **no keyring**: a container has no session bus, so the password comes from
  `OPENSEARCH_PASSWORD` or from the config file (`allow_plaintext: true`).
  `-e OPENSEARCH_PASSWORD` forwards it from the client environment;
  `docker inspect` exposes it to anyone who can reach the daemon, so keep the
  socket private (the default) or use an owner-only env-file;
- **author label**: provenance defaults to the OS user inside the container,
  which is `nonroot`. Set `VELRUNA_AUTHOR` and `VELRUNA_HOST` (the compose
  service forwards both) so shared-store facts stay attributable.

## 3. Configure velruna

Config lookup order:

1. `--conf <path>` flag (`velruna mcp`, `velruna status`, `velruna dashboards push`)
2. `VELRUNA_CONF` environment variable
3. `./velruna.conf` (current directory)
4. `velruna.conf` next to the binary
5. `~/.velruna.conf`

Create `~/.velruna.conf`:

```json
{
  "opensearch_url": "https://127.0.0.1:9200",
  "opensearch_user": "admin",
  "insecure_skip_verify": true,
  "index_name": "velruna",
  "history_index_period": "month",
  "allow_plaintext": false
}
```

```bash
chmod 600 ~/.velruna.conf
```

### Several nodes (a real cluster)

Point the client at more than one node and it round-robins between them, so one
unreachable node costs a retry instead of an outage — no proxy or load balancer
in front:

```json
{
  "opensearch_urls": [
    "https://os1.example:9200",
    "https://os2.example:9200",
    "https://os3.example:9200"
  ]
}
```

- `OPENSEARCH_URLS` (comma-separated) overrides the file list; `opensearch_url`
  stays valid for a single node and is used when the list is empty.
- Credentials and TLS settings are shared by every node — one user, one store.
  The keyring account is derived from the **first** address, so keep that node
  first if you reorder the list.
- Each entry gets the same validation as `opensearch_url` (no user info in the
  URL, host required); one bad entry fails the config load with its index.
- `velruna status` prints the whole list (`urls`) and the MCP `velruna_status`
  reports it too.

- Leave `opensearch_password` out of the file: store the password in the OS
  keyring with `velruna creds set` (run it in a terminal — it prompts for the
  password; the password is never an argument, so it never ends up in shell
  history). Resolution order: `OPENSEARCH_PASSWORD` env → keyring → file.
  A plaintext password in the file is only read when `allow_plaintext: true`
  (off by default). Containers and ssh sessions have no keyring — export
  `OPENSEARCH_PASSWORD` there.
- **Keyring by desktop.** On Linux the keyring path is the freedesktop Secret
  Service (`org.freedesktop.secrets` over D-Bus), so any desktop that provides
  one works without DE-specific code:

  | Desktop | Provider | Notes |
  |---|---|---|
  | GNOME | gnome-keyring | login keyring, alias normally present; covered by the keyring tests |
  | KDE Plasma with a KWallet-backed Secret Service | kwalletd / ksecretd | KWallet registers the Secret Service name (collection labelled with the wallet name, default alias when present). Works since KF 6.13 when kwalletd is started by PAM and the name is free (verified on Debian 13); `ksecretd` (KF 6.14+) is the separately packaged variant. If the collection is missing, the one labelled by `VELRUNA_KEYRING_COLLECTION` (default `kdewallet`, the KDE wallet name) is used |
  | KDE Plasma with no Secret Service provider on the bus | KWallet-native (`org.kde.kwalletd6`) | used when the name has no owner: wallet `kdewallet`, folder = service, key = account |
  | XFCE / LXQt / MATE / Cinnamon / tiling WMs | whatever Secret Service is installed (usually gnome-keyring) | works if the daemon runs and the keyring is unlocked |
  | KeePassXC (Secret Service integration) | KeePassXC | entries are namespaced by service + username, same as everywhere |
  | headless / container / ssh | none | no bus, no keyring: export `OPENSEARCH_PASSWORD`; `velruna status` reports `password_from: env` and the keyring error |
  | Windows | Windows Credential Manager (zalando/go-keyring) | exercised by the `windows` CI job, but its locked-store and dismissed-prompt policy is unexplored: for a reproducible setup use `OPENSEARCH_PASSWORD` or a config file only the user can read |

  A renamed KDE wallet is addressed by label on the Secret Service path:
  `export VELRUNA_KEYRING_COLLECTION=<wallet label>` (kwalletrc "Default Wallet").
  The KWallet-native path asks kwalletd for its default wallet and accepts an
  override too: `VELRUNA_KWALLET_NAME=<wallet>` (trimmed; empty keeps the
  kwalletd default).

  The provider is detected with a read-only probe of the bus owner, and an
  unusable one is refused instead of being used anyway:

  On KDE the provider is whoever owns the name first: with `kwalletd` started by
  PAM (`kwalletd6 --pam-login`) the wallet-backed Secret Service wins, and
  gnome-keyring must not be running (mask `gnome-keyring-daemon.service` and
  `.socket` to keep it out); if gnome-keyring gets the name instead, the case
  below applies.

  - **KDE where `org.freedesktop.secrets` is owned by gnome-keyring** (a
    typical Debian KDE install pulls it in as a dependency): its login
    collection gets an ephemeral master key, so `creds set` "succeeds" and the
    password is gone after the next logout. That provider is refused; velruna
    falls back to the KWallet-native backend when it is available, otherwise
    `creds set` fails with a hint.
  - **No owner on the bus** (no daemon, no provider): the keyring is refused
    with a hint — never reported as a working store.
  - **A locked wallet**: `open()` may raise the KDE Wallet password dialog. On
    a desktop, answer it within the call budget (`open` waits ~20 s); a headless
    MCP server has nobody to answer, so the call ends with a deadline and a
    "wallet locked" error instead of hanging. Enable `pam_kwallet5` (or unlock
    the wallet once per session) for an unattended server.
  - **The native fallback is not KDE-only.** Whichever desktop runs kwalletd
    without a Secret Service owner gets the KWallet-native backend; it is a
    durable store there too (only the gnome-keyring-on-KDE case above is
    refused).
  - **Credentials do not migrate.** An entry stored through Secret Service and
    one stored in KWallet are different records: after switching backends (or
    installing ksecretd) run `velruna creds set` once more.

  When the backend refuses, nothing is written: use `OPENSEARCH_PASSWORD`
  (env) or a plaintext `opensearch_password` in a mode-0600 config with
  `allow_plaintext: true`. `velruna creds status` names the refusal reason.

- The index is created automatically on first use — you do not have to create it.
- Newly created velruna indices stamp a replica count from `index_replicas`
  (config, default `-1` = auto): a single data node gets `0` (a replica there
  could never assign — permanent yellow), two or more data nodes get `1`, so
  losing one node loses neither facts nor journal history. Set an explicit
  `0`–`N` to override. The count is not frozen at creation: each client start
  re-checks the existing facts/docs indices and the journal template against
  the current topology and corrects drift — an install that grows from one
  node into a cluster self-heals. In auto mode nothing is changed while the
  health probe fails (an unknown topology is never "fixed" toward 0). The
  realignment is uniform across history periods — per-period cold-tiering
  (replicas=0 on old periods) is not a supported layout; if you want the
  whole store pinned, set an explicit `index_replicas`.
- The history journal is created automatically on the first accepted write: an
  ingest pipeline `<index>-journal-ts` (stamps entries with the server clock),
  a composable index template `<index>-journal`, and dated journal indices
  `<index>-history-<period-date>` (e.g. `velruna-history-2026.11`) — monthly
  by default (`history_index_period`:
  `day`/`week`/`month`/`quarter`/`year`). The store user needs privileges to
  create the template, the pipeline, and `<index>-history-*` indices.
- Journal entries are append-only and never pruned automatically (write
  forever): disk usage grows with the write rate, not with fact count.
  Dated indices keep old periods cheap to inspect, delete, or snapshot;
  `velruna prune` is a manual admin tool for one key.
- Every write is stamped with authorship labels (`author`, `author_source`,
  `model`, `host`, `session`; later writes move `updated_by`). The label comes
  from the store's security plugin (`/_security/authinfo`) when it is
  available, otherwise from the config keys `author`/`model`/`host`/`session`
  (env `VELRUNA_AUTHOR`/`VELRUNA_MODEL`/`VELRUNA_HOST`/`VELRUNA_SESSION`),
  otherwise the OS user. This is self-reported audit metadata — see
  [docs/how-it-works.md](how-it-works.md).
- Long facts stay skimmable: a value over `summary_min_len` characters
  (default 2000) must ship a `summary` (CLI `--summary`, MCP `summary`), kept
  under `summary_max_len` (default 400). Set `summary_min_len: 0` to disable
  the requirement, `summary_max_len: 0` to leave the summary uncapped.
- Block extraction is tunable: `fetch_block_chars` (env
  `VELRUNA_FETCH_BLOCK_CHARS`, default 8000, range 1000..64000) is the target
  size of one stored block — a longer section is split at a line boundary, not
  cut; `fetch_doc_chars` (env `VELRUNA_FETCH_DOC_CHARS`, default 100000, `0` =
  the whole document) is the storage budget for one document — when it runs
  out, whole blocks are dropped from the end and the answer says so
  (`truncated`, `chars_stored`/`chars_total`). The physical ceiling stays
  `fetch_max_bytes` (default 2 MB): a bigger page is refused, not trimmed.
- Upgrading from a pre-journal release: run `velruna migrate-history` (prints
  the plan; `--yes` writes) to move the old in-doc history arrays into the
  journal. Unmigrated facts keep working; their history just stays in-doc.
- A malformed config file fails loudly — `velruna` prints which file it tried to
  read.

Environment variables override the file (the complete list; `velruna status`
lists the active ones by name, never by value):
`OPENSEARCH_URL`, `OPENSEARCH_USER`, `OPENSEARCH_PASSWORD`,
`OPENSEARCH_INSECURE_SKIP_VERIFY`, `VELRUNA_CONF`, `VELRUNA_INDEX`,
`VELRUNA_HISTORY_PERIOD`, `VELRUNA_HISTORY_LIMIT`, `VELRUNA_SUMMARY_MIN_LEN`,
`VELRUNA_SUMMARY_MAX_LEN`, `VELRUNA_SECRET_GUARD`,
`VELRUNA_ALLOW_PLAINTEXT`,
`VELRUNA_AUTHOR`, `VELRUNA_MODEL`, `VELRUNA_HOST`, `VELRUNA_SESSION`,
`VELRUNA_DOCS_INDEX`, `VELRUNA_DOCS_TTL_DAYS`, `VELRUNA_FETCH_TIMEOUT`,
`VELRUNA_FETCH_MAX_BYTES`, `VELRUNA_FETCH_BLOCK_CHARS`,
`VELRUNA_FETCH_DOC_CHARS`, `VELRUNA_FETCH_HOSTS`, `VELRUNA_FETCH_USER_AGENT`,
`VELRUNA_FETCH_PROXY_URL`, `VELRUNA_FETCH_PROXY_FALLBACK`,
`VELRUNA_FETCH_RESPECT_ROBOTS`, `VELRUNA_FETCH_ALLOW_PRIVATE`,
`VELRUNA_FETCH_RENDER`, `VELRUNA_FETCH_OBSCURA_PATH`,
`VELRUNA_FETCH_OBSCURA_STEALTH`,
`VELRUNA_LOCAL_REPO_ROOTS`, `VELRUNA_REPO_SCAN_DEPTH`.

## 4. Verify

```
velruna status
```

Expected output:

```
config file: /home/you/.velruna.conf
url: https://127.0.0.1:9200
index: velruna
history_index_period: month
history_limit: 20
secret_guard: reject
author: "admin" (security)
password: keyring
cluster: opensearch (OpenSearch 3.8.0)
storage: cluster=green index=green unassigned_shards=0 data_nodes=1
disk: busiest data node node-1 at 15% used
status: OK
```

`password:` shows where the password resolves from (`env`, `keyring`, `file`,
`none`); the value is never printed. `velruna creds status` shows the same
plus the keyring state. `author:` is the label stamped on new writes and
which chain link produced it (`security` / `config` / `os`); it is absent
when nothing resolves.

If OpenSearch is unreachable, `status` prints `NOT AVAILABLE` with the reason and
a hint, and exits non-zero. If any environment variable is overriding the file,
it is listed under `env overrides` — check that first when a setting looks wrong.

## 5. First fact

```
velruna add global:demo:hello "hello world" behavior
velruna get global:demo:hello
velruna search hello
velruna list global
velruna obsolete global:demo:hello "demo removed"
```

## 6. curl recipes (raw OpenSearch)

`velruna` never prints the password, so raw curl takes it from one of the
sources:

- **Keyring** (desktop; needs the tools package, e.g. `libsecret-tools` on
  Debian/Ubuntu):

  ```bash
  curl -k -u admin:"$(secret-tool lookup service velruna username 127.0.0.1:9200)" \
       https://127.0.0.1:9200
  ```

- **Environment**: `read -rs OPENSEARCH_PASSWORD` (prompt, nothing lands in
  history), then `curl -k -u "admin:$OPENSEARCH_PASSWORD" https://127.0.0.1:9200`.
- **Plaintext config** (only when `allow_plaintext: true`): read
  `opensearch_password` out of the file with your JSON tool.

## 7. Least-privilege store user (security plugin)

With the security plugin enabled, `velruna` does not need an admin account.
Create a dedicated user whose role can do exactly what the app does — and
**cannot delete the facts index**. The names below use the default index
names; if you set `index_name`/`docs_index_name`, adjust the patterns
(the journal pattern is `<index>-history-*`).

What the app touches, permission by permission:

| Target | Why | Actions |
|---|---|---|
| cluster | version probe, `status` health/disk, journal infra | `cluster:monitor/main`, `cluster:monitor/health`, `cluster:monitor/nodes/stats`, `manage_index_templates`, `manage_ingest_pipelines` |
| `velruna` (facts) | index checks, read, search, create/update docs, replica-count realignment | `read`, `search`, `indices:admin/get`, `indices:data/write/index`, `indices:data/write/update`, `indices:admin/create`, `indices:admin/settings/update` (drift correction of `number_of_replicas`) — **no delete** (the app never deletes facts; `obsolete` is a status, and index loss must be an admin decision) |
| `velruna-history-*` | append entries, read journal, `prune`, pin the ts pipeline | `read`, `search`, `indices:admin/get`, `indices:data/write/*` (append-only by app design; `delete` here exists only for the manual `prune`), `indices:admin/create` (a new period index is auto-created on the first append of that period), `indices:admin/settings/update` (the app pins the ts pipeline on journal indices) |
| `velruna-docs` | fetch cache: store, read, search, re-tag snapshots, replica-count realignment | `read`, `search`, `indices:admin/get`, `indices:data/write/index`, `indices:data/write/update` (re-tagging a stored snapshot with a different project), `indices:admin/create`, `indices:admin/settings/update` |

`indices:admin/get` covers the index-existence HEAD the app runs on every
start and the `GET /<index>` settings read behind `velruna status`.

An older role may be missing a later permission: journal appends also need
`indices:admin/create` and `indices:admin/settings/update` on
`velruna-history-*` (without them appends fail loudly with 403), the docs index
needs `indices:data/write/update` for re-tagging, and since 0.8.0 the facts
and docs patterns also need `indices:admin/settings/update`: the client realigns
replica counts at startup, and without the permission that realignment is
silently refused (the app treats it as best-effort).

`GET /_security/authinfo` (the authorship probe) is self-service: every
authenticated user may call it for themselves, so no extra permission is
needed.

Create it as admin (passwords via prompts so they never land in shell history):

```bash
read -rs OS_ADMIN_PW

cat > /tmp/velruna_role.json << 'EOF'
{
  "cluster_permissions": [
    "cluster:monitor/main",
    "cluster:monitor/health",
    "cluster:monitor/nodes/stats",
    "manage_index_templates",
    "manage_ingest_pipelines"
  ],
  "index_permissions": [
    {"index_patterns": ["velruna"],
     "allowed_actions": ["read", "search", "indices:admin/get",
                         "indices:data/write/index",
                         "indices:data/write/update", "indices:admin/create",
                         "indices:admin/settings/update"]},
    {"index_patterns": ["velruna-history-*"],
     "allowed_actions": ["read", "search", "indices:admin/get",
                         "indices:data/write/*",
                         "indices:admin/create", "indices:admin/settings/update"]},
    {"index_patterns": ["velruna-docs"],
     "allowed_actions": ["read", "search", "indices:admin/get",
                         "indices:data/write/index",
                         "indices:data/write/update",
                         "indices:admin/create",
                         "indices:admin/settings/update"]}
  ]
}
EOF

curl -k -u "admin:$OS_ADMIN_PW" -X PUT \
  https://127.0.0.1:9200/_plugins/_security/api/roles/velruna_app \
  -H 'Content-Type: application/json' -d @/tmp/velruna_role.json

# the app user (the prompt reads the password; the value stays in your shell).
# jq builds valid JSON no matter what the password contains (", \, $...).
read -rs VELRUNA_USER_PW
curl -k -u "admin:$OS_ADMIN_PW" -X PUT \
  https://127.0.0.1:9200/_plugins/_security/api/internalusers/velruna \
  -H 'Content-Type: application/json' \
  -d "$(jq -n --arg pw "$VELRUNA_USER_PW" '{password: $pw}')"

# map the role to the user
curl -k -u "admin:$OS_ADMIN_PW" -X PUT \
  https://127.0.0.1:9200/_plugins/_security/api/rolesmapping/velruna_app \
  -H 'Content-Type: application/json' -d '{"users":["velruna"]}'
rm -f /tmp/velruna_role.json
```

Then set `opensearch_user: "velruna"` in the config and run `velruna creds set`
(and `velruna status`). Two side effects worth knowing:

- Writes now stamp `author: "velruna" (security)` — the name the *server*
  authenticated, which is exactly the point of a shared store.
- The journal template stamps `index.number_of_replicas: 0` on a single-node
  cluster: journal indices stay green (replicas would hang unassigned — the
  classic single-node trap). On a multi-node store the app raises it to `1`
  automatically, and it re-aligns both the template and existing indices at
  every client start — so hand-edits to the stamped replica count do not
  survive; pin the count with `index_replicas` in the velruna config instead.

### Shared store: several users, project separation

A single OpenSearch can back many people and many agents — that is one of the
reasons the store is a server, not an embedded file. Two ways to draw the
boundaries, composable:

- **Separate index namespaces (simplest, hard isolation).** Give each team its
  own config: `index_name: team-a-velruna`, `docs_index_name:
  team-a-velruna-docs`. Everything the app manages — journal indices,
  templates, pipelines — derives from the prefix, so namespaces never touch.
  One least-privilege role per namespace (swap the patterns in the recipe
  above). Cross-team visibility then requires an explicit shared project or a
  second config, by choice.
- **Document-Level Security (same index, filtered views).** Keep one shared
  index set and create a role per group whose index_permissions carry a DLS
  query on the `project` field:

  ```json
  {"index_patterns": ["velruna"],
   "dls": "{\"term\":{\"project\":\"team-a\"}}",
   "allowed_actions": ["read", "search", "indices:admin/get",
                       "indices:data/write/index",
                       "indices:data/write/update", "indices:admin/create"]},
  {"index_patterns": ["velruna-history-*"],
   "dls": "{\"term\":{\"project\":\"team-a\"}}",
   "allowed_actions": ["read", "search", "indices:admin/get",
                       "indices:data/write/*", "indices:admin/create"]}
  ```

  DLS is enforced by the server on every search **and** on get-by-id (a
  foreign fact simply reads as "not found"), and it applies to the journal
  too — journal entries carry the same `project` field, so history visibility
  follows fact visibility. Use `role` + `rolesmapping` per group; a user may
  hold several such roles to see several projects.

Two honest caveats for the shared layout:

- `manage_index_templates`/`manage_ingest_pipelines` are cluster-level: give
  them to one service account (or a cooperative single-user install) that owns
  the journal template and pipeline. A human DLS role above carries **neither**,
  and that is now supported end to end: the client treats a 403 reading the
  pipeline or template, or pinning the pipeline on a journal index, as "this
  infra is externally managed" and proceeds — so humans can read, write facts
  and append history as long as the service account (or an earlier append) has
  provisioned the template and the current period's pipeline. Without that
  provisioning the first append of a new period fails loudly, which is the
  correct signal that the infra owner has not run yet.
- The store is still plaintext. Per-project DLS is *access control*, not
  encryption: never put secrets in facts, docs or journal — the secret-guard
  is there to keep even honest mistakes out.
- DLS is **read** isolation, not write ownership: a client with the app role
  can write any `project`/`author` label (they are self-reported). Cooperative
  stores rely on the agents behaving; a store where users must not overwrite
  each other's keyspace needs per-project roles or per-project indices.

Per-user authorship then comes for free: each user's writes carry their
server-authenticated name (`author: "..." (security)`), and the journal shows
who changed what.

The dev compose (`make dev-opensearch`) runs with the security plugin off and
no auth at all — that is the documented dev posture (localhost-only bind).

## 8. JS rendering (optional, obscura)

Sites that gate on TLS/HTTP fingerprints answer plain fetches with
403/422/429. `fetch_render: "obscura"` adds a fallback through the obscura
headless browser (Chrome-shaped stealth fingerprint). It is opt-in: a
third-party binary (~75 MB, linux x86_64, glibc 2.35+) that executes page
JS locally.

```
make obscura
```

installs `obscura` and `obscura-worker` into `~/.local/bin` (keep them in
one directory) from the sha256-pinned 0.2.6 stealth release; put the
directory on PATH or point `fetch_obscura_path` at the binary. Enable with
`fetch_render: "obscura"` (env `VELRUNA_FETCH_RENDER`); a typo in the value
is a startup error, never a silent plain fetch. Rendered docs are marked
`via: obscura`; rendered pages flow through the same robots gate, guard and
TTL pipeline as plain fetches. v1 limitation: `final_url` of a rendered doc
is the requested URL (the dump mode does not report redirects).

The proxy is the operator's chosen egress and the renderer must not bypass
it: when `fetch_proxy_url` is set, obscura is passed `--proxy`; when only the
standard `HTTP_PROXY`/`HTTPS_PROXY` environment is configured, the renderer
consults it for the target (the plain transport honors it, so the render must
not run direct behind it). A proxy URL that cannot be forwarded (credentials
in the URL, a scheme-less value like `host:3128`) is a **config load error**
for every command — the render never falls back to direct egress behind your
back. The environment proxy is read through Go's standard proxy machinery,
which normalizes malformed values; the render validates the **parsed struct**,
so a credential hidden in a normalized path never reaches the renderer argv
(`/proc`-visible). `NO_PROXY` applies to the render as well: the renderer
shares the plain transport's egress decision. An explicit `socks5://` proxy is
forwarded to obscura verbatim — the renderer must support the scheme.

SSRF: the plain fetch refuses internal addresses at dial time (loopback,
RFC1918, link-local/cloud metadata, and the IPv4 addresses embedded in v6
tunnels — 6to4, Teredo, the NAT64 well-known prefix with its local /48
variant, and the deprecated IPv4-compatible form `::x.x.x.x`; arbitrary
RFC 6052 prefixes stay a documented residual), and a host refused
that way fails as a network error — which makes it render-eligible. A
POLICY refusal (a host outside `fetch_allow_hosts`, a redirect that would
cross to a blocked host) is final: neither the proxy nor the render
fallback retries it — the operator's wall is the same wall everywhere.
The renderer dials on its
own, so a **direct** render resolves the target through the same rule and
refuses a non-public answer (`fetch_allow_private: true` is the documented
opt-in for self-hosted targets). With a proxy the proxy is the egress and
enforces the policy — run the renderer behind a proxy if you need the
allowlist and the internal-address rule enforced on the render route: obscura
follows redirects on its own, and a DNS-rebinding window between the
pre-check and obscura's dial cannot be closed from velruna's side.
Stored docs also drop image references aimed at internal hosts (the
drop sets `truncated`; `fetch_allow_private` keeps them for self-hosted
documentation).

Fetch honesty limits, stated plainly: `robots.txt` is honored by default
(`fetch_respect_robots`; one config line turns it off for a site that blocks
the agent — the rude default would buy nothing) and gates the REQUESTED URL
(the rules honor RFC 9309 path wildcards `*` and end-anchor `$`; a redirect
to a different host is checked by the allowlist/SSRF wall, not by a re-read
robots file); response bodies are treated as UTF-8 — a server declaring
another charset is not transcoded (mojibake is visible in the stored text;
the renderer or a manual re-encode are the cures), and the renderer's
environment drops every variable naming VELRUNA or OPENSEARCH (the MCP's
own config, up to a password set via env) plus a sandboxed HOME; the rest
of the operator environment passes through, so wrapper scripts around
`obscura_bin` keep working.

Time budget of one `velruna fetch` / `velruna_fetch_doc`, worst case: the
polite 429 retry (a ~2 s pause + one more request) plus the proxy fallback
(one more `fetch_timeout`) plus the render fallback (the nav timeout + 20 s
of process headroom) — a few times `fetch_timeout` plus the render window.
The stages are each bounded; there is no single global deadline (MCP clients
set their own tool timeouts).

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `connection refused` | OpenSearch is not running (or not at the configured URL) — check your cluster and `opensearch_url` |
| `x509: certificate signed by unknown authority` | Self-signed certificate: set `insecure_skip_verify: true` |
| `401 Unauthorized` | Wrong user/password (env, keyring, or config file) — or the security plugin was never initialized |
| `password: none` and a REFUSED hint | The file has a plaintext password while `allow_plaintext` is off: run `velruna creds set`, then delete `opensearch_password` from the file |
| Timeout after 30s | Host unreachable (firewall, wrong host/port) — requests time out after 30s |
| `status` shows an unexpected config file | The lookup chain picked another file — pass `--conf <path>`, or set `VELRUNA_CONF` |
| `search` misses a fact by its key | Search covers `value`, `notes` and `summary`, not keys — use `velruna get <key>` |
| render error reports both reasons, obscura side: `navigation exceeded ... deadline` | The renderer ran but could not load the page: raise `fetch_timeout_seconds` (it is the nav budget) or the site hangs the render route |
| a doc with `via: obscura` whose body is a block page ("Access denied", IP-Block/Country-Block) | The site gates on egress IP/country, not the fingerprint — the renderer passes the TLS wall only: configure `fetch_proxy_url` (the renderer inherits it; a proxy is a new IP), then re-fetch with `--refresh` (the block page stays cached until TTL) |

## Security notes

- **Secrets do not belong in facts** (passwords, tokens, private keys): the index
  is plaintext. The write paths scan for credential-like text — `secret_guard`
  in the config: `reject` (default), `ask` (confirm a false positive with
  `--allow-secret` / `confirm_secret: true`), `allow` (scanning off). A real
  credential refuses the write; a heuristic hit (a long hash, base64) is
  replaced with `[velruna-redacted:pattern]` and listed in `redactions`, so the
  stored value ends up shorter than what was written. Detection is best effort:
  the rule is still the first line of defense.
- The OpenSearch password lives in the OS keyring (`velruna creds set`), not in
  the config file: `allow_plaintext` is off by default, so a file password is
  refused. `velruna creds status` shows the source; neither command prints the
  value.
- Change the default `admin` password before the cluster is reachable by anyone else.
- The facts index is the accumulated memory — back it up (snapshot API; recipe:
  [docs/backup.md](backup.md)). Snapshots are plaintext, like the store.
- `insecure_skip_verify: true` disables certificate verification. TLS is
  verified by default; enable this only for a local node with the demo
  certificate, never for anything shared.
- On a cluster with the security plugin, run `velruna` as a least-privilege
  user (§7): the app cannot delete the facts index, and every write carries
  the authorship label the server authenticated.
