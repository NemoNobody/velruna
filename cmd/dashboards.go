package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/dashboards"

	"github.com/spf13/cobra"
)

// DashboardsCmd returns the `velruna dashboards` command group: maintenance of
// the OpenSearch Dashboards side of the store.
func DashboardsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dashboards",
		Short: "Install the ready-made OpenSearch Dashboards bundle",
	}
	cmd.AddCommand(dashboardsPushCmd())
	return cmd
}

func dashboardsPushCmd() *cobra.Command {
	// Flags live in the constructor, not in a package-level struct: the command
	// tree is documented as independent per NewRootCmd call, and a shared
	// struct would leak state between two trees built in one process.
	var flags struct {
		conf  string
		index string
		file  string
	}

	push := &cobra.Command{
		Use:   "push",
		Short: "Write the overview bundle into the global Dashboards tenant",
		Long: `Write the bundled index patterns, visualizations and the "Velruna — store
overview" dashboard into the global tenant.

The objects are written straight into the saved-objects index behind the
.kibana alias, with their stable velruna-store-* ids, so the command is
re-runnable: a second run overwrites the same documents instead of creating
duplicates. The saved-objects REST API is not used because it cannot target the
global tenant with basic-auth credentials (the tenant follows the browser
cookie, so an import silently lands in the personal tenant).

Credentials and the address come from the same config, keyring and environment
chain as every other command. The Dashboards index only exists after
OpenSearch Dashboards has started at least once against the cluster.`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			cfg, err := loadDashboardsConfig(flags.conf)
			if err != nil {
				fatalErr(err)
			}
			// No EnsureIndex: pushing dashboards must not create store indices
			// as a side effect (it is a read-mostly maintenance action).
			c, err := client.NewClient(cfg)
			if err != nil {
				fatalErr(err)
			}
			bundle := dashboards.Bundle()
			if flags.file != "" {
				bundle, err = os.ReadFile(flags.file)
				if err != nil {
					fatalErr(fmt.Errorf("read bundle: %w", err))
				}
			}
			res, err := c.PushDashboards(context.Background(), bundle, flags.index)
			// Report before failing: when the final refresh is rejected the
			// objects are already written, and the operator needs the counts to
			// know that a retry is safe rather than duplicated.
			if res != nil {
				if werr := printDashboardPush(os.Stdout, os.Stderr, res, cfg.EffectiveURLs()[0], err); werr != nil {
					fatalErr(werr)
				}
			}
			if err != nil {
				fatalErr(err)
			}
			if res.Failed > 0 {
				os.Exit(1)
			}
		},
	}
	push.Flags().StringVar(&flags.conf, "conf", "",
		"config file to use (overrides VELRUNA_CONF and the default lookup)")
	push.Flags().StringVar(&flags.index, "index", "",
		"saved-objects write index (default: resolve the .kibana alias)")
	push.Flags().StringVar(&flags.file, "file", "",
		"bundle to push (default: the one embedded in this binary)")
	return push
}

func loadDashboardsConfig(conf string) (*config.Config, error) {
	if conf != "" {
		return config.LoadConfigFrom(conf)
	}
	return config.LoadConfig()
}

// printDashboardPush reports what landed and how to see it. The tenant and
// reload notes are not decoration: the UI keeps the index-pattern list per
// session, so a fresh push looks absent until the tab is reloaded, and any
// tenant other than Global shows the personal copy instead. runErr suppresses
// the success hint: a failed refresh means the objects are not visible yet.
func printDashboardPush(out, errOut io.Writer, res *client.DashboardPush, url string, runErr error) error {
	target, tenant := res.Index, "requested index"
	if res.Global {
		// Writes go through the alias; the resolved index is what it pointed at
		// when the command probed it (a migration may have moved it since).
		target, tenant = ".kibana", "global tenant"
	}
	line := fmt.Sprintf("indexed %d saved object(s) into %s, %s, at %s", res.Indexed, target, tenant, url)
	if res.Global {
		line = fmt.Sprintf("indexed %d saved object(s) into .kibana, global tenant, at %s "+
			"(write index %s at probe time)", res.Indexed, url, res.Index)
	}
	if err := writeLine(out, line); err != nil {
		return err
	}
	if res.Unverified {
		if _, werr := fmt.Fprintf(errOut, "warning: %s could not be read (mapping probe refused): "+
			"it may be the wrong index — the writes are the only check\n", res.Index); werr != nil {
			return werr
		}
	}
	for _, e := range res.Errors {
		if _, werr := fmt.Fprintf(errOut, "failed: %s\n", e); werr != nil {
			return werr
		}
	}
	if res.Failed > 0 {
		if _, werr := fmt.Fprintf(errOut, "%d object(s) failed\n", res.Failed); werr != nil {
			return werr
		}
		// Every object failing points at the target, not at the objects: the
		// usual cause is a mistyped --index whose probe was refused (a write-only
		// role cannot read the mapping) and which auto-create then made.
		if res.Objects > 0 && res.Failed == res.Objects {
			if _, werr := fmt.Fprintf(errOut, "nothing was written: check the target index %q "+
				"(and that --index is an index, not an alias or pattern)\n", res.Index); werr != nil {
				return werr
			}
		}
		return nil
	}
	if runErr != nil || !res.Global {
		return nil
	}
	return writeLine(out, "open Dashboards, pick the Global tenant in the tenant switcher and "+
		"hard-reload the tab (the UI caches the index-pattern list per session)")
}
