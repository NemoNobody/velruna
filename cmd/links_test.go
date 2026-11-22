package cmd

import (
	"strings"
	"testing"

	"github.com/NemoNobody/velruna/internal/client"
)

// The honest-counts contract: not-fetched edges get the raise-limit advice,
// unreadable hits never do, and "clean" is reserved for a fully audited
// graph. These branches drive exit-code decisions, so they are pinned as
// pure functions rather than left to live verification.
func TestLinksSummaryLineCounts(t *testing.T) {
	// Everything audited: no extra clauses.
	rep := &client.LinksReport{Edges: 5, Fetched: 5, Total: 5}
	if got := linksSummaryLine(rep, 0); got != "links: 5 edge(s) inspected" {
		t.Errorf("clean scan: %q", got)
	}

	// Unreadable hits were fetched already: raising --limit recovers
	// nothing, so the summary must not suggest it.
	rep = &client.LinksReport{Edges: 5, Fetched: 6, Total: 6, Skipped: 1, Truncated: true}
	got := linksSummaryLine(rep, 0)
	if !strings.Contains(got, "1 hit(s) carried a superseded_by this client cannot read") {
		t.Errorf("unreadable hits unreported: %q", got)
	}
	if strings.Contains(got, "raise --limit") {
		t.Errorf("wrong advice for fetched-but-unreadable hits: %q", got)
	}

	// Edges the scan never returned: raise --limit is right advice.
	rep = &client.LinksReport{Edges: 5, Fetched: 5, Total: 12, Truncated: true}
	got = linksSummaryLine(rep, 5)
	if !strings.Contains(got, "7 edge(s) in the store not fetched (raise --limit)") {
		t.Errorf("not-fetched advice missing: %q", got)
	}

	// At the window edge the advice must say what raising can and cannot
	// do: from limit=9999 on the scan already asks for the whole result
	// window, so no higher limit fetches another edge. The pair here is
	// exactly what the scan returns at limit=9999: 9999 edges kept, the
	// +1 probe fetched but trimmed, the rest beyond the window.
	for _, lim := range []int{9999, 10000, 20000} {
		rep = &client.LinksReport{Edges: client.LinkScanMax - 1, Fetched: client.LinkScanMax, Total: 12345, Truncated: true}
		got = linksSummaryLine(rep, lim)
		if strings.Contains(got, "not fetched (raise --limit)") || !strings.Contains(got, "beyond the 10000-hit result window") {
			t.Errorf("window-span advice wrong at --limit %d: %q", lim, got)
		}
	}

	// The pair at limit=9999 must not contradict itself: the missed edge
	// is beyond the window (raising fetches nothing), while the trimmed
	// probe edge IS recoverable by raising (to 10000 the trim stops).
	rep = &client.LinksReport{Edges: 9999, Fetched: 10000, Total: 10001, Truncated: true}
	got = linksSummaryLine(rep, 9999)
	if !strings.Contains(got, "1 edge(s) beyond the 10000-hit result window") {
		t.Errorf("missed clause wrong at 9999: %q", got)
	}
	if !strings.Contains(got, "1 edge(s) fetched but left unaudited at --limit (raise --limit)") {
		t.Errorf("trimmed clause must offer the raise that actually audits it: %q", got)
	}

	// A scan over a store of exactly limit+1 edges fetched everything but
	// trimmed the +1 probe back: the summary must still show the gap the
	// verdict's "NOT fully audited" points to.
	rep = &client.LinksReport{Edges: 1000, Fetched: 1001, Total: 1001, Truncated: true}
	got = linksSummaryLine(rep, 1000)
	if !strings.Contains(got, "1 edge(s) fetched but left unaudited at --limit") {
		t.Errorf("trimmed probe edge invisible in the summary: %q", got)
	}
	if strings.Contains(got, "not fetched (raise --limit)") {
		t.Errorf("nothing was left unfetched, raise-advice wrong: %q", got)
	}
}

func TestLinksVerdict(t *testing.T) {
	if got := linksVerdict(&client.LinksReport{Edges: 1, Fetched: 1, Total: 1}); !strings.HasPrefix(got, "clean:") {
		t.Errorf("fully audited and quiet graph: %q", got)
	}
	// Skipped or not-fetched edges must keep "clean" unclaimed.
	if got := linksVerdict(&client.LinksReport{Edges: 1, Fetched: 2, Total: 2, Skipped: 1, Truncated: true}); !strings.Contains(got, "NOT fully audited") {
		t.Errorf("partial audit claimed clean: %q", got)
	}
	if got := linksVerdict(&client.LinksReport{Edges: 1, Fetched: 1, Total: 2, Truncated: true}); !strings.Contains(got, "NOT fully audited") {
		t.Errorf("unfetched edges claimed clean: %q", got)
	}
	if got := linksVerdict(&client.LinksReport{Findings: []client.LinkFinding{{Key: "a", Target: "b", Problem: "dangling"}}}); !strings.Contains(got, "velruna_patch") {
		t.Errorf("findings must carry the repair line: %q", got)
	}
}
