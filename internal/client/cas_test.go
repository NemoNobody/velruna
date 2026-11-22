package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/NemoNobody/velruna/internal/model"
	"github.com/opensearch-project/opensearch-go/v4"
)

// CAS contract under test: snapshot-derived writes (the touch, the
// resurrect, the retirement, the status flip) carry the document version
// pair read with the fact into the update (if_seq_no/if_primary_term), so a
// concurrent write that landed in between fails as ErrConflict instead of
// silently clobbering it. Value-change writes stay unconditional by design
// (both writers journal; the final current_ref stays consistent) — the
// negative test below pins that boundary so a future "fix" cannot quietly
// flip the contract.

const casSource = `{"key":"k","value":"same","category":"config","project":"p","status":"verified"}`

const casConflictBody = `{"error":{"root_cause":[{"type":"version_conflict_engine_exception",` +
	`"reason":"[k]: version conflict, required seq_no [5], primary_term [2], but was [6] [2]",` +
	`"index":"velruna","shard":"0","document_id":"k"}],` +
	`"type":"version_conflict_engine_exception",` +
	`"reason":"[k]: version conflict, required seq_no [5], primary_term [2], but was [6] [2]"},` +
	`"status":409}`

// casRespond serves the fact GET with an envelope that carries the version
// pair, and the main-doc _update with a caller-chosen outcome. The update
// request (query params included) is captured in *got.
func casRespond(source string, updateStatus int, updateBody string, got *recordedRequest) func(recordedRequest) (int, string) {
	return func(r recordedRequest) (int, string) {
		if code, body := journalRoutesOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		if code, body := linkProbeOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/velruna/_doc/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","_version":3,"_seq_no":5,"_primary_term":2,"found":true,"_source":` + source + `}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			*got = r
			return updateStatus, updateBody
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
	}
}

func assertCasParams(t *testing.T, got recordedRequest) {
	t.Helper()
	if q := got.Query.Get("if_seq_no"); q != "5" {
		t.Errorf("if_seq_no = %q, want 5 (the version pair from the snapshot read)", q)
	}
	if q := got.Query.Get("if_primary_term"); q != "2" {
		t.Errorf("if_primary_term = %q, want 2", q)
	}
}

func TestAddSameValueTouchIsConditional(t *testing.T) {
	var got recordedRequest
	c, _ := newFakeClient(t, casRespond(casSource, http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`, &got))

	if _, err := c.Add(context.Background(), model.NewFact("k", "same", "config", "p", "test:cas", "retouched")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	assertCasParams(t, got)
}

func TestAddTouchConflictIsErrConflict(t *testing.T) {
	var got recordedRequest
	c, _ := newFakeClient(t, casRespond(casSource, http.StatusConflict, casConflictBody, &got))

	_, err := c.Add(context.Background(), model.NewFact("k", "same", "config", "p", "test:cas", "retouched"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "re-read and retry") {
		t.Errorf("the conflict must state the remedy, got: %v", err)
	}
	if !strings.Contains(err.Error(), "changed under this write") {
		t.Errorf("the conflict must name what happened, got: %v", err)
	}
}

func TestAddResurrectIsConditional(t *testing.T) {
	var got recordedRequest
	c, _ := newFakeClient(t, casRespond(
		`{"key":"k","value":"same","category":"config","project":"p","status":"obsolete"}`,
		http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`, &got))

	res, err := c.Add(context.Background(), model.NewFact("k", "same", "config", "p", "test:cas", ""))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !res.Resurrected {
		t.Fatal("resurrected = false, want true")
	}
	assertCasParams(t, got)
}

