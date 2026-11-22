package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NemoNobody/velruna/internal/model"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// addCorrectionScript records a value change on the main fact doc: the new
// value replaces the old, and current_ref points at the journal entry that
// was appended for it (the journal append happens BEFORE this script: the
// journal is the log, the main doc is the projection, and an orphan journal
// entry from a failed main write is a harmless truth, never a lost value).
// updated_by moves to the last toucher; the birth author labels are
// write-once and deliberately absent from this script.
//
// The in-doc history array is no longer maintained: the journal index is the
// history. Facts created before the journal keep their array until
// 'velruna migrate-history' moves it.
const addCorrectionScript = `
ctx._source.value = params.value;
// The guard audit describes the value: a clean correction clears the rows of
// the value it replaced (a stale audit would claim a loss that did not happen).
if (params.redactions != null) { ctx._source.redactions = params.redactions; }
else { ctx._source.remove("redactions"); }
ctx._source.category = params.category;
if (params.ttl_days != null) { ctx._source.ttl_days = params.ttl_days; }
if (params.keywords != null) { ctx._source.keywords = params.keywords; }
if (params.confirmed_by != null) { ctx._source.confirmed_by = params.confirmed_by; }
ctx._source.observed_at = params.observed_at;
if (params.source != null && params.source != "") { ctx._source.source = params.source; }
if (params.notes != null && params.notes != "") { ctx._source.notes = params.notes; }
ctx._source.status = "verified";
ctx._source.summary = params.summary;
ctx._source.current_ref = params.current_ref;
ctx._source.updated_by = params.updated_by;
ctx._source.updated_at = params.updated_at;
`

// AddResult reports what an accepted Add did — the answer to "what happened
// to my write", which {created} alone cannot tell (a replacement and a
// same-value touch both report created=false, and on a critical fact the
// difference is the whole point).
type AddResult struct {
	// Created: the fact did not exist and this write gave it birth.
	Created bool
	// Changed: a new version was accepted (birth, value correction, status
	// transition). false means a same-value touch: notes/source moved,
	// observed_at and the journal untouched.
	Changed bool
	// PreviousSummary is the summary the correction replaced ("" when the
	// prior fact had none) — names the superseded version cheaply.
	PreviousSummary string
	// Resurrected: the fact was obsolete and this write re-declared it
	// alive (status back to verified, observed_at moved, the event is
	// journaled). Without it a same-value resurrect reads as a value
	// correction (changed=true), and a correction over an obsolete fact
	// hides the status transition entirely.
	Resurrected bool
}

// redactionsForFields keeps the audit rows of the fields this write stores. A
// field the write drops (a summary on a same-value touch) must not claim a loss
// that never landed, and a field the caller did not touch must not inherit
// another field's rows.
func redactionsForFields(rows []model.Redaction, fields ...string) []model.Redaction {
	if len(rows) == 0 || len(fields) == 0 {
		return nil
	}
	keep := make(map[string]bool, len(fields))
	for _, f := range fields {
		keep[f] = true
	}
	out := make([]model.Redaction, 0, len(rows))
	for _, r := range rows {
		if keep[r.Field] {
			out = append(out, r)
		}
	}
	return out
}

