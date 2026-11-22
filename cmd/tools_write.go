package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/model"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxGetKeys and maxAddBatchItems cap the batch tools: a batch is a
// round-trip saving, not a context flood.
const (
	maxGetKeys       = 10
	maxAddBatchItems = 10
)

type addInput struct {
	Key      string   `json:"key" jsonschema:"stable fact key, e.g. proj:foo:loc:bar.py — re-use the same key to update"`
	Value    string   `json:"value" jsonschema:"the fact text"`
	Category string   `json:"category" jsonschema:"location | config | endpoint | behavior | context_map | recipe | rule"`
	Project  string   `json:"project,omitempty" jsonschema:"project name; defaults to 'global'"`
	Source   string   `json:"source,omitempty" jsonschema:"where the fact came from (tool:read_file, doc:README, user:confirmed); defaults to tool:mcp"`
	Notes    string   `json:"notes,omitempty" jsonschema:"qualifiers: what was checked, how far, caveats — carried to later sessions"`
	Keywords []string `json:"keywords,omitempty" jsonschema:"tags for cross-project slices and filtered search (normalized to lowercase single tokens, max 10, deduplicated); an empty list leaves the stored tags alone on a same-value re-add"`
	TTLDays  int      `json:"ttl_days,omitempty" jsonschema:"optional staleness window in days, set it only when you know this fact rots faster than its category default; it may only shorten the window (cap: config/endpoint 14, location/behavior 30, recipe 180, rule 90; context_map uncapped)"`
	Summary  string   `json:"summary,omitempty" jsonschema:"short table-of-contents rendering of the value (under summary_max_len chars); required when the value is longer than summary_min_len; not applied on same-value re-adds, but an over-cap summary is still refused at every edge"`

	// ConfirmSecret bypasses a guard hit at secret_guard=ask. It is the agent's
	// barrier: show the masked finding to the user first, set this only after
	// their explicit approval. At reject it has no effect.
	ConfirmSecret bool `json:"confirm_secret,omitempty" jsonschema:"confirm a guard hit is a false positive (secret_guard=ask only); set only after the user approved the exact finding"`
}

type addOutput struct {
	Key     string `json:"key" jsonschema:"the stored key"`
	Created bool   `json:"created" jsonschema:"true when the fact was newly created, false when an existing fact was updated"`
	Changed bool   `json:"changed" jsonschema:"true when a new version was accepted (birth, value correction); false = same-value touch: notes/source moved, observed_at and the journal untouched"`
	Status  string `json:"status" jsonschema:"status of the stored fact"`
	// PreviousSummary names the superseded version at zero journal cost:
	// the summary the accepted correction replaced ("" when the prior fact
	// had none). It is what closes the "did I replace the critical value or
	// just touch it?" ambiguity.
	PreviousSummary string `json:"previous_summary,omitempty" jsonschema:"summary of the value this write replaced (empty when the fact was created fresh or the prior version had no summary)"`
	// Note explains the least-obvious answer (a same-value touch): without
	// it, created=false+changed=false reads like a no-op and agents mis-age
	// the fact.
	Note string `json:"note,omitempty" jsonschema:"what exactly this write did when the shape alone does not say (same-value touch: observed_at and age keep counting, notes/source moved only; resurrect: an obsolete fact re-declared alive)"`
	// Redactions names every span the secret guard replaced before storing
	// (field, detector pattern, size — never the text). Non-empty means the
	// stored value is SHORTER than the one written: the flag is not noise, it
	// tells the caller the claim lost a piece.
	Redactions []model.Redaction `json:"redactions,omitempty" jsonschema:"spans the secret guard replaced before storing (field, pattern, chars); the stored value is shorter than the one written"`
}

func (s *mcpServer) add(ctx context.Context, req *mcp.CallToolRequest, in addInput) (*mcp.CallToolResult, addOutput, error) {
	cfg, cerr := s.loadConfig()
	if cerr != nil {
		return nil, addOutput{}, storeError("velruna_add", cerr)
	}
	out, err := s.addOne(ctx, cfg, nil, nil, in)
	if err != nil {
		return nil, addOutput{}, err
	}
	return nil, out, nil
}

