package client

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/model"
)

func TestBuildSearchBody_NoFilters(t *testing.T) {
	got := buildSearchBody("hello world", "", "", "", "", 0)
	q := got["query"].(map[string]interface{})
	boolQ := q["bool"].(map[string]interface{})
	must := boolQ["must"].(map[string]interface{})
	mm := must["multi_match"].(map[string]interface{})
	if mm["query"].(string) != "hello world" {
		t.Errorf("query = %v, want hello world", mm["query"])
	}
	fields := mm["fields"].([]string)
	if len(fields) != 3 || fields[0] != "value" || fields[1] != "notes" || fields[2] != "summary" {
		t.Errorf("fields = %v, want [value notes summary]", fields)
	}
	if _, ok := boolQ["filter"]; ok {
		t.Error("filter present, want none")
	}
	if size, ok := got["size"].(int); !ok || size != NavSizeCap {
		// limit 0 = "no cap": an explicit size beats the server's implicit
		// default of 10, which silently truncated the result.
		t.Errorf("size = %v, want NavSizeCap (%d) for limit 0", got["size"], NavSizeCap)
	}
}

func TestBuildSearchBody_Limit(t *testing.T) {
	got := buildSearchBody("hello", "", "", "", "", 7)
	if size, ok := got["size"].(int); !ok || size != 7 {
		t.Errorf("size = %v, want 7", got["size"])
	}
}

// Navigation queries must carry an explicit size: without it OpenSearch
// returns an arbitrary 10-hit subset with no truncation signal.
func TestBuildNavBodiesSizeCapped(t *testing.T) {
	bodies := map[string]map[string]interface{}{
		"project": buildProjectBody("p", "", "key"),
		"keys":    buildKeysBody("p:", "", "", ""),
	}
	for name, body := range bodies {
		if size, ok := body["size"].(int); !ok || size != NavSizeCap {
			t.Errorf("%s: size = %v, want NavSizeCap (%d)", name, body["size"], NavSizeCap)
		}
	}
}

func TestBuildSearchBody_AllFilters(t *testing.T) {
	got := buildSearchBody("hello", "proj1", "config", "verified", "", 0)
	q := got["query"].(map[string]interface{})
	boolQ := q["bool"].(map[string]interface{})
	must := boolQ["must"].(map[string]interface{})
	mm := must["multi_match"].(map[string]interface{})
	if mm["query"].(string) != "hello" {
		t.Errorf("query = %v, want hello", mm["query"])
	}
	fields := mm["fields"].([]string)
	if len(fields) != 3 || fields[0] != "value" || fields[1] != "notes" || fields[2] != "summary" {
		t.Errorf("fields = %v, want [value notes summary]", fields)
	}
	filters, ok := boolQ["filter"].([]map[string]interface{})
	if !ok {
		t.Error("filter missing")
		return
	}
	if len(filters) != 3 {
		t.Errorf("filter count = %d, want 3", len(filters))
		return
	}
	for i, m := range filters {
		term := m["term"].(map[string]interface{})
		switch i {
		case 0:
			if term["project"].(string) != "proj1" {
				t.Errorf("filter[0] project = %v, want proj1", term["project"])
			}
		case 1:
			if term["category"].(string) != "config" {
				t.Errorf("filter[1] category = %v, want config", term["category"])
			}
		case 2:
			if term["status"].(string) != "verified" {
				t.Errorf("filter[2] status = %v, want verified", term["status"])
			}
		}
	}
}

func TestBuildSearchBody_SingleFilter(t *testing.T) {
	got := buildSearchBody("hello", "", "config", "", "", 0)
	q := got["query"].(map[string]interface{})
	boolQ := q["bool"].(map[string]interface{})
	must := boolQ["must"].(map[string]interface{})
	mm := must["multi_match"].(map[string]interface{})
	if mm["query"].(string) != "hello" {
		t.Errorf("query = %v, want hello", mm["query"])
	}
	filters, ok := boolQ["filter"].([]map[string]interface{})
	if !ok {
		t.Error("filter missing")
		return
	}
	if len(filters) != 1 {
		t.Errorf("filter count = %d, want 1", len(filters))
		return
	}
	m := filters[0]
	term := m["term"].(map[string]interface{})
	if term["category"].(string) != "config" {
		t.Errorf("filter[0] category = %v, want config", term["category"])
	}
}

