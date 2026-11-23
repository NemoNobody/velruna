package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/model"
	"github.com/opensearch-project/opensearch-go/v4"
)

// TestIsReadOnlyClusterBlock pins the 403 classification: only a deliberate
// cluster block may be skipped by a read path, never an authorization failure.
func TestIsReadOnlyClusterBlock(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"cluster block (struct)", &opensearch.StructError{Status: http.StatusForbidden, Err: opensearch.Err{
			Type:   "cluster_block_exception",
			Reason: "index [velruna-worksets] blocked by: [FORBIDDEN/12/index read-only / allow delete (api)]",
		}}, true},
		{"security (struct)", &opensearch.StructError{Status: http.StatusForbidden, Err: opensearch.Err{
			Type:   "security_exception",
			Reason: "no permissions for [indices:admin/create]",
		}}, false},
		{"cluster block (string)", &opensearch.StringError{Status: http.StatusForbidden, Err: "cluster_block_exception: index read-only"}, true},
		{"security (string)", &opensearch.StringError{Status: http.StatusForbidden, Err: "security_exception: no permissions"}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadOnlyClusterBlock(tc.err); got != tc.want {
				t.Errorf("isReadOnlyClusterBlock(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// Workset tests run against the recordedRequest fake transport (cas_test.go /
// contract_test.go): no live OpenSearch. They pin the HTTP contract — which
// endpoints, which query params, which bodies — because that is where the CAS,
// read-after-write and journal-order bugs live.

const worksetTestIndex = "velruna-worksets"

// worksetTestConfig fills the config fields the workset paths read. NewClient
// itself only needs the password/URL the fake sets.
func worksetTestConfig(cfg *config.Config) {
	cfg.WorksetsIndexName = worksetTestIndex
	cfg.HistoryIndexPeriod = "month"
	cfg.WorksetTakeoverSeconds = 3600
	cfg.Author = "tester"
}

// worksetPrelude answers the requests that surround every workset write: the
// author probe (the fake has no security plugin) and the per-process history
// index check (the index "already exists" here). ok=false means "not mine".
func worksetPrelude(r recordedRequest) (status int, body string, ok bool) {
	if r.Method == http.MethodGet && r.Path == "/_security/authinfo" {
		return http.StatusNotFound, `{"status":404,"error":"security plugin disabled"}`, true
	}
	if strings.HasPrefix(r.Path, "/_ingest/pipeline/") {
		if r.Method == http.MethodGet {
			return http.StatusNotFound, `{"status":404,"error":"pipeline not found"}`, true
		}
		return http.StatusOK, `{"acknowledged":true}`, true
	}
	if strings.HasPrefix(r.Path, "/"+worksetTestIndex+"-history-") {
		if r.Method == http.MethodHead {
			return http.StatusOK, "", true
		}
		return http.StatusOK, `{"_index":"history","_id":"wj-1","result":"created"}`, true
	}
	return 0, "", false
}

// worksetSearchResponse renders a search page from whole documents (the _source
// is what readers decode; the top-level _id is what resolve uses).
func worksetSearchResponse(docs ...model.Workset) (int, string) {
	hits := make([]string, 0, len(docs))
	for _, ws := range docs {
		src, _ := json.Marshal(ws)
		hits = append(hits, fmt.Sprintf(`{"_index":%q,"_id":%q,"_source":%s}`, worksetTestIndex, ws.ID, src))
	}
	return http.StatusOK, fmt.Sprintf(
		`{"took":1,"hits":{"total":{"value":%d,"relation":"eq"},"hits":[%s]}}`,
		len(hits), strings.Join(hits, ","))
}

// worksetDocResponse renders a realtime GET with the version pair a CAS write
// must carry.
func worksetDocResponse(ws model.Workset, seqNo, primaryTerm int) (int, string) {
	src, _ := json.Marshal(ws)
	return http.StatusOK, fmt.Sprintf(
		`{"_index":%q,"_id":%q,"_version":2,"_seq_no":%d,"_primary_term":%d,"found":true,"_source":%s}`,
		worksetTestIndex, ws.ID, seqNo, primaryTerm, src)
}

// worksetStandardResponder serves one pre-write snapshot: resolve search → GET
// with seq 5/term 2 → PUT capture. writeStatus/writeBody is the CAS write's
// outcome (200 "updated", or a 409 conflict body).
func worksetStandardResponder(read model.Workset, capture *recordedRequest, writeStatus int, writeBody string) func(recordedRequest) (int, string) {
	return func(r recordedRequest) (int, string) {
		if code, body, ok := worksetPrelude(r); ok {
			return code, body
		}
		switch {
		case r.Method == http.MethodPost && r.Path == "/"+worksetTestIndex+"/_search":
			return worksetSearchResponse(read)
		case r.Method == http.MethodGet && r.Path == "/"+worksetTestIndex+"/_doc/"+read.ID:
			return worksetDocResponse(read, 5, 2)
		case r.Method == http.MethodPut && r.Path == "/"+worksetTestIndex+"/_doc/"+read.ID:
			if capture != nil {
				*capture = r
			}
			return writeStatus, writeBody
		}
		return http.StatusInternalServerError,
			`{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
	}
}

// worksetJournalRequests returns the workset journal appends (POST _doc into a
// period index; auto-id appends use POST).
func worksetJournalRequests(fake *fakeOpenSearch) []recordedRequest {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var out []recordedRequest
	for _, r := range fake.requests {
		if r.Method == http.MethodPost &&
			strings.HasPrefix(r.Path, "/"+worksetTestIndex+"-history-") &&
			strings.Contains(r.Path, "/_doc") {
			out = append(out, r)
		}
	}
	return out
}

// mappingProps / mappingMeta keep the body-shape assertions readable.
func mappingProps(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	mappings, ok := body["mappings"].(map[string]interface{})
	if !ok {
		t.Fatalf("body has no mappings: %#v", body)
	}
	props, ok := mappings["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("mappings has no properties: %#v", mappings)
	}
	return props
}

func mappingMeta(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	mappings, ok := body["mappings"].(map[string]interface{})
	if !ok {
		t.Fatalf("body has no mappings: %#v", body)
	}
	meta, ok := mappings["_meta"].(map[string]interface{})
	if !ok {
		t.Fatalf("mappings has no _meta: %#v", mappings)
	}
	return meta
}

func fieldType(t *testing.T, props map[string]interface{}, name string) string {
	t.Helper()
	raw, ok := props[name]
	if !ok {
		t.Fatalf("mapping is missing field %q", name)
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("field %q is not an object: %#v", name, raw)
	}
	if t, ok := m["type"].(string); ok {
		return t
	}
	return ""
}

// TestWorksetIndexBodies pins both catalogs: every required field plus the two
// schema markers. The mappings are what make name/alias resolution and project
// filtering work at all (a dynamic mapping would index name as text and the
// exact term lookup would miss).
func TestWorksetIndexBodies(t *testing.T) {
	wsBody := buildWorksetsIndexBody(0)
	if got := mappingMeta(t, wsBody)["velruna_worksets_schema"]; got != 1 {
		t.Errorf("worksets _meta[velruna_worksets_schema] = %v, want 1", got)
	}
	props := mappingProps(t, wsBody)
	wantTypes := map[string]string{
		"id":         "keyword",
		"name":       "keyword",
		"aliases":    "keyword",
		"project":    "keyword",
		"created_at": "date",
		"updated_at": "date",
		"created_by": "keyword",
		"updated_by": "keyword",
		"version":    "long",
		"entries":    "object",
		"cursors":    "object",
	}
	for name, want := range wantTypes {
		if got := fieldType(t, props, name); got != want {
			t.Errorf("worksets property %q type = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"entries", "cursors"} {
		m := props[name].(map[string]interface{})
		if enabled, ok := m["enabled"]; !ok || enabled != false {
			t.Errorf("worksets property %q must be enabled:false (stored-only), got %#v", name, m)
		}
	}

	histBody := buildWorksetsHistoryIndexBody(0, worksetTestIndex)
	if got := mappingMeta(t, histBody)["velruna_worksets_history_schema"]; got != 1 {
		t.Errorf("history _meta[velruna_worksets_history_schema] = %v, want 1", got)
	}
	settings, ok := histBody["settings"].(map[string]interface{})
	if !ok {
		t.Fatalf("history body has no settings: %#v", histBody)
	}
	if got := settings["index.default_pipeline"]; got != "velruna-worksets-journal-ts" {
		t.Errorf("history default_pipeline = %v, want velruna-worksets-journal-ts", got)
	}
	hprops := mappingProps(t, histBody)
	histTypes := map[string]string{
		"workset_id": "keyword",
		"workset":    "keyword",
		"op":         "keyword",
		"takeover":   "keyword",
		"fact_id":    "keyword",
		"from":       "keyword",
		"to":         "keyword",
		"by":         "keyword",
		"at":         "date",
		"ingest_at":  "date",
		"note":       "text",
	}
	for name, want := range histTypes {
		if got := fieldType(t, hprops, name); got != want {
			t.Errorf("history property %q type = %q, want %q", name, got, want)
		}
	}
}

// TestWorksetResolveSearchAndListFilter pins the two read bodies: resolution is
// a bool-should over name OR aliases (a normalized term, not a text match), and
// a project listing filters with a term.
func TestWorksetResolveSearchAndListFilter(t *testing.T) {
	t.Run("resolve name or alias", func(t *testing.T) {
		doc := model.Workset{ID: "ws-1", Name: "p2", Project: "p"}
		c, fake := newFakeClientCfg(t, worksetTestConfig, func(r recordedRequest) (int, string) {
			if code, body, ok := worksetPrelude(r); ok {
				return code, body
			}
			switch {
			case r.Method == http.MethodPost && r.Path == "/"+worksetTestIndex+"/_search":
				return worksetSearchResponse(model.Workset{ID: "ws-1"})
			case r.Method == http.MethodGet && r.Path == "/"+worksetTestIndex+"/_doc/ws-1":
				return worksetDocResponse(doc, 1, 1)
			}
			return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
		})

		got, err := c.GetWorkset(context.Background(), " P2 ")
		if err != nil {
			t.Fatalf("GetWorkset: %v", err)
		}
		if got.Name != "p2" {
			t.Errorf("name = %q, want p2", got.Name)
		}
		searches := fake.requestsFor(http.MethodPost, "/"+worksetTestIndex+"/_search")
		if len(searches) != 1 {
			t.Fatalf("search requests = %d, want 1", len(searches))
		}
		body := searches[0].Body
		for _, want := range []string{
			`"should"`,
			`"name":"p2"`,
			`"aliases":"p2"`,
			`"minimum_should_match":1`,
			`"sort"`,
			`"created_at"`,
			`"id"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("resolve body missing %s: %s", want, body)
			}
		}
	})

	t.Run("list project filter", func(t *testing.T) {
		c, fake := newFakeClientCfg(t, worksetTestConfig, func(r recordedRequest) (int, string) {
			if code, body, ok := worksetPrelude(r); ok {
				return code, body
			}
			if r.Method == http.MethodPost && r.Path == "/"+worksetTestIndex+"/_search" {
				return worksetSearchResponse()
			}
			return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
		})

		list, err := c.ListWorksets(context.Background(), "proj1")
		if err != nil {
			t.Fatalf("ListWorksets: %v", err)
		}
		if len(list) != 0 {
			t.Errorf("worksets = %d, want 0", len(list))
		}
		searches := fake.requestsFor(http.MethodPost, "/"+worksetTestIndex+"/_search")
		if len(searches) != 1 {
			t.Fatalf("search requests = %d, want 1", len(searches))
		}
		body := searches[0].Body
		if !strings.Contains(body, `"term":{"project":"proj1"}`) {
			t.Errorf("list body missing project term: %s", body)
		}
		if !strings.Contains(body, `"size":200`) {
			t.Errorf("list body missing size cap 200: %s", body)
		}
	})
}

