package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// incomingSupersedeRefs returns the keys of facts whose superseded_by points at
// key, the total count the store reported, and whether the list was truncated.
// An error is a store failure: the guard could not be checked.
func (c *Client) incomingSupersedeRefs(ctx context.Context, key string) ([]string, int, bool, error) {
	body, err := json.Marshal(map[string]interface{}{
		"query":   map[string]interface{}{"term": map[string]interface{}{"superseded_by": key}},
		"size":    incomingProbeLimit,
		"_source": []string{"key"},
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
	return contractErrorf("retire refused: %d %s still point at %q with superseded_by (%s): "+
		"repoint or retire them first — 'velruna links check' maps the graph", total, what, key, named)
}