// Add stores a fact under its key. Three cases: the key is new (created, then
// journaled and linked); the value is unchanged (only notes/source/status
// move — observed_at must not, the observation is old and only the qualifier
// is new, and the journal is untouched; the exception is a dead fact: an
// unchanged re-add of an obsolete fact is a fresh verification, journaled
// like the Patch resurrect, and observed_at moves); the value changed (a
// journal entry is appended for the new value, then the main doc is
// corrected to point at it — this also resurrects an obsolete fact).
//
// The journal append precedes the main write: the journal is the accepted
// log, the main doc is the projection. A failed main write leaves an orphan
// entry (truth: the value was accepted, the projection lags and is fixed by
// the next write); the reverse order could lose values from the log.
//
// Notes are only replaced by a non-empty new note: the CLI flag and the MCP
// field default to empty, and a correction without --notes must not silently
// erase the qualifiers (they are not kept in history, so erasure is irreversible).
//
// The category moves with the correction on both update paths: the write edge
// validates and canonicalizes it, and a fact stored with the wrong category
// must not keep the wrong staleness TTL forever (category drives stale).
//
// The summary moves with the value unconditionally: the summary describes the
// current value, and a correction to a short value (no summary required)
// erasing the old summary is the honest state — an old table of contents for
// a new value is a lie.
//
// It reports the accepted outcome (see AddResult). Writes wait for a refresh,
// so the fact is immediately visible to subsequent Search/List calls.
func (c *Client) Add(ctx context.Context, f *model.Fact) (AddResult, error) {
	if err := c.checkSummaryCap(f); err != nil {
		return AddResult{}, err
	}
	// Shape before store: keyword normalization is shared with the MCP edge,
	// and storing the raw form would split one tag across two spellings.
	if kw, err := model.NormalizeKeywords(f.Keywords); err != nil {
		return AddResult{}, contractErrorf("add fact: %v", err)
	} else {
		f.Keywords = kw
	}
	// Shape before store (the summary-cap rule): a writer-chosen staleness
	// window must fit the category cap even before we know whether this is a
	// create, a touch, or a correction.
	if f.TTLDays != 0 {
		if err := model.ValidTTLOverride(f.Category, f.TTLDays); err != nil {
			return AddResult{}, contractErrorf("add fact: %v", err)
		}
	}
	existing, seqNo, primaryTerm, err := c.getFactVersion(ctx, f.Key)
	if errors.Is(err, ErrNotFound) {
		// Summary bounds are enforced on accepted values only (create and
		// correction): a same-value notes touch must not fail over a summary
		// the pre-summary fact never had.
		if err := c.checkSummaryRequired(f); err != nil {
			return AddResult{}, err
		}
		// Birth labels are stamped once (write-once): who/what/where created
		// the fact. Later writes move only updated_by. Adding a fact IS the
		// first verification of its value, so the creator opens confirmed_by.
		author := c.resolveAuthor(ctx)
		author.stampFact(f)
		f.UpdatedBy = f.Author
		f.ConfirmedBy = []model.Confirmer{author.confirmer(f.ObservedAt)}
		// Distinct names: the create pair belongs to the link only. The outer
		// seqNo/primaryTerm must keep receiving the create-loser re-read below —
		// the touch path conditions on those.
		created, birthSeq, birthTerm, err := c.createFact(ctx, f)
		if err != nil {
			return AddResult{}, err
		}
		if created {
			// Journal the birth value under a deterministic id (the same
			// contract as corrections and the migration), then link the fact
			// to it. A journal failure leaves the fact in the pre-journal
			// shape (migrate-history catches it), not a broken fact. A link
			// failure (a blackout between the append and the link) is also
			// repairable: a same-value retry is a touch, not a relink, but
			// migrate-history re-appends the current entry under the SAME
			// deterministic id and links it — the collision is success, so
			// the repair needs no old auto id. (The id collision assumes
			// notes/source unchanged since birth; a touch that moved them
			// hashes a new id and appends the newer state as its own entry —
			// a second snapshot, not a duplicate of the birth.)
			ts := f.ObservedAt
			if ts.IsZero() {
				ts = f.UpdatedAt
			}
			ref := journalEntryID(f.Key, model.OpWrite, f.Value, f.Notes, f.Source, ts)
			if _, jErr := c.appendJournalID(ctx, c.newJournalEntryFor(ctx, f, f.FactID, ts), ref, "", true); jErr != nil {
				return AddResult{Created: true, Changed: true}, fmt.Errorf("add: the fact doc was stored, the journal entry lagged: %w", jErr)
			}
			if lErr := c.linkCurrentRef(ctx, f.Key, ref, birthSeq, birthTerm); lErr != nil {
				return AddResult{Created: true, Changed: true}, fmt.Errorf("add: the fact and its birth journal entry were stored, the link to the journal failed (migrate-history repairs the link): %w", lErr)
			}
			return AddResult{Created: true, Changed: true}, nil
		}
		// op_type=create lost the race to a concurrent Add of the same key
		// (a plain index would last-write-win and drop the winner's value
		// with no journal entry): re-read and apply as a correction below.
		existing, seqNo, primaryTerm, err = c.getFactVersion(ctx, f.Key)
		if err != nil {
			return AddResult{}, fmt.Errorf("add fact: %w", err)
		}
	} else if err != nil {
		return AddResult{}, fmt.Errorf("add fact: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// A write may move the category; the ttl_days that survives this write —
	// including a stored override the request does not mention — must fit the
	// incoming category's cap, or the fact would outlive what the category
	// promises auditors.
	effTTL := f.TTLDays
	if effTTL == 0 {
		effTTL = existing.TTLDays
	}
	if effTTL > 0 {
		if err := model.ValidTTLOverride(f.Category, effTTL); err != nil {
			if f.TTLDays == 0 {
				return AddResult{}, contractErrorf("add fact: the stored ttl_days=%d exceeds the max for category %q (%dd): re-add with a smaller ttl_days to move the category", effTTL, f.Category, model.TTLDays(f.Category))
			}
			return AddResult{}, contractErrorf("add fact: %v", err)
		}
	}

	if existing.Value == f.Value {
		// Re-declaring a dead fact alive IS a fresh verification (the Patch
		// resurrect rule, kept symmetric): flipping obsolete→verified on a
		// same-value add without a journal entry left the audit asymmetry
		// half-closed — "when was it last seen alive" would go unrecorded.
		// Only the explicit dead state resurrects (a status-less legacy doc
		// is not "dead": it stays a plain touch).
		resurrect := existing.Status == "obsolete"
		// The audit follows the fields this write actually stores. A same-value
		// touch never writes the summary (it is dropped, see the write edge), so
		// only notes can contribute rows; and because the touch OVERWRITES notes,
		// the stored notes row is dropped even when the new note is clean (a stale
		// row would claim a loss that no longer exists). Rows of the value survive.
		var touchFields []string
		if f.Notes != "" {
			touchFields = append(touchFields, "notes")
		}
		touchRows := redactionsForFields(f.Redactions, touchFields...)
		mergedRows := model.MergeRedactions(existing.Redactions, touchFields, touchRows)
		var entry *model.JournalEntry
		var ref string
		doc := map[string]interface{}{
			"category":   f.Category,
			"status":     "verified",
			"updated_by": c.resolveAuthor(ctx).Author,
			"updated_at": now,
		}
		if f.TTLDays != 0 {
			// A touch may retune the staleness window (qualifier-like, not
			// journaled); omitted means "keep the stored one".
			doc["ttl_days"] = f.TTLDays
		}
		if len(f.Keywords) > 0 {
			// Same rule as ttl_days: keywords are a stored qualifier, not a
			// journaled observation; an empty list means "leave them alone".
			doc["keywords"] = f.Keywords
		}
		if resurrect {
			observed := time.Now().UTC()
			stated := *existing
			// The touch doc moves category unconditionally (the write edge
			// canonicalizes it); the journaled resurrect must state the same.
			stated.Category = f.Category
			if f.Notes != "" {
				stated.Notes = f.Notes
			}
			if len(touchFields) > 0 || len(touchRows) > 0 {
				stated.Redactions = mergedRows
			}
			if len(f.Keywords) > 0 {
				stated.Keywords = f.Keywords
			}
			if f.Source != "" {
				stated.Source = f.Source
			}
			stated.Status = "verified"
			stated.ObservedAt = observed
			// A resurrection is a fresh verification by the writer: the prior
			// confirmations belong to a fact that was declared dead.
			stated.ConfirmedBy = []model.Confirmer{c.resolveAuthor(ctx).confirmer(observed)}
			doc["confirmed_by"] = stated.ConfirmedBy
			// Doc-first: the entry is built and its id preallocated here, the
			// entry itself is indexed only after the update is accepted.
			entry = c.newJournalEntryFor(ctx, &stated, existing.FactID, observed)
			ref = uuid.New().String()
			doc["observed_at"] = observed.Format(time.RFC3339)
			doc["current_ref"] = ref
		}
		// source follows the notes rule: empty means "not given", never
		// "erase" (Patch's contract). The qualifier is not kept in history on
		// the touch path, so an unconditional write of an empty source would
		// be an irreversible silent wipe.
		if f.Source != "" {
			doc["source"] = f.Source
		}
		if f.Notes != "" {
			doc["notes"] = f.Notes
		}
		if len(touchFields) > 0 || len(touchRows) > 0 {
			// The note was rewritten (or a span in it was cut): the stored notes
			// row must follow, and rows of the value it sits next to stay.
			doc["redactions"] = mergedRows
		}
		body, _ := json.Marshal(map[string]interface{}{
			"doc": doc,
		})
		// CAS: the touch moves status/updated_by/updated_at (and on a
		// resurrect current_ref/observed_at) from the snapshot read above.
		// A concurrent correction or retirement that landed since that read
		// makes this write wrong: the conflict is honest ("re-read and
		// retry") instead of silently clobbering the other writer's state.
		_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
			Index:      c.cfg.IndexName,
			DocumentID: f.Key,
			Body:       strings.NewReader(string(body)),
			Params: opensearchapi.UpdateParams{
				Refresh:       "wait_for",
				IfSeqNo:       &seqNo,
				IfPrimaryTerm: &primaryTerm,
			},
		})
		if err != nil {
			if isVersionConflict(err) {
				return AddResult{}, conflictError("add fact", f.Key, err)
			}
			return AddResult{}, wrapWriteError("add fact", err)
		}
		if entry != nil {
			if _, jErr := c.appendJournalID(ctx, entry, ref, "", false); jErr != nil {
				return AddResult{Changed: true}, fmt.Errorf("add: resurrection accepted, the journal entry lagged: %w", jErr)
			}
		}
		// Same value: notes/source moved, observed_at must not (the
		// observation is old), the journal is untouched — the resurrection
		// exception above is the one same-value write that IS a new
		// observation, and it reports Changed=true and Resurrected=true.
		return AddResult{Changed: resurrect, Resurrected: resurrect}, nil
	}

	if err := c.checkSummaryRequired(f); err != nil {
		return AddResult{}, err
	}

	// The journal entry carries the doc's fact_id (not the caller's fresh
	// uuid): entries group by the identity that survives, and the doc's
	// fact_id is that identity. The qualifiers are MERGED onto the incoming
	// value before journaling: the script keeps the stored source/notes when
	// the incoming ones are empty, and the journal entry must state the
	// accepted state exactly as the doc will read after the write — an entry
	// with an empty source next to a doc that kept "doc:README" is a small
	// audit fiction. (The entry body is value/notes/source only — see
	// journalAppend — so the merge stays three fields.)
	stated := *f
	// project is write-once (the correction script never moves it): the
	// journal entry of a correction states the doc's project, not the
	// request's — the CLI/MCP default "global" would otherwise journal a
	// project the fact never had.
	stated.Project = existing.Project
	if stated.Source == "" {
		stated.Source = existing.Source
	}
	if stated.Notes == "" {
		stated.Notes = existing.Notes
	}
	if len(stated.Keywords) == 0 {
		// Tags follow the notes/source rule: omitted means "keep the stored
		// set", and the journaled snapshot must state what the doc will hold.
		stated.Keywords = existing.Keywords
	}
	// The correction script leaves the successor link alone; the entry states
	// the doc's link, not the request's empty field (a correction must not
	// read as "this fact lost its successor").
	stated.SupersededBy = existing.SupersededBy
	entry := c.newJournalEntryFor(ctx, &stated, existing.FactID, f.ObservedAt)
	ref := uuid.New().String()

	// ttl_days follows the patch convention: nil keeps the stored window, a
	// positive value replaces it (the shape was already checked above).
	var ttlParam interface{}
	if f.TTLDays != 0 {
		ttlParam = f.TTLDays
	}
	var kwParam interface{}
	if len(f.Keywords) > 0 {
		kwParam = f.Keywords
	}
	// nil when the guard cut nothing: the script removes the field, so a clean
	// value never inherits the audit of the value it replaced.
	var redactionsParam interface{}
	if len(f.Redactions) > 0 {
		redactionsParam = f.Redactions
	}

	body, _ := json.Marshal(map[string]interface{}{
		"script": map[string]interface{}{
			"lang":   "painless",
			"source": addCorrectionScript,
			"params": map[string]interface{}{
				"value":    f.Value,
				"category": f.Category,
				"ttl_days": ttlParam,
				"keywords": kwParam,
				// Redactions of the value being stored: nil clears the audit of the
				// value it replaces.
				"redactions": redactionsParam,
				// A new value is verified only by its writer: the set resets.
				"confirmed_by": []model.Confirmer{c.resolveAuthor(ctx).confirmer(f.ObservedAt)},
				"observed_at":  f.ObservedAt.UTC().Format(time.RFC3339),
				"source":       f.Source,
				"notes":        f.Notes,
				"summary":      f.Summary,
				"updated_at":   now,
				"current_ref":  ref,
				"updated_by":   c.resolveAuthor(ctx).Author,
			},
		},
	})
	_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: f.Key,
		Body:       strings.NewReader(string(body)),
		Params:     opensearchapi.UpdateParams{Refresh: "wait_for"},
	})
	if err != nil {
		// Nothing was accepted: no journal entry, Changed stays false (the
		// doc-first order is what makes this answer truthful).
		return AddResult{}, wrapWriteError("add fact", err)
	}
	if _, jErr := c.appendJournalID(ctx, entry, ref, "", false); jErr != nil {
		return AddResult{Changed: true, PreviousSummary: existing.Summary}, fmt.Errorf("add: correction accepted, the journal entry lagged: %w", jErr)
	}
	// The value was accepted as a new version (journal entry + correction):
	// changed=true, and the summary the correction is replacing is reported
	// so the caller can name the superseded version without reading the
	// journal. A correction over an obsolete fact also resurrects it (the
	// script writes status=verified): the transition is named explicitly.
	return AddResult{Changed: true, PreviousSummary: existing.Summary, Resurrected: existing.Status == "obsolete"}, nil
}

