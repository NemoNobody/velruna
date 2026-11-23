package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/model"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The workset MCP surface mirrors the CLI one-for-one: a queue is coordinated
// by agents, so every CLI operation has an agent-callable equivalent.

type worksetCreateInput struct {
	Name          string `json:"name" jsonschema:"workset slug: 1..64 characters from a-z, 0-9 and '_' (no leading/trailing '_', no '__')"`
	Project       string `json:"project,omitempty" jsonschema:"project label; defaults to global"`
	ConfirmSecret bool   `json:"confirm_secret,omitempty" jsonschema:"confirm a guard hit on the project label is a false positive (secret_guard=ask only); set only after the user approved the exact finding"`
}

type worksetListInput struct {
	Project string `json:"project,omitempty" jsonschema:"project label to list; empty lists every project"`
}

type worksetShowInput struct {
	Name     string   `json:"name" jsonschema:"workset name or alias"`
	After    int      `json:"after,omitempty" jsonschema:"keep entries after this 1-based position (exclusive)"`
	Before   int      `json:"before,omitempty" jsonschema:"keep entries before this 1-based position (exclusive)"`
	Limit    int      `json:"limit,omitempty" jsonschema:"max entries to return (default 50, cap 200)"`
	Phase    []string `json:"phase,omitempty" jsonschema:"phase filter (todo|in_progress|blocked|testing|done|cancelled); applied before the positional window"`
	LiveOnly bool     `json:"live_only,omitempty" jsonschema:"keep only entries whose fact is still verified (drop obsolete and missing facts)"`
	Journal  int      `json:"journal,omitempty" jsonschema:"also return the newest N journal entries (0: no journal)"`
}

type worksetAddInput struct {
	Name          string   `json:"name" jsonschema:"workset name or alias"`
	FactIDs       []string `json:"fact_ids" jsonschema:"fact ids to add (at least one)"`
	Phase         string   `json:"phase,omitempty" jsonschema:"initial phase: todo|in_progress|blocked|testing|done|cancelled (default todo); done is accepted for reference entries"`
	Note          string   `json:"note,omitempty" jsonschema:"note stored on each added entry"`
	ConfirmSecret bool     `json:"confirm_secret,omitempty" jsonschema:"confirm a guard hit on note is a false positive (secret_guard=ask only); set only after the user approved the exact finding"`
}

type worksetRemoveInput struct {
	Name    string   `json:"name" jsonschema:"workset name or alias"`
	FactIDs []string `json:"fact_ids" jsonschema:"fact ids to remove (at least one)"`
}

type worksetPhaseInput struct {
	Name          string   `json:"name" jsonschema:"workset name or alias"`
	FactIDs       []string `json:"fact_ids" jsonschema:"fact ids to move (at least one)"`
	To            string   `json:"to" jsonschema:"target phase: todo|in_progress|blocked|testing|done|cancelled"`
	Note          string   `json:"note,omitempty" jsonschema:"reason for the move (stored in the journal)"`
	Takeover      bool     `json:"takeover,omitempty" jsonschema:"break a claim held by another author (journaled as an explicit takeover)"`
	ConfirmSecret bool     `json:"confirm_secret,omitempty" jsonschema:"confirm a guard hit on note is a false positive (secret_guard=ask only); set only after the user approved the exact finding"`
}

type worksetRenameInput struct {
	Name    string `json:"name" jsonschema:"current workset name or alias"`
	NewName string `json:"new_name" jsonschema:"new slug; the old name stays resolvable as an alias"`
}

// worksetDeleteInput carries a confirm flag: the delete is irreversible and
// shared, and MCP has no interactive prompt, so an agent must show the name to
// the user and re-call with confirm=true (the velruna_prune contract).
type worksetDeleteInput struct {
	Name    string `json:"name" jsonschema:"workset name or alias to delete (destructive, no undo)"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"must be true to delete; without it the call is refused with the workset name for confirmation"`
}

