package model

import (
	"fmt"
	"strings"
	"time"
)

// Phase is the workflow state of one workset entry. The zero value ("") is
// not a phase: the write edge always stores one of the constants below, so a
// reader may switch on Phase without a default that silently invents a state.
type Phase string

const (
	PhaseTodo       Phase = "todo"
	PhaseInProgress Phase = "in_progress"
	PhaseBlocked    Phase = "blocked"
	PhaseTesting    Phase = "testing"
	PhaseDone       Phase = "done"
	PhaseCancelled  Phase = "cancelled"
)

// allPhases is the canonical declaration order. It is private so a caller
// cannot reorder the matrix or mutate the shared slice through AllPhases.
var allPhases = []Phase{
	PhaseTodo,
	PhaseInProgress,
	PhaseBlocked,
	PhaseTesting,
	PhaseDone,
	PhaseCancelled,
}

// ValidPhase reports whether p is one of the workflow phases.
func ValidPhase(p Phase) bool {
	switch p {
	case PhaseTodo, PhaseInProgress, PhaseBlocked, PhaseTesting, PhaseDone, PhaseCancelled:
		return true
	}
	return false
}

// AllPhases returns the phases in declaration order (todo → in_progress →
// blocked → testing → done → cancelled). Each call returns a fresh slice:
// callers build menus and matrices from it, and a shared slice would let one
// caller's edit corrupt every later listing.
func AllPhases() []Phase {
	out := make([]Phase, len(allPhases))
	copy(out, allPhases)
	return out
}

// transitions is the structural matrix: which phase changes the workflow
// permits at all, independent of who is asking. It encodes two rules that are
// easy to lose in ad-hoc checks:
//
//   - done is reachable only through testing — nothing is "done" that was not
//     observed to be verified; and
//   - a finished entry (done, cancelled) never slides back into blocked or
//     cancelled, so the end states stay terminal for those transitions.
//
// Every phase may be released to todo or cancelled, which keeps an abandoned
// item recoverable by any reader without special-casing per phase.
var transitions = map[Phase]map[Phase]bool{
	PhaseTodo: {
		PhaseTodo:       true,
		PhaseInProgress: true,
		PhaseBlocked:    true,
		PhaseTesting:    true,
		PhaseDone:       false,
		PhaseCancelled:  true,
	},
	PhaseInProgress: {
		PhaseTodo:       true,
		PhaseInProgress: true,
		PhaseBlocked:    true,
		PhaseTesting:    true,
		PhaseDone:       false,
		PhaseCancelled:  true,
	},
	PhaseBlocked: {
		PhaseTodo:       true,
		PhaseInProgress: true,
		PhaseBlocked:    true,
		PhaseTesting:    true,
		PhaseDone:       false,
		PhaseCancelled:  true,
	},
	PhaseTesting: {
		PhaseTodo:       true,
		PhaseInProgress: true,
		PhaseBlocked:    true,
		PhaseTesting:    true,
		PhaseDone:       true,
		PhaseCancelled:  true,
	},
	PhaseDone: {
		PhaseTodo:       true,
		PhaseInProgress: true,
		PhaseBlocked:    false,
		PhaseTesting:    true,
		PhaseDone:       true,
		PhaseCancelled:  false,
	},
	PhaseCancelled: {
		PhaseTodo:       true,
		PhaseInProgress: true,
		PhaseBlocked:    false,
		PhaseTesting:    false,
		PhaseDone:       false,
		PhaseCancelled:  true,
	},
}

// CanTransition is the structural matrix: may an entry move from → to at all?
// A self-transition returns true; callers treat a same-owner self-transition
// as a no-op rather than an error, because repeating a phase write is a
// retry, not a workflow change. Unknown phases never transition.
func CanTransition(from, to Phase) bool {
	row, ok := transitions[from]
	if !ok {
		return false
	}
	return row[to]
}

// ownershipLocked reports whether from carries a claim held by somebody other
// than by. Only in_progress and blocked are claims: the lock exists to stop
// two agents from working the same entry at once, so it exists only while
// somebody is actually on the entry. from==todo carries no claim (nobody has
// started), and testing/done/cancelled never do.
func ownershipLocked(from Phase, owner, by string) bool {
	if owner == "" || owner == by {
		return false
	}
	return from == PhaseInProgress || from == PhaseBlocked
}

// ClaimHeld reports whether from carries an ownership claim held by somebody
// other than by (the condition a takeover or staleness breaks). It is the
// exported form of the lock predicate so a writer can classify a move as an
// explicit or stale takeover without re-deriving the rule and drifting from it.
func ClaimHeld(from Phase, owner, by string) bool {
	return ownershipLocked(from, owner, by)
}

