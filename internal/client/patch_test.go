package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/model"
)

// patchRespond serves the routes Patch touches: doc read, journal routes
// (infra probes + append), and the scripted main-doc update (body captured).
func patchRespond(source string, updateBody *string) func(recordedRequest) (int, string) {
	return func(r recordedRequest) (int, string) {
		if code, body := journalRoutesOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		if code, body := linkProbeOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/velruna/_doc/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","found":true,"_source":` + source + `}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			*updateBody = r.Body
			return http.StatusOK, `{"_index":"velruna","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
	}
}

// patchParams digs params out of the captured _update body (scripted form).
func patchParams(t *testing.T, body string) map[string]any {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("update body is not JSON: %v (%s)", err, body)
	}
	script, ok := req["script"].(map[string]any)
	if !ok {
		t.Fatalf("update body has no script (patch must be scripted): %s", body)
	}
	params, ok := script["params"].(map[string]any)
	if !ok {
		t.Fatalf("script has no params: %s", body)
	}
	return params
}

// TestPatchNotesAppendIsTouch pins the cheap-update contract: a dated note
// appends atomically (inside the script, so a concurrent add cannot drop it),
// touches the fact (Changed=false), and writes NO journal entry — the journal
// records accepted values, and the value did not move.
func TestPatchNotesAppendIsTouch(t *testing.T) {
	var body string
	c, fake := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"verified","notes":"old"}`,
		&body))

	res, err := c.Patch(context.Background(), "k", PatchFields{NotesAppend: "checked in prod"})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if res.Changed {
		t.Error("Changed = true for a notes-only patch; it is a touch")
	}
	params := patchParams(t, body)
	// The dated line is stamped by the server clock: pin the shape
	// ("YYYY-MM-DD: text"), not the exact day (a run across midnight must
	// not fail for a legal timestamp).
	got, _ := params["notes_append"].(string)
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}: checked in prod$`).MatchString(got) {
		t.Errorf("notes_append = %q, want the dated line", got)
	}
	if v, ok := params["value"]; !ok || v != nil {
		t.Errorf("value must be null (untouched), got %v", v)
	}
	if v, ok := params["notes"]; !ok || v != nil {
		t.Errorf("notes must be null (the script appends), got %v", v)
	}
	if fake.sawAnyPrefix("/velruna-history-") {
		t.Error("a notes-only patch must not write the journal")
	}
}

// TestPatchValueChangeJournalsAndClearsSummary pins the correction path:
// the new value is journaled, observed_at and current_ref move, and the old
// summary is CLEARED (a table of contents for the old value is a lie).
func TestPatchValueChangeJournalsAndClearsSummary(t *testing.T) {
	var body string
	c, fake := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v1","summary":"old contents","category":"config","project":"p","status":"verified"}`,
		&body))

	res, err := c.Patch(context.Background(), "k", PatchFields{Value: "v2"})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if !res.Changed {
		t.Error("Changed = false for a value change")
	}
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(appends))
	}
	var je map[string]any
	if err := json.Unmarshal([]byte(appends[0].Body), &je); err != nil {
		t.Fatalf("journal body: %v", err)
	}
	if je["value"] != "v2" {
		t.Errorf("journal value = %v, want v2 (the journal records accepted state)", je["value"])
	}
	params := patchParams(t, body)
	if params["value"] != "v2" {
		t.Errorf("params.value = %v, want v2", params["value"])
	}
	if s, ok := params["summary"]; !ok || s != "" {
		t.Errorf("params.summary = %v, want \"\" (the old summary must be cleared with the value)", params["summary"])
	}
	if v, ok := params["observed_at"]; !ok || v == nil {
		t.Error("params.observed_at must move on a value change")
	}
	if refID := appends[0].Path[strings.LastIndex(appends[0].Path, "/")+1:]; refID == "" || params["current_ref"] != refID {
		t.Errorf("current_ref = %v, want the preallocated entry id %q", params["current_ref"], refID)
	}
}

