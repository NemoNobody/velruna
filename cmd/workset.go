package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/model"

	"github.com/spf13/cobra"
)

// Worksets are windows over entries: a workset can hold thousands of them, and
// a show call that dumps all of them is exactly the context flood the compact
// fact listings avoid. The defaults mirror the fact-reading tools.
const (
	worksetDefaultLimit = 50
	worksetMaxLimit     = 200

	// worksetStateReadCap bounds one fact_state batch. A workset can hold
	// thousands of entries; asking the facts index for every id would flood a
	// single search. Ids beyond the cap are not queried and render as unknown
	// (the documented partial-read state), never as verified.
	worksetStateReadCap = 200
)

// fact_state values as rendered by show. They are plain strings here so the
// window/table helpers stay independent of the client package (and testable
// without a store).
const (
	worksetStateVerified = "verified"
	worksetStateObsolete = "obsolete"
	worksetStateMissing  = "missing"
	worksetStateUnknown  = "unknown"
)

// worksetFlags holds the flags of every workset subcommand. The fields are
// per-subcommand (no flag is bound twice) so the shared struct cannot leak one
// command's defaults into another.
var worksetFlags struct {
	// create
	createProject string
	// list
	listProject string
	// add
	addPhase string
	addNote  string
	// show
	showAfter    int
	showBefore   int
	showLimit    int
	showPhases   string
	showLiveOnly bool
	showJournal  int
	// phase
	toPhase       string
	phaseNote     string
	takeover      bool
	takeoverAfter time.Duration
	// delete
	yes bool
	// guard confirmation shared by create/add/phase (only one command binds it)
	allowSecret bool
	// cursor
	at     int
	clear  bool
	author string
	host   string
}

// WorksetCmd builds the `velruna workset` command tree: a named, shared,
// ordered list of fact references with per-entry phases and per-reader cursors.
func WorksetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workset",
		Short: "Named shared queues of facts, with phases and per-reader cursors",
		Long: `A workset is a named, ordered list of references to facts, with a phase per
entry and a cursor per reader. It is the unit two agents coordinate on: the
entries are the queue, and each entry's phase and owner are the shared state
that would otherwise live in one agent's context.

A workset is a work queue over facts, not a copy of them: every entry names a
fact_id, and the fact's value stays in the facts index. show resolves each
entry's fact_state (verified | obsolete | missing | unknown) so a queue cannot
silently point at a retired fact.

Phases: todo | in_progress | blocked | testing | done | cancelled. done is
reachable only through testing; in_progress/blocked carry an ownership lock
that another author may break with --takeover or after the entry has been
silent longer than workset_takeover_seconds.`,
	}
	cmd.AddCommand(
		worksetCreateCmd(),
		worksetListCmd(),
		worksetShowCmd(),
		worksetAddCmd(),
		worksetRemoveCmd(),
		worksetPhaseCmd(),
		worksetRenameCmd(),
		worksetDeleteCmd(),
		worksetCursorCmd(),
	)
	return cmd
}

func worksetCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name> [--project P]",
		Short: "Create a workset",
		Long: `Create a named workset. The name is a slug (1..64 characters from a-z, 0-9
and '_', no leading/trailing '_', no '__'): it is the handle commands and
agents type. Names are globally unique; the project label groups them for
listing (default ` + model.DefaultProject + `).`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			name, err := model.NormalizeName(args[0])
			if err != nil {
				fatalErr(err)
			}
			project := strings.TrimSpace(worksetFlags.createProject)
			if project == "" {
				project = model.DefaultProject
			}
			cfg, err := config.LoadConfig()
			if err != nil {
				fatalErr(err)
			}
			// The project label is stored and matched on, so it is an identity
			// field: a finding refuses the write instead of rewriting it.
			if err := guardCheck(cfg.SecretGuard, worksetFlags.allowSecret, guardField{Name: "project", Text: project}); err != nil {
				fatalErr(err)
			}
			ctx := context.Background()
			c, labels, err := newWorksetClient(ctx)
			if err != nil {
				fatalErr(err)
			}
			ws, err := c.CreateWorkset(ctx, client.WorksetCreate{
				Name:    name,
				Project: project,
				By:      labels.Author,
			})
			if err != nil {
				fatalErr(worksetError(err))
			}
			if err := writeLine(os.Stdout, worksetSummary(ws)); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().StringVar(&worksetFlags.createProject, "project", model.DefaultProject,
		"project label (names are globally unique; the label only groups listings)")
	cmd.Flags().BoolVar(&worksetFlags.allowSecret, "allow-secret", false,
		"store a guard-flagged project label as written (secret_guard=ask only)")
	return cmd
}

func worksetListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list [--project P]",
		Short: "List worksets (optionally limited to one project)",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			project := strings.TrimSpace(worksetFlags.listProject)
			ctx := context.Background()
			c, _, err := newWorksetClient(ctx)
			if err != nil {
				fatalErr(err)
			}
			sets, err := c.ListWorksets(ctx, project)
			if err != nil {
				fatalErr(worksetError(err))
			}
			if len(sets) == 0 {
				line := "no worksets"
				if project != "" {
					line += " for project " + strconv.Quote(project)
				}
				if err := writeLine(os.Stdout, line); err != nil {
					fatalErr(err)
				}
				return
			}
			var b strings.Builder
			for _, ws := range sets {
				if ws == nil {
					continue
				}
				b.WriteString(worksetSummary(ws))
				b.WriteString("\n")
			}
			if _, err := io.WriteString(os.Stdout, b.String()); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().StringVar(&worksetFlags.listProject, "project", "",
		"limit to this project label (omit: every project)")
	return cmd
}

func worksetShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <name> [--after N] [--before N] [--limit N] [--phase P,...] [--live-only] [--journal N]",
		Short: "Show a workset's entries, presence and journal",
		Long: `Show the entries of a workset with their fact_state, phase, owner and the
age of the current phase. Also prints the in_progress presence line
(author@host@index), the per-reader cursor positions (author@host@index, newest
first), a self_verified marker for done entries, and optionally the change
journal.

Filters apply in this order: --phase and --live-only first, then the
positional window (--after/--before, both exclusive, 1-based positions in the
filtered view), then --limit. --live-only keeps only entries whose fact is
still verified. The presence, cursor and self_verified lines are computed from
the whole workset, not from the filtered window.`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			name, err := model.NormalizeName(args[0])
			if err != nil {
				fatalErr(err)
			}
			after, before := worksetFlags.showAfter, worksetFlags.showBefore
			if after < 0 {
				fatalErr(fmt.Errorf("--after must be >= 0 (got %d): positions are 1-based in the filtered view", after))
			}
			if before < 0 {
				fatalErr(fmt.Errorf("--before must be >= 0 (got %d): positions are 1-based in the filtered view", before))
			}
			limit, err := worksetLimit(worksetFlags.showLimit)
			if err != nil {
				fatalErr(err)
			}
			journalN, err := worksetJournalCount(worksetFlags.showJournal)
			if err != nil {
				fatalErr(err)
			}
			phases, err := parsePhaseList(worksetFlags.showPhases)
			if err != nil {
				fatalErr(err)
			}
			ctx := context.Background()
			c, cfg, labels, err := newWorksetClientCfg(ctx)
			if err != nil {
				fatalErr(err)
			}
			ws, err := c.GetWorkset(ctx, name)
			if err != nil {
				fatalErr(worksetError(err))
			}
			states, err := worksetStates(ctx, c, ws.Entries)
			if err != nil {
				fatalErr(worksetError(err))
			}
			// The filters and the positional window are applied first; the limit is
			// the last cut, so the omitted count is measured against the filtered
			// view the reader actually asked about.
			filtered := worksetWindow(ws.Entries, states, phases, worksetFlags.showLiveOnly, after, before, 0)
			rows := filtered
			omitted := 0
			if limit > 0 && len(filtered) > limit {
				rows = filtered[:limit]
				omitted = len(filtered) - limit
			}
			// Presence, cursors and self_verified describe the whole workset: a
			// filtered table must not hide the other in_progress claims or the
			// cursor positions the reader came to see.
			allRows := worksetWindow(ws.Entries, states, nil, false, 0, 0, 0)
			var journal []model.WorksetJournalEntry
			if journalN > 0 {
				journal, err = c.WorksetJournal(ctx, name, journalN)
				if err != nil {
					fatalErr(worksetError(err))
				}
			}
			if err := printWorkset(os.Stdout, ws, rows, allRows, omitted, labels.Host, journal, journalN > 0,
				time.Now().UTC(), worksetTakeoverWindow(cfg)); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().IntVar(&worksetFlags.showAfter, "after", 0,
		"keep entries after this 1-based position (exclusive)")
	cmd.Flags().IntVar(&worksetFlags.showBefore, "before", 0,
		"keep entries before this 1-based position (exclusive)")
	cmd.Flags().IntVar(&worksetFlags.showLimit, "limit", 0,
		"max entries to show (default 50, cap 200)")
	cmd.Flags().StringVar(&worksetFlags.showPhases, "phase", "",
		"comma-separated phase filter, e.g. todo,in_progress (applied before the positional window)")
	cmd.Flags().BoolVar(&worksetFlags.showLiveOnly, "live-only", false,
		"keep only entries whose fact is still verified (drop obsolete, missing and unknown facts)")
	cmd.Flags().IntVar(&worksetFlags.showJournal, "journal", 0,
		"also print the newest N journal entries (0: no journal)")
	return cmd
}

func worksetAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <name> <fact_id>...",
		Short: "Add fact references to a workset",
		Long: `Add one or more facts to a workset. The initial phase defaults to todo; done
is accepted on purpose for reference entries (a documented exception to the
testing-only entry rule, used when the queue lists work already finished).`,
		Args: cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			name, err := model.NormalizeName(args[0])
			if err != nil {
				fatalErr(err)
			}
			ids, err := normalizeFactIDs(args[1:])
			if err != nil {
				fatalErr(err)
			}
			phase, err := parsePhaseValue(worksetFlags.addPhase)
			if err != nil {
				fatalErr(err)
			}
			if phase == "" {
				phase = model.PhaseTodo
			}
			cfg, err := config.LoadConfig()
			if err != nil {
				fatalErr(err)
			}
			// A note is rewritable text: a high-confidence credential refuses the
			// write, a heuristic hit is redacted in place (the same policy as fact
			// notes), and the audit rows are printed below.
			redactions, gErr := guardSanitize(cfg.SecretGuard, worksetFlags.allowSecret,
				textField("note", &worksetFlags.addNote))
			if gErr != nil {
				fatalErr(gErr)
			}
			ctx := context.Background()
			// AddEntries takes no author: the client resolves the writer label
			// for the journal itself, exactly like the fact write paths.
			c, _, err := newWorksetClient(ctx)
			if err != nil {
				fatalErr(err)
			}
			ws, err := c.AddEntries(ctx, name, client.WorksetAdd{
				FactIDs: ids,
				Note:    worksetFlags.addNote,
				Phase:   phase,
			})
			if err != nil {
				fatalErr(worksetError(err))
			}
			if err := writeLine(os.Stdout, worksetSummary(ws)); err != nil {
				fatalErr(err)
			}
			if err := writeLine(os.Stdout, "requested: "+strconv.Itoa(len(ids))+" fact ids; already-present entries are skipped"); err != nil {
				fatalErr(err)
			}
			if s := redactionLines(redactions); s != "" {
				if err := writeLine(os.Stdout, s); err != nil {
					fatalErr(err)
				}
			}
		},
	}
	cmd.Flags().StringVar(&worksetFlags.addPhase, "phase", "",
		"initial phase (todo|in_progress|blocked|testing|done|cancelled); default todo")
	cmd.Flags().StringVar(&worksetFlags.addNote, "note", "", "note stored on each added entry")
	cmd.Flags().BoolVar(&worksetFlags.allowSecret, "allow-secret", false,
		"store a guard-flagged note as written (secret_guard=ask only)")
	return cmd
}

func worksetRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <name> <fact_id>...",
		Short: "Remove fact references from a workset",
		Args:  cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			name, err := model.NormalizeName(args[0])
			if err != nil {
				fatalErr(err)
			}
			ids, err := normalizeFactIDs(args[1:])
			if err != nil {
				fatalErr(err)
			}
			ctx := context.Background()
			c, labels, err := newWorksetClient(ctx)
			if err != nil {
				fatalErr(err)
			}
			ws, err := c.RemoveEntries(ctx, name, ids, labels.Author)
			if err != nil {
				fatalErr(worksetError(err))
			}
			if err := writeLine(os.Stdout, worksetSummary(ws)); err != nil {
				fatalErr(err)
			}
			if err := writeLine(os.Stdout, "requested: "+strconv.Itoa(len(ids))+" fact ids; absent ids are no-ops"); err != nil {
				fatalErr(err)
			}
		},
	}
	return cmd
}

func worksetPhaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "phase <name> <fact_id>... --to P [--note S] [--takeover] [--takeover-after D]",
		Short: "Move workset entries to another phase",
		Long: `Move entries to another phase. The phase matrix and the ownership lock are
enforced by the store: a claim held by another author is broken only by
--takeover, by moving the entry to todo, or by staleness (the claim went quiet
longer than workset_takeover_seconds).

--takeover-after overrides the staleness window for this call (whole seconds;
the configured value is used when the flag is omitted).`,
		Args: cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			name, err := model.NormalizeName(args[0])
			if err != nil {
				fatalErr(err)
			}
			ids, err := normalizeFactIDs(args[1:])
			if err != nil {
				fatalErr(err)
			}
			to, err := parsePhaseValue(worksetFlags.toPhase)
			if err != nil {
				fatalErr(err)
			}
			if to == "" {
				fatalErr(errors.New("--to is required (todo|in_progress|blocked|testing|done|cancelled)"))
			}
			if worksetFlags.takeoverAfter < 0 {
				fatalErr(fmt.Errorf("--takeover-after must be > 0 when set (got %s); omit it to use workset_takeover_seconds", worksetFlags.takeoverAfter))
			}
			ctx := context.Background()
			cfg, err := config.LoadConfig()
			if err != nil {
				fatalErr(err)
			}
			redactions, gErr := guardSanitize(cfg.SecretGuard, worksetFlags.allowSecret,
				textField("note", &worksetFlags.phaseNote))
			if gErr != nil {
				fatalErr(gErr)
			}
			if d := worksetFlags.takeoverAfter; d > 0 {
				// The store-side client reads the staleness window from its
				// config; the CLI override is expressed in whole seconds because
				// workset_takeover_seconds is an int. Round up so a sub-second
				// value cannot collapse to the rejected zero window.
				cfg.WorksetTakeoverSeconds = int((d + time.Second - 1) / time.Second)
			}
			c, err := newClientFromConfig(cfg)
			if err != nil {
				fatalErr(err)
			}
			if err := c.EnsureWorksetIndexes(ctx); err != nil {
				fatalErr(worksetError(err))
			}
			labels := c.AuthorLabels(ctx)
			ws, err := c.SetPhases(ctx, name, client.WorksetPhaseChange{
				FactIDs:  ids,
				To:       to,
				Note:     worksetFlags.phaseNote,
				By:       labels.Author,
				Takeover: worksetFlags.takeover,
			})
			if err != nil {
				fatalErr(worksetError(err))
			}
			if err := writeLine(os.Stdout, worksetSummary(ws)); err != nil {
				fatalErr(err)
			}
			if err := writeLine(os.Stdout, "requested: "+strconv.Itoa(len(ids))+" fact ids -> "+string(to)+"; entries already in that phase are no-ops"); err != nil {
				fatalErr(err)
			}
			if s := redactionLines(redactions); s != "" {
				if err := writeLine(os.Stdout, s); err != nil {
					fatalErr(err)
				}
			}
		},
	}
	cmd.Flags().StringVar(&worksetFlags.toPhase, "to", "",
		"target phase (todo|in_progress|blocked|testing|done|cancelled)")
	cmd.Flags().StringVar(&worksetFlags.phaseNote, "note", "", "reason for the move (stored in the journal)")
	cmd.Flags().BoolVar(&worksetFlags.allowSecret, "allow-secret", false,
		"store a guard-flagged note as written (secret_guard=ask only)")
	cmd.Flags().BoolVar(&worksetFlags.takeover, "takeover", false,
		"break a claim held by another author (journaled as an explicit takeover)")
	cmd.Flags().DurationVar(&worksetFlags.takeoverAfter, "takeover-after", 0,
		"override the staleness window for this call, e.g. 90s (0: workset_takeover_seconds)")
	return cmd
}

func worksetRenameCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rename <name> <new-name>",
		Short: "Rename a workset (the old name stays as an alias)",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			oldName, err := model.NormalizeName(args[0])
			if err != nil {
				fatalErr(err)
			}
			newName, err := model.NormalizeName(args[1])
			if err != nil {
				fatalErr(err)
			}
			if oldName == newName {
				fatalErr(errors.New("new name equals the current name: nothing to rename"))
			}
			ctx := context.Background()
			c, labels, err := newWorksetClient(ctx)
			if err != nil {
				fatalErr(err)
			}
			ws, err := c.RenameWorkset(ctx, oldName, newName, labels.Author)
			if err != nil {
				fatalErr(worksetError(err))
			}
			if err := writeLine(os.Stdout, worksetSummary(ws)); err != nil {
				fatalErr(err)
			}
			if err := writeLine(os.Stdout, "renamed: "+oldName+" -> "+ws.Name+" (the old name resolves as an alias)"); err != nil {
				fatalErr(err)
			}
		},
	}
	return cmd
}

func worksetDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name> --yes",
		Short: "Delete a workset (destructive)",
		Long: `Delete a workset and its cursors. This is destructive and shared: every reader
of the queue loses it, and there is no undo. Without --yes nothing is deleted.`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			name, err := model.NormalizeName(args[0])
			if err != nil {
				fatalErr(err)
			}
			if err := requireDeleteConfirm(name, worksetFlags.yes); err != nil {
				fatalErr(err)
			}
			ctx := context.Background()
			c, labels, err := newWorksetClient(ctx)
			if err != nil {
				fatalErr(err)
			}
			if err := c.DeleteWorkset(ctx, name, labels.Author); err != nil {
				fatalErr(worksetError(err))
			}
			if err := writeLine(os.Stdout, "deleted workset "+strconv.Quote(name)); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().BoolVar(&worksetFlags.yes, "yes", false,
		"confirm the deletion (without it the command refuses)")
	return cmd
}

func worksetCursorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cursor <name> [--at N | --clear] [--author A] [--host H]",
		Short: "Read or move this reader's cursor in a workset",
		Long: `Read or move the per-reader cursor. The cursor key is author@host, so agents
reading the same queue do not re-read what another has consumed. With
neither --at nor --clear the command only prints the current position.`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			name, err := model.NormalizeName(args[0])
			if err != nil {
				fatalErr(err)
			}
			var at *int
			if cmd.Flags().Changed("at") {
				v := worksetFlags.at
				at = &v
			}
			change, err := worksetCursorArgsFor(at, worksetFlags.clear)
			if err != nil {
				fatalErr(err)
			}
			ctx := context.Background()
			c, cfg, labels, err := newWorksetClientCfg(ctx)
			if err != nil {
				fatalErr(err)
			}
			author, host, err := worksetCursorIdentity(labels.Author, labels.Host, worksetFlags.author, worksetFlags.host)
			if err != nil {
				fatalErr(err)
			}
			key := model.CursorKey(author, host)
			if key == "" {
				fatalErr(errors.New("cursor: author is empty: a cursor names the reader it belongs to"))
			}
			if !change.Mutate {
				ws, err := c.GetWorkset(ctx, name)
				if err != nil {
					fatalErr(worksetError(err))
				}
				cur, ok := ws.Cursors[key]
				if !ok {
					if err := writeLine(os.Stdout, "cursor: none for "+displaySafe(key, false)); err != nil {
						fatalErr(err)
					}
					return
				}
				if err := writeLine(os.Stdout, worksetCursorLine(key, cur.At, cur.UpdatedAt)); err != nil {
					fatalErr(err)
				}
				return
			}
			// The cursor key is stored identity: a guard finding refuses the write
			// instead of rewriting it, exactly like the workset project label.
			if err := guardCheck(cfg.SecretGuard, worksetFlags.allowSecret,
				guardField{Name: "author", Text: author, HighOnly: true},
				guardField{Name: "host", Text: host, HighOnly: true}); err != nil {
				fatalErr(err)
			}
			ws, err := c.SetCursor(ctx, name, client.CursorInput{
				Author: author,
				Host:   host,
				At:     change.At,
				Clear:  change.Clear,
			})
			if err != nil {
				fatalErr(worksetError(err))
			}
			if cur, ok := ws.Cursors[key]; ok {
				if err := writeLine(os.Stdout, worksetCursorLine(key, cur.At, cur.UpdatedAt)); err != nil {
					fatalErr(err)
				}
				return
			}
			if err := writeLine(os.Stdout, "cursor: cleared for "+displaySafe(key, false)); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().IntVar(&worksetFlags.at, "at", 0,
		"set the cursor to this 1-based position (0 is a valid position)")
	cmd.Flags().BoolVar(&worksetFlags.clear, "clear", false, "clear this reader's cursor")
	cmd.Flags().StringVar(&worksetFlags.author, "author", "",
		"author label (default: the resolved writer label)")
	cmd.Flags().StringVar(&worksetFlags.host, "host", "",
		"host label (default: the resolved host label)")
	cmd.Flags().BoolVar(&worksetFlags.allowSecret, "allow-secret", false,
		"store guard-flagged author/host labels as written (secret_guard=ask only)")
	return cmd
}

