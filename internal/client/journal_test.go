package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/model"
)

// journalInfraOK answers the infra probes as "pipeline and template already
// exist" so a test focuses on the journal call under test, not the setup.
func journalInfraOK(r recordedRequest) (int, string) {
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_ingest/pipeline/"):
		return http.StatusOK, `{"velruna-journal-ts":{"processors":[]}}`
	case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_index_template/"):
		return http.StatusOK, `{"index_templates":[{"name":"velruna-journal","index_template":{"template":{"settings":{"index.number_of_replicas":"0"}}}}]}`
	case r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/_settings"):
		return http.StatusOK, `{"acknowledged":true}`
	}
	return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Method + " " + r.Path + `"}`
}

// journalRoutesOK answers every journal route (infra probes, append, search)
// with neutral results — infra exists, append creates a fixed entry, search
// is empty — so main-index contract tests can pin main-doc behavior without
// spelling out the journal plumbing. A 500 sentinel means "not a journal
// route": the caller's own respond takes over.
func journalRoutesOK(r recordedRequest) (int, string) {
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/velruna-history-") && strings.Contains(r.Path, "/_doc"):
		// auto-id appends (POST /_doc, no DocumentID)
		return http.StatusOK, `{"_id":"journal-entry-1","result":"created"}`
	case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
		// preallocated-id appends (migration, birth): Index sends PUT
		// /<index>/_doc/<id> whenever a DocumentID is set (op_type=create
		// is only the query param)
		return http.StatusOK, `{"_id":"journal-entry-1","result":"created"}`
	case r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/velruna-history-") && strings.HasSuffix(r.Path, "/_search"):
		return http.StatusOK, `{"took":1,"hits":{"total":{"value":0,"relation":"eq"},"hits":[]}}`
	}
	return journalInfraOK(r)
}

// journalAppends returns writes to ANY journal dated index: POST /_doc
// (auto id) and PUT /_doc/<id> (deterministic id — migration and the
// doc-first write paths). Pinning one month name flakes when a test run
// crosses the month boundary: the write lands in the next period's index.
func journalAppends(fake *fakeOpenSearch) []recordedRequest {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var out []recordedRequest
	for _, r := range fake.requests {
		if (r.Method == http.MethodPost || r.Method == http.MethodPut) &&
			strings.HasPrefix(r.Path, "/velruna-history-") &&
			strings.Contains(r.Path, "/_doc") {
			out = append(out, r)
		}
	}
	return out
}

func TestJournalIndexNamePeriods(t *testing.T) {
	day := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	isoWeekFlip := time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC) // ISO 2026-W53
	tests := []struct {
		period string
		at     time.Time
		want   string
	}{
		{"day", day, "velruna-history-2026.11.09"},
		{"week", day, "velruna-history-2026-w46"},
		{"week", isoWeekFlip, "velruna-history-2026-w53"},
		{"month", day, "velruna-history-2026.11"},
		{"quarter", day, "velruna-history-2026-q4"},
		{"quarter", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "velruna-history-2026-q1"},
		// quarter/year must partition on UTC, not the writer's zone: 00:30
		// at UTC+3 on Oct 1 is Sep 30 21:30 UTC — the previous quarter.
		{"quarter", time.Date(2026, 10, 1, 0, 30, 0, 0, time.FixedZone("UTC+3", 3*3600)), "velruna-history-2026-q3"},
		{"year", day, "velruna-history-2026"},
		{"bogus", day, "velruna-history-2026.11"}, // config validates; raw calls default to month
	}
	for _, tc := range tests {
		if got := journalIndexName("velruna", tc.period, tc.at); got != tc.want {
			t.Errorf("period %s at %s: got %s, want %s", tc.period, tc.at, got, tc.want)
		}
	}
}

func TestAppendJournalWritesToDatedIndexWithCreate(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		if r.Method == http.MethodPost && r.Path == "/velruna-history-2026.11/_doc" {
			return http.StatusOK, `{"_id":"entry-1","result":"created"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})

	ts := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	e := model.NewJournalEntry("k", "id-1", "proj", "config", model.OpWrite, "v", "notes", "tool:read_file", ts)
	id, err := c.AppendJournal(context.Background(), e)
	if err != nil {
		t.Fatalf("AppendJournal: %v", err)
	}
	if id != "entry-1" {
		t.Errorf("id = %q, want entry-1", id)
	}
	writes := fake.requestsFor(http.MethodPost, "/velruna-history-2026.11/_doc")
	if len(writes) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(writes))
	}
	q := writes[0].Query
	if q.Get("op_type") != "create" {
		t.Errorf("op_type = %q, want create (appends must never overwrite)", q.Get("op_type"))
	}
	if q.Get("refresh") != "wait_for" {
		t.Errorf("refresh = %q, want wait_for (reads-after-write must see the entry)", q.Get("refresh"))
	}
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(writes[0].Body), &body); err != nil {
		t.Fatalf("journal body: %v", err)
	}
	if body["fact_key"] != "k" || body["op"] != model.OpWrite || body["value"] != "v" {
		t.Errorf("journal body = %v", body)
	}
}

func TestJournalReadsCarryUnavailableFlags(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		if r.Method == http.MethodPost && r.Path == "/velruna-history-*/_search" {
			return http.StatusOK, `{"hits":{"total":{"value":1},"hits":[
				{"_index":"velruna-history-2026.11","_id":"e1","_source":{
					"fact_key":"k","op":"write","value":"v","created_at":"2026-11-09T10:00:00Z"}}]}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})

	entries, meta, err := c.JournalEntries(context.Background(), JournalQuery{Key: "k", Limit: 5})
	if err != nil {
		t.Fatalf("JournalEntries: %v", err)
	}
	if meta.Total != 1 || meta.Truncated || len(entries) != 1 || entries[0].Value != "v" {
		t.Fatalf("entries=%v total=%d truncated=%v", entries, meta.Total, meta.Truncated)
	}
	searches := fake.requestsFor(http.MethodPost, "/velruna-history-*/_search")
	if len(searches) != 1 {
		t.Fatalf("searches = %d, want 1", len(searches))
	}
	if searches[0].Query.Get("ignore_unavailable") != "true" ||
		searches[0].Query.Get("allow_no_indices") != "true" {
		t.Error("journal search must carry ignore_unavailable+allow_no_indices (admin index deletes must not break reads)")
	}
}

func TestJournalMissingIndexReadsAsEmpty(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		return http.StatusNotFound, `{"status":404,"error":{"type":"index_not_found_exception"}}`
	})

	entries, meta, err := c.JournalEntries(context.Background(), JournalQuery{Key: "k", Limit: 5})
	if err != nil {
		t.Fatalf("a store without a journal index must read as empty, got %v", err)
	}
	if meta.Total != 0 || len(entries) != 0 {
		t.Fatalf("entries=%v total=%d, want empty", entries, meta.Total)
	}
}

