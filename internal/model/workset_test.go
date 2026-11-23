package model

import (
	"strings"
	"testing"
	"time"
)

// TestCanTransition pins the structural matrix cell by cell. The expected
// table is written out literally (6 phases x 6 targets) instead of derived
// from the implementation, so a change to the workflow has to be made twice on
// purpose and cannot slip in unnoticed.
func TestCanTransition(t *testing.T) {
	want := map[[2]Phase]bool{
		// todo
		{PhaseTodo, PhaseTodo}:       true,
		{PhaseTodo, PhaseInProgress}: true,
		{PhaseTodo, PhaseBlocked}:    true,
		{PhaseTodo, PhaseTesting}:    true,
		{PhaseTodo, PhaseDone}:       false,
		{PhaseTodo, PhaseCancelled}:  true,
		// in_progress
		{PhaseInProgress, PhaseTodo}:       true,
		{PhaseInProgress, PhaseInProgress}: true,
		{PhaseInProgress, PhaseBlocked}:    true,
		{PhaseInProgress, PhaseTesting}:    true,
		{PhaseInProgress, PhaseDone}:       false,
		{PhaseInProgress, PhaseCancelled}:  true,
		// blocked
		{PhaseBlocked, PhaseTodo}:       true,
		{PhaseBlocked, PhaseInProgress}: true,
		{PhaseBlocked, PhaseBlocked}:    true,
		{PhaseBlocked, PhaseTesting}:    true,
		{PhaseBlocked, PhaseDone}:       false,
		{PhaseBlocked, PhaseCancelled}:  true,
		// testing
		{PhaseTesting, PhaseTodo}:       true,
		{PhaseTesting, PhaseInProgress}: true,
		{PhaseTesting, PhaseBlocked}:    true,
		{PhaseTesting, PhaseTesting}:    true,
		{PhaseTesting, PhaseDone}:       true,
		{PhaseTesting, PhaseCancelled}:  true,
		// done
		{PhaseDone, PhaseTodo}:       true,
		{PhaseDone, PhaseInProgress}: true,
		{PhaseDone, PhaseBlocked}:    false,
		{PhaseDone, PhaseTesting}:    true,
		{PhaseDone, PhaseDone}:       true,
		{PhaseDone, PhaseCancelled}:  false,
		// cancelled
		{PhaseCancelled, PhaseTodo}:       true,
		{PhaseCancelled, PhaseInProgress}: true,
		{PhaseCancelled, PhaseBlocked}:    false,
		{PhaseCancelled, PhaseTesting}:    false,
		{PhaseCancelled, PhaseDone}:       false,
		{PhaseCancelled, PhaseCancelled}:  true,
	}
	if len(want) != 36 {
		t.Fatalf("expected table has %d cells, want 36", len(want))
	}

	phases := AllPhases()
	if len(phases) != 6 {
		t.Fatalf("AllPhases() = %d phases, want 6", len(phases))
	}
	for _, from := range phases {
		for _, to := range phases {
			exp, ok := want[[2]Phase{from, to}]
			if !ok {
				t.Fatalf("expected table is missing %s -> %s", from, to)
			}
			if got := CanTransition(from, to); got != exp {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, exp)
			}
		}
	}

	// Unknown phases are not transitions in either direction.
	for _, bad := range []Phase{"", "doing", "TODO", "in-progress"} {
		if CanTransition(bad, PhaseTodo) {
			t.Errorf("CanTransition(%q, todo) = true, want false (unknown source)", bad)
		}
		if CanTransition(PhaseTodo, bad) {
			t.Errorf("CanTransition(todo, %q) = true, want false (unknown target)", bad)
		}
	}
}