// TestBuildSearchBody_KeywordFilter pins that the tag filter is a plain term on
// the keywords array (exact match on the stored canonical, no analysis).
func TestBuildSearchBody_KeywordFilter(t *testing.T) {
	got := buildSearchBody("hello", "", "", "", "perf", 0)
	q := got["query"].(map[string]interface{})
	boolQ := q["bool"].(map[string]interface{})
	filters, ok := boolQ["filter"].([]map[string]interface{})
	if !ok || len(filters) != 1 {
		t.Fatalf("filters = %v, want one keyword term", boolQ["filter"])
	}
	term := filters[0]["term"].(map[string]interface{})
	if term["keywords"].(string) != "perf" {
		t.Errorf("keywords term = %v, want perf", term["keywords"])
	}
}

func TestBuildProjectBody(t *testing.T) {
	got := buildProjectBody("proj1", "", "key")
	q := got["query"].(map[string]interface{})
	b := q["bool"].(map[string]interface{})
	filters := b["filter"].([]map[string]interface{})
	if len(filters) != 1 {
		t.Fatalf("filters = %d, want 1 (no prefix)", len(filters))
	}
	term := filters[0]["term"].(map[string]interface{})
	if term["project"].(string) != "proj1" {
		t.Errorf("project = %v, want proj1", term["project"])
	}

	got = buildProjectBody("proj1", "proj1:state:", "key")
	q = got["query"].(map[string]interface{})
	b = q["bool"].(map[string]interface{})
	filters = b["filter"].([]map[string]interface{})
	if len(filters) != 2 {
		t.Fatalf("filters = %d, want 2 (project + prefix)", len(filters))
	}
	pfx := filters[1]["prefix"].(map[string]interface{})
	if pfx["key"].(string) != "proj1:state:" {
		t.Errorf("prefix key = %v, want proj1:state:", pfx["key"])
	}
}