// TestPatchValueChangeCarriesRedactions pins the write-path audit: a value the
// secret guard shortened is stored WITH its redaction rows (the fact and the
// journal entry), so a reader can tell the value is not the full claim; a clean
// value explicitly clears the rows of the value it replaced (a stale audit
// would claim a loss that did not happen).
func TestPatchValueChangeCarriesRedactions(t *testing.T) {
	const source = `{"key":"k","value":"v1","category":"config","project":"p","status":"verified"}`
	var body string
	c, fake := newFakeClient(t, patchRespond(source, &body))

	reds := []model.Redaction{{Field: "value", Pattern: "long-hex", Chars: 48}}
	res, err := c.Patch(context.Background(), "k", PatchFields{Value: "id [velruna-redacted:long-hex]", Redactions: reds})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	// The answer reports what the write stored — the value rows here.
	if len(res.Redactions) != 1 || res.Redactions[0].Field != "value" {
		t.Errorf("res.Redactions = %+v, want the stored value row", res.Redactions)
	}
	params := patchParams(t, body)
	rows, ok := params["redactions"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("params.redactions = %v, want one row", params["redactions"])
	}
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(appends))
	}
	if !strings.Contains(appends[0].Body, `"redactions"`) {
		t.Errorf("the journal entry must state the audit: %s", appends[0].Body)
	}

	// A clean correction clears the audit explicitly (nil + the clear flag):
	// leaving the old rows would describe a value that no longer exists.
	var cleanBody string
	c2, _ := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v1","category":"config","project":"p","status":"verified","redactions":[{"field":"value","pattern":"long-hex","chars":48}]}`,
		&cleanBody))
	cleanRes, err := c2.Patch(context.Background(), "k", PatchFields{Value: "clean value"})
	if err != nil {
		t.Fatalf("Patch clean: %v", err)
	}
	cleanParams := patchParams(t, cleanBody)
	if v := cleanParams["redactions"]; v != nil {
		t.Errorf("params.redactions = %v, want null on a clean value", v)
	}
	if v := cleanParams["redactions_clear"]; v != true {
		t.Errorf("params.redactions_clear = %v, want true", v)
	}
	if len(cleanRes.Redactions) != 0 {
		t.Errorf("res.Redactions = %+v, want none on a clean value", cleanRes.Redactions)
	}

	// A qualifier-only patch reports only the rows of the fields it wrote: a
	// summary row lands, a value row cut from the INPUT must not be announced.
	var qualBody string
	c3, _ := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v1","category":"config","project":"p","status":"verified"}`,
		&qualBody))
	qualRows := []model.Redaction{
		{Field: "value", Pattern: "long-hex", Chars: 48},
		{Field: "summary", Pattern: "long-hex", Chars: 48},
	}
	qualRes, err := c3.Patch(context.Background(), "k", PatchFields{Summary: "short toc", Redactions: qualRows})
	if err != nil {
		t.Fatalf("Patch qualifier: %v", err)
	}
	if len(qualRes.Redactions) != 1 || qualRes.Redactions[0].Field != "summary" {
		t.Errorf("res.Redactions = %+v, want exactly the summary row", qualRes.Redactions)
	}

	// A value change rewrites the value and the summary, not the notes: the
	// stored notes keep their marker, so the audit must keep their rows too
	// (round-27: a value-only patch used to drop them while the text stayed).
	var keepBody string
	c4, _ := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v1","category":"config","project":"p","status":"verified","notes":"note with [velruna-redacted:long-hex]","redactions":[{"field":"notes","pattern":"long-hex","chars":48}]}`,
		&keepBody))
	keepRows := []model.Redaction{{Field: "value", Pattern: "long-base64", Chars: 44}}
	if _, err := c4.Patch(context.Background(), "k", PatchFields{Value: "v2", Redactions: keepRows}); err != nil {
		t.Fatalf("Patch value change keeping notes: %v", err)
	}
	keepParams := patchParams(t, keepBody)
	stored, ok := keepParams["redactions"].([]any)
	if !ok || len(stored) != 2 {
		t.Fatalf("params.redactions = %v, want the kept notes row plus the new value row", keepParams["redactions"])
	}
	seenFields := map[string]bool{}
	for _, row := range stored {
		m, _ := row.(map[string]any)
		if f, _ := m["field"].(string); f != "" {
			seenFields[f] = true
		}
	}
	if !seenFields["notes"] || !seenFields["value"] {
		t.Errorf("stored redactions fields = %v, want notes kept and value replaced", seenFields)
	}
}