type worksetCursorInput struct {
	Name          string `json:"name" jsonschema:"workset name or alias"`
	At            *int   `json:"at,omitempty" jsonschema:"set the cursor to this 1-based position (0 is a valid position)"`
	Clear         bool   `json:"clear,omitempty" jsonschema:"clear this reader's cursor"`
	Author        string `json:"author,omitempty" jsonschema:"author label; defaults to the resolved writer label"`
	Host          string `json:"host,omitempty" jsonschema:"host label; defaults to the resolved host label"`
	ConfirmSecret bool   `json:"confirm_secret,omitempty" jsonschema:"confirm a guard hit on author/host is a false positive (secret_guard=ask only); set only after the user approved the exact finding"`
}

type worksetEntryView struct {
	Index     int    `json:"index" jsonschema:"1-based position in the workset (after phase/live filters, before the --after/--before window)"`
	FactID    string `json:"fact_id" jsonschema:"the referenced fact"`
	FactState string `json:"fact_state" jsonschema:"verified | obsolete | missing | unknown (unknown = the state read did not answer)"`
	Phase     string `json:"phase" jsonschema:"todo | in_progress | blocked | testing | done | cancelled"`
	PhaseBy   string `json:"phase_by,omitempty" jsonschema:"author who entered the current phase"`
	PhaseAt   string `json:"phase_at,omitempty" jsonschema:"RFC3339 timestamp of when the current phase was entered"`
	Note      string `json:"note,omitempty" jsonschema:"per-entry note"`
}

type worksetView struct {
	ID        string             `json:"id,omitempty" jsonschema:"workset id"`
	Name      string             `json:"name" jsonschema:"workset slug"`
	Aliases   []string           `json:"aliases,omitempty" jsonschema:"former names that still resolve"`
	Project   string             `json:"project" jsonschema:"project label"`
	Version   int64              `json:"version" jsonschema:"workset document write counter"`
	Entries   []worksetEntryView `json:"entries,omitempty" jsonschema:"entries in workset order"`
	CreatedBy string             `json:"created_by,omitempty" jsonschema:"author who created the workset"`
	UpdatedBy string             `json:"updated_by,omitempty" jsonschema:"author who last changed the workset"`
	UpdatedAt string             `json:"updated_at,omitempty" jsonschema:"RFC3339 timestamp of the last change"`
}

type worksetSummaryView struct {
	Name      string   `json:"name" jsonschema:"workset slug"`
	Project   string   `json:"project" jsonschema:"project label"`
	Aliases   []string `json:"aliases,omitempty" jsonschema:"former names that still resolve"`
	Entries   int      `json:"entries" jsonschema:"number of entries"`
	Version   int64    `json:"version" jsonschema:"workset document write counter"`
	UpdatedAt string   `json:"updated_at,omitempty" jsonschema:"RFC3339 timestamp of the last change"`
}

type worksetJournalView struct {
	Op       string `json:"op" jsonschema:"create|add|remove|rename|delete|phase|takeover"`
	Takeover string `json:"takeover,omitempty" jsonschema:"explicit or stale when the row broke another author's claim; empty otherwise"`
	FactID   string `json:"fact_id,omitempty" jsonschema:"entry the record is about"`
	From     string `json:"from,omitempty" jsonschema:"previous phase"`
	To       string `json:"to,omitempty" jsonschema:"new phase"`
	Note     string `json:"note,omitempty" jsonschema:"reason recorded with the change"`
	By       string `json:"by" jsonschema:"author of the change"`
	At       string `json:"at" jsonschema:"RFC3339 timestamp"`
}

type worksetCursorView struct {
	Key       string `json:"key" jsonschema:"author@host cursor key"`
	At        int    `json:"at" jsonschema:"1-based position"`
	UpdatedAt string `json:"updated_at,omitempty" jsonschema:"RFC3339 timestamp of the last move"`
}