// addOne is the add contract for one item: validate → guard → write. The
// config arrives loaded (the guard and the client share it; a locked wallet
// is not prompted twice), so velruna_add_batch runs the identical pipeline
// per item without re-reading config per call.
// addOne is the shared write path of velruna_add and velruna_add_batch.
// c is the shared client (a batch reuses one connection and one EnsureIndex);
// nil means "create one for this single call". cErr is a client-creation
// failure the batch already paid for: the item runs validation and guard and
// reports it WITHOUT a store attempt (a slow store must not cost the batch
// ten timeouts and ten identical per-item client creations). The key is
// trimmed at this edge: "k " and "k" must not become two docs with
// diverging history.
func (s *mcpServer) addOne(ctx context.Context, cfg *config.Config, c *client.Client, cErr error, in addInput) (addOutput, error) {
	// Validate the shape before guard and store: an empty key produces a
	// cryptic store error, and an unknown category never ages (silent rot).
	in.Key = strings.TrimSpace(in.Key)
	if in.Key == "" {
		return addOutput{}, errors.New("velruna_add: key is required")
	}
	if strings.TrimSpace(in.Value) == "" {
		return addOutput{}, errors.New("velruna_add: value is required")
	}
	if !model.ValidCategory(strings.ToLower(in.Category)) {
		return addOutput{}, fmt.Errorf("velruna_add: unknown category %q (want location | config | endpoint | behavior | context_map | recipe | rule)", in.Category)
	}
	// Canonicalize for storage: the TTL table and the search category filter
	// are case-sensitive, so a mixed-case write would silently never age and
	// never match category filters.
	in.Category = strings.ToLower(in.Category)
	in.Project = strings.TrimSpace(in.Project)
	if in.Project == "" {
		in.Project = "global"
	}
	if in.Source == "" {
		in.Source = "tool:mcp"
	}
	// The guard runs before any store contact: a rejected write must be
	// rejected even when OpenSearch is down. The project label is agent-supplied
	// identity text stored in the shared index (the fetch path gates it the same
	// way) — the default "global" is inert.
	//
	// Identity fields (key, project, source, tags) can only refuse: rewriting a
	// span there would silently change what the fact IS. Text fields may come
	// back shortened (a medium-confidence heuristic hit is replaced, not fatal),
	// and the audit of what was cut travels with the stored fact.
	kwForGuard := strings.Join(in.Keywords, " ")
	redactions, gErr := guardSanitize(cfg.SecretGuard, in.ConfirmSecret,
		identityField("key", &in.Key),
		textField("value", &in.Value),
		identityField("project", &in.Project),
		textField("notes", &in.Notes),
		textField("summary", &in.Summary),
		identityField("keywords", &kwForGuard),
		identityField("source", &in.Source))
	if gErr != nil {
		return addOutput{}, gErr
	}
	if cErr != nil {
		return addOutput{}, cErr
	}
	if c == nil {
		var cerr error
		c, cerr = s.clientFromConfig(ctx, cfg)
		if cerr != nil {
			return addOutput{}, storeError("velruna_add", cerr)
		}
	}
	f := model.NewFact(in.Key, in.Value, in.Category, in.Project, in.Source, in.Notes)
	f.Summary = in.Summary
	f.TTLDays = in.TTLDays
	f.Redactions = redactions
	// Normalized at the edge as well as in the client: the error must name the
	// tool call, and client.Add re-normalizes for the CLI path.
	kw, kwErr := model.NormalizeKeywords(in.Keywords)
	if kwErr != nil {
		return addOutput{}, fmt.Errorf("velruna_add: %v", kwErr)
	}
	f.Keywords = kw
	res, err := c.Add(ctx, f)
	if err != nil {
		// Partial success (the fact doc landed, the journal/link follow-up
		// failed): Add reports what landed in res. The single velruna_add edge
		// cannot surface the flags — go-sdk discards the typed output when the
		// handler returns an error, so the honest state travels in the error
		// text — but velruna_add_batch copies these flags into its per-item
		// output next to item.Error, where they are visible. Do not "simplify"
		// this away: the batch answer would silently zero what did store.
		if res.Created || res.Changed {
			return addOutput{Key: in.Key, Created: res.Created, Changed: res.Changed, Status: f.Status, Redactions: redactions}, writeFailure("velruna_add", err)
		}
		return addOutput{}, writeFailure("velruna_add", err)
	}
	out := addOutput{Key: in.Key, Created: res.Created, Changed: res.Changed,
		Status: f.Status, PreviousSummary: res.PreviousSummary, Redactions: redactions}
	if !res.Created && !res.Changed {
		out.Note = "same-value touch: observed_at and the journal untouched, the fact keeps aging (re-verify when stale); notes/source moved only"
		if in.Summary != "" {
			// A passed summary would rewrite the CURRENT value's table of
			// contents without a new version — silently misleading in a
			// shared store. It is dropped; say so instead of hiding it.
			out.Note += "; the passed summary was ignored on a same-value touch (correct the value to change its summary)"
		}
	} else if res.Resurrected {
		out.Note = "resurrected: the fact was obsolete; status is back to verified, observed_at moved, and the event is journaled"
	}
	return out, nil
}

