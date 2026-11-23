package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/NemoNobody/velruna/internal/model"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// Workset client surface. A workset is a named, shared queue of facts with a
// per-entry phase; this file holds the CRUD paths and the journal append.
//
// Every mutation follows the same shape as the fact writes: read the document
// with its version pair, compute the new state in Go, write it back with
// if_seq_no/if_primary_term, then append the journal entry. The CAS condition
// is what turns "two agents moved the same entry" into an honest
// ErrWorksetConflict ("re-read and retry") instead of a silent clobber. The
// document is written before its journal entry, so a lagging journal is
// reported as such and never claims a change that did not land.

// FactState is the fact's state as seen from a workset: the facts index only
// distinguishes verified from obsolete; a fact id that is not there at all is
// missing, which is what an entry pointing at a deleted fact looks like.
type FactState string

const (
	FactStateVerified FactState = "verified"
	FactStateObsolete FactState = "obsolete"
	FactStateMissing  FactState = "missing"
)

var (
	// ErrWorksetNotFound reports that no workset answers to the given name or
	// alias.
	ErrWorksetNotFound = errors.New("workset not found")
	// ErrWorksetExists reports that a name or alias already names another
	// workset (create and rename both enforce uniqueness).
	ErrWorksetExists = errors.New("workset already exists")
	// ErrWorksetConflict reports a CAS failure: a concurrent writer changed the
	// workset between this call's read and its conditional write. The caller
	// re-reads and retries.
	ErrWorksetConflict = errors.New("workset changed under this write")
	// ErrWorksetLocked reports that an entry is claimed by another author and
	// the requested transition is refused by the ownership lock.
	ErrWorksetLocked = errors.New("workset entry is claimed by another author")
)

// WorksetCreate is the input of CreateWorkset.
type WorksetCreate struct {
	Name    string
	Project string
	By      string
}

// WorksetAdd is the input of AddEntries. A zero Phase means model.PhaseTodo.
// The input carries no author: the writer is the client's resolved identity
// (see resolveAuthor), exactly like the fact write paths.
type WorksetAdd struct {
	FactIDs []string
	Note    string
	Phase   model.Phase
}

// WorksetPhaseChange is the input of SetPhases (and the takeover case).
type WorksetPhaseChange struct {
	FactIDs  []string
	To       model.Phase
	Note     string
	By       string
	Takeover bool
}

// CursorInput is the input of SetCursor. Clear removes the reader's cursor;
// otherwise At is stored with UpdatedAt=now.
type CursorInput struct {
	Author string
	Host   string
	At     int
	Clear  bool
}

// CreateWorkset creates a workset. The name (and, after renames, every alias)
// must be unique: the search-then-create check is not atomic with the create,
// but the create itself is op_type=create on a fresh id, so a lost race here
// yields a duplicate workset only in the (unlikely) window between the check
// and the write — acceptable for a human-facing handle, and journals name both
// documents.
func (c *Client) CreateWorkset(ctx context.Context, in WorksetCreate) (*model.Workset, error) {
	name, err := model.NormalizeName(in.Name)
	if err != nil {
		return nil, contractErrorf("create workset: %v", err)
	}
	project := in.Project
	if project == "" {
		project = model.DefaultProject
	}
	if _, _, _, rerr := c.resolveWorkset(ctx, name); rerr == nil {
		return nil, fmt.Errorf("%w: %q", ErrWorksetExists, name)
	} else if !errors.Is(rerr, ErrWorksetNotFound) {
		return nil, rerr
	}

	now := time.Now().UTC()
	ws := &model.Workset{
		ID:        uuid.New().String(),
		Name:      name,
		Project:   project,
		Entries:   []model.Entry{},
		CreatedAt: now,
		UpdatedAt: now,
		CreatedBy: in.By,
		UpdatedBy: in.By,
		Version:   1,
	}
	body, _ := json.Marshal(ws)
	if _, err := c.api.Index(ctx, opensearchapi.IndexReq{
		Index:      c.worksetsIndex(),
		DocumentID: ws.ID,
		Body:       bytes.NewReader(body),
		Params:     opensearchapi.IndexParams{Refresh: "wait_for", OpType: "create"},
	}); err != nil {
		if isVersionConflict(err) {
			// The only colliding key is the fresh uuid (the name race is
			// handled above): treat it as the same "already exists" answer.
			return nil, fmt.Errorf("%w: %q", ErrWorksetExists, name)
		}
		return nil, wrapWriteError("create workset", err)
	}
	if jerr := c.appendWorksetJournal(ctx, model.WorksetJournalEntry{
		WorksetID: ws.ID,
		Workset:   ws.Name,
		Op:        "create",
		By:        in.By,
		At:        now,
	}); jerr != nil {
		return ws, fmt.Errorf("create workset: %q was created, the journal entry lagged: %w", name, jerr)
	}
	return ws, nil
}