// A hit whose _source cannot decode must be COUNTED, not silently dropped:
// the reader must know the page lost entries to corruption, and corruption
// must not masquerade as size-truncation (finding 115).
func TestJournalEntriesCountUndecodable(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		if r.Method == http.MethodPost && r.Path == "/velruna-history-*/_search" {
			return http.StatusOK, `{"hits":{"total":{"value":3},"hits":[
				{"_index":"velruna-history-2026.11","_id":"e1","_source":{"fact_key":"k","op":"write","value":"good","created_at":"2026-11-09T10:00:00Z"}},
				{"_index":"velruna-history-2026.11","_id":"e2","_source":"not-an-object"},
				{"_index":"velruna-history-2026.11","_id":"e3","_source":"also-broken"}]}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})

	entries, meta, err := c.JournalEntries(context.Background(), JournalQuery{Key: "k", Limit: 5})
	if err != nil {
		t.Fatalf("JournalEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Value != "good" {
		t.Fatalf("entries=%v, want only the decodable one", entries)
	}
	if meta.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2", meta.Skipped)
	}
	// total 3 = 1 delivered + 2 corrupt: the page is NOT size-truncated.
	if meta.Total != 3 || meta.Truncated {
		t.Errorf("meta=%+v, want total=3 truncated=false", meta)
	}
}

func TestHistoryInfoParsesAggregations(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/velruna/_doc/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","found":true,"_source":{
				"key":"k","value":"v","category":"config","current_ref":"e2"}}`
		case r.Method == http.MethodPost && r.Path == "/velruna-history-*/_search":
			return http.StatusOK, `{"hits":{"total":{"value":3},"hits":[]},
				"aggregations":{
					"writes":{"doc_count":2},
					"rediscoveries":{"doc_count":1},
					"first":{"value":1762665600000},
					"last":{"value":1762752000000}}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})

	info, err := c.HistoryInfo(context.Background(), "k")
	if err != nil {
		t.Fatalf("HistoryInfo: %v", err)
	}
	if info.Writes != 2 || info.Rediscoveries != 1 {
		t.Errorf("writes=%d rediscoveries=%d, want 2/1", info.Writes, info.Rediscoveries)
	}
	if info.CurrentRef != "e2" || info.Category != "config" {
		t.Errorf("current_ref=%q category=%q, want e2/config", info.CurrentRef, info.Category)
	}
	if info.FirstAt == nil || info.LastAt == nil {
		t.Fatal("first_at/last_at must be present when aggregations answer")
	}
}

func TestPlanJournalPrune(t *testing.T) {
	ids := []string{"new", "mid", "old", "cur"}
	// keep=1: the newest survives; the currentRef entry is ALWAYS spared even
	// when it is older than the kept window (it proves the current value).
	drop := planJournalPrune(ids, "cur", 1)
	want := []string{"mid", "old"}
	if len(drop) != len(want) || drop[0] != want[0] || drop[1] != want[1] {
		t.Errorf("drop = %v, want %v", drop, want)
	}
	if d := planJournalPrune(ids, "", 4); d != nil {
		t.Errorf("len <= keep must drop nothing, got %v", d)
	}
}

// TestAddCorrectionUpdatesMainBeforeJournaling pins the doc-first order:
// the scripted update lands first, the journal entry is indexed only after
// it was accepted — a refused update must leave no journal trace (the
// journal is a history of accepted state; phantom entries are an audit lie).
// The doc->entry link survives via the preallocated entry id.
func TestAddCorrectionUpdatesMainBeforeJournaling(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/velruna/_doc/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","found":true,"_source":{
				"key":"k","value":"v1","category":"config","status":"verified"}}`
		case (r.Method == http.MethodPost || r.Method == http.MethodPut) && strings.HasPrefix(r.Path, "/velruna-history-"):
			return http.StatusOK, `{"_id":"ignored","result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
	})

	f := model.NewFact("k", "v2", "config", "p", "tool:read_file", "")
	res, err := c.Add(context.Background(), f)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if res.Created {
		t.Error("created = true, want false (key existed)")
	}

	// The journal entry for v2 must exist, and the main update must point at it.
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(appends))
	}
	var je map[string]interface{}
	if err := json.Unmarshal([]byte(appends[0].Body), &je); err != nil {
		t.Fatalf("journal body: %v", err)
	}
	if je["value"] != "v2" || je["op"] != model.OpWrite {
		t.Errorf("journal entry = %v, want op=write value=v2", je)
	}

	updates := fake.requestsFor(http.MethodPost, "/velruna/_update/k")
	if len(updates) != 1 {
		t.Fatalf("main updates = %d, want 1", len(updates))
	}
	// The link: the update carries current_ref = the preallocated id, and
	// the entry is indexed under that same id (op_type=create).
	id := appends[0].Path[strings.LastIndex(appends[0].Path, "/")+1:]
	if id == "" || !strings.Contains(updates[0].Body, id) {
		t.Errorf("main update must set current_ref to the entry id %q, body=%s", id, updates[0].Body)
	}
	if strings.Contains(updates[0].Body, "history.add") {
		t.Error("main script must not append to the in-doc array anymore (the journal is the history)")
	}

	// Order: the main write precedes the journal append.
	appendedIdx, updatedIdx := -1, -1
	fake.mu.Lock()
	for i, r := range fake.requests {
		if (r.Method == http.MethodPost || r.Method == http.MethodPut) && strings.HasPrefix(r.Path, "/velruna-history-") {
			appendedIdx = i
		}
		if r.Method == http.MethodPost && r.Path == "/velruna/_update/k" {
			updatedIdx = i
		}
	}
	fake.mu.Unlock()
	if appendedIdx < 0 || updatedIdx < 0 || appendedIdx < updatedIdx {
		t.Errorf("journal append (idx %d) must follow the main write (idx %d)", appendedIdx, updatedIdx)
	}
}

func TestAddSameValueDoesNotAppendJournal(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/velruna/_doc/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","found":true,"_source":{
				"key":"k","value":"v1","category":"config","status":"verified"}}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})

	f := model.NewFact("k", "v1", "config", "p", "tool:read_file", "n")
	if _, err := c.Add(context.Background(), f); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if fake.sawAnyPrefix("/velruna-history-") {
		t.Error("same-value add must not touch the journal (no new observation)")
	}
}

// TestJournalCollisionStringShape pins the isVersionConflict gate at the
// site where the predicate DECIDES SUCCESS: a middlebox-rewritten 409 in
// string shape that names the collision (deterministic-id re-append of a
// repair) is success; an unrelated 409 wording must not fake an append
// that never happened. The struct-shape twin pins the reason-substring
// fallback (a 409 whose type is not the engine's canonical one), and the
// negative subtests assert WHERE they fail — an error bubbling up from a
// later link update would hide a faked append.
func TestJournalCollisionStringShape(t *testing.T) {
	t.Run("collision wording is success", func(t *testing.T) {
		c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
			if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
				return code, body
			}
			switch {
			case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
				return http.StatusConflict, `{"error":"version conflict, document already exists","status":409}`
			case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
				return http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`
			}
			return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
		})
		f := model.NewFact("k", "v", "config", "p", "s", "")
		appended, err := c.MigrateFact(context.Background(), f, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("MigrateFact repair over a string-shaped collision: %v", err)
		}
		if appended != 1 {
			t.Errorf("appended = %d, want 1 (collision-as-success)", appended)
		}
	})
	t.Run("unrelated 409 wording is an honest failure", func(t *testing.T) {
		c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
			if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
				return code, body
			}
			switch {
			case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
				return http.StatusConflict, `{"error":"index blocked by policy","status":409}`
			}
			return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
		})
		f := model.NewFact("k", "v", "config", "p", "s", "")
		appended, err := c.MigrateFact(context.Background(), f, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
		if err == nil || !strings.Contains(err.Error(), "journal append") {
			t.Fatalf("failure must be the append itself, not something downstream: %v", err)
		}
		if appended != 0 {
			t.Errorf("appended = %d, want 0: an unrelated 409 must not fake the append", appended)
		}
	})
	t.Run("sub-409 collision wording never fakes the append", func(t *testing.T) {
		// Pins the status gate per shape: a 400 whose reason happens to
		// carry collision wording is a validation failure, not a doc
		// collision — dropping the status==409 requirement from either
		// branch would fake the append on these bodies.
		for _, body := range []string{
			`{"error":{"type":"mapper_parsing_exception","reason":"document already exists"},"status":400}`,
			`{"error":"document already exists","status":400}`,
		} {
			c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
				if code, b := journalInfraOK(r); code != http.StatusInternalServerError {
					return code, b
				}
				switch {
				case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
					return http.StatusBadRequest, body
				}
				return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
			})
			f := model.NewFact("k", "v", "config", "p", "s", "")
			appended, err := c.MigrateFact(context.Background(), f, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
			if err == nil || !strings.Contains(err.Error(), "journal append") {
				t.Fatalf("collision wording below 409 (%s) must fail the append honestly: %v", body, err)
			}
			if appended != 0 {
				t.Errorf("appended = %d, want 0 for %s: no document was written", appended, body)
			}
		}
	})
	t.Run("struct shape with collision wording is success", func(t *testing.T) {
		c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
			if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
				return code, body
			}
			switch {
			case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
				return http.StatusConflict, `{"error":{"type":"mapper_parsing_exception","reason":"document already exists"},"status":409}`
			case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
				return http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`
			}
			return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
		})
		f := model.NewFact("k", "v", "config", "p", "s", "")
		appended, err := c.MigrateFact(context.Background(), f, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("MigrateFact repair over a struct-shaped collision wording: %v", err)
		}
		if appended != 1 {
			t.Errorf("appended = %d, want 1 (reason-substring collision-as-success)", appended)
		}
	})
	t.Run("index-level already-exists is not a doc collision", func(t *testing.T) {
		// Each body is the ONLY one pinning its guard shape: the first
		// pins the string branch's resource_already_exists exclusion, the
		// second its "index [" marker half, the third the struct branch's
		// reason-wording exclusion (type rewritten, exception named in
		// the reason, no marker).
		for _, body := range []string{
			// names the exception type, no "index [" marker
			`{"error":"resource_already_exists_exception: [k2] already exists","status":409}`,
			// carries the "index [" marker, no exception-type wording
			`{"error":"index [velruna-history-2026-11/uuid] already exists","status":409}`,
			// struct shape: middlebox rewrote the type but left the
			// exception name in the reason
			`{"error":{"type":"mapper_parsing_exception","reason":"resource_already_exists_exception: [k2] already exists"},"status":409}`,
			// struct shape: the CANONICAL collision type carrying the race
			// wording — the guards must outrank the type acceptor, or a
			// rewritten body fakes the append through the canonical door
			`{"error":{"type":"version_conflict_engine_exception","reason":"index [velruna-history-2026-11/uuid] already exists"},"status":409}`,
			// struct shape: top type AND reason rewritten off the race
			// signature, but the root_cause array still names the index
			// race — the guard must read it too (three bodies, one per
			// root_cause signature, marker-free where the signature is
			// the name/type so each disjunct is pinned alone)
			`{"error":{"type":"mapper_parsing_exception","reason":"[k2] already exists","root_cause":[{"type":"resource_already_exists_exception","reason":"[k2] cannot create"}]},"status":409}`,
			`{"error":{"type":"mapper_parsing_exception","reason":"[k2] already exists","root_cause":[{"type":"engine_exception","reason":"resource_already_exists_exception for [k2]"}]},"status":409}`,
			`{"error":{"type":"mapper_parsing_exception","reason":"[k2] already exists","root_cause":[{"type":"engine_exception","reason":"index [velruna-history-2026-11/uuid] already exists"}]},"status":409}`,
			// struct shape: the race lives only in the caused_by chain
			`{"error":{"type":"mapper_parsing_exception","reason":"[k2] already exists","caused_by":{"type":"resource_already_exists_exception","reason":"index [velruna-history-2026-11/uuid] already exists"}},"status":409}`,
		} {
			c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
				if code, b := journalInfraOK(r); code != http.StatusInternalServerError {
					return code, b
				}
				switch {
				case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
					return http.StatusConflict, body
				}
				return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
			})
			f := model.NewFact("k", "v", "config", "p", "s", "")
			appended, err := c.MigrateFact(context.Background(), f, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
			if err == nil || !strings.Contains(err.Error(), "journal append") {
				t.Fatalf("index-race body %s must fail the append honestly: %v", body, err)
			}
			if appended != 0 {
				t.Errorf("appended = %d, want 0 for %s: no document was written", appended, body)
			}
		}
	})
	t.Run("string-shaped index race is not a doc collision", func(t *testing.T) {
		c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
			if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
				return code, body
			}
			switch {
			case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
				return http.StatusConflict, `{"error":"resource_already_exists_exception: index [velruna-history-2026-11/uuid] already exists","status":409}`
			}
			return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
		})
		f := model.NewFact("k", "v", "config", "p", "s", "")
		appended, err := c.MigrateFact(context.Background(), f, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
		if err == nil || !strings.Contains(err.Error(), "journal append") {
			t.Fatalf("a string-shaped index-creation race must fail the append honestly: %v", err)
		}
		if appended != 0 {
			t.Errorf("appended = %d, want 0: no document was written", appended)
		}
	})
	t.Run("struct shape with rewritten type keeps the race wording guard", func(t *testing.T) {
		c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
			if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
				return code, body
			}
			switch {
			case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
				return http.StatusConflict, `{"error":{"type":"mapper_parsing_exception","reason":"index [velruna-history-2026-11/uuid] already exists"},"status":409}`
			}
			return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
		})
		f := model.NewFact("k", "v", "config", "p", "s", "")
		appended, err := c.MigrateFact(context.Background(), f, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
		if err == nil || !strings.Contains(err.Error(), "journal append") {
			t.Fatalf("a type-rewritten index race must fail the append honestly: %v", err)
		}
		if appended != 0 {
			t.Errorf("appended = %d, want 0: no document was written", appended)
		}
	})
	t.Run("rewritten body saying version conflict is success in both shapes", func(t *testing.T) {
		// Pins the "version conflict" disjunct on its own (no "already
		// exists" in the reason): without this, corrupting that disjunct
		// stays green while CAS-path classification silently dies.
		for _, body := range []string{
			`{"error":{"type":"search_phase_execution_exception","reason":"[k]: version conflict, required seq_no [5], primary_term [2], but was [6] [2]"},"status":409}`,
			`{"error":"[k]: version conflict, required seq_no [5], primary_term [2], but was [6] [2]","status":409}`,
			// string-only disjunct pins: "already exists" alone, and the
			// underscore token "version_conflict" naming no phrase with a
			// space — each is the only match in its body, so deleting that
			// one disjunct from the string branch fails here.
			`{"error":"document already exists","status":409}`,
			`{"error":"version_conflict for document [k]","status":409}`,
			// the race-wording compound must be CONJUNCTIVE: an "index ["
			// marker with real conflict wording and no "already exists"
			// is a doc collision, not the index race.
			`{"error":{"type":"search_phase_execution_exception","reason":"index [velruna]: version conflict, required seq_no [5]"},"status":409}`,
			`{"error":"index [velruna]: version conflict, required seq_no [5]","status":409}`,
		} {
			c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
				if code, b := journalInfraOK(r); code != http.StatusInternalServerError {
					return code, b
				}
				switch {
				case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
					return http.StatusConflict, body
				case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
					return http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`
				}
				return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
			})
			f := model.NewFact("k", "v", "config", "p", "s", "")
			appended, err := c.MigrateFact(context.Background(), f, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("MigrateFact over collision body %s: %v", body, err)
			}
			if appended != 1 {
				t.Errorf("appended = %d, want 1 (version-conflict wording in %s)", appended, body)
			}
		}
	})
}