// TransitionAllowed is the full admission rule for one entry transition: the
// structural matrix plus the ownership lock. It is the single decision point
// so no caller can re-derive the rule and drift from it.
//
//  1. the transition must be structurally legal (CanTransition);
//  2. a claim held by another owner (from in_progress/blocked, owner non-empty
//     and not by) admits exactly one target: todo — the release hatch, so an
//     entry claimed by a vanished agent is never hostage; any other target is
//     refused;
//  3. that refusal is lifted by an explicit takeover or by staleness: the
//     claim has gone silent for longer than takeoverAfter (see IsStale).
//
// Parameters: owner is the current claim holder and by the actor (empty by
// means an unattributed actor, which counts as "not the owner"); phaseAt is
// when the current phase was entered and is the lock's clock; takeoverAfter
// is the silence window and takeover is the explicit override. takeoverAfter
// <= 0 means any phase timestamp strictly in the past is already stale.
func TransitionAllowed(from, to Phase, owner, by string, phaseAt, now time.Time, takeoverAfter time.Duration, takeover bool) bool {
	if !CanTransition(from, to) {
		return false
	}
	if !ownershipLocked(from, owner, by) {
		return true
	}
	if to == PhaseTodo {
		return true
	}
	if takeover {
		return true
	}
	return IsStale(phaseAt, now, takeoverAfter)
}

// Locked reports whether an ownership lock is the ONLY reason a transition is
// refused: the move is structurally legal and no takeover or staleness would
// rescue it. It exists so a caller can render a precise message ("claimed by
// alice 12m ago; wait, ask for a takeover, or release to todo") instead of the
// generic "invalid transition" that hides the actionable part.
//
// takeover is not a parameter on purpose: a caller that has already decided to
// take over is not asking for the diagnosis. Structural refusals return false
// — the lock is not what blocks them.
func Locked(from, to Phase, owner, by string, phaseAt, now time.Time, takeoverAfter time.Duration) bool {
	if !CanTransition(from, to) {
		return false
	}
	if !ownershipLocked(from, owner, by) {
		return false
	}
	if to == PhaseTodo {
		return false
	}
	return !IsStale(phaseAt, now, takeoverAfter)
}

// IsStale reports whether a phase timestamp has gone quiet long enough that a
// claim may be taken over. Staleness is strictly greater than takeoverAfter:
// exactly at the threshold the owner still holds the entry, which makes the
// constant a minimum hold time and keeps a boundary from flipping on a
// nanosecond.
//
// Two consequences worth stating: a zero phaseAt (older data) counts as stale,
// because an unknown timestamp must not lock an entry forever; a phaseAt in the
// future (clock skew) is never stale, so a skewed writer cannot be robbed.
func IsStale(phaseAt, now time.Time, takeoverAfter time.Duration) bool {
	return now.Sub(phaseAt) > takeoverAfter
}

// Entry is one fact inside a workset, with its work state. The fact's value
// lives in the facts index; an entry carries only the phase, the claim and the
// joining metadata. Storing the value here too would create a second source of
// truth that silently diverges — a workset is a work queue over facts, not a
// copy of them.
type Entry struct {
	FactID  string `json:"fact_id"`
	Note    string `json:"note,omitempty"`
	Phase   Phase  `json:"phase"`
	PhaseBy string `json:"phase_by,omitempty"`
	// PhaseAt is when the current phase was entered: the clock the ownership
	// lock and the staleness takeover are measured against.
	PhaseAt time.Time `json:"phase_at"`
	AddedAt time.Time `json:"added_at"`
	AddedBy string    `json:"added_by"`
}

// Cursor is one reader's position in a workset (see CursorKey), so agents
// reading the same list do not re-read what another has already consumed.
// UpdatedAt is both the recency marker and the eviction key: a reader that
// moved the cursor later is more likely to come back.
type Cursor struct {
	At        int       `json:"at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Workset is a named, shared, ordered list of facts being worked through, with
// a per-entry phase. It is the unit agents coordinate on: the name is the
// handle in commands, the entries are the queue, and each entry's phase and
// owner are the shared state two agents would otherwise keep in their heads.
//
// Aliases keep a renamed workset reachable under its old names: a rename must
// not break the handle another agent (or a human's note) still uses. Version is
// the workset document's write counter, bumped on every accepted change so a
// reader can tell a stale copy from a current one.
type Workset struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Aliases   []string          `json:"aliases,omitempty"`
	Project   string            `json:"project"`
	Entries   []Entry           `json:"entries"`
	Cursors   map[string]Cursor `json:"cursors,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	CreatedBy string            `json:"created_by,omitempty"`
	UpdatedBy string            `json:"updated_by,omitempty"`
	Version   int64             `json:"version"`
}

// WorksetJournalEntry is one append-only record of a workset change: the audit
// trail that answers "who moved this entry, when, and from what". Entries are
// immutable once written; a correction appends a new record, exactly like the
// fact journal.
type WorksetJournalEntry struct {
	WorksetID string `json:"workset_id"`
	Workset   string `json:"workset"`
	Op        string `json:"op"` // create|add|remove|rename|delete|phase|takeover|note
	// Takeover is set only on a move that broke another author's claim:
	// "explicit" when --takeover was required, "stale" when the claim had
	// gone quiet past the takeover window. Empty on every other row.
	Takeover string    `json:"takeover,omitempty"`
	FactID   string    `json:"fact_id,omitempty"`
	From     Phase     `json:"from,omitempty"`
	To       Phase     `json:"to,omitempty"`
	Note     string    `json:"note,omitempty"`
	By       string    `json:"by"`
	At       time.Time `json:"at"`
}