func TestNormalizePrefix(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"state:", "state:", false},
		{"state:*", "state:", false},
		{" proj:foo:loc:* ", "proj:foo:loc:", false},
		{"proj:*:loc", "", true},
		{"*abc", "", true},
	}
	for _, tt := range tests {
		got, err := NormalizePrefix(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("normalizePrefix(%q) = %q, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizePrefix(%q): unexpected error %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("normalizePrefix(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBuildKeysBody(t *testing.T) {
	got := buildKeysBody("proj:", "state", "velruna", "verified")
	q := got["query"].(map[string]interface{})
	b := q["bool"].(map[string]interface{})
	filters := b["filter"].([]map[string]interface{})
	if len(filters) != 4 {
		t.Fatalf("filters = %d, want 4 (prefix, wildcard, project, status)", len(filters))
	}
	if pfx := filters[0]["prefix"].(map[string]interface{}); pfx["key"] != "proj:" {
		t.Errorf("prefix = %v, want proj:", pfx["key"])
	}
	if wc := filters[1]["wildcard"].(map[string]interface{}); wc["key"] != "*state*" {
		t.Errorf("wildcard = %v, want *state*", wc["key"])
	}
	if term := filters[2]["term"].(map[string]interface{}); term["project"] != "velruna" {
		t.Errorf("project filter = %v, want velruna", term["project"])
	}
	if term := filters[3]["term"].(map[string]interface{}); term["status"] != "verified" {
		t.Errorf("status filter = %v, want verified", term["status"])
	}
	sort := got["sort"].([]map[string]interface{})
	if order := sort[0]["key"].(map[string]interface{}); order["order"] != "asc" {
		t.Errorf("sort = %v, want key asc", sort[0])
	}
}

func TestBuildIndexBody_Mappings(t *testing.T) {
	got := buildIndexBody(0)
	props := got["mappings"].(map[string]interface{})["properties"].(map[string]interface{})

	expectedTypes := map[string]string{
		"fact_id":       "keyword",
		"key":           "keyword",
		"value":         "text",
		"category":      "keyword",
		"project":       "keyword",
		"status":        "keyword",
		"created_at":    "date",
		"updated_at":    "date",
		"observed_at":   "date",
		"ttl_days":      "integer",
		"keywords":      "keyword",
		"confirmed_by":  "object",
		"source":        "keyword",
		"notes":         "text",
		"summary":       "text",
		"current_ref":   "keyword",
		"superseded_by": "keyword",
		"author":        "keyword",
		"author_source": "keyword",
		"model":         "keyword",
		"host":          "keyword",
		"session":       "keyword",
		"updated_by":    "keyword",
	}

	for field, expectedType := range expectedTypes {
		if _, ok := props[field]; !ok {
			t.Errorf("missing mapping for %q", field)
			continue
		}
		m := props[field].(map[string]interface{})
		if m["type"].(string) != expectedType {
			t.Errorf("%q type = %v, want %s", field, m["type"], expectedType)
		}
	}

	// history carries enabled instead of just a type, so it is checked
	// separately from the loop above: stored, never indexed.
	history, ok := props["history"].(map[string]interface{})
	if !ok {
		t.Fatal("missing mapping for \"history\"")
	}
	if history["type"] != "object" {
		t.Errorf("history type = %v, want object", history["type"])
	}
	if history["enabled"] != false {
		t.Errorf("history enabled = %v, want false (stored, not indexed)", history["enabled"])
	}

	if len(props) != 24 {
		t.Errorf("expected 24 mappings, got %d", len(props))
	}
}

func TestBuildIndexBody_ValidJSON(t *testing.T) {
	got := buildIndexBody(0)
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal index body: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal index body: %v", err)
	}
	if _, ok := m["mappings"]; !ok {
		t.Error("missing mappings in index body")
	}
}

// TestBuildProjectBodySort pins the list sort contract: sort=updated orders
// by last activity (newest first, undated docs last — legacy facts must not
// hijack the top), anything else keeps the stable key-ascending navigation.
func TestBuildProjectBodySort(t *testing.T) {
	got := buildProjectBody("p", "", "updated")
	sort, ok := got["sort"].([]map[string]interface{})
	if !ok || len(sort) != 1 {
		t.Fatalf("sort = %v, want one clause", got["sort"])
	}
	updated, ok := sort[0]["updated_at"].(map[string]interface{})
	if !ok || updated["order"] != "desc" || updated["missing"] != "_last" {
		t.Errorf("updated sort clause = %v, want desc with missing:_last", sort[0])
	}

	got = buildProjectBody("p", "", "key")
	sort = got["sort"].([]map[string]interface{})
	key, ok := sort[0]["key"].(map[string]interface{})
	if !ok || key["order"] != "asc" {
		t.Errorf("key sort clause = %v, want asc", sort[0])
	}
}

func TestAddTouchPreservesObservedAt(t *testing.T) {
	existing := `{"fact_id":"fid","key":"k","value":"same","category":"config","project":"proj","status":"verified","created_at":"2020-01-01T00:00:00Z","updated_at":"2020-01-01T00:00:00Z","observed_at":"2020-01-01T00:00:00Z"}`
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == "GET":
			return 200, `{"_index":"velruna","_id":"k","_version":2,"found":true,"_source":` + existing + `}`
		case r.Method == "POST" && strings.Contains(r.Path, "_update"):
			return 200, `{"result":"updated"}`
		}
		return 200, `{}`
	})

	res, err := c.Add(context.Background(), model.NewFact("k", "same", "config", "proj", "", ""))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if res.Created {
		t.Error("Created = true, want false (same value)")
	}
	if res.Changed {
		t.Error("Changed = true, want false (same value)")
	}
	if len(fake.requestsFor("POST", "/velruna/_doc")) != 0 {
		t.Error("touch must not index a new document")
	}
	updates := fake.requestsFor("POST", "/velruna/_update/k")
	if len(updates) != 1 {
		t.Fatalf("touch updates = %d, want 1", len(updates))
	}
	// The observation is old: a same-value touch must not move observed_at.
	if strings.Contains(updates[0].Body, "observed_at") {
		t.Errorf("touch body = %s, want no observed_at (the observation is old)", updates[0].Body)
	}
}