// newWorksetClient builds the client every workset command needs and ensures
// the worksets index exists: the fact index is ensured by newClient, the
// worksets index is a separate pair of indices and must be ensured here.
func newWorksetClient(ctx context.Context) (*client.Client, client.AuthorInfo, error) {
	c, err := newClient()
	if err != nil {
		return nil, client.AuthorInfo{}, err
	}
	if err := c.EnsureWorksetIndexes(ctx); err != nil {
		return nil, client.AuthorInfo{}, worksetError(err)
	}
	return c, c.AuthorLabels(ctx), nil
}

// newWorksetClientCfg is newWorksetClient plus the effective config, for show
// (the staleness marker needs workset_takeover_seconds).
func newWorksetClientCfg(ctx context.Context) (*client.Client, *config.Config, client.AuthorInfo, error) {
	c, cfg, err := newClientWithConfig()
	if err != nil {
		return nil, nil, client.AuthorInfo{}, err
	}
	if err := c.EnsureWorksetIndexes(ctx); err != nil {
		return nil, nil, client.AuthorInfo{}, worksetError(err)
	}
	return c, cfg, c.AuthorLabels(ctx), nil
}

// worksetTakeoverWindow is the staleness window used for display.
func worksetTakeoverWindow(cfg *config.Config) time.Duration {
	return time.Duration(cfg.WorksetTakeoverSeconds) * time.Second
}

// worksetError turns a client workset error into an actionable line. The
// client's own message is kept (it names the author, the lock and what was
// refused); the hint names the next command that resolves the situation.
func worksetError(err error) error {
	switch {
	case errors.Is(err, client.ErrWorksetNotFound):
		return fmt.Errorf("%w — no workset or alias matches that name; run `velruna workset list` to see the available names", err)
	case errors.Is(err, client.ErrWorksetExists):
		return fmt.Errorf("%w — a workset with that name or alias already exists; pick another name or rename the existing one", err)
	case errors.Is(err, client.ErrWorksetLocked):
		return fmt.Errorf("%w — another author holds the entry; wait for the staleness window, pass --takeover, or move it to todo first", err)
	case errors.Is(err, client.ErrWorksetConflict):
		return fmt.Errorf("%w — the workset changed or the phase transition is not allowed; re-read with `velruna workset show <name>` and retry", err)
	}
	return err
}

// worksetLimit resolves the show limit: 0 means the default, anything above
// the cap is clamped, and a negative value is a caller error (a silently
// negative limit would render an empty page and read as "no entries").
func worksetLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, fmt.Errorf("limit must be >= 0 (got %d); omit it for the default %d", limit, worksetDefaultLimit)
	}
	if limit == 0 {
		return worksetDefaultLimit, nil
	}
	if limit > worksetMaxLimit {
		return worksetMaxLimit, nil
	}
	return limit, nil
}

// worksetJournalCount resolves --journal: 0 disables the journal, negative is
// an error, and the same context cap as the entry window applies.
func worksetJournalCount(n int) (int, error) {
	if n < 0 {
		return 0, fmt.Errorf("journal must be >= 0 (got %d); omit it to skip the journal", n)
	}
	if n > worksetMaxLimit {
		return worksetMaxLimit, nil
	}
	return n, nil
}

// worksetCursorChange is the validated shape of the cursor flags.
type worksetCursorChange struct {
	At     int
	Clear  bool
	Mutate bool
}

// worksetCursorArgsFor validates --at/--clear. Neither means "read only": the
// cursor command then prints the current position instead of writing one.
func worksetCursorArgsFor(at *int, clear bool) (worksetCursorChange, error) {
	if at != nil && clear {
		return worksetCursorChange{}, errors.New("--at and --clear are mutually exclusive")
	}
	if at != nil {
		if *at < 0 {
			return worksetCursorChange{}, fmt.Errorf("--at must be >= 0 (got %d): a cursor is a 1-based position and 0 is the start", *at)
		}
		return worksetCursorChange{At: *at, Mutate: true}, nil
	}
	if clear {
		return worksetCursorChange{Clear: true, Mutate: true}, nil
	}
	return worksetCursorChange{}, nil
}

