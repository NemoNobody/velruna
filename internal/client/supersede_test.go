package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/NemoNobody/velruna/internal/model"
)

// supersedeStore is a fake cluster holding a handful of fact documents by
// key, enough for the chain walk behind superseded_by (each hop is one
// document GET).
type supersedeStore struct {
	docs   map[string]string
	writes []string
	// failIDs makes linkMGet answer those ids with a per-document error
	// (shard unavailable), which the check must treat as a store failure.
	failIDs map[string]string
}

// respond serves the routes Patch touches: document GETs from docs, the
// scripted update (body captured), and neutral journal routes.
func (s *supersedeStore) respond(r recordedRequest) (int, string) {
	if code, body := journalRoutesOK(r); code != http.StatusInternalServerError {
		return code, body
	}
	const docPrefix = "/velruna/_doc/"
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.Path, docPrefix):
		key := strings.TrimPrefix(r.Path, docPrefix)
		src, ok := s.docs[key]
		if !ok {
			return http.StatusNotFound, `{"_index":"velruna","_id":"` + key + `","found":false}`
		}
		return http.StatusOK, `{"_index":"velruna","_id":"` + key + `","found":true,"_seq_no":1,"_primary_term":1,"_source":` + src + `}`
	case r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/velruna/_update/"):
		key := strings.TrimPrefix(r.Path, "/velruna/_update/")
		s.writes = append(s.writes, key)
		return http.StatusOK, `{"_index":"velruna","_id":"` + key + `","result":"updated"}`
	case r.Method == http.MethodPost && r.Path == "/velruna/_search":
		// The write path's incoming probe (term) and links check's full scan
		// (exists) are the only searches these fixtures serve.
		return s.linkSearch(r.Body)
	case r.Method == http.MethodPost && r.Path == "/velruna/_mget":
		return s.linkMGet(r.Body)
	}
	return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
}

