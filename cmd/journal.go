package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/spf13/cobra"
)

var journalFlags struct {
	limit int
	since string
	until string
}

func JournalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "journal <key>",
		Short: "Read the journal: the append-only history of a fact key",
		Long: `Read journal entries for a key, newest first.

The journal is the history: every accepted value for the key is one entry
(op=write; op=rediscovery records a re-confirmation of the same value,
written by velruna_patch reverify; op=retire marks a verified→obsolete
transition; op=supersede records a superseded_by link to the fact that
replaced this one — a relation, not a value change). Counts after an admin
deleted old journal indices are lower bounds — the surviving journal, not
the lifetime.`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			key := strings.TrimSpace(args[0])
			if key == "" {
				fatalErr(errors.New("key is required"))
			}
			if journalFlags.limit < 1 {
				// A zero/negative limit collapsing to the 1000-entry cap reads
				// as a small page; demand an explicit positive bound.
				fatalErr(fmt.Errorf("journal: --limit must be >= 1, got %d", journalFlags.limit))
			}
			since, serr := parseJournalBound("--since", journalFlags.since)
			if serr != nil {
				fatalErr(serr)
			}
			until, uerr := parseJournalBound("--until", journalFlags.until)
			if uerr != nil {
				fatalErr(uerr)
			}
			if !since.IsZero() && !until.IsZero() && since.After(until) {
				fatalErr(errors.New("journal: --since is after --until"))
			}
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fatalErr(err)
			}
			if journalFlags.limit > client.NavSizeCap {
				// The client would clamp silently; say so, or a capped page
				// reads like the whole journal (same contract as search).
				journalFlags.limit = client.NavSizeCap
				if err := writeLine(os.Stderr, "note: --limit clamped to "+strconv.Itoa(client.NavSizeCap)+" (the navigation cap of one page)"); err != nil {
					fatalErr(err)
				}
			}
			entries, meta, err := c.JournalEntries(ctx, client.JournalQuery{
				Key: key, Limit: journalFlags.limit, Since: since, Until: until,
			})
			if err != nil {
				fatalErr(err)
			}
			var b strings.Builder
			b.WriteString("journal for ")
			b.WriteString(strconv.Quote(key))
			b.WriteString(": ")
			b.WriteString(strconv.Itoa(meta.Total))
			b.WriteString(" entries (newest first)")
			if meta.Truncated {
				b.WriteString(" — showing the newest ")
				b.WriteString(strconv.Itoa(len(entries)))
				if journalFlags.limit < client.NavSizeCap {
					b.WriteString(" (raise --limit, max ")
					b.WriteString(strconv.Itoa(client.NavSizeCap))
					b.WriteString(")")
				} else {
					// The caller already asked for the ceiling: "raise --limit"
					// would be advice that cannot be followed.
					b.WriteString(" (at the navigation cap: use velruna history-info for counts)")
				}
			}
			if meta.Skipped > 0 {
				b.WriteString(" — ")
				b.WriteString(strconv.Itoa(meta.Skipped))
				b.WriteString(" entries failed to decode (index corruption: report to the user, raising --limit does not recover them)")
			}
			b.WriteString("\n")
			for _, e := range entries {
				b.WriteString("  - ")
				b.WriteString(displayTime(e.CreatedAt))
				b.WriteString(" [")
				// Quoted: even a sanitized label like read] value="..." can
				// forge the bracket layout, and op is self-reportable through
				// out-of-band journal writes.
				b.WriteString(strconv.Quote(displaySafe(e.Op, false)))
				b.WriteString("]")
				if e.Marker != "" {
					b.WriteString(" marker=")
					b.WriteString(strconv.Quote(displaySafe(e.Marker, false)))
				}
				if e.SupersededBy != "" {
					b.WriteString(" superseded_by=")
					b.WriteString(strconv.Quote(displaySafe(e.SupersededBy, false)))
				}
				if e.Source != "" {
					b.WriteString(" source=")
					b.WriteString(displaySafe(e.Source, false))
				}
				if e.Author != "" {
					// Quoted: labels are self-reported (server/OS user), and
					// an unquoted "John Doe" parses ambiguously in listings.
					b.WriteString(" author=")
					b.WriteString(strconv.Quote(e.Author))
				}
				b.WriteString(" value=")
				b.WriteString(strconv.Quote(e.Value))
				b.WriteString("\n")
			}
			if _, err := io.WriteString(os.Stdout, b.String()); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().IntVar(&journalFlags.limit, "limit", 20,
		"newest entries to show (the journal is a log: bound the read, do not flood the context)")
	cmd.Flags().StringVar(&journalFlags.since, "since", "",
		"only entries at or after this RFC3339 timestamp (e.g. 2026-11-10T00:00:00Z)")
	cmd.Flags().StringVar(&journalFlags.until, "until", "",
		"only entries at or before this RFC3339 timestamp")
	return cmd
}

// parseJournalBound parses an optional RFC3339 bound for a journal window.
// A date-only value is refused on purpose: "2026-11-10" silently means local
// midnight, and the journal is stored in UTC.
func parseJournalBound(flag, raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, fmt.Errorf("journal: %s must be RFC3339 (e.g. 2026-11-10T00:00:00Z), got %q", flag, raw)
	}
	return t, nil
}

func HistoryInfoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history-info <key>",
		Short: "Compact journal overview: writes, rediscoveries, time range",
		Long: `Fast overview an agent can consult before pulling the full journal:
how many values this key accepted (changes = writes - 1), how often it was
re-discovered, and the surviving journal's time range.

Counts are LOWER BOUNDS: an admin pruning old journal indices removes
entries the app cannot see. Trust them with the category's staleness rule,
not as lifetime totals.`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			key := strings.TrimSpace(args[0])
			if key == "" {
				fatalErr(errors.New("key is required"))
			}
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fatalErr(err)
			}
			info, err := c.HistoryInfo(ctx, key)
			if err != nil {
				fatalErr(err)
			}
			var b strings.Builder
			b.WriteString("key: ")
			b.WriteString(strconv.Quote(info.Key))
			b.WriteString("\n")
			b.WriteString("writes: ")
			b.WriteString(strconv.Itoa(info.Writes))
			b.WriteString("\n")
			b.WriteString("rediscoveries: ")
			b.WriteString(strconv.Itoa(info.Rediscoveries))
			b.WriteString("\n")
			b.WriteString("first_at: ")
			b.WriteString(displayTimePtr(info.FirstAt))
			b.WriteString("\n")
			b.WriteString("last_at: ")
			b.WriteString(displayTimePtr(info.LastAt))
			b.WriteString("\n")
			if info.Category != "" {
				b.WriteString("category: ")
				b.WriteString(strconv.Quote(info.Category))
				b.WriteString("\n")
			}
			if info.CurrentRef != "" {
				b.WriteString("current_ref: ")
				b.WriteString(info.CurrentRef)
				b.WriteString("\n")
			}
			if _, err := io.WriteString(os.Stdout, b.String()); err != nil {
				fatalErr(err)
			}
		},
	}
	return cmd
}

// displayTimePtr renders an optional timestamp ("none" when the journal has
// no entries yet, so the field is never silently absent).
func displayTimePtr(t *time.Time) string {
	if t == nil {
		return "none"
	}
	return displayTime(*t)
}