// TestPhaseHelpers pins that the phase set is exactly the six workflow states
// and that AllPhases hands out a copy: callers build menus from it, and a
// shared slice would let one caller's edit corrupt every later listing.
func TestPhaseHelpers(t *testing.T) {
	phases := AllPhases()
	wantOrder := []Phase{PhaseTodo, PhaseInProgress, PhaseBlocked, PhaseTesting, PhaseDone, PhaseCancelled}
	if len(phases) != len(wantOrder) {
		t.Fatalf("AllPhases() = %v, want %v", phases, wantOrder)
	}
	for i, p := range wantOrder {
		if phases[i] != p {
			t.Errorf("AllPhases()[%d] = %q, want %q", i, phases[i], p)
		}
		if !ValidPhase(p) {
			t.Errorf("ValidPhase(%q) = false, want true", p)
		}
	}
	phases[0] = "tampered"
	if AllPhases()[0] != PhaseTodo {
		t.Error("AllPhases must return a copy: a caller mutated the shared list")
	}
	for _, bad := range []Phase{"", "TODO", "in-progress", "wip"} {
		if ValidPhase(bad) {
			t.Errorf("ValidPhase(%q) = true, want false", bad)
		}
	}
}

// TestTransitionAllowedAndLocked pins the admission rule as a combination of
// the matrix and the ownership lock: a claim by another owner narrows the
// allowed targets to todo only, and takeover or staleness lifts the claim.
func TestTransitionAllowedAndLocked(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	const window = time.Hour
	fresh := now.Add(-10 * time.Minute)
	stale := now.Add(-2 * window)
	atThreshold := now.Add(-window)

	cases := []struct {
		name          string
		from, to      Phase
		owner, by     string
		phaseAt       time.Time
		takeoverAfter time.Duration
		takeover      bool
		wantAllowed   bool
		wantLocked    bool
	}{
		{
			name: "same owner is never locked",
			from: PhaseInProgress, to: PhaseTesting,
			owner: "alice", by: "alice", phaseAt: fresh, takeoverAfter: window,
			wantAllowed: true, wantLocked: false,
		},
		{
			name: "other owner cannot advance to testing",
			from: PhaseInProgress, to: PhaseTesting,
			owner: "alice", by: "bob", phaseAt: fresh, takeoverAfter: window,
			wantAllowed: false, wantLocked: true,
		},
		{
			name: "other owner may release to todo",
			from: PhaseInProgress, to: PhaseTodo,
			owner: "alice", by: "bob", phaseAt: fresh, takeoverAfter: window,
			wantAllowed: true, wantLocked: false,
		},
		{
			name: "blocked to done is structurally forbidden for anybody",
			from: PhaseBlocked, to: PhaseDone,
			owner: "alice", by: "bob", phaseAt: fresh, takeoverAfter: window,
			wantAllowed: false, wantLocked: false,
		},
		{
			name: "takeover on a forbidden move stays forbidden",
			from: PhaseInProgress, to: PhaseDone,
			owner: "alice", by: "bob", phaseAt: fresh, takeoverAfter: window, takeover: true,
			wantAllowed: false, wantLocked: false,
		},
		{
			name: "stale claim may be taken over",
			from: PhaseInProgress, to: PhaseTesting,
			owner: "alice", by: "bob", phaseAt: stale, takeoverAfter: window,
			wantAllowed: true, wantLocked: false,
		},
		{
			name: "explicit takeover lifts the claim",
			from: PhaseInProgress, to: PhaseBlocked,
			owner: "alice", by: "bob", phaseAt: fresh, takeoverAfter: window, takeover: true,
			// Locked is takeover-blind by design: it reports what the claim
			// alone would refuse, because the caller overrode it already.
			wantAllowed: true, wantLocked: true,
		},
		{
			name: "exactly at the threshold is still locked",
			from: PhaseInProgress, to: PhaseTesting,
			owner: "alice", by: "bob", phaseAt: atThreshold, takeoverAfter: window,
			wantAllowed: false, wantLocked: true,
		},
		{
			name: "todo ignores the owner field",
			from: PhaseTodo, to: PhaseInProgress,
			owner: "alice", by: "bob", phaseAt: fresh, takeoverAfter: window,
			wantAllowed: true, wantLocked: false,
		},
		{
			name: "testing never locks",
			from: PhaseTesting, to: PhaseBlocked,
			owner: "alice", by: "bob", phaseAt: fresh, takeoverAfter: window,
			wantAllowed: true, wantLocked: false,
		},
		{
			name: "done never locks",
			from: PhaseDone, to: PhaseTesting,
			owner: "alice", by: "bob", phaseAt: fresh, takeoverAfter: window,
			wantAllowed: true, wantLocked: false,
		},
		{
			name: "cancelled never locks",
			from: PhaseCancelled, to: PhaseInProgress,
			owner: "alice", by: "bob", phaseAt: fresh, takeoverAfter: window,
			wantAllowed: true, wantLocked: false,
		},
		{
			name: "blocked claim: another owner may release to todo",
			from: PhaseBlocked, to: PhaseTodo,
			owner: "alice", by: "bob", phaseAt: fresh, takeoverAfter: window,
			wantAllowed: true, wantLocked: false,
		},
		{
			name: "blocked claim: another owner cannot resume work",
			from: PhaseBlocked, to: PhaseInProgress,
			owner: "alice", by: "bob", phaseAt: fresh, takeoverAfter: window,
			wantAllowed: false, wantLocked: true,
		},
		{
			name: "self-transition by another owner is locked, not a no-op",
			from: PhaseInProgress, to: PhaseInProgress,
			owner: "alice", by: "bob", phaseAt: fresh, takeoverAfter: window,
			wantAllowed: false, wantLocked: true,
		},
		{
			name: "self-transition by the owner is allowed",
			from: PhaseInProgress, to: PhaseInProgress,
			owner: "alice", by: "alice", phaseAt: fresh, takeoverAfter: window,
			wantAllowed: true, wantLocked: false,
		},
		{
			name: "unknown target phase is refused structurally",
			from: PhaseInProgress, to: Phase("doing"),
			owner: "alice", by: "bob", phaseAt: fresh, takeoverAfter: window,
			wantAllowed: false, wantLocked: false,
		},
	}

	for _, tc := range cases {
		gotAllowed := TransitionAllowed(tc.from, tc.to, tc.owner, tc.by, tc.phaseAt, now, tc.takeoverAfter, tc.takeover)
		if gotAllowed != tc.wantAllowed {
			t.Errorf("%s: TransitionAllowed = %v, want %v", tc.name, gotAllowed, tc.wantAllowed)
		}
		gotLocked := Locked(tc.from, tc.to, tc.owner, tc.by, tc.phaseAt, now, tc.takeoverAfter)
		if gotLocked != tc.wantLocked {
			t.Errorf("%s: Locked = %v, want %v", tc.name, gotLocked, tc.wantLocked)
		}
	}
}