// GetWorkset resolves a name or alias to the workset document. The search
// answers "which document", the realtime GET that follows answers "what is in
// it, at which version" — a search alone lags the refresh window.
func (c *Client) GetWorkset(ctx context.Context, nameOrAlias string) (*model.Workset, error) {
	ws, _, _, err := c.resolveWorkset(ctx, nameOrAlias)
	return ws, err
}

// ListWorksets lists worksets, optionally narrowed to one project. The list is
// ordered by name so two runs render the same page.
func (c *Client) ListWorksets(ctx context.Context, project string) ([]*model.Workset, error) {
	resp, err := c.worksetSearch(ctx, buildListWorksetsBody(project))
	if err != nil {
		return nil, err
	}
	out := make([]*model.Workset, 0, len(resp.Hits.Hits))
	for _, hit := range resp.Hits.Hits {
		var ws model.Workset
		if err := json.Unmarshal(hit.Source, &ws); err != nil {
			return nil, fmt.Errorf("list worksets: decode %q: %w", hit.ID, err)
		}
		if ws.ID == "" {
			ws.ID = hit.ID
		}
		out = append(out, &ws)
	}
	return out, nil
}

// AddEntries appends facts to a workset. It is idempotent: a fact already in
// the workset is skipped and never re-phased (a re-add is a retry, not a claim).
// New entries start in the requested phase (todo by default) and are owned by
// the client's resolved author.
func (c *Client) AddEntries(ctx context.Context, name string, in WorksetAdd) (*model.Workset, error) {
	phase := in.Phase
	if phase == "" {
		phase = model.PhaseTodo
	}
	if !model.ValidPhase(phase) {
		return nil, contractErrorf("add entries: unknown phase %q", phase)
	}
	ws, seqNo, primaryTerm, err := c.resolveWorkset(ctx, name)
	if err != nil {
		return nil, err
	}
	by := c.resolveAuthor(ctx).Author
	now := time.Now().UTC()

	present := make(map[string]struct{}, len(ws.Entries))
	for _, e := range ws.Entries {
		present[e.FactID] = struct{}{}
	}
	var added []model.Entry
	var journal []model.WorksetJournalEntry
	seen := make(map[string]struct{}, len(in.FactIDs))
	for _, id := range in.FactIDs {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if _, exists := present[id]; exists {
			continue
		}
		added = append(added, model.Entry{
			FactID:  id,
			Note:    in.Note,
			Phase:   phase,
			PhaseBy: by,
			PhaseAt: now,
			AddedAt: now,
			AddedBy: by,
		})
		journal = append(journal, model.WorksetJournalEntry{
			WorksetID: ws.ID,
			Workset:   ws.Name,
			Op:        "add",
			FactID:    id,
			To:        phase,
			Note:      in.Note,
			By:        by,
			At:        now,
		})
	}
	if len(added) == 0 {
		// Every fact was already there: no document write, no journal rows.
		return ws, nil
	}
	ws.Entries = append(ws.Entries, added...)
	ws.UpdatedAt = now
	ws.UpdatedBy = by
	ws.Version++
	if err := c.putWorksetCAS(ctx, ws, seqNo, primaryTerm, "add entries"); err != nil {
		return nil, err
	}
	if jerr := c.appendWorksetJournals(ctx, journal); jerr != nil {
		return ws, fmt.Errorf("add entries: workset %q was updated, the journal entry lagged: %w", ws.Name, jerr)
	}
	return ws, nil
}

