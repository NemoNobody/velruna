package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/NemoNobody/velruna/internal/model"
)

// The tool inputs are the MCP contract: an agent sends exactly these JSON keys.
// These tests pin the wiring (json tag names, array vs scalar, the optional
// cursor at) without a live server.
func TestWorksetShowInputUnmarshal(t *testing.T) {
	raw := `{"name":"p2","after":2,"before":9,"limit":10,"phase":["todo","in_progress"],"live_only":true,"journal":5}`
	var in worksetShowInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("unmarshal worksetShowInput: %v", err)
	}
	if in.Name != "p2" || in.After != 2 || in.Before != 9 || in.Limit != 10 || !in.LiveOnly || in.Journal != 5 {
		t.Fatalf("scalar fields = %+v", in)
	}
	if len(in.Phase) != 2 || in.Phase[0] != "todo" || in.Phase[1] != "in_progress" {
		t.Fatalf("phase = %v, want [todo in_progress]", in.Phase)
	}
	phases, err := normalizePhases(in.Phase)
	if err != nil || len(phases) != 2 {
		t.Fatalf("normalizePhases(input) = %v, %v", phases, err)
	}
}

func TestWorksetAddInputJSONKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want []string
	}{
		{
			name: "add",
			in:   worksetAddInput{Name: "p2", FactIDs: []string{"a", "b"}, Phase: "todo"},
			want: []string{`"name":"p2"`, `"fact_ids":["a","b"]`, `"phase":"todo"`},
		},
		{
			name: "phase",
			in:   worksetPhaseInput{Name: "p2", FactIDs: []string{"c"}, To: "done", Takeover: true},
			want: []string{`"fact_ids":["c"]`, `"to":"done"`, `"takeover":true`},
		},
		{
			name: "rename",
			in:   worksetRenameInput{Name: "old", NewName: "new"},
			want: []string{`"new_name":"new"`},
		},
		{
			name: "delete",
			in:   worksetDeleteInput{Name: "p2", Confirm: true},
			want: []string{`"name":"p2"`},
		},
	} {
		b, err := json.Marshal(tc.in)
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.name, err)
		}
		for _, want := range tc.want {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s: %s missing %s", tc.name, b, want)
			}
		}
	}
}

// at is a pointer so "0" (a valid cursor position) is distinguishable from
// "absent" (read-only mode).
func TestWorksetCursorInputAtPointer(t *testing.T) {
	var withAt worksetCursorInput
	if err := json.Unmarshal([]byte(`{"name":"p2","at":0}`), &withAt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if withAt.At == nil || *withAt.At != 0 {
		t.Fatalf("at = %v, want pointer to 0", withAt.At)
	}
	var withoutAt worksetCursorInput
	if err := json.Unmarshal([]byte(`{"name":"p2"}`), &withoutAt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if withoutAt.At != nil {
		t.Fatalf("at = %v, want nil", withoutAt.At)
	}
}

func TestWorksetToolRejectsUnknownPhase(t *testing.T) {
	if _, _, _, err := worksetMutationArgs("op", "p2", []string{"a"}, "working", true); err == nil {
		t.Fatal("unknown phase: want an edge validation error")
	}
	// add defaults an omitted phase to todo
	_, ids, phase, err := worksetMutationArgs("op", "p2", []string{"a", "a"}, "", false)
	if err != nil {
		t.Fatalf("worksetMutationArgs(add): %v", err)
	}
	if len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("ids = %v, want [a] (duplicates collapsed)", ids)
	}
	if phase != model.PhaseTodo {
		t.Fatalf("phase = %q, want todo", phase)
	}
	// phase requires a target
	if _, _, _, err := worksetMutationArgs("op", "p2", []string{"a"}, "", true); err == nil {
		t.Fatal("phase without to: want an error")
	}
	// the name is a slug, not free text
	if _, _, _, err := worksetMutationArgs("op", "p2 fixes", []string{"a"}, "todo", true); err == nil {
		t.Fatal("invalid slug: want an error")
	}
}

func TestWorksetToolOutputOmitsEmptyFields(t *testing.T) {
	b, err := json.Marshal(worksetOutput{Deleted: true, Name: "p2"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(b)
	for _, unwanted := range []string{`"workset"`, `"worksets"`, `"journal"`, `"cursor"`} {
		if strings.Contains(out, unwanted) {
			t.Errorf("delete answer must not carry %s: %s", unwanted, out)
		}
	}
	if !strings.Contains(out, `"deleted":true`) || !strings.Contains(out, `"name":"p2"`) {
		t.Errorf("delete answer = %s", out)
	}
}

// TestWorksetJournalViewCarriesTakeover pins the journal field that tells an
// explicit takeover from a stale one: op alone no longer has to encode it.
func TestWorksetJournalViewCarriesTakeover(t *testing.T) {
	views := worksetJournalViews([]model.WorksetJournalEntry{
		{Op: "takeover", Takeover: "explicit", FactID: "f1", To: model.PhaseInProgress, By: "bob"},
		{Op: "phase", Takeover: "stale", FactID: "f2", To: model.PhaseTesting, By: "carol"},
		{Op: "add", FactID: "f3", By: "alice"},
	})
	b, err := json.Marshal(views)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(b)
	for _, want := range []string{`"takeover":"explicit"`, `"takeover":"stale"`} {
		if !strings.Contains(out, want) {
			t.Errorf("journal views missing %s: %s", want, out)
		}
	}
	if views[2].Takeover != "" {
		t.Errorf("a plain add carries takeover=%q, want empty", views[2].Takeover)
	}
}

// TestWorksetCursorInputConfirmSecret: the cursor key is stored identity, so
// the input accepts the same per-call confirmation as the other writes.
func TestWorksetCursorInputConfirmSecret(t *testing.T) {
	var in worksetCursorInput
	if err := json.Unmarshal([]byte(`{"name":"p2","author":"alice","confirm_secret":true}`), &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !in.ConfirmSecret {
		t.Fatal("confirm_secret did not parse")
	}
}