// TestUUIDAppendCollisionIsNotSuccess pins the idempotent flag's other
// half: the correction path's journal ref is a fresh uuid that cannot
// pre-exist, so a 409 with collision wording there is an anomaly (a
// rewritten body or middlebox) — the accepted correction must report
// the lagged entry, never return a clean success whose current_ref
// points at an entry that was never written.
func TestUUIDAppendCollisionIsNotSuccess(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/velruna/_doc/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","_version":1,"_seq_no":1,"_primary_term":1,"found":true,"_source":{"key":"k","value":"old","category":"config","project":"p","status":"verified"}}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`
		// opensearch-go Index sends PUT /<index>/_doc/<id> for a set
		// DocumentID (op_type=create is only the query param)
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
			return http.StatusConflict, `{"error":"version conflict, document already exists","status":409}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
	})
	res, err := c.Add(context.Background(), model.NewFact("k", "new", "config", "p", "s", ""))
	if err == nil || !strings.Contains(err.Error(), "journal entry lagged") || !strings.Contains(err.Error(), "journal append") {
		t.Fatalf("a 409 over a fresh uuid ref must report the lagged entry, got: %v", err)
	}
	// The verified fixture must NOT claim a resurrection: at the MCP error
	// edge the text is the only surface, so the wording itself is pinned.
	if strings.Contains(err.Error(), "resurrect") {
		t.Errorf("a correction over a verified fact must not name a resurrection: %v", err)
	}
	if !res.Changed || res.Resurrected {
		t.Errorf("res = %+v, want Changed=true Resurrected=false (the fixture is verified, not obsolete)", res)
	}
	// The 409 branch must have actually run: an append DOC request was
	// sent and rejected (not the pipeline _settings PUT, not a
	// fall-through to the 500 sentinel).
	fake.mu.Lock()
	var sawAppend bool
	for _, r := range fake.requests {
		if r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-") && strings.Contains(r.Path, "/_doc/") {
			sawAppend = true
		}
	}
	fake.mu.Unlock()
	if !sawAppend {
		t.Fatal("the journal append doc write never reached the fake — the 409 path is not being exercised")
	}
}

// A value-change add over an OBSOLETE fact resurrects; if its journal
// append lags, the typed answer is discarded at the MCP error edge, so
// the error text itself must name the resurrection.
func TestCorrectionOverObsoleteLagNamesResurrection(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/velruna/_doc/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","_version":1,"_seq_no":1,"_primary_term":1,"found":true,"_source":{"key":"k","value":"old","category":"config","project":"p","status":"obsolete"}}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
			return http.StatusConflict, `{"error":"version conflict, document already exists","status":409}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
	})
	res, err := c.Add(context.Background(), model.NewFact("k", "new", "config", "p", "s", ""))
	if err == nil || !strings.Contains(err.Error(), "resurrecting an obsolete fact") || !strings.Contains(err.Error(), "journal entry lagged") || !strings.Contains(err.Error(), "journal append") {
		t.Fatalf("a lagged correction over an obsolete fact must name the resurrection, got: %v", err)
	}
	if !res.Changed || !res.Resurrected {
		t.Errorf("res = %+v, want Changed=true Resurrected=true", res)
	}
	// The lag must come from the append collision, not from journal-infra
	// setup failing first (same reachability pin as the sibling tests).
	fake.mu.Lock()
	var sawAppend bool
	for _, r := range fake.requests {
		if r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-") && strings.Contains(r.Path, "/_doc/") {
			sawAppend = true
		}
	}
	fake.mu.Unlock()
	if !sawAppend {
		t.Fatal("the journal append doc write never reached the fake — the 409 path is not being exercised")
	}
}