// RemoveEntries drops facts from a workset. Absent fact ids are skipped, so a
// retry is a no-op; each removed fact leaves an op=remove journal row.
func (c *Client) RemoveEntries(ctx context.Context, name string, factIDs []string, by string) (*model.Workset, error) {
	ws, seqNo, primaryTerm, err := c.resolveWorkset(ctx, name)
	if err != nil {
		return nil, err
	}
	drop := make(map[string]struct{}, len(factIDs))
	for _, id := range factIDs {
		drop[id] = struct{}{}
	}
	now := time.Now().UTC()
	kept := make([]model.Entry, 0, len(ws.Entries))
	var removed []model.Entry
	for _, e := range ws.Entries {
		if _, ok := drop[e.FactID]; ok {
			removed = append(removed, e)
			continue
		}
		kept = append(kept, e)
	}
	if len(removed) == 0 {
		return ws, nil
	}
	ws.Entries = kept
	ws.UpdatedAt = now
	if by != "" {
		ws.UpdatedBy = by
	}
	ws.Version++
	if err := c.putWorksetCAS(ctx, ws, seqNo, primaryTerm, "remove entries"); err != nil {
		return nil, err
	}
	journal := make([]model.WorksetJournalEntry, 0, len(removed))
	for _, e := range removed {
		journal = append(journal, model.WorksetJournalEntry{
			WorksetID: ws.ID,
			Workset:   ws.Name,
			Op:        "remove",
			FactID:    e.FactID,
			From:      e.Phase,
			By:        by,
			At:        now,
		})
	}
	if jerr := c.appendWorksetJournals(ctx, journal); jerr != nil {
		return ws, fmt.Errorf("remove entries: workset %q was updated, the journal entry lagged: %w", ws.Name, jerr)
	}
	return ws, nil
}

