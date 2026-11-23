package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/NemoNobody/velruna/internal/model"
)

func wsTestEntry(id string, phase model.Phase) model.Entry {
	return model.Entry{FactID: id, Phase: phase}
}

// The window contract: phase and live filters run FIRST, so the positions
// --after/--before name are positions in the filtered view, not in the raw
// workset. This test pins that order (and would fail if the index were taken
// from the unfiltered list).
func TestWorksetWindowFiltersBeforeWindow(t *testing.T) {
	entries := []model.Entry{
		wsTestEntry("e1", model.PhaseTodo),
		wsTestEntry("e2", model.PhaseInProgress),
		wsTestEntry("e3", model.PhaseInProgress),
		wsTestEntry("e4", model.PhaseTesting),
		wsTestEntry("e5", model.PhaseInProgress),
		wsTestEntry("e6", model.PhaseInProgress),
	}
	states := map[string]string{
		"e1": worksetStateVerified,
		"e2": worksetStateVerified,
		"e3": worksetStateObsolete,
		"e4": worksetStateVerified,
		"e5": worksetStateMissing,
		"e6": worksetStateVerified,
	}
	// in_progress = e2,e3,e5,e6; live-only drops e3 (obsolete) and e5 (missing).
	rows := worksetWindow(entries, states, []model.Phase{model.PhaseInProgress}, true, 0, 0, 0)
	if len(rows) != 2 || rows[0].Entry.FactID != "e2" || rows[1].Entry.FactID != "e6" {
		t.Fatalf("filters = %v, want [e2 e6]", rowIDs(rows))
	}
	if rows[0].Index != 1 || rows[1].Index != 2 {
		t.Fatalf("indices = %d,%d, want 1,2 (assigned after filtering)", rows[0].Index, rows[1].Index)
	}

	// --after is exclusive on the filtered index: e2 (1) goes, e6 (2) stays.
	rows = worksetWindow(entries, states, []model.Phase{model.PhaseInProgress}, true, 1, 0, 0)
	if len(rows) != 1 || rows[0].Entry.FactID != "e6" || rows[0].Index != 2 {
		t.Fatalf("after=1 = %v, want [e6@2]", rowIDs(rows))
	}
	// --before is exclusive too.
	rows = worksetWindow(entries, states, []model.Phase{model.PhaseInProgress}, true, 0, 2, 0)
	if len(rows) != 1 || rows[0].Entry.FactID != "e2" || rows[0].Index != 1 {
		t.Fatalf("before=2 = %v, want [e2@1]", rowIDs(rows))
	}
	// --limit is applied last.
	rows = worksetWindow(entries, states, []model.Phase{model.PhaseInProgress}, true, 0, 0, 1)
	if len(rows) != 1 || rows[0].Entry.FactID != "e2" {
		t.Fatalf("limit=1 = %v, want [e2]", rowIDs(rows))
	}
}

func TestWorksetWindowWithoutFiltersKeepsOrderAndIndexes(t *testing.T) {
	entries := []model.Entry{
		wsTestEntry("a", model.PhaseTodo),
		wsTestEntry("b", model.PhaseDone),
	}
	rows := worksetWindow(entries, nil, nil, false, 0, 0, 0)
	if len(rows) != 2 || rows[0].Index != 1 || rows[1].Index != 2 {
		t.Fatalf("rows = %+v, want two entries indexed 1,2", rows)
	}
	// A missing state answer must render as unknown, never as verified.
	if rows[0].State != worksetStateUnknown {
		t.Fatalf("state = %q, want %q for an absent state", rows[0].State, worksetStateUnknown)
	}
}

func rowIDs(rows []worksetRow) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.Entry.FactID
	}
	return ids
}