// TestResurrectAppendCollisionIsNotSuccess pins the resurrect site's
// idempotent=false flag and its error wording: the fresh uuid ref of a
// resurrection cannot pre-exist, so the 409 must surface as a lagged
// entry beside Changed=true (the resurrect itself landed) — and the
// message must name the resurrection, not a correction.
func TestResurrectAppendCollisionIsNotSuccess(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/velruna/_doc/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","_version":1,"_seq_no":1,"_primary_term":1,"found":true,"_source":{"key":"k","value":"v","category":"config","project":"p","status":"obsolete"}}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
			return http.StatusConflict, `{"error":"version conflict, document already exists","status":409}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
	})
	// A summary rides along: the same-value resurrect drops it, and on this
	// path the error text is the only surface that reaches the caller (the
	// typed answer is discarded at the MCP error edge) — pin the hint.
	f := model.NewFact("k", "v", "config", "p", "s", "")
	f.Summary = "a toc the same-value resurrect must drop"
	res, err := c.Add(context.Background(), f)
	if err == nil || !strings.Contains(err.Error(), "resurrection accepted") || !strings.Contains(err.Error(), "journal append") {
		t.Fatalf("resurrect over a 409 uuid append must report the lagged entry, got: %v", err)
	}
	if !strings.Contains(err.Error(), "passed summary was ignored") {
		t.Errorf("the lagged-resurrect error must name the dropped summary, got: %v", err)
	}
	if !res.SummaryIgnored {
		t.Errorf("res.SummaryIgnored = false, want true on a lagged same-value resurrect")
	}
	if !res.Changed {
		t.Errorf("res.Changed = false, want true (the resurrect itself landed): %v", err)
	}
	if !res.Resurrected {
		t.Errorf("res.Resurrected = false, want true (the doc is verified again even though the entry lagged): %v", err)
	}
	fake.mu.Lock()
	var sawAppend bool
	for _, r := range fake.requests {
		if r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-") && strings.Contains(r.Path, "/_doc/") {
			sawAppend = true
		}
	}
	fake.mu.Unlock()
	if !sawAppend {
		t.Fatal("the resurrect journal append doc write never reached the fake — the 409 path is not being exercised")
	}
}

func TestMigrateFactMovesArrayToJournal(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
			return http.StatusOK, `{"_id":"entry","result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})

	f := model.NewFact("k", "v3", "config", "p", "s", "")
	f.History = []model.HistoryEntry{
		{Value: "v1", ObservedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Source: "a"},
		{Value: "v2", ObservedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Source: "b"},
	}
	appended, err := c.MigrateFact(context.Background(), f, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("MigrateFact: %v", err)
	}
	if appended != 3 {
		t.Errorf("appended = %d, want 3 (2 history + 1 current)", appended)
	}

	var oct, nov int
	fake.mu.Lock()
	for _, r := range fake.requests {
		if r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-") {
			switch {
			case strings.HasPrefix(r.Path, "/velruna-history-2026.10/_doc/"):
				oct++
			case strings.HasPrefix(r.Path, "/velruna-history-2026.11/_doc/"):
				nov++
			}
		}
	}
	fake.mu.Unlock()
	if oct != 1 || nov != 2 {
		t.Errorf("entries by month: oct=%d nov=%d, want 1/2 (observed_at decides the index)", oct, nov)
	}

	updates := fake.requestsFor(http.MethodPost, "/velruna/_update/k")
	if len(updates) != 1 {
		t.Fatalf("fact updates = %d, want 1", len(updates))
	}
	var upd struct {
		Script struct {
			Lang   string `json:"lang"`
			Source string `json:"source"`
		} `json:"script"`
	}
	if err := json.Unmarshal([]byte(updates[0].Body), &upd); err != nil {
		t.Fatalf("update body: %v", err)
	}
	if upd.Script.Lang != "painless" {
		t.Errorf("script lang = %q, want painless", upd.Script.Lang)
	}
	if !strings.Contains(upd.Script.Source, `remove("history")`) || !strings.Contains(upd.Script.Source, "current_ref") {
		t.Errorf("migration update must link current_ref and drop the array, source=%s", upd.Script.Source)
	}
	// The link must be conditional: a concurrent Add that already set
	// current_ref carries a fresher ref; overwriting it would point the new
	// value at the old journal entry.
	if !strings.Contains(upd.Script.Source, `current_ref == null || ctx._source.current_ref == ""`) {
		t.Errorf("migration link must only set current_ref while empty, source=%s", upd.Script.Source)
	}
	// The painless lexer contract (house-wide invariant, now pinned for this
	// script too): double-quoted literals, no backslashes.
	if strings.Contains(upd.Script.Source, "'") || strings.Contains(upd.Script.Source, "\\") {
		t.Errorf("migration link script must stay quote- and escape-free, source=%s", upd.Script.Source)
	}
}

