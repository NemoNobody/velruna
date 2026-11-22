package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NemoNobody/velruna/internal/model"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// The journal is the append-only log of accepted fact values. Every accepted
// write appends one entry (op_type=create, so appends never race: two racing
// writers both land), and the main fact doc keeps current_ref = the journal
// id of its current value, so "the journal contains the current value" is a
// fact, not an assumption about write order.
//
// Journal indices are date-partitioned: <prefix>-history-<period-date>
// (velruna-history-2026.11 by default). The period is config because index
// proliferation is cluster load, and the same knob is the admin's pruning
// granularity: delete a whole old index, history for every key ages out.

// journalIndexName derives the journal index for a period and a time. The
// date is part of the name (an undated index is one infinitely growing
// index, which defeats the point). Names are all-lowercase (an OpenSearch
// index name must be): 2026-w02 (ISO week), 2026-q1, 2026.11, 2026.11.09,
// 2026.
func journalIndexName(prefix, period string, t time.Time) string {
	t = t.UTC()
	var date string
	switch period {
	case "week":
		y, w := t.UTC().ISOWeek()
		date = fmt.Sprintf("%d-w%02d", y, w)
	case "quarter":
		date = fmt.Sprintf("%d-q%d", t.Year(), (int(t.Month())-1)/3+1)
	case "month":
		date = t.UTC().Format("2006.01")
	case "day":
		date = t.UTC().Format("2006.01.02")
	case "year":
		date = strconv.Itoa(t.Year())
	default:
		// Config validates the period at load; a raw call with a bad period
		// must not invent a naming scheme: month is the documented default.
		date = t.UTC().Format("2006.01")
	}
	return prefix + "-history-" + date
}

// journalWildcard matches every journal index of a prefix. Reads carry
// ignore_unavailable + allow_no_indices so deleting old indices (the admin
// pruning path) never breaks reads.
func journalWildcard(prefix string) string { return prefix + "-history-*" }

// journalTemplateName is the composable index template applied to every
// journal index at auto-creation. It is app-managed and idempotent: explicit
// mappings stop a first write from auto-mapping created_at as text.
func journalTemplateName(prefix string) string { return prefix + "-journal" }

// journalPipelineName is the ingest pipeline stamping entries with the
// server clock. Writer clocks skew — on a shared store the writers are
// different machines — so entry ordering follows the store's clock, not the
// clients'. The template wires it as the index default_pipeline.
func journalPipelineName(prefix string) string { return prefix + "-journal-ts" }

// buildJournalTemplateBody builds the composable index template: explicit
// mappings (keyword for grouping fields, text for searched values, date for
// timestamps) and the ts pipeline. replicas 0: single-node personal installs
// are the primary shape, and a journal replica would sit unassigned forever.
func buildJournalTemplateBody(prefix string, replicas int) map[string]interface{} {
	return map[string]interface{}{
		"index_patterns": []string{prefix + "-history-*"},
		"priority":       10,
		"template": map[string]interface{}{
			"settings": map[string]interface{}{
				"index.default_pipeline": journalPipelineName(prefix),
				// replicas follows the cluster topology (see indexReplicas):
				// 0 on a lone node (nothing to replicate to), 1+ on a real
				// cluster — history must survive a node the same way facts do.
				"index.number_of_replicas": replicas,
			},
			"mappings": map[string]interface{}{
				"properties": map[string]interface{}{
					"fact_key":      map[string]interface{}{"type": "keyword"},
					"fact_id":       map[string]interface{}{"type": "keyword"},
					"project":       map[string]interface{}{"type": "keyword"},
					"category":      map[string]interface{}{"type": "keyword"},
					"op":            map[string]interface{}{"type": "keyword"},
					"keywords":      map[string]interface{}{"type": "keyword"},
					"value":         map[string]interface{}{"type": "text"},
					"notes":         map[string]interface{}{"type": "text"},
					"source":        map[string]interface{}{"type": "keyword"},
					"superseded_by": map[string]interface{}{"type": "keyword"},
					"marker":        map[string]interface{}{"type": "keyword"},
					"created_at":    map[string]interface{}{"type": "date"},
					"ingest_at":     map[string]interface{}{"type": "date"},
					"author":        map[string]interface{}{"type": "keyword"},
					"author_source": map[string]interface{}{"type": "keyword"},
					"model":         map[string]interface{}{"type": "keyword"},
					"host":          map[string]interface{}{"type": "keyword"},
					"session":       map[string]interface{}{"type": "keyword"},
				},
			},
		},
	}
}