// SetPhases moves entries to a new phase. All fact ids are validated before
// anything is written, so a bad id in the batch leaves the workset untouched.
// The ownership rule itself is model.TransitionAllowed; this method only
// decides between the lock diagnosis and the generic structural refusal, and
// records whether an explicit takeover was what admitted the move.
func (c *Client) SetPhases(ctx context.Context, name string, in WorksetPhaseChange) (*model.Workset, error) {
	if !model.ValidPhase(in.To) {
		return nil, contractErrorf("set phase: unknown phase %q", in.To)
	}
	ws, seqNo, primaryTerm, err := c.resolveWorkset(ctx, name)
	if err != nil {
		return nil, err
	}
	index := make(map[string]int, len(ws.Entries))
	for i := range ws.Entries {
		index[ws.Entries[i].FactID] = i
	}
	now := time.Now().UTC()
	takeoverAfter := time.Duration(c.cfg.WorksetTakeoverSeconds) * time.Second

	// Pass 1: validate every id and every transition without mutating.
	type planItem struct {
		idx              int
		from             model.Phase
		takeoverRequired bool
		takeover         string // "explicit" | "stale" | ""
		noteOnly         bool   // same phase, only the note changes
	}
	plan := make([]planItem, 0, len(in.FactIDs))
	seen := make(map[string]struct{}, len(in.FactIDs))
	for _, id := range in.FactIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		idx, ok := index[id]
		if !ok {
			return nil, fmt.Errorf("set phase: workset %q has no entry for fact %q: add it first", ws.Name, id)
		}
		e := &ws.Entries[idx]
		if e.Phase == in.To && !model.ClaimHeld(e.Phase, e.PhaseBy, in.By) {
			// Repeating the same phase is a no-op: a retry for the owner, and an
			// unlocked phase (todo/testing/done/cancelled) has no claim to move.
			// Only a live claim held by another author makes the same value a
			// takeover. A non-empty note on an unchanged phase is a note update,
			// not a workflow change: journal it instead of dropping it silently.
			if in.Note == "" {
				continue
			}
			plan = append(plan, planItem{idx: idx, from: e.Phase, noteOnly: true})
			continue
		}
		if !model.TransitionAllowed(e.Phase, in.To, e.PhaseBy, in.By, e.PhaseAt, now, takeoverAfter, in.Takeover) {
			if model.Locked(e.Phase, in.To, e.PhaseBy, in.By, e.PhaseAt, now, takeoverAfter) {
				return nil, fmt.Errorf("%w: workset %q fact %q: phase_by=%q phase_at=%s (phase %q): use --takeover or move to todo first",
					ErrWorksetLocked, ws.Name, id, e.PhaseBy, e.PhaseAt.UTC().Format(time.RFC3339), e.Phase)
			}
			return nil, fmt.Errorf("set phase: workset %q fact %q cannot move from %q to %q", ws.Name, id, e.Phase, in.To)
		}
		// takeoverRequired marks the moves admitted ONLY by the explicit flag:
		// the strict rule (staleness included) refuses them. A claim crossed
		// without the flag is a stale takeover: the holder went quiet past the
		// window and the move needed neither the flag nor a release to todo.
		takeoverRequired := !model.TransitionAllowed(e.Phase, in.To, e.PhaseBy, in.By, e.PhaseAt, now, takeoverAfter, false)
		takeover := ""
		if model.ClaimHeld(e.Phase, e.PhaseBy, in.By) && in.To != model.PhaseTodo {
			if takeoverRequired {
				takeover = "explicit"
			} else {
				takeover = "stale"
			}
		}
		plan = append(plan, planItem{idx: idx, from: e.Phase, takeoverRequired: takeoverRequired, takeover: takeover})
	}
	if len(plan) == 0 {
		return ws, nil
	}

	// Pass 2: apply. Nothing above this point touched the document.
	var journal []model.WorksetJournalEntry
	for _, p := range plan {
		e := &ws.Entries[p.idx]
		if !p.noteOnly {
			e.Phase = in.To
			e.PhaseBy = in.By
			e.PhaseAt = now
		}
		if in.Note != "" {
			e.Note = in.Note
		}
		op := "phase"
		switch {
		case p.noteOnly:
			op = "note"
		case p.takeoverRequired:
			op = "takeover"
		}
		journal = append(journal, model.WorksetJournalEntry{
			WorksetID: ws.ID,
			Workset:   ws.Name,
			Op:        op,
			Takeover:  p.takeover,
			FactID:    e.FactID,
			From:      p.from,
			To:        in.To,
			Note:      in.Note,
			By:        in.By,
			At:        now,
		})
	}
	ws.UpdatedAt = now
	if in.By != "" {
		ws.UpdatedBy = in.By
	}
	ws.Version++
	if err := c.putWorksetCAS(ctx, ws, seqNo, primaryTerm, "set phase"); err != nil {
		return nil, err
	}
	if jerr := c.appendWorksetJournals(ctx, journal); jerr != nil {
		return ws, fmt.Errorf("set phase: workset %q was updated, the journal entry lagged: %w", ws.Name, jerr)
	}
	return ws, nil
}