func TestAddCreatesIndexThenJournalsNewFact(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/velruna/_doc/k":
			return http.StatusNotFound, `{"status":404,"error":{"type":"document_missing_exception"}}`
		case r.Method == http.MethodPut && r.Path == "/velruna/_doc/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","result":"created"}`
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
			return http.StatusOK, `{"_id":"j1","result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})

	f := model.NewFact("k", "v1", "config", "p", "s", "")
	res, err := c.Add(context.Background(), f)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !res.Created {
		t.Error("created = false, want true")
	}
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("birth journal writes = %d, want 1", len(appends))
	}
	// The birth entry is appended under a deterministic id (PUT /_doc/<id>,
	// op_type=create): migrate-history's repair path re-appends the current
	// value under the same content-derived id, so a link failure between
	// append and link repairs by collision, never by duplication.
	if !strings.Contains(appends[0].Path, "/_doc/") {
		t.Errorf("birth append must carry a deterministic doc id, path=%s", appends[0].Path)
	}
	if appends[0].Query.Get("op_type") != "create" {
		t.Errorf("birth append must be create-only, query=%s", appends[0].Query.Encode())
	}
	updates := fake.requestsFor(http.MethodPost, "/velruna/_update/k")
	if len(updates) != 1 {
		t.Fatalf("fact updates = %d, want 1 (the link)", len(updates))
	}
	birthID := appends[0].Path[strings.LastIndex(appends[0].Path, "/")+1:]
	if !strings.Contains(updates[0].Body, birthID) {
		t.Errorf("the created fact must be linked to the deterministic entry id %q, body=%s", birthID, updates[0].Body)
	}
}

// TestAddBirthLinkFailureRepairedByMigrationWithoutDuplication is the full
// finding-111 contract: the blackout lands between the birth append and
// the link update — the fact stays unlinked, a same-value retry is a touch
// and never relinks, but migrate-history repairs the fact and the repair
// collides with the already-written birth entry (same content-derived id)
// instead of appending a duplicate.
func TestAddBirthLinkFailureRepairedByMigrationWithoutDuplication(t *testing.T) {
	var mu sync.Mutex
	journalDocs := map[string]bool{}
	updateCalls := 0
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/velruna/_doc/k":
			return http.StatusNotFound, `{"status":404,"error":{"type":"document_missing_exception"}}`
		case r.Method == http.MethodPut && r.Path == "/velruna/_doc/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","result":"created"}`
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
			mu.Lock()
			defer mu.Unlock()
			if journalDocs[r.Path] {
				return http.StatusConflict, `{"error":{"type":"version_conflict_engine_exception","reason":"document already exists"},"status":409}`
			}
			journalDocs[r.Path] = true
			return http.StatusOK, `{"_id":"j1","result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			updateCalls++
			if updateCalls == 1 {
				// The blackout: the link update fails after the append landed.
				return http.StatusInternalServerError, `{"status":500,"error":"transient link failure"}`
			}
			return http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})

	f := model.NewFact("k", "v1", "config", "p", "s", "")
	if _, err := c.Add(context.Background(), f); err == nil {
		t.Fatal("Add must report the link failure")
	}
	if n := len(journalDocs); n != 1 {
		t.Fatalf("journal docs after failed Add = %d, want 1 (the birth entry)", n)
	}
	// migrate-history catches the unlinked fact: the same-value current
	// entry re-appends under the same deterministic id (409 = success).
	n, err := c.MigrateFact(context.Background(), f, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("MigrateFact repair: %v", err)
	}
	if n != 1 {
		t.Errorf("repair appended = %d, want 1", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(journalDocs) != 1 {
		t.Errorf("journal docs after repair = %d, want 1: the repair must collide, not duplicate", len(journalDocs))
	}
	if got := len(fake.requestsFor(http.MethodPost, "/velruna/_update/k")); got != 2 {
		t.Errorf("fact updates = %d, want 2 (failed link, repair link)", got)
	}
}

// TestEnsureJournalInfraRetriesAfterFailure pins the infra-install caching
// contract: only SUCCESS is remembered. A failed first attempt (403,
// network blip) must not mark the infra as installed for the rest of the
// process — a long-lived MCP server would append journal entries forever
// without the template's mappings and the server-clock pipeline.
func TestEnsureJournalInfraRetriesAfterFailure(t *testing.T) {
	tplPuts := 0
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_ingest/pipeline/"):
			return http.StatusOK, `{"velruna-journal-ts":{"processors":[]}}`
		case r.Method == http.MethodGet && r.Path == "/_index_template/velruna-journal":
			return http.StatusNotFound, `{"status":404,"error":{"type":"status_exception","reason":"index template not found"}}`
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/_index_template/"):
			tplPuts++
			if tplPuts == 1 {
				return http.StatusForbidden, `{"status":403,"error":{"type":"security_exception","reason":"no permission"}}`
			}
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/_settings"):
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodPost && r.Path == "/velruna-history-2026.11/_doc":
			return http.StatusOK, `{"_id":"e1","result":"created"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Method + " " + r.Path + `"}`
	})
	e := model.NewJournalEntry("k", "id", "p", "config", model.OpWrite, "v", "", "s", time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC))
	if _, err := c.AppendJournal(context.Background(), e); err == nil {
		t.Fatal("first append must fail while the template cannot be installed")
	}
	if _, err := c.AppendJournal(context.Background(), e); err != nil {
		t.Fatalf("second append must re-attempt the infra install: %v", err)
	}
	if tplPuts != 2 {
		t.Errorf("template PUTs = %d, want 2: a failed install is not a success", tplPuts)
	}
}

// TestPruneJournalFailsOnFactReadFailure pins that a transient Get failure
// must abort the prune: swallowing it reads as "no current_ref to spare",
// silently disarming the protection of the entry that carries the current
// value.
func TestPruneJournalFailsOnFactReadFailure(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodPost && r.Path == "/velruna-history-*/_search":
			return http.StatusOK, `{"took":1,"hits":{"total":{"value":2,"relation":"eq"},"hits":[
				{"_index":"velruna-history-2026.11","_id":"a","_source":{"fact_key":"k","op":"write","value":"v1","created_at":"2026-11-01T10:00:00Z"}},
				{"_index":"velruna-history-2026.11","_id":"b","_source":{"fact_key":"k","op":"write","value":"v0","created_at":"2026-10-01T10:00:00Z"}}]}}`
		case r.Method == http.MethodGet && r.Path == "/velruna/_doc/k":
			return http.StatusInternalServerError, `{"status":500,"error":"transient"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Path + `"}`
	})
	if _, err := c.PruneJournal(context.Background(), "k", 1); err == nil {
		t.Fatal("a transient fact-read failure must not disarm the current_ref protection")
	} else if !strings.Contains(err.Error(), "read fact") {
		t.Errorf("the error must name the fact read: %v", err)
	}
	if fake.sawAnyPrefix("/_bulk") {
		t.Error("no bulk deletes may be issued when the fact read failed")
	}
}

// TestPruneJournalCapMessageOnOverflow: the query now pins
// track_total_hits, so the reported total is exact and the cap message
// states it plainly (the old "at least" hedge covered the 10000-capped
// estimate that track_total_hits removes). A server that still reports
// relation=gte must not crash or hide the overflow.
func TestPruneJournalCapMessageOnOverflow(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodPost && r.Path == "/velruna-history-*/_search" {
			return http.StatusOK, `{"took":1,"hits":{"total":{"value":10000,"relation":"eq"},"hits":[
				{"_index":"velruna-history-2026.11","_id":"a","_source":{"fact_key":"k","op":"write","value":"v1","created_at":"2026-11-01T10:00:00Z"}}]}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Path + `"}`
	})
	_, err := c.PruneJournal(context.Background(), "k", 1)
	if err == nil {
		t.Fatal("exceeding the readable cap must be an error")
	}
	if !strings.Contains(err.Error(), "10000 entries") || !strings.Contains(err.Error(), "readable cap 1000") {
		t.Errorf("cap message must state the exact count and the cap: %v", err)
	}
}