// buildJournalPipelineBody stamps entries with the store clock on ingest.
func buildJournalPipelineBody(prefix string) map[string]interface{} {
	return map[string]interface{}{
		"description": "stamp " + prefix + " journal entries with the server clock",
		"processors": []map[string]interface{}{
			{"set": map[string]interface{}{
				"field": "ingest_at",
				"value": "{{_ingest.timestamp}}",
			}},
		},
	}
}

// infraGate marks one piece of store infrastructure as installed for the whole
// process, and makes concurrent callers wait for the first pass instead of
// skipping it: work that lands on an unprepared index (an append before the
// template exists, a write before the mapping is additive) cannot be undone.
//
// The claim lives for the process: the topology and the store's templates are
// assumed stable while a process runs. A cluster swap or a deleted template
// under the same URL is picked up on restart — the same assumption the replica
// reconcile already makes.
type installGate struct {
	mu   sync.Mutex
	done bool
	// unreadable marks a check that finished without a verdict (the role may
	// not read it): the work must not be retried, and callers that act on the
	// verdict must treat it as unknown.
	unreadable bool
}

// infraGate returns the gate for a key, creating it on first use. Keys are
// prefixed by the piece of infrastructure ("journal|", "mapping|",
// "pipeline|...|<index>") so the pieces do not share a claim.
func infraGate(key string) *installGate {
	infraGatesMu.Lock()
	defer infraGatesMu.Unlock()
	if gate, ok := infraGates[key]; ok {
		return gate
	}
	if len(infraGates) >= maxInfraGates {
		// Bounded: a day-period journal adds one pipeline key per day. Dropping
		// the claims only repeats idempotent work (an index the template already
		// prepared stays prepared), and a caller still holding a dropped gate
		// finishes its own pass harmlessly.
		infraGates = map[string]*installGate{}
	}
	gate := &installGate{}
	infraGates[key] = gate
	return gate
}

var (
	infraGatesMu sync.Mutex
	infraGates   = map[string]*installGate{}
)

// maxInfraGates bounds the per-key claims. A day-period journal adds one
// pipeline key per day; dropping the map only repeats idempotent work.
const maxInfraGates = 512

// ensureJournalInfra installs the journal pipeline and index template.
// PUT pipeline and the template existence check are idempotent, and the
// create-race between processes is benign. The gate marks success only: a
// first-call failure (network blip, 403) must leave the next write free to
// retry, or a long-lived MCP server would append journal entries forever
// without the template's mappings and the server-clock pipeline.
func (c *Client) ensureJournalInfra(ctx context.Context) error {
	gate := infraGate("journal|" + c.cfg.OpenSearchURL + "|" + c.cfg.IndexName)
	// The lock makes a concurrent append wait for the first pass instead of
	// skipping it: an entry written before the template exists would land on an
	// index without the ingest pipeline and the stamped replica count.
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.done {
		return nil
	}
	if err := c.installJournalInfra(ctx); err != nil {
		// Not done: the next append retries.
		return err
	}
	gate.done = true
	return nil
}