// newJournalEntryFor builds — without writing — the entry for a state the
// caller is about to accept. The scripted write paths preallocate the entry
// id, carry it into the update as current_ref, and index the entry under
// that id only after the update is accepted: a refused update (the cluster
// vetoes the script, a shard refuses the write) must leave no journal
// trace — an entry for a state that never landed is an audit lie. The
// residual the other way (update accepted, entry write lost) leaves the doc
// right and the log lagging one entry: current_ref dangles, the fact keeps
// working.
func (c *Client) newJournalEntryFor(ctx context.Context, f *model.Fact, factID string, ts time.Time) *model.JournalEntry {
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	e := model.NewJournalEntry(f.Key, factID, f.Project, f.Category, model.OpWrite, f.Value, f.Notes, f.Source, ts)
	e.Keywords = f.Keywords
	e.Redactions = f.Redactions
	e.SupersededBy = f.SupersededBy
	c.resolveAuthor(ctx).applyTo(e)
	return e
}

// linkCurrentRef points a fact at its journal entry. Used on the create
// path (the fact doc is written before the entry id is committed); the
// correction script sets current_ref inside the same write.
// The link is a snapshot-derived write: it is conditioned on the version
// pair the create itself reported — "nothing has touched this doc since I
// wrote it". A concurrent correction landing after the create bumps the
// version, the link conflicts honestly, and the winner's link survives: a
// correction or resurrect sets its own current_ref (pointing at the entry
// that carries the live value) inside its script. A plain same-value touch
// bumps the version without setting a ref — the conflict then leaves the
// fact created-but-unlinked, which migrate-history genuinely repairs
// (deterministic-id re-append, collision-as-success). An unconditional
// link here would instead overwrite the winner's ref with the birth one,
// leaving current_ref pointing at an entry that does not carry the live
// value — a later prune --keep could then delete the entry that does.
// Re-reading the pair at link time would NOT close that window (the
// correction would be inside the snapshot); the pair must come from the
// create response.
func (c *Client) linkCurrentRef(ctx context.Context, key, ref string, seqNo, primaryTerm int) error {
	body, _ := json.Marshal(map[string]interface{}{
		"doc": map[string]interface{}{"current_ref": ref},
	})
	if _, err := c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: key,
		Body:       strings.NewReader(string(body)),
		Params: opensearchapi.UpdateParams{
			Refresh:       "wait_for",
			IfSeqNo:       &seqNo,
			IfPrimaryTerm: &primaryTerm,
		},
	}); err != nil {
		if isVersionConflict(err) {
			return conflictError("link current_ref", key, err)
		}
		return wrapWriteError("link current_ref", err)
	}
	return nil
}