func TestAddSameValueTouchMovesUpdatedBy(t *testing.T) {
	// A re-confirmation by a new author is not a correction, but the audit
	// trail must move: updated_by records who last confirmed the fact.
	existing := `{"fact_id":"fid","key":"k","value":"same","category":"config","project":"proj","status":"verified","created_at":"2020-01-01T00:00:00Z","updated_at":"2020-01-01T00:00:00Z","observed_at":"2020-01-01T00:00:00Z"}`
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) { cfg.Author = "agent-b" },
		func(r recordedRequest) (int, string) {
			switch {
			case r.Method == "GET":
				return 200, `{"_index":"velruna","_id":"k","_version":2,"found":true,"_source":` + existing + `}`
			case strings.Contains(r.Path, "_update"):
				return 200, `{"result":"updated"}`
			}
			return 200, `{}`
		})

	if _, err := c.Add(context.Background(), model.NewFact("k", "same", "config", "proj", "", "")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	updates := fake.requestsFor("POST", "/velruna/_update/k")
	if len(updates) != 1 {
		t.Fatalf("touch runs = %d, want 1", len(updates))
	}
	if !strings.Contains(updates[0].Body, `"agent-b"`) {
		t.Errorf("touch body = %s, want updated_by carrying the new author", updates[0].Body)
	}
}

func TestAddNewValueWritesCurrentRef(t *testing.T) {
	// The doc's current_ref must not lag the journal: after a correction it
	// points at the journal entry that carries the new value.
	fakeSrc := `{"fact_id":"fid","key":"k","value":"old","category":"config","project":"proj","status":"verified","created_at":"2020-01-01T00:00:00Z","updated_at":"2020-01-01T00:00:00Z","observed_at":"2020-01-01T00:00:00Z"}`
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == "GET":
			return 200, `{"_index":"velruna","_id":"k","_version":2,"found":true,"_source":` + fakeSrc + `}`
		case strings.Contains(r.Path, "-history-"):
			return 200, `{"result":"created","_id":"journal-entry-1"}`
		case strings.Contains(r.Path, "_update"):
			return 200, `{"result":"updated"}`
		}
		return 200, `{}`
	})

	res, err := c.Add(context.Background(), model.NewFact("k", "new value", "config", "proj", "", ""))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !res.Changed {
		t.Fatal("Changed = false, want true")
	}
	updates := fake.requestsFor("POST", "/velruna/_update/k")
	if len(updates) != 1 {
		t.Fatalf("correction script runs = %d, want 1", len(updates))
	}
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(appends))
	}
	if refID := appends[0].Path[strings.LastIndex(appends[0].Path, "/")+1:]; refID == "" || !strings.Contains(updates[0].Body, `"current_ref":"`+refID+`"`) {
		t.Errorf("correction body = %s, want current_ref pointing at the journaled entry id %q", updates[0].Body, refID)
	}
}

func TestAddSameValueNoSummaryKeepsExisting(t *testing.T) {
	// "Not provided, leave alone" applies to summary too: the no-op touch must
	// not send summary at all, so the stored one survives.
	existing := `{"fact_id":"fid","key":"k","value":"same","summary":"kept","category":"config","project":"proj","status":"verified","created_at":"2020-01-01T00:00:00Z","updated_at":"2020-01-01T00:00:00Z","observed_at":"2020-01-01T00:00:00Z"}`
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == "GET":
			return 200, `{"_index":"velruna","_id":"k","_version":2,"found":true,"_source":` + existing + `}`
		case strings.Contains(r.Path, "_update"):
			return 200, `{"result":"updated"}`
		}
		return 200, `{}`
	})

	if _, err := c.Add(context.Background(), model.NewFact("k", "same", "config", "proj", "", "")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	updates := fake.requestsFor("POST", "/velruna/_update/k")
	if len(updates) != 1 {
		t.Fatalf("touch runs = %d, want 1", len(updates))
	}
	if strings.Contains(updates[0].Body, `"summary"`) {
		t.Errorf("touch body = %s, want no summary param (leave the stored one alone)", updates[0].Body)
	}
}