// MaxCursors bounds the per-reader cursor map. A cursor is navigation state,
// not data: every reader on every host adds one, so an unbounded map would
// make each workset document grow with the agent fleet and every write pay for
// it. Twenty leaves headroom over a typical fleet of a few agents.
const MaxCursors = 20

// DefaultProject is the project of a workset created without one. Worksets are
// project-scoped like facts, and defaulting keeps a workset visible to a
// cross-project listing instead of hiding it under an empty name.
const DefaultProject = "global"

// maxNameLen is the slug length cap: a handle people type, not a description.
const maxNameLen = 64

// NormalizeName canonicalizes a workset slug: surrounding whitespace trimmed,
// lowercased, then validated as 1..64 characters from [a-z0-9_], with no
// leading or trailing '_' and no consecutive "__".
//
// Why a slug and not free text: the name is a handle typed by agents and
// humans ("open the p2 workset"), so it needs exactly one canonical spelling —
// case and surrounding whitespace are noise, and an interior space or slash
// would split into two different terminals. The underscore rules keep the
// "__" marker look out of user names (it is reserved for generated
// qualifiers) and keep the compact project:workset rendering unambiguous.
//
// The accepted alphabet is ASCII, so the 64-character cap is also 64 bytes;
// any non-ASCII input is refused rather than transliterated, because a silent
// transliteration would give two spellings for one handle.
func NormalizeName(s string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(s))
	if name == "" {
		return "", fmt.Errorf("workset name is empty: a slug identifies the workset in commands")
	}
	if len(name) > maxNameLen {
		return "", fmt.Errorf("workset name is %d characters, over the %d limit: a name is a handle, not a description", len(name), maxNameLen)
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return "", fmt.Errorf("workset name %q contains %q: allowed are a-z, 0-9 and '_'", name, r)
	}
	if name[0] == '_' || name[len(name)-1] == '_' {
		return "", fmt.Errorf("workset name %q must not start or end with '_': the leading and trailing position is reserved", name)
	}
	if strings.Contains(name, "__") {
		return "", fmt.Errorf("workset name %q contains '__': the double underscore is reserved for generated qualifiers", name)
	}
	return name, nil
}

// CursorKey renders the per-reader cursor key: "author@host", or just author
// when the host is unknown. Author alone would collide across machines in a
// multi-machine store, and a bare "@host" would name no reader at all, so the
// separator appears only when both halves exist and an empty author yields no
// key (the writer treats that as "this reader cannot have a cursor").
func CursorKey(author, host string) string {
	if author == "" {
		return ""
	}
	if host == "" {
		return author
	}
	return author + "@" + host
}

// cursorLess orders cursors oldest first, ties broken lexicographically by
// key. The tie-break is not cosmetic: map iteration is random, so without it
// "which cursor got evicted" would differ between two identical calls.
func cursorLess(c map[string]Cursor, a, b string) bool {
	ta, tb := c[a].UpdatedAt, c[b].UpdatedAt
	if ta.Equal(tb) {
		return a < b
	}
	return ta.Before(tb)
}

// EvictCursors removes the oldest cursors by UpdatedAt while more than max
// remain, and returns the removed keys in removal order (oldest first, ties
// lexicographically). max <= 0 removes all cursors and returns them all.
//
// Eviction is by recency rather than by count-of-readers because a cursor only
// matters to a reader that will come back; the oldest one is the best guess at
// the one that will not. The map is bounded by MaxCursors, so the insertion
// sort costs nothing, and avoiding a sort import keeps this model free of
// dependencies beyond the standard formatting/string/time trio.
func EvictCursors(c map[string]Cursor, max int) []string {
	if len(c) == 0 {
		return nil
	}
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && cursorLess(c, keys[j], keys[j-1]); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	drop := len(keys) - max
	if max <= 0 {
		drop = len(keys)
	}
	if drop <= 0 {
		return nil
	}
	removed := make([]string, 0, drop)
	for _, k := range keys[:drop] {
		delete(c, k)
		removed = append(removed, k)
	}
	return removed
}

// MatchesName reports whether query resolves to this workset by its name or any
// alias. Both sides are compared in normalized form, so " P2_Fixes " matches
// "p2_fixes" and an alias written before normalization still resolves. A query
// that is not a valid slug matches nothing: it cannot name a workset, and
// treating it as a substring would make "p2 fixes" silently hit "p2_fixes".
func (w *Workset) MatchesName(query string) bool {
	q, err := NormalizeName(query)
	if err != nil {
		return false
	}
	if name, err := NormalizeName(w.Name); err == nil && name == q {
		return true
	}
	for _, alias := range w.Aliases {
		if name, err := NormalizeName(alias); err == nil && name == q {
			return true
		}
	}
	return false
}
