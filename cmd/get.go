package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/model"
	"github.com/spf13/cobra"
)

var getFlags struct {
	history bool
}

func GetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <key>",
		Short: "Get fact by key",
		Args:  cobra.ExactArgs(1),
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
			f, err := c.Get(ctx, key)
			if err != nil {
				fatalIfNotFound(err)
			}
			// --history on a journal-era fact must not print nothing: the
			// in-doc array is empty there, the chain lives in the journal
			// indices. Fetch it (best-effort: a journal read failure is
			// reported under the fact, not a fatal error after the fact).
			var entries []model.JournalEntry
			var jTotal, jSkipped int
			var jErr error
			if getFlags.history && len(f.History) == 0 {
				var jMeta client.JournalMeta
				entries, jMeta, jErr = c.JournalEntries(ctx, client.JournalQuery{Key: key, Limit: 20})
				jTotal = jMeta.Total
				jSkipped = jMeta.Skipped
			}
			if err := printFact(os.Stdout, f, time.Now().UTC(), getFlags.history, entries, jTotal, jSkipped, jErr); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().BoolVar(&getFlags.history, "history", false,
		"include history: the legacy in-doc array, or the newest 20 journal entries for journal-era facts (off by default: history bloats the context)")
	return cmd
}

// printFact renders one fact for humans on w. The journal entries (and
// their total) come from the caller so this function stays pure and
// testable: journal-era facts have no in-doc history array, and --history
// renders the fetched journal instead (or says explicitly that there is
// nothing, rather than printing nothing).
func printFact(w io.Writer, f *model.Fact, now time.Time, history bool, entries []model.JournalEntry, jTotal, jSkipped int, jErr error) error {
	var b strings.Builder
	b.WriteString("key: ")
	b.WriteString(strconv.Quote(f.Key))
	b.WriteString("\n")
	b.WriteString("value: ")
	b.WriteString(displaySafe(f.Value, true))
	b.WriteString("\n")
	// The summary is the table of contents of a long value: show it next to
	// the value (stored facts predate the field, so it is often empty).
	if f.Summary != "" {
		b.WriteString("summary: ")
		b.WriteString(displaySafe(f.Summary, false))
		b.WriteString("\n")
	}
	b.WriteString("category: ")
	b.WriteString(strconv.Quote(f.Category))
	b.WriteString("\n")
	b.WriteString("project: ")
	b.WriteString(strconv.Quote(f.Project))
	b.WriteString("\n")
	b.WriteString("status: ")
	b.WriteString(strconv.Quote(f.Status))
	b.WriteString("\n")
	b.WriteString("source: ")
	b.WriteString(strconv.Quote(f.Source))
	b.WriteString("\n")
	// Authorship is self-reported (audit metadata, not authentication): show
	// who wrote the fact and which chain link named them.
	if f.Author != "" {
		b.WriteString("author: ")
		b.WriteString(strconv.Quote(f.Author))
		b.WriteString(" (")
		b.WriteString(displaySafe(f.AuthorSource, false))
		b.WriteString(")\n")
	}
	if f.UpdatedBy != "" && f.UpdatedBy != f.Author {
		b.WriteString("updated_by: ")
		b.WriteString(strconv.Quote(f.UpdatedBy))
		b.WriteString("\n")
	}
	if f.Notes != "" {
		b.WriteString("notes: ")
		// keepNewline: notes are one line per qualifier (appendNote joins
		// with \n) — collapsing them into one run-on line would destroy the
		// documented audit structure.
		b.WriteString(displaySafe(f.Notes, true))
		b.WriteString("\n")
	}
	if len(f.Keywords) > 0 {
		b.WriteString("keywords: ")
		b.WriteString(strings.Join(f.Keywords, ", "))
		b.WriteString("\n")
	}
	if f.SupersededBy != "" {
		// The successor is navigation, not prose: a reader who trusts this
		// fact must be sent to the record that replaced it.
		b.WriteString("superseded_by: ")
		b.WriteString(strconv.Quote(f.SupersededBy))
		b.WriteString("\n")
	}
	if len(f.Redactions) > 0 {
		// The stored value is shorter than the one that was written: a reader
		// must not take it for the complete claim (see the guard's write path).
		b.WriteString("redacted: ")
		for i, r := range f.Redactions {
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(r.Field)
			b.WriteString("=")
			b.WriteString(r.Pattern)
			b.WriteString("(")
			b.WriteString(strconv.Itoa(r.Chars))
			b.WriteString(" chars)")
		}
		b.WriteString("\n")
	}
	if len(f.ConfirmedBy) > 0 {
		// One line: the trust signal is who verified the CURRENT value, and
		// user+host is what identifies a confirmer in a shared store.
		b.WriteString("confirmed_by: ")
		for i, c := range f.ConfirmedBy {
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(c.Author)
			if c.Host != "" {
				b.WriteString("@")
				b.WriteString(c.Host)
			}
			if c.AuthorSource != "" {
				b.WriteString(" (")
				b.WriteString(c.AuthorSource)
				b.WriteString(")")
			}
			b.WriteString(" ")
			b.WriteString(displayTime(c.At))
		}
		b.WriteString("\n")
	}
	b.WriteString("observed_at: ")
	b.WriteString(displayTime(f.ObservedAt))
	b.WriteString("\n")
	if f.TTLDays > 0 {
		// The override is the one TTL number the reader cannot infer from the
		// category; show it with the default it replaces.
		b.WriteString("ttl_days: ")
		b.WriteString(strconv.Itoa(f.TTLDays))
		b.WriteString(" (per-fact override; category default ")
		b.WriteString(strconv.Itoa(model.TTLDays(f.Category)))
		b.WriteString("d)\n")
	}
	b.WriteString("age_days: ")
	b.WriteString(displayAge(f.AgeDays(now)))
	b.WriteString("\n")
	b.WriteString("stale: ")
	b.WriteString(displayStale(f, now))
	b.WriteString("\n")
	b.WriteString("updated_at: ")
	b.WriteString(displayTime(f.UpdatedAt))
	b.WriteString("\n")
	if history {
		if len(f.History) > 0 {
			b.WriteString("history:\n")
			for _, h := range f.History {
				b.WriteString("  - ")
				b.WriteString(strconv.Quote(h.Value))
				b.WriteString(" @ ")
				b.WriteString(displayTime(h.ObservedAt))
				b.WriteString(" [")
				b.WriteString(displaySafe(h.Source, false))
				b.WriteString("]\n")
			}
		} else if jErr != nil {
			b.WriteString("history: journal read failed: ")
			b.WriteString(jErr.Error())
			b.WriteString("\n")
		} else if jTotal > 0 {
			b.WriteString("history (journal, newest first): ")
			b.WriteString(strconv.Itoa(jTotal))
			b.WriteString(" entries")
			// Skipped counts toward what the page held: calling undecodable
			// entries "not shown due to the limit" hides index corruption behind
			// ordinary truncation advice (the journal viewer names corruption;
			// get must not silently downgrade it).
			if jTotal > len(entries)+jSkipped {
				b.WriteString(" — showing the newest ")
				b.WriteString(strconv.Itoa(len(entries)))
				// --limit must be in the advice: the journal command defaults to
				// the same window get already showed, so the bare suggestion
				// would return the identical page.
				b.WriteString(" (see `velruna journal ")
				b.WriteString(strconv.Quote(f.Key))
				b.WriteString(" --limit <N>`)")
			}
			if jSkipped > 0 {
				b.WriteString(" — ")
				b.WriteString(strconv.Itoa(jSkipped))
				b.WriteString(" entries failed to decode (index corruption: report to the user)")
			}
			b.WriteString("\n")
			for _, e := range entries {
				b.WriteString("  - ")
				b.WriteString(displayTime(e.CreatedAt))
				b.WriteString(" [")
				b.WriteString(strconv.Quote(displaySafe(e.Op, false)))
				b.WriteString("] value=")
				b.WriteString(strconv.Quote(e.Value))
				b.WriteString("\n")
			}
		} else {
			b.WriteString("history: none (no in-doc chain, no journal entries)\n")
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// displayTime renders a timestamp for humans, or "unknown" for the zero
// time (facts created before observed_at existed; an unset updated_at
// renders the same way instead of year 1). The trailing Z states the
// obvious — every stored timestamp is UTC — so listings from different
// machines cannot be read as local time.
func displayTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

// displayAge renders the age in days, marking an unknown observation.
func displayAge(days int) string {
	if days < 0 {
		return "unknown"
	}
	return strconv.Itoa(days)
}

// staleMark is the compact staleness marker for one-line listings; empty when
// the fact is fresh, so clean output stays clean.
func staleMark(f *model.Fact, now time.Time) string {
	if !f.Stale(now) {
		return ""
	}
	age := f.AgeDays(now)
	if age < 0 {
		return " [stale: observed_at unknown]"
	}
	return " [stale: " + strconv.Itoa(age) + "d]"
}

// displayStale explains the staleness verdict: past-TTL facts carry the numbers
// so the reader knows exactly what to re-verify, and an unknown observation
// time is treated as stale (the age cannot be trusted).
func displayStale(f *model.Fact, now time.Time) string {
	ttl := f.EffectiveTTLDays()
	if ttl == 0 {
		return "false (category has no TTL)"
	}
	ttlLabel := "ttl for " + strconv.Quote(f.Category) + " is " + strconv.Itoa(ttl) + "d"
	if f.TTLDays > 0 {
		ttlLabel = "ttl is " + strconv.Itoa(ttl) + "d (per-fact override)"
	}
	if f.ObservedAt.IsZero() {
		return "true (observed_at unknown, " + ttlLabel + ")"
	}
	if f.Stale(now) {
		var b strings.Builder
		b.WriteString("true (age ")
		b.WriteString(strconv.Itoa(f.AgeDays(now)))
		b.WriteString("d, ")
		b.WriteString(ttlLabel)
		b.WriteString(") — re-verify before trusting")
		return b.String()
	}
	return "false"
}