// linkSearch answers the superseded_by searches: the retire guard's probe
// (bool filter on superseded_by + status, must_not the retiring key) and
// links check's exists scan. Clauses are evaluated against the stored
// documents; query shapes the client must not send fail as 400s, which
// keeps the outgoing queries themselves pinned.
func (s *supersedeStore) linkSearch(body string) (int, string) {
	var req struct {
		Query struct {
			Term   map[string]json.RawMessage `json:"term"`
			Exists map[string]string          `json:"exists"`
			Bool   struct {
				Filter  []linkClause `json:"filter"`
				MustNot []linkClause `json:"must_not"`
			} `json:"bool"`
		} `json:"query"`
		Size           int             `json:"size"`
		Sort           json.RawMessage `json:"sort"`
		TrackTotalHits bool            `json:"track_total_hits"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		return http.StatusBadRequest, `{"error":"bad probe body"}`
	}
	// The store's index.max_result_window: from+size beyond it is a raw 400.
	if req.Size > 10000 {
		return http.StatusBadRequest, `{"error":{"type":"search_phase_execution_exception","reason":"Result window is too large"}}`
	}
	var filters, mustNot []linkClause
	if len(req.Query.Term) > 0 || len(req.Query.Exists) > 0 {
		filters = append(filters, linkClause{Term: req.Query.Term, Exists: req.Query.Exists})
	}
	filters = append(filters, req.Query.Bool.Filter...)
	mustNot = append(mustNot, req.Query.Bool.MustNot...)

	scan := false
	for _, f := range filters {
		if f.Exists["field"] == "superseded_by" {
			scan = true
		}
	}
	if scan {
		// The scan contract: exclude empty links (they eat scan slots and
		// blur truncation), ask for exact totals, and pin a sort so the
		// truncated window is reproducible.
		filtered := false
		for _, m := range mustNot {
			if v, ok := m.Term["superseded_by"]; ok && string(v) == `""` {
				filtered = true
			}
		}
		if !filtered {
			return http.StatusBadRequest, `{"error":"scan must exclude empty superseded_by links"}`
		}
		if !req.TrackTotalHits {
			return http.StatusBadRequest, `{"error":"scan must pin the total"}`
		}
		if len(req.Sort) == 0 {
			return http.StatusBadRequest, `{"error":"scan must sort deterministically"}`
		}
	} else {
		// The retire guard's probe states its total as fact in the refusal,
		// so it must pin the total exactly like the scan does.
		guardProbe := false
		for _, f := range filters {
			if _, ok := f.Term["superseded_by"]; ok {
				guardProbe = true
			}
		}
		if guardProbe && !req.TrackTotalHits {
			return http.StatusBadRequest, `{"error":"link probe must pin the total"}`
		}
	}

	var matched []string
	for key := range s.docs {
		var view struct {
			Key          string          `json:"key"`
			Status       string          `json:"status"`
			SupersededBy json.RawMessage `json:"superseded_by"`
		}
		if err := json.Unmarshal([]byte(s.docs[key]), &view); err != nil {
			continue
		}
		match := true
		for _, f := range filters {
			if !matchClause(f, key, view.Status, view.SupersededBy) {
				match = false
				break
			}
		}
		if match {
			for _, m := range mustNot {
				if matchClause(m, key, view.Status, view.SupersededBy) {
					match = false
					break
				}
			}
		}
		if match {
			matched = append(matched, key)
		}
	}
	// The scan pins key-asc; the fake must hand out the SAME window the
	// store would, or a test pinning window membership would flake
	// against map-iteration order.
	sort.Strings(matched)
	var hits []string
	for _, key := range matched {
		if req.Size > 0 && len(hits) >= req.Size {
			continue
		}
		var view struct {
			SupersededBy json.RawMessage `json:"superseded_by"`
		}
		_ = json.Unmarshal([]byte(s.docs[key]), &view)
		q, _ := json.Marshal(key)
		source := `{"key":` + string(q)
		if len(view.SupersededBy) > 0 {
			source += `,"superseded_by":` + string(view.SupersededBy)
		}
		source += `}`
		hits = append(hits, `{"_source":`+source+`}`)
	}
	return http.StatusOK, `{"hits":{"total":{"value":` + fmt.Sprint(len(matched)) + `},"hits":[` + strings.Join(hits, ",") + `]}}`
}

// linkClause is one leaf query the fake understands: a term on the indexed
// keyword fields or an exists on superseded_by.
type linkClause struct {
	Term   map[string]json.RawMessage `json:"term"`
	Exists map[string]string          `json:"exists"`
}

func matchClause(cl linkClause, key, status string, superseded json.RawMessage) bool {
	for field, rawVal := range cl.Term {
		var want string
		if err := json.Unmarshal(rawVal, &want); err != nil {
			return false
		}
		switch field {
		case "key":
			if key != want {
				return false
			}
		case "status":
			if status != want {
				return false
			}
		case "superseded_by":
			if !termMatches(superseded, want) {
				return false
			}
		default:
			return false
		}
	}
	for _, field := range cl.Exists {
		switch field {
		case "superseded_by":
			// An empty array indexes no values, so `exists` matches nothing
			// (same as a missing or null field in the real store).
			if len(superseded) == 0 || string(superseded) == "null" || string(superseded) == "[]" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// termMatches mirrors how the real store flattens keyword arrays: a term
// matches a scalar exactly, or an array when ANY element equals the value. A
// legacy multi-valued superseded_by therefore does NOT hide the referrer from
// the retire guard, and an array carrying "" really is excluded by must_not
// term superseded_by:"".
func termMatches(raw json.RawMessage, want string) bool {
	if string(raw) == "null" {
		// A null indexes no values: no term matches it.
		return false
	}
	var scalar string
	if err := json.Unmarshal(raw, &scalar); err == nil {
		return scalar == want
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, v := range list {
			if v == want {
				return true
			}
		}
	}
	return false
}

// linkMGet answers links check's target probes with found + status from the
// stored documents.
func (s *supersedeStore) linkMGet(body string) (int, string) {
	var req struct {
		Docs []struct {
			ID string `json:"_id"`
		} `json:"docs"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		return http.StatusBadRequest, `{"error":"bad mget body"}`
	}
	var docs []string
	for _, d := range req.Docs {
		if reason, bad := s.failIDs[d.ID]; bad {
			docs = append(docs, `{"_index":"velruna","_id":"`+d.ID+`","error":{"type":"unavailable_shards_exception","reason":"`+reason+`"}}`)
			continue
		}
		src, ok := s.docs[d.ID]
		if !ok {
			docs = append(docs, `{"_index":"velruna","_id":"`+d.ID+`","found":false}`)
			continue
		}
		var fact struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(src), &fact); err != nil {
			docs = append(docs, `{"_index":"velruna","_id":"`+d.ID+`","found":false}`)
			continue
		}
		docs = append(docs, `{"_index":"velruna","_id":"`+d.ID+`","found":true,"_source":{"status":"`+fact.Status+`"}}`)
	}
	return http.StatusOK, `{"docs":[` + strings.Join(docs, ",") + `]}`
}