// checkSummaryCap rejects an over-long summary in any write: a provided
// summary that is longer than the cap stops being a summary. The cap lives
// on the shape, not on the accepted value, so it runs before the store
// read.
func (c *Client) checkSummaryCap(f *model.Fact) error {
	if c.cfg.SummaryMaxLen > 0 {
		if n := utf8.RuneCountInString(f.Summary); n > c.cfg.SummaryMaxLen {
			return contractErrorf("add fact: summary is %d characters, over summary_max_len=%d: keep it a table of contents, not a second value", n, c.cfg.SummaryMaxLen)
		}
	}
	return nil
}

// checkSummaryRequired enforces the table-of-contents contract on an
// accepted value (create and correction paths): a value over summary_min_len
// must ship a summary, so store listings stay skimmable. Lengths count
// runes: the store is text-first and byte counts would punish non-Latin
// scripts.
func (c *Client) checkSummaryRequired(f *model.Fact) error {
	if c.cfg.SummaryMinLen > 0 && f.Summary == "" {
		if n := utf8.RuneCountInString(f.Value); n > c.cfg.SummaryMinLen {
			return contractErrorf("add fact: value is %d characters, over summary_min_len=%d: a summary is required (CLI --summary, MCP summary) — it keeps store listings skimmable", n, c.cfg.SummaryMinLen)
		}
	}
	return nil
}

