package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NemoNobody/velruna/internal/model"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// PatchFields is the partial-update payload: an empty string means "not
// provided, leave the field alone" (the same presence convention as add).
// Erasing a field to empty is not a patch operation — corrections carry
// content, not deletions.
type PatchFields struct {
	Value       string   // new value: journaled, observed_at moves, summary resets with it
	Summary     string   // new table of contents; with a value change, "" clears the old one
	Notes       string   // replaces notes when non-empty (add's rule)
	NotesAppend string   // dated "YYYY-MM-DD: text" line appended atomically (painless)
	Source      string   // replaces source when non-empty
	Status      string   // verified | obsolete — a transition is journaled (audit symmetry)
	Reason      string   // why it became obsolete (folded into notes; guard-checked)
	TTLDays     int      // retunes the staleness window (bounded by the category cap); 0 = untouched
	Keywords    []string // replaces the tag set when non-empty (tags are journaled with each accepted value)
	// SupersededBy sets the successor link (see model.Fact.SupersededBy): one
	// link per fact, target verified, no cycles. A different target repoints
	// the link (journaled: a successor can be retired or merged again, so the
	// chain must stay repairable). The link is journaled with op=supersede;
	// the same value on an already-linked fact is a touch.
	SupersededBy string
	// Reverify re-confirms the current value: observed_at moves to now, the
	// journal gains an op=rediscovery entry, and the writer joins confirmed_by.
	// It cannot be combined with a value or status change — those are other
	// events (and both reset the confirmation set).
	Reverify bool
	// Redactions is the write-time secret-guard audit for the value this patch
	// stores (see model.Fact). A value change replaces the audit: the old rows
	// described the old value. Non-value patches leave the stored audit alone.
	Redactions []model.Redaction
}

// PatchResult reports what the patch did.
type PatchResult struct {
	// Changed: a new version was accepted (value change, status transition,
	// supersede link) or a re-confirmation was journaled (Reverified).
	Changed bool
	Status  string
	// Reverified: the value was re-confirmed in place (observed_at moved, the
	// journal gained op=rediscovery, the value itself did not change).
	Reverified bool
}

// patchScript applies only the provided params (null = untouched). The notes
// append runs inside the script: read-modify-write of notes from the Go side
// would race with a concurrent add/patch and silently drop one of the notes
// (the correction script's notes handling has the same property by design).
const patchScript = `
if (params.value != null) { ctx._source.value = params.value; }
if (params.redactions != null) { ctx._source.redactions = params.redactions; }
else if (params.redactions_clear) { ctx._source.remove("redactions"); }
if (params.summary != null) { ctx._source.summary = params.summary; }
if (params.notes != null) { ctx._source.notes = params.notes; }
if (params.notes_append != null) {
  String n = ctx._source.notes == null ? "" : ctx._source.notes.toString();
  ctx._source.notes = n.isEmpty() ? params.notes_append : n + params.nsep + params.notes_append;
}
if (params.reason_append != null) {
  String n = ctx._source.notes == null ? "" : ctx._source.notes.toString();
  ctx._source.notes = n.isEmpty() ? params.reason_append : n + params.nsep + params.reason_append;
}
if (params.source != null) { ctx._source.source = params.source; }
if (params.ttl_days != null) { ctx._source.ttl_days = params.ttl_days; }
if (params.keywords != null) { ctx._source.keywords = params.keywords; }
if (params.confirmed_by != null) { ctx._source.confirmed_by = params.confirmed_by; }
if (params.confirmer != null) {
  if (ctx._source.confirmed_by == null) { ctx._source.confirmed_by = new ArrayList(); }
  boolean found = false;
  for (int i = 0; i < ctx._source.confirmed_by.size(); i++) {
    def c = ctx._source.confirmed_by.get(i);
    if (c.author == params.confirmer.author && c.host == params.confirmer.host) {
      ctx._source.confirmed_by.set(i, params.confirmer);
      found = true;
    }
  }
  if (!found) { ctx._source.confirmed_by.add(params.confirmer); }
}
if (params.status != null) { ctx._source.status = params.status; }
if (params.superseded_by != null) { ctx._source.superseded_by = params.superseded_by; }
if (params.observed_at != null) { ctx._source.observed_at = params.observed_at; }
if (params.current_ref != null) { ctx._source.current_ref = params.current_ref; }
ctx._source.updated_at = params.updated_at;
ctx._source.updated_by = params.updated_by;
`