// TestWorksetCASParams pins that a mutation carries the version pair from the
// read: without it a concurrent writer's change would be silently clobbered.
func TestWorksetCASParams(t *testing.T) {
	doc := model.Workset{ID: "ws-1", Name: "p2", Project: "p", Version: 1}
	var put recordedRequest
	c, _ := newFakeClientCfg(t, worksetTestConfig, worksetStandardResponder(doc, &put,
		http.StatusOK, `{"_index":"velruna-worksets","_id":"ws-1","result":"updated"}`))

	if _, err := c.SetCursor(context.Background(), "p2", CursorInput{Author: "reader", Host: "box", At: 3}); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}
	if got := put.Query.Get("if_seq_no"); got != "5" {
		t.Errorf("if_seq_no = %q, want 5 (the version pair from the snapshot read)", got)
	}
	if got := put.Query.Get("if_primary_term"); got != "2" {
		t.Errorf("if_primary_term = %q, want 2", got)
	}
	if got := put.Query.Get("refresh"); got != "wait_for" {
		t.Errorf("refresh = %q, want wait_for", got)
	}
}

// TestWorksetConflictMaps: a 409 on the conditional write is a stale snapshot,
// not a generic store failure — callers retry via errors.Is.
func TestWorksetConflictMaps(t *testing.T) {
	doc := model.Workset{ID: "ws-1", Name: "p2", Project: "p", Version: 1}
	c, fake := newFakeClientCfg(t, worksetTestConfig, worksetStandardResponder(doc, nil,
		http.StatusConflict, casConflictBody))

	_, err := c.SetCursor(context.Background(), "p2", CursorInput{Author: "reader", Host: "box", At: 3})
	if !errors.Is(err, ErrWorksetConflict) {
		t.Fatalf("err = %v, want ErrWorksetConflict", err)
	}
	if len(worksetJournalRequests(fake)) != 0 {
		t.Error("a failed CAS wrote journal rows; nothing was applied")
	}
}