// TestMigrateFactRerunCollidesOnDeterministicIDs pins the partial-failure
// recovery contract: the migration appends under ids derived from the entry
// content, so the rerun the CLI instructs collides with the entries the
// first run wrote (409 = success) instead of duplicating them.
func TestMigrateFactRerunCollidesOnDeterministicIDs(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	updateCalls := 0
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
			mu.Lock()
			defer mu.Unlock()
			if seen[r.Path] {
				return http.StatusConflict, `{"error":{"type":"version_conflict_engine_exception","reason":"document already exists"},"status":409}`
			}
			seen[r.Path] = true
			return http.StatusOK, `{"_id":"x","result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			updateCalls++
			if updateCalls == 1 {
				return http.StatusInternalServerError, `{"status":500,"error":"transient link failure"}`
			}
			return http.StatusOK, `{"result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Path + `"}`
	})
	mk := func() *model.Fact {
		f := model.NewFact("k", "v3", "config", "p", "s", "")
		f.ObservedAt = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		f.History = []model.HistoryEntry{
			{Value: "v1", ObservedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Source: "a"},
		}
		return f
	}
	// First run: the appends land, the link update fails (partial failure).
	if _, err := c.MigrateFact(context.Background(), mk(), time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("first run must report the link failure")
	}
	// The rerun re-appends the same logical entries: the ids collide, and
	// the collision is success, not duplication.
	if _, err := c.MigrateFact(context.Background(), mk(), time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("rerun must succeed on collisions: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Errorf("journal docs = %d, want 2: the rerun must not duplicate entries", len(seen))
	}
}

// TestMigrateFactZeroTimestampRerunCollides: legacy facts with NO
// timestamps at all (observed_at/updated_at both zero) must stay rerun-safe
// too: the id hashes a zero-time marker and the target index is derived
// from the SAME stable time — not the rerun's clock — so the second run
// collides with the first in the same doc of the same index even when the
// runs land in different periods.
func TestMigrateFactZeroTimestampRerunCollides(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	updateCalls := 0
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
			mu.Lock()
			defer mu.Unlock()
			if seen[r.Path] {
				return http.StatusConflict, `{"error":{"type":"version_conflict_engine_exception","reason":"document already exists"},"status":409}`
			}
			seen[r.Path] = true
			return http.StatusOK, `{"_id":"x","result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			updateCalls++
			if updateCalls == 1 {
				return http.StatusInternalServerError, `{"status":500,"error":"transient link failure"}`
			}
			return http.StatusOK, `{"result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Path + `"}`
	})
	mk := func() *model.Fact {
		f := model.NewFact("k", "v2", "config", "p", "s", "")
		f.ObservedAt = time.Time{}
		f.UpdatedAt = time.Time{}
		f.History = []model.HistoryEntry{{Value: "v1", Source: "a"}} // no timestamp
		return f
	}
	if _, err := c.MigrateFact(context.Background(), mk(), time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("first run must report the link failure")
	}
	// A rerun after the month flipped: the display ts (now-fallback) moved
	// to a new period, the stable id and index must not have.
	if _, err := c.MigrateFact(context.Background(), mk(), time.Date(2026, 12, 14, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("rerun must succeed on collisions: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Errorf("journal docs = %d, want 2: zero-timestamp rerun across a period boundary must not duplicate", len(seen))
	}
}

// TestAddCorrectionJournalsDocFactID: the journal entry of a correction
// carries the doc's fact_id (the identity that survives), not the fresh
// uuid of the caller's in-memory fact.
func TestAddCorrectionJournalsDocFactID(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalRoutesOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/velruna/_doc/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","found":true,"_source":{
				"key":"k","fact_id":"doc-1","value":"old","category":"config","status":"verified"}}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			return http.StatusOK, `{"result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Path + `"}`
	})
	f := model.NewFact("k", "new", "config", "p", "s", "")
	if _, err := c.Add(context.Background(), f); err != nil {
		t.Fatalf("Add: %v", err)
	}
	writes := journalAppends(fake)
	if len(writes) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(writes))
	}
	if !strings.Contains(writes[0].Body, `"fact_id":"doc-1"`) {
		t.Errorf("the correction entry must carry the doc's fact_id, body=%s", writes[0].Body)
	}
}

// TestJournalEntriesSortedByStoreClockFirst: the pipeline stamps ingest_at
// with the store clock so ordering survives writer clock skew; reads must
// honor that clock, not the clients'.
func TestJournalEntriesSortedByStoreClockFirst(t *testing.T) {
	body := buildJournalEntriesBody(JournalQuery{Key: "k", Limit: 5})
	sortKeys, ok := body["sort"].([]map[string]interface{})
	if !ok || len(sortKeys) != 2 {
		t.Fatalf("sort = %v, want two keys", body["sort"])
	}
	if _, ok := sortKeys[0]["ingest_at"]; !ok {
		t.Errorf("first sort key must be the pipeline's server clock: %v", sortKeys[0])
	}
	if _, ok := sortKeys[1]["created_at"]; !ok {
		t.Errorf("second sort key must be created_at: %v", sortKeys[1])
	}
}

// TestAppendJournalPinsPipelineOnExistingIndex: an append pins the ts
// pipeline on the journal index via one idempotent settings PUT — the heal
// path for indices auto-created while infra install was broken (entries
// without ingest_at sort last and become the first prune victims). One PUT
// per index per process.
func TestAppendJournalPinsPipelineOnExistingIndex(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		if r.Method == http.MethodPost && r.Path == "/velruna-history-2026.11/_doc" {
			return http.StatusOK, `{"_id":"e1","result":"created"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Method + " " + r.Path + `"}`
	})
	e := model.NewJournalEntry("k", "id", "p", "config", model.OpWrite, "v", "", "s", time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC))
	if _, err := c.AppendJournal(context.Background(), e); err != nil {
		t.Fatalf("AppendJournal: %v", err)
	}
	if _, err := c.AppendJournal(context.Background(), e); err != nil {
		t.Fatalf("AppendJournal #2: %v", err)
	}
	puts := fake.requestsFor(http.MethodPut, "/velruna-history-2026.11/_settings")
	if len(puts) != 1 {
		t.Errorf("settings PUTs = %d, want 1 (pinned once per index per process)", len(puts))
	}
	if !strings.Contains(puts[0].Body, "velruna-journal-ts") {
		t.Errorf("settings body must wire the ts pipeline: %s", puts[0].Body)
	}
}

