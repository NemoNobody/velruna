# Backup & restore

`velruna` keeps everything in OpenSearch, so the backup plan is the standard
OpenSearch snapshot API — no bespoke export tooling.

What matters:

- the facts index (`index_name`, default `velruna`) — **irreplaceable**: this is
  the accumulated memory;
- the journal indices (`<index_name>-history-*` — dated, one per period) —
  **irreplaceable** too: they are the history of every accepted value; a
  snapshot that skips them keeps the facts and loses the chain;
- the docs index (`docs_index_name`, default `velruna-docs`) — a cache: a lost
  snapshot only costs re-fetches, include it if convenient;
- snapshots carry **plaintext facts** exactly like the store: the same rule
  applies — no secrets in facts, protect the backup location like the data
  directory itself.

## 1. Filesystem repository (single machine)

`path.repo` is a node setting: it must be declared in `opensearch.yml` before a
repository can be registered there (dynamic registration into an undeclared
location is refused by design).

As root (package install):

```bash
mkdir -p /var/lib/opensearch/backup
chown opensearch:opensearch /var/lib/opensearch/backup
chmod 770 /var/lib/opensearch/backup

# declare the location, then restart the node
echo 'path.repo: ["/var/lib/opensearch/backup"]' >> /etc/opensearch/opensearch.yml
systemctl restart opensearch
```

Register the repository (it survives restarts; one-time):

```bash
curl -k -u "admin:$OPENSEARCH_PASSWORD" -X PUT \
  "https://127.0.0.1:9200/_snapshot/velruna-fs" \
  -H 'Content-Type: application/json' \
  -d '{"type":"fs","settings":{"location":"/var/lib/opensearch/backup","compress":true}}'
```

## 2. Snapshot

```bash
curl -k -u "admin:$OPENSEARCH_PASSWORD" -X PUT \
  "https://127.0.0.1:9200/_snapshot/velruna-fs/backup-$(date +%Y%m%d)?wait_for_completion=true" \
  -H 'Content-Type: application/json' \
  -d '{"indices":"velruna,velruna-history-*,velruna-docs","ignore_unavailable":true,"include_global_state":false}'
```

Check it:

```bash
curl -k -u "admin:$OPENSEARCH_PASSWORD" \
  "https://127.0.0.1:9200/_snapshot/velruna-fs/backup-$(date +%Y%m%d)/_status?pretty"
```

Schedule the `curl` with cron/systemd-timer; prune old snapshots by deleting
their names (the API removes repository files safely):

```bash
curl -k -u "admin:$OPENSEARCH_PASSWORD" -X DELETE \
  "https://127.0.0.1:9200/_snapshot/velruna-fs/backup-YYYYMMDD"
```

## 3. Restore — and verify it

A backup you never restored is not a backup. Restore to new index names so the
running store is untouched. On a single node the cluster is `yellow` (the
replica of the restored index can never allocate) and the plain restore
answers 503 while it waits for allocation — set the replica count in the
restore itself:

```bash
curl -k -u "admin:$OPENSEARCH_PASSWORD" -X POST \
  "https://127.0.0.1:9200/_snapshot/velruna-fs/backup-YYYYMMDD/_restore" \
  -H 'Content-Type: application/json' \
  -d '{"indices":"velruna,velruna-history-*","include_global_state":false,
       "ignore_unavailable":true,
       "rename_pattern":"velruna","rename_replacement":"velruna-restore-test",
       "index_settings":{"index.number_of_replicas":0}}'
```

(`rename_pattern` is a prefix regex: `velruna-history-2026.11` restores as
`velruna-restore-test-history-2026.11`; `ignore_unavailable` lets a store
that never wrote a journal entry restore without the missing pattern.)

(OpenSearch `_restore` has no `wait_for_status` parameter — Elasticsearch
recipes using it fail with 400 here.)

Verify, then clean up:

```bash
velruna status                              # the real index is untouched
curl -k -u "admin:$OPENSEARCH_PASSWORD" "https://127.0.0.1:9200/velruna-restore-test/_count?pretty"
# compare _count with the live index, sample a known key with velruna get, then:
curl -k -u "admin:$OPENSEARCH_PASSWORD" -X DELETE \
  "https://127.0.0.1:9200/velruna-restore-test,velruna-restore-test-history-*"
```

A full recovery (store lost): create the index name as it was (the mapping is
restored from the snapshot), stop `velruna mcp`, restore without the rename,
restart the MCP server.

## 4. Off-box backup

The fs location can be any mount (`rsync` it away, mount an S3 fuse bucket, …).
For native remote snapshots use the s3 repository plugin: declare
`s3.client.*` in `opensearch.yml`, register `{"type":"s3","settings":{"bucket":…}}`.
Same snapshot/restore calls.

## 5. Containers (dev-opensearch, single-machine recipe)

Containers lose data when the operator cleans up. Two rules:

- put the data directory in a **named volume** (not an anonymous one —
  anonymous volumes get pruned);
- take a snapshot **before** `compose down`: snapshot → copy the location out
  of the volume → `down`. The repository location must be declared in the
  container's `opensearch.yml` the same way as above (bind-mount the directory
  so copying it out is trivial).

The compose stack already declares `/snapshots` (`path.repo`; the image
entrypoint forwards only dotted lowercase env keys) and bind-mounts it at
`./os-snapshots`. The dev profile runs plain HTTP with the security plugin
off, so its calls drop `-k -u` and use `http://127.0.0.1:9200` (or
`$VELRUNA_OS_PORT`); the `secure` profile declares no snapshot mount, so
register a location it actually holds out of `path.repo` there.
Register the repository on the dev location once:

```bash
curl -X PUT \
  "http://127.0.0.1:9200/_snapshot/velruna-fs" \
  -H 'Content-Type: application/json' \
  -d '{"type":"fs","settings":{"location":"/snapshots","compress":true}}'

# the §2 snapshot/status/prune calls, same scheme and no auth on this profile
curl -X PUT \
  "http://127.0.0.1:9200/_snapshot/velruna-fs/backup-$(date +%Y%m%d)?wait_for_completion=true" \
  -H 'Content-Type: application/json' \
  -d '{"indices":"velruna,velruna-history-*,velruna-docs","ignore_unavailable":true,"include_global_state":false}'
```