// ValidatePatchFields is the shape contract of a partial update, exported so
// the MCP edge can refuse a malformed patch before any store contact (the
// same "shape before availability" rule as list/search/keys: a bogus status
// must not pay for an index ensure, and on an unreachable store the shape
// error would drown in the "do not retry" advice).
func ValidatePatchFields(p PatchFields) error {
	if p.Value == "" && p.Summary == "" && p.Notes == "" && p.NotesAppend == "" &&
		p.Source == "" && p.Status == "" && p.Reason == "" && p.TTLDays == 0 && len(p.Keywords) == 0 &&
		p.SupersededBy == "" && !p.Reverify {
		return fmt.Errorf("patch: nothing to change (provide value, summary, notes, notes_append, source, status, reason, ttl_days, keywords, superseded_by, or reverify)")
	}
	if p.Reverify && (p.Value != "" || p.Summary != "" || p.Status != "") {
		return fmt.Errorf("patch: reverify cannot be combined with a value, summary or status change (a new value is verified by its writer; a status transition is its own event)")
	}
	if p.Reverify && p.SupersededBy != "" {
		return fmt.Errorf("patch: reverify cannot be combined with superseded_by (a re-confirmation and a successor link are different events)")
	}
	if p.TTLDays < 0 {
		return fmt.Errorf("patch: ttl_days must be positive, got %d", p.TTLDays)
	}
	if len(p.Keywords) > 0 {
		// Shape only: the length/count bounds are the shared normalizer's job
		// (the client runs it before the write, like add).
		for _, k := range p.Keywords {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("patch: keywords must not contain blank entries")
			}
		}
	}
	if p.Reason != "" && p.Status != "obsolete" {
		return fmt.Errorf("patch: reason is only accepted with status=obsolete")
	}
	if p.Status != "" && p.Status != "verified" && p.Status != "obsolete" {
		return fmt.Errorf("patch: status must be verified|obsolete, got %q", p.Status)
	}
	return nil
}

// maxSupersedeChain bounds the cycle walk behind superseded_by. A real chain
// of renamed records is a handful of links; a hundred means a loop the walk
// failed to recognize or a store corrupted by hand.
const maxSupersedeChain = 100

// checkSupersedeTarget validates a proposed superseded_by link and reports
// whether it points back in time (the target was created before the linking
// fact — allowed, but journaled with a marker).
//
// The invariants keep the link a chain a reader can follow to a trusted end:
// one outgoing link per fact (a repoint replaces it), no self-reference, no
// cycles, and every fact on the chain — the target and whatever the target
// itself superseded — verified. A reader sent through an obsolete hop would
// land on a record the store itself says is dead.
func (c *Client) checkSupersedeTarget(ctx context.Context, fact *model.Fact, targetKey string) (reversed bool, err error) {
	if targetKey == fact.Key {
		return false, contractErrorf("patch: fact %q cannot be superseded by itself", fact.Key)
	}
	seen := map[string]struct{}{fact.Key: {}}
	cur := targetKey
	var target *model.Fact
	for hops := 0; ; hops++ {
		if hops >= maxSupersedeChain {
			return false, contractErrorf("patch: superseded_by chain from %q is longer than %d facts (a loop or a hand-made chain)", targetKey, maxSupersedeChain)
		}
		if _, dup := seen[cur]; dup {
			return false, contractErrorf("patch: superseded_by would create a cycle (%q is already on the chain)", cur)
		}
		seen[cur] = struct{}{}
		f, gerr := c.Get(ctx, cur)
		if errors.Is(gerr, ErrNotFound) {
			return false, contractErrorf("patch: superseded_by target %q does not exist (a link must point at a stored fact)", cur)
		}
		if gerr != nil {
			return false, fmt.Errorf("patch: superseded_by: %w", gerr)
		}
		if f.Status != "verified" {
			return false, contractErrorf("patch: superseded_by target %q is %s: a reader sent there would land on an untrusted record", cur, f.Status)
		}
		if hops == 0 {
			target = f
		}
		if f.SupersededBy == "" {
			break
		}
		cur = f.SupersededBy
	}
	return target.CreatedAt.Before(fact.CreatedAt), nil
}

