package client

import (
	"context"
	"encoding/json"
	"net/http"
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
	}
	return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
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