type worksetOutput struct {
	Workset          *worksetView         `json:"workset,omitempty" jsonschema:"the workset (create/add/remove/phase/rename/show)"`
	Worksets         []worksetSummaryView `json:"worksets,omitempty" jsonschema:"list results"`
	Name             string               `json:"name,omitempty" jsonschema:"affected name (delete)"`
	Count            int                  `json:"count,omitempty" jsonschema:"number of items in the answer"`
	Deleted          bool                 `json:"deleted,omitempty" jsonschema:"true when the workset was deleted"`
	Cursor           *worksetCursorView   `json:"cursor,omitempty" jsonschema:"this reader's cursor"`
	Cursors          []string             `json:"cursors,omitempty" jsonschema:"every reader's cursor position, author@host@index, newest first (show)"`
	Journal          []worksetJournalView `json:"journal,omitempty" jsonschema:"newest journal entries (show with journal>0)"`
	InProgress       []string             `json:"in_progress,omitempty" jsonschema:"presence line entries: author@host@index for every in_progress entry"`
	SelfVerified     []string             `json:"self_verified,omitempty" jsonschema:"done entries closed by the author who tested them, when the loaded journal proves it"`
	TruncatedEntries int                  `json:"truncated_entries,omitempty" jsonschema:"entries omitted from the returned rows because the workset is larger than the cap"`
	Hint             string               `json:"hint,omitempty" jsonschema:"next right move (filter order, missing cursor, empty list)"`
	Redactions       []model.Redaction    `json:"redactions,omitempty" jsonschema:"secret-guard audit rows: what was edited out before the write"`
}

// worksetToolClient is the lazy loader shared by every workset tool: config is
// reloaded per call (a broken file or an unreachable store must not kill the
// server), the fact index is ensured by newClientWithConfig, and the separate
// worksets indices are ensured here.
func (s *mcpServer) worksetToolClient(ctx context.Context) (*client.Client, *config.Config, error) {
	c, cfg, err := s.newClientWithConfig(ctx)
	if err != nil {
		return nil, nil, storeError("velruna workset", err)
	}
	if err := c.EnsureWorksetIndexes(ctx); err != nil {
		return nil, nil, storeError("velruna workset", err)
	}
	return c, cfg, nil
}