// TestPatchLongValueRequiresNewSummary keeps add's summary rule on the patch
// path: a long new value without a summary is refused before any write.
func TestPatchLongValueRequiresNewSummary(t *testing.T) {
	var body string
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) { cfg.SummaryMinLen = 10 }, patchRespond(
		`{"key":"k","value":"short","category":"config","project":"p","status":"verified"}`,
		&body))

	long := strings.Repeat("x", 20)
	_, err := c.Patch(context.Background(), "k", PatchFields{Value: long})
	if err == nil || !strings.Contains(err.Error(), "summary") {
		t.Fatalf("err = %v, want a summary-required refusal", err)
	}
	if body != "" {
		t.Error("a refused patch must not write the document")
	}
	if fake.sawAnyPrefix("/velruna-history-") {
		t.Error("a refused patch must not touch the journal")
	}
}

// TestPatchResurrectSetsObservedAt pins resurrection: declaring a dead fact
// alive again IS a fresh verification — observed_at moves and the transition
// is journaled.
func TestPatchResurrectSetsObservedAt(t *testing.T) {
	var body string
	c, fake := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"obsolete","observed_at":"2020-01-01T00:00:00Z"}`,
		&body))

	res, err := c.Patch(context.Background(), "k", PatchFields{Status: "verified"})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if !res.Changed || res.Status != "verified" {
		t.Errorf("res = %+v, want Changed=true Status=verified", res)
	}
	if fake.sawAnyPrefix("/velruna-history-") == false {
		t.Error("the status transition must be journaled")
	}
	params := patchParams(t, body)
	if v, ok := params["observed_at"]; !ok || v == nil {
		t.Error("resurrection must set observed_at=now")
	}
	if params["status"] != "verified" {
		t.Errorf("params.status = %v, want verified", params["status"])
	}
}

// TestPatchObsoleteWithReasonJournals: reason folds into notes ATOMICALLY
// (reason_append inside the script), the journal entry carries it, and the
// transition is journaled (audit symmetry with velruna_obsolete).
func TestPatchObsoleteWithReasonJournals(t *testing.T) {
	var body string
	c, fake := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"verified","notes":"n"}`,
		&body))

	res, err := c.Patch(context.Background(), "k", PatchFields{Status: "obsolete", Reason: "file deleted"})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if !res.Changed || res.Status != "obsolete" {
		t.Errorf("res = %+v, want Changed=true Status=obsolete", res)
	}
	params := patchParams(t, body)
	// The reason appends inside the script (atomic), never as a Go-side
	// notes replace that would race a concurrent notes_append.
	if params["reason_append"] != "file deleted" {
		t.Errorf("reason_append = %v, want 'file deleted'", params["reason_append"])
	}
	if v, ok := params["notes"]; !ok || v != nil {
		t.Errorf("notes must stay null on retire-with-reason (the script appends), got %v", params["notes"])
	}
	// The journal entry carries the reason: "why did this die" must be
	// answerable from the log (audit symmetry with velruna_obsolete).
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(appends))
	}
	if !strings.Contains(appends[0].Body, "n\\nfile deleted") {
		t.Errorf("journal entry must carry the folded reason: %s", appends[0].Body)
	}
	if !strings.Contains(appends[0].Body, `"op":"`+model.OpRetire+`"`) {
		t.Errorf("retire entry must carry op=retire: %s", appends[0].Body)
	}
}

