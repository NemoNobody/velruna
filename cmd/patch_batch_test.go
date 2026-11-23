package cmd

import (
	"context"
	"strings"
	"testing"
)

func TestPatchBatchShapeLimits(t *testing.T) {
	s := &mcpServer{}
	if _, _, err := s.patchBatch(context.Background(), nil, patchBatchInput{}); err == nil {
		t.Error("empty batch must be refused")
	}
	many := make([]patchInput, maxAddBatchItems+1)
	if _, _, err := s.patchBatch(context.Background(), nil, patchBatchInput{Items: many}); err == nil {
		t.Error("over-cap batch must be refused")
	}
}

// Duplicate keys are as ambiguous in a patch batch as in an add batch (the
// second item patches the first item's fresh state); the comparison runs on
// the TRIMMED key — " k " and "k" collide loudly.
func TestPatchBatchDuplicateKeysRejected(t *testing.T) {
	s := &mcpServer{}
	in := patchBatchInput{Items: []patchInput{
		{Key: "k", NotesAppend: "note one"},
		{Key: " k ", NotesAppend: "note two"},
	}}
	if _, _, err := s.patchBatch(context.Background(), nil, in); err == nil ||
		!strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("want duplicate-key refusal, got %v", err)
	}
}

// The guard must run before any store contact: with an unreachable store and
// a sentinel note, the per-item errors must name the secret guard, not the
// store — proving rejection happens offline. A malformed item (unknown
// status) is a per-item shape error, never a batch failure.
func TestPatchBatchGuardRunsBeforeStore(t *testing.T) {
	t.Setenv("VELRUNA_SECRET_GUARD", "reject")
	t.Setenv("OPENSEARCH_URL", "http://127.0.0.1:9")
	s := &mcpServer{}
	in := patchBatchInput{Items: []patchInput{
		{Key: "k1", NotesAppend: "clean note"},
		{Key: "k2", NotesAppend: "leaked: " + guardSentinel},
		{Key: "k3", Status: "banana"},
	}}
	_, out, err := s.patchBatch(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("batch-level: %v", err)
	}
	if len(out.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(out.Items))
	}
	if !strings.Contains(out.Items[1].Error, "secret") {
		t.Errorf("guard item error = %q, want the secret guard to fire", out.Items[1].Error)
	}
	if !strings.Contains(out.Items[2].Error, "status must be") {
		t.Errorf("shape item error = %q, want the status validation to fire", out.Items[2].Error)
	}
	// The clean item reaches the (broken) store path: a store/config error,
	// not a guard or shape error — the batch itself never fails. Checking the
	// store half directly avoids the "velruna_status" hint substring.
	if !strings.Contains(out.Items[0].Error, "password is empty") {
		t.Errorf("clean item error = %q, want the store-path error", out.Items[0].Error)
	}
}