// worksetToolError classifies a workset failure into an actionable line: the
// client's message is kept, the hint names the tool call that resolves it.
func worksetToolError(op string, err error) error {
	switch {
	case errors.Is(err, client.ErrWorksetNotFound):
		return fmt.Errorf("%s: %w. No workset or alias matches that name; call velruna_workset_list to see the available names", op, err)
	case errors.Is(err, client.ErrWorksetExists):
		return fmt.Errorf("%s: %w. A workset with that name or alias already exists; pick another name or use velruna_workset_rename", op, err)
	case errors.Is(err, client.ErrWorksetLocked):
		return fmt.Errorf("%s: %w. Another author holds the entry; pass takeover=true, move it to todo first, or wait for the staleness window", op, err)
	case errors.Is(err, client.ErrWorksetConflict):
		return fmt.Errorf("%s: %w. Re-read with velruna_workset_show and retry; the phase matrix may forbid this move", op, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// worksetWindowHint documents the filter order once, in the answer, so an
// agent reading positions does not have to guess which view they index.
const worksetWindowHint = "filters apply in order: phase/live_only, then 1-based after/before positions in the filtered view, then limit"

func (s *mcpServer) worksetCreate(ctx context.Context, req *mcp.CallToolRequest, in worksetCreateInput) (*mcp.CallToolResult, worksetOutput, error) {
	const op = "velruna_workset_create"
	name, err := model.NormalizeName(in.Name)
	if err != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %v", op, err)
	}
	project := strings.TrimSpace(in.Project)
	if project == "" {
		project = model.DefaultProject
	}
	c, cfg, err := s.worksetToolClient(ctx)
	if err != nil {
		return nil, worksetOutput{}, err
	}
	if err := guardCheck(cfg.SecretGuard, in.ConfirmSecret, guardField{Name: "project", Text: project}); err != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %w", op, err)
	}
	labels := c.AuthorLabels(ctx)
	ws, err := c.CreateWorkset(ctx, client.WorksetCreate{Name: name, Project: project, By: labels.Author})
	if err != nil {
		return nil, worksetOutput{}, worksetToolError(op, err)
	}
	return nil, worksetOutput{
		Workset: worksetViewOf(ws, nil),
		Hint:    "add entries with velruna_workset_add(name, fact_ids); phase defaults to todo",
	}, nil
}

func (s *mcpServer) worksetList(ctx context.Context, req *mcp.CallToolRequest, in worksetListInput) (*mcp.CallToolResult, worksetOutput, error) {
	const op = "velruna_workset_list"
	project := strings.TrimSpace(in.Project)
	c, _, err := s.worksetToolClient(ctx)
	if err != nil {
		return nil, worksetOutput{}, err
	}
	sets, err := c.ListWorksets(ctx, project)
	if err != nil {
		return nil, worksetOutput{}, worksetToolError(op, err)
	}
	out := worksetOutput{Count: len(sets)}
	for _, ws := range sets {
		if ws == nil {
			continue
		}
		out.Worksets = append(out.Worksets, worksetSummaryView{
			Name:      ws.Name,
			Project:   ws.Project,
			Aliases:   ws.Aliases,
			Entries:   len(ws.Entries),
			Version:   ws.Version,
			UpdatedAt: formatWorksetTime(ws.UpdatedAt),
		})
	}
	if len(out.Worksets) == 0 {
		out.Hint = "no worksets; create one with velruna_workset_create"
		if project != "" {
			out.Hint = "no worksets for project " + project + "; omit project to list every project, or create one with velruna_workset_create"
		}
	}
	return nil, out, nil
}

func (s *mcpServer) worksetShow(ctx context.Context, req *mcp.CallToolRequest, in worksetShowInput) (*mcp.CallToolResult, worksetOutput, error) {
	const op = "velruna_workset_show"
	name, err := model.NormalizeName(in.Name)
	if err != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %v", op, err)
	}
	if in.After < 0 {
		return nil, worksetOutput{}, fmt.Errorf("%s: after must be >= 0 (got %d): positions are 1-based", op, in.After)
	}
	if in.Before < 0 {
		return nil, worksetOutput{}, fmt.Errorf("%s: before must be >= 0 (got %d): positions are 1-based", op, in.Before)
	}
	limit, err := worksetLimit(in.Limit)
	if err != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %v", op, err)
	}
	journalN, err := worksetJournalCount(in.Journal)
	if err != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %v", op, err)
	}
	phases, err := normalizePhases(in.Phase)
	if err != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %v", op, err)
	}
	c, _, err := s.worksetToolClient(ctx)
	if err != nil {
		return nil, worksetOutput{}, err
	}
	ws, err := c.GetWorkset(ctx, name)
	if err != nil {
		return nil, worksetOutput{}, worksetToolError(op, err)
	}
	states, err := worksetStates(ctx, c, ws.Entries)
	if err != nil {
		return nil, worksetOutput{}, worksetToolError(op, err)
	}
	filtered := worksetWindow(ws.Entries, states, phases, in.LiveOnly, in.After, in.Before, 0)
	rows := filtered
	omitted := 0
	if limit > 0 && len(filtered) > limit {
		rows = filtered[:limit]
		omitted = len(filtered) - limit
	}
	var journal []model.WorksetJournalEntry
	if journalN > 0 {
		journal, err = c.WorksetJournal(ctx, name, journalN)
		if err != nil {
			return nil, worksetOutput{}, worksetToolError(op, err)
		}
	}
	labels := c.AuthorLabels(ctx)
	allRows := worksetRows(ws.Entries, states)
	out := worksetOutput{
		Workset:          worksetViewOf(ws, rows),
		Cursors:          worksetCursorItems(ws.Cursors),
		Journal:          worksetJournalViews(journal),
		InProgress:       worksetPresenceItems(allRows, labels.Host),
		SelfVerified:     worksetSelfVerifiedIDs(allRows, journal),
		Hint:             worksetWindowHint,
		TruncatedEntries: omitted,
	}
	if omitted > 0 {
		out.Hint = fmt.Sprintf("%d entries are past the limit; raise limit to see them; %s", omitted, worksetWindowHint)
	} else if len(rows) == 0 {
		out.Hint = "no entries match the filters (or the workset is empty); " + worksetWindowHint
	}
	return nil, out, nil
}

