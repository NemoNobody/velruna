package client

import (
	"context"
	"fmt"
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

// A keyword array is flattened by the store: a legacy referrer whose
// superseded_by list carries the key really does point there, so the guard
// must count it instead of explaining the document away as unparsable.
func TestIncomingLinksLegacyArrayReferrerBlocks(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"hub":   factJSON("hub", "verified", "2026-01-01T00:00:00Z", ""),
		"multi": `{"key":"multi","status":"verified","superseded_by":["hub","other"]}`,
	}}
	c, _ := supersedeClient(t, store)

	keys, total, _, err := c.incomingSupersedeRefs(context.Background(), "hub")
	if err != nil {
		t.Fatalf("incomingSupersedeRefs: %v", err)
	}
	if total != 1 || len(keys) != 1 || keys[0] != "multi" {
		t.Errorf("total=%d keys=%v, want 1 and [multi]", total, keys)
	}
	if err := c.refuseRetireWithIncoming(context.Background(), "hub"); err == nil ||
		!strings.Contains(err.Error(), "multi") {
		t.Errorf("the array referrer must block the retirement and be named: %v", err)
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

// Only verified referrers hold a target: a retired fact's edge is one
// readers already tolerate, and counting it would make the last edge of a
// chain unretirable forever (the documented repair could never finish).
func TestObsoleteIgnoresObsoleteReferrers(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"old":  factJSON("old", "verified", "2026-01-01T00:00:00Z", ""),
		"gone": factJSON("gone", "obsolete", "2026-02-01T00:00:00Z", "old"),
	}}
	c, _ := supersedeClient(t, store)
	if err := c.Obsolete(context.Background(), "old", "subject gone", nil); err != nil {
		t.Fatalf("an obsolete referrer must not block retirement: %v", err)
	}
}

// A fact that points at itself resolves itself on retirement: edge and
// target become obsolete together, so it must not deadlock the guard.
func TestObsoleteSelfLinkDoesNotBlock(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"a": factJSON("a", "verified", "2026-01-01T00:00:00Z", "a"),
	}}
	c, _ := supersedeClient(t, store)
	if err := c.Obsolete(context.Background(), "a", "subject gone", nil); err != nil {
		t.Fatalf("a self-link must not block retirement of its own fact: %v", err)
	}
}

// The advertised --limit maximum must work: a store rejects from+size
// beyond max_result_window, so the client clamps the window and judges
// truncation from the exact total instead.
func TestCheckLinksAtLimitMaxStaysUnderTheWindow(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"a": factJSON("a", "verified", "2026-01-01T00:00:00Z", "b"),
		"b": factJSON("b", "verified", "2026-01-01T00:00:00Z", ""),
	}}
	c, _ := supersedeClient(t, store)
	rep, err := c.CheckLinks(context.Background(), LinkScanMax)
	if err != nil {
		t.Fatalf("CheckLinks at --limit max (fake enforces the result window): %v", err)
	}
	if rep.Edges != 1 || rep.Truncated {
		t.Errorf("edges=%d truncated=%v, want 1/false", rep.Edges, rep.Truncated)
	}
}

// An edge the client cannot parse (e.g. a legacy multi-valued
// superseded_by) must surface as skipped and keep the walk honest about
// truncation — never silently shrink the count to look complete.
func TestCheckLinksUnreadableEdgeStaysHonest(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"hub": factJSON("hub", "verified", "2026-01-01T00:00:00Z", ""),
	}}
	for i := 0; i < 5; i++ {
		key := "ref-" + string(rune('a'+i))
		store.docs[key] = factJSON(key, "verified", "2026-02-01T00:00:00Z", "hub")
	}
	// The sixth edge is malformed: superseded_by is an array, unparseable
	// into a single successor.
	store.docs["multi"] = `{"key":"multi","status":"verified","superseded_by":["hub","other"]}`
	c, _ := supersedeClient(t, store)

	rep, err := c.CheckLinks(context.Background(), 5)
	if err != nil {
		t.Fatalf("CheckLinks: %v", err)
	}
	if rep.Total != 6 || rep.Skipped == 0 || !rep.Truncated {
		t.Errorf("total=%d skipped=%d truncated=%v, want 6/>0/true", rep.Total, rep.Skipped, rep.Truncated)
	}
}

