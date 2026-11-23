package model

import "time"

// Section is one heading-delimited chunk of a fetched document. Documents are
// stored as sections so an agent can pull a single part into context instead
// of the whole page — the point of the docs index is context economy.
type Section struct {
	Order   int    `json:"order"`
	Heading string `json:"heading,omitempty"`
	Text    string `json:"text"`
}

// ImageRef records an image the page references. The binary is deliberately
// not stored: the URL plus alt/title is enough to reopen the page, and alt
// text is what makes the picture searchable.
type ImageRef struct {
	URL   string `json:"url"`
	Alt   string `json:"alt,omitempty"`
	Title string `json:"title,omitempty"`
}

// Redaction records one credential span edited out of a stored snapshot:
// pattern name and chars only, never the matched text (the secret must not
// travel back into agent context).
type Redaction struct {
	Field   string `json:"field" jsonschema:"doc field the span was redacted from"`
	Pattern string `json:"pattern" jsonschema:"detector pattern name"`
	Chars   int    `json:"chars" jsonschema:"length of the redacted span"`
}

// AppendRedactionFields are the fields whose text APPENDS to another one
// (notes_append and a retire reason become new notes lines): a row for them
// describes a span that is still in the stored notes, so repeated appends
// accumulate rows instead of replacing each other.
func isAppendRedactionField(field string) bool {
	return field == "notes_append" || field == "reason"
}

// MergeRedactions combines the stored audit with the rows of a write.
//
// replacedFields names the fields this write OVERWRITES: their stored rows are
// dropped first, whether or not the new text lost anything — a field rewritten
// cleanly must not keep a row claiming a loss that no longer exists. Rows of
// untouched fields survive, and rows of APPEND fields (notes_append, reason)
// accumulate: the earlier text is still in the notes — unless the write
// overwrites notes itself, which erases every appended line folded into it.
// The new rows are added last, so a row for a replaced field describes the text
// that is stored now.
func MergeRedactions(stored []Redaction, replacedFields []string, add []Redaction) []Redaction {
	drop := make(map[string]bool, len(replacedFields)+len(add))
	for _, f := range replacedFields {
		drop[f] = true
	}
	// Appended lines and retire reasons are folded INTO notes: replacing notes
	// removes their text, so their rows go with it.
	if drop["notes"] {
		drop["notes_append"] = true
		drop["reason"] = true
	}
	for _, a := range add {
		if !isAppendRedactionField(a.Field) {
			drop[a.Field] = true
		}
	}
	out := make([]Redaction, 0, len(stored)+len(add))
	for _, r := range stored {
		if drop[r.Field] {
			continue
		}
		out = append(out, r)
	}
	return append(out, add...)
}

// Doc is one fetched web page snapshot: the raw body plus extracted,
// section-split text.
//
// It is deliberately NOT a Fact. A fact is a small verified claim with history
// and trust semantics; a doc is bulk research material with a TTL. Separate
// indices keep fact search cheap and keep page text out of the trusted-facts
// contract. The raw HTML is stored for re-extraction and for the cache vision
// (shared across machines), indexed=false: BM25 runs over extracted sections,
// never over markup.
type Doc struct {
	URL     string `json:"url"`
	URLHash string `json:"url_hash"`
	// FinalURL records the URL after redirects: audit for "the link moved".
	FinalURL string `json:"final_url,omitempty"`
	// ContentHash is sha256 over the page's kept text before the storage budget is
	// applied: the page's identity must not move when max_chars or the block size
	// does. It is recorded for dedup (same content under different URLs); its input
	// changed with the block model, so it is not an identity across builds.
	ContentHash string `json:"content_hash,omitempty"`
	Title       string `json:"title"`
	Project     string `json:"project,omitempty"`

	// ContentType is the server's media type (normalized: no parameters).
	// It routes the write-time secret guard: documentation types tolerate
	// medium-confidence token shapes in page content (see guardFetch), API
	// types do not.
	ContentType string `json:"content_type,omitempty"`

	FetchedAt time.Time `json:"fetched_at"`
	// ExpiresAt is the TTL boundary; the zero value means "never expires".
	// Past it the doc is stale: the web moved, the agent must re-fetch.
	// omitzero (not omitempty): time.Time is a struct, and omitempty does not
	// omit structs — a never-expiring snapshot used to store year 1, which a
	// range filter and a reader both then treated as an expiry in the past.
	ExpiresAt time.Time `json:"expires_at,omitzero"`

	// Size is the extracted text length in runes (what reads ship into
	// context); SizeRaw is the stored body in bytes (disk accounting).
	Size    int `json:"size"`
	SizeRaw int `json:"size_raw,omitempty"`
	// FullChars is the length of the kept text before the storage budget (chrome
	// blocks were already filtered out), so a reader can tell how much of the page
	// was stored.
	FullChars int `json:"full_chars,omitempty"`
	// Truncated marks a doc whose stored text is a prefix of the page: the
	// budget dropped whole blocks, and the loss is never silent.
	Truncated bool `json:"truncated,omitempty"`

	// FetchedBy is the hostname that performed the fetch and Via the route
	// (direct|proxy): audit in the shared-store, multi-machine setup.
	FetchedBy string `json:"fetched_by,omitempty"`
	Via       string `json:"via,omitempty"`

	// ETag and LastModified are the origin's HTTP validators captured with
	// this copy. A revalidation sends them and, on 304, only the timestamps
	// move. They describe the plain-fetch representation, so a copy captured
	// through the renderer carries none: the renderer's response is not what
	// the origin's validator covers.
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`

	// HTML is the raw body: stored for re-extraction, never indexed, and
	// excluded from every read path (_source excludes) so it cannot leak
	// into an agent's context by accident.
	HTML string `json:"html,omitempty"`

	Sections []Section  `json:"sections"`
	Images   []ImageRef `json:"images,omitempty"`

	// Redactions is the audit trail of the write-time secret guard: what was
	// edited out of the agent-visible fields (high-confidence shapes only).
	// The stored HTML is redacted too; it is never served to agents.
	Redactions []Redaction `json:"redactions,omitempty"`

	// GuardMediumIgnored counts the medium-confidence shapes the doc-route
	// guard left in the content on purpose (asset hashes, quoted SHAs).
	// Set only on a fresh fetch; surfaced in the outline so the operator
	// sees that leniency happened without re-scanning.
	GuardMediumIgnored int `json:"guard_medium_ignored,omitempty"`
}

// Stale reports whether the doc is past its TTL and must be re-fetched before
// being quoted. A doc without an expiry is never stale.
// Stale reports whether the document is past its TTL. A snapshot without an
// expiry never goes stale. The year-1 guard covers copies stored before
// omitzero existed: the zero time was serialized verbatim, and reading it back
// as a real past date would make every access re-fetch the page forever.
func (d *Doc) Stale(now time.Time) bool {
	if d.ExpiresAt.IsZero() || d.ExpiresAt.Year() <= 1 {
		return false
	}
	return now.After(d.ExpiresAt)
}