func (s *mcpServer) worksetAdd(ctx context.Context, req *mcp.CallToolRequest, in worksetAddInput) (*mcp.CallToolResult, worksetOutput, error) {
	const op = "velruna_workset_add"
	name, ids, phase, err := worksetMutationArgs(op, in.Name, in.FactIDs, in.Phase, false)
	if err != nil {
		return nil, worksetOutput{}, err
	}
	c, cfg, err := s.worksetToolClient(ctx)
	if err != nil {
		return nil, worksetOutput{}, err
	}
	redactions, gErr := guardSanitize(cfg.SecretGuard, in.ConfirmSecret, textField("note", &in.Note))
	if gErr != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %w", op, gErr)
	}
	ws, err := c.AddEntries(ctx, name, client.WorksetAdd{FactIDs: ids, Note: in.Note, Phase: phase})
	if err != nil {
		return nil, worksetOutput{}, worksetToolError(op, err)
	}
	out, err := s.worksetOutputWithStates(ctx, c, ws)
	if err != nil {
		return nil, worksetOutput{}, worksetToolError(op, err)
	}
	out.Redactions = redactions
	return nil, out, nil
}

func (s *mcpServer) worksetRemove(ctx context.Context, req *mcp.CallToolRequest, in worksetRemoveInput) (*mcp.CallToolResult, worksetOutput, error) {
	const op = "velruna_workset_remove"
	name, err := model.NormalizeName(in.Name)
	if err != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %v", op, err)
	}
	ids, err := normalizeFactIDs(in.FactIDs)
	if err != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %v", op, err)
	}
	c, _, err := s.worksetToolClient(ctx)
	if err != nil {
		return nil, worksetOutput{}, err
	}
	labels := c.AuthorLabels(ctx)
	ws, err := c.RemoveEntries(ctx, name, ids, labels.Author)
	if err != nil {
		return nil, worksetOutput{}, worksetToolError(op, err)
	}
	out, err := s.worksetOutputWithStates(ctx, c, ws)
	if err != nil {
		return nil, worksetOutput{}, worksetToolError(op, err)
	}
	return nil, out, nil
}

func (s *mcpServer) worksetPhase(ctx context.Context, req *mcp.CallToolRequest, in worksetPhaseInput) (*mcp.CallToolResult, worksetOutput, error) {
	const op = "velruna_workset_phase"
	name, ids, to, err := worksetMutationArgs(op, in.Name, in.FactIDs, in.To, true)
	if err != nil {
		return nil, worksetOutput{}, err
	}
	c, cfg, err := s.worksetToolClient(ctx)
	if err != nil {
		return nil, worksetOutput{}, err
	}
	redactions, gErr := guardSanitize(cfg.SecretGuard, in.ConfirmSecret, textField("note", &in.Note))
	if gErr != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %w", op, gErr)
	}
	labels := c.AuthorLabels(ctx)
	ws, err := c.SetPhases(ctx, name, client.WorksetPhaseChange{
		FactIDs:  ids,
		To:       to,
		Note:     in.Note,
		By:       labels.Author,
		Takeover: in.Takeover,
	})
	if err != nil {
		return nil, worksetOutput{}, worksetToolError(op, err)
	}
	out, err := s.worksetOutputWithStates(ctx, c, ws)
	if err != nil {
		return nil, worksetOutput{}, worksetToolError(op, err)
	}
	out.Redactions = redactions
	return nil, out, nil
}