func TestPatchFieldsNilParams(t *testing.T) {
	// The script applies only non-null params: every field the caller left
	// empty must arrive as null, and status must be null for a touch.
	var body string
	c, _ := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"verified"}`,
		&body))

	if _, err := c.Patch(context.Background(), "k", PatchFields{NotesAppend: "x"}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	params := patchParams(t, body)
	for _, field := range []string{"value", "summary", "notes", "source", "status", "reason_append"} {
		if params[field] != nil {
			t.Errorf("params[%q] = %v, want null for a notes-only patch", field, params[field])
		}
	}
	if params["notes_append"] == nil {
		t.Error("params[notes_append] = null, want the dated line")
	}
}

func TestListSortUpdated(t *testing.T) {
	// sort="updated" must produce the stored-field sort: the presentation
	// order ("newest first") is a property of the query, not the client.
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		return 200, `{"hits":{"total":{"value":0},"hits":[]}}`
	})

	if _, _, err := c.List(context.Background(), "proj", "", "updated"); err != nil {
		t.Fatalf("List: %v", err)
	}
	queries := fake.requestsFor("POST", "/velruna/_search")
	if len(queries) != 1 {
		t.Fatalf("search requests = %d, want 1", len(queries))
	}
	if !strings.Contains(queries[0].Body, `"updated_at"`) || !strings.Contains(queries[0].Body, `"desc"`) {
		t.Errorf("list query = %s, want updated_at desc sort", queries[0].Body)
	}
	// The default stays stable by key.
	if _, _, err := c.List(context.Background(), "proj", "", ""); err != nil {
		t.Fatalf("List default: %v", err)
	}
	queries = fake.requestsFor("POST", "/velruna/_search")
	if len(queries) != 2 {
		t.Fatalf("search requests = %d, want 2", len(queries))
	}
	if !strings.Contains(queries[1].Body, `"key":{"order":"asc"}`) {
		t.Errorf("default list query = %s, want key asc sort", queries[1].Body)
	}
}

// TestObsoleteReRetireWithReasonRefused pins the audit symmetry with Patch:
// the journal gate skips a re-retire (no status change) while the script
// would append the reason — notes would grow an audit line the journal never
// recorded. Patch refuses at its "already obsolete" check; Obsolete mirrors it.
func TestObsoleteReRetireWithReasonRefused(t *testing.T) {
	obsoleteSrc := `{"fact_id":"fid","key":"k","value":"v","category":"config","project":"proj","status":"obsolete","created_at":"2020-01-01T00:00:00Z","updated_at":"2020-01-01T00:00:00Z","observed_at":"2020-01-01T00:00:00Z"}`
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == "GET":
			return 200, `{"_index":"velruna","_id":"k","found":true,"_source":` + obsoleteSrc + `}`
		case r.Method == "POST" && strings.Contains(r.Path, "_update"):
			if strings.Contains(r.Body, `"reason_append":"`) {
				t.Errorf("a reason reached the script on an already obsolete fact: %.120s", r.Body)
			}
			return 200, `{"result":"updated"}`
		}
		return 200, `{}`
	})
	if err := c.Obsolete(context.Background(), "k", "again", nil); err == nil {
		t.Fatal("expected refusal for a reason on an already obsolete fact")
	}
	// A re-confirmation without a reason stays allowed (idempotent touch).
	if err := c.Obsolete(context.Background(), "k", "", nil); err != nil {
		t.Fatalf("reason-less re-retire: %v", err)
	}
	if len(fake.requestsFor("POST", "/velruna/_update/k")) != 1 {
		t.Error("reason-less re-retire should still run the touch script")
	}
}

// TestObsoleteReasonSingleLine pins the notes convention: a retire reason is
// one line (whitespace runs folded), so the audit trail stays one event per
// line.
func TestObsoleteReasonSingleLine(t *testing.T) {
	var body string
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == "GET":
			return 200, `{"_index":"velruna","_id":"k","found":true,"_source":{"key":"k","value":"v","category":"config","project":"proj","status":"verified"}}`
		case strings.Contains(r.Path, "_update"):
			body = r.Body
			return 200, `{"result":"updated"}`
		}
		return 200, `{}`
	})
	if err := c.Obsolete(context.Background(), "k", "file deleted\non the server   today", nil); err != nil {
		t.Fatalf("Obsolete: %v", err)
	}
	if !strings.Contains(body, `"reason_append":"file deleted on the server today"`) {
		t.Errorf("update body = %.160s, want the reason folded to one line", body)
	}
}

