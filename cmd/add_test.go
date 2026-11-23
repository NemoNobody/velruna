package cmd

import (
	"testing"

	"github.com/NemoNobody/velruna/internal/client"
)

// TestAddVerbNamesResurrection pins the CLI wording: a write that flips an
// obsolete fact back to verified must say so, including the same-value touch
// path (where the shape alone reads as "nothing changed").
func TestAddVerbNamesResurrection(t *testing.T) {
	cases := []struct {
		name string
		res  client.AddResult
		want string
	}{
		{"birth", client.AddResult{Created: true}, "added"},
		{"correction", client.AddResult{Changed: true}, "updated"},
		{"touch", client.AddResult{}, "touched (same value)"},
		{"same-value resurrection", client.AddResult{Resurrected: true}, "touched (same value), resurrected an obsolete fact"},
		{"correction resurrection", client.AddResult{Changed: true, Resurrected: true}, "updated, resurrected an obsolete fact"},
	}
	for _, c := range cases {
		if got := addVerb(c.res); got != c.want {
			t.Errorf("%s: addVerb = %q, want %q", c.name, got, c.want)
		}
	}
}
