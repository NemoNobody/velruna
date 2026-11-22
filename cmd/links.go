package cmd

import (
	"context"
	"fmt"
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
scan says so. Exit code 1 when findings exist, 0 when the graph is clean.
Repair a finding by repointing the source (velruna_patch superseded_by) or
retiring it too. Run it occasionally; the write guards prevent most breaks
but never all of them.`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fatalErr(err)
			}
			rep, err := c.CheckLinks(ctx, limit)
			if err != nil {
				fatalErr(err)
			}
			suffix := ""
			if rep.Truncated {
				suffix = fmt.Sprintf(" (%d in the store; raise --limit)", rep.Total)
			}
			fmt.Printf("links: %d edge(s) inspected%s\n", rep.Edges, suffix)
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