// addBatchInput carries add-shaped items; confirm_secret is per item, so a
// user-approved false positive on one fact never whitewashes the next.
type addBatchInput struct {
	Items []addInput `json:"items" jsonschema:"1..10 add-shaped items (key, value, category, ...); duplicate keys are rejected"`
}

type addBatchOutput struct {
	Count int `json:"count" jsonschema:"number of items processed"`
	// Summary counters: the whole-session ritual without re-reading items.
	Created int            `json:"created" jsonschema:"items newly created"`
	Changed int            `json:"changed" jsonschema:"items that accepted a new version (includes created)"`
	Touched int            `json:"touched" jsonschema:"items touched on the same value (notes/source only)"`
	Items   []addBatchItem `json:"items" jsonschema:"per-item outcome in request order; a failed item does not roll back the others"`
}

type addBatchItem struct {
	Key     string `json:"key" jsonschema:"the item's key"`
	Created bool   `json:"created" jsonschema:"see velruna_add"`
	Changed bool   `json:"changed" jsonschema:"see velruna_add"`
	Status  string `json:"status,omitempty" jsonschema:"status of the stored fact"`
	// Error carries the item's rejection (guard hit, validation, store
	// failure). Partial success is legal: OpenSearch writes are per-doc, and
	// a batch transaction would be a fiction.
	Error string `json:"error,omitempty" jsonschema:"this item's failure (guard/validation/store); other items were processed regardless"`
	// Redactions is velruna_add's audit per item: the stored value lost a
	// flagged span. The batch answer must carry it — the per-item outcome is
	// the only place a caller can see that a write landed shortened.
	Redactions []model.Redaction `json:"redactions,omitempty" jsonschema:"spans the secret guard replaced before storing (see velruna_add)"`
}