// createFact stores a brand-new fact with op_type=create: OpenSearch refuses
// the write when the document exists, so two concurrent creators of the same
// key cannot silently overwrite each other (last-write-wins would lose the
// first value with no history entry). It reports created=true on success
// (with the document version pair the create itself wrote — the birth link
// is conditioned on exactly that state) and created=false on a version
// conflict, leaving the caller to re-read the winner and apply the fact as a
// correction.
func (c *Client) createFact(ctx context.Context, f *model.Fact) (bool, int, int, error) {
	body, _ := json.Marshal(f)
	resp, err := c.api.Index(ctx, opensearchapi.IndexReq{
		Index:      c.cfg.IndexName,
		DocumentID: f.Key,
		Body:       bytes.NewReader(body),
		Params:     opensearchapi.IndexParams{Refresh: "wait_for", OpType: "create"},
	})
	if err == nil {
		return true, resp.SeqNo, resp.PrimaryTerm, nil
	}
	if isVersionConflict(err) {
		return false, 0, 0, nil
	}
	return false, 0, 0, wrapWriteError("add fact", err)
}

// Get gets a fact by key. It uses the realtime document GET API, so it sees
// the fact immediately after Add (unlike a search, which waits for a refresh).
func (c *Client) Get(ctx context.Context, key string) (*model.Fact, error) {
	f, _, _, err := c.getFactVersion(ctx, key)
	return f, err
}