// TestAddSameValueOnObsoleteJournalsResurrect pins the resurrect symmetry:
// re-adding the same value to an obsolete fact is a fresh verification — the
// transition is journaled (Patch resurrect parity), observed_at moves, and
// the fact points at the new journal entry. Changed=true tells the caller a
// version was accepted.
func TestAddSameValueOnObsoleteJournalsResurrect(t *testing.T) {
	obsoleteSrc := `{"fact_id":"fid","key":"k","value":"v","category":"config","project":"proj","status":"obsolete","created_at":"2020-01-01T00:00:00Z","updated_at":"2020-01-01T00:00:00Z","observed_at":"2020-01-01T00:00:00Z"}`
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalRoutesOK(r); code != 500 {
			return code, body
		}
		switch {
		case r.Method == "GET":
			return 200, `{"_index":"velruna","_id":"k","found":true,"_source":` + obsoleteSrc + `}`
		case r.Method == "POST" && strings.Contains(r.Path, "_update"):
			return 200, `{"result":"updated"}`
		}
		return 500, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
	})

	res, err := c.Add(context.Background(), model.NewFact("k", "v", "config", "proj", "", ""))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !res.Changed {
		t.Error("Changed = false, want true (resurrection is a new accepted state)")
	}
	if !res.Resurrected {
		t.Error("Resurrected = false, want true: a same-value add over an obsolete fact re-declares it alive")
	}
	journals := fake.sawAnyPrefix("/velruna-history-")
	if !journals {
		t.Error("resurrection must write a journal entry")
	}
	updates := fake.requestsFor("POST", "/velruna/_update/k")
	if len(updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(updates))
	}
	if !strings.Contains(updates[0].Body, "observed_at") {
		t.Errorf("update body = %.160s, want observed_at moved (fresh verification)", updates[0].Body)
	}
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(appends))
	}
	if refID := appends[0].Path[strings.LastIndex(appends[0].Path, "/")+1:]; refID == "" || !strings.Contains(updates[0].Body, `"current_ref":"`+refID+`"`) {
		t.Errorf("update body = %.160s, want current_ref pointing at the resurrection entry id %q", updates[0].Body, refID)
	}
}

// TestAddCorrectionOnObsoleteReportsResurrected (finding 129): a value
// correction over an obsolete fact also resurrects it (the script writes
// status=verified) — changed=true alone hides the status transition, so
// the result must name it.
func TestAddCorrectionOnObsoleteReportsResurrected(t *testing.T) {
	obsoleteSrc := `{"fact_id":"fid","key":"k","value":"v","category":"config","project":"proj","status":"obsolete","created_at":"2020-01-01T00:00:00Z","updated_at":"2020-01-01T00:00:00Z","observed_at":"2020-01-01T00:00:00Z"}`
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalRoutesOK(r); code != 500 {
			return code, body
		}
		switch {
		case r.Method == "GET":
			return 200, `{"_index":"velruna","_id":"k","found":true,"_source":` + obsoleteSrc + `}`
		case r.Method == "POST" && strings.Contains(r.Path, "_update"):
			return 200, `{"result":"updated"}`
		}
		return 500, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
	})
	res, err := c.Add(context.Background(), model.NewFact("k", "v-new", "config", "proj", "", ""))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !res.Changed || !res.Resurrected {
		t.Errorf("res = %+v, want changed and resurrected both true", res)
	}
	// A plain correction (verified fact) must NOT report Resurrected.
	c2, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalRoutesOK(r); code != 500 {
			return code, body
		}
		switch {
		case r.Method == "GET":
			return 200, `{"_index":"velruna","_id":"k","found":true,"_source":{"fact_id":"fid","key":"k","value":"v","category":"config","project":"proj","status":"verified","created_at":"2020-01-01T00:00:00Z","updated_at":"2020-01-01T00:00:00Z","observed_at":"2020-01-01T00:00:00Z"}}`
		case r.Method == "POST" && strings.Contains(r.Path, "_update"):
			return 200, `{"result":"updated"}`
		}
		return 500, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
	})
	res2, err := c2.Add(context.Background(), model.NewFact("k", "v-new", "config", "proj", "", ""))
	if err != nil {
		t.Fatalf("Add (verified): %v", err)
	}
	if !res2.Changed || res2.Resurrected {
		t.Errorf("res = %+v, want changed only (no resurrection on a live fact)", res2)
	}
}