func (s *mcpServer) addBatch(ctx context.Context, req *mcp.CallToolRequest, in addBatchInput) (*mcp.CallToolResult, addBatchOutput, error) {
	if len(in.Items) == 0 || len(in.Items) > maxAddBatchItems {
		return nil, addBatchOutput{}, fmt.Errorf("velruna_add_batch: items must be 1..%d", maxAddBatchItems)
	}
	// Duplicate keys in one batch are ambiguous by construction: the second
	// item silently corrects the first (order-dependent created/changed and
	// summary requirements). Reject the shape instead of picking a winner —
	// on the TRIMMED key, which is what addOne stores.
	seen := make(map[string]bool, len(in.Items))
	for _, it := range in.Items {
		k := strings.TrimSpace(it.Key)
		if seen[k] {
			return nil, addBatchOutput{}, fmt.Errorf("velruna_add_batch: duplicate key %q in one batch", k)
		}
		seen[k] = true
	}
	cfg, cerr := s.loadConfig()
	if cerr != nil {
		return nil, addBatchOutput{}, storeError("velruna_add_batch", cerr)
	}
	// One client for the batch: a shared connection and a single EnsureIndex
	// (the per-item version made the "one round-trip" tool ten round-trips).
	c, cErr := s.clientFromConfig(ctx, cfg)
	if cErr != nil {
		cErr = storeError("velruna_add_batch", cErr)
	}
	out := addBatchOutput{Items: make([]addBatchItem, 0, len(in.Items))}
	for i, it := range in.Items {
		res, err := s.addOne(ctx, cfg, c, cErr, it)
		// The echo carries the TRIMMED key: it is what the store holds, and a
		// caller reconciling answers against the store must not miss on " k ".
		item := addBatchItem{Key: strings.TrimSpace(it.Key), Created: res.Created, Changed: res.Changed, Status: res.Status, Redactions: res.Redactions}
		if err != nil {
			// Label the item, not the single-add tool: an agent reading a
			// batch answer must not conclude velruna_add itself broke.
			item.Error = "velruna_add_batch item " + strconv.Itoa(i) + ": " + strings.TrimPrefix(err.Error(), "velruna_add: ")
		}
		// The counters read what LANDED, not only what completed cleanly: a
		// partial success stored the fact (item.Error names the lagged
		// follow-up), and a summary that contradicts the per-item truth is
		// exactly the audit lie the counters exist to prevent.
		if res.Created {
			out.Created++
		}
		if res.Changed {
			out.Changed++
		}
		if err == nil && !res.Changed {
			out.Touched++
		}
		out.Items = append(out.Items, item)
	}
	out.Count = len(out.Items)
	// A store-wide failure (config/client) surfaces as per-item errors: the
	// batch itself answers "what happened to each write", which is the point.
	return nil, out, nil
}

type patchInput struct {
	Key          string   `json:"key" jsonschema:"exact fact key of an EXISTING fact (patch never creates: use velruna_add)"`
	Value        string   `json:"value,omitempty" jsonschema:"new value: journaled, observed_at moves; the summary resets with it (pass a new summary for a long value)"`
	Summary      string   `json:"summary,omitempty" jsonschema:"new table of contents (with a value change: omitting it CLEARS the old summary — an old summary for a new value is a lie)"`
	Notes        string   `json:"notes,omitempty" jsonschema:"replaces notes (non-empty = provided, add's convention)"`
	NotesAppend  string   `json:"notes_append,omitempty" jsonschema:"append a dated line 'YYYY-MM-DD: text' to notes (atomic server-side; the cheap qualifier update — no need to resend the value)"`
	Source       string   `json:"source,omitempty" jsonschema:"replaces source when non-empty"`
	Status       string   `json:"status,omitempty" jsonschema:"verified | obsolete: a transition is journaled; obsolete→verified sets observed_at=now (re-declaring alive is a fresh verification)"`
	Reason       string   `json:"reason,omitempty" jsonschema:"why it became obsolete (only with status=obsolete; folded into notes)"`
	Reverify     bool     `json:"reverify,omitempty" jsonschema:"re-confirm the current value: observed_at moves to now, the journal gains op=rediscovery, the writer joins confirmed_by; cannot be combined with value/summary/status"`
	TTLDays      int      `json:"ttl_days,omitempty" jsonschema:"retune the staleness window in days (bounded by the category cap: config/endpoint 14, location/behavior 30, recipe 180, rule 90; context_map uncapped); omitted = untouched; a qualifier touch, not journaled"`
	Keywords     []string `json:"keywords,omitempty" jsonschema:"replaces the tag set when non-empty (same normalization as add: lowercase single tokens, max 10); a qualifier touch, not journaled — the tags appear in the history with the next accepted value"`
	SupersededBy string   `json:"superseded_by,omitempty" jsonschema:"key of the fact that replaces this one (the successor to trust instead): set once, never repointed; the target must exist and be verified and the chain must stay cycle-free; journaled as op=supersede"`

	// ConfirmSecret bypasses a guard hit at secret_guard=ask (see addInput).
	ConfirmSecret bool `json:"confirm_secret,omitempty" jsonschema:"confirm a guard hit is a false positive (secret_guard=ask only); set only after the user approved the exact finding"`
}