// TestPatchRefusedUpdateLeavesNoJournalEntry pins the journal contract the
// live smoke broke: when the cluster refuses the scripted update (the
// painless compile-error class, a shard refusing the write), the journal
// must stay silent — an entry describing a state that never landed is an
// audit lie. The doc-first order (entry built, id preallocated into
// current_ref, entry indexed only after the update is accepted) is what
// guarantees this; journal-first left phantom entries behind on every
// refused patch.
func TestPatchRefusedUpdateLeavesNoJournalEntry(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalRoutesOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/velruna/_doc/k":
			return http.StatusOK, `{"_index":"velruna","_id":"k","found":true,"_source":{"key":"k","value":"v","category":"config","project":"p","status":"verified"}}`
		case r.Method == http.MethodPost && r.Path == "/velruna/_update/k":
			return http.StatusBadRequest, `{"status":400,"error":{"type":"illegal_argument_exception","reason":"failed to execute script"}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
	})
	if _, err := c.Patch(context.Background(), "k", PatchFields{Value: "new value"}); err == nil {
		t.Fatal("refused update must report an error")
	}
	if appends := journalAppends(fake); len(appends) != 0 {
		t.Errorf("a refused update must not journal, got %d appends: %s", len(appends), appends[0].Body)
	}
}

// TestPatchJournalEntryLinkedByPreallocatedID pins the link that the
// doc-first order needs: the update carries current_ref = the entry id,
// and the entry is indexed under that same id (op_type=create), so the
// doc->entry link survives even though the entry lands after the update.
func TestPatchJournalEntryLinkedByPreallocatedID(t *testing.T) {
	var body string
	c, fake := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"verified"}`,
		&body))
	if _, err := c.Patch(context.Background(), "k", PatchFields{Value: "new value"}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	params := patchParams(t, body)
	ref, _ := params["current_ref"].(string)
	if ref == "" {
		t.Fatalf("current_ref must carry the preallocated entry id: %v", params["current_ref"])
	}
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(appends))
	}
	if appends[0].Path[strings.LastIndex(appends[0].Path, "/")+1:] != ref {
		t.Errorf("journal entry must be indexed under current_ref id, path = %s, ref = %s", appends[0].Path, ref)
	}
	// Explicit-id append must be create-only (a retry collides, never
	// duplicates): PUT /_doc/<id> IS the create API in OpenSearch REST.
	if appends[0].Method != http.MethodPut && appends[0].Query.Get("op_type") != "create" {
		t.Errorf("the entry must be an idempotent create: %s %s %v", appends[0].Method, appends[0].Path, appends[0].Query)
	}
}

// TestPatchValidation covers the pre-store refusals: empty patch, reason
// without status, unknown status, and a missing fact.
func TestPatchValidation(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusInternalServerError, `{"status":500,"error":"must not reach the store"}`
	})
	ctx := context.Background()

	if _, err := c.Patch(ctx, "k", PatchFields{}); err == nil ||
		!strings.Contains(err.Error(), "nothing to change") {
		t.Errorf("empty patch: err = %v", err)
	}
	if _, err := c.Patch(ctx, "k", PatchFields{Reason: "why"}); err == nil ||
		!strings.Contains(err.Error(), "status=obsolete") {
		t.Errorf("reason without status: err = %v", err)
	}
	if _, err := c.Patch(ctx, "k", PatchFields{Status: "bogus"}); err == nil ||
		!strings.Contains(err.Error(), "verified|obsolete") {
		t.Errorf("bad status: err = %v", err)
	}

	notFound, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusNotFound, `{"status":404,"error":"document_missing_exception"}`
	})
	if _, err := notFound.Patch(ctx, "nope", PatchFields{NotesAppend: "x"}); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("missing fact: err = %v, want a not-found refusal", err)
	} else if !errors.Is(err, ErrNotFound) {
		// The MCP edge classifies this as "no such fact" (a clean answer) only
		// if the chain survives the wrap; without %w it reads as a store failure.
		t.Errorf("patch not-found must wrap ErrNotFound: %v", err)
	}
}

