package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/model"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- page snapshots (docs index) ---

type fetchDocInput struct {
	URL     string `json:"url" jsonschema:"page url to fetch; a missing scheme becomes https, utm_* parameters are stripped"`
	Project string `json:"project,omitempty" jsonschema:"tag the document with this project"`
	Refresh bool   `json:"refresh,omitempty" jsonschema:"force a fresh download even when a stored snapshot exists"`

	// MaxChars and BlockChars override the configured extraction limits for
	// this call (fetch_doc_chars, fetch_block_chars). Pointers: 0 is a
	// meaningful MaxChars (store the whole page).
	MaxChars   *int `json:"max_chars,omitempty" jsonschema:"characters to store; 0 = the whole document; default: fetch_doc_chars"`
	BlockChars *int `json:"block_chars,omitempty" jsonschema:"target block size in runes (1000..64000); default: fetch_block_chars"`

	// ConfirmSecret bypasses a guard hit at secret_guard=ask, the same
	// contract as velruna_add: page text is untrusted and can quote
	// credential-like strings.
	ConfirmSecret bool `json:"confirm_secret,omitempty" jsonschema:"confirm a guard hit is a false positive (secret_guard=ask only); set only after the user approved the exact finding"`
}

type docSectionOutline struct {
	Order   int    `json:"order" jsonschema:"section number for velruna_get_doc"`
	Heading string `json:"heading"`
	Chars   int    `json:"chars" jsonschema:"text length in runes"`
}

func newDocOutline(d *model.Doc) []docSectionOutline {
	out := make([]docSectionOutline, 0, len(d.Sections))
	for _, s := range d.Sections {
		out = append(out, docSectionOutline{Order: s.Order, Heading: s.Heading, Chars: len([]rune(s.Text))})
	}
	return out
}