// installJournalInfra performs the one-time pipeline and template work.
func (c *Client) installJournalInfra(ctx context.Context) error {
	pipeline := journalPipelineName(c.cfg.IndexName)
	_, getErr := c.api.Ingest.Get(ctx, &opensearchapi.IngestGetReq{PipelineIDs: []string{pipeline}})
	if getErr != nil && !statusIs(getErr, 403) {
		body, _ := json.Marshal(buildJournalPipelineBody(c.cfg.IndexName))
		if _, mkErr := c.api.Ingest.Create(ctx, opensearchapi.IngestCreateReq{
			PipelineID: pipeline,
			Body:       strings.NewReader(string(body)),
		}); mkErr != nil && !statusIs(mkErr, 409) { // lost the create race: it exists now
			return wrapWriteError("create journal pipeline", mkErr)
		}
	}
	// 403 on the pipeline GET: the role has no ingest rights, so the infra is
	// externally managed (the shared-store DLS recipe). Skipping the create is
	// the only way a read/write-only role can append at all.

	tpl := journalTemplateName(c.cfg.IndexName)
	got, exErr := c.api.IndexTemplate.Get(ctx, &opensearchapi.IndexTemplateGetReq{IndexTemplates: tpl})
	if exErr != nil && statusIs(exErr, 403) {
		// Same story as the pipeline: no template read permission = externally
		// managed template, leave it alone and let the append proceed.
		return nil
	}
	if exErr != nil {
		// Missing (or briefly unreadable): create. A lost create-race just
		// re-PUTs the same body — composable templates are replaced by name.
		body, _ := json.Marshal(buildJournalTemplateBody(c.cfg.IndexName, c.indexReplicas(ctx)))
		if _, mkErr := c.api.IndexTemplate.Create(ctx, opensearchapi.IndexTemplateCreateReq{
			IndexTemplate: tpl,
			Body:          strings.NewReader(string(body)),
		}); mkErr != nil {
			return wrapWriteError("create journal template", mkErr)
		}
	} else if desired, ok := c.desiredReplicas(ctx); ok {
		// Drift correction: the template stamps every NEW history
		// index, so a template frozen at replicas=0 on a grown cluster would
		// keep producing unreplicated history forever. Re-PUT with the current
		// topology only when the stamped count disagrees.
		if stamped := templateStampedReplicas(got); stamped >= 0 && stamped != desired {
			body, _ := json.Marshal(buildJournalTemplateBody(c.cfg.IndexName, desired))
			if _, mkErr := c.api.IndexTemplate.Create(ctx, opensearchapi.IndexTemplateCreateReq{
				IndexTemplate: tpl,
				Body:          strings.NewReader(string(body)),
			}); mkErr != nil {
				// Best-effort: the template exists and appends work with it; a
				// failed alignment (read-only store, permission gap) must not fail
				// the write the caller asked for. The create branch above stays
				// fatal — without a template the appends are broken.
				//
				// The drift check runs once per process: a cluster that grows
				// afterwards keeps stamping new history indices with the count
				// read here until the process restarts (the same assumption the
				// replica reconcile makes).
				_ = mkErr
			}
		}
	}
	return nil
}

// statusIs reports whether a typed API error carries an HTTP status code
// (StringError/StructError expose it; transport errors do not).
func statusIs(err error, code int) bool {
	var se *opensearch.StringError
	if errors.As(err, &se) {
		return se.Status == code
	}
	var ste *opensearch.StructError
	if errors.As(err, &ste) {
		return ste.Status == code
	}
	return false
}

// AppendJournal appends one accepted entry (op_type=create: appends never
// overwrite each other) and returns the entry id.
func (c *Client) AppendJournal(ctx context.Context, e *model.JournalEntry) (string, error) {
	return c.appendJournalID(ctx, e, "", "", false)
}

// appendJournalID appends one entry, optionally under a caller-chosen doc
// id. A chosen id with op_type=create makes the append idempotent: a retry
// of the same logical entry collides (409) instead of duplicating — the
// migration path uses this to survive partial-failure reruns. indexName
// overrides the target index: the migration derives it from the entry's
// stored observation time (stable), not the display timestamp (the
// rerun's clock), so a rerun crossing a period boundary lands on the SAME
// doc id in the SAME index and the collision holds. idempotent marks ids
// derived from the fact's content (journalEntryID hashes key/op/value/
// notes/source/observation time): only those can legitimately collide on
// a rerun, and that tuple identifies the logical write — project is
// write-once and never enters the hash, and a category move that also
// changes the value hashes to a different id (a same-value category
// move is a touch: it journals nothing — and never a content id —
// unless it resurrects a dead fact, whose entry goes under a fresh
// uuid ref). Fresh uuid refs (the touch/
// resurrect/correction paths) can never pre-exist, so a 409 there is an
// anomaly — a rewritten body or a middlebox — and must surface, never
// masquerade as a completed append with a dangling current_ref.
func (c *Client) appendJournalID(ctx context.Context, e *model.JournalEntry, id string, indexName string, idempotent bool) (string, error) {
	if !model.ValidOp(e.Op) {
		return "", fmt.Errorf("journal: unknown op %q", e.Op)
	}
	if err := c.ensureJournalInfra(ctx); err != nil {
		return "", err
	}
	index := indexName
	if index == "" {
		index = journalIndexName(c.cfg.IndexName, c.cfg.HistoryIndexPeriod, e.CreatedAt)
	}
	if err := c.ensureJournalIndexPipeline(ctx, index); err != nil {
		return "", err
	}
	body, _ := json.Marshal(e)
	resp, err := c.api.Index(ctx, opensearchapi.IndexReq{
		Index:      index,
		DocumentID: id,
		Body:       strings.NewReader(string(body)),
		Params: opensearchapi.IndexParams{
			Refresh: "wait_for",
			OpType:  "create",
		},
	})
	if err != nil {
		if idempotent && isVersionConflict(err) {
			// The entry is already there (a retry of a deterministic id):
			// the append succeeded before the caller's view of it broke.
			return id, nil
		}
		return "", wrapWriteError("journal append", err)
	}
	return resp.ID, nil
}