// getFactVersion reads a fact together with OpenSearch's document version
// pair (seq_no/primary_term). Snapshot-derived writes — touches,
// resurrects, retirements, status transitions — carry it into the update
// as if_seq_no/if_primary_term: without the condition, a concurrent write
// landing between our read and the update lets current_ref be rewritten to
// a journal entry carrying the OLD value, breaking the invariant that
// current_ref journals the doc's current value (and lets a plain touch
// silently un-verify a fact another writer just retired).
func (c *Client) getFactVersion(ctx context.Context, key string) (*model.Fact, int, int, error) {
	resp, err := c.api.Document.Get(ctx, opensearchapi.DocumentGetReq{
		Index:      c.cfg.IndexName,
		DocumentID: key,
	})
	if err != nil {
		if isNotFound(err) {
			return nil, 0, 0, fmt.Errorf("%w: %q", ErrNotFound, key)
		}
		return nil, 0, 0, fmt.Errorf("get fact: %w", err)
	}
	if !resp.Found {
		return nil, 0, 0, fmt.Errorf("%w: %q", ErrNotFound, key)
	}
	var f model.Fact
	if err := json.Unmarshal(resp.Source, &f); err != nil {
		return nil, 0, 0, fmt.Errorf("get fact: decode %q: %w", key, err)
	}
	return &f, resp.SeqNo, resp.PrimaryTerm, nil
}

