package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// Worksets live in two catalogs, mirroring the facts/journal split:
//
//   - the worksets index holds one document per workset, keyed by the stable
//     workset id (never by name: a rename must not move a document, and aliases
//     would make a name an unstable primary key);
//   - a date-partitioned history index holds the append-only workset journal,
//     named by journalIndexName on cfg.WorksetsIndexName.
//
// The two indices carry their own schema markers (_meta) because they are a
// different document shape from facts and docs, and a newer binary may evolve
// one without the other.

const (
	// worksetsIndexDefault is the fallback when config carries no worksets
	// index name (a hand-built Config, e.g. in tests).
	worksetsIndexDefault = "velruna-worksets"

	// Schema markers stamped on the two mappings. worksetSchemaVersion is the
	// document schema this binary reads and writes; a newer marker is refused
	// (see ensureWorksetSchemaVersion).
	worksetsSchemaKey        = "velruna_worksets_schema"
	worksetsHistorySchemaKey = "velruna_worksets_history_schema"
	worksetSchemaVersion     = 1

	// worksetListSizeCap bounds a project listing. Worksets are few (a fleet
	// coordinates on a handful), so this is a runaway guard, not paging.
	worksetListSizeCap = 200
)

// worksetsIndex is the worksets index name, with the documented default.
func (c *Client) worksetsIndex() string {
	if c.cfg.WorksetsIndexName != "" {
		return c.cfg.WorksetsIndexName
	}
	return worksetsIndexDefault
}

// worksetsHistoryPeriod resolves the workset journal granularity:
// WorksetsHistoryIndexPeriod when set, otherwise the shared journal period.
func (c *Client) worksetsHistoryPeriod() string {
	if c.cfg.WorksetsHistoryIndexPeriod != "" {
		return c.cfg.WorksetsHistoryIndexPeriod
	}
	return c.cfg.HistoryIndexPeriod
}

// worksetsHistoryIndexAt names the workset journal index for a moment, using
// the same date-partitioning as the fact journal.
func (c *Client) worksetsHistoryIndexAt(t time.Time) string {
	return journalIndexName(c.worksetsIndex(), c.worksetsHistoryPeriod(), t)
}

// worksetsHistoryWildcard matches every workset journal index of the prefix.
func (c *Client) worksetsHistoryWildcard() string {
	return journalWildcard(c.worksetsIndex())
}

// buildWorksetsIndexBody is the single worksets catalog: creation and any
// later reconciliation share it, so a field cannot drift between the two.
func buildWorksetsIndexBody(replicas int) map[string]interface{} {
	return map[string]interface{}{
		"settings": map[string]interface{}{
			"index.number_of_replicas": replicas,
		},
		"mappings": map[string]interface{}{
			"properties": buildWorksetsProperties(),
			"_meta":      map[string]interface{}{worksetsSchemaKey: worksetSchemaVersion},
		},
	}
}

// buildWorksetsProperties is the indexed surface of a workset document. The
// handle fields (id, name, aliases, project) and the doc timestamps are
// searched/listed by, so they are mapped.
//
// entries and cursors are stored-only objects (enabled:false): they are read
// back from _source, never queried. That is deliberate — entries carry the
// whole queue and cursors one row per reader, and indexing either would bloat
// the index for no query. The consequence is that per-entry timestamps
// (entries.phase_at / entries.added_at) are not mapped: an object that is not
// indexed cannot also declare sub-fields, and reads take them from _source.
func buildWorksetsProperties() map[string]interface{} {
	return map[string]interface{}{
		"id":         map[string]interface{}{"type": "keyword"},
		"name":       map[string]interface{}{"type": "keyword"},
		"aliases":    map[string]interface{}{"type": "keyword"},
		"project":    map[string]interface{}{"type": "keyword"},
		"created_at": map[string]interface{}{"type": "date"},
		"updated_at": map[string]interface{}{"type": "date"},
		"created_by": map[string]interface{}{"type": "keyword"},
		"updated_by": map[string]interface{}{"type": "keyword"},
		"version":    map[string]interface{}{"type": "long"},
		"entries":    map[string]interface{}{"type": "object", "enabled": false},
		"cursors":    map[string]interface{}{"type": "object", "enabled": false},
	}
}