type fetchDocOutput struct {
	Hash      string `json:"hash,omitempty" jsonschema:"document id for velruna_get_doc"`
	URL       string `json:"url,omitempty"`
	Title     string `json:"title,omitempty"`
	Project   string `json:"project,omitempty"`
	FetchedAt string `json:"fetched_at,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Stale     bool   `json:"stale,omitempty" jsonschema:"past TTL: re-fetch with velruna_fetch_doc before quoting"`
	Cached    bool   `json:"cached,omitempty" jsonschema:"true when the stored snapshot was returned (no new content was downloaded)"`
	// Revalidated names the 304 path: the origin confirmed the stored copy
	// with HTTP validators, so the content is unchanged and only fetched_at /
	// expires_at moved. It is a network request, not a cache hit — an agent
	// asking "did the page change?" needs the difference.
	Revalidated bool                `json:"revalidated,omitempty" jsonschema:"true when the origin answered 304 Not Modified and the stored snapshot was kept (timestamps moved; a larger max_chars may have re-extracted more text from the saved html)"`
	Truncated   bool                `json:"truncated,omitempty" jsonschema:"extraction hit a limit: the stored text is a prefix"`
	Via         string              `json:"via,omitempty" jsonschema:"route that served the fetch when not direct: proxy | obscura (audit; obscura means the content was browser-rendered)"`
	Sections    []docSectionOutline `json:"sections,omitempty" jsonschema:"outline: pull a section with velruna_get_doc(hash, section)"`

	// Chars is the stored text and CharsTotal the text extracted before the
	// budget: N of M tells the agent how much of the page it has. The outline
	// is capped at outlineRows lines; SectionsHidden counts the rest.
	Chars          int    `json:"chars_stored,omitempty" jsonschema:"characters stored in this snapshot (runes)"`
	CharsTotal     int    `json:"chars_total,omitempty" jsonschema:"characters extracted from the whole page before the storage budget (runes)"`
	SectionsHidden int    `json:"sections_hidden,omitempty" jsonschema:"outline blocks beyond the listed ones (hint summarizes their range)"`
	Hint           string `json:"hint,omitempty" jsonschema:"navigation advice: more blocks, or a truncated snapshot and how to store the rest"`

	// Redactions: high-confidence credential spans edited out of the stored
	// snapshot (pattern/field/size, no preview): the secret never entered the
	// store, and the answer is transparent about the surgery.
	Redactions []redactionView `json:"redactions,omitempty" jsonschema:"credential spans redacted out of the stored snapshot (no preview)"`
	Warning    string          `json:"warning,omitempty" jsonschema:"set when redactions happened: the stored text is edited, quote from it with that in mind"`

	// GuardMediumIgnored surfaces the doc-route leniency: medium-confidence
	// token shapes left in documentation content on purpose (asset hashes,
	// quoted SHAs). High-confidence secrets are redacted, never ignored.
	GuardMediumIgnored int `json:"guard_medium_ignored,omitempty" jsonschema:"medium-confidence shapes left in documentation content on purpose (documentation route only); high-confidence secrets are always redacted"`

	// GitHub local-clone resolution: the answer is a file on this machine.
	Local bool   `json:"local,omitempty" jsonschema:"true when the url resolved to a file in a local git clone: Path is the file; nothing was fetched"`
	Path  string `json:"path,omitempty" jsonschema:"local clone file path when local=true"`
}

// viaOut hides the default route (direct) from the contract: it carries
// information only when the fetch did NOT go straight to the target.
func viaOut(doc *model.Doc) string {
	if doc.Via == client.ViaDirect {
		return ""
	}
	return doc.Via
}

type redactionView struct {
	Field   string `json:"field" jsonschema:"doc field the span was redacted from"`
	Pattern string `json:"pattern" jsonschema:"detector pattern name"`
	Chars   int    `json:"chars" jsonschema:"length of the redacted span"`
}

func (s *mcpServer) fetchDoc(ctx context.Context, req *mcp.CallToolRequest, in fetchDocInput) (*mcp.CallToolResult, fetchDocOutput, error) {
	// Validate before any store contact: a bad url, a denied host, a robots
	// verdict is the caller's problem, not an availability problem. Config is
	// local, and normalization needs fetch_allow_private.
	cfg, err := s.loadConfig()
	if err != nil {
		return nil, fetchDocOutput{}, err
	}
	rawURL, err := client.NormalizeURL(in.URL, cfg.FetchAllowPrivate)
	if err != nil {
		return nil, fetchDocOutput{}, err
	}
	if err := client.HostAllowed(cfg, rawURL); err != nil {
		return nil, fetchDocOutput{}, err
	}
	if p, ok := client.GitHubLocal(cfg, rawURL); ok {
		// The content is in a local clone: hand out the path. No fetch, no
		// store write: the repo mapping is machine-specific and must never
		// enter the shared index.
		return nil, fetchDocOutput{Local: true, Path: p}, nil
	}
	// The project tag is agent-supplied text that lands in a shared index,
	// and the cache-hit path re-tags a stored doc WITHOUT the content
	// guard (guardFetch gates only the fresh path): scan the tag here.
	// Trim first: stored labels are trimmed, and the guard must see the
	// value that will be stored.
	in.Project = strings.TrimSpace(in.Project)
	if err := guardCheck(cfg.SecretGuard, in.ConfirmSecret,
		guardField{Name: "project", Text: in.Project}); err != nil {
		return nil, fetchDocOutput{}, err
	}
	limits, err := resolveExtractLimits(cfg, in.BlockChars, in.MaxChars)
	if err != nil {
		return nil, fetchDocOutput{}, err
	}
	c, err := client.NewClient(cfg) // FetchDoc ensures the docs index itself
	if err != nil {
		return nil, fetchDocOutput{}, storeError("velruna_fetch_doc", err)
	}
	doc, cached, revalidated, err := c.FetchDocWithin(ctx, rawURL, in.Project, in.Refresh, limits,
		func(d *model.Doc) ([]model.Redaction, error) {
			// Guard after extraction, before the doc is finalized.
			return guardFetch(d, cfg.SecretGuard, in.ConfirmSecret)
		})
	if err != nil {
		return nil, fetchDocOutput{}, err
	}
	outline := newDocOutline(doc)
	shown := len(outline)
	if shown > outlineRows {
		shown = outlineRows
	}
	hint := outlineSummary(doc, shown)
	if doc.Truncated && (doc.FullChars == 0 || doc.Size < doc.FullChars) {
		// A truncation with size == full_chars came from dropped image refs (or
		// a single oversized block): more text is not available, so the budget
		// advice would send the caller after nothing.
		truncHint := "stored a prefix of the page; call again with max_chars=0 to store the whole page"
		if doc.FullChars > 0 {
			truncHint = fmt.Sprintf("stored the first %d of %d chars; call again with max_chars=0 to store the whole page", doc.Size, doc.FullChars)
		}
		if hint != "" {
			hint += " "
		}
		hint += truncHint
	}
	now := time.Now().UTC()
	out := fetchDocOutput{
		Hash:           doc.URLHash,
		URL:            doc.URL,
		Title:          doc.Title,
		Project:        doc.Project,
		FetchedAt:      rfc3339OrEmpty(doc.FetchedAt),
		ExpiresAt:      rfc3339OrEmpty(doc.ExpiresAt),
		Stale:          doc.Stale(now),
		Cached:         cached,
		Revalidated:    revalidated,
		Truncated:      doc.Truncated,
		Via:            viaOut(doc),
		Sections:       outline[:shown],
		Chars:          doc.Size,
		CharsTotal:     doc.FullChars,
		SectionsHidden: len(outline) - shown,
		Hint:           hint,

		GuardMediumIgnored: doc.GuardMediumIgnored,
	}
	if len(doc.Redactions) > 0 {
		out.Redactions = make([]redactionView, 0, len(doc.Redactions))
		for _, r := range doc.Redactions {
			out.Redactions = append(out.Redactions, redactionView{Field: r.Field, Pattern: r.Pattern, Chars: r.Chars})
		}
		out.Warning = fmt.Sprintf("%d high-confidence credential span(s) were redacted out of the stored snapshot; the page is stored edited (the secret never entered the store)", len(doc.Redactions))
	}
	return nil, out, nil
}

type getDocInput struct {
	Hash string `json:"hash" jsonschema:"document id from velruna_fetch_doc/velruna_search_docs"`

	// Section is a pointer: 0 is a valid section number, and its absence
	// must mean "outline only".
	Section *int `json:"section,omitempty" jsonschema:"section number to return in full (from the outline); omit for the outline only"`
	// Blocks is how many consecutive blocks a section read returns: 1 keeps
	// the answer small, more saves round-trips.
	Blocks *int `json:"blocks,omitempty" jsonschema:"how many consecutive blocks to return (default 1, max 10)"`
	Full   bool `json:"full,omitempty" jsonschema:"return every section (bloats the context; prefer one section)"`
}

type getDocOutput struct {
	Found     bool   `json:"found" jsonschema:"false when no stored doc has this hash"`
	Hash      string `json:"hash,omitempty"`
	URL       string `json:"url,omitempty"`
	Title     string `json:"title,omitempty"`
	Stale     bool   `json:"stale,omitempty" jsonschema:"past TTL: refresh with velruna_fetch_doc before quoting"`
	Truncated bool   `json:"truncated,omitempty" jsonschema:"extraction hit a limit: the stored text is a prefix"`
	// GuardMediumIgnored surfaces the doc-route leniency recorded at fetch
	// time: the leniency must be visible on every read path, not only the
	// outline printed by velruna_fetch_doc.
	GuardMediumIgnored int                 `json:"guard_medium_ignored,omitempty" jsonschema:"medium-confidence shapes left in documentation content on purpose (documentation route only); high-confidence secrets are always redacted"`
	Sections           []docSectionOutline `json:"sections,omitempty" jsonschema:"outline: heading + length per section"`

	// The outline is capped at outlineRows lines, like the fetch answer:
	// SectionsHidden counts the rest and Hint summarizes their range.
	SectionsHidden int    `json:"sections_hidden,omitempty" jsonschema:"outline blocks beyond the listed ones (hint summarizes their range)"`
	Hint           string `json:"hint,omitempty" jsonschema:"how to continue: more outline blocks, or a capped read"`

	SectionText string `json:"section_text,omitempty" jsonschema:"the text of the requested section"`
	// BlocksIncluded counts the blocks in SectionText; Next is where a capped
	// read continues (absent when the read reached the end).
	BlocksIncluded int    `json:"blocks_included,omitempty" jsonschema:"blocks returned in section_text"`
	FullText       string `json:"full_text,omitempty" jsonschema:"every section, delimited by headers (full=true)"`
	Next           *int   `json:"next,omitempty" jsonschema:"section to continue from when the read was capped"`
}

// mergeHint joins two navigation hints without losing either: one answer
// carries at most one hint field.
func mergeHint(existing, extra string) string {
	switch {
	case existing == "":
		return extra
	case extra == "":
		return existing
	}
	return existing + "; " + extra
}

func (s *mcpServer) getDoc(ctx context.Context, req *mcp.CallToolRequest, in getDocInput) (*mcp.CallToolResult, getDocOutput, error) {
	in.Hash = strings.TrimSpace(in.Hash)
	if in.Hash == "" {
		return nil, getDocOutput{}, errors.New("velruna_get_doc: hash is required")
	}
	// Validate before the read: readDocBlocks skips orders below start, so a
	// negative section would silently return block 0.
	if in.Section != nil && *in.Section < 0 {
		return nil, getDocOutput{}, fmt.Errorf("velruna_get_doc: section must be 0 or greater, got %d", *in.Section)
	}
	if in.Blocks != nil && (*in.Blocks < 1 || *in.Blocks > maxReadBlocks) {
		return nil, getDocOutput{}, fmt.Errorf("velruna_get_doc: blocks must be between 1 and %d, got %d", maxReadBlocks, *in.Blocks)
	}
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, getDocOutput{}, storeError("velruna_get_doc", err)
	}
	doc, err := c.GetDocByHash(ctx, in.Hash)
	if errors.Is(err, client.ErrNoDoc) {
		return nil, getDocOutput{Found: false}, nil
	}
	if err != nil {
		return nil, getDocOutput{}, storeError("velruna_get_doc", err)
	}
	// The outline is capped like the fetch answer; the hint carries the range
	// of the blocks it left out and is merged with the read hints below.
	outline := newDocOutline(doc)
	shown := len(outline)
	if shown > outlineRows {
		shown = outlineRows
	}
	now := time.Now().UTC()
	out := getDocOutput{
		Found:              true,
		Hash:               doc.URLHash,
		URL:                doc.URL,
		Title:              doc.Title,
		Stale:              doc.Stale(now),
		Truncated:          doc.Truncated,
		GuardMediumIgnored: doc.GuardMediumIgnored,
		Sections:           outline[:shown],
		SectionsHidden:     len(outline) - shown,
		Hint:               outlineSummary(doc, shown),
	}
	if in.Full {
		text, _, next := readDocBlocks(doc, 0, len(doc.Sections), docReadBudget)
		out.FullText = text
		if next >= 0 {
			out.Next = &next
			out.Hint = mergeHint(out.Hint, fmt.Sprintf("full text capped at %d runes; continue with section=%d", docReadBudget, next))
		}
	}
	if in.Section != nil && !in.Full {
		want := 1
		if in.Blocks != nil {
			want = *in.Blocks
		}
		text, included, next := readDocBlocks(doc, *in.Section, want, docReadBudget)
		if included == 0 {
			return nil, getDocOutput{}, fmt.Errorf("velruna_get_doc: doc %s has no section %d (the outline lists %d sections)",
				doc.URLHash, *in.Section, len(doc.Sections))
		}
		out.SectionText = text
		out.BlocksIncluded = included
		if next >= 0 {
			out.Next = &next
			out.Hint = mergeHint(out.Hint, fmt.Sprintf("continue with section=%d", next))
		}
		return nil, out, nil
	}
	return nil, out, nil
}

type searchDocsInput struct {
	Query   string `json:"query" jsonschema:"text to search in stored document sections (the raw html is never searched)"`
	Project string `json:"project,omitempty" jsonschema:"limit to documents tagged with this project"`
	Limit   int    `json:"limit,omitempty" jsonschema:"max documents to return (default 5, cap 20): the context budget for search results"`
}

// docSectionHit is the section that matched the query: get_doc(section=order)
// returns the full text around the match.
type docSectionHit struct {
	Order   int    `json:"order" jsonschema:"section number: velruna_get_doc(hash, section=order) reads the full section"`
	Heading string `json:"heading,omitempty" jsonschema:"section heading (table of contents)"`
	Snippet string `json:"snippet" jsonschema:"match-centered text (±160 runes), whitespace-collapsed"`
}

type docHitView struct {
	Hash    string `json:"hash" jsonschema:"document id for velruna_get_doc"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Project string `json:"project,omitempty" jsonschema:"project tag of the stored document"`
	Stale   bool   `json:"stale" jsonschema:"past TTL: refresh with velruna_fetch_doc before quoting"`
	// Section is the matched section (all-terms scan in Go); Snippet is the
	// doc-level fallback (first section) when no section is identified.
	Section *docSectionHit `json:"section,omitempty" jsonschema:"the section that matched the query"`
	Snippet string         `json:"snippet,omitempty" jsonschema:"first section, whitespace-collapsed and capped (fallback when no section matched)"`
}