// conflictError names a failed CAS condition as what it is: a valid
// operation over a stale snapshot. The message orders the remedy (re-read,
// retry) first because that is the whole answer; the store's own reason is
// chained too (two %w keeps errors.Is(err, ErrConflict) true while a
// non-standard 409 payload still reaches logs instead of being discarded).
func conflictError(op, key string, err error) error {
	return fmt.Errorf("%s: fact %q changed under this write (a concurrent writer won the race): re-read and retry: %w; store: %w", op, key, ErrConflict, err)
}

// NormalizePrefix validates a key prefix and strips the optional trailing
// '*'. It is exported so CLI and MCP can reject bad prefixes before touching
// the store. It is idempotent: the result is trimmed again after the strip
// ("k *" normalizes to "k", not "k "), so what a guard-checked edge accepts
// is exactly the term the store sees on the wire.
func NormalizePrefix(prefix string) (string, error) {
	p := strings.TrimSpace(prefix)
	p = strings.TrimSpace(strings.TrimSuffix(p, "*"))
	if strings.Contains(p, "*") {
		return "", fmt.Errorf("key prefix %q: only a trailing * is supported", prefix)
	}
	return p, nil
}

// Obsolete marks a fact obsolete because its subject is gone. The value and
// observed_at are intentionally left untouched — there is no new observation.
// The reason is appended to Notes so earlier qualifiers are not lost.
//
// The transition is journaled (the journal records accepted state, and a
// status flip is state): the audit trail is symmetric — going obsolete and
// coming back verified both leave entries. The journal entry carries the
// current value with the reason folded into notes, so "when did this die and
// why" is answerable from the log.
// appendNote joins a line into notes without leading/trailing blank lines
// (the shared join for retire reasons and dated note appends).
func appendNote(base, line string) string {
	if line == "" {
		return base
	}
	if base == "" {
		return line
	}
	return base + "\n" + line
}

// obsoleteScript retires a fact atomically: the reason appends INSIDE the
// script (read-modify-write of notes from the Go side would race a
// concurrent notes_append and silently drop it — the same contract as
// patchScript), and status/current_ref/updated_* land in the same write.
const obsoleteScript = `
if (params.reason_append != null) {
  String n = ctx._source.notes == null ? "" : ctx._source.notes.toString();
  ctx._source.notes = n.isEmpty() ? params.reason_append : n + params.nsep + params.reason_append;
}
if (params.redactions != null) { ctx._source.redactions = params.redactions; }
ctx._source.status = params.status;
ctx._source.current_ref = params.current_ref;
ctx._source.updated_at = params.updated_at;
ctx._source.updated_by = params.updated_by;
`