// buildWorksetsHistoryIndexBody is the workset journal mapping: one immutable
// row per accepted change, grouped by workset_id and searchable by fact.
// prefix is the worksets index name: the journal index name starts with it, so
// the store-clock pipeline is named from the same prefix. ingestion stamps
// ingest_at (server clock), which is the primary sort key; `at` is the writer
// clock and remains the fallback for rows written before the pipeline existed.
func buildWorksetsHistoryIndexBody(replicas int, prefix string) map[string]interface{} {
	return map[string]interface{}{
		"settings": map[string]interface{}{
			"index.number_of_replicas": replicas,
			"index.default_pipeline":   journalPipelineName(prefix),
		},
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"workset_id": map[string]interface{}{"type": "keyword"},
				"workset":    map[string]interface{}{"type": "keyword"},
				"op":         map[string]interface{}{"type": "keyword"},
				"takeover":   map[string]interface{}{"type": "keyword"},
				"fact_id":    map[string]interface{}{"type": "keyword"},
				"from":       map[string]interface{}{"type": "keyword"},
				"to":         map[string]interface{}{"type": "keyword"},
				"by":         map[string]interface{}{"type": "keyword"},
				"at":         map[string]interface{}{"type": "date"},
				"ingest_at":  map[string]interface{}{"type": "date"},
				"note":       map[string]interface{}{"type": "text"},
			},
			"_meta": map[string]interface{}{worksetsHistorySchemaKey: 1},
		},
	}
}

// EnsureWorksetIndexes creates the worksets index and the current workset
// journal index when they are missing, with the same read-first shape as
// EnsureIndex (a read-only cluster must not break reads). A document index
// stamped with a newer schema is refused before anything is written.
func (c *Client) EnsureWorksetIndexes(ctx context.Context) error {
	if err := c.ensureWorksetSchemaVersion(ctx); err != nil {
		return err
	}
	if err := c.ensureIndexWithBody(ctx, c.worksetsIndex(),
		buildWorksetsIndexBody(c.indexReplicas(ctx)), "create worksets index"); err != nil {
		return err
	}
	return c.ensureWorksetHistoryIndex(ctx, c.worksetsHistoryIndexAt(time.Now().UTC()))
}