func (s *mcpServer) worksetRename(ctx context.Context, req *mcp.CallToolRequest, in worksetRenameInput) (*mcp.CallToolResult, worksetOutput, error) {
	const op = "velruna_workset_rename"
	oldName, err := model.NormalizeName(in.Name)
	if err != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %v", op, err)
	}
	newName, err := model.NormalizeName(in.NewName)
	if err != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %v", op, err)
	}
	if oldName == newName {
		return nil, worksetOutput{}, fmt.Errorf("%s: new_name equals the current name: nothing to rename", op)
	}
	c, _, err := s.worksetToolClient(ctx)
	if err != nil {
		return nil, worksetOutput{}, err
	}
	labels := c.AuthorLabels(ctx)
	ws, err := c.RenameWorkset(ctx, oldName, newName, labels.Author)
	if err != nil {
		return nil, worksetOutput{}, worksetToolError(op, err)
	}
	out, err := s.worksetOutputWithStates(ctx, c, ws)
	if err != nil {
		return nil, worksetOutput{}, worksetToolError(op, err)
	}
	return nil, out, nil
}

func (s *mcpServer) worksetDelete(ctx context.Context, req *mcp.CallToolRequest, in worksetDeleteInput) (*mcp.CallToolResult, worksetOutput, error) {
	const op = "velruna_workset_delete"
	name, err := model.NormalizeName(in.Name)
	if err != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %v", op, err)
	}
	if !in.Confirm {
		return nil, worksetOutput{}, fmt.Errorf("%s: workset %q is deleted for every reader and there is no undo; show that name to the user and re-call with confirm=true only after their approval", op, name)
	}
	c, _, err := s.worksetToolClient(ctx)
	if err != nil {
		return nil, worksetOutput{}, err
	}
	labels := c.AuthorLabels(ctx)
	if err := c.DeleteWorkset(ctx, name, labels.Author); err != nil {
		return nil, worksetOutput{}, worksetToolError(op, err)
	}
	return nil, worksetOutput{Name: name, Deleted: true}, nil
}

func (s *mcpServer) worksetCursor(ctx context.Context, req *mcp.CallToolRequest, in worksetCursorInput) (*mcp.CallToolResult, worksetOutput, error) {
	const op = "velruna_workset_cursor"
	name, err := model.NormalizeName(in.Name)
	if err != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %v", op, err)
	}
	change, err := worksetCursorArgsFor(in.At, in.Clear)
	if err != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %v", op, err)
	}
	c, cfg, err := s.worksetToolClient(ctx)
	if err != nil {
		return nil, worksetOutput{}, err
	}
	labels := c.AuthorLabels(ctx)
	author, host, err := worksetCursorIdentityFromInput(labels, in.Author, in.Host)
	if err != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %v", op, err)
	}
	key := model.CursorKey(author, host)
	if key == "" {
		return nil, worksetOutput{}, fmt.Errorf("%s: author is empty: a cursor names the reader it belongs to", op)
	}
	if !change.Mutate {
		ws, err := c.GetWorkset(ctx, name)
		if err != nil {
			return nil, worksetOutput{}, worksetToolError(op, err)
		}
		out := worksetOutput{
			Workset: worksetViewOf(ws, nil),
			Cursor:  worksetCursorViewOf(ws, author, host),
		}
		if out.Cursor == nil {
			out.Hint = "no cursor for " + key + "; pass at to set one"
		}
		return nil, out, nil
	}
	// The cursor key is stored identity: a guard finding refuses the write.
	if err := guardCheck(cfg.SecretGuard, in.ConfirmSecret,
		guardField{Name: "author", Text: author, HighOnly: true},
		guardField{Name: "host", Text: host, HighOnly: true}); err != nil {
		return nil, worksetOutput{}, fmt.Errorf("%s: %w", op, err)
	}
	ws, err := c.SetCursor(ctx, name, client.CursorInput{Author: author, Host: host, At: change.At, Clear: change.Clear})
	if err != nil {
		return nil, worksetOutput{}, worksetToolError(op, err)
	}
	out, err := s.worksetOutputWithStates(ctx, c, ws)
	if err != nil {
		return nil, worksetOutput{}, worksetToolError(op, err)
	}
	out.Cursor = worksetCursorViewOf(ws, author, host)
	if out.Cursor == nil {
		out.Hint = "cursor cleared for " + key
	}
	return nil, out, nil
}