// ensureJournalIndexPipeline pins the ts pipeline on a journal index that
// already exists. The heal path matters for indices auto-created during the
// window when infra install failed (journal entries would carry no
// ingest_at: they sort last in reads and become the first prune victims).
// A 404 means the index does not exist yet — its creation applies the
// template (which wires the pipeline), so nothing more is needed. One
// idempotent settings PUT per index per process.
func (c *Client) ensureJournalIndexPipeline(ctx context.Context, index string) error {
	gate := infraGate("pipeline|" + c.cfg.OpenSearchURL + "|" + c.cfg.IndexName + "|" + index)
	// Wait, do not skip: an append that proceeds before the pipeline is pinned
	// writes an entry without ingest_at, which sorts last and becomes the first
	// prune victim.
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.done {
		return nil
	}
	body, _ := json.Marshal(map[string]interface{}{
		"index.default_pipeline": journalPipelineName(c.cfg.IndexName),
	})
	_, err := c.api.Indices.Settings.Put(ctx, opensearchapi.SettingsPutReq{
		Indices: []string{index},
		Body:    strings.NewReader(string(body)),
	})
	// A 404 means the index does not exist (its creation applies the template).
	// A 403 means the role deliberately cannot manage index settings — the
	// shared-store recipe where a service account owns the infra: skip the pin,
	// the externally-managed index already carries the pipeline. Only a genuine
	// transport/server error fails the append and leaves the gate open.
	if err != nil && !isNotFound(err) && !statusIs(err, 403) {
		return wrapWriteError("journal index pipeline", err)
	}
	gate.done = true
	return nil
}

// journalEntryID derives a deterministic doc id for a journal entry from
// its observation content (key, op, value, notes, source, timestamp). The
// migration uses it so a rerun after a partial failure re-appends collide
// with the entries the previous run wrote instead of duplicating them.
func journalEntryID(factKey, op, value, notes, source string, ts time.Time) string {
	h := sha256.New()
	for _, part := range []string{factKey, op, value, notes, source, ts.UTC().Format(time.RFC3339Nano)} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// buildJournalEntriesBody builds the journal read: one fact key, newest
// first, bounded size. Ordering is the ingest pipeline's server clock
// first (writer clocks skew on a shared store — that is why the pipeline
// exists), the entry's own created_at as fallback for entries stored
// without the pipeline.
func buildJournalEntriesBody(q JournalQuery) map[string]interface{} {
	size := q.Limit
	if size <= 0 || size > NavSizeCap {
		size = NavSizeCap
	}
	must := []map[string]interface{}{
		{"term": map[string]interface{}{"fact_key": q.Key}},
	}
	if !q.Since.IsZero() || !q.Until.IsZero() {
		window := map[string]interface{}{}
		if !q.Since.IsZero() {
			window["gte"] = q.Since.UTC().Format(time.RFC3339)
		}
		if !q.Until.IsZero() {
			window["lte"] = q.Until.UTC().Format(time.RFC3339)
		}
		must = append(must, map[string]interface{}{
			"range": map[string]interface{}{"created_at": window},
		})
	}
	return map[string]interface{}{
		"query": map[string]interface{}{
			"bool": map[string]interface{}{"must": must},
		},
		"sort": []map[string]interface{}{
			{"ingest_at": map[string]interface{}{"order": "desc"}},
			{"created_at": map[string]interface{}{"order": "desc"}},
		},
		"size":             size,
		"track_total_hits": true,
	}
}

// journalSearch runs a journal query across the wildcard with the flags
// that make old-index deletes invisible to readers. A 404 (no journal index
// exists yet: a store that never appended) means "empty journal", not an
// error — the same answer the wildcard+allow_no_indices flags give in the
// real cluster.
func (c *Client) journalSearch(ctx context.Context, body map[string]interface{}) (*opensearchapi.SearchResp, error) {
	data, _ := json.Marshal(body)
	yes := true
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{journalWildcard(c.cfg.IndexName)},
		Body:    strings.NewReader(string(data)),
		Params: opensearchapi.SearchParams{
			IgnoreUnavailable: &yes,
			AllowNoIndices:    &yes,
		},
	})
	if err != nil {
		if isNotFound(err) {
			return &opensearchapi.SearchResp{}, nil
		}
		return nil, fmt.Errorf("journal read: %w", err)
	}
	return resp, nil
}

