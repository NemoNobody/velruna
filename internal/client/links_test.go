package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// linkProbeOK answers the incoming-link probe (the only search the retire and
// patch write paths issue against the main index) with "nothing points here",
// so write-contract fakes do not have to hand-roll it. A 500 sentinel means
// "not the probe": the caller's own respond takes over.
func linkProbeOK(r recordedRequest) (int, string) {
	if r.Method == http.MethodPost && r.Path == "/velruna/_search" {
		return http.StatusOK, `{"took":1,"hits":{"total":{"value":0,"relation":"eq"},"hits":[]}}`
	}
	return http.StatusInternalServerError, ""
}

// Retiring a fact other records name as replaced leaves those readers walking
// into nothing, so the transition is refused while the edge exists.
func TestObsoleteRefusedWithIncomingLink(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"old": factJSON("old", "verified", "2026-01-01T00:00:00Z", ""),
		"new": factJSON("new", "verified", "2026-02-01T00:00:00Z", "old"),
	}}
	c, fake := supersedeClient(t, store)

	err := c.Obsolete(context.Background(), "old", "subject gone", nil)
	if err == nil {
		t.Fatal("retiring a pointed-at fact must be refused")
	}
	if !strings.Contains(err.Error(), `point at "old" with superseded_by`) || !strings.Contains(err.Error(), "new") {
		t.Errorf("refusal must name the referrers: %v", err)
	}
	if !strings.Contains(err.Error(), "links check") {
		t.Errorf("refusal should name the tool that maps the graph: %v", err)
	}
	if len(store.writes) != 0 {
		t.Errorf("the retirement must not have been written: %v", store.writes)
	}
	for _, r := range fake.requests {
		if r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/velruna/_update/") {
			t.Errorf("an update reached the store: %s", r.Path)
		}
	}
}

// After the referrer is repointed, the same retirement goes through.
func TestObsoleteAllowedAfterRepoint(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"old": factJSON("old", "verified", "2026-01-01T00:00:00Z", ""),
		"new": factJSON("new", "verified", "2026-02-01T00:00:00Z", ""),
	}}
	c, _ := supersedeClient(t, store)
	if err := c.Obsolete(context.Background(), "old", "subject gone", nil); err != nil {
		t.Fatalf("Obsolete with no incoming link: %v", err)
	}
	if len(store.writes) != 1 || store.writes[0] != "old" {
		t.Errorf("writes = %v, want the retirement of old", store.writes)
	}
}

// The same guard belongs to the patch path too: retiring through
// velruna_patch/`patch --status obsolete` must not bypass it.
func TestPatchObsoleteRefusedWithIncomingLink(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"old": factJSON("old", "verified", "2026-01-01T00:00:00Z", ""),
		"new": factJSON("new", "verified", "2026-02-01T00:00:00Z", "old"),
	}}
	c, _ := supersedeClient(t, store)

	if _, err := c.Patch(context.Background(), "old", PatchFields{Status: "obsolete", Reason: "gone"}); err == nil {
		t.Fatal("patch retire must honour the same guard")
	} else if !strings.Contains(err.Error(), "retire refused") {
		t.Errorf("patch refusal: %v", err)
	}
	if len(store.writes) != 0 {
		t.Errorf("patch wrote despite the guard: %v", store.writes)
	}
}

// A long fan-in is reported honestly: ten names, and the count of the rest.
func TestIncomingLinksReportIsBounded(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"hub": factJSON("hub", "verified", "2026-01-01T00:00:00Z", ""),
	}}
	for i := 0; i < 12; i++ {
		key := "ref-" + string(rune('a'+i))
		store.docs[key] = factJSON(key, "verified", "2026-02-01T00:00:00Z", "hub")
	}
	c, _ := supersedeClient(t, store)

	keys, total, truncated, err := c.incomingSupersedeRefs(context.Background(), "hub")
	if err != nil {
		t.Fatalf("incomingSupersedeRefs: %v", err)
	}
	if total != 12 || len(keys) != 10 || !truncated {
		t.Errorf("total=%d keys=%d truncated=%v, want 12/10/true", total, len(keys), truncated)
	}

	err = c.refuseRetireWithIncoming(context.Background(), "hub")
	if err == nil || !strings.Contains(err.Error(), "(+2 more)") {
		t.Errorf("the refusal must state how many it left out: %v", err)
	}

	// A fact nothing points at passes the guard.
	if err := c.refuseRetireWithIncoming(context.Background(), "nobody"); err != nil {
		t.Errorf("no incoming links: %v", err)
	}
}