// TestIsStalePhaseBoundaries pins the staleness boundary: strictly greater
// than the window. Exactly at the threshold the owner still holds the entry,
// so the constant reads as a minimum hold time; an unknown (zero) timestamp is
// stale so old data cannot lock an entry forever, while a future timestamp
// (clock skew) is not.
func TestIsStalePhaseBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	const window = time.Hour

	cases := []struct {
		name          string
		phaseAt       time.Time
		takeoverAfter time.Duration
		want          bool
	}{
		{"exactly at the threshold is not stale", now.Add(-window), window, false},
		{"one nanosecond under is not stale", now.Add(-window + time.Nanosecond), window, false},
		{"one nanosecond over is stale", now.Add(-window - time.Nanosecond), window, true},
		{"just entered is fresh", now, window, false},
		{"zero timestamp is stale", time.Time{}, window, true},
		{"future timestamp is never stale", now.Add(time.Hour), window, false},
		{"zero window and no age is fresh", now, 0, false},
		{"zero window with any age is stale", now.Add(-time.Nanosecond), 0, true},
		{"negative window makes now stale", now, -time.Second, true},
	}
	for _, tc := range cases {
		if got := IsStale(tc.phaseAt, now, tc.takeoverAfter); got != tc.want {
			t.Errorf("%s: IsStale = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestEvictCursors pins the eviction contract: oldest first, ties broken by
// key so two identical calls evict the same cursors, and max <= 0 drops
// everything.
func TestEvictCursors(t *testing.T) {
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }

	t.Run("oldest are removed by age, not by key order", func(t *testing.T) {
		c := map[string]Cursor{
			"z": {At: 0, UpdatedAt: at(0)}, // oldest, but lexicographically last
			"m": {At: 1, UpdatedAt: at(1)},
			"a": {At: 2, UpdatedAt: at(2)}, // newest
		}
		got := EvictCursors(c, 1)
		want := []string{"z", "m"}
		if len(got) != len(want) {
			t.Fatalf("removed = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("removed = %v, want %v", got, want)
			}
		}
		if len(c) != 1 || c["a"].At != 2 {
			t.Errorf("remaining = %v, want only the newest cursor", c)
		}
	})

	t.Run("ties are broken lexicographically", func(t *testing.T) {
		c := map[string]Cursor{
			"d": {At: 3, UpdatedAt: at(1)},
			"b": {At: 1, UpdatedAt: at(0)},
			"a": {At: 0, UpdatedAt: at(0)},
			"c": {At: 2, UpdatedAt: at(1)},
		}
		first := EvictCursors(c, 2)
		want := []string{"a", "b"}
		if len(first) != len(want) {
			t.Fatalf("removed = %v, want %v", first, want)
		}
		for i := range want {
			if first[i] != want[i] {
				t.Fatalf("removed = %v, want %v", first, want)
			}
		}
		if len(c) != 2 || c["c"].At != 2 || c["d"].At != 3 {
			t.Errorf("remaining = %v, want c and d", c)
		}

		// Determinism: the same input state yields the same eviction order.
		c2 := map[string]Cursor{
			"d": {At: 3, UpdatedAt: at(1)},
			"b": {At: 1, UpdatedAt: at(0)},
			"a": {At: 0, UpdatedAt: at(0)},
			"c": {At: 2, UpdatedAt: at(1)},
		}
		second := EvictCursors(c2, 2)
		if strings.Join(first, ",") != strings.Join(second, ",") {
			t.Errorf("eviction order differs between identical calls: %v vs %v", first, second)
		}
	})

	t.Run("max zero removes all", func(t *testing.T) {
		c := map[string]Cursor{
			"a": {At: 0, UpdatedAt: at(2)},
			"b": {At: 1, UpdatedAt: at(0)},
			"c": {At: 2, UpdatedAt: at(1)},
		}
		got := EvictCursors(c, 0)
		want := []string{"b", "c", "a"} // oldest first, ties (none) by key
		if len(got) != len(want) {
			t.Fatalf("removed = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("removed = %v, want %v", got, want)
			}
		}
		if len(c) != 0 {
			t.Errorf("map still holds %d cursors, want 0", len(c))
		}
	})

	t.Run("negative max removes all too", func(t *testing.T) {
		c := map[string]Cursor{"a": {At: 0, UpdatedAt: at(0)}}
		got := EvictCursors(c, -1)
		if len(got) != 1 || got[0] != "a" {
			t.Errorf("removed = %v, want [a]", got)
		}
		if len(c) != 0 {
			t.Errorf("map still holds %d cursors, want 0", len(c))
		}
	})

	t.Run("under the cap nothing is removed", func(t *testing.T) {
		c := map[string]Cursor{"a": {At: 0, UpdatedAt: at(0)}}
		if got := EvictCursors(c, 5); got != nil {
			t.Errorf("removed = %v, want nil", got)
		}
		if len(c) != 1 {
			t.Errorf("map holds %d cursors, want the untouched one", len(c))
		}
	})

	t.Run("nil map is a no-op", func(t *testing.T) {
		if got := EvictCursors(nil, 5); got != nil {
			t.Errorf("removed = %v, want nil", got)
		}
		if got := EvictCursors(nil, 0); got != nil {
			t.Errorf("removed = %v, want nil", got)
		}
	})
}

// TestNormalizeName pins the slug rule the workset handle depends on: one
// canonical spelling (trimmed, lowercased) from a narrow alphabet, so a handle
// typed by an agent and by a human resolves to the same workset.
func TestNormalizeName(t *testing.T) {
	valid := []struct{ in, want string }{
		{"team", "team"},
		{"  Team  ", "team"},
		{"P2_Fixes", "p2_fixes"},
		{"a", "a"},
		{"x_1", "x_1"},
		{"a_b_c", "a_b_c"},
		{"todo_2026q1", "todo_2026q1"},
		{strings.Repeat("a", maxNameLen), strings.Repeat("a", maxNameLen)},
	}
	for _, tc := range valid {
		got, err := NormalizeName(tc.in)
		if err != nil {
			t.Errorf("NormalizeName(%q) = %q, %v; want %q, nil", tc.in, got, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	invalid := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"only whitespace", "   "},
		{"leading underscore", "_lead"},
		{"trailing underscore", "trail_"},
		{"only underscores", "_"},
		{"double underscore", "double__underscore"},
		{"interior space", "with space"},
		{"dash", "has-dash"},
		{"slash", "a/b"},
		{"dot", "v1.2"},
		{"over the length cap", strings.Repeat("a", maxNameLen+1)},
		{"unicode letters", "имя"},
		{"emoji", "p2🙂"},
		{"non-breaking space", "p2\u00a0x"},
	}
	for _, tc := range invalid {
		if got, err := NormalizeName(tc.in); err == nil {
			t.Errorf("%s: NormalizeName(%q) = %q, nil; want an error", tc.name, tc.in, got)
		}
	}
}

// TestWorksetMatchesName pins lookup semantics: a query resolves by name or
// alias in normalized form (case and surrounding whitespace are noise), and an
// invalid query matches nothing rather than guessing.
func TestWorksetMatchesName(t *testing.T) {
	// Aliases are slugs like the name: a dash is not in the alphabet, so an
	// alias written with one could never resolve and is not used here.
	w := &Workset{
		Name:    "p2_fixes",
		Aliases: []string{"phase_two", "P2_Fixes_Old"},
	}

	matching := []string{
		"p2_fixes",
		"P2_FIXES",
		"  p2_fixes  ",
		"phase_two",
		"PHASE_TWO",
		" p2_fixes_old ",
	}
	for _, q := range matching {
		if !w.MatchesName(q) {
			t.Errorf("MatchesName(%q) = false, want true", q)
		}
	}

	notMatching := []string{
		"other",
		"p2",
		"p2_fixes_v2",
		"",
		"   ",
		"p2 fixes",  // invalid query: a space cannot name a workset
		"p2__fixes", // invalid query: double underscore
		"p2-fixes",  // a dash is not in the alphabet for queries either
	}
	for _, q := range notMatching {
		if w.MatchesName(q) {
			t.Errorf("MatchesName(%q) = true, want false", q)
		}
	}

	// An empty workset matches nothing, not even an empty query.
	if (&Workset{}).MatchesName("") {
		t.Error("empty workset must not match an empty query")
	}

	// A stored name that is not a valid slug is skipped rather than failing
	// the lookup: legacy data must not crash resolution for other worksets.
	legacy := &Workset{Name: "Bad Name", Aliases: []string{"good_name"}}
	if !legacy.MatchesName("good_name") {
		t.Error("a valid alias must still resolve when the name is not a slug")
	}
	if legacy.MatchesName("bad name") {
		t.Error("an invalid stored name must not resolve")
	}
}

// TestCursorKey pins the key shape: the separator appears only when both
// halves exist, and an empty author names no reader (SetCursor treats the
// empty key as an error rather than storing an anonymous "@host" cursor).
func TestCursorKey(t *testing.T) {
	cases := []struct {
		author, host, want string
	}{
		{author: "alice", host: "box", want: "alice@box"},
		{author: "alice", host: "", want: "alice"},
		{author: "", host: "box", want: ""},
		{author: "", host: "", want: ""},
	}
	for _, tc := range cases {
		if got := CursorKey(tc.author, tc.host); got != tc.want {
			t.Errorf("CursorKey(%q, %q) = %q, want %q", tc.author, tc.host, got, tc.want)
		}
	}
}

// TestClaimHeld pins the exported lock predicate the writer uses to classify a
// takeover: only in_progress/blocked claims held by another author count.
func TestClaimHeld(t *testing.T) {
	if !ClaimHeld(PhaseInProgress, "alice", "bob") {
		t.Error("in_progress held by alice must block bob")
	}
	if !ClaimHeld(PhaseBlocked, "alice", "bob") {
		t.Error("blocked held by alice must block bob")
	}
	if ClaimHeld(PhaseInProgress, "alice", "alice") {
		t.Error("the owner's own claim must not count as held")
	}
	if ClaimHeld(PhaseTesting, "alice", "bob") {
		t.Error("testing is not a claim")
	}
	if ClaimHeld(PhaseInProgress, "", "bob") {
		t.Error("an unattributed claim must not count")
	}
}
