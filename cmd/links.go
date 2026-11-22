package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"
)

// LinksCmd is the link-graph namespace. superseded_by edges are guarded at
// write time, but the guards are advisory and data predates them: this audits
// the whole graph in one bounded read.
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
		Long: `Walk every fact that carries a superseded_by link and report the broken edges:

  dangling    the target fact does not exist (a reader walks into nothing)
  unverified  the target fact exists but is not verified (obsolete or other)
  cycle       the edge is part of a superseded_by loop

The scan is bounded (--limit, default 1000, max 10000 edges); a truncated
scan says so. Exit codes distinguish outcomes: 0 when the graph is clean,
1 when findings exist, 2 when the check itself failed (unreachable store) —
scripts can branch on them without parsing stderr. Repair a finding by
repointing the source (velruna_patch superseded_by) or retiring it too.
Run it occasionally; the write guards prevent most breaks but never all
of them.`,
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
			fmt.Printf("links: %d edge(s) inspected", rep.Edges)
			if rep.Skipped > 0 {
				fmt.Printf(", %d hit(s) unreadable", rep.Skipped)
			}
			if rep.Truncated {
				fmt.Printf(" (%d edges in the store; raise --limit)", rep.Total)
			}
			fmt.Println()
			for _, f := range rep.Findings {
				fmt.Printf("%-10s %s -> %s: %s\n", f.Problem,
					strconv.Quote(f.Key), strconv.Quote(f.Target), f.Detail)
			}
			if len(rep.Findings) > 0 {
				fmt.Println("repair: velruna_patch {key, superseded_by: <live verified target>} to repoint, " +
					"or retire the source as well")
				os.Exit(1)
			}
			fmt.Println("clean: every superseded_by link resolves to a verified fact")
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max edges to inspect (default 1000, max 10000)")
	return cmd
}

// fatalCheckFailed separates "the audit could not run" (exit 2) from "the
// audit ran and found broken edges" (exit 1): an operator must never read a
// store outage as a clean or broken graph.
func fatalCheckFailed(err error) {
	_, _ = io.WriteString(os.Stderr, err.Error()+"\n")
	os.Exit(2)
}
