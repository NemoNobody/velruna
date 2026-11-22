package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// The store keeps one kind of edge: superseded_by, "this record is replaced by
// that one". A retirement that deletes the target of such an edge leaves a
// reader walking into nothing, so retiring a pointed-at fact is refused while
// the edge exists. The check is advisory in the same way every other
// check-then-write guard is: a link created after this probe can still land,
// and a reader must therefore tolerate a dangling target — but the common
// accident (retire the old fact, orphan the newer one's link) is stopped at
// the write.

const (
	// maxIncomingLinks caps how many pointing keys a refusal names.
	maxIncomingLinks = 10
	// incomingProbeLimit asks for one more than the cap, so "and there are
	// more" can be reported without a second query.
	incomingProbeLimit = maxIncomingLinks + 1
)

// incomingSupersedeRefs returns the keys of VERIFIED facts whose superseded_by
// points at key, the total count the store reported, and whether the list was
// truncated. An error is a store failure: the guard could not be checked.
// Only verified referrers block: an obsolete referrer's edge is one readers
// are already told to tolerate, and counting it would make a retired link
// hold its target hostage forever (A->B: retire A, and B could never retire).
// The retiring fact's own self-link is excluded: edge and target are retired
// together, so it resolves itself.
func (c *Client) incomingSupersedeRefs(ctx context.Context, key string) ([]string, int, bool, error) {
	body, err := json.Marshal(map[string]interface{}{
		"query": map[string]interface{}{"bool": map[string]interface{}{
			"filter": []interface{}{
				map[string]interface{}{"term": map[string]interface{}{"superseded_by": key}},
				map[string]interface{}{"term": map[string]interface{}{"status": "verified"}},
			},
			"must_not": []interface{}{
				map[string]interface{}{"term": map[string]interface{}{"key": key}},
			},
		}},
		// Exact totals: the refusal states the count as fact, and the default
		// 10000-capped gte estimate would undercount silently — the same pin
		// the scan applies below.
		"track_total_hits": true,
		"size":             incomingProbeLimit,
		"_source":          []string{"key"},
	})
	if err != nil {
		return nil, 0, false, fmt.Errorf("build link probe: %w", err)
	}
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{c.cfg.IndexName},
		Body:    bytes.NewReader(body),
	})
	if err != nil {
		return nil, 0, false, fmt.Errorf("check incoming links for %q: %w", key, err)
	}
	keys := make([]string, 0, incomingProbeLimit)
	for _, hit := range resp.Hits.Hits {
		var doc struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(hit.Source, &doc); err != nil || doc.Key == "" {
			// A fact whose key cannot be read cannot be named in the refusal;
			// the edge still counts, so the count comes from the response.
			continue
		}
		keys = append(keys, doc.Key)
	}
	total := resp.Hits.Total.Value
	truncated := total > len(keys)
	if len(keys) > maxIncomingLinks {
		keys = keys[:maxIncomingLinks]
		truncated = true
	}
	return keys, total, truncated, nil
}

// refuseRetireWithIncoming refuses a retirement of key while other facts point
// at it. The message names the referrers so the operator can repoint them
// instead of guessing, and says which command maps the whole graph.
func (c *Client) refuseRetireWithIncoming(ctx context.Context, key string) error {
	keys, total, truncated, err := c.incomingSupersedeRefs(ctx, key)
	if err != nil {
		return err
	}
	if total == 0 {
		return nil
	}
	named := strings.Join(keys, ", ")
	if truncated || len(keys) < total {
		named = fmt.Sprintf("%s (+%d more)", named, total-len(keys))
	}
	what := "fact"
	if total != 1 {
		what = "facts"
	}
	return contractErrorf("retire refused: %d verified %s still point at %q with superseded_by (%s): "+
		"repoint or retire them first — 'velruna links check' maps the graph", total, what, key, named)
}

// The link graph has one edge kind and at most one outgoing edge per fact, so
// it is a forest of chains with cycles glued on. velruna links check walks it
// and reports the three ways an edge goes wrong: its target is gone, its
// target is retired, or the chain loops. Write-time guards try to prevent
// all three, but guards are advisory and older data predates them.