// TestPatchRetireEmptyNotes: the reason join must not produce a leading
// blank line when notes are empty (the painless append and the journal
// snapshot both use the same conditional join).
func TestPatchRetireEmptyNotes(t *testing.T) {
	var body string
	c, fake := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"verified"}`,
		&body))
	if _, err := c.Patch(context.Background(), "k", PatchFields{Status: "obsolete", Reason: "gone"}); err != nil {
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
	if je["notes"] != "gone" {
		t.Errorf("journal notes = %q, want exactly 'gone' (no leading newline)", je["notes"])
	}
}

// TestPatchRetireOnObsoleteFactIsRejected: a reason belongs to the
// verified→obsolete transition. Re-retiring an already obsolete fact with a
// reason would append a duplicate notes line with no journal entry (no state
// was accepted) — a silent audit lie, so it fails loudly.
func TestPatchRetireOnObsoleteFactIsRejected(t *testing.T) {
	var body string
	c, fake := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"obsolete"}`, &body))
	_, err := c.Patch(context.Background(), "k", PatchFields{Status: "obsolete", Reason: "gone"})
	if err == nil || !strings.Contains(err.Error(), "already obsolete") {
		t.Errorf("re-retire with a reason must fail loudly: %v", err)
	} else if !errors.Is(err, ErrContract) {
		t.Errorf("a pre-transport rejection must be classified ErrContract: %v", err)
	}
	if len(fake.requestsFor(http.MethodPost, "/velruna/_update/k")) != 0 {
		t.Error("the rejected patch must not touch the doc")
	}
}

// TestPatchRetireReasonFolded pins the notes convention on the Patch edge
// too: the retire reason joins notes as ONE line (whitespace runs folded).
func TestPatchRetireReasonFolded(t *testing.T) {
	var body string
	c, _ := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"verified"}`, &body))

	if _, err := c.Patch(context.Background(), "k", PatchFields{
		Status: "obsolete", Reason: "file deleted\non the server   today",
	}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	params := patchParams(t, body)
	if params["reason_append"] != "file deleted on the server today" {
		t.Errorf("reason_append = %v, want the folded single line", params["reason_append"])
	}
}

// TestPatchKeywordsReplacesTags pins that a patch can re-tag a fact without
// resending its value: the tag set is normalized like add, written in the same
// scripted update, and a tag-only patch stays a qualifier touch (not journaled).
func TestPatchKeywordsReplacesTags(t *testing.T) {
	var body string
	c, fake := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"verified"}`, &body))

	res, err := c.Patch(context.Background(), "k", PatchFields{Keywords: []string{" Perf ", "go", "perf"}})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if res.Changed {
		t.Error("a tag-only patch is a qualifier touch, Changed must stay false")
	}
	params := patchParams(t, body)
	kw, ok := params["keywords"].([]interface{})
	if !ok || len(kw) != 2 || kw[0] != "perf" || kw[1] != "go" {
		t.Errorf("keywords param = %v, want normalized [perf go]", params["keywords"])
	}
	if appends := journalAppends(fake); len(appends) != 0 {
		t.Errorf("a tag-only patch must not journal: %d appends", len(appends))
	}
}