// ensureWorksetSchemaVersion mirrors ensureSchemaVersion for the worksets
// document index: a marker newer than this binary understands is refused by
// name, an older or absent marker is adoptable (either the store predates the
// marker or the change is additive). A check that cannot read the mapping is
// not a verdict: it leaves the store usable and the gate open, exactly like the
// fact check.
func (c *Client) ensureWorksetSchemaVersion(ctx context.Context) error {
	gate := infraGate("worksets-schema|" + c.storeKey() + "|" + c.worksetsIndex())
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.done {
		return nil
	}

	status, body, err := c.doSavedObjectRequest(ctx, http.MethodGet,
		"/"+c.worksetsIndex()+"/_mapping", nil)
	if err != nil {
		return nil // unreachable store: not a schema verdict, retry next call
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		// No worksets index yet: adoptable, it is created with this schema.
		gate.done = true
		return nil
	case http.StatusForbidden, http.StatusUnauthorized:
		// The role may write but not read mappings. Cache the answer so every
		// command does not pay a refused GET; the verdict is unknown.
		gate.done, gate.unreadable = true, true
		return nil
	default:
		return nil
	}

	var res map[string]struct {
		Mappings struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"mappings"`
	}
	if uerr := json.Unmarshal([]byte(body), &res); uerr != nil {
		return nil // an unparsable mapping is not a version verdict
	}
	found := 0
	for _, idx := range res {
		raw, ok := idx.Mappings.Meta[worksetsSchemaKey]
		if !ok {
			continue
		}
		var v int
		if uerr := json.Unmarshal(raw, &v); uerr != nil {
			return nil // a non-integer marker is unknown, not "unversioned"
		}
		if v > found {
			found = v
		}
	}
	if found > worksetSchemaVersion {
		return contractErrorf("the worksets store at %s was written by a newer velruna (schema %d, this binary "+
			"understands %d): upgrade the binary before using this store", c.endpointLabel(), found, worksetSchemaVersion)
	}
	gate.done = true
	return nil
}

// ensureWorksetHistoryIndex prepares one workset journal index. Journal appends
// cross a period boundary into an index that did not exist when the caller ran
// EnsureWorksetIndexes; without this the first write of the period would
// auto-create the index with dynamic mappings. The process-wide gate makes the
// check once per index per process (the same assumption as the fact journal).
func (c *Client) ensureWorksetHistoryIndex(ctx context.Context, index string) error {
	gate := infraGate("workset-history|" + c.storeKey() + "|" + index)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.done {
		return nil
	}
	if err := c.ensureWorksetJournalInfra(ctx); err != nil {
		return err
	}
	if err := c.ensureIndexWithBody(ctx, index,
		buildWorksetsHistoryIndexBody(c.indexReplicas(ctx), c.worksetsIndex()), "create worksets history index"); err != nil {
		return err
	}
	// Pin the pipeline on an index that already existed (created before the
	// pipeline, or auto-created during a failed infra pass): without it the
	// append carries no ingest_at and sorts last.
	if err := c.ensureWorksetHistoryPipeline(ctx, index); err != nil {
		return err
	}
	gate.done = true
	return nil
}

// ensureWorksetJournalInfra installs the workset journal ingest pipeline that
// stamps ingest_at with the store clock. Writer clocks skew on a shared store,
// so entry order follows the store's clock; `at` stays the writer-clock field
// and the fallback for rows written before the pipeline. A role that cannot
// manage ingest (403) is treated as externally managed: the append proceeds,
// exactly like the fact journal.
func (c *Client) ensureWorksetJournalInfra(ctx context.Context) error {
	gate := infraGate("workset-journal|" + c.storeKey() + "|" + c.worksetsIndex())
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.done {
		return nil
	}
	pipeline := journalPipelineName(c.worksetsIndex())
	if _, err := c.api.Ingest.Get(ctx, &opensearchapi.IngestGetReq{PipelineIDs: []string{pipeline}}); err != nil && !statusIs(err, 403) {
		body, _ := json.Marshal(buildJournalPipelineBody(c.worksetsIndex()))
		if _, mkErr := c.api.Ingest.Create(ctx, opensearchapi.IngestCreateReq{
			PipelineID: pipeline,
			Body:       bytes.NewReader(body),
		}); mkErr != nil && !statusIs(mkErr, 409) { // lost the create race: it exists now
			return wrapWriteError("create workset journal pipeline", mkErr)
		}
	}
	gate.done = true
	return nil
}

// ensureWorksetHistoryPipeline pins the store-clock pipeline as the index
// default. One idempotent settings PUT per index per process; a 403 means the
// role deliberately cannot manage index settings (externally managed infra),
// and a 404 means the index is gone (its re-creation applies the template).
func (c *Client) ensureWorksetHistoryPipeline(ctx context.Context, index string) error {
	gate := infraGate("workset-pipeline|" + c.storeKey() + "|" + index)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.done {
		return nil
	}
	body, _ := json.Marshal(map[string]interface{}{
		"index.default_pipeline": journalPipelineName(c.worksetsIndex()),
	})
	_, err := c.api.Indices.Settings.Put(ctx, opensearchapi.SettingsPutReq{
		Indices: []string{index},
		Body:    bytes.NewReader(body),
	})
	if err != nil && !isNotFound(err) && !statusIs(err, 403) {
		return wrapWriteError("workset journal index pipeline", err)
	}
	gate.done = true
	return nil
}

// ensureIndexWithBody creates one index with a caller-supplied mapping when a
// HEAD says it is missing. An existing index is left alone: this helper never
// rewrites mappings, so it cannot downgrade a store written by a newer binary.
func (c *Client) ensureIndexWithBody(ctx context.Context, index string, body map[string]interface{}, op string) error {
	resp, err := c.api.Indices.Exists(ctx, opensearchapi.IndicesExistsReq{Indices: []string{index}})
	switch {
	case err == nil:
		return nil
	case resp != nil && resp.StatusCode == http.StatusNotFound:
		// Missing: create below. HEAD has no body, so the status code is the
		// only signal the client can give.
	default:
		return c.storeHint(fmt.Errorf("check index %s: %w", index, err))
	}
	data, _ := json.Marshal(body)
	if _, err := c.api.Indices.Create(ctx, opensearchapi.IndicesCreateReq{
		Index: index,
		Body:  bytes.NewReader(data),
	}); err != nil {
		if isAlreadyExists(err) {
			// Lost the create race: the index is there, which is the goal.
			return nil
		}
		if isReadOnlyClusterBlock(err) {
			// A read-only cluster block refuses the create. Reads must keep
			// working (the index is only needed by writes), so this is not an
			// error here. An authorization failure is NOT skipped: it would hide
			// a real permission problem behind a later, confusing write error.
			return nil
		}
		return wrapWriteError(op, err)
	}
	return nil
}

// isReadOnlyClusterBlock tells a deliberate read-only cluster block (the store
// is write-protected on purpose) from any other 403. Only the former may be
// skipped by a read path: swallowing an authorization failure would hide a real
// permission problem and let the first write fail later with a confusing error.
// The classification reads the typed error type, like isAlreadyExists: every
// 403 gate must agree on what it is looking at.
func isReadOnlyClusterBlock(err error) bool {
	var ste *opensearch.StructError
	if errors.As(err, &ste) {
		return ste.Err.Type == "cluster_block_exception" || strings.Contains(ste.Err.Reason, "read-only")
	}
	var se *opensearch.StringError
	if errors.As(err, &se) {
		return strings.Contains(se.Err, "cluster_block_exception") || strings.Contains(se.Err, "read-only")
	}
	return false
}