const (
	// LinkScanDefault caps how many superseded_by edges one check walks.
	LinkScanDefault = 1000
	// LinkScanMax bounds what --limit may raise the cap to: the check must
	// stay a bounded read even when asked generously.
	LinkScanMax = 10000
	// maxResultWindow is the index.max_result_window the store ships with
	// (buildIndexBody never raises it): a from+size beyond it is a raw 400,
	// so the scan window must never exceed it.
	maxResultWindow = 10000
	// mgetBatch caps one _mget round trip's ids.
	mgetBatch = 100
)

// LinkFinding is one broken edge. Problem is "dangling" (the target does not
// exist), "unverified" (the target is not verified) or "cycle" (the edge is
// part of a superseded_by loop).
type LinkFinding struct {
	Key     string
	Target  string
	Problem string
	Detail  string
}

// LinksReport is the outcome of one graph walk.
type LinksReport struct {
	Edges     int // edges inspected
	Fetched   int // hits the scan returned (audited + unreadable)
	Total     int // edges in the store (exact: track_total_hits)
	Skipped   int // fetched hits we could not read as an edge
	Truncated bool
	Findings  []LinkFinding // sorted by key, then problem
}

// CheckLinks walks the superseded_by graph in the facts index and reports
// every broken edge. limit bounds the scan (0 selects the default).
func (c *Client) CheckLinks(ctx context.Context, limit int) (*LinksReport, error) {
	if limit <= 0 {
		limit = LinkScanDefault
	}
	if limit > LinkScanMax {
		limit = LinkScanMax
	}
	// The +1 probe for "is there more" must not breach the result window:
	// beyond it the store answers a raw 400. Truncation is judged from the
	// exact total instead.
	size := limit + 1
	if size > maxResultWindow {
		size = maxResultWindow
	}
	body, err := json.Marshal(map[string]interface{}{
		"query": map[string]interface{}{"bool": map[string]interface{}{
			"filter": []interface{}{
				map[string]interface{}{"exists": map[string]interface{}{"field": "superseded_by"}},
			},
			// An empty link is not an edge: without this filter such docs
			// eat scan slots and blur the truncation signal. The store
			// flattens keyword arrays, so a legacy superseded_by carrying
			// "" alongside a real edge is excluded here too — a known
			// limitation of any keyword filter, not a lookup miss.
			"must_not": []interface{}{
				map[string]interface{}{"term": map[string]interface{}{"superseded_by": ""}},
			},
		}},
		"size": size,
		// Exact totals: the truncated suffix states one as fact, and the
		// default 10000-capped gte estimate would undercount silently.
		"track_total_hits": true,
		// A deterministic window: without a sort the inspected edge set
		// (and with it the findings) shifts between runs.
		"sort":    []interface{}{map[string]interface{}{"key": "asc"}},
		"_source": []string{"key", "superseded_by"},
	})
	if err != nil {
		return nil, fmt.Errorf("build link scan: %w", err)
	}
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{c.cfg.IndexName},
		Body:    bytes.NewReader(body),
	})
	if err != nil {
		return nil, fmt.Errorf("scan superseded_by links: %w", err)
	}
	type edge struct {
		key    string
		target string
	}
	var edges []edge
	skipped := 0
	for _, hit := range resp.Hits.Hits {
		var doc struct {
			Key          string `json:"key"`
			SupersededBy string `json:"superseded_by"`
		}
		if err := json.Unmarshal(hit.Source, &doc); err != nil || doc.Key == "" || doc.SupersededBy == "" {
			// An edge we cannot name cannot be reported — count it as not
			// inspected rather than let it vanish silently.
			skipped++
			continue
		}
		edges = append(edges, edge{key: doc.Key, target: doc.SupersededBy})
	}
	report := &LinksReport{Total: resp.Hits.Total.Value, Fetched: len(resp.Hits.Hits), Skipped: skipped}
	if len(edges) > limit {
		edges = edges[:limit]
	}
	report.Edges = len(edges)
	// The walk is incomplete whenever the store counted edges we did not
	// inspect — beyond the window, beyond --limit, or fetched but unreadable.
	report.Truncated = report.Total > report.Edges

	// Resolve every distinct target once: found + status decide two of the
	// three findings; the third (cycle) needs only the edge set.
	next := make(map[string]string, len(edges))
	var targets []string
	seenTarget := make(map[string]bool, len(edges))
	for _, e := range edges {
		next[e.key] = e.target
		if !seenTarget[e.target] {
			seenTarget[e.target] = true
			targets = append(targets, e.target)
		}
	}
	status, err := c.targetStatuses(ctx, targets)
	if err != nil {
		return nil, err
	}

	for _, e := range edges {
		st, found := status[e.target]
		if !found {
			report.Findings = append(report.Findings, LinkFinding{
				Key: e.key, Target: e.target, Problem: "dangling",
				Detail: "target does not exist",
			})
		} else if st != "verified" {
			report.Findings = append(report.Findings, LinkFinding{
				Key: e.key, Target: e.target, Problem: "unverified",
				Detail: "target is " + st,
			})
		}
	}

	// Cycle pass: each node has at most one outgoing edge, so a walk from any
	// node either leaves the edge set or repeats a node already on the walk.
	// Reported edges are marked so a loop names its whole ring once.
	state := make(map[string]int, len(next)) // 0 unvisited, 1 on current walk, 2 done
	cycled := make(map[string]bool, len(next))
	for start := range next {
		if state[start] != 0 {
			continue
		}
		path := []string{}
		onPath := make(map[string]int)
		cur := start
		for {
			if _, hasEdge := next[cur]; !hasEdge {
				break // the chain leaves the link set: no cycle through it
			}
			if state[cur] == 2 {
				break // already walked to a conclusion elsewhere
			}
			if at, ok := onPath[cur]; ok {
				for _, k := range path[at:] {
					cycled[k] = true
				}
				break
			}
			onPath[cur] = len(path)
			path = append(path, cur)
			state[cur] = 1
			cur = next[cur]
		}
		for _, k := range path {
			state[k] = 2
		}
	}
	for _, e := range edges {
		if cycled[e.key] {
			report.Findings = append(report.Findings, LinkFinding{
				Key: e.key, Target: e.target, Problem: "cycle",
				Detail: "edge is part of a superseded_by loop",
			})
		}
	}

	sort.Slice(report.Findings, func(i, j int) bool {
		if report.Findings[i].Key != report.Findings[j].Key {
			return report.Findings[i].Key < report.Findings[j].Key
		}
		return report.Findings[i].Problem < report.Findings[j].Problem
	})
	return report, nil
}