// RenameWorkset changes the handle of a workset. The old name becomes an alias
// so references to it (notes, running agents) keep resolving; the target name
// is dropped from the alias list if it was there. A target already held by
// another workset is refused — but a target that resolves back to THIS
// document (it is already one of its aliases) is fine.
func (c *Client) RenameWorkset(ctx context.Context, name, newName, by string) (*model.Workset, error) {
	target, err := model.NormalizeName(newName)
	if err != nil {
		return nil, contractErrorf("rename workset: %v", err)
	}
	ws, seqNo, primaryTerm, err := c.resolveWorkset(ctx, name)
	if err != nil {
		return nil, err
	}
	if target == ws.Name {
		return ws, nil
	}
	existing, _, _, rerr := c.resolveWorkset(ctx, target)
	switch {
	case rerr == nil:
		if existing.ID != ws.ID {
			return nil, fmt.Errorf("%w: %q is already used by workset %q (id %s): choose another name",
				ErrWorksetExists, target, existing.Name, existing.ID)
		}
	case errors.Is(rerr, ErrWorksetNotFound):
		// Free: proceed.
	default:
		return nil, rerr
	}

	now := time.Now().UTC()
	oldName := ws.Name
	ws.Aliases = appendWorksetAlias(ws.Aliases, oldName, target)
	ws.Name = target
	ws.UpdatedAt = now
	if by != "" {
		ws.UpdatedBy = by
	}
	ws.Version++
	if err := c.putWorksetCAS(ctx, ws, seqNo, primaryTerm, "rename workset"); err != nil {
		return nil, err
	}
	if jerr := c.appendWorksetJournal(ctx, model.WorksetJournalEntry{
		WorksetID: ws.ID,
		Workset:   ws.Name,
		Op:        "rename",
		Note:      "from " + oldName,
		By:        by,
		At:        now,
	}); jerr != nil {
		return ws, fmt.Errorf("rename workset: renamed %q to %q, the journal entry lagged: %w", oldName, target, jerr)
	}
	return ws, nil
}

// DeleteWorkset removes a workset by id, CAS-conditioned on the read that
// produced the journal row. The op=delete journal entry is appended after the
// delete: the document is already gone, so a journal failure is surfaced but
// the operation stands.
func (c *Client) DeleteWorkset(ctx context.Context, name, by string) error {
	ws, seqNo, primaryTerm, err := c.resolveWorkset(ctx, name)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, derr := c.api.Document.Delete(ctx, opensearchapi.DocumentDeleteReq{
		Index:      c.worksetsIndex(),
		DocumentID: ws.ID,
		Params: opensearchapi.DocumentDeleteParams{
			Refresh:       "wait_for",
			IfSeqNo:       &seqNo,
			IfPrimaryTerm: &primaryTerm,
		},
	}); derr != nil {
		if isVersionConflict(derr) {
			return worksetConflict("delete workset", ws.Name, derr)
		}
		if isNotFound(derr) {
			return fmt.Errorf("%w: %q", ErrWorksetNotFound, name)
		}
		return wrapWriteError("delete workset", derr)
	}
	if jerr := c.appendWorksetJournal(ctx, model.WorksetJournalEntry{
		WorksetID: ws.ID,
		Workset:   ws.Name,
		Op:        "delete",
		By:        by,
		At:        now,
	}); jerr != nil {
		return fmt.Errorf("delete workset: %q was deleted, the journal entry lagged: %w", ws.Name, jerr)
	}
	return nil
}

// SetCursor records a reader's position in a workset. Cursors are bounded by
// model.MaxCursors and evicted oldest-first; they are navigation state, so the
// change is NOT journaled (the journal records work, not where people are
// looking).
//
// A cursor move is idempotent, so a CAS race with a concurrent phase or entry
// write is retried: the whole document is CAS-written, and refusing a harmless
// cursor move because somebody else changed a phase would be noise. Three
// retries cover a burst; after that the conflict is returned honestly.
func (c *Client) SetCursor(ctx context.Context, name string, in CursorInput) (*model.Workset, error) {
	key := model.CursorKey(in.Author, in.Host)
	if key == "" {
		return nil, contractErrorf("set cursor: author is empty: a cursor names the reader it belongs to")
	}
	const retries = 3
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		ws, seqNo, primaryTerm, err := c.resolveWorkset(ctx, name)
		if err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		changed := false
		if in.Clear {
			if _, ok := ws.Cursors[key]; ok {
				delete(ws.Cursors, key)
				changed = true
			}
		} else {
			if ws.Cursors == nil {
				ws.Cursors = make(map[string]model.Cursor)
			}
			ws.Cursors[key] = model.Cursor{At: in.At, UpdatedAt: now}
			changed = true
		}
		if !changed {
			return ws, nil
		}
		model.EvictCursors(ws.Cursors, model.MaxCursors)
		ws.UpdatedAt = now
		if in.Author != "" {
			ws.UpdatedBy = in.Author
		}
		ws.Version++
		if err := c.putWorksetCAS(ctx, ws, seqNo, primaryTerm, "set workset cursor"); err != nil {
			if !errors.Is(err, ErrWorksetConflict) {
				return nil, err
			}
			lastErr = err
			continue
		}
		return ws, nil
	}
	return nil, lastErr
}

