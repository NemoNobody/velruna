package cmd

import (
	"context"
	"strings"
	"testing"
)

// TestMCPSearchUnknownCategoryRefused (finding 130): an unknown category
// filter used to answer a silent zero forever (the term filter is exact on
// the stored canonical); the typo must be refused at the edge, before any
// store contact.
func TestMCPSearchUnknownCategoryRefused(t *testing.T) {
	s := &mcpServer{}
	_, _, err := s.search(context.Background(), nil, searchInput{Query: "q", Category: "Locaton"})
	if err == nil || !strings.Contains(err.Error(), "unknown category") {
		t.Errorf("unknown category must be refused, got %v", err)
	}
	// Canonical categories (case/whitespace variants) must pass the shape
	// check — they may still fail later on the unreachable store, but never
	// with the category refusal.
	for _, cat := range []string{" location ", "CONFIG", "recipe", " Rule "} {
		_, _, err := s.search(context.Background(), nil, searchInput{Query: "q", Category: cat})
		if err != nil && strings.Contains(err.Error(), "unknown category") {
			t.Errorf("canonical category %q must not be refused: %v", cat, err)
		}
	}
}

// TestMCPSearchKeywordShapedLikeTheWriteEdge pins the tag filter's shape rule:
// a multi-token tag can never match the stored canonical, so it is refused at
// the edge instead of answering a silent zero.
func TestMCPSearchKeywordShapedLikeTheWriteEdge(t *testing.T) {
	s := &mcpServer{}
	_, _, err := s.search(context.Background(), nil, searchInput{Query: "q", Keyword: "two words"})
	if err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("multi-token keyword must be refused, got %v", err)
	}
	if _, _, err := s.search(context.Background(), nil, searchInput{Query: "q", Keyword: "   "}); err == nil {
		t.Error("blank keyword must be refused")
	}
}

// TestMCPPruneNegativeKeepRefused (finding 132): a negative keep used to
// collapse silently into the configured default.
func TestMCPPruneNegativeKeepRefused(t *testing.T) {
	t.Setenv("OPENSEARCH_URL", "http://127.0.0.1:9")
	t.Setenv("VELRUNA_ALLOW_PLAINTEXT", "1")
	t.Setenv("OPENSEARCH_PASSWORD", "testpw")
	s := &mcpServer{}
	_, _, err := s.prune(context.Background(), nil, pruneInput{Key: "k", Keep: -5})
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("negative keep must be refused loudly, got %v", err)
	}
}

// TestMCPAddBatchItemErrorLabel (finding 131): a failed batch item is
// labeled as the batch item, not as the single-add tool.
func TestMCPAddBatchItemErrorLabel(t *testing.T) {
	t.Setenv("OPENSEARCH_URL", "http://127.0.0.1:9")
	t.Setenv("VELRUNA_ALLOW_PLAINTEXT", "1")
	t.Setenv("OPENSEARCH_PASSWORD", "testpw")
	s := &mcpServer{}
	// An item with an empty key fails addOne's shape check with the
	// "velruna_add: key is required" text: the batch must relabel it.
	_, out, err := s.addBatch(context.Background(), nil, addBatchInput{Items: []addInput{
		{Key: "ok:key", Value: "v", Category: "config"},
		{Key: " ", Value: "v", Category: "config"},
	}})
	if err != nil {
		// The batch shape check rejects a whitespace-only key before the
		// store: the refusal must carry the batch label, not "velruna_add".
		if !strings.Contains(err.Error(), "velruna_add_batch") {
			t.Errorf("batch-level refusal must say velruna_add_batch: %v", err)
		}
		return
	}
	for _, it := range out.Items {
		if it.Error != "" && !strings.Contains(it.Error, "velruna_add_batch item") {
			t.Errorf("item error must carry the batch item label: %q", it.Error)
		}
		if it.Error != "" && strings.HasPrefix(it.Error, "velruna_add:") {
			t.Errorf("item error must not masquerade as the single tool: %q", it.Error)
		}
	}
}
