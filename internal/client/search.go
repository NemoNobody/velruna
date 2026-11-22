package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/NemoNobody/velruna/internal/model"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// Search performs a keyword search, capped to limit hits. limit <= 0 means
// "no caller cap": the size is then NavSizeCap (1000), not the server's
// default of 10, which would silently truncate. Relevance order is
// OpenSearch's.
func (c *Client) Search(ctx context.Context, query, project, category, status, keyword string, limit int) ([]model.Fact, SearchMeta, error) {
	return c.search(ctx, buildSearchBody(query, project, category, status, keyword, limit))
}

// List returns all facts for a project whose key starts with prefix.
// An empty prefix lists the whole project. A prefix may carry the trailing
// '*' sugar ("state:*" == "state:"); a '*' anywhere else is rejected.
//
// sort: "updated" (newest activity first — the session-start use-case: what
// moved since last time) or "key" (stable navigation order). Facts written
// before updated_at existed sort last under "updated" (missing:_last).
func (c *Client) List(ctx context.Context, project, prefix, sort string) ([]model.Fact, SearchMeta, error) {
	p, err := NormalizePrefix(prefix)
	if err != nil {
		return nil, SearchMeta{}, err
	}
	return c.search(ctx, buildProjectBody(project, p, sort))
}

// Keys returns facts whose key matches a prefix and/or a substring, across
// all projects (project == "" keeps the scope global). It is the key-level
// navigation tool: prefix answers "what is under this subtree", match
// answers "which keys mention this token". At least one of them is required:
// an unbounded key query is a context flood, not a search.
//
// match is a Lucene keyword pattern: '*' and '?' inside it are wildcards.
func (c *Client) Keys(ctx context.Context, prefix, match, project, status string) ([]model.Fact, SearchMeta, error) {
	p, err := NormalizePrefix(prefix)
	if err != nil {
		return nil, SearchMeta{}, err
	}
	if p == "" && match == "" {
		return nil, SearchMeta{}, fmt.Errorf("keys: provide a key prefix and/or a match pattern")
	}
	return c.search(ctx, buildKeysBody(p, match, project, status))
}

// buildSearchBody builds the search query body for a keyword search.
// The result budget matters as soon as one index holds bulk text (docs):
// an uncapped search ships every hit into the caller's context.
func buildSearchBody(query string, project, category, status, keyword string, limit int) map[string]interface{} {
	filters := []map[string]interface{}{}
	if project != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"project": project}})
	}
	if category != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"category": category}})
	}
	if status != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"status": status}})
	}
	if keyword != "" {
		// A term filter on the keyword array: exact tag match, no analysis.
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"keywords": keyword}})
	}
	boolQuery := map[string]interface{}{
		"must": map[string]interface{}{
			// multi_match treats the query as text. query_string would parse
			// Lucene syntax, so keys like "proj:foo" or "foo(bar" would either
			// match the wrong field or fail with a 400.
			"multi_match": map[string]interface{}{
				"query":  query,
				"fields": []string{"value", "notes", "summary"},
			},
		},
	}
	if len(filters) > 0 {
		boolQuery["filter"] = filters
	}
	body := map[string]interface{}{
		"query": map[string]interface{}{"bool": boolQuery},
		// Exact totals, not the 10000-capped "gte" estimate: the truncation
		// advice ("of N matches") must be true at any store size, and the
		// counting cost is irrelevant at this store's scale.
		"track_total_hits": true,
	}
	if limit > 0 {
		body["size"] = limit
	} else {
		// "no cap": size is mandatory for predictability; the server default
		// of 10 silently truncates limit=0 queries.
		body["size"] = NavSizeCap
	}
	return body
}

// NavSizeCap is the explicit size for navigation queries (list/keys) and for
// "no cap" searches: without it OpenSearch returns 10 hits and callers
// silently receive an arbitrary subset.
const NavSizeCap = 1000

// buildProjectBody builds the search query body for a project lookup,
// optionally narrowed to a key prefix.
func buildProjectBody(project, prefix, sort string) map[string]interface{} {
	filters := []map[string]interface{}{
		{"term": map[string]interface{}{"project": project}},
	}
	if prefix != "" {
		filters = append(filters, map[string]interface{}{
			"prefix": map[string]interface{}{"key": prefix},
		})
	}
	body := map[string]interface{}{
		"query":            map[string]interface{}{"bool": map[string]interface{}{"filter": filters}},
		"size":             NavSizeCap,
		"track_total_hits": true,
	}
	if sort == "updated" {
		body["sort"] = []map[string]interface{}{{
			"updated_at": map[string]interface{}{"order": "desc", "missing": "_last"},
		}}
	} else {
		body["sort"] = []map[string]interface{}{{"key": map[string]interface{}{"order": "asc"}}}
	}
	return body
}

// buildKeysBody builds the key-navigation query: AND of the given filters,
// sorted by key so the navigation output is stable.
func buildKeysBody(prefix, match, project, status string) map[string]interface{} {
	filters := []map[string]interface{}{}
	if prefix != "" {
		filters = append(filters, map[string]interface{}{
			"prefix": map[string]interface{}{"key": prefix},
		})
	}
	if match != "" {
		filters = append(filters, map[string]interface{}{
			"wildcard": map[string]interface{}{"key": "*" + match + "*"},
		})
	}
	if project != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"project": project}})
	}
	if status != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"status": status}})
	}
	return map[string]interface{}{
		"query":            map[string]interface{}{"bool": map[string]interface{}{"filter": filters}},
		"sort":             []map[string]interface{}{{"key": map[string]interface{}{"order": "asc"}}},
		"size":             NavSizeCap,
		"track_total_hits": true,
	}
}

// SearchMeta carries query diagnostics that a facts-only return hides:
// Total is the cluster-side match count (the honest truncation signal: a
// page exactly filling the cap is a complete result), Skipped counts hits
// whose _source failed to decode — index corruption must never be silent.
type SearchMeta struct {
	Total   int
	Skipped int
}

// search executes a search and returns the matching facts with meta.
func (c *Client) search(ctx context.Context, body map[string]interface{}) ([]model.Fact, SearchMeta, error) {
	data, _ := json.Marshal(body)
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{c.cfg.IndexName},
		Body:    bytes.NewReader(data),
	})
	if err != nil {
		return nil, SearchMeta{}, fmt.Errorf("search: %w", err)
	}
	meta := SearchMeta{Total: resp.Hits.Total.Value}
	var results []model.Fact
	for _, hit := range resp.Hits.Hits {
		var f model.Fact
		if err := json.Unmarshal(hit.Source, &f); err != nil {
			meta.Skipped++
			continue
		}
		results = append(results, f)
	}
	return results, meta, nil
}