type docsHitsOutput struct {
	Count     int          `json:"count"`
	Docs      []docHitView `json:"docs"`
	Truncated bool         `json:"truncated,omitempty" jsonschema:"true when the store matched more documents than this page (cap 20): narrow the query before concluding there is nothing more; absent means complete"`
	Skipped   int          `json:"skipped,omitempty" jsonschema:"stored documents whose content failed to decode (index corruption): report to the user"`
}

func (s *mcpServer) searchDocs(ctx context.Context, req *mcp.CallToolRequest, in searchDocsInput) (*mcp.CallToolResult, docsHitsOutput, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, docsHitsOutput{}, errors.New("velruna_search_docs: query is required")
	}
	in.Project = strings.TrimSpace(in.Project)
	limit := in.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > client.DocsSearchCap {
		limit = client.DocsSearchCap
	}
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, docsHitsOutput{}, storeError("velruna_search_docs", err)
	}
	docs, meta, err := c.SearchDocs(ctx, in.Query, in.Project, limit)
	if err != nil {
		return nil, docsHitsOutput{}, storeError("velruna_search_docs", err)
	}
	now := time.Now().UTC()
	terms := queryTerms(in.Query)
	hits := make([]docHitView, 0, len(docs))
	for _, d := range docs {
		h := docHitView{Hash: d.URLHash, URL: d.URL, Title: d.Title, Project: d.Project, Stale: d.Stale(now)}
		if i := pickMatchedSection(d.Sections, terms); i >= 0 {
			sec := d.Sections[i]
			h.Section = &docSectionHit{Order: sec.Order, Heading: sec.Heading, Snippet: centerSnippet(sec.Text, terms)}
		} else if len(d.Sections) > 0 {
			h.Snippet = snippet(d.Sections[0].Text, 300)
		}
		hits = append(hits, h)
	}
	return nil, docsHitsOutput{Count: len(hits), Truncated: meta.Total > len(hits)+meta.Skipped, Skipped: meta.Skipped, Docs: hits}, nil
}

