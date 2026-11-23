package cmd

import (
	"context"
	"io"
	"strconv"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/version"

	"github.com/spf13/cobra"
)

// noteSkipped reports hits the store returned but velruna could not decode.
// Silent drops train agents (and users) to trust an incomplete view; a
// visible note makes index corruption actionable.
func noteSkipped(w io.Writer, meta client.SearchMeta) error {
	if meta.Skipped <= 0 {
		return nil
	}
	return writeLine(w, "note: "+strconv.Itoa(meta.Skipped)+
		" matching hit(s) failed to decode (index corruption? report this to the user)")
}

// NewRootCmd builds and returns the root command with all subcommands
// registered. It is safe to call multiple times: each call returns a fresh,
// independent command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "velruna",
		Short:   "Verified-fact memory",
		Version: version.Version,
	}
	root.AddCommand(
		SearchCmd(),
		GetCmd(),
		KeysCmd(),
		AddCmd(),
		ObsoleteCmd(),
		PruneCmd(),
		JournalCmd(),
		HistoryInfoCmd(),
		MigrateHistoryCmd(),
		ListCmd(),
		StatusCmd(),
		FetchCmd(),
		DocCmd(),
		DocsCmd(),
		DashboardsCmd(),
		CredsCmd(),
		LinksCmd(),
		WorksetCmd(),
		ExecCmd(),
		McpCmd(),
	)
	return root
}

// newClient creates a client and ensures the index exists.
func newClient() (*client.Client, error) {
	c, _, err := newClientWithConfig()
	return c, err
}

// newClientWithConfig is newClient plus the effective config, for commands
// that need configuration values beyond the connection (e.g. prune's default
// --keep from history_limit).
func newClientWithConfig() (*client.Client, *config.Config, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, nil, err
	}
	c, err := newClientFromConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	return c, cfg, nil
}

// newClientFromConfig builds a client for an already-loaded config: commands
// that guard the write path load the config once and hand it here, so the
// config (and the keyring, behind a locked wallet) is touched exactly once
// per command.
func newClientFromConfig(cfg *config.Config) (*client.Client, error) {
	c, err := client.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	if err := c.EnsureIndex(ctx); err != nil {
		return nil, err
	}
	return c, nil
}