type patchOutput struct {
	Key        string `json:"key" jsonschema:"the patched key"`
	Changed    bool   `json:"changed" jsonschema:"true = a journaled event landed (new value, status transition, or a re-confirmation); false = qualifier-only touch"`
	Status     string `json:"status" jsonschema:"status of the fact after the patch"`
	Reverified bool   `json:"reverified,omitempty" jsonschema:"true when the patch re-confirmed the value in place (observed_at moved, op=rediscovery journaled, value unchanged)"`
	Note       string `json:"note,omitempty" jsonschema:"set on a qualifier-only touch: observed_at and age untouched, the journal not appended"`
	// Redactions: see velruna_add — the stored value lost a flagged span.
	Redactions []model.Redaction `json:"redactions,omitempty" jsonschema:"spans the secret guard replaced before storing (see velruna_add)"`
}

func (s *mcpServer) patch(ctx context.Context, req *mcp.CallToolRequest, in patchInput) (*mcp.CallToolResult, patchOutput, error) {
	// Shape first: a malformed patch is the caller's problem, answered before
	// config, guard and any store contact (the list/search/keys contract).
	if strings.TrimSpace(in.Key) == "" {
		return nil, patchOutput{}, errors.New("velruna_patch: key is required")
	}
	if err := client.ValidatePatchFields(patchFields(in)); err != nil {
		return nil, patchOutput{}, err
	}
	cfg, cerr := s.loadConfig()
	if cerr != nil {
		return nil, patchOutput{}, storeError("velruna_patch", cerr)
	}
	out, err := s.patchOne(ctx, cfg, nil, nil, in)
	if err != nil {
		return nil, patchOutput{}, err
	}
	return nil, out, nil
}

// patchFields maps the MCP input onto the client's patch shape (one place,
// shared by the single and batch paths).
func patchFields(in patchInput) client.PatchFields {
	return client.PatchFields{
		Value:        in.Value,
		Summary:      in.Summary,
		Notes:        in.Notes,
		NotesAppend:  in.NotesAppend,
		Source:       in.Source,
		Status:       in.Status,
		Reason:       in.Reason,
		TTLDays:      in.TTLDays,
		Keywords:     in.Keywords,
		Reverify:     in.Reverify,
		SupersededBy: in.SupersededBy,
	}
}

// patchOne is the shared write path of velruna_patch and velruna_patch_batch
// (the addOne contract): validate → guard → write. c is the shared client
// (a batch reuses one connection); nil means "create one". cErr is a
// client-creation failure the batch already paid for: the item still runs
// validation and guard and reports the failure WITHOUT a store attempt.
func (s *mcpServer) patchOne(ctx context.Context, cfg *config.Config, c *client.Client, cErr error, in patchInput) (patchOutput, error) {
	in.Key = strings.TrimSpace(in.Key)
	if in.Key == "" {
		return patchOutput{}, errors.New("velruna_patch: key is required")
	}
	// Shape first, on the text as written: a malformed patch is the caller's
	// problem and must be answered before the guard rewrites anything.
	if err := client.ValidatePatchFields(patchFields(in)); err != nil {
		return patchOutput{}, err
	}
	// Every provided field is user-supplied text on a write path — the guard
	// covers all of them, notes_append and reason included (a secret can
	// arrive in a dated note as easily as in a value). Same split as add:
	// identity fields refuse, text fields may come back shortened.
	kwForGuard := strings.Join(in.Keywords, " ")
	redactions, gErr := guardSanitize(cfg.SecretGuard, in.ConfirmSecret,
		identityField("key", &in.Key),
		textField("value", &in.Value),
		textField("summary", &in.Summary),
		textField("notes", &in.Notes),
		textField("notes_append", &in.NotesAppend),
		identityField("source", &in.Source),
		identityField("keywords", &kwForGuard),
		identityField("superseded_by", &in.SupersededBy),
		textField("reason", &in.Reason))
	if gErr != nil {
		return patchOutput{}, gErr
	}
	fields := patchFields(in)
	fields.Redactions = redactions
	if cErr != nil {
		return patchOutput{}, cErr
	}
	if c == nil {
		var cerr error
		c, cerr = s.clientFromConfig(ctx, cfg)
		if cerr != nil {
			return patchOutput{}, storeError("velruna_patch", cerr)
		}
	}
	res, err := c.Patch(ctx, in.Key, fields)
	if errors.Is(err, client.ErrNotFound) {
		return patchOutput{}, fmt.Errorf("velruna_patch: no fact with key %q: nothing to patch (velruna_add creates)", in.Key)
	}
	if err != nil {
		// Partial success (the patch landed, the journal follow-up lagged):
		// Patch reports what landed in res. velruna_patch_batch copies the
		// flags into its per-item output; do not drop them (see addOne's note —
		// the single edge cannot show them, the batch edge must).
		if res.Changed || res.Status != "" {
			return patchOutput{Key: in.Key, Changed: res.Changed, Status: res.Status, Reverified: res.Reverified}, writeFailure("velruna_patch", err)
		}
		return patchOutput{}, writeFailure("velruna_patch", err)
	}
	out := patchOutput{Key: in.Key, Changed: res.Changed, Status: res.Status, Reverified: res.Reverified, Redactions: redactions}
	if !res.Changed {
		out.Note = "qualifier-only touch: observed_at and the journal untouched, the fact keeps aging (re-verify when stale)"
	}
	return out, nil
}