// TestScanAllFactsEmptyProjectScansAll: the default migrate-history scope
// must scan every project. A term{"project":""} filter matches no document
// (writers always store a non-empty project), so an empty project omits the
// scope filter instead of emptying it — otherwise the one-time migration
// silently no-ops and reports "scanned: 0". The scan also filters
// needs-migration SERVER-side so reruns converge past the page cap.
func TestScanAllFactsEmptyProjectScansAll(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodPost && r.Path == "/velruna/_search" {
			return http.StatusOK, `{"took":1,"hits":{"total":{"value":1,"relation":"eq"},"hits":[
				{"_index":"velruna","_id":"k","_source":{"fact_id":"i","key":"k","project":"p1","value":"v","category":"config","status":"verified"}}]}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Method + " " + r.Path + `"}`
	})
	facts, _, err := c.ScanAllFacts(context.Background(), "")
	if err != nil {
		t.Fatalf("ScanAllFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("facts = %d, want 1: an empty project must scan all projects", len(facts))
	}
	reqs := fake.requestsFor(http.MethodPost, "/velruna/_search")
	if len(reqs) != 1 {
		t.Fatalf("searches = %d, want 1", len(reqs))
	}
	if strings.Contains(reqs[0].Body, `"term":{"project":`) {
		t.Errorf("empty-project scan must omit the project term filter, body=%s", reqs[0].Body)
	}
	for _, want := range []string{"current_ref", "instanceof List", "isEmpty", "!= null", "minimum_should_match"} {
		if !strings.Contains(reqs[0].Body, want) {
			t.Errorf("empty-project scan must carry the needs-migration filter (%s), body=%s", want, reqs[0].Body)
		}
	}
	// The history arm must NOT be an exists query: history is mapped
	// enabled:false (never indexed), so exists silently matches zero docs
	// and the linked-but-array cohort would never converge.
	if strings.Contains(reqs[0].Body, `{"exists":{"field":"history"}}`) {
		t.Errorf("exists on an unindexed field is a dead branch, body=%s", reqs[0].Body)
	}
	if _, _, err := c.ScanAllFacts(context.Background(), "p1"); err != nil {
		t.Fatalf("ScanAllFacts scoped: %v", err)
	}
	reqs = fake.requestsFor(http.MethodPost, "/velruna/_search")
	if len(reqs) != 2 {
		t.Fatalf("searches = %d, want 2", len(reqs))
	}
	if !strings.Contains(reqs[1].Body, `"term":{"project":"p1"}`) {
		t.Errorf("scoped scan must keep the project term filter, body=%s", reqs[1].Body)
	}
}

// TestMigrateFactLinkedWithArrayAppendsOnlyHistory: a fact that already
// carries current_ref but still carries the legacy array gets the array
// entries and nothing else — no fabricated duplicate of the current value,
// no re-link (the script keeps the live ref) — and its array is removed.
func TestMigrateFactLinkedWithArrayAppendsOnlyHistory(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/velruna-history-"):
			return http.StatusOK, `{"_id":"entry","result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})
	f := model.NewFact("k", "v-current", "config", "p", "s", "")
	f.CurrentRef = "live-ref"
	f.History = []model.HistoryEntry{
		{Value: "v1", ObservedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Source: "a"},
	}
	appended, err := c.MigrateFact(context.Background(), f, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("MigrateFact: %v", err)
	}
	if appended != 1 {
		t.Errorf("appended = %d, want 1 (array entry only, the linked current value is already journaled)", appended)
	}
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal writes = %d, want 1", len(appends))
	}
	if strings.Contains(appends[0].Body, "v-current") {
		t.Errorf("must not re-journal the linked current value, body=%s", appends[0].Body)
	}
	updates := fake.requestsFor(http.MethodPost, "/velruna/_update/k")
	if len(updates) != 1 {
		t.Fatalf("fact updates = %d, want 1 (the array must still be removed)", len(updates))
	}
	if !strings.Contains(updates[0].Body, "current_ref == null") {
		t.Errorf("link script must stay conditional (live ref untouched), body=%s", updates[0].Body)
	}

	// A fully migrated fact (linked, no array) is a no-op: no further
	// appends, no further update request.
	full := model.NewFact("k2", "v", "config", "p", "s", "")
	full.CurrentRef = "live-ref"
	if n, err := c.MigrateFact(context.Background(), full, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)); err != nil || n != 0 {
		t.Errorf("fully migrated fact: n=%d err=%v, want 0/no error", n, err)
	}
	if got := len(journalAppends(fake)); got != 1 {
		t.Errorf("journal writes after no-op = %d, want still 1", got)
	}
	if got := len(fake.requestsFor(http.MethodPost, "/velruna/_update/k")); got != 1 {
		t.Errorf("updates after no-op = %d, want still 1", got)
	}
}

// journalDriftRePutAnswers answers a drift scenario: template stamped 0, a
// 3-node topology (desired 1), and lets the caller decide what the corrective
// template PUT returns.
func journalDriftRePutAnswers(tplPutStatus int) func(recordedRequest) (int, string) {
	return func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_ingest/pipeline/"):
			return http.StatusOK, `{"velruna-journal-ts":{"processors":[]}}`
		case r.Method == http.MethodGet && r.Path == "/_index_template/velruna-journal":
			return http.StatusOK, `{"index_templates":[{"name":"velruna-journal","index_template":{"template":{"settings":{"index":{"number_of_replicas":"0"}}}}}]}`
		case r.Method == http.MethodGet && r.Path == "/_cluster/health":
			return http.StatusOK, `{"status":"green","number_of_data_nodes":3}`
		case r.Method == http.MethodPut && r.Path == "/_index_template/velruna-journal":
			if tplPutStatus == http.StatusForbidden {
				return http.StatusForbidden, `{"status":403,"error":{"type":"security_exception"}}`
			}
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/_settings"):
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/velruna-history-") && strings.Contains(r.Path, "/_doc"):
			return http.StatusOK, `{"_id":"e1","result":"created"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Method + " " + r.Path + `"}`
	}
}

func TestJournalDriftRePutFailureIsTolerated(t *testing.T) {
	// The template exists and appends work with it; a failed corrective re-PUT
	// (read-only store / permission gap) must NOT fail the append.
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.IndexReplicas = -1
	}, journalDriftRePutAnswers(http.StatusForbidden))
	e := model.NewJournalEntry("k", "id", "p", "config", model.OpWrite, "v", "", "s", time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC))
	if _, err := c.AppendJournal(context.Background(), e); err != nil {
		t.Fatalf("AppendJournal must survive a failed drift re-PUT: %v", err)
	}
	if puts := fake.requestsFor(http.MethodPut, "/_index_template/velruna-journal"); len(puts) != 1 {
		t.Errorf("drift re-PUT attempts = %d, want 1 (attempted then tolerated)", len(puts))
	}
}

func TestJournalInfra403SkipsCreate(t *testing.T) {
	// A shared-store DLS role cannot read the pipeline or the template (403).
	// It must NOT attempt to create either (it would fail too); the append must
	// proceed against the externally-managed infra.
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.IndexReplicas = -1
	}, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_ingest/pipeline/"):
			return http.StatusForbidden, `{"status":403,"error":{"type":"security_exception"}}`
		case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_index_template/"):
			return http.StatusForbidden, `{"status":403,"error":{"type":"security_exception"}}`
		case r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/_settings"):
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/velruna-history-") && strings.Contains(r.Path, "/_doc"):
			return http.StatusOK, `{"_id":"e1","result":"created"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Method + " " + r.Path + `"}`
	})
	e := model.NewJournalEntry("k", "id", "p", "config", model.OpWrite, "v", "", "s", time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC))
	if _, err := c.AppendJournal(context.Background(), e); err != nil {
		t.Fatalf("AppendJournal with 403 on infra GET: %v", err)
	}
	if p := fake.requestsFor(http.MethodPut, "/_ingest/pipeline/velruna-journal-ts"); len(p) != 0 {
		t.Errorf("pipeline create attempts = %d, want 0 on 403", len(p))
	}
	if p := fake.requestsFor(http.MethodPut, "/_index_template/velruna-journal"); len(p) != 0 {
		t.Errorf("template create attempts = %d, want 0 on 403", len(p))
	}
	if len(journalAppends(fake)) != 1 {
		t.Errorf("journal appends = %d, want 1", len(journalAppends(fake)))
	}
}