// factJSON renders a stored fact document. created is the creation date the
// backward-link check compares; an empty superseded_by is omitted.
func factJSON(key, status, created, supersededBy string) string {
	doc := map[string]any{
		"fact_id":     "id-" + key,
		"key":         key,
		"value":       "v-" + key,
		"category":    "location",
		"project":     "p",
		"status":      status,
		"source":      "tool:test",
		"created_at":  created,
		"updated_at":  created,
		"observed_at": created,
	}
	if supersededBy != "" {
		doc["superseded_by"] = supersededBy
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// supersedeClient builds a fake client over the given documents.
func supersedeClient(t *testing.T, store *supersedeStore) (*Client, *fakeOpenSearch) {
	t.Helper()
	return newFakeClient(t, store.respond)
}

func TestPatchSupersededByJournalsLink(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"k":   factJSON("k", "verified", "2026-01-01T00:00:00Z", ""),
		"new": factJSON("new", "verified", "2026-02-01T00:00:00Z", ""),
	}}
	c, fake := supersedeClient(t, store)

	res, err := c.Patch(context.Background(), "k", PatchFields{SupersededBy: "new"})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if !res.Changed {
		t.Error("Changed = false for a supersede link; it is a journaled event")
	}
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(appends))
	}
	var je map[string]any
	if err := json.Unmarshal([]byte(appends[0].Body), &je); err != nil {
		t.Fatalf("journal body: %v", err)
	}
	if je["op"] != model.OpSupersede {
		t.Errorf("journal op = %v, want %q (a link is not a value write)", je["op"], model.OpSupersede)
	}
	if je["superseded_by"] != "new" {
		t.Errorf("journal superseded_by = %v, want new", je["superseded_by"])
	}
	if _, marked := je["marker"]; marked {
		t.Errorf("marker = %v on a forward link; only a backward link is flagged", je["marker"])
	}
	params := patchParams(t, updateBody(fake))
	if params["superseded_by"] != "new" {
		t.Errorf("params.superseded_by = %v, want new", params["superseded_by"])
	}
	// A link states a relation, not a re-observation: age must not restart.
	if v, ok := params["observed_at"]; !ok || v != nil {
		t.Errorf("observed_at = %v, want untouched (a link is not a re-verification)", v)
	}
	if v, ok := params["current_ref"]; !ok || v == nil {
		t.Errorf("current_ref = %v, want the new entry id (the entry is the current state)", v)
	}
}

func TestPatchSupersededByRejectsBadTargets(t *testing.T) {
	base := func() *supersedeStore {
		return &supersedeStore{docs: map[string]string{
			"k":     factJSON("k", "verified", "2026-01-01T00:00:00Z", ""),
			"new":   factJSON("new", "verified", "2026-02-01T00:00:00Z", ""),
			"other": factJSON("other", "verified", "2026-02-01T00:00:00Z", ""),
			"dead":  factJSON("dead", "obsolete", "2026-02-01T00:00:00Z", ""),
		}}
	}
	tests := []struct {
		name   string
		mutate func(*supersedeStore)
		target string
		want   string
	}{
		{"self reference", func(*supersedeStore) {}, "k", "by itself"},
		{"missing target", func(*supersedeStore) {}, "ghost", "does not exist"},
		{"obsolete target", func(*supersedeStore) {}, "dead", "is obsolete"},
		{"cycle", func(s *supersedeStore) {
			s.docs["new"] = factJSON("new", "verified", "2026-02-01T00:00:00Z", "k")
		}, "new", "cycle"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := base()
			tc.mutate(store)
			c, fake := supersedeClient(t, store)
			_, err := c.Patch(context.Background(), "k", PatchFields{SupersededBy: tc.target})
			if err == nil {
				t.Fatalf("Patch accepted a bad link (target %q)", tc.target)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
			if len(store.writes) != 0 {
				t.Errorf("store writes = %v, want none (refused before the update)", store.writes)
			}
			if len(journalAppends(fake)) != 0 {
				t.Error("a refused link must not journal")
			}
		})
	}
}

func TestPatchSupersededByRepoints(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"k":     factJSON("k", "verified", "2026-01-01T00:00:00Z", "new"),
		"new":   factJSON("new", "verified", "2026-02-01T00:00:00Z", ""),
		"other": factJSON("other", "verified", "2026-02-01T00:00:00Z", ""),
	}}
	c, fake := supersedeClient(t, store)

	res, err := c.Patch(context.Background(), "k", PatchFields{SupersededBy: "other"})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if !res.Changed {
		t.Error("Changed = false for a repoint; every link event is journaled")
	}
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(appends))
	}
	var je map[string]any
	if err := json.Unmarshal([]byte(appends[0].Body), &je); err != nil {
		t.Fatalf("journal body: %v", err)
	}
	if je["op"] != model.OpSupersede || je["superseded_by"] != "other" {
		t.Errorf("journal op/superseded_by = %v/%v, want %q/other", je["op"], je["superseded_by"], model.OpSupersede)
	}
	params := patchParams(t, updateBody(fake))
	if params["superseded_by"] != "other" {
		t.Errorf("params.superseded_by = %v, want other (the link is replaced)", params["superseded_by"])
	}
}

