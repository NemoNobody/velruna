package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
		Short: "Maintain the ready-made OpenSearch Dashboards bundle",
	}
	cmd.AddCommand(dashboardsPushCmd(), dashboardsGenFieldsCmd())
	return cmd
}

// Bundle pattern titles this tool knows how to feed, and how the store index
// behind each one is named in the current configuration. A pattern outside
// this set keeps its baked list: there is no mapping to read for it.
const (
	patternFacts   = "velruna"
	patternHistory = "velruna-history-*"
	patternDocs    = "velruna-docs"
)

func storeIndexForTitle(cfg *config.Config, title string) string {
	switch title {
	case patternFacts:
		return cfg.IndexName
	case patternHistory:
		return cfg.IndexName + "-history-*"
	case patternDocs:
		return cfg.DocsIndexName
	default:
		return ""
	}
}

// dashboardsGenFieldsCmd bakes the live index-pattern field lists back into
// the bundle. OSD 3.x cannot introspect fields over an API the way the bundle
// needs, so the lists are stored up front: a pattern with fields:[] cannot
// resolve its timeFieldName and every visualization on it dies with "Could not
// locate that index-pattern-field". Re-run this after a mapping change, rebuild
// the binary (the bundle is embedded) and push again.
func dashboardsGenFieldsCmd() *cobra.Command {
	var flags struct {
		conf  string
		file  string
		out   string
		index string
	}

	gen := &cobra.Command{
		Use:   "gen-fields",
		Short: "Bake the field lists of the bundled index patterns from a live store",
		Long: `Read the mappings of the store this configuration points at and rewrite the
"fields" list of each bundled index pattern (velruna, velruna-history-*,
velruna-docs) in the bundle file.

This is a development tool, run from a checkout: it regenerates the bundle that
is later embedded in the binary. Everything else about a bundle stays byte for
byte identical, so the diff shows the mapping change and nothing else. A pattern
whose index does not exist keeps its old list and is reported.`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			if err := genFields(cmd.OutOrStdout(), cmd.ErrOrStderr(), flags.conf, flags.file, flags.out, flags.index); err != nil {
				fatalErr(err)
			}
		},
	}
	gen.Flags().StringVar(&flags.conf, "conf", "",
		"config file to use (overrides VELRUNA_CONF and the default lookup)")
	gen.Flags().StringVar(&flags.file, "file", "",
		"bundle to regenerate (default: the one embedded in this binary)")
	gen.Flags().StringVar(&flags.out, "out", "",
		"where to write the regenerated bundle (default: internal/dashboards/objects.ndjson)")
	gen.Flags().StringVar(&flags.index, "index", "",
		"feed a pattern this store index instead (for bundles with custom pattern titles)")
	return gen
}

// genFields reads the store's mappings and rewrites the bundle's pattern field
// lists. It returns an error instead of exiting so the failure paths (nothing
// resolved, an output path that is not a checkout) are testable.
func genFields(out, errOut io.Writer, conf, file, outPath, indexOverride string) error {
	cfg, err := loadDashboardsConfig(conf)
	if err != nil {
		return err
	}
	c, err := client.NewClient(cfg)
	if err != nil {
		return err
	}
	bundle := dashboards.Bundle()
	if file != "" {
		bundle, err = os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read bundle: %w", err)
		}
	}
	objs, err := dashboards.Parse(bundle)
	if err != nil {
		return err
	}
	type target struct{ title, index string }
	var targets []target
	seen := map[string]bool{}
	for _, o := range objs {
		if o.Type != "index-pattern" {
			continue
		}
		var attrs struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal(o.Attributes, &attrs); err != nil {
			return fmt.Errorf("%s: attributes: %w", o.DocumentID(), err)
		}
		if seen[attrs.Title] {
			continue
		}
		idx := storeIndexForTitle(cfg, attrs.Title)
		if idx == "" && indexOverride != "" {
			idx = indexOverride
		}
		if idx == "" {
			continue
		}
		seen[attrs.Title] = true
		targets = append(targets, target{attrs.Title, idx})
	}
	if len(targets) == 0 {
		return fmt.Errorf("the bundle carries no index pattern this command can feed (%s, %s, %s); pass --index for a bundle with custom pattern titles",
			patternFacts, patternHistory, patternDocs)
	}
	fieldsByTitle := map[string]string{}
	for _, t := range targets {
		props, _, err := c.IndexMappingProps(context.Background(), t.index)
		if err != nil {
			if _, werr := fmt.Fprintf(errOut, "%s: mapping unavailable (%v): fields unchanged\n", t.title, err); werr != nil {
				return werr
			}
			continue
		}
		fields := dashboards.FieldsFromMapping(props)
		rendered, err := dashboards.RenderFields(fields)
		if err != nil {
			return fmt.Errorf("%s: %w", t.title, err)
		}
		fieldsByTitle[t.title] = rendered
		if err := writeLine(out, fmt.Sprintf("%s: %d field(s) baked from %s", t.title, len(fields), t.index)); err != nil {
			return err
		}
	}
	if len(fieldsByTitle) == 0 {
		return errors.New("no mapping could be read: nothing was written")
	}
	updated, err := dashboards.RewriteFields(bundle, fieldsByTitle)
	if err != nil {
		return err
	}
	if outPath == "" {
		outPath = filepath.Join("internal", "dashboards", "objects.ndjson")
		if _, err := os.Stat(outPath); err != nil {
			// Writing to the default path from outside a checkout would scatter
			// generated files; --out says what was meant.
			return fmt.Errorf("%s not found: run this from the repository root or pass --out", outPath)
		}
		if file != "" && file != outPath {
			return fmt.Errorf("--file and the default output are different files (%s, %s): pass --out to say where the regenerated bundle goes", file, outPath)
		}
	}
	if err := os.WriteFile(outPath, updated, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	return writeLine(out, outPath+" updated")
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