// Cycle shapes: a loop rings only its cycle, a lollipop tail stays clean,
// and a self-loop is a cycle of one.
func TestCheckLinksCycleShapes(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"a": factJSON("a", "verified", "2026-01-01T00:00:00Z", "b"),
		"b": factJSON("b", "verified", "2026-01-01T00:00:00Z", "c"),
		"c": factJSON("c", "verified", "2026-01-01T00:00:00Z", "b"),
	}}
	c, _ := supersedeClient(t, store)
	rep, err := c.CheckLinks(context.Background(), 0)
	if err != nil {
		t.Fatalf("CheckLinks: %v", err)
	}
	cycled := map[string]bool{}
	for _, f := range rep.Findings {
		if f.Problem == "cycle" {
			cycled[f.Key] = true
		}
		if f.Key == "a" {
			t.Errorf("the tail of a lollipop was flagged: %+v", f)
		}
	}
	if !cycled["b"] || !cycled["c"] || len(cycled) != 2 {
		t.Errorf("cycled = %v, want exactly b and c", cycled)
	}

	self := &supersedeStore{docs: map[string]string{
		"s": factJSON("s", "verified", "2026-01-01T00:00:00Z", "s"),
	}}
	c2, _ := supersedeClient(t, self)
	rep2, err := c2.CheckLinks(context.Background(), 0)
	if err != nil {
		t.Fatalf("CheckLinks self-loop: %v", err)
	}
	if len(rep2.Findings) != 1 || rep2.Findings[0].Problem != "cycle" || rep2.Findings[0].Key != "s" {
		t.Errorf("self-loop findings = %+v, want one cycle on s", rep2.Findings)
	}
}

// More targets than one _mget batch: every batch must be issued and
// resolved, and exactly the missing target reported once.
func TestCheckLinksResolvesAcrossMGetBatches(t *testing.T) {
	store := &supersedeStore{docs: map[string]string{
		"missing": factJSON("missing", "verified", "2026-01-01T00:00:00Z", "nowhere"),
	}}
	for i := 0; i < 104; i++ {
		tkey := fmt.Sprintf("t%03d", i)
		store.docs[tkey] = factJSON(tkey, "verified", "2026-01-01T00:00:00Z", "")
		store.docs["ref-"+tkey] = factJSON("ref-"+tkey, "verified", "2026-02-01T00:00:00Z", tkey)
	}
	c, _ := supersedeClient(t, store)
	rep, err := c.CheckLinks(context.Background(), 0)
	if err != nil {
		t.Fatalf("CheckLinks across batches: %v", err)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Key != "missing" || rep.Findings[0].Problem != "dangling" {
		t.Fatalf("findings = %+v, want exactly the one dangling edge", rep.Findings)
	}
}

// A per-document _mget error is a store failure, never a fake dangling link.
func TestCheckLinksMGetPerDocErrorFailsTheCheck(t *testing.T) {
	store := &supersedeStore{
		docs: map[string]string{
			"a": factJSON("a", "verified", "2026-01-01T00:00:00Z", "b"),
			"b": factJSON("b", "verified", "2026-01-01T00:00:00Z", ""),
		},
		failIDs: map[string]string{"b": "shard unavailable"},
	}
	c, _ := supersedeClient(t, store)
	if _, err := c.CheckLinks(context.Background(), 0); err == nil {
		t.Fatal("a per-document probe error must fail the check")
	} else if strings.Contains(err.Error(), "dangling") {
		t.Errorf("the probe error must not be laundered into a finding: %v", err)
	}
}