// targetStatuses resolves each id to its stored status; a missing entry in the
// result map means the document does not exist. A store-level failure is an
// error, never a silent "missing".
func (c *Client) targetStatuses(ctx context.Context, ids []string) (map[string]string, error) {
	statuses := make(map[string]string, len(ids))
	for start := 0; start < len(ids); start += mgetBatch {
		end := start + mgetBatch
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		docs := make([]map[string]any, 0, len(batch))
		for _, id := range batch {
			// _source per entry: the top-level _source is not accepted by
			// _mget (only docs and ids are).
			docs = append(docs, map[string]any{"_id": id, "_source": []string{"status"}})
		}
		body, err := json.Marshal(map[string]interface{}{
			"docs": docs,
		})
		if err != nil {
			return nil, fmt.Errorf("build target probe: %w", err)
		}
		resp, err := c.api.MGet(ctx, opensearchapi.MGetReq{
			Index: c.cfg.IndexName,
			Body:  bytes.NewReader(body),
		})
		if err != nil {
			return nil, fmt.Errorf("probe link targets: %w", err)
		}
		for _, d := range resp.Docs {
			if d.Error != nil {
				// A per-document error is not "missing": an unavailable shard
				// must fail the whole check rather than fake a dangling link.
				return nil, fmt.Errorf("probe link target %q: %s", d.ID, d.Error.Reason)
			}
			if !d.Found {
				continue
			}
			var src struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(d.Source, &src); err != nil {
				return nil, fmt.Errorf("read status of link target %q: %w", d.ID, err)
			}
			statuses[d.ID] = src.Status
		}
	}
	return statuses, nil
}