// patchBatchInput carries patch-shaped items; confirm_secret is per item
// (the add_batch rule): one approved false positive never whitewashes the
// next item.
type patchBatchInput struct {
	Items []patchInput `json:"items" jsonschema:"1..10 patch-shaped items (key, value/notes_append/status, ...); duplicate keys are rejected"`
}

type patchBatchOutput struct {
	Count int `json:"count" jsonschema:"number of items processed"`
	// Summary counters (the add_batch rule): what the batch actually did.
	Changed    int              `json:"changed" jsonschema:"items that journaled an event (value change, status transition, re-confirmation)"`
	Reverified int              `json:"reverified,omitempty" jsonschema:"subset of changed: items that re-confirmed the value in place"`
	Touched    int              `json:"touched" jsonschema:"items patched on qualifiers only (no journal entry)"`
	Items      []patchBatchItem `json:"items" jsonschema:"per-item outcome in request order; a failed item does not roll back the others"`
}

type patchBatchItem struct {
	Key        string            `json:"key" jsonschema:"the item's key"`
	Changed    bool              `json:"changed" jsonschema:"see velruna_patch"`
	Reverified bool              `json:"reverified,omitempty" jsonschema:"see velruna_patch"`
	Status     string            `json:"status,omitempty" jsonschema:"status of the fact after the patch"`
	Error      string            `json:"error,omitempty" jsonschema:"this item's failure (guard/validation/store); other items were processed regardless"`
	Redactions []model.Redaction `json:"redactions,omitempty" jsonschema:"spans the secret guard replaced before storing (see velruna_add)"`
}

func (s *mcpServer) patchBatch(ctx context.Context, req *mcp.CallToolRequest, in patchBatchInput) (*mcp.CallToolResult, patchBatchOutput, error) {
	if len(in.Items) == 0 || len(in.Items) > maxAddBatchItems {
		return nil, patchBatchOutput{}, fmt.Errorf("velruna_patch_batch: items must be 1..%d", maxAddBatchItems)
	}
	// Duplicate keys are ambiguous in a patch batch too: the second item
	// patches the first item's fresh state (order-dependent journaled
	// transitions). Reject the shape — on the TRIMMED key.
	seen := make(map[string]bool, len(in.Items))
	for _, it := range in.Items {
		k := strings.TrimSpace(it.Key)
		if seen[k] {
			return nil, patchBatchOutput{}, fmt.Errorf("velruna_patch_batch: duplicate key %q in one batch", k)
		}
		seen[k] = true
	}
	cfg, cerr := s.loadConfig()
	if cerr != nil {
		return nil, patchBatchOutput{}, storeError("velruna_patch_batch", cerr)
	}
	c, cErr := s.clientFromConfig(ctx, cfg)
	if cErr != nil {
		cErr = storeError("velruna_patch_batch", cErr)
	}
	out := patchBatchOutput{Items: make([]patchBatchItem, 0, len(in.Items))}
	for i, it := range in.Items {
		res, err := s.patchOne(ctx, cfg, c, cErr, it)
		item := patchBatchItem{Key: strings.TrimSpace(it.Key), Changed: res.Changed, Status: res.Status, Reverified: res.Reverified, Redactions: res.Redactions}
		if err != nil {
			// Batch label, same as velruna_add_batch: the item failed, not
			// the single-patch tool.
			item.Error = "velruna_patch_batch item " + strconv.Itoa(i) + ": " + strings.TrimPrefix(err.Error(), "velruna_patch: ")
		}
		// Counters read what landed (the add_batch rule): a partial success
		// did change the fact; the error names its lagged follow-up.
		if res.Changed {
			out.Changed++
			if res.Reverified {
				out.Reverified++
			}
		} else if err == nil {
			out.Touched++
		}
		out.Items = append(out.Items, item)
	}
	out.Count = len(out.Items)
	return nil, out, nil
}