// TestWorksetAddIdempotent pins the add contract: an existing entry keeps its
// phase and owner, a new entry gets the default phase and the writer as its
// author, and exactly one journal row is written (per added fact).
func TestWorksetAddIdempotent(t *testing.T) {
	old := time.Now().UTC().Add(-time.Hour)
	doc := model.Workset{
		ID: "ws-1", Name: "p2", Project: "p", Version: 1,
		Entries: []model.Entry{{
			FactID: "f1", Phase: model.PhaseInProgress, PhaseBy: "alice",
			PhaseAt: old, AddedAt: old, AddedBy: "alice",
		}},
	}
	var put recordedRequest
	c, fake := newFakeClientCfg(t, worksetTestConfig, worksetStandardResponder(doc, &put,
		http.StatusOK, `{"_index":"velruna-worksets","_id":"ws-1","result":"updated"}`))

	res, err := c.AddEntries(context.Background(), "p2", WorksetAdd{
		FactIDs: []string{"f1", "f2", "f2"},
		Note:    "queued",
	})
	if err != nil {
		t.Fatalf("AddEntries: %v", err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (f1 already present, f2 deduped)", len(res.Entries))
	}
	first := res.Entries[0]
	if first.FactID != "f1" || first.Phase != model.PhaseInProgress || first.PhaseBy != "alice" {
		t.Errorf("existing entry was overwritten: %#v", first)
	}
	added := res.Entries[1]
	if added.FactID != "f2" {
		t.Fatalf("added fact = %q, want f2", added.FactID)
	}
	if added.Phase != model.PhaseTodo {
		t.Errorf("default phase = %q, want todo", added.Phase)
	}
	if added.PhaseBy != "tester" || added.AddedBy != "tester" {
		t.Errorf("entry author = %q/%q, want tester/tester", added.PhaseBy, added.AddedBy)
	}
	if added.Note != "queued" {
		t.Errorf("note = %q, want queued", added.Note)
	}
	if added.AddedAt.IsZero() || added.PhaseAt.IsZero() {
		t.Error("entry timestamps are zero")
	}
	if time.Since(added.AddedAt) > time.Minute {
		t.Errorf("added_at = %s, not the write moment", added.AddedAt)
	}
	if got := put.Query.Get("if_seq_no"); got != "5" {
		t.Errorf("if_seq_no = %q, want 5", got)
	}

	journals := worksetJournalRequests(fake)
	if len(journals) != 1 {
		t.Fatalf("journal appends = %d, want 1 (one per added fact)", len(journals))
	}
	body := journals[0].Body
	for _, want := range []string{`"op":"add"`, `"fact_id":"f2"`, `"to":"todo"`, `"by":"tester"`} {
		if !strings.Contains(body, want) {
			t.Errorf("journal row missing %s: %s", want, body)
		}
	}
}

// TestWorksetSetPhases covers the ownership matrix end to end: a lock is
// diagnosed (not a generic refusal), release to todo is always allowed, an
// explicit takeover and a stale claim are both admitted, and a bad id in the
// batch leaves the document untouched.
func TestWorksetSetPhases(t *testing.T) {
	freshClaim := func() model.Workset {
		now := time.Now().UTC()
		return model.Workset{
			ID: "ws-1", Name: "p2", Project: "p", Version: 1,
			Entries: []model.Entry{{
				FactID: "f1", Phase: model.PhaseInProgress, PhaseBy: "alice",
				PhaseAt: now.Add(-time.Minute), AddedAt: now.Add(-time.Hour), AddedBy: "alice",
			}},
		}
	}
	staleClaim := func() model.Workset {
		ws := freshClaim()
		ws.Entries[0].PhaseAt = time.Now().UTC().Add(-2 * time.Hour)
		return ws
	}
	updatedBody := `{"_index":"velruna-worksets","_id":"ws-1","result":"updated"}`

	t.Run("lock refusal", func(t *testing.T) {
		c, fake := newFakeClientCfg(t, worksetTestConfig,
			worksetStandardResponder(freshClaim(), nil, http.StatusOK, updatedBody))
		_, err := c.SetPhases(context.Background(), "p2", WorksetPhaseChange{
			FactIDs: []string{"f1"}, To: model.PhaseInProgress, By: "bob",
		})
		if !errors.Is(err, ErrWorksetLocked) {
			t.Fatalf("err = %v, want ErrWorksetLocked", err)
		}
		for _, want := range []string{"phase_by=", "phase_at=", "use --takeover or move to todo first"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("lock error missing %q: %v", want, err)
			}
		}
		if puts := fake.requestsFor(http.MethodPut, "/"+worksetTestIndex+"/_doc/ws-1"); len(puts) != 0 {
			t.Errorf("a refused transition wrote the document (%d PUTs)", len(puts))
		}
	})

	t.Run("release to todo is allowed", func(t *testing.T) {
		c, fake := newFakeClientCfg(t, worksetTestConfig,
			worksetStandardResponder(freshClaim(), nil, http.StatusOK, updatedBody))
		res, err := c.SetPhases(context.Background(), "p2", WorksetPhaseChange{
			FactIDs: []string{"f1"}, To: model.PhaseTodo, By: "bob",
		})
		if err != nil {
			t.Fatalf("SetPhases to todo: %v", err)
		}
		if res.Entries[0].Phase != model.PhaseTodo || res.Entries[0].PhaseBy != "bob" {
			t.Errorf("entry = %#v, want todo owned by bob", res.Entries[0])
		}
		journals := worksetJournalRequests(fake)
		if len(journals) != 1 || !strings.Contains(journals[0].Body, `"op":"phase"`) {
			t.Errorf("journal = %v, want one op=phase row", journals)
		}
	})

	t.Run("explicit takeover is allowed and journaled as takeover", func(t *testing.T) {
		c, fake := newFakeClientCfg(t, worksetTestConfig,
			worksetStandardResponder(freshClaim(), nil, http.StatusOK, updatedBody))
		res, err := c.SetPhases(context.Background(), "p2", WorksetPhaseChange{
			FactIDs: []string{"f1"}, To: model.PhaseInProgress, By: "bob", Takeover: true,
		})
		if err != nil {
			t.Fatalf("SetPhases with takeover: %v", err)
		}
		if res.Entries[0].PhaseBy != "bob" {
			t.Errorf("phase_by = %q, want bob", res.Entries[0].PhaseBy)
		}
		journals := worksetJournalRequests(fake)
		if len(journals) != 1 || !strings.Contains(journals[0].Body, `"op":"takeover"`) {
			t.Errorf("journal = %v, want one op=takeover row", journals)
		}
		if len(journals) == 1 && !strings.Contains(journals[0].Body, `"takeover":"explicit"`) {
			t.Errorf("journal row missing takeover=explicit: %s", journals[0].Body)
		}
	})

	t.Run("stale claim is allowed without a takeover", func(t *testing.T) {
		c, fake := newFakeClientCfg(t, worksetTestConfig,
			worksetStandardResponder(staleClaim(), nil, http.StatusOK, updatedBody))
		if _, err := c.SetPhases(context.Background(), "p2", WorksetPhaseChange{
			FactIDs: []string{"f1"}, To: model.PhaseInProgress, By: "bob",
		}); err != nil {
			t.Fatalf("SetPhases over a stale claim: %v", err)
		}
		journals := worksetJournalRequests(fake)
		if len(journals) != 1 || !strings.Contains(journals[0].Body, `"op":"phase"`) {
			t.Errorf("journal = %v, want one op=phase row", journals)
		}
		if len(journals) == 1 && !strings.Contains(journals[0].Body, `"takeover":"stale"`) {
			t.Errorf("stale takeover row missing takeover=stale: %s", journals[0].Body)
		}
	})

	t.Run("unknown fact id writes nothing", func(t *testing.T) {
		c, fake := newFakeClientCfg(t, worksetTestConfig,
			worksetStandardResponder(freshClaim(), nil, http.StatusOK, updatedBody))
		_, err := c.SetPhases(context.Background(), "p2", WorksetPhaseChange{
			FactIDs: []string{"f1", "nope"}, To: model.PhaseTodo, By: "bob",
		})
		if err == nil {
			t.Fatal("SetPhases with an unknown fact id succeeded")
		}
		if !strings.Contains(err.Error(), `"nope"`) {
			t.Errorf("error does not name the unknown id: %v", err)
		}
		if puts := fake.requestsFor(http.MethodPut, "/"+worksetTestIndex+"/_doc/ws-1"); len(puts) != 0 {
			t.Errorf("atomicity broken: %d PUTs on a failed batch", len(puts))
		}
		if len(worksetJournalRequests(fake)) != 0 {
			t.Error("atomicity broken: journal rows on a failed batch")
		}
	})
}