// The guard is a store read: when the store fails, the retirement fails with
// the store error rather than a false "no links" verdict.
func TestIncomingLinksStoreFailureIsNotCleared(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodPost && r.Path == "/velruna/_search" {
			return http.StatusInternalServerError, `{"error":{"type":"cluster_exception"}}`
		}
		return http.StatusOK, `{"hits":{"total":{"value":0},"hits":[]}}`
	})
	if err := c.refuseRetireWithIncoming(context.Background(), "k"); err == nil {
		t.Fatal("a failed link probe must not clear the guard")
	}
}

// links check must surface every way an edge goes wrong, in one bounded walk:
// a missing target, a retired target, and a loop.
func TestCheckLinksReportsEveryBreak(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"a":       factJSON("a", "verified", "2026-01-01T00:00:00Z", "b"),
		"b":       factJSON("b", "verified", "2026-01-01T00:00:00Z", ""),
		"gone":    factJSON("gone", "verified", "2026-01-01T00:00:00Z", "ghost"),
		"dead":    factJSON("dead", "verified", "2026-01-01T00:00:00Z", "retired"),
		"retired": factJSON("retired", "obsolete", "2026-01-01T00:00:00Z", ""),
		"p":       factJSON("p", "verified", "2026-01-01T00:00:00Z", "q"),
		"q":       factJSON("q", "verified", "2026-01-01T00:00:00Z", "p"),
	}}
	c, _ := supersedeClient(t, store)

	rep, err := c.CheckLinks(context.Background(), 0)
	if err != nil {
		t.Fatalf("CheckLinks: %v", err)
	}
	if rep.Edges != 5 || rep.Truncated {
		t.Errorf("edges=%d truncated=%v, want 5/false", rep.Edges, rep.Truncated)
	}
	want := []struct{ key, problem string }{
		{"dead", "unverified"},
		{"gone", "dangling"},
		{"p", "cycle"},
		{"q", "cycle"},
	}
	if len(rep.Findings) != len(want) {
		t.Fatalf("findings = %+v, want %d", rep.Findings, len(want))
	}
	for i, f := range rep.Findings {
		if f.Key != want[i].key || f.Problem != want[i].problem {
			t.Errorf("finding %d = %s/%s, want %s/%s", i, f.Key, f.Problem, want[i].key, want[i].problem)
		}
	}
	// The clean edge names nothing.
	for _, f := range rep.Findings {
		if f.Key == "a" {
			t.Errorf("a verified link was reported: %+v", f)
		}
	}
}

// The scan is bounded: beyond --limit the report says it is truncated rather
// than pretending the graph was fully walked.
func TestCheckLinksTruncates(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"hub": factJSON("hub", "verified", "2026-01-01T00:00:00Z", ""),
	}}
	for i := 0; i < 12; i++ {
		key := "ref-" + string(rune('a'+i))
		store.docs[key] = factJSON(key, "verified", "2026-02-01T00:00:00Z", "hub")
	}
	c, _ := supersedeClient(t, store)

	rep, err := c.CheckLinks(context.Background(), 5)
	if err != nil {
		t.Fatalf("CheckLinks: %v", err)
	}
	if rep.Edges != 5 || !rep.Truncated || rep.Total != 12 {
		t.Errorf("edges=%d truncated=%v total=%d, want 5/true/12", rep.Edges, rep.Truncated, rep.Total)
	}
}

// A failed scan is an error, not an empty clean report.
func TestCheckLinksStoreFailureIsError(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodPost && r.Path == "/velruna/_search" {
			return http.StatusInternalServerError, `{"error":{"type":"cluster_exception"}}`
		}
		return http.StatusOK, `{}`
	})
	if _, err := c.CheckLinks(context.Background(), 0); err == nil {
		t.Fatal("a failed scan must not report a clean graph")
	}
}