// JournalEntries reads the newest journal entries for a key. total is the
// cluster-side entry count (the honest truncation signal).
// JournalMeta is the journal-read equivalent of SearchMeta: Truncated is
// the honest page signal driven by the match count, and Skipped counts hits
// whose _source failed to decode — index corruption must never be silent
// (the same contract as fact search).
type JournalMeta struct {
	Total     int
	Truncated bool
	Skipped   int
}

// JournalQuery is one journal read: the fact key, the page size, and an
// optional created_at window (zero bounds are open). The window answers "what
// changed since my last session" as a query instead of a full scan.
type JournalQuery struct {
	Key   string
	Limit int
	Since time.Time
	Until time.Time
}

func (c *Client) JournalEntries(ctx context.Context, q JournalQuery) ([]model.JournalEntry, JournalMeta, error) {
	resp, err := c.journalSearch(ctx, buildJournalEntriesBody(q))
	if err != nil {
		return nil, JournalMeta{}, err
	}
	meta := JournalMeta{Total: resp.Hits.Total.Value}
	entries := make([]model.JournalEntry, 0, len(resp.Hits.Hits))
	for _, hit := range resp.Hits.Hits {
		var e model.JournalEntry
		if err := json.Unmarshal(hit.Source, &e); err != nil {
			meta.Skipped++
			continue
		}
		entries = append(entries, e)
	}
	// Truncation is measured against what was DELIVERED (skipped hits are
	// not recoverable by raising the limit — counting them as page entries
	// would claim the page was full when it was actually corrupt).
	meta.Truncated = meta.Total > len(entries)+meta.Skipped
	return entries, meta, nil
}

// buildJournalInfoBody builds the per-key history aggregation: entry counts
// per operation and the surviving journal's time range. size 0: the answer
// is the aggregation, not the documents.
func buildJournalInfoBody(key string) map[string]interface{} {
	return map[string]interface{}{
		"size": 0,
		"query": map[string]interface{}{
			"term": map[string]interface{}{"fact_key": key},
		},
		"aggs": map[string]interface{}{
			"writes": map[string]interface{}{
				"filter": map[string]interface{}{"term": map[string]interface{}{"op": model.OpWrite}},
			},
			"rediscoveries": map[string]interface{}{
				"filter": map[string]interface{}{"term": map[string]interface{}{"op": model.OpRediscovery}},
			},
			"first": map[string]interface{}{"min": map[string]interface{}{"field": "created_at"}},
			"last":  map[string]interface{}{"max": map[string]interface{}{"field": "created_at"}},
		},
	}
}