// TestWorksetRename covers alias bookkeeping and collision refusal. Renaming to
// a name the document already answers to (an alias of itself) is allowed and
// drops the duplicate alias; a name held by another document is refused.
func TestWorksetRename(t *testing.T) {
	t.Run("old name becomes an alias, target alias dropped", func(t *testing.T) {
		doc := model.Workset{
			ID: "ws-1", Name: "old", Aliases: []string{"p2", "new"}, Project: "p", Version: 1,
		}
		var put recordedRequest
		c, fake := newFakeClientCfg(t, worksetTestConfig, func(r recordedRequest) (int, string) {
			if code, body, ok := worksetPrelude(r); ok {
				return code, body
			}
			switch {
			case r.Method == http.MethodPost && r.Path == "/"+worksetTestIndex+"/_search":
				// Both lookups (the current name and the target, which is an
				// alias of this same document) resolve to ws-1.
				return worksetSearchResponse(model.Workset{ID: "ws-1"})
			case r.Method == http.MethodGet && r.Path == "/"+worksetTestIndex+"/_doc/ws-1":
				return worksetDocResponse(doc, 5, 2)
			case r.Method == http.MethodPut && r.Path == "/"+worksetTestIndex+"/_doc/ws-1":
				put = r
				return http.StatusOK, `{"_index":"velruna-worksets","_id":"ws-1","result":"updated"}`
			}
			return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
		})

		res, err := c.RenameWorkset(context.Background(), "old", "new", "tester")
		if err != nil {
			t.Fatalf("RenameWorkset: %v", err)
		}
		if res.Name != "new" {
			t.Errorf("name = %q, want new", res.Name)
		}
		wantAliases := []string{"p2", "old"}
		if strings.Join(res.Aliases, ",") != strings.Join(wantAliases, ",") {
			t.Errorf("aliases = %v, want %v (drop the new name, append the old)", res.Aliases, wantAliases)
		}
		if res.Version != 2 {
			t.Errorf("version = %d, want 2", res.Version)
		}
		if got := put.Query.Get("if_seq_no"); got != "5" {
			t.Errorf("if_seq_no = %q, want 5", got)
		}
		journals := worksetJournalRequests(fake)
		if len(journals) != 1 || !strings.Contains(journals[0].Body, `"op":"rename"`) {
			t.Errorf("journal = %v, want one op=rename row", journals)
		}
	})

	t.Run("collision with another workset is refused", func(t *testing.T) {
		self := model.Workset{ID: "ws-1", Name: "old", Project: "p", Version: 1}
		other := model.Workset{ID: "ws-2", Name: "new", Project: "p", Version: 1}
		c, fake := newFakeClientCfg(t, worksetTestConfig, func(r recordedRequest) (int, string) {
			if code, body, ok := worksetPrelude(r); ok {
				return code, body
			}
			switch {
			case r.Method == http.MethodPost && r.Path == "/"+worksetTestIndex+"/_search":
				if strings.Contains(r.Body, `"name":"new"`) {
					return worksetSearchResponse(other)
				}
				return worksetSearchResponse(self)
			case r.Method == http.MethodGet && r.Path == "/"+worksetTestIndex+"/_doc/ws-1":
				return worksetDocResponse(self, 5, 2)
			case r.Method == http.MethodGet && r.Path == "/"+worksetTestIndex+"/_doc/ws-2":
				return worksetDocResponse(other, 1, 1)
			case r.Method == http.MethodPut:
				return http.StatusOK, `{"result":"updated"}`
			}
			return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
		})

		_, err := c.RenameWorkset(context.Background(), "old", "new", "tester")
		if !errors.Is(err, ErrWorksetExists) {
			t.Fatalf("err = %v, want ErrWorksetExists", err)
		}
		if puts := fake.requestsFor(http.MethodPut, "/"+worksetTestIndex+"/_doc/ws-1"); len(puts) != 0 {
			t.Errorf("a refused rename wrote the document (%d PUTs)", len(puts))
		}
	})
}

