package cmd

import (
	"context"
	"strings"
	"testing"
)

// TestMCPKeysShapeErrorsBeforeStore pins the keys guard shape: a prefix that
// normalizes to "" (" ", "*") with no usable --match is a caller error, not
// an availability error — no store contact, no "do not retry" advisory. A
// padded prefix is a real query and must proceed (normalized), not refuse.
func TestMCPKeysShapeErrorsBeforeStore(t *testing.T) {
	s := &mcpServer{}
	for _, prefix := range []string{"", " ", "*", "  *"} {
		_, _, err := s.keys(context.Background(), nil, keysInput{Prefix: prefix, Match: " "})
		if err == nil {
			t.Errorf("prefix %q with a whitespace-only match: want the empty-query refusal", prefix)
			continue
		}
		if !strings.Contains(err.Error(), "provide a key prefix") {
			t.Errorf("prefix %q: want the empty-query refusal, got %v", prefix, err)
		}
	}
	// A padded prefix is a query: it must proceed past the guard (the store
	// path is unreachable here — a store error, never the empty-query text).
	t.Setenv("OPENSEARCH_URL", "http://127.0.0.1:9")
	t.Setenv("VELRUNA_ALLOW_PLAINTEXT", "1")
	t.Setenv("OPENSEARCH_PASSWORD", "testpw")
	if _, _, err := s.keys(context.Background(), nil, keysInput{Prefix: " k "}); err == nil ||
		strings.Contains(err.Error(), "provide a key prefix") {
		t.Errorf("padded prefix must reach the store path, got %v", err)
	}
}

func TestAddBatchShapeLimits(t *testing.T) {
	s := &mcpServer{}
	if _, _, err := s.addBatch(context.Background(), nil, addBatchInput{}); err == nil {
		t.Error("empty batch must be refused")
	}
	many := make([]addInput, maxAddBatchItems+1)
	if _, _, err := s.addBatch(context.Background(), nil, addBatchInput{Items: many}); err == nil {
		t.Error("over-cap batch must be refused")
	}
}

// Duplicate keys inside one batch are ambiguous by construction (the second
// item corrects the first); the comparison runs on the TRIMMED key — what the
// store actually keys on — so " k " and "k" collide loudly.
func TestAddBatchDuplicateKeysRejected(t *testing.T) {
	s := &mcpServer{}
	in := addBatchInput{Items: []addInput{
		{Key: "k", Value: "v", Category: "config"},
		{Key: " k ", Value: "w", Category: "config"},
	}}
	if _, _, err := s.addBatch(context.Background(), nil, in); err == nil ||
		!strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("want duplicate-key refusal, got %v", err)
	}
}

// The guard must run before any store contact: with an unreachable store and
// a sentinel value, the per-item errors must name the secret guard, not the
// store — proving rejection happens offline.
func TestAddBatchGuardRunsBeforeStore(t *testing.T) {
	t.Setenv("VELRUNA_SECRET_GUARD", "reject")
	t.Setenv("OPENSEARCH_URL", "http://127.0.0.1:9")
	s := &mcpServer{}
	in := addBatchInput{Items: []addInput{
		{Key: "k1", Value: "clean value", Category: "config"},
		{Key: "k2", Value: "leaked: " + guardSentinel, Category: "config"},
	}}
	_, out, err := s.addBatch(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("batch-level: %v", err)
	}
	if len(out.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(out.Items))
	}
	if !strings.Contains(out.Items[1].Error, "secret") {
		t.Errorf("guard item error = %q, want the secret guard to fire", out.Items[1].Error)
	}
	// The clean item reaches the (broken) store path: a store error, not a
	// guard error — both paths are per item, the batch itself never fails.
	if strings.Contains(out.Items[0].Error, "secret") {
		t.Errorf("clean item error = %q, want a store error", out.Items[0].Error)
	}
}