// worksetMutationArgs validates the name, fact ids and (when required) the
// target phase shared by add/phase. An empty phase is only an error when the
// operation requires a target; add defaults to todo at the call site.
func worksetMutationArgs(op, rawName string, rawIDs []string, rawPhase string, phaseRequired bool) (string, []string, model.Phase, error) {
	name, err := model.NormalizeName(rawName)
	if err != nil {
		return "", nil, "", fmt.Errorf("%s: %v", op, err)
	}
	ids, err := normalizeFactIDs(rawIDs)
	if err != nil {
		return "", nil, "", fmt.Errorf("%s: %v", op, err)
	}
	phase, err := parsePhaseValue(rawPhase)
	if err != nil {
		return "", nil, "", fmt.Errorf("%s: %v", op, err)
	}
	if phase == "" {
		if phaseRequired {
			return "", nil, "", fmt.Errorf("%s: to is required (todo|in_progress|blocked|testing|done|cancelled)", op)
		}
		phase = model.PhaseTodo
	}
	return name, ids, phase, nil
}

// worksetOutputWithStates builds the mutation answer: the workset plus the
// entries with their current fact_state. The rows are capped at
// worksetMaxLimit and the omitted count is reported, so a large workset does
// not flood the answer (the full queue is a velruna_workset_show paged read).
func (s *mcpServer) worksetOutputWithStates(ctx context.Context, c *client.Client, ws *model.Workset) (worksetOutput, error) {
	entries := ws.Entries
	if len(entries) > worksetMaxLimit {
		entries = entries[:worksetMaxLimit]
	}
	states, err := worksetStates(ctx, c, entries)
	if err != nil {
		return worksetOutput{}, err
	}
	out := worksetOutput{
		Workset:          worksetViewOf(ws, worksetRows(entries, states)),
		TruncatedEntries: len(ws.Entries) - len(entries),
	}
	if out.TruncatedEntries > 0 {
		out.Hint = fmt.Sprintf("%d entries omitted (first %d of %d shown); call velruna_workset_show with after/before to page the rest",
			out.TruncatedEntries, len(entries), len(ws.Entries))
	}
	return out, nil
}

// worksetRows renders every entry with its 1-based index (no filters, no cap).
func worksetRows(entries []model.Entry, states map[string]string) []worksetRow {
	return worksetWindow(entries, states, nil, false, 0, 0, 0)
}

func worksetViewOf(ws *model.Workset, rows []worksetRow) *worksetView {
	v := &worksetView{
		ID:        ws.ID,
		Name:      ws.Name,
		Aliases:   ws.Aliases,
		Project:   ws.Project,
		Version:   ws.Version,
		CreatedBy: ws.CreatedBy,
		UpdatedBy: ws.UpdatedBy,
		UpdatedAt: formatWorksetTime(ws.UpdatedAt),
	}
	for _, r := range rows {
		v.Entries = append(v.Entries, worksetEntryView{
			Index:     r.Index,
			FactID:    r.Entry.FactID,
			FactState: r.State,
			Phase:     string(r.Entry.Phase),
			PhaseBy:   r.Entry.PhaseBy,
			PhaseAt:   formatWorksetTime(r.Entry.PhaseAt),
			Note:      r.Entry.Note,
		})
	}
	return v
}

func worksetJournalViews(entries []model.WorksetJournalEntry) []worksetJournalView {
	if len(entries) == 0 {
		return nil
	}
	out := make([]worksetJournalView, 0, len(entries))
	for _, e := range entries {
		out = append(out, worksetJournalView{
			Op:       e.Op,
			Takeover: e.Takeover,
			FactID:   e.FactID,
			From:     string(e.From),
			To:       string(e.To),
			Note:     e.Note,
			By:       e.By,
			At:       formatWorksetTime(e.At),
		})
	}
	return out
}

func worksetCursorViewOf(ws *model.Workset, author, host string) *worksetCursorView {
	key := model.CursorKey(author, host)
	cur, ok := ws.Cursors[key]
	if !ok {
		return nil
	}
	return &worksetCursorView{Key: key, At: cur.At, UpdatedAt: formatWorksetTime(cur.UpdatedAt)}
}