// TestWorksetSetCursor pins the cursor key shape, the MaxCursors eviction and
// the "cursors are not journaled" rule.
func TestWorksetSetCursor(t *testing.T) {
	base := time.Now().UTC()
	cursors := make(map[string]model.Cursor, model.MaxCursors)
	for i := 0; i < model.MaxCursors; i++ {
		cursors[fmt.Sprintf("c%02d", i)] = model.Cursor{
			At:        i,
			UpdatedAt: base.Add(-time.Duration(i) * time.Minute),
		}
	}
	doc := model.Workset{ID: "ws-1", Name: "p2", Project: "p", Version: 1, Cursors: cursors}
	var put recordedRequest
	c, fake := newFakeClientCfg(t, worksetTestConfig, worksetStandardResponder(doc, &put,
		http.StatusOK, `{"_index":"velruna-worksets","_id":"ws-1","result":"updated"}`))

	res, err := c.SetCursor(context.Background(), "p2", CursorInput{Author: "reader", Host: "box", At: 7})
	if err != nil {
		t.Fatalf("SetCursor: %v", err)
	}
	cur, ok := res.Cursors["reader@box"]
	if !ok {
		t.Fatalf("cursor key reader@box missing; got %v", res.Cursors)
	}
	if cur.At != 7 {
		t.Errorf("cursor at = %d, want 7", cur.At)
	}
	if len(res.Cursors) != model.MaxCursors {
		t.Errorf("cursors = %d, want %d (evicted to the cap)", len(res.Cursors), model.MaxCursors)
	}
	if _, gone := res.Cursors["c19"]; gone {
		t.Error("oldest cursor c19 was not evicted")
	}
	if _, kept := res.Cursors["c00"]; !kept {
		t.Error("newest seeded cursor c00 was evicted")
	}
	if got := put.Query.Get("if_seq_no"); got != "5" {
		t.Errorf("if_seq_no = %q, want 5", got)
	}
	if len(worksetJournalRequests(fake)) != 0 {
		t.Error("SetCursor wrote a journal row; cursor moves are not journaled")
	}
}