func TestWorksetLimit(t *testing.T) {
	cases := []struct {
		in      int
		want    int
		wantErr bool
	}{
		{in: 0, want: worksetDefaultLimit},
		{in: 1, want: 1},
		{in: worksetMaxLimit, want: worksetMaxLimit},
		{in: worksetMaxLimit + 100, want: worksetMaxLimit},
		{in: -1, wantErr: true},
	}
	for _, tc := range cases {
		got, err := worksetLimit(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("worksetLimit(%d): want error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("worksetLimit(%d): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("worksetLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestWorksetJournalCount(t *testing.T) {
	if got, err := worksetJournalCount(0); err != nil || got != 0 {
		t.Fatalf("worksetJournalCount(0) = %d, %v; want 0, nil", got, err)
	}
	if got, err := worksetJournalCount(500); err != nil || got != worksetMaxLimit {
		t.Fatalf("worksetJournalCount(500) = %d, %v; want %d, nil", got, err, worksetMaxLimit)
	}
	if _, err := worksetJournalCount(-1); err == nil {
		t.Fatal("worksetJournalCount(-1): want error")
	}
}

func TestParsePhaseList(t *testing.T) {
	got, err := parsePhaseList("todo, in_progress")
	if err != nil {
		t.Fatalf("parsePhaseList: %v", err)
	}
	if len(got) != 2 || got[0] != model.PhaseTodo || got[1] != model.PhaseInProgress {
		t.Fatalf("parsePhaseList = %v, want [todo in_progress]", got)
	}
	// Duplicates collapse, order is preserved.
	got, err = parsePhaseList("todo, todo , blocked")
	if err != nil {
		t.Fatalf("parsePhaseList: %v", err)
	}
	if len(got) != 2 || got[0] != model.PhaseTodo || got[1] != model.PhaseBlocked {
		t.Fatalf("parsePhaseList dedupe = %v, want [todo blocked]", got)
	}
	if got, err := parsePhaseList("  "); err != nil || got != nil {
		t.Fatalf("parsePhaseList(blank) = %v, %v; want nil, nil", got, err)
	}
	if _, err := parsePhaseList("todo,nope"); err == nil {
		t.Fatal("parsePhaseList with an unknown phase: want error")
	}
}

func TestNormalizePhases(t *testing.T) {
	got, err := normalizePhases([]string{" Testing ", "testing"})
	if err != nil {
		t.Fatalf("normalizePhases: %v", err)
	}
	if len(got) != 1 || got[0] != model.PhaseTesting {
		t.Fatalf("normalizePhases = %v, want [testing]", got)
	}
	if _, err := normalizePhases([]string{"nope"}); err == nil {
		t.Fatal("normalizePhases with an unknown phase: want error")
	}
}

func TestParsePhaseValue(t *testing.T) {
	p, err := parsePhaseValue(" In_Progress ")
	if err != nil || p != model.PhaseInProgress {
		t.Fatalf("parsePhaseValue = %q, %v; want in_progress, nil", p, err)
	}
	if p, err := parsePhaseValue(""); err != nil || p != "" {
		t.Fatalf("parsePhaseValue(empty) = %q, %v; want empty, nil", p, err)
	}
	if _, err := parsePhaseValue("working"); err == nil {
		t.Fatal("parsePhaseValue(working): want error")
	}
}

func TestWorksetPresence(t *testing.T) {
	rows := []worksetRow{
		{Index: 3, Entry: model.Entry{Phase: model.PhaseInProgress, PhaseBy: "alice"}},
		{Index: 7, Entry: model.Entry{Phase: model.PhaseInProgress, PhaseBy: "bob"}},
		{Index: 1, Entry: model.Entry{Phase: model.PhaseTodo, PhaseBy: "carol"}},
	}
	got := worksetPresence(rows, "box")
	want := "in_progress: alice@box@3, bob@box@7"
	if got != want {
		t.Fatalf("worksetPresence = %q, want %q", got, want)
	}
	if got := worksetPresence([]worksetRow{{Index: 1, Entry: model.Entry{Phase: model.PhaseTodo}}}, "box"); got != "" {
		t.Fatalf("worksetPresence with no in_progress = %q, want empty", got)
	}
}

func TestWorksetSelfVerified(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	done := model.Entry{FactID: "f1", Phase: model.PhaseDone, PhaseBy: "alice", PhaseAt: now}

	// Without the journal there is nothing to compare: no marker, never a
	// fallback that would call every done entry self-verified.
	if worksetSelfVerified(done, nil) {
		t.Fatal("no journal: self_verified must not be inferred")
	}
	sameAuthor := []model.WorksetJournalEntry{
		{FactID: "f1", To: model.PhaseTesting, By: "alice", At: now.Add(-time.Hour)},
	}
	if !worksetSelfVerified(done, sameAuthor) {
		t.Fatal("testing by the done author must mark self_verified")
	}
	otherAuthor := []model.WorksetJournalEntry{
		{FactID: "f1", To: model.PhaseTesting, By: "bob", At: now.Add(-time.Hour)},
	}
	if worksetSelfVerified(done, otherAuthor) {
		t.Fatal("testing by another author must not mark self_verified")
	}
	// A transition recorded after the done phase belongs to a later cycle and
	// proves nothing about who tested this done entry.
	laterCycle := []model.WorksetJournalEntry{
		{FactID: "f1", To: model.PhaseInProgress, By: "bob", At: now.Add(time.Hour)},
	}
	if worksetSelfVerified(done, laterCycle) {
		t.Fatal("a later cycle's transition must not be compared with this done entry")
	}
	// A small --journal window can miss the testing row: the entry is not
	// marked, because the loaded window does not contain the proof.
	unrelatedWindow := []model.WorksetJournalEntry{
		{FactID: "f2", To: model.PhaseTesting, By: "alice", At: now.Add(-time.Hour)},
	}
	if worksetSelfVerified(done, unrelatedWindow) {
		t.Fatal("a window without the entry's testing row must not mark self_verified")
	}
	if worksetSelfVerified(model.Entry{FactID: "f1", Phase: model.PhaseCancelled, PhaseBy: "alice"}, nil) {
		t.Fatal("only done entries can be self_verified")
	}
}

func TestWorksetCursorsLine(t *testing.T) {
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	cursors := map[string]model.Cursor{
		"old@box": {At: 1, UpdatedAt: base.Add(-2 * time.Hour)},
		"new@box": {At: 9, UpdatedAt: base},
		"mid@box": {At: 4, UpdatedAt: base.Add(-time.Hour)},
	}
	got := worksetCursorsLine(cursors)
	want := "cursors: new@box@9, mid@box@4, old@box@1"
	if got != want {
		t.Fatalf("worksetCursorsLine = %q, want %q", got, want)
	}
	if got := worksetCursorsLine(nil); got != "" {
		t.Fatalf("empty cursors = %q, want empty", got)
	}
	// Ties break by key, so two identical reads render the same page.
	tie := map[string]model.Cursor{
		"b": {At: 2, UpdatedAt: base},
		"a": {At: 1, UpdatedAt: base},
	}
	if got := worksetCursorsLine(tie); got != "cursors: a@1, b@2" {
		t.Fatalf("tie order = %q, want a then b", got)
	}
}

// TestPrintWorksetPresenceFromAllEntries pins finding 9: the table may be
// filtered, but presence, cursors and self_verified describe the whole
// workset.
func TestPrintWorksetPresenceFromAllEntries(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	ws := &model.Workset{
		Name: "p2", Project: "p", Version: 2,
		Cursors: map[string]model.Cursor{"reader@box": {At: 2, UpdatedAt: now}},
		Entries: []model.Entry{
			{FactID: "f1", Phase: model.PhaseInProgress, PhaseBy: "alice", PhaseAt: now},
			{FactID: "f2", Phase: model.PhaseInProgress, PhaseBy: "bob", PhaseAt: now},
		},
	}
	allRows := worksetWindow(ws.Entries, nil, nil, false, 0, 0, 0)
	filtered := worksetWindow(ws.Entries, nil, []model.Phase{model.PhaseInProgress}, false, 0, 0, 1)
	var b strings.Builder
	if err := printWorkset(&b, ws, filtered, allRows, 0, "box", nil, false, now, time.Hour); err != nil {
		t.Fatalf("printWorkset: %v", err)
	}
	out := b.String()
	for _, want := range []string{"in_progress: alice@box@1, bob@box@2", "cursors: reader@box@2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The table itself stays filtered to one row.
	if strings.Contains(out, "f2 ") {
		t.Errorf("filtered table leaked the second entry:\n%s", out)
	}
}

func TestWorksetTableMarksStale(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	rows := []worksetRow{{
		Index: 1,
		Entry: model.Entry{
			FactID:  "f1",
			Phase:   model.PhaseInProgress,
			PhaseBy: "alice",
			PhaseAt: now.Add(-2 * time.Hour),
			Note:    "work",
		},
		State: worksetStateVerified,
	}}
	out := worksetTable(rows, now, time.Hour)
	for _, want := range []string{"fact_id", "state", "phase", "phase_by", "age", "f1", "verified", "in_progress", "alice", "(stale)", "work"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

func TestWorksetPhaseAge(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{name: "zero", at: time.Time{}, want: "unknown"},
		{name: "seconds", at: now.Add(-30 * time.Second), want: "<1m"},
		{name: "minutes", at: now.Add(-5 * time.Minute), want: "5m"},
		{name: "hours", at: now.Add(-2 * time.Hour), want: "2h"},
		{name: "days", at: now.Add(-72 * time.Hour), want: "3d"},
	}
	for _, tc := range cases {
		if got := worksetPhaseAge(tc.at, now, 365*24*time.Hour); got != tc.want {
			t.Errorf("%s: worksetPhaseAge = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := worksetPhaseAge(now.Add(-2*time.Hour), now, time.Hour); got != "2h (stale)" {
		t.Errorf("stale age = %q, want %q", got, "2h (stale)")
	}
	// A future timestamp (clock skew) is never stale and never negative.
	if got := worksetPhaseAge(now.Add(time.Hour), now, time.Hour); got != "<1m" {
		t.Errorf("future age = %q, want <1m", got)
	}
}

func TestWorksetJournalBlock(t *testing.T) {
	if got := worksetJournalBlock(nil); got != "journal: none\n" {
		t.Fatalf("empty journal = %q", got)
	}
	out := worksetJournalBlock([]model.WorksetJournalEntry{{
		Op:       "takeover",
		Takeover: "explicit",
		FactID:   "f1",
		From:     model.PhaseTodo,
		To:       model.PhaseInProgress,
		Note:     "start",
		By:       "alice",
		At:       time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
	}})
	for _, want := range []string{"journal (newest first)", "takeover", "takeover=explicit", "f1", "todo→in_progress", "by=alice", "note=start"} {
		if !strings.Contains(out, want) {
			t.Errorf("journal block missing %q:\n%s", want, out)
		}
	}
}

func TestWorksetSummary(t *testing.T) {
	ws := &model.Workset{
		Name:    "p2",
		Project: "global",
		Version: 3,
		Entries: []model.Entry{wsTestEntry("a", model.PhaseTodo), wsTestEntry("b", model.PhaseDone)},
		Aliases: []string{"old"},
	}
	out := worksetSummary(ws)
	for _, want := range []string{"workset: p2", "project: global", "version: 3", "entries: 2", "aliases: old"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q: %s", want, out)
		}
	}
}

func TestRequireDeleteConfirm(t *testing.T) {
	if err := requireDeleteConfirm("p2", true); err != nil {
		t.Fatalf("requireDeleteConfirm(yes) = %v, want nil", err)
	}
	err := requireDeleteConfirm("p2", false)
	if err == nil {
		t.Fatal("requireDeleteConfirm(no): want error")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("refusal must name --yes, got: %v", err)
	}
}

func TestWorksetCursorArgsFor(t *testing.T) {
	if got, err := worksetCursorArgsFor(nil, false); err != nil || got.Mutate {
		t.Fatalf("read-only = %+v, %v; want no mutation", got, err)
	}
	zero := 0
	if got, err := worksetCursorArgsFor(&zero, false); err != nil || !got.Mutate || got.At != 0 {
		t.Fatalf("at=0 = %+v, %v; want mutate at 0", got, err)
	}
	three := 3
	if got, err := worksetCursorArgsFor(&three, false); err != nil || !got.Mutate || got.At != 3 {
		t.Fatalf("at=3 = %+v, %v; want mutate at 3", got, err)
	}
	if _, err := worksetCursorArgsFor(&three, true); err == nil {
		t.Fatal("at + clear: want error")
	}
	neg := -1
	if _, err := worksetCursorArgsFor(&neg, false); err == nil {
		t.Fatal("at=-1: want error")
	}
	if got, err := worksetCursorArgsFor(nil, true); err != nil || !got.Mutate || !got.Clear {
		t.Fatalf("clear = %+v, %v; want clear mutation", got, err)
	}
}

func TestWorksetCursorIdentity(t *testing.T) {
	author, host, err := worksetCursorIdentity("alice", "box", "", "")
	if err != nil || author != "alice" || host != "box" {
		t.Fatalf("defaults = %q,%q,%v; want alice,box,nil", author, host, err)
	}
	author, host, err = worksetCursorIdentity("alice", "box", " bob ", " rack1 ")
	if err != nil || author != "bob" || host != "rack1" {
		t.Fatalf("overrides = %q,%q,%v; want bob,rack1,nil", author, host, err)
	}
	if _, _, err := worksetCursorIdentity("alice", "box", "  ", ""); err != nil {
		t.Fatalf("blank override must be ignored, got %v", err)
	}
	if _, _, err := worksetCursorIdentity("alice", "box", "\x00", ""); err == nil {
		t.Fatal("a label that is blank after sanitizing: want error")
	}
}

func TestNormalizeFactIDs(t *testing.T) {
	got, err := normalizeFactIDs([]string{" a ", "b", "a"})
	if err != nil {
		t.Fatalf("normalizeFactIDs: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("normalizeFactIDs = %v, want [a b]", got)
	}
	if _, err := normalizeFactIDs([]string{"a", " "}); err == nil {
		t.Fatal("an empty id: want error")
	}
	if _, err := normalizeFactIDs(nil); err == nil {
		t.Fatal("no ids: want error")
	}
}