// HistoryInfo answers the agent's fast-overview question: has this fact
// changed, how often, was it re-discovered, and what is the surviving
// journal's time range. Counts are LOWER BOUNDS: an admin deleting old
// journal indices removes entries the app cannot know about.
func (c *Client) HistoryInfo(ctx context.Context, key string) (*model.HistoryInfo, error) {
	info := &model.HistoryInfo{Key: key}
	fact, err := c.Get(ctx, key)
	if err == nil {
		info.Category = fact.Category
		info.CurrentRef = fact.CurrentRef
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	resp, err := c.journalSearch(ctx, buildJournalInfoBody(key))
	if err != nil {
		return nil, fmt.Errorf("history info: %w", err)
	}
	aggs := map[string]struct {
		DocCount float64  `json:"doc_count"`
		Value    *float64 `json:"value"`
	}{}
	if len(resp.Aggregations) > 0 {
		if err := json.Unmarshal(resp.Aggregations, &aggs); err != nil {
			return nil, fmt.Errorf("history info: decode aggregations: %w", err)
		}
	}
	info.Writes = int(aggs["writes"].DocCount)
	info.Rediscoveries = int(aggs["rediscoveries"].DocCount)
	if v := aggs["first"].Value; v != nil {
		t := time.UnixMilli(int64(*v)).UTC()
		info.FirstAt = &t
	}
	if v := aggs["last"].Value; v != nil {
		t := time.UnixMilli(int64(*v)).UTC()
		info.LastAt = &t
	}
	return info, nil
}

// planJournalPrune decides which journal entries to drop. ids arrive
// newest-first; the newest `keep` are retained, and the entry whose id ==
// currentRef is ALWAYS retained (it is the journal's proof of the current
// value; deleting it breaks the "journal contains the current" invariant
// even when it is older than the kept window).
func planJournalPrune(ids []string, currentRef string, keep int) []string {
	if len(ids) <= keep {
		return nil
	}
	drop := make([]string, 0, len(ids)-keep)
	for i, id := range ids {
		if i < keep || id == currentRef {
			continue
		}
		drop = append(drop, id)
	}
	return drop
}

// PruneJournal drops journal entries beyond the newest keep for a key,
// always sparing the current_ref entry. It returns the number of deleted
// entries. Destructive: the CLI (--yes) / MCP (confirm) barrier must
// precede it.
func (c *Client) PruneJournal(ctx context.Context, key string, keep int) (int, error) {
	if keep < 0 {
		return 0, fmt.Errorf("prune: keep must be >= 0, got %d", keep)
	}
	resp, err := c.journalSearch(ctx, buildJournalEntriesBody(JournalQuery{Key: key, Limit: NavSizeCap}))
	if err != nil {
		return 0, err
	}
	if resp.Hits.Total.Value > len(resp.Hits.Hits) {
		// buildJournalEntriesBody pins track_total_hits, so Total is exact:
		// more entries exist than the readable page can hold, and pruning
		// blind past the cap would delete entries the operator never saw.
		return 0, fmt.Errorf("prune journal: %d entries for %q exceeds the readable cap %d", resp.Hits.Total.Value, key, NavSizeCap)
	}
	if len(resp.Hits.Hits) <= keep {
		return 0, nil
	}
	ref := ""
	fact, gErr := c.Get(ctx, key)
	if gErr != nil && !errors.Is(gErr, ErrNotFound) {
		// A transient read failure must not silently disarm the current_ref
		// protection: deleting the entry that carries the current value
		// breaks the journal's central invariant.
		return 0, fmt.Errorf("prune journal: read fact for %q: %w", key, gErr)
	}
	if gErr == nil {
		ref = fact.CurrentRef
	}

	ids := make([]string, 0, len(resp.Hits.Hits))
	indexByID := map[string]string{}
	for _, h := range resp.Hits.Hits {
		ids = append(ids, h.ID)
		indexByID[h.ID] = h.Index
	}
	drop := planJournalPrune(ids, ref, keep)
	if len(drop) == 0 {
		return 0, nil
	}

	var ndjson strings.Builder
	for _, id := range drop {
		line, _ := json.Marshal(map[string]interface{}{
			"delete": map[string]interface{}{"_index": indexByID[id], "_id": id},
		})
		ndjson.Write(append(line, '\n'))
	}
	bulk, err := c.api.Bulk(ctx, opensearchapi.BulkReq{
		Body:   strings.NewReader(ndjson.String()),
		Params: opensearchapi.BulkParams{Refresh: "wait_for"},
	})
	if err != nil {
		return 0, wrapWriteError("prune journal", err)
	}
	if bulk.Errors {
		return 0, fmt.Errorf("prune journal: some deletes failed")
	}
	return len(drop), nil
}

// migrateLinkScript is the fact-side half of MigrateFact: it drops the
// legacy array and (only while the ref is still empty) links current_ref.
// Same lexer contract as the other scripts: double-quoted literals, no
// backslashes (painless processes no standard escapes — see
// TestPainlessScriptsAreEscapeFree).
const migrateLinkScript = `
if (ctx._source.current_ref == null || ctx._source.current_ref == "") { ctx._source.current_ref = params.ref; }
ctx._source.remove("history");
if (ctx._source.updated_at == null || params.updated_at.substring(0, 19).compareTo(ctx._source.updated_at.substring(0, 19)) > 0) { ctx._source.updated_at = params.updated_at; }
`

// MigrateFact finishes one fact's journal migration (big-bang migration,
// one-time): legacy in-doc history entries become op=write journal entries
// in oldest-first order, the current value gets its own entry when the fact
// is not linked yet, current_ref points at it, and the array is removed
// from _source. It returns the number of journal entries appended.
//
// A fact that already carries current_ref but still has the legacy array
// (a post-upgrade edit linked the ref but left the array behind, or an
// earlier rerun died before the array was dropped) gets ONLY the array
// entries appended: re-appending the current value would fabricate a
// duplicate journal entry, and the link script keeps a live ref untouched.
//
// The appends use deterministic doc ids (journalEntryID) AND target the
// index derived from the same stored time: a rerun after a partial failure
// (some entries appended, the link update failed) collides with the
// entries the previous run wrote and treats the collision as success, so
// reruns never duplicate — not even when the rerun crosses a period
// boundary and the display timestamp (a now-fallback) moves. The ids hash
// the entry's stored observation time (a zero-time marker when the fact
// has none), never the rerun's clock. The link script only sets
// current_ref when it is still empty: a concurrent Add that already linked
// the fact carries a fresher ref (the migration's snapshot predates it),
// and overwriting it would point the new value at the old journal entry.
func (c *Client) MigrateFact(ctx context.Context, f *model.Fact, now time.Time) (int, error) {
	if f.CurrentRef != "" && len(f.History) == 0 {
		return 0, nil // fully migrated: linked, the array is already gone
	}
	// The migration runner authors the appended entries: the original writers
	// predate the journal and left no per-entry labels, and an entry must
	// name the hand that appended it.
	info := c.resolveAuthor(ctx)
	appended := 0
	for _, h := range f.History {
		ts := h.ObservedAt
		if ts.IsZero() {
			ts = f.UpdatedAt
		}
		if ts.IsZero() {
			ts = now
		}
		// idTs excludes the now-fallback on purpose: the id must reproduce
		// on a rerun, and the rerun's clock does not.
		idTs := h.ObservedAt
		if idTs.IsZero() {
			idTs = f.UpdatedAt
		}
		e := model.NewJournalEntry(f.Key, f.FactID, f.Project, f.Category, model.OpWrite, h.Value, "", h.Source, ts)
		info.applyTo(e)
		// The target index comes from idTs (stable), not ts (a rerun's
		// now-fallback): the collision must find the entry the previous run
		// wrote, and doc ids are per-index.
		histIdx := journalIndexName(c.cfg.IndexName, c.cfg.HistoryIndexPeriod, idTs)
		if _, err := c.appendJournalID(ctx, e, journalEntryID(f.Key, model.OpWrite, h.Value, "", h.Source, idTs), histIdx, true); err != nil {
			return appended, fmt.Errorf("migrate %q: history entry: %w", f.Key, err)
		}
		appended++
	}
	ref := f.CurrentRef
	if f.CurrentRef == "" {
		// The current value gets its own entry only while the fact is
		// unlinked: a linked fact's current version is already journaled.
		ts := f.ObservedAt
		if ts.IsZero() {
			ts = f.UpdatedAt
		}
		if ts.IsZero() {
			ts = now
		}
		idTs := f.ObservedAt
		if idTs.IsZero() {
			idTs = f.UpdatedAt
		}
		cur := model.NewJournalEntry(f.Key, f.FactID, f.Project, f.Category, model.OpWrite, f.Value, f.Notes, f.Source, ts)
		info.applyTo(cur)
		ref = journalEntryID(f.Key, model.OpWrite, f.Value, f.Notes, f.Source, idTs)
		curIdx := journalIndexName(c.cfg.IndexName, c.cfg.HistoryIndexPeriod, idTs)
		if _, err := c.appendJournalID(ctx, cur, ref, curIdx, true); err != nil {
			return appended, fmt.Errorf("migrate %q: current entry: %w", f.Key, err)
		}
		appended++
	}

	body, _ := json.Marshal(map[string]interface{}{
		"script": map[string]interface{}{
			"lang":   "painless",
			"source": migrateLinkScript,
			"params": map[string]interface{}{
				"ref":        ref,
				"updated_at": now.UTC().Format(time.RFC3339Nano),
			},
		},
	})
	if _, err := c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: f.Key,
		Body:       strings.NewReader(string(body)),
		Params:     opensearchapi.UpdateParams{Refresh: "wait_for"},
	}); err != nil {
		return appended, fmt.Errorf("migrate %q: link current_ref: %w", f.Key, err)
	}
	return appended, nil
}

