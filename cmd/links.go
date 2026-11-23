package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/spf13/cobra"
)

// LinksCmd is the link-graph namespace. superseded_by edges are guarded at
// write time, but the guards are advisory and data predates them: this audits
// the whole graph in one bounded scan plus a batched probe round.
func LinksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "links",
		Short: "Audit the superseded_by link graph",
	}
	cmd.AddCommand(linksCheckCmd())
	return cmd
}

func linksCheckCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Find dangling, unverified or cyclic superseded_by links",
		Long: fmt.Sprintf(`Walk every live (verified) fact that carries a superseded_by link and report
the broken edges. Edges on retired facts are counted apart, not reported:
the audit looks for live breakage, and retiring a source is the documented
way to take its dead edge out of it.

  dangling    the target fact does not exist (a reader walks into nothing)
  unverified  the target fact exists but is not verified (obsolete or other)
  cycle       the edge is part of a superseded_by loop

The scan is bounded (--limit, default %d, max %d edges); a truncated
scan says so. Exit codes distinguish outcomes: 0 when no findings exist
(the closing line says when the graph was not fully audited), 1 when
findings exist, 2 when the check itself failed (unreachable store) —
scripts can branch on them without parsing stderr. Repair a finding by
repointing the source (the MCP tool velruna_patch with superseded_by)
or retiring it too.
Run it occasionally; the write guards prevent most breaks but never all
of them.`, client.LinkScanDefault, client.LinkScanMax),
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fatalCheckFailed(err)
			}
			rep, err := c.CheckLinks(ctx, limit)
			if err != nil {
				fatalCheckFailed(err)
			}
			fmt.Println(linksSummaryLine(rep, limit))
			for _, f := range rep.Findings {
				fmt.Printf("%-10s %s -> %s: %s\n", f.Problem,
					strconv.Quote(f.Key), strconv.Quote(f.Target), f.Detail)
			}
			// The verdict distinguishes "fully audited, clean" from "nothing
			// broken among what was audited"; findings exit 1 so scripts can
			// branch without parsing stdout.
			fmt.Println(linksVerdict(rep))
			if len(rep.Findings) > 0 {
				os.Exit(1)
			}
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0,
		fmt.Sprintf("max edges to inspect (default %d, max %d)", client.LinkScanDefault, client.LinkScanMax))
	return cmd
}

// linksSummaryLine is the pure formatting of the scan accounting, kept out
// of the Run closure so the honest-counts contract is pinned by tests.
func linksSummaryLine(rep *client.LinksReport, limit int) string {
	line := fmt.Sprintf("links: %d live edge(s) inspected", rep.Edges)
	if rep.Retired > 0 {
		line += fmt.Sprintf(", %d edge(s) on retired facts (not audited — a retired source's edge is dead weight; retiring is how it leaves the audit)", rep.Retired)
	}
	if rep.Skipped > 0 {
		line += fmt.Sprintf(", %d hit(s) carried a superseded_by this client cannot read (not audited)", rep.Skipped)
	}
	// Only hits the store counted but the scan never returned are
	// recoverable by --limit; unreadable hits were fetched already,
	// and telling the operator to raise the limit for them is wrong
	// advice (cf. fetch/journal, which treat skipped the same way).
	// The +1 probe for "is there more" also counts as fetched, so it
	// never shows up as missed; a trimmed probe edge is reported
	// separately below.
	if missed := rep.Total - rep.Fetched; missed > 0 {
		effectiveLimit := limit
		if effectiveLimit <= 0 {
			effectiveLimit = client.LinkScanDefault
		}
		// The scan window is min(limit+1, max_result_window=10000),
		// so from limit=9999 on raising cannot FETCH another edge —
		// though it can still AUDIT more (the trimmed clause below).
		if effectiveLimit < client.LinkScanMax-1 {
			line += fmt.Sprintf(", %d edge(s) in the store not fetched (raise --limit)", missed)
		} else {
			line += fmt.Sprintf(", %d edge(s) beyond the 10000-hit result window (no higher --limit fetches them)", missed)
		}
	}
	if trimmed := rep.Fetched - rep.Edges - rep.Retired - rep.Skipped; trimmed > 0 {
		line += fmt.Sprintf(", %d edge(s) fetched but left unaudited at --limit (raise --limit)", trimmed)
	}
	return line
}

// linksVerdict is the closing line: "clean" only for a fully audited graph.
func linksVerdict(rep *client.LinksReport) string {
	if len(rep.Findings) > 0 {
		return "repair: repoint with the MCP tool velruna_patch {key, superseded_by: <live verified target>}, " +
			"or retire the source too (velruna obsolete <key>)"
	}
	if rep.Skipped > 0 || rep.Truncated {
		return "no broken edges among the audited links; the graph was NOT fully audited (see the counts above)"
	}
	return "clean: every superseded_by link on a live fact resolves to a verified fact"
}

// fatalCheckFailed separates "the audit could not run" (exit 2) from "the
// audit ran and found broken edges" (exit 1): an operator must never read a
// store outage as a clean or broken graph.
func fatalCheckFailed(err error) {
	_, _ = io.WriteString(os.Stderr, err.Error()+"\n")
	os.Exit(2)
}