// TestPatchReverifyConfirmsInPlace pins the re-verification path: the same
// value, freshly observed. observed_at moves (the whole point of a stale
// re-check), the journal gains op=rediscovery, the writer joins confirmed_by,
// and the value itself does not change.
func TestPatchReverifyConfirmsInPlace(t *testing.T) {
	var body string
	c, fake := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"verified",`+
			`"observed_at":"2026-01-01T00:00:00Z","confirmed_by":[{"author":"alice","author_source":"os","host":"h1","at":"2026-01-01T00:00:00Z"}]}`,
		&body))

	res, err := c.Patch(context.Background(), "k", PatchFields{Reverify: true})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if !res.Reverified || !res.Changed || res.Status != "verified" {
		t.Fatalf("res = %+v, want Reverified+Changed+verified", res)
	}
	params := patchParams(t, body)
	if params["observed_at"] == nil || params["current_ref"] == nil {
		t.Errorf("reverify must move observed_at and journal the entry: %v", params)
	}
	if params["value"] != nil || params["confirmed_by"] != nil {
		t.Errorf("reverify must not rewrite the value or reset the set: %v", params)
	}
	conf, ok := params["confirmer"].(map[string]any)
	if !ok || conf["author"] == "" || conf["at"] == "" {
		t.Fatalf("confirmer param = %v, want author/at (the writer)", params["confirmer"])
	}
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(appends))
	}
	if !strings.Contains(appends[0].Body, `"op":"`+model.OpRediscovery+`"`) {
		t.Errorf("reverify must journal op=rediscovery: %s", appends[0].Body)
	}
}

// TestPatchReverifyRefusedCases pins the shape: a re-confirmation is one event,
// so it cannot ride along with a value/status change, and a dead fact is
// resurrected, not re-verified.
func TestPatchReverifyRefusedCases(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    PatchFields
	}{
		{"with value", PatchFields{Reverify: true, Value: "v2"}},
		{"with status", PatchFields{Reverify: true, Status: "verified"}},
		{"with summary", PatchFields{Reverify: true, Summary: "s"}},
	} {
		if err := ValidatePatchFields(tc.p); err == nil {
			t.Errorf("%s: reverify must be refused in combination", tc.name)
		}
	}
	var body string
	c, _ := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"obsolete"}`, &body))
	_, err := c.Patch(context.Background(), "k", PatchFields{Reverify: true})
	if err == nil || !strings.Contains(err.Error(), "obsolete") {
		t.Errorf("reverify on an obsolete fact must be refused, got %v", err)
	}
}

// TestPatchValueChangeResetsConfirmers pins the trust rule: confirmations
// belong to the value they verified, so a new value starts with its writer.
func TestPatchValueChangeResetsConfirmers(t *testing.T) {
	var body string
	c, _ := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"verified",`+
			`"confirmed_by":[{"author":"alice","host":"h1","at":"2026-01-01T00:00:00Z"}]}`, &body))

	if _, err := c.Patch(context.Background(), "k", PatchFields{Value: "v2"}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	params := patchParams(t, body)
	set, ok := params["confirmed_by"].([]any)
	if !ok || len(set) != 1 {
		t.Fatalf("confirmed_by = %v, want exactly the writer", params["confirmed_by"])
	}
	if _, has := set[0].(map[string]any)["author"]; !has {
		t.Errorf("reset confirmer must name its author: %v", set[0])
	}
}

// TestPatchCleanNoteClearsStaleRow: a note rewritten without a finding drops the
// stored rows of the note AND of the lines appended into it (the replacement
// erased that text), while the audit of the value it sits next to survives.
func TestPatchCleanNoteClearsStaleRow(t *testing.T) {
	var body string
	c, _ := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"verified","notes":"old [velruna-redacted:long-hex]","redactions":[{"field":"value","pattern":"long-hex","chars":48},{"field":"notes","pattern":"long-hex","chars":40},{"field":"notes_append","pattern":"long-hex","chars":41},{"field":"reason","pattern":"long-hex","chars":42}]}`,
		&body))
	if _, err := c.Patch(context.Background(), "k", PatchFields{Notes: "checked by hand"}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	params := patchParams(t, body)
	rows, ok := params["redactions"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("params.redactions = %v, want only the value row", params["redactions"])
	}
	if row, _ := rows[0].(map[string]any); row["field"] != "value" {
		t.Errorf("row = %v, want the value row to survive", rows[0])
	}
}

// TestPatchNotesAppendKeepsEarlierRows: appended text adds rows instead of
// replacing them — the earlier appended line is still in the notes.
func TestPatchNotesAppendKeepsEarlierRows(t *testing.T) {
	var body string
	c, _ := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"verified","notes":"old","redactions":[{"field":"notes_append","pattern":"long-hex","chars":40}]}`,
		&body))
	rows := []model.Redaction{{Field: "notes_append", Pattern: "long-base64", Chars: 41}}
	if _, err := c.Patch(context.Background(), "k", PatchFields{NotesAppend: "line [velruna-redacted:long-base64]", Redactions: rows}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	params := patchParams(t, body)
	got, ok := params["redactions"].([]any)
	if !ok || len(got) != 2 {
		t.Fatalf("params.redactions = %v, want both appended rows", params["redactions"])
	}
}