// TestObsoleteMergesReasonRedactions pins the audit contract on the retire
// path: rows cut from the reason land on the fact next to the CURRENT value's
// rows (a retirement does not rewrite the value), a row of the same field is
// replaced, and the journal retire entry states the merged set.
func TestObsoleteMergesReasonRedactions(t *testing.T) {
	const src = `{"fact_id":"fid","key":"k","value":"id [velruna-redacted:long-hex]","category":"config","project":"proj","status":"verified","redactions":[{"field":"value","pattern":"long-hex","chars":48}]}`
	var body string
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == "GET":
			return 200, `{"_index":"velruna","_id":"k","found":true,"_source":` + src + `}`
		case strings.Contains(r.Path, "_update"):
			body = r.Body
			return 200, `{"result":"updated"}`
		}
		return 200, `{}`
	})
	rows := []model.Redaction{{Field: "reason", Pattern: "long-hex", Chars: 40}}
	if err := c.Obsolete(context.Background(), "k", "deleted [velruna-redacted:long-hex]", rows); err != nil {
		t.Fatalf("Obsolete: %v", err)
	}
	if !strings.Contains(body, `"field":"value","pattern":"long-hex","chars":48`) {
		t.Errorf("the value's audit row was dropped: %.260s", body)
	}
	if !strings.Contains(body, `"field":"reason"`) {
		t.Errorf("the reason's audit row is missing: %.260s", body)
	}
	appends := journalAppends(fake)
	if len(appends) != 1 || !strings.Contains(appends[0].Body, `"redactions"`) {
		t.Errorf("the retire entry must state the merged audit: %+v", appends)
	}
}

// TestTouchCleanNotesClearsStaleRow: a same-value re-add that rewrites the notes
// without a finding drops the stored notes row and keeps the value's — the note
// no longer contains what the stale row claims.
func TestTouchCleanNotesClearsStaleRow(t *testing.T) {
	const existing = `{"fact_id":"fid","key":"k","value":"same","category":"config","project":"proj","status":"verified","notes":"old [velruna-redacted:long-hex]","redactions":[{"field":"value","pattern":"long-hex","chars":48},{"field":"notes","pattern":"long-hex","chars":40}],"created_at":"2020-01-01T00:00:00Z","updated_at":"2020-01-01T00:00:00Z","observed_at":"2020-01-01T00:00:00Z"}`
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == "GET":
			return 200, `{"_index":"velruna","_id":"k","_version":2,"found":true,"_source":` + existing + `}`
		case strings.Contains(r.Path, "_update"):
			return 200, `{"result":"updated"}`
		}
		return 200, `{}`
	})
	f := model.NewFact("k", "same", "config", "proj", "", "checked by hand")
	if _, err := c.Add(context.Background(), f); err != nil {
		t.Fatalf("Add touch: %v", err)
	}
	updates := fake.requestsFor("POST", "/velruna/_update/k")
	if len(updates) != 1 {
		t.Fatalf("touch updates = %d, want 1", len(updates))
	}
	body := updates[0].Body
	if strings.Contains(body, `"field":"notes"`) {
		t.Errorf("stale notes row survived a clean note rewrite: %s", body)
	}
	if !strings.Contains(body, `"field":"value"`) {
		t.Errorf("the value's audit row was dropped: %s", body)
	}
}

// TestNewClientExplainsKeyringOutage: when the keyring itself is unusable, the
// "store one with creds set" advice sends the user into the same failure. The
// message has to name the outage instead.
func TestNewClientExplainsKeyringOutage(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.OpenSearchURL = "https://os.test:9200"
	cfg.KeyringError = "no usable backend: no session bus"
	_, err := NewClient(cfg)
	if err == nil {
		t.Fatal("NewClient accepted a config without a password")
	}
	if !strings.Contains(err.Error(), "keyring is unavailable") {
		t.Errorf("error %q does not name the keyring outage", err)
	}
	if strings.Contains(err.Error(), "store one with 'velruna creds set'") {
		t.Errorf("error %q advertises a store that cannot be written to", err)
	}
}