// worksetCursorIdentityFromInput resolves the cursor key halves for the MCP
// edge: explicit input wins, normalized like a config label so a control
// character cannot forge the rendered key.
func worksetCursorIdentityFromInput(labels client.AuthorInfo, authorOverride, hostOverride string) (string, string, error) {
	author := labels.Author
	if strings.TrimSpace(authorOverride) != "" {
		a, err := config.NormalizeLabel(authorOverride)
		if err != nil {
			return "", "", fmt.Errorf("author: %w", err)
		}
		author = a
	}
	host := labels.Host
	if strings.TrimSpace(hostOverride) != "" {
		h, err := config.NormalizeLabel(hostOverride)
		if err != nil {
			return "", "", fmt.Errorf("host: %w", err)
		}
		host = h
	}
	return author, host, nil
}

// formatWorksetTime renders a workset timestamp; the zero time is omitted so a
// pre-field document does not answer "0001-01-01".
func formatWorksetTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// registerWorksetTools registers the nine velruna_workset_* tools. It is called
// from runMCPServer (cmd/mcp.go) next to the other AddTool blocks.
func registerWorksetTools(srv *mcp.Server, s *mcpServer) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_workset_create",
		Description: "Create a named workset: a shared ordered queue of fact references " +
			"with a per-entry phase. The name is a slug (1..64 chars: a-z, 0-9, '_'); " +
			"names are globally unique and a project label (default global) groups them " +
			"for listing. Use velruna_workset_add to fill the queue.",
	}, s.worksetCreate)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_workset_list",
		Description: "List worksets, optionally limited to one project label. Each row " +
			"carries name, project, aliases, entry count, version and updated_at — enough " +
			"to pick the queue; velruna_workset_show reads the entries.",
	}, s.worksetList)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_workset_show",
		Description: "Show a workset's entries with fact_state (verified|obsolete|missing|unknown), " +
			"phase, phase owner and phase age. Filters apply in order: phase/live_only " +
			"first, then 1-based after/before positions in the filtered view (both " +
			"exclusive), then limit (default 50, cap 200). live_only keeps only verified " +
			"facts. The answer also carries the in_progress presence (author@host@index), " +
			"every reader's cursor positions (author@host@index, newest first), " +
			"self_verified done entries (only when the loaded journal contains a same-author " +
			"testing/in_progress row before the done), and the newest journal entries when " +
			"journal>0. Presence, cursors and self_verified describe the whole workset, not " +
			"the filtered window. A limit cut is reported as truncated_entries plus a hint.",
	}, s.worksetShow)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_workset_add",
		Description: "Add one or more fact ids to a workset. The initial phase defaults to " +
			"todo; done is accepted for reference entries (work already finished). " +
			"Duplicate ids in one call are collapsed.",
	}, s.worksetAdd)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_workset_remove",
		Description: "Remove one or more fact ids from a workset. Removing an entry drops " +
			"its phase and claim; the change is journaled.",
	}, s.worksetRemove)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_workset_phase",
		Description: "Move workset entries to another phase. The phase matrix and the " +
			"ownership lock are enforced: done is reachable only through testing, and an " +
			"in_progress/blocked claim held by another author is broken only by takeover, " +
			"by moving the entry to todo, or by staleness (workset_takeover_seconds). " +
			"note is the journaled reason.",
	}, s.worksetPhase)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_workset_rename",
		Description: "Rename a workset. The old slug remains resolvable as an alias, so " +
			"another agent's saved handle keeps working; entries and cursors are kept.",
	}, s.worksetRename)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_workset_delete",
		Description: "Delete a workset and its cursors. DESTRUCTIVE and shared: every reader " +
			"of the queue loses it and there is no undo. MCP has no interactive prompt: pass " +
			"confirm=true only after showing the name to the user; without it the call is refused.",
	}, s.worksetDelete)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "velruna_workset_cursor",
		Description: "Read or move this reader's cursor (key author@host). With neither at " +
			"nor clear it only reads the current position. A cursor is navigation state, " +
			"not a journaled change: movers do not append to the workset journal. author and " +
			"host are stored identity: the secret guard refuses a credential-like label.",
	}, s.worksetCursor)
}