// TestAddJournalsKeywords pins that an accepted value's entry carries the tag
// set. Tags are a stored qualifier (a tag-only change is a touch, not an entry),
// so the previous set must stay recoverable from the last entry that carried it
// — that is what makes re-tagging safe.
func TestAddJournalsKeywords(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalRoutesOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/velruna/_doc/kw":
			return http.StatusNotFound, `{"status":404,"error":"document_missing_exception"}`
		case r.Method == http.MethodPut && r.Path == "/velruna/_doc/kw":
			return http.StatusOK, `{"result":"created","_seq_no":1,"_primary_term":1}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/kw":
			return http.StatusOK, `{"result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + ` ` + r.Path + `"}`
	})

	f := model.NewFact("kw", "v", "config", "p", "user:confirmed", "")
	f.Keywords = []string{"perf", "go"}
	if _, err := c.Add(context.Background(), f); err != nil {
		t.Fatalf("Add: %v", err)
	}
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(appends))
	}
	if !strings.Contains(appends[0].Body, `"keywords":["perf","go"]`) {
		t.Errorf("journal entry must carry the tag set: %s", appends[0].Body)
	}
}

// TestBuildJournalEntriesWindow pins the time window: a one- or two-sided
// created_at range turns "what changed since my last session" into a query,
// not a full scan. Bounds are RFC3339 in UTC.
func TestBuildJournalEntriesWindow(t *testing.T) {
	since := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)

	mustOf := func(q JournalQuery) []map[string]interface{} {
		body := buildJournalEntriesBody(q)
		return body["query"].(map[string]interface{})["bool"].(map[string]interface{})["must"].([]map[string]interface{})
	}

	must := mustOf(JournalQuery{Key: "k", Since: since, Until: until})
	if len(must) != 2 {
		t.Fatalf("must clauses = %d, want 2 (key term + range)", len(must))
	}
	rng := must[1]["range"].(map[string]interface{})["created_at"].(map[string]interface{})
	if rng["gte"] != "2026-11-10T00:00:00Z" || rng["lte"] != "2026-11-11T00:00:00Z" {
		t.Errorf("range = %v, want the RFC3339 window", rng)
	}

	// A one-sided window carries only the side that was asked for.
	must = mustOf(JournalQuery{Key: "k", Since: since})
	rng = must[1]["range"].(map[string]interface{})["created_at"].(map[string]interface{})
	if _, has := rng["lte"]; has {
		t.Errorf("one-sided window must not invent an upper bound: %v", rng)
	}
	if _, has := rng["gte"]; !has {
		t.Errorf("window lost its lower bound: %v", rng)
	}

	// No window: the plain key term only.
	if must := mustOf(JournalQuery{Key: "k"}); len(must) != 1 {
		t.Errorf("must clauses = %d, want 1 without a window", len(must))
	}
}

// TestJournalInfraRunsOncePerProcess: a fresh client per tool call must not
// re-probe the pipeline and template on every append.
func TestJournalInfraRunsOncePerProcess(t *testing.T) {
	c1, fake := newFakeClientCfg(t, nil, journalInfraOK)
	if err := c1.ensureJournalInfra(context.Background()); err != nil {
		t.Fatalf("first ensureJournalInfra: %v", err)
	}
	c2, err := NewClient(c1.cfg)
	if err != nil {
		t.Fatalf("second NewClient: %v", err)
	}
	if err := c2.ensureJournalInfra(context.Background()); err != nil {
		t.Fatalf("second ensureJournalInfra: %v", err)
	}
	probes := 0
	fake.mu.Lock()
	for _, r := range fake.requests {
		if r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_ingest/pipeline/") {
			probes++
		}
	}
	fake.mu.Unlock()
	if probes != 1 {
		t.Fatalf("pipeline probes = %d, want 1 (the claim is per store, not per client)", probes)
	}
}

// TestJournalIndexPipelinePinnedOncePerProcess: the default_pipeline settings
// PUT is idempotent but not free, and it used to run on every append.
func TestJournalIndexPipelinePinnedOncePerProcess(t *testing.T) {
	const index = "velruna-history-2026.11"
	c1, fake := newFakeClientCfg(t, nil, journalInfraOK)
	if err := c1.ensureJournalIndexPipeline(context.Background(), index); err != nil {
		t.Fatalf("first pin: %v", err)
	}
	c2, err := NewClient(c1.cfg)
	if err != nil {
		t.Fatalf("second NewClient: %v", err)
	}
	if err := c2.ensureJournalIndexPipeline(context.Background(), index); err != nil {
		t.Fatalf("second pin: %v", err)
	}
	puts := 0
	fake.mu.Lock()
	for _, r := range fake.requests {
		if r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/_settings") {
			puts++
		}
	}
	fake.mu.Unlock()
	if puts != 1 {
		t.Fatalf("settings puts = %d, want 1 (per index per process)", puts)
	}
}

// TestJournalInfraFailureIsReleasedForTheNextClient: a failed install must not
// leave the store marked done, or a long-lived server appends entries without
// the ingest pipeline and the template mappings.
func TestJournalInfraFailureIsReleasedForTheNextClient(t *testing.T) {
	broken := func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_ingest/pipeline/"):
			return http.StatusInternalServerError, `{"error":"boom"}`
		case r.Method == http.MethodPut && strings.Contains(r.Path, "/_ingest/pipeline/"):
			return http.StatusInternalServerError, `{"error":"boom"}`
		}
		return journalInfraOK(r)
	}
	c1, fake := newFakeClientCfg(t, nil, broken)
	if err := c1.ensureJournalInfra(context.Background()); err == nil {
		t.Fatal("first install must fail")
	}
	c2, err := NewClient(c1.cfg)
	if err != nil {
		t.Fatalf("second NewClient: %v", err)
	}
	if err := c2.ensureJournalInfra(context.Background()); err == nil {
		t.Fatal("the second client must retry and fail too")
	}
	probes := 0
	fake.mu.Lock()
	for _, r := range fake.requests {
		if r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_ingest/pipeline/") {
			probes++
		}
	}
	fake.mu.Unlock()
	if probes != 2 {
		t.Fatalf("pipeline probes = %d, want 2 (a failed pass must not claim the store)", probes)
	}
}

// TestJournalIndexPipelineFailureIsReleasedForTheNextClient: a failed settings
// PUT must leave the pin claim open, or a legacy index keeps writing entries
// without ingest_at for the rest of the process.
func TestJournalIndexPipelineFailureIsReleasedForTheNextClient(t *testing.T) {
	const index = "velruna-history-2026.11"
	broken := func(r recordedRequest) (int, string) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/_settings") {
			return http.StatusInternalServerError, `{"error":"boom"}`
		}
		return journalInfraOK(r)
	}
	c1, fake := newFakeClientCfg(t, nil, broken)
	if err := c1.ensureJournalIndexPipeline(context.Background(), index); err == nil {
		t.Fatal("first pin must fail")
	}
	c2, err := NewClient(c1.cfg)
	if err != nil {
		t.Fatalf("second NewClient: %v", err)
	}
	if err := c2.ensureJournalIndexPipeline(context.Background(), index); err == nil {
		t.Fatal("the second client must retry and fail too")
	}
	puts := 0
	fake.mu.Lock()
	for _, r := range fake.requests {
		if r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/_settings") {
			puts++
		}
	}
	fake.mu.Unlock()
	if puts != 2 {
		t.Fatalf("settings puts = %d, want 2 (a failed pin must not claim the index)", puts)
	}
}

// TestJournalInfraWaitsForTheFirstPass pins the "wait, not skip" property: a
// second caller must return only after the first install finished. With
// skip-on-claim it would return while the template is still missing and write
// an entry without the ingest pipeline.
func TestJournalInfraWaitsForTheFirstPass(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	c1, _ := newFakeClientCfg(t, nil, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_ingest/pipeline/") {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
		}
		return journalInfraOK(r)
	})
	c2, err := NewClient(c1.cfg)
	if err != nil {
		t.Fatalf("second NewClient: %v", err)
	}

	firstDone := make(chan struct{})
	go func() {
		_ = c1.ensureJournalInfra(context.Background())
		close(firstDone)
	}()
	<-started

	secondDone := make(chan struct{})
	go func() {
		_ = c2.ensureJournalInfra(context.Background())
		close(secondDone)
	}()

	select {
	case <-secondDone:
		t.Fatal("the second caller returned while the first install was still running (skip, not wait)")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	<-firstDone
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the second caller never returned after the first install finished")
	}
}