// queryTerms approximates OpenSearch's standard tokenizer: lowercased
// alphanumeric runs of length >= 3, deduplicated. Stopwords need no list:
// the all-terms section scan punishes them uniformly.
func queryTerms(query string) []string {
	var out []string
	seen := map[string]bool{}
	var cur []rune
	flush := func() {
		w := string(cur)
		cur = cur[:0]
		if len(w) >= 3 && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	for _, r := range strings.ToLower(query) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// pickMatchedSection picks the section containing ALL query terms — a direct
// match beats any frequency-based pick (frequency overvalues long sections).
// Fallback: the section with the most distinct terms; -1 = no match.
// This is the cheap Go-side scan the design chose over nested inner_hits.
func pickMatchedSection(sections []model.Section, terms []string) int {
	if len(terms) == 0 {
		return -1
	}
	best, bestHits := -1, 0
	for i, sec := range sections {
		lower := strings.ToLower(sec.Text)
		hits := 0
		for _, t := range terms {
			if strings.Contains(lower, t) {
				hits++
			}
		}
		if hits == len(terms) {
			return i
		}
		if hits > bestHits {
			best, bestHits = i, hits
		}
	}
	if bestHits > 0 {
		return best
	}
	return -1
}

// centerSnippet windows the text around the first term hit (±160 runes) so
// the answer is visible in the snippet, not just the section opening.
func centerSnippet(text string, terms []string) string {
	collapsed := []rune(strings.Join(strings.Fields(text), " "))
	lower := strings.ToLower(string(collapsed))
	pos := -1
	for _, t := range terms {
		if p := strings.Index(lower, t); p >= 0 && (pos < 0 || p < pos) {
			pos = p
		}
	}
	const win = 160
	if pos < 0 {
		return snippet(text, 2*win)
	}
	// pos is a byte index into the lowercased string; rune-align it onto the
	// collapsed text (case folding can shift byte lengths; snippets tolerate).
	r := utf8.RuneCountInString(lower[:pos])
	start, end := r-win, r+win
	if start < 0 {
		start = 0
	}
	if end > len(collapsed) {
		end = len(collapsed)
	}
	out := string(collapsed[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(collapsed) {
		out += "…"
	}
	return out
}
