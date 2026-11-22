package client

import (
	"context"
	"net/http"
	"testing"
)

func TestIndexMappingProps_MergesAndSorts(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodGet && r.Path == "/velruna/_mapping" {
			// Response order is not defined by the API; the merge must not
			// depend on it. "b" is defined in both: the first concrete index
			// in sorted-name order wins.
			return http.StatusOK, `{
				"velruna_b": {"mappings": {"properties": {"b": {"type": "long"}, "z": {"type": "keyword"}}}},
				"velruna_a": {"mappings": {"properties": {"a": {"type": "text"}, "b": {"type": "keyword"}}}}
			}`
		}
		t.Errorf("unexpected request %s %s", r.Method, r.Path)
		return http.StatusInternalServerError, `{"error":"unexpected request"}`
	})

	props, names, err := c.IndexMappingProps(context.Background(), "velruna")
	if err != nil {
		t.Fatalf("IndexMappingProps: %v", err)
	}
	if len(names) != 2 || names[0] != "velruna_a" || names[1] != "velruna_b" {
		t.Errorf("names = %v, want sorted concrete indices", names)
	}
	if props["b"].Type != "keyword" {
		t.Errorf("merge order wrong: b typed %q, want keyword from velruna_a", props["b"].Type)
	}
	if props["a"].Type != "text" || props["z"].Type != "keyword" {
		t.Errorf("merged props wrong: %+v", props)
	}
	if len(fake.requests) != 1 {
		t.Errorf("requests = %d, want one mapping read", len(fake.requests))
	}
}

func TestIndexMappingProps_AbsentIndexIsAnError(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusNotFound, `{"error":{"type":"index_not_found_exception"}}`
	})
	if _, _, err := c.IndexMappingProps(context.Background(), "velruna-history-*"); err == nil {
		t.Fatal("an absent index must be reported, not an empty mapping")
	}
}

// ignore_unavailable answers 200 with an empty object when a pattern matches
// nothing. Treating that as "an index with no fields" would bake a pattern down
// to just _index and silently erase a good list.
func TestIndexMappingProps_PatternMatchingNothingIsAnError(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusOK, `{}`
	})
	if _, _, err := c.IndexMappingProps(context.Background(), "velruna-history-*"); err == nil {
		t.Fatal("a pattern matching no index must be reported, not baked from")
	}
}