// worksetCursorIdentity resolves the cursor key halves: the flags win when
// given, otherwise the client-resolved writer labels are used. Labels are
// normalized like config labels so a control character cannot forge the
// rendered key.
func worksetCursorIdentity(defAuthor, defHost, authorOverride, hostOverride string) (string, string, error) {
	author := defAuthor
	if strings.TrimSpace(authorOverride) != "" {
		a, err := config.NormalizeLabel(authorOverride)
		if err != nil {
			return "", "", fmt.Errorf("--author: %w", err)
		}
		author = a
	}
	host := defHost
	if strings.TrimSpace(hostOverride) != "" {
		h, err := config.NormalizeLabel(hostOverride)
		if err != nil {
			return "", "", fmt.Errorf("--host: %w", err)
		}
		host = h
	}
	return author, host, nil
}

func worksetCursorLine(key string, at int, updatedAt time.Time) string {
	return "cursor: " + displaySafe(key, false) + " at=" + strconv.Itoa(at) + " updated=" + displayTime(updatedAt)
}

// requireDeleteConfirm is the destructive-write barrier: deleting a workset is
// irreversible and shared, so a bare invocation must refuse rather than
// silently destroy another reader's queue.
func requireDeleteConfirm(name string, yes bool) error {
	if yes {
		return nil
	}
	return fmt.Errorf("refusing to delete workset %q: this removes the shared queue, its phases and its cursors for every reader; re-run with --yes to confirm", name)
}

// normalizeFactIDs trims, rejects empty ids and drops duplicates (a repeated
// id in one call would otherwise ask the store to add or move the same entry
// twice, which reads as a self-conflict). Order is preserved.
func normalizeFactIDs(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for i, id := range raw {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("empty fact_id at position %d", i+1)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, errors.New("at least one fact_id is required")
	}
	return out, nil
}

// parsePhaseValue parses one phase name; empty means "not given" (the caller
// applies its default). Mixed case and surrounding space are normalized, while
// an unknown spelling is a loud error: a phase typo would otherwise be stored
// as a phase no transition matrix row covers.
func parsePhaseValue(raw string) (model.Phase, error) {
	p := model.Phase(strings.ToLower(strings.TrimSpace(raw)))
	if p == "" {
		return "", nil
	}
	if !model.ValidPhase(p) {
		return "", fmt.Errorf("unknown phase %q (want %s)", raw, phaseNames())
	}
	return p, nil
}

// parsePhaseList parses the comma-separated --phase flag.
func parsePhaseList(raw string) ([]model.Phase, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	return normalizePhases(strings.Split(raw, ","))
}