// Obsolete retires a fact. redactions is the write-time secret-guard audit of
// the reason text: a redacted reason lands in notes, and the rows travel with
// the fact (merged with the current value's audit, which the retirement does
// not rewrite).
func (c *Client) Obsolete(ctx context.Context, key, reason string, redactions []model.Redaction) error {
	existing, seqNo, primaryTerm, err := c.getFactVersion(ctx, key)
	if err != nil {
		return fmt.Errorf("obsolete fact: %w", err)
	}
	reason = strings.TrimSpace(reason)
	if reason != "" && existing.Status == "obsolete" {
		// Mirror Patch's refusal: a reason belongs to the verified→obsolete
		// transition. Re-retiring with a reason would append a duplicate notes
		// line while the journal stays untouched (it fires only on status
		// changes) — notes would grow audit lines the journal never recorded.
		return contractErrorf("obsolete: fact %q is already obsolete (a reason belongs to the verified→obsolete transition)", key)
	}
	// notes is one line per qualifier (see Patch): fold any whitespace run a
	// caller passes into single spaces before the reason joins the notes.
	reason = strings.Join(strings.Fields(reason), " ")
	// The journal snapshot carries the notes as of journal time (a concurrent
	// atomic append that lands after this Get lags one line; the doc stays
	// right — see the Patch journal contract). The already-obsolete refusal
	// above is computed from the stale read, exactly like Patch's: two racing
	// retires both pass the check, so each journals an entry and each appends
	// the reason — at worst a duplicated notes line and an extra journal entry
	// per race (the journal records every accepted write, so the audit trail
	// stays truthful about the events even then).
	notes := appendNote(existing.Notes, reason)
	currentRef := existing.CurrentRef
	// The audit of the stored value survives a retirement: only the reason text
	// may add rows (an appended reason accumulates — the earlier text is still
	// in the notes).
	mergedRedactions := model.MergeRedactions(existing.Redactions, nil, redactions)
	var entry *model.JournalEntry
	if existing.Status != "obsolete" {
		stated := *existing
		stated.Notes = notes
		stated.Redactions = mergedRedactions
		// Doc-first: the entry is built and its id preallocated here, the
		// entry itself is indexed only after the transition is accepted.
		entry = c.newJournalEntryFor(ctx, &stated, existing.FactID, time.Now().UTC())
		// The ops marker names the event: a retirement entry is not a value
		// write, and "who retired this, when" must not be inferred from notes.
		entry.Op = model.OpRetire
		currentRef = uuid.New().String()
	}
	params := map[string]interface{}{
		"reason_append": nil,
		"status":        "obsolete",
		"current_ref":   currentRef,
		"updated_at":    time.Now().UTC().Format(time.RFC3339),
		"updated_by":    c.resolveAuthor(ctx).Author,
		// The newline separator is a param, never "\n" in the script source:
		// painless string literals on this cluster process no escapes beyond
		// \\ and \" and refuse the script at compile time otherwise.
		"nsep": "\n",
	}
	if reason != "" {
		params["reason_append"] = reason
	}
	if len(redactions) > 0 {
		params["redactions"] = mergedRedactions
	}
	body, _ := json.Marshal(map[string]interface{}{
		"script": map[string]interface{}{
			"lang":   "painless",
			"source": obsoleteScript,
			"params": params,
		},
	})
	_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: key,
		Body:       strings.NewReader(string(body)),
		Params:     obsoleteUpdateParams(entry, seqNo, primaryTerm),
	})
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("obsolete %w: %q", ErrNotFound, key)
		}
		if isVersionConflict(err) {
			return conflictError("obsolete fact", key, err)
		}
		return wrapWriteError("obsolete fact", err)
	}
	if entry != nil {
		if _, jErr := c.appendJournalID(ctx, entry, currentRef, "", false); jErr != nil {
			return fmt.Errorf("obsolete: the transition was accepted, the journal entry lagged: %w", jErr)
		}
	}
	return nil
}

// obsoleteUpdateParams makes a retirement that journals a snapshot
// (entry != nil) a CAS write: if a concurrent correction or retirement
// landed after the read, the entry being journaled states a value the doc
// no longer holds and current_ref would point the doc at it. A
// re-retirement that journals nothing stays unconditional.
func obsoleteUpdateParams(entry *model.JournalEntry, seqNo, primaryTerm int) opensearchapi.UpdateParams {
	p := opensearchapi.UpdateParams{Refresh: "wait_for"}
	if entry != nil {
		p.IfSeqNo, p.IfPrimaryTerm = &seqNo, &primaryTerm
	}
	return p
}