// FactStates reports the facts index state of each id: verified, obsolete, or
// missing. One terms query answers the whole batch; ids the store did not
// return are missing (the entry points at a fact that was never there or was
// hard-deleted).
//
// This reads with a plain search, not the realtime GET: facts are written by
// other commands, so a stale-by-one-refresh answer is the normal contract for a
// cross-document read here, and the batch is the point. (Refresh=wait_for is a
// WRITE concern, used by this file's mutations so a following search sees the
// workset they just wrote.)
func (c *Client) FactStates(ctx context.Context, factIDs []string) (map[string]FactState, error) {
	out := make(map[string]FactState, len(factIDs))
	if len(factIDs) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(factIDs))
	seen := make(map[string]struct{}, len(factIDs))
	for _, id := range factIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		out[id] = FactStateMissing
	}
	body := map[string]interface{}{
		"query": map[string]interface{}{
			"bool": map[string]interface{}{
				"filter": []map[string]interface{}{
					{"terms": map[string]interface{}{"fact_id": ids}},
				},
			},
		},
		"size":    len(ids),
		"_source": []string{"fact_id", "status"},
	}
	data, _ := json.Marshal(body)
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{c.cfg.IndexName},
		Body:    bytes.NewReader(data),
	})
	if err != nil {
		if isNotFound(err) {
			// No facts index: every requested id is missing, not an error.
			return out, nil
		}
		return nil, fmt.Errorf("fact states: %w", err)
	}
	for _, hit := range resp.Hits.Hits {
		var doc struct {
			FactID string `json:"fact_id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(hit.Source, &doc); err != nil || doc.FactID == "" {
			continue
		}
		if doc.Status == "obsolete" {
			out[doc.FactID] = FactStateObsolete
		} else {
			out[doc.FactID] = FactStateVerified
		}
	}
	return out, nil
}

// WorksetJournal reads the newest journal rows for a workset, resolved through
// its current name or any alias. Rows are ordered by the store-stamped
// ingest_at first (the writer clocks skew, and `at` is the client clock),
// falling back to the stored `at` for rows written before the ingest pipeline
// existed. The read tolerates deleted old period indices.
func (c *Client) WorksetJournal(ctx context.Context, name string, limit int) ([]model.WorksetJournalEntry, error) {
	ws, _, _, err := c.resolveWorkset(ctx, name)
	if err != nil {
		return nil, err
	}
	size := limit
	if size <= 0 || size > NavSizeCap {
		size = NavSizeCap
	}
	body := map[string]interface{}{
		"query": map[string]interface{}{
			"bool": map[string]interface{}{
				"must": []map[string]interface{}{
					{"term": map[string]interface{}{"workset_id": ws.ID}},
				},
			},
		},
		"sort": []map[string]interface{}{
			{"ingest_at": map[string]interface{}{"order": "desc", "missing": "_last"}},
			{"at": map[string]interface{}{"order": "desc"}},
		},
		"size": size,
	}
	data, _ := json.Marshal(body)
	yes := true
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{c.worksetsHistoryWildcard()},
		Body:    bytes.NewReader(data),
		Params: opensearchapi.SearchParams{
			IgnoreUnavailable: &yes,
			AllowNoIndices:    &yes,
		},
	})
	if err != nil {
		if isNotFound(err) {
			return []model.WorksetJournalEntry{}, nil
		}
		return nil, fmt.Errorf("workset journal read: %w", err)
	}
	entries := make([]model.WorksetJournalEntry, 0, len(resp.Hits.Hits))
	for _, hit := range resp.Hits.Hits {
		var e model.WorksetJournalEntry
		if err := json.Unmarshal(hit.Source, &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// resolveWorkset maps a name or alias to its document. The search answers
// which document; the realtime GET that follows returns the version pair a
// conditional write needs (and a search cannot provide).
func (c *Client) resolveWorkset(ctx context.Context, nameOrAlias string) (*model.Workset, int, int, error) {
	name, err := model.NormalizeName(nameOrAlias)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: %q: %v", ErrWorksetNotFound, nameOrAlias, err)
	}
	resp, err := c.worksetSearch(ctx, buildResolveWorksetBody(name))
	if err != nil {
		return nil, 0, 0, err
	}
	if len(resp.Hits.Hits) == 0 {
		return nil, 0, 0, fmt.Errorf("%w: %q", ErrWorksetNotFound, name)
	}
	id := resp.Hits.Hits[0].ID
	if id == "" {
		var doc struct {
			ID string `json:"id"`
		}
		if uerr := json.Unmarshal(resp.Hits.Hits[0].Source, &doc); uerr != nil || doc.ID == "" {
			return nil, 0, 0, fmt.Errorf("resolve workset %q: search hit carries no document id", name)
		}
		id = doc.ID
	}
	return c.getWorksetVersion(ctx, id)
}

// getWorksetVersion reads one workset by id with its version pair.
func (c *Client) getWorksetVersion(ctx context.Context, id string) (*model.Workset, int, int, error) {
	resp, err := c.api.Document.Get(ctx, opensearchapi.DocumentGetReq{
		Index:      c.worksetsIndex(),
		DocumentID: id,
	})
	if err != nil {
		if isNotFound(err) {
			return nil, 0, 0, fmt.Errorf("%w: %q", ErrWorksetNotFound, id)
		}
		return nil, 0, 0, fmt.Errorf("get workset: %w", err)
	}
	if !resp.Found {
		return nil, 0, 0, fmt.Errorf("%w: %q", ErrWorksetNotFound, id)
	}
	var ws model.Workset
	if err := json.Unmarshal(resp.Source, &ws); err != nil {
		return nil, 0, 0, fmt.Errorf("get workset: decode %q: %w", id, err)
	}
	if ws.ID == "" {
		ws.ID = id
	}
	return &ws, resp.SeqNo, resp.PrimaryTerm, nil
}

// worksetSearch runs a search over the worksets index. A 404 (the index does
// not exist yet) means "no worksets", not an error: readers may run before the
// first create.
func (c *Client) worksetSearch(ctx context.Context, body map[string]interface{}) (*opensearchapi.SearchResp, error) {
	data, _ := json.Marshal(body)
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{c.worksetsIndex()},
		Body:    bytes.NewReader(data),
	})
	if err != nil {
		if isNotFound(err) {
			return &opensearchapi.SearchResp{}, nil
		}
		return nil, fmt.Errorf("worksets read: %w", err)
	}
	return resp, nil
}

// putWorksetCAS writes a whole workset document back, conditioned on the
// version pair from the read that produced it. A mismatch is the honest
// "another writer won" answer, never a silent overwrite. A zero pair is a
// caller bug, not a licence to write unconditionally: without the condition a
// concurrent change would be clobbered, which is exactly what the CAS exists
// to prevent.
func (c *Client) putWorksetCAS(ctx context.Context, ws *model.Workset, seqNo, primaryTerm int, op string) error {
	if seqNo == 0 && primaryTerm == 0 {
		return fmt.Errorf("%s: internal: missing version pair for workset %q (seq_no=%d primary_term=%d): refusing an unconditional write",
			op, ws.Name, seqNo, primaryTerm)
	}
	body, _ := json.Marshal(ws)
	params := opensearchapi.IndexParams{
		Refresh:       "wait_for",
		IfSeqNo:       &seqNo,
		IfPrimaryTerm: &primaryTerm,
	}
	_, err := c.api.Index(ctx, opensearchapi.IndexReq{
		Index:      c.worksetsIndex(),
		DocumentID: ws.ID,
		Body:       bytes.NewReader(body),
		Params:     params,
	})
	if err != nil {
		if isVersionConflict(err) {
			return worksetConflict(op, ws.Name, err)
		}
		return wrapWriteError(op, err)
	}
	return nil
}

// worksetConflict names a CAS failure as what it is: a valid operation over a
// stale snapshot, with the re-read first because that is the whole remedy.
func worksetConflict(op, name string, err error) error {
	return fmt.Errorf("%s: workset %q changed under this write (a concurrent writer won the race): re-read and retry: %w; store: %w",
		op, name, ErrWorksetConflict, err)
}

// appendWorksetJournals appends a batch in order, reporting the first failure.
func (c *Client) appendWorksetJournals(ctx context.Context, entries []model.WorksetJournalEntry) error {
	for _, e := range entries {
		if err := c.appendWorksetJournal(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// appendWorksetJournal writes one immutable journal row (op_type=create, auto
// id) into the period index. A following search sees it (wait_for), matching
// the fact journal contract.
func (c *Client) appendWorksetJournal(ctx context.Context, e model.WorksetJournalEntry) error {
	at := e.At
	if at.IsZero() {
		at = time.Now().UTC()
		e.At = at
	}
	index := c.worksetsHistoryIndexAt(at)
	if err := c.ensureWorksetHistoryIndex(ctx, index); err != nil {
		return err
	}
	body, _ := json.Marshal(e)
	if _, err := c.api.Index(ctx, opensearchapi.IndexReq{
		Index: index,
		Body:  bytes.NewReader(body),
		Params: opensearchapi.IndexParams{
			Refresh: "wait_for",
			OpType:  "create",
		},
	}); err != nil {
		return wrapWriteError("workset journal append", err)
	}
	return nil
}

// buildResolveWorksetBody is the name-or-alias lookup: a bool should over the
// name and the aliases, one hit. The sort makes a create race deterministic:
// when two documents briefly answer one name, the reader always picks the same
// one (the older document, then the id) instead of whatever the shards happen
// to return first.
func buildResolveWorksetBody(name string) map[string]interface{} {
	return map[string]interface{}{
		"query": map[string]interface{}{
			"bool": map[string]interface{}{
				"should": []map[string]interface{}{
					{"term": map[string]interface{}{"name": name}},
					{"term": map[string]interface{}{"aliases": name}},
				},
				"minimum_should_match": 1,
			},
		},
		"sort": []map[string]interface{}{
			{"created_at": map[string]interface{}{"order": "asc", "missing": "_last"}},
			{"id": map[string]interface{}{"order": "asc"}},
		},
		"_source": []string{"id"},
		"size":    1,
	}
}

// buildListWorksetsBody lists worksets, filtered to a project when one is
// given, ordered by name.
func buildListWorksetsBody(project string) map[string]interface{} {
	body := map[string]interface{}{
		"sort":             []map[string]interface{}{{"name": map[string]interface{}{"order": "asc"}}},
		"size":             worksetListSizeCap,
		"track_total_hits": true,
	}
	if project != "" {
		body["query"] = map[string]interface{}{
			"bool": map[string]interface{}{
				"filter": []map[string]interface{}{
					{"term": map[string]interface{}{"project": project}},
				},
			},
		}
	}
	return body
}

// appendWorksetAlias returns aliases (deduped) with add appended when it is
// not present, and drop removed. Order of surviving aliases is preserved.
func appendWorksetAlias(aliases []string, add, drop string) []string {
	out := make([]string, 0, len(aliases)+1)
	seen := make(map[string]struct{}, len(aliases)+1)
	for _, a := range aliases {
		if a == drop {
			continue
		}
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	if add != drop && add != "" {
		if _, dup := seen[add]; !dup {
			out = append(out, add)
		}
	}
	return out
}