// Patch applies a partial update to an existing fact. It exists so an agent
// can extend qualifiers (append a dated note) or flip status without
// re-sending — and re-paying context for — the whole value. Journal
// semantics mirror add: the journal records accepted state, so a value
// change and a status transition both append entries; notes/source/summary
// moves are touches (no entry, observed_at stays).
//
// Summary invalidation: the summary describes the current value. On a value
// change the summary is replaced by the provided one ("" clears it — an old
// table of contents for a new value is a lie). A long new value requires a
// new summary (the add rule).
//
// Resurrection (obsolete → verified) sets observed_at=now: re-declaring a
// dead fact alive IS a fresh verification, and a fact that resurrects while
// staying stale would contradict its own staleness signal.
//
// SupersededBy is its own event: setting or repointing the link journals
// op=supersede (never op=write — the value did not move, and churn counters
// must stay truthful). A patch that also changes the value or retires the
// fact keeps that event's op and its entry states the link. Unlike notes or
// tags, the link does not touch observed_at: it states a relation between
// records, not a re-observation of the fact.
func (c *Client) Patch(ctx context.Context, key string, p PatchFields) (PatchResult, error) {
	if err := ValidatePatchFields(p); err != nil {
		return PatchResult{}, err
	}

	existing, seqNo, primaryTerm, err := c.getFactVersion(ctx, key)
	if errors.Is(err, ErrNotFound) {
		// The chain must survive: edges classify this as "no such fact"
		// (a clean answer), not as a store failure (Obsolete does the same).
		return PatchResult{}, fmt.Errorf("patch: %w: fact %q not found — use add to create it", ErrNotFound, key)
	}
	if err != nil {
		return PatchResult{}, fmt.Errorf("patch: %w", err)
	}

	// The cap belongs to the fact's stored category: a writer may shorten the
	// window, never lengthen it past what the category promises.
	if p.TTLDays > 0 {
		if err := model.ValidTTLOverride(existing.Category, p.TTLDays); err != nil {
			return PatchResult{}, contractErrorf("patch: %v", err)
		}
	}
	if len(p.Keywords) > 0 {
		kw, err := model.NormalizeKeywords(p.Keywords)
		if err != nil {
			return PatchResult{}, contractErrorf("patch: %v", err)
		}
		p.Keywords = kw
	}

	valueChanged := p.Value != "" && p.Value != existing.Value
	statusChanged := p.Status != "" && p.Status != existing.Status
	resurrect := statusChanged && p.Status == "verified"
	linkChanged := p.SupersededBy != "" && p.SupersededBy != existing.SupersededBy
	linkReversed := false
	if linkChanged {
		rev, lerr := c.checkSupersedeTarget(ctx, existing, p.SupersededBy)
		if lerr != nil {
			return PatchResult{}, lerr
		}
		linkReversed = rev
	}

	// notes for the doc: replace wins, then append, then the retire reason.
	newNotes := existing.Notes
	if p.Notes != "" {
		newNotes = p.Notes
	}
	appendLine := ""
	if p.NotesAppend != "" {
		appendLine = time.Now().UTC().Format("2006-01-02") + ": " + p.NotesAppend
		newNotes = appendNote(newNotes, appendLine)
	}
	journalNotes := newNotes
	retireReason := ""
	if p.Reason != "" && p.Status == "obsolete" && !statusChanged {
		// The reason belongs to the transition. Re-retiring an already
		// obsolete fact would append a duplicate notes line with no journal
		// entry (no state was accepted) — a silent audit lie.
		return PatchResult{}, contractErrorf("patch: fact %q is already obsolete (a reason belongs to the verified→obsolete transition)", key)
	}
	if statusChanged && p.Status == "obsolete" && p.Reason != "" {
		// notes is one line per qualifier: a reason with newlines would
		// embed a multi-line block and break the "one line, one event"
		// reading of the audit trail.
		retireReason = strings.Join(strings.Fields(p.Reason), " ")
		journalNotes = appendNote(journalNotes, retireReason)
	}

	if valueChanged {
		probe := model.NewFact(key, p.Value, existing.Category, existing.Project, existing.Source, newNotes)
		probe.Summary = p.Summary
		if err := c.checkSummaryRequired(probe); err != nil {
			return PatchResult{}, err
		}
		if err := c.checkSummaryCap(probe); err != nil {
			return PatchResult{}, err
		}
	} else if p.Summary != "" {
		probe := model.NewFact(key, existing.Value, existing.Category, existing.Project, existing.Source, existing.Notes)
		probe.Summary = p.Summary
		if err := c.checkSummaryCap(probe); err != nil {
			return PatchResult{}, err
		}
	}

	now := time.Now().UTC()
	currentRef := existing.CurrentRef
	// The audit rows this write lands. A value change REPLACES the set (a clean
	// value clears it — stale rows would describe text that no longer exists);
	// a qualifier-only write MERGES its rows in, so the audit of the value it
	// sits next to stays true.
	var qualifierRedactions []model.Redaction
	var replacedFields []string
	if !valueChanged {
		if p.Summary != "" {
			replacedFields = append(replacedFields, "summary")
		}
		if p.Notes != "" {
			replacedFields = append(replacedFields, "notes")
		}
		// Appended text adds rows instead of replacing them, but it is scanned
		// only when the patch actually appends it.
		scanFields := append([]string(nil), replacedFields...)
		if p.NotesAppend != "" {
			scanFields = append(scanFields, "notes_append")
		}
		if p.Reason != "" {
			scanFields = append(scanFields, "reason")
		}
		qualifierRedactions = redactionsForFields(p.Redactions, scanFields...)
	}
	effectiveRedactions := existing.Redactions
	if valueChanged {
		effectiveRedactions = p.Redactions
	} else if len(replacedFields) > 0 || len(qualifierRedactions) > 0 {
		effectiveRedactions = model.MergeRedactions(existing.Redactions, replacedFields, qualifierRedactions)
	}
	var entry *model.JournalEntry
	if valueChanged || statusChanged || p.Reverify || linkChanged {
		stated := *existing
		if valueChanged {
			stated.Value = p.Value
		}
		// The journal carries the notes as of journal time — a concurrent
		// atomic append that landed after this Get is not in the snapshot (the
		// doc stays right, the log lags by one append; the journal is a
		// history of accepted state, not a changelog of every byte).
		stated.Notes = journalNotes
		if p.Source != "" {
			stated.Source = p.Source
		}
		if len(p.Keywords) > 0 {
			// The snapshot states the tag set the doc will hold.
			stated.Keywords = p.Keywords
		}
		if linkChanged {
			// The entry states the successor link this write establishes.
			stated.SupersededBy = p.SupersededBy
		}
		if valueChanged || len(replacedFields) > 0 || len(qualifierRedactions) > 0 {
			// The entry states the text it records, audit included.
			stated.Redactions = effectiveRedactions
		}
		// Doc-first: the entry is built and its id preallocated here, the
		// entry itself is indexed only after the update is accepted.
		entry = c.newJournalEntryFor(ctx, &stated, existing.FactID, now)
		if statusChanged && p.Status == "obsolete" {
			// Only the transition is a retirement. A value correction on an
			// already-obsolete fact (status unchanged, the script leaves it
			// dead) is a write: marking it retire would date the burial wrong.
			entry.Op = model.OpRetire
		}
		if p.Reverify {
			// Same value, freshly observed: the reserved rediscovery marker is
			// exactly this event, and the entry names who re-confirmed it.
			entry.Op = model.OpRediscovery
		}
		if linkChanged && !valueChanged && !statusChanged && !p.Reverify {
			// Nothing but the link moved: the entry states the relation, not a
			// value observation (op=write would inflate the churn counters).
			entry.Op = model.OpSupersede
		}
		if linkChanged && linkReversed {
			entry.Marker = model.MarkerNewerWithOlder
		}
		currentRef = uuid.New().String()
	}

	newStatus := existing.Status
	if p.Status != "" {
		newStatus = p.Status
	}
	if p.Reverify && existing.Status == "obsolete" {
		return PatchResult{}, contractErrorf("patch: fact %q is obsolete — re-declaring it alive is a resurrection (status=verified or add), not a re-verification", key)
	}

	params := map[string]interface{}{
		"value":            nil,
		"redactions":       nil,
		"redactions_clear": false,
		"summary":          nil,
		"notes":            nil,
		"notes_append":     nil,
		"reason_append":    nil,
		"source":           nil,
		"ttl_days":         nil,
		"keywords":         nil,
		"confirmed_by":     nil,
		"confirmer":        nil,
		"status":           nil,
		"superseded_by":    nil,
		"observed_at":      nil,
		"current_ref":      nil,
		"updated_at":       now.Format(time.RFC3339),
		"updated_by":       c.resolveAuthor(ctx).Author,
		// Notes-line separator is data, not source: painless string literals
		// refuse "\n" at compile time (only \\ and \" escapes are valid).
		"nsep": "\n",
	}
	if valueChanged {
		params["value"] = p.Value
		// The summary moves with the value unconditionally: the provided one
		// (possibly "" = clear) replaces the old table of contents.
		params["summary"] = p.Summary
		params["observed_at"] = now.Format(time.RFC3339)
		params["current_ref"] = currentRef
		// A new value is verified only by its writer: the set resets.
		params["confirmed_by"] = []model.Confirmer{c.resolveAuthor(ctx).confirmer(now)}
	} else if statusChanged || linkChanged {
		params["current_ref"] = currentRef
	}
	// The guard audit describes the stored text: a value change sets or clears
	// the rows; a qualifier write drops the rows of the fields it overwrote and
	// merges the new ones in.
	if valueChanged || len(replacedFields) > 0 || len(qualifierRedactions) > 0 {
		if len(effectiveRedactions) > 0 {
			params["redactions"] = effectiveRedactions
		} else {
			params["redactions_clear"] = true
		}
	}
	if resurrect {
		params["observed_at"] = now.Format(time.RFC3339)
		// A resurrection is a fresh verification by the resurrector: the prior
		// confirmations belong to a fact that was declared dead.
		params["confirmed_by"] = []model.Confirmer{c.resolveAuthor(ctx).confirmer(now)}
	}
	if p.Reverify {
		// observed_at moves (that is the point), and the confirmer is appended
		// INSIDE the script: a Go-side read-modify-write of the list would drop
		// a concurrent confirmation.
		params["observed_at"] = now.Format(time.RFC3339)
		params["current_ref"] = currentRef
		params["confirmer"] = c.resolveAuthor(ctx).confirmer(now)
	}
	if p.Summary != "" && !valueChanged {
		params["summary"] = p.Summary
	}
	if p.Notes != "" {
		params["notes"] = newNotes
	} else if appendLine != "" {
		params["notes_append"] = appendLine
	}
	if retireReason != "" {
		// The reason appends INSIDE the script: folding it into a Go-side
		// notes replace would be a read-modify-write of the stale Get, which
		// silently drops a concurrent atomic notes_append — the exact race
		// the scripted patch exists to prevent.
		params["reason_append"] = retireReason
	}
	if p.Source != "" {
		params["source"] = p.Source
	}
	if p.TTLDays > 0 {
		params["ttl_days"] = p.TTLDays
	}
	if len(p.Keywords) > 0 {
		params["keywords"] = p.Keywords
	}
	if p.Status != "" {
		params["status"] = p.Status
	}
	if linkChanged {
		params["superseded_by"] = p.SupersededBy
	}

	body, _ := json.Marshal(map[string]interface{}{
		"script": map[string]interface{}{
			"lang":   "painless",
			"source": patchScript,
			"params": params,
		},
	})
	_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: key,
		Body:       strings.NewReader(string(body)),
		Params:     patchUpdateParams(entry, valueChanged, seqNo, primaryTerm),
	})
	if err != nil {
		if isNotFound(err) {
			return PatchResult{}, fmt.Errorf("patch %w: %q", ErrNotFound, key)
		}
		if isVersionConflict(err) {
			return PatchResult{}, conflictError("patch fact", key, err)
		}
		return PatchResult{}, wrapWriteError("patch fact", err)
	}
	if entry != nil {
		if _, jErr := c.appendJournalID(ctx, entry, currentRef, "", false); jErr != nil {
			return PatchResult{Changed: valueChanged || statusChanged || p.Reverify || linkChanged, Status: newStatus, Reverified: p.Reverify}, fmt.Errorf("patch: accepted, the journal entry lagged: %w", jErr)
		}
	}
	return PatchResult{Changed: valueChanged || statusChanged || p.Reverify || linkChanged, Status: newStatus, Reverified: p.Reverify}, nil
}