// TestWorksetCASRequiresVersionPair: a zero version pair must never degrade
// into an unconditional write. The fake returns a snapshot without seq/term
// (a broken read), and the mutation must refuse before any PUT is sent.
func TestWorksetCASRequiresVersionPair(t *testing.T) {
	doc := model.Workset{ID: "ws-1", Name: "p2", Project: "p", Version: 1}
	var puts int
	c, _ := newFakeClientCfg(t, worksetTestConfig, func(r recordedRequest) (int, string) {
		if code, body, ok := worksetPrelude(r); ok {
			return code, body
		}
		switch {
		case r.Method == http.MethodPost && r.Path == "/"+worksetTestIndex+"/_search":
			return worksetSearchResponse(model.Workset{ID: "ws-1"})
		case r.Method == http.MethodGet && r.Path == "/"+worksetTestIndex+"/_doc/ws-1":
			return worksetDocResponse(doc, 0, 0)
		case r.Method == http.MethodPut && r.Path == "/"+worksetTestIndex+"/_doc/ws-1":
			puts++
			return http.StatusOK, `{"result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
	})

	_, err := c.SetCursor(context.Background(), "p2", CursorInput{Author: "reader", Host: "box", At: 3})
	if err == nil {
		t.Fatal("SetCursor with a zero version pair succeeded")
	}
	if !strings.Contains(err.Error(), "missing version pair") {
		t.Errorf("error does not name the missing pair: %v", err)
	}
	if puts != 0 {
		t.Errorf("wrote the document %d time(s) without a version pair", puts)
	}
}

// TestWorksetSetCursorRetriesCAS: cursor moves are idempotent, so a conflict
// from a concurrent phase write is retried (up to three attempts) instead of
// surfacing as noise. A burst that outlasts the retries still reports the
// conflict honestly.
func TestWorksetSetCursorRetriesCAS(t *testing.T) {
	doc := model.Workset{ID: "ws-1", Name: "p2", Project: "p", Version: 1}
	responder := func(conflicts int) (func(recordedRequest) (int, string), *int) {
		puts := 0
		return func(r recordedRequest) (int, string) {
			if code, body, ok := worksetPrelude(r); ok {
				return code, body
			}
			switch {
			case r.Method == http.MethodPost && r.Path == "/"+worksetTestIndex+"/_search":
				return worksetSearchResponse(model.Workset{ID: "ws-1"})
			case r.Method == http.MethodGet && r.Path == "/"+worksetTestIndex+"/_doc/ws-1":
				return worksetDocResponse(doc, 5, 2)
			case r.Method == http.MethodPut && r.Path == "/"+worksetTestIndex+"/_doc/ws-1":
				puts++
				if puts <= conflicts {
					return http.StatusConflict, casConflictBody
				}
				return http.StatusOK, `{"result":"updated"}`
			}
			return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
		}, &puts
	}

	t.Run("retries and succeeds", func(t *testing.T) {
		respond, puts := responder(2)
		c, _ := newFakeClientCfg(t, worksetTestConfig, respond)
		if _, err := c.SetCursor(context.Background(), "p2", CursorInput{Author: "reader", Host: "box", At: 3}); err != nil {
			t.Fatalf("SetCursor after conflicts: %v", err)
		}
		if *puts != 3 {
			t.Errorf("PUT attempts = %d, want 3 (two conflicts, then success)", *puts)
		}
	})

	t.Run("gives up after three retries", func(t *testing.T) {
		respond, puts := responder(99)
		c, _ := newFakeClientCfg(t, worksetTestConfig, respond)
		_, err := c.SetCursor(context.Background(), "p2", CursorInput{Author: "reader", Host: "box", At: 3})
		if !errors.Is(err, ErrWorksetConflict) {
			t.Fatalf("err = %v, want ErrWorksetConflict", err)
		}
		if *puts != 4 {
			t.Errorf("PUT attempts = %d, want 4 (initial write plus three retries)", *puts)
		}
	})
}

// TestWorksetSchemaVersion pins the schema-marker policy: a newer marker is
// refused before any write, while an older or absent marker is adoptable and
// an unreadable mapping is not a verdict.
func TestWorksetSchemaVersion(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "newer is refused", status: http.StatusOK,
			body: `{"velruna-worksets":{"mappings":{"_meta":{"velruna_worksets_schema":2}}}}`, wantErr: true},
		{name: "current is adopted", status: http.StatusOK,
			body: `{"velruna-worksets":{"mappings":{"_meta":{"velruna_worksets_schema":1}}}}`},
		{name: "older is adopted", status: http.StatusOK,
			body: `{"velruna-worksets":{"mappings":{"_meta":{"velruna_worksets_schema":0}}}}`},
		{name: "absent is adopted", status: http.StatusOK,
			body: `{"velruna-worksets":{"mappings":{"properties":{"name":{"type":"keyword"}}}}}`},
		{name: "index absent is adoptable", status: http.StatusNotFound,
			body: `{"status":404}`},
		{name: "unreadable is not a verdict", status: http.StatusForbidden,
			body: `{"status":403}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newFakeClientCfg(t, worksetTestConfig, func(r recordedRequest) (int, string) {
				if r.Method == http.MethodGet && r.Path == "/"+worksetTestIndex+"/_mapping" {
					return tc.status, tc.body
				}
				return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
			})
			err := c.ensureWorksetSchemaVersion(context.Background())
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "newer velruna") {
					t.Fatalf("err = %v, want a newer-schema refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ensureWorksetSchemaVersion: %v", err)
			}
		})
	}
}