// ScanAllFacts returns the facts that still NEED journal migration (size
// cap NavSizeCap), scoped to project or to every project when empty. The
// needs-migration filter runs SERVER-side on purpose: MigrateFact does not
// change keys, so a client-side filter over a key-asc capped scan would
// re-serve the same first page on every rerun — with more than
// NavSizeCap facts the rerun loop could never reach the rest while the
// plan cheerfully reported "0 to migrate". Filtering on the query makes
// reruns converge: migrated facts drop out of the result set and the next
// page surfaces.
func (c *Client) ScanAllFacts(ctx context.Context, project string) ([]model.Fact, SearchMeta, error) {
	return c.search(ctx, buildMigrateScanBody(project))
}

// buildMigrateScanBody builds the migration scan. Writers always store a
// non-empty project, so an empty scope OMITS the filter (a
// term{"project":""} matches nothing and the default migrate-history run
// would silently no-op). "Needs migration" mirrors needsMigration: not
// linked yet (no current_ref, or a legacy empty one) or still carrying the
// pre-journal history array.
//
// The array arm CANNOT be an exists query: the mapping declares history as
// {"type":"object","enabled":false} — stored in _source only, never indexed,
// so exists would match zero documents and the linked-but-array cohort
// (a post-upgrade edit that journalled a new value but left the old array
// behind) would never converge. A _source script query is the only
// semantics that sees the field; migration is a rare admin scan, so the
// per-document script cost is the right trade (an unsupported script query
// fails the scan loudly, never silently no-ops). In the query script
// context the source map is reached via params._source — a bare _source
// symbol does not compile (verified against the cluster); the map can be
// null for some docs, so the guard is load-bearing (verified too). The arm
// requires a NON-EMPTY list: an empty/absent array is already migrated for
// MigrateFact's eyes, and selecting it forever would leave a plan that
// reports matches but migrates nothing (the non-convergence F1 killed).
// A history that is neither absent nor a list (foreign writer, corruption)
// gets its own arm — but only for LINKED docs (an unlinked doc is already
// selected by the current_ref arms regardless). null is excluded: it
// decodes cleanly as "no array", so a null-history doc would re-match the
// scan forever while migration can never change it — the non-convergence
// F1 killed. A genuine non-list stays selected: the Go decode fails and
// the plan reports it as decode-skipped, i.e. corruption that stays
// visible forever is better than a "converged" lie.
func buildMigrateScanBody(project string) map[string]interface{} {
	boolQuery := map[string]interface{}{
		"must": []map[string]interface{}{{
			"bool": map[string]interface{}{
				"should": []interface{}{
					map[string]interface{}{"bool": map[string]interface{}{
						"must_not": []interface{}{map[string]interface{}{"exists": map[string]interface{}{"field": "current_ref"}}},
					}},
					map[string]interface{}{"term": map[string]interface{}{"current_ref": ""}},
					map[string]interface{}{"script": map[string]interface{}{
						"script": map[string]interface{}{
							"lang":   "painless",
							"source": `params._source != null && params._source.history instanceof List && !params._source.history.isEmpty()`,
						},
					}},
					map[string]interface{}{"script": map[string]interface{}{
						"script": map[string]interface{}{
							"lang":   "painless",
							"source": `params._source != null && params._source.history != null && !(params._source.history instanceof List) && params._source.current_ref != null && params._source.current_ref != ""`,
						},
					}},
				},
				"minimum_should_match": 1,
			}},
		},
	}
	if project != "" {
		boolQuery["filter"] = []interface{}{map[string]interface{}{"term": map[string]interface{}{"project": project}}}
	}
	return map[string]interface{}{
		"query":            map[string]interface{}{"bool": boolQuery},
		"sort":             []map[string]interface{}{{"key": map[string]interface{}{"order": "asc"}}},
		"size":             NavSizeCap,
		"track_total_hits": true,
	}
}