// normalizePhases validates a list of phase names (MCP takes the list already
// split) and dedupes it, preserving order.
func normalizePhases(raw []string) ([]model.Phase, error) {
	out := make([]model.Phase, 0, len(raw))
	seen := make(map[model.Phase]bool, len(raw))
	for _, r := range raw {
		p, err := parsePhaseValue(r)
		if err != nil {
			return nil, err
		}
		if p == "" {
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// phaseNames renders the accepted phases for error messages.
func phaseNames() string {
	all := model.AllPhases()
	names := make([]string, len(all))
	for i, p := range all {
		names[i] = string(p)
	}
	return strings.Join(names, "|")
}

// worksetRow is one entry as show renders it: its 1-based index in the
// filtered view, the entry itself, and its fact_state as a plain string.
type worksetRow struct {
	Index int
	Entry model.Entry
	State string
}

// worksetWindow applies the show filters. The order is deliberate and is the
// contract the command documents:
//
//  1. --phase and --live-only narrow the workset's entries, so a reader asking
//     for "the in_progress entries" counts positions in that view;
//  2. indices are assigned 1..n over the surviving entries;
//  3. --after and --before cut by that index (both exclusive), then --limit
//     keeps the first N of the remainder.
//
// limit <= 0 means "no cap": the CLI resolves its default/cap before calling,
// and the mutation views pass 0 to render every entry.
func worksetWindow(entries []model.Entry, states map[string]string, phases []model.Phase, liveOnly bool, after, before, limit int) []worksetRow {
	phaseSet := make(map[model.Phase]bool, len(phases))
	for _, p := range phases {
		phaseSet[p] = true
	}
	rows := make([]worksetRow, 0, len(entries))
	index := 0
	for _, e := range entries {
		if len(phaseSet) > 0 && !phaseSet[e.Phase] {
			continue
		}
		state := states[e.FactID]
		if state == "" {
			// The store answers every requested id, so an absent answer is a
			// partial read, not a verified fact: never let it pass live-only.
			state = worksetStateUnknown
		}
		if liveOnly && state != worksetStateVerified {
			continue
		}
		index++
		if after > 0 && index <= after {
			continue
		}
		if before > 0 && index >= before {
			continue
		}
		if limit > 0 && len(rows) >= limit {
			break
		}
		rows = append(rows, worksetRow{Index: index, Entry: e, State: state})
	}
	return rows
}

// worksetStates loads fact_state for the entries with one store call (the
// state of a fact is computed at read time, never stored in the workset). The
// batch is capped at worksetStateReadCap distinct ids: ids beyond the cap are
// left out of the map, which the renderers show as unknown.
func worksetStates(ctx context.Context, c *client.Client, entries []model.Entry) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if len(ids) >= worksetStateReadCap {
			break
		}
		if _, dup := seen[e.FactID]; dup {
			continue
		}
		seen[e.FactID] = struct{}{}
		ids = append(ids, e.FactID)
	}
	raw, err := c.FactStates(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for id, st := range raw {
		out[id] = string(st)
	}
	return out, nil
}

// worksetSummary is the one-line header shared by every command and by list.
func worksetSummary(ws *model.Workset) string {
	var b strings.Builder
	b.WriteString("workset: ")
	b.WriteString(displaySafe(ws.Name, false))
	b.WriteString("  project: ")
	b.WriteString(displaySafe(ws.Project, false))
	b.WriteString("  version: ")
	b.WriteString(strconv.FormatInt(ws.Version, 10))
	b.WriteString("  entries: ")
	b.WriteString(strconv.Itoa(len(ws.Entries)))
	if len(ws.Aliases) > 0 {
		b.WriteString("  aliases: ")
		for i, a := range ws.Aliases {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(displaySafe(a, false))
		}
	}
	return b.String()
}

// printWorkset renders the show view: header, table, presence, cursor
// positions, self_verified and (when requested) the journal. rows is the
// filtered window the table shows; allRows is the unfiltered list the presence
// and self_verified lines are computed from, so a filter cannot hide another
// agent's claim or a reader's position.
func printWorkset(w io.Writer, ws *model.Workset, rows, allRows []worksetRow, omitted int, host string,
	journal []model.WorksetJournalEntry, showJournal bool, now time.Time, takeoverAfter time.Duration) error {
	var b strings.Builder
	b.WriteString(worksetSummary(ws))
	b.WriteString("\n")
	if len(rows) == 0 {
		b.WriteString("entries: none match the filters\n")
	} else {
		b.WriteString(worksetTable(rows, now, takeoverAfter))
		// A silent cut reads as "that is the whole workset": say how many the
		// limit hid, and which flag raises it.
		if omitted > 0 {
			b.WriteString("omitted: ")
			b.WriteString(strconv.Itoa(omitted))
			b.WriteString(" entries past the limit (raise --limit; the cap is 200)\n")
		}
	}
	if line := worksetPresence(allRows, host); line != "" {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if line := worksetCursorsLine(ws.Cursors); line != "" {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if line := worksetSelfVerifiedLine(allRows, journal); line != "" {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if showJournal {
		b.WriteString(worksetJournalBlock(journal))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// worksetTable renders the entry table: index, fact_id, fact_state, phase,
// phase_by, the age of phase_at (marked stale against the takeover window) and
// the note.
func worksetTable(rows []worksetRow, now time.Time, takeoverAfter time.Duration) string {
	var b strings.Builder
	b.WriteString(padRight("#", 4))
	b.WriteString("  ")
	b.WriteString(padRight("fact_id", 26))
	b.WriteString("  ")
	b.WriteString(padRight("state", 9))
	b.WriteString("  ")
	b.WriteString(padRight("phase", 12))
	b.WriteString("  ")
	b.WriteString(padRight("phase_by", 14))
	b.WriteString("  ")
	b.WriteString(padRight("age", 14))
	b.WriteString("  note\n")
	for _, r := range rows {
		by := r.Entry.PhaseBy
		if by == "" {
			by = "-"
		}
		b.WriteString(padRight(strconv.Itoa(r.Index), 4))
		b.WriteString("  ")
		b.WriteString(padRight(displaySafe(r.Entry.FactID, false), 26))
		b.WriteString("  ")
		b.WriteString(padRight(r.State, 9))
		b.WriteString("  ")
		b.WriteString(padRight(string(r.Entry.Phase), 12))
		b.WriteString("  ")
		b.WriteString(padRight(displaySafe(by, false), 14))
		b.WriteString("  ")
		b.WriteString(padRight(worksetPhaseAge(r.Entry.PhaseAt, now, takeoverAfter), 14))
		b.WriteString("  ")
		b.WriteString(displaySafe(r.Entry.Note, false))
		b.WriteString("\n")
	}
	return b.String()
}

// worksetCursorsLine renders the per-reader reading positions, newest first.
// A cursor is navigation state ("where each session last was"), distinct from
// the in_progress presence line, which is a claim on an entry.
func worksetCursorsLine(cursors map[string]model.Cursor) string {
	items := worksetCursorItems(cursors)
	if len(items) == 0 {
		return ""
	}
	return "cursors: " + strings.Join(items, ", ")
}

// worksetCursorItems builds the cursor positions as key@position, newest
// first (ties by key so two identical reads render the same page). The MCP
// answer carries them as a JSON array.
func worksetCursorItems(cursors map[string]model.Cursor) []string {
	if len(cursors) == 0 {
		return nil
	}
	keys := make([]string, 0, len(cursors))
	for k := range cursors {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ti, tj := cursors[keys[i]].UpdatedAt, cursors[keys[j]].UpdatedAt
		if ti.Equal(tj) {
			return keys[i] < keys[j]
		}
		return ti.After(tj)
	})
	items := make([]string, 0, len(keys))
	for _, k := range keys {
		items = append(items, displaySafe(k, false)+"@"+strconv.Itoa(cursors[k].At))
	}
	return items
}

// worksetPresence is the in_progress presence line: who is on which entry,
// rendered author@host@index. The stored entry carries only the author label
// (phase_by) and no host, so the host is the one resolved for THIS reader —
// unless the stored author already carries an "@host" suffix, which is then
// kept as-is.
func worksetPresence(rows []worksetRow, host string) string {
	items := worksetPresenceItems(rows, host)
	if len(items) == 0 {
		return ""
	}
	return "in_progress: " + strings.Join(items, ", ")
}

// worksetPresenceItems builds the presence entries without the line prefix (the
// MCP answer carries them as a JSON array).
func worksetPresenceItems(rows []worksetRow, host string) []string {
	if host == "" {
		host = "unknown"
	}
	items := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Entry.Phase != model.PhaseInProgress {
			continue
		}
		author := displaySafe(r.Entry.PhaseBy, false)
		if author == "" {
			author = "unknown"
		}
		if strings.Contains(author, "@") {
			items = append(items, author+"@"+strconv.Itoa(r.Index))
			continue
		}
		items = append(items, author+"@"+host+"@"+strconv.Itoa(r.Index))
	}
	return items
}

// worksetSelfVerifiedLine names the done entries that the same author both
// tested and closed.
func worksetSelfVerifiedLine(rows []worksetRow, journal []model.WorksetJournalEntry) string {
	ids := worksetSelfVerifiedIDs(rows, journal)
	if len(ids) == 0 {
		return ""
	}
	return "self_verified: " + strings.Join(ids, ", ")
}

// worksetSelfVerifiedIDs lists the self-verified fact ids (the MCP answer
// carries them as a JSON array).
func worksetSelfVerifiedIDs(rows []worksetRow, journal []model.WorksetJournalEntry) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if worksetSelfVerified(r.Entry, journal) {
			ids = append(ids, displaySafe(r.Entry.FactID, false))
		}
	}
	return ids
}

// worksetSelfVerified reports whether a done entry was closed by the author
// who last tested it. Live entries carry only phase_by (the author of the
// current phase), so the only honest source is the journal: a testing (or
// in_progress) row by the same author, recorded before the done transition.
// Without that row in the loaded window there is nothing to compare, and the
// entry is NOT marked — a missing journal proves nothing, and the earlier
// fallback (mark any done entry with a phase_by) was a false positive.
func worksetSelfVerified(e model.Entry, journal []model.WorksetJournalEntry) bool {
	if e.Phase != model.PhaseDone || e.PhaseBy == "" {
		return false
	}
	lastBy := ""
	var lastAt time.Time
	for _, j := range journal {
		if j.FactID != e.FactID {
			continue
		}
		if j.To != model.PhaseTesting && j.To != model.PhaseInProgress {
			continue
		}
		if !e.PhaseAt.IsZero() && j.At.After(e.PhaseAt) {
			continue
		}
		if lastBy == "" || j.At.After(lastAt) {
			lastBy, lastAt = j.By, j.At
		}
	}
	if lastBy == "" {
		return false
	}
	return lastBy == e.PhaseBy
}

// worksetJournalBlock renders the change journal. The API returns entries
// newest first; this preserves that order.
func worksetJournalBlock(entries []model.WorksetJournalEntry) string {
	if len(entries) == 0 {
		return "journal: none\n"
	}
	var b strings.Builder
	b.WriteString("journal (newest first):\n")
	for _, e := range entries {
		b.WriteString("  - ")
		b.WriteString(displayTime(e.At))
		b.WriteString(" [")
		b.WriteString(displaySafe(e.Op, false))
		b.WriteString("]")
		if e.FactID != "" {
			b.WriteString(" fact=")
			b.WriteString(displaySafe(e.FactID, false))
		}
		if e.Takeover != "" {
			b.WriteString(" takeover=")
			b.WriteString(displaySafe(e.Takeover, false))
		}
		if e.From != "" || e.To != "" {
			b.WriteString(" ")
			b.WriteString(string(e.From))
			b.WriteString("→")
			b.WriteString(string(e.To))
		}
		b.WriteString(" by=")
		b.WriteString(displaySafe(e.By, false))
		if e.Note != "" {
			b.WriteString(" note=")
			b.WriteString(displaySafe(e.Note, false))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// worksetPhaseAge renders the age of phase_at in a compact unit and marks a
// claim that has gone quiet past the takeover window.
func worksetPhaseAge(phaseAt, now time.Time, takeoverAfter time.Duration) string {
	if phaseAt.IsZero() {
		return "unknown"
	}
	age := now.Sub(phaseAt)
	if age < 0 {
		// Clock skew: a future timestamp is never stale and must not render as
		// a negative age.
		age = 0
	}
	out := compactDuration(age)
	if model.IsStale(phaseAt, now, takeoverAfter) {
		out += " (stale)"
	}
	return out
}

func compactDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
}

// padRight pads s to width runes. Rune count, not bytes: a label may contain
// non-ASCII text, and byte padding would misalign the table.
func padRight(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}