// TestWorksetSetCursorRequiresAuthor: an empty author names no reader, so no
// anonymous "@host" cursor is ever stored (and no request is sent).
func TestWorksetSetCursorRequiresAuthor(t *testing.T) {
	c, _ := newFakeClientCfg(t, worksetTestConfig, func(r recordedRequest) (int, string) {
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
	})
	_, err := c.SetCursor(context.Background(), "p2", CursorInput{Host: "box", At: 3})
	if err == nil || !strings.Contains(err.Error(), "author is empty") {
		t.Fatalf("err = %v, want an empty-author refusal", err)
	}
}

// TestWorksetJournalSortsByIngestClock pins finding 14: the workset journal
// read orders by the store-stamped ingest_at first (writer clocks skew), with
// the writer-clock `at` as the fallback for rows written before the pipeline.
func TestWorksetJournalSortsByIngestClock(t *testing.T) {
	doc := model.Workset{ID: "ws-1", Name: "p2", Project: "p"}
	c, fake := newFakeClientCfg(t, worksetTestConfig, func(r recordedRequest) (int, string) {
		if code, body, ok := worksetPrelude(r); ok {
			return code, body
		}
		switch {
		case r.Method == http.MethodPost && r.Path == "/"+worksetTestIndex+"/_search":
			return worksetSearchResponse(model.Workset{ID: "ws-1"})
		case r.Method == http.MethodGet && r.Path == "/"+worksetTestIndex+"/_doc/ws-1":
			return worksetDocResponse(doc, 1, 1)
		case r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/"+worksetTestIndex+"-history-") && strings.HasSuffix(r.Path, "/_search"):
			return http.StatusOK, `{"took":1,"hits":{"total":{"value":0,"relation":"eq"},"hits":[]}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
	})

	if _, err := c.WorksetJournal(context.Background(), "p2", 10); err != nil {
		t.Fatalf("WorksetJournal: %v", err)
	}
	var searched recordedRequest
	fake.mu.Lock()
	for _, r := range fake.requests {
		if r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/"+worksetTestIndex+"-history-") && strings.HasSuffix(r.Path, "/_search") {
			searched = r
		}
	}
	fake.mu.Unlock()
	if searched.Path == "" {
		t.Fatal("no workset journal search was sent")
	}
	for _, want := range []string{`"ingest_at"`, `"order":"desc"`, `"missing":"_last"`, `"sort"`} {
		if !strings.Contains(searched.Body, want) {
			t.Errorf("journal read body missing %s: %s", want, searched.Body)
		}
	}
	if strings.Index(searched.Body, `"ingest_at"`) > strings.Index(searched.Body, `"at"`) {
		t.Errorf("ingest_at is not the primary sort key: %s", searched.Body)
	}
}

// TestFactStates pins the batch mapping: verified/obsolete come from the hit,
// and an id with no hit is missing (one terms query, not N gets).
func TestFactStates(t *testing.T) {
	c, fake := newFakeClientCfg(t, worksetTestConfig, func(r recordedRequest) (int, string) {
		if code, body, ok := worksetPrelude(r); ok {
			return code, body
		}
		if r.Method == http.MethodPost && r.Path == "/velruna/_search" {
			return http.StatusOK, `{"took":1,"hits":{"total":{"value":2,"relation":"eq"},"hits":[` +
				`{"_index":"velruna","_id":"a","_source":{"fact_id":"a","status":"verified"}},` +
				`{"_index":"velruna","_id":"b","_source":{"fact_id":"b","status":"obsolete"}}` +
				`]}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
	})

	states, err := c.FactStates(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("FactStates: %v", err)
	}
	if states["a"] != FactStateVerified {
		t.Errorf("a = %q, want verified", states["a"])
	}
	if states["b"] != FactStateObsolete {
		t.Errorf("b = %q, want obsolete", states["b"])
	}
	if states["c"] != FactStateMissing {
		t.Errorf("c = %q, want missing", states["c"])
	}
	if len(states) != 3 {
		t.Errorf("states = %d, want 3", len(states))
	}

	searches := fake.requestsFor(http.MethodPost, "/velruna/_search")
	if len(searches) != 1 {
		t.Fatalf("fact searches = %d, want 1", len(searches))
	}
	body := searches[0].Body
	for _, want := range []string{
		`"terms":{"fact_id":["a","b","c"]}`,
		`"size":3`,
		`"_source":["fact_id","status"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fact-states body missing %s: %s", want, body)
		}
	}
}