type obsoleteInput struct {
	Key    string `json:"key" jsonschema:"key of the fact whose subject is gone"`
	Reason string `json:"reason,omitempty" jsonschema:"why it is gone (file deleted, endpoint removed); appended to notes"`

	// ConfirmSecret bypasses a guard hit at secret_guard=ask (see addInput).
	ConfirmSecret bool `json:"confirm_secret,omitempty" jsonschema:"confirm a guard hit is a false positive (secret_guard=ask only); set only after the user approved the exact finding"`
}

type obsoleteOutput struct {
	Key    string `json:"key" jsonschema:"the key that was marked obsolete"`
	Status string `json:"status" jsonschema:"the new status"`
	// Redactions: see velruna_add — the stored reason text lost a flagged span.
	Redactions []model.Redaction `json:"redactions,omitempty" jsonschema:"spans the secret guard replaced before storing (see velruna_add)"`
}

func (s *mcpServer) obsolete(ctx context.Context, req *mcp.CallToolRequest, in obsoleteInput) (*mcp.CallToolResult, obsoleteOutput, error) {
	// The config is the guard's policy: a load failure must fail the write
	// (as in add), never silently skip the scan of a credential-like reason.
	// Loaded once and reused: a locked wallet is not prompted twice per write.
	in.Key = strings.TrimSpace(in.Key)
	if in.Key == "" {
		return nil, obsoleteOutput{}, errors.New("velruna_obsolete: key is required")
	}
	cfg, cerr := s.loadConfig()
	if cerr != nil {
		return nil, obsoleteOutput{}, storeError("velruna_obsolete", cerr)
	}
	// The CLI edge scans key+reason; the MCP edge must not be the narrower
	// gate (a credential-shaped key reaches the shared index via MCP only).
	// Key is identity (rename it), the reason is text: a heuristic hit there is
	// redacted and the retirement proceeds, the same policy as add/patch.
	redactions, gErr := guardSanitize(cfg.SecretGuard, in.ConfirmSecret,
		identityField("key", &in.Key),
		textField("reason", &in.Reason))
	if gErr != nil {
		return nil, obsoleteOutput{}, gErr
	}
	c, err := s.clientFromConfig(ctx, cfg)
	if err != nil {
		return nil, obsoleteOutput{}, storeError("velruna_obsolete", err)
	}
	err = c.Obsolete(ctx, in.Key, in.Reason, redactions)
	if errors.Is(err, client.ErrNotFound) {
		return nil, obsoleteOutput{}, fmt.Errorf("no fact with key %q: nothing to mark obsolete", in.Key)
	}
	if err != nil {
		return nil, obsoleteOutput{}, writeFailure("velruna_obsolete", err)
	}
	return nil, obsoleteOutput{Key: in.Key, Status: "obsolete", Redactions: redactions}, nil
}

type pruneInput struct {
	Key     string `json:"key" jsonschema:"key of the fact whose history to prune"`
	Keep    int    `json:"keep,omitempty" jsonschema:"number of newest history entries to keep; 0 = history_limit from config"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"must be true to write; without it only the plan is returned"`
}

