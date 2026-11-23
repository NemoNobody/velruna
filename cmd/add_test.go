package cmd

import (
	"strings"
	"testing"

	"github.com/NemoNobody/velruna/internal/client"
)

// TestAddVerbNamesResurrection pins the CLI wording: a write that flips an
// obsolete fact back to verified must say so (Add reports Changed=true on
// every resurrection path, so a bare "updated" would hide the status flip).
func TestAddVerbNamesResurrection(t *testing.T) {
	cases := []struct {
		name string
		res  client.AddResult
		want string
	}{
		{"birth", client.AddResult{Created: true}, "added"},
		{"correction", client.AddResult{Changed: true}, "updated"},
		{"touch", client.AddResult{}, "touched (same value)"},
		{"correction resurrection", client.AddResult{Changed: true, Resurrected: true}, "updated, resurrected an obsolete fact"},
	}
	for _, c := range cases {
		if got := addVerb(c.res); got != c.want {
			t.Errorf("%s: addVerb = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestAddSummaryNoteNamesTheDrop pins the CLI hint for a dropped summary: the
// resurrection branch reports Changed=true, so the note cannot be derived
// from the shape alone, and the advice must be CLI-executable (no `velruna
// patch` command exists).
func TestAddSummaryNoteNamesTheDrop(t *testing.T) {
	if got := addSummaryNote(client.AddResult{}); got != "" {
		t.Errorf("addSummaryNote (nothing dropped) = %q, want empty", got)
	}
	touch := addSummaryNote(client.AddResult{SummaryIgnored: true})
	if !strings.Contains(touch, "same-value touch") || !strings.Contains(touch, "correct the value") {
		t.Errorf("touch note = %q, want the same-value wording with CLI advice", touch)
	}
	resurrect := addSummaryNote(client.AddResult{Changed: true, Resurrected: true, SummaryIgnored: true})
	if !strings.Contains(resurrect, "stored value was unchanged") || strings.Contains(resurrect, "same-value touch") {
		t.Errorf("resurrect note = %q, want the unchanged-value wording, not the touch phrasing", resurrect)
	}
	for _, s := range []string{touch, resurrect} {
		if strings.Contains(s, "velruna patch") {
			t.Errorf("note = %q names a command the CLI does not have", s)
		}
	}
}