// patchUpdateParams conditions a patch on the document version pair when it
// journals a STATUS transition computed from the snapshot read: a
// concurrent value change that landed after the read would leave current_ref
// pointing at an entry stating the value the doc no longer holds. A patch
// that also changes the value computes current_ref from the new value, so
// the invariant holds under any write order and the update stays
// unconditional (last-write-wins, matching add's correction).
func patchUpdateParams(entry *model.JournalEntry, valueChanged bool, seqNo, primaryTerm int) opensearchapi.UpdateParams {
	p := opensearchapi.UpdateParams{Refresh: "wait_for"}
	if entry != nil && !valueChanged {
		p.IfSeqNo, p.IfPrimaryTerm = &seqNo, &primaryTerm
	}
	return p
}

// pruneHistoryScript keeps only the newest params.keep history entries in
// the legacy in-doc array. It serves facts that predate the journal until
// 'velruna migrate-history' moves their array; journal pruning is
// PruneJournal (the journal index is the history for journal-era facts).
const pruneHistoryScript = `
def h = ctx._source.history;
if (h != null && h.size() > params.keep) {
  ctx._source.history = new ArrayList(h.subList(h.size() - params.keep, h.size()));
}
ctx._source.updated_at = params.updated_at;
`

// JournalEntriesForPrune exposes the journal entry count for a key (the
// prune planning path shows the journal, not the legacy array, once a key
// has entries there).
func (c *Client) JournalCount(ctx context.Context, key string) (int, error) {
	_, meta, err := c.JournalEntries(ctx, JournalQuery{Key: key, Limit: 1})
	return meta.Total, err
}