type pruneOutput struct {
	Key          string `json:"key" jsonschema:"the pruned key"`
	HistoryTotal int    `json:"history_total" jsonschema:"history entries before the call"`
	Keep         int    `json:"keep" jsonschema:"history entries that survive"`
	Removed      int    `json:"removed" jsonschema:"entries deleted (0 when nothing was written)"`
	Pruned       bool   `json:"pruned" jsonschema:"true when history was actually rewritten"`
	Plan         string `json:"plan,omitempty" jsonschema:"no-write preview: what confirm=true would destroy. Relay it to the user and re-call with confirm=true only after their approval"`
}

// prune is destructive, so it carries the same barrier as the CLI: without
// confirm=true the plan is returned and nothing is written. An agent must
// relay the plan to the user and re-call only after explicit approval.
func (s *mcpServer) prune(ctx context.Context, req *mcp.CallToolRequest, in pruneInput) (*mcp.CallToolResult, pruneOutput, error) {
	in.Key = strings.TrimSpace(in.Key)
	if in.Key == "" {
		return nil, pruneOutput{}, errors.New("velruna_prune: key is required")
	}
	cfg, cerr := s.loadConfig()
	if cerr != nil {
		return nil, pruneOutput{}, storeError("velruna_prune", cerr)
	}
	// keep resolves from local config BEFORE any store contact: the refusal
	// needs no store, and it must not pay for an index ensure.
	keep := in.Keep
	if keep < 0 {
		// A negative keep used to collapse silently into the configured
		// default: the operator asked to prune and got a different plan.
		return nil, pruneOutput{}, fmt.Errorf("velruna_prune: keep must not be negative (got %d); pass keep=0 for the configured history_limit", in.Keep)
	}
	if keep <= 0 {
		keep = cfg.HistoryLimit
	}
	if keep <= 0 {
		// history_limit: 0 means "keep everything": silently collapsing it
		// to 1 would destroy every history entry. Demand an explicit keep.
		return nil, pruneOutput{}, fmt.Errorf("velruna_prune: history_limit is 0 (keep everything) and no keep value was given: pass an explicit keep (e.g. keep: 5)")
	}
	c, err := s.clientFromConfig(ctx, cfg)
	if err != nil {
		return nil, pruneOutput{}, storeError("velruna_prune", err)
	}
	f, err := c.Get(ctx, in.Key)
	if errors.Is(err, client.ErrNotFound) {
		return nil, pruneOutput{}, fmt.Errorf("no fact with key %q: nothing to prune", in.Key)
	}
	if err != nil {
		return nil, pruneOutput{}, storeError("velruna_prune", err)
	}
	total := len(f.History)
	// Journal-first: once a key has journal entries, the journal IS the
	// history and the legacy in-doc array is only counted pre-migration.
	if jt, jErr := c.JournalCount(ctx, in.Key); jErr != nil {
		return nil, pruneOutput{}, storeError("velruna_prune", jErr)
	} else if jt > 0 {
		total = jt
	}
	if total <= keep {
		return nil, pruneOutput{Key: in.Key, HistoryTotal: total, Keep: total, Removed: 0}, nil
	}
	if !in.Confirm {
		// The barrier: report the plan as a normal result and write nothing.
		// The plan is the deliverable of this call, not a failure: error-shaped
		// results get dropped by clients that swallow tool errors, silently
		// removing the safety barrier the agent must relay to the user.
		return nil, pruneOutput{
			Key:          in.Key,
			HistoryTotal: total,
			Keep:         keep,
			Removed:      0,
			Pruned:       false,
			Plan: fmt.Sprintf("key %q has %d history entries; confirm=true keeps the newest %d and drops at most %d observations for good (the entry that carries the current value is always spared). Nothing was written. Show this plan to the user and re-call with confirm=true only after their approval",
				in.Key, total, keep, total-keep),
		}, nil
	}
	removed, err := c.Prune(ctx, in.Key, keep)
	if err != nil {
		return nil, pruneOutput{}, storeError("velruna_prune", err)
	}
	return nil, pruneOutput{Key: in.Key, HistoryTotal: total, Keep: keep, Removed: removed, Pruned: true}, nil
}