func TestObsoleteTransitionIsConditional(t *testing.T) {
	var got recordedRequest
	c, _ := newFakeClient(t, casRespond(casSource, http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`, &got))

	if err := c.Obsolete(context.Background(), "k", "file deleted", nil); err != nil {
		t.Fatalf("Obsolete: %v", err)
	}
	assertCasParams(t, got)
}

func TestPatchStatusFlipIsConditional(t *testing.T) {
	var got recordedRequest
	c, _ := newFakeClient(t, casRespond(casSource, http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`, &got))

	if _, err := c.Patch(context.Background(), "k", PatchFields{Status: "obsolete", Reason: "file deleted"}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	assertCasParams(t, got)
}

func TestPatchStatusFlipConflictIsErrConflict(t *testing.T) {
	var got recordedRequest
	c, _ := newFakeClient(t, casRespond(casSource, http.StatusConflict, casConflictBody, &got))

	_, err := c.Patch(context.Background(), "k", PatchFields{Status: "obsolete", Reason: "file deleted"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "re-read and retry") {
		t.Errorf("the conflict must state the remedy, got: %v", err)
	}
}

// TestAddCreateLinkIsConditional pins the create path's last snapshot-derived
// write: the birth link, conditioned on the version pair the create response
// itself reported (re-reading at link time would NOT close the window — a
// correction landing after the create would already be in the snapshot).
// A lost race must surface as ErrConflict, not silently overwrite the
// winner's current_ref with the birth ref (prune --keep could then delete
// the entry that carries the live value).
func TestAddCreateLinkIsConditional(t *testing.T) {
	var got recordedRequest
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalRoutesOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/velruna/_doc/k":
			return http.StatusNotFound, `{"error":{"type":"document_missing_exception","reason":"Document with id k not found.","index":"velruna"},"status":404}`
		case r.Method == http.MethodPut && r.Path == "/velruna/_doc/k":
			return http.StatusCreated, `{"_index":"velruna","_id":"k","_version":1,"_seq_no":4,"_primary_term":2,"result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			got = r
			return http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
	})

	res, err := c.Add(context.Background(), model.NewFact("k", "v", "config", "p", "test:cas", ""))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !res.Created {
		t.Fatal("created = false, want true")
	}
	if q := got.Query.Get("if_seq_no"); q != "4" {
		t.Errorf("the birth link must be CAS-conditioned on the create response, if_seq_no = %q, want 4", q)
	}
	if q := got.Query.Get("if_primary_term"); q != "2" {
		t.Errorf("the birth link must be CAS-conditioned on the create response, if_primary_term = %q, want 2", q)
	}
}

// TestAddTouchConflictStringShapedBody: a middlebox that rewrites the 409
// body into the client's string-shaped error must still surface ErrConflict
// (the gate names collisions by wording; the guidance contract survives).
func TestAddTouchConflictStringShapedBody(t *testing.T) {
	var got recordedRequest
	c, _ := newFakeClient(t, casRespond(casSource, http.StatusConflict,
		`{"error":"[k]: version conflict, required seq_no [5], primary_term [2], but was [6] [2]","status":409}`, &got))

	_, err := c.Add(context.Background(), model.NewFact("k", "same", "config", "p", "test:cas", "retouched"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict (string-shaped collision body must classify as conflict)", err)
	}
	if !strings.Contains(err.Error(), "re-read and retry") {
		t.Errorf("the conflict must state the remedy, got: %v", err)
	}
}

// TestAddCanonicalRaceTypeIsNeverDocConflict: the struct type guard
// (type != resource_already_exists_exception) excludes the INDEX-level
// race from the reason fallback everywhere, not just at the append
// success site — even wording that reads like a CAS collision must not
// classify as ErrConflict when the engine names the index race. This
// pins the guard at a CAS site: deleting it turns this body into a
// fake "re-read and retry" conflict on every fixture that only checks
// "some error", so only this test distinguishes the two contracts.
func TestAddCanonicalRaceTypeIsNeverDocConflict(t *testing.T) {
	var got recordedRequest
	c, _ := newFakeClient(t, casRespond(casSource, http.StatusConflict,
		`{"error":{"type":"resource_already_exists_exception","reason":"[k]: version conflict, required seq_no [5], primary_term [2], but was [6] [2]"},"status":409}`, &got))

	_, err := c.Add(context.Background(), model.NewFact("k", "same", "config", "p", "test:cas", "retouched"))
	if errors.Is(err, ErrConflict) {
		t.Fatalf("an index-level race must not masquerade as a doc CAS conflict, got: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "resource_already_exists_exception") {
		t.Fatalf("the race must surface honestly as the cluster error it is, got: %v", err)
	}
}

// TestAddSub409WordingIsNotConflict: the status gates are part of the
// contract — a 400 whose reason happens to carry collision wording is a
// validation failure, not a doc CAS collision. Without the gates this
// body would masquerade as ErrConflict ("re-read and retry") and send
// the agent on a futile loop against a permanent validation error.
func TestAddSub409WordingIsNotConflict(t *testing.T) {
	var got recordedRequest
	c, _ := newFakeClient(t, casRespond(casSource, http.StatusBadRequest,
		`{"error":{"type":"mapper_parsing_exception","reason":"document already exists"},"status":400}`, &got))

	_, err := c.Add(context.Background(), model.NewFact("k", "same", "config", "p", "test:cas", "retouched"))
	if errors.Is(err, ErrConflict) {
		t.Fatalf("a 400 must not classify as a CAS conflict, got: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "mapper_parsing_exception") {
		t.Fatalf("the validation failure must surface honestly, got: %v", err)
	}
}

// TestAddConflictCanonicalTypeRewrittenReason: the canonical type
// disjunct must answer on its own — a proxy that keeps the engine's
// version_conflict_engine_exception type but rewrites the reason away
// from the fallback wording is still a CAS conflict, not a plain
// cluster error.
func TestAddConflictCanonicalTypeRewrittenReason(t *testing.T) {
	var got recordedRequest
	c, _ := newFakeClient(t, casRespond(casSource, http.StatusConflict,
		`{"error":{"type":"version_conflict_engine_exception","reason":"document version mismatch"},"status":409}`, &got))

	_, err := c.Add(context.Background(), model.NewFact("k", "same", "config", "p", "test:cas", "retouched"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict (canonical type must classify without fallback wording)", err)
	}
}

// TestAddGatewayStatusRewriteIsNotConflict: even the canonical
// version_conflict_engine_exception type requires the 409 status — a
// gateway that rewrites the status to 500 while keeping the engine's
// type body is not an engine conflict. Trusting it would answer a
// permanent upstream error with "re-read and retry" and send the agent
// into a futile CAS loop.
func TestAddGatewayStatusRewriteIsNotConflict(t *testing.T) {
	var got recordedRequest
	c, _ := newFakeClient(t, casRespond(casSource, http.StatusInternalServerError,
		`{"error":{"type":"version_conflict_engine_exception","reason":"[k]: version conflict, required seq_no [5], primary_term [2], but was [6] [2]"},"status":500}`, &got))

	_, err := c.Add(context.Background(), model.NewFact("k", "same", "config", "p", "test:cas", "retouched"))
	if errors.Is(err, ErrConflict) {
		t.Fatalf("a 500-carrying body must not classify as a CAS conflict, got: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "version_conflict_engine_exception") {
		t.Fatalf("the upstream failure must surface honestly, got: %v", err)
	}
}

// TestIsVersionConflictDelegatesBothShapes pins the shape wiring itself:
// both error shapes must flow through the shared conflictAnswer gate,
// so a future edit that re-inlines a diverging predicate into either
// branch fails HERE, not only at some call-site fixture. The table is
// built from the same bodies as the call-site pins.
func TestIsVersionConflictDelegatesBothShapes(t *testing.T) {
	raceRC := `{"error":{"type":"version_conflict_engine_exception","reason":"[k2] already exists","root_cause":[{"type":"resource_already_exists_exception","reason":"index [velruna-history-2026-11/uuid] already exists"}]},"status":409}`
	mkStruct := func(body string) *opensearch.StructError {
		var ste opensearch.StructError
		if err := json.Unmarshal([]byte(body), &ste); err != nil {
			t.Fatalf("fixture %s: %v", body, err)
		}
		return &ste
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"struct canonical conflict", mkStruct(casConflictBody), true},
		{"struct doc-create conflict", mkStruct(`{"error":{"type":"version_conflict_engine_exception","reason":"[k]: version conflict, document already exists (current version [3])"},"status":409}`), true},
		{"struct race type", mkStruct(`{"error":{"type":"resource_already_exists_exception","reason":"index [velruna-history-2026-11/uuid] already exists"},"status":409}`), false},
		{"struct race in root_cause", mkStruct(raceRC), false},
		{"struct race in caused_by chain", mkStruct(`{"error":{"type":"mapper_parsing_exception","reason":"[k2] already exists","caused_by":{"type":"engine_exception","reason":"outer","caused_by":{"type":"resource_already_exists_exception","reason":"index detail"}}},"status":409}`), false},
		{"struct race marker pair in root_cause only", mkStruct(`{"error":{"type":"mapper_parsing_exception","reason":"[k2] already exists","root_cause":[{"type":"engine_exception","reason":"index [velruna-history-2026-11/uuid] already exists"}]},"status":409}`), false},
		{"struct status rewritten to 500", mkStruct(`{"error":{"type":"version_conflict_engine_exception","reason":"[k]: version conflict, required seq_no [5]"},"status":500}`), false},
		{"struct sub-409 wording", mkStruct(`{"error":{"type":"mapper_parsing_exception","reason":"document already exists"},"status":400}`), false},
		{"string conflict", &opensearch.StringError{Err: "document already exists", Status: http.StatusConflict}, true},
		{"string race marker", &opensearch.StringError{Err: "index [velruna-history-2026-11/uuid] already exists", Status: http.StatusConflict}, false},
		{"string race type name", &opensearch.StringError{Err: "resource_already_exists_exception: [k2] already exists", Status: http.StatusConflict}, false},
		{"string sub-409", &opensearch.StringError{Err: "document already exists", Status: http.StatusBadRequest}, false},
	} {
		if got := isVersionConflict(tc.err); got != tc.want {
			t.Errorf("%s: isVersionConflict = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestGatewayHTTPErrorWithConflictBodyIsConflict documents the OTHER
// half of the status contract: for classified error bodies
// opensearch-go takes the status from the body (only its final
// unclassified fallback uses the HTTP code), so when the gateway
// rewrites HTTP 409→500 but passes a genuine conflict body through,
// the body's declared 409 wins and the write fails as a named
// conflict — honest "re-read and retry" beats losing the
// conflict signal entirely (which would fake append success).
func TestGatewayHTTPErrorWithConflictBodyIsConflict(t *testing.T) {
	var got recordedRequest
	c, _ := newFakeClient(t, casRespond(casSource, http.StatusInternalServerError, casConflictBody, &got))

	_, err := c.Add(context.Background(), model.NewFact("k", "same", "config", "p", "test:cas", "retouched"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a genuine conflict body under a rewritten HTTP code must stay a conflict, got: %v", err)
	}
}

func TestPatchValueChangeIsNotConditional(t *testing.T) {
	var got recordedRequest
	c, _ := newFakeClient(t, casRespond(casSource, http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`, &got))
	if _, err := c.Patch(context.Background(), "k", PatchFields{Value: "corrected", Summary: "the corrected value"}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	// Deliberate design: a correction journals its own snapshot and wins
	// last; conditioning it would refuse legitimate corrections to a doc a
	// touch just moved. The boundary must stay where it is.
	if q := got.Query.Get("if_seq_no"); q != "" {
		t.Errorf("a value correction must not be CAS-conditioned (last-write-wins by design), got if_seq_no=%q", q)
	}
	if q := got.Query.Get("if_primary_term"); q != "" {
		t.Errorf("a value correction must not be CAS-conditioned, got if_primary_term=%q", q)
	}
}
