package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"

	"github.com/NemoNobody/velruna/internal/dashboards"
)

// IndexMappingProps reads the mapping properties of every concrete index
// behind a name or a pattern and merges them, first definition wins. The
// concrete indices are merged in sorted-name order: the store's response order
// is not defined, and a field list that depends on it would change between
// runs of the same generator against the same store.
//
// An absent index is an error, not an empty mapping: the generator keeps the
// old field list and must be able to say why.
func (c *Client) IndexMappingProps(ctx context.Context, index string) (map[string]dashboards.MappingField, []string, error) {
	path := "/" + url.PathEscape(index) + "/_mapping?ignore_unavailable=true&allow_no_indices=true"
	status, body, err := c.doSavedObjectRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("mapping of %s: %w", index, err)
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, nil, fmt.Errorf("index %s does not exist at %s", index, c.endpointLabel())
	case http.StatusForbidden:
		return nil, nil, fmt.Errorf("mapping of %s: HTTP 403 — the role cannot read mappings", index)
	default:
		return nil, nil, fmt.Errorf("mapping of %s: HTTP %d — %s", index, status, bodySnippet(body))
	}

	var res map[string]struct {
		Mappings struct {
			Properties map[string]dashboards.MappingField `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return nil, nil, fmt.Errorf("mapping of %s: cannot parse the response: %w", index, err)
	}
	names := make([]string, 0, len(res))
	for name := range res {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		// ignore_unavailable answers 200 with an empty body when the name or
		// pattern matches nothing: that is not "an index without fields", and
		// baking from it would replace a good list with just the meta fields.
		return nil, nil, fmt.Errorf("no index matches %s at %s", index, c.endpointLabel())
	}

	merged := map[string]dashboards.MappingField{}
	for _, name := range names {
		for field, spec := range res[name].Mappings.Properties {
			if _, seen := merged[field]; !seen {
				merged[field] = spec
			}
		}
	}
	return merged, names, nil
}