func TestPatchSupersededByReverifyRefused(t *testing.T) {
	err := ValidatePatchFields(PatchFields{Reverify: true, SupersededBy: "new"})
	if err == nil {
		t.Fatal("ValidatePatchFields accepted reverify + superseded_by")
	}
	if !strings.Contains(err.Error(), "reverify") {
		t.Errorf("error = %v, want it to name reverify", err)
	}
}

func TestPatchSupersededByBackwardLinkIsMarked(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"newer": factJSON("newer", "verified", "2026-02-01T00:00:00Z", ""),
		"older": factJSON("older", "verified", "2026-01-01T00:00:00Z", ""),
	}}
	c, fake := supersedeClient(t, store)

	if _, err := c.Patch(context.Background(), "newer", PatchFields{SupersededBy: "older"}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(appends))
	}
	var je map[string]any
	if err := json.Unmarshal([]byte(appends[0].Body), &je); err != nil {
		t.Fatalf("journal body: %v", err)
	}
	if je["marker"] != model.MarkerNewerWithOlder {
		t.Errorf("marker = %v, want %q (a newer fact declaring an older successor)", je["marker"], model.MarkerNewerWithOlder)
	}
}

func TestPatchSupersededBySameValueIsTouch(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"k":   factJSON("k", "verified", "2026-01-01T00:00:00Z", "new"),
		"new": factJSON("new", "verified", "2026-02-01T00:00:00Z", ""),
	}}
	c, fake := supersedeClient(t, store)

	res, err := c.Patch(context.Background(), "k", PatchFields{SupersededBy: "new"})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if res.Changed {
		t.Error("Changed = true for a link that was already set; it is a touch")
	}
	if len(journalAppends(fake)) != 0 {
		t.Error("a same-link touch must not journal")
	}
	params := patchParams(t, updateBody(fake))
	if v, ok := params["superseded_by"]; !ok || v != nil {
		t.Errorf("params.superseded_by = %v, want null (untouched)", v)
	}
}

func TestPatchValueChangeCarriesExistingLink(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"k":   factJSON("k", "verified", "2026-01-01T00:00:00Z", "new"),
		"new": factJSON("new", "verified", "2026-02-01T00:00:00Z", ""),
	}}
	c, fake := supersedeClient(t, store)

	if _, err := c.Patch(context.Background(), "k", PatchFields{Value: "v2"}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(appends))
	}
	var je map[string]any
	if err := json.Unmarshal([]byte(appends[0].Body), &je); err != nil {
		t.Fatalf("journal body: %v", err)
	}
	if je["op"] != model.OpWrite {
		t.Errorf("journal op = %v, want %q (the value moved)", je["op"], model.OpWrite)
	}
	if je["superseded_by"] != "new" {
		t.Errorf("journal superseded_by = %v, want the link the fact still carries", je["superseded_by"])
	}
}

func TestPatchSupersededByAndValueChangeJournalsWrite(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"k":   factJSON("k", "verified", "2026-01-01T00:00:00Z", ""),
		"new": factJSON("new", "verified", "2026-02-01T00:00:00Z", ""),
	}}
	c, fake := supersedeClient(t, store)

	if _, err := c.Patch(context.Background(), "k", PatchFields{Value: "v2", SupersededBy: "new"}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1 (one event)", len(appends))
	}
	var je map[string]any
	if err := json.Unmarshal([]byte(appends[0].Body), &je); err != nil {
		t.Fatalf("journal body: %v", err)
	}
	if je["op"] != model.OpWrite {
		t.Errorf("journal op = %v, want %q (a value change outranks the link)", je["op"], model.OpWrite)
	}
	if je["superseded_by"] != "new" {
		t.Errorf("journal superseded_by = %v, want new", je["superseded_by"])
	}
}

// updateBody returns the body of the single scripted update the fake recorded.
func updateBody(fake *fakeOpenSearch) string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for i := len(fake.requests) - 1; i >= 0; i-- {
		r := fake.requests[i]
		if r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/velruna/_update/") {
			return r.Body
		}
	}
	return ""
}