// Prune drops history beyond the newest keep. It is journal-first: a key
// with journal entries is pruned in the journal (the legacy in-doc array is
// ignored for such keys — migrate-history removes it); keys that predate the
// journal fall back to the legacy array so live stores prune cleanly between
// release and migration. It returns the number of entries removed. Like every
// destructive call it must sit behind an explicit user confirmation — the
// barrier lives in the CLI (--yes) and the MCP tool (confirm=true).
func (c *Client) Prune(ctx context.Context, key string, keep int) (int, error) {
	if keep < 0 {
		return 0, fmt.Errorf("prune: keep must be >= 0, got %d", keep)
	}
	total, err := c.JournalCount(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("prune fact: %w", err)
	}
	if total > 0 {
		return c.PruneJournal(ctx, key, keep)
	}

	existing, err := c.Get(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("prune fact: %w", err)
	}
	removed := len(existing.History) - keep
	if removed <= 0 {
		return 0, nil // nothing beyond the limit: no write, no updated_at move
	}
	body, _ := json.Marshal(map[string]interface{}{
		"script": map[string]interface{}{
			"lang":   "painless",
			"source": pruneHistoryScript,
			"params": map[string]interface{}{
				"keep":       keep,
				"updated_at": time.Now().UTC().Format(time.RFC3339),
			},
		},
	})
	_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: key,
		Body:       bytes.NewReader(body),
		Params:     opensearchapi.UpdateParams{Refresh: "wait_for"},
	})
	if err != nil {
		if isNotFound(err) {
			return 0, fmt.Errorf("prune %w: %q", ErrNotFound, key)
		}
		return 0, wrapWriteError("prune fact", err)
	}
	// The script trims the REAL array; the pre-read snapshot can lag a
	// concurrent write, so the reported count is recomputed from the stored
	// doc ("dropped N" must not be a snapshot guess — legacy path only,
	// journal-backed keys report exactly).
	after, gErr := c.Get(ctx, key)
	if gErr != nil {
		return removed, nil // write succeeded; the recount is best effort here
	}
	if removed = len(existing.History) - len(after.History); removed < 0 {
		removed = 0
	}
	return removed, nil
}
