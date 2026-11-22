package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/NemoNobody/velruna/internal/config"
)

// mappingCase drives one mapping-reconcile scenario: the facts index exists,
// replicas are pinned (so the replica reconcile stays silent), and the mapping
// PUT answers with failFirst failures before succeeding.
func mappingCase(t *testing.T, failFirst int) (*Client, *fakeOpenSearch, *int) {
	t.Helper()
	puts := 0
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.IndexReplicas = 0
	}, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodHead && r.Path == "/velruna":
			return http.StatusOK, ""
		case r.Method == http.MethodGet && r.Path == "/velruna/_mapping":
			// The schema check reads this first; an unknown verdict would skip
			// the mapping reconcile under test.
			return http.StatusOK, `{}`
		case r.Method == http.MethodPut && r.Path == "/velruna/_mapping":
			puts++
			if puts <= failFirst {
				return http.StatusInternalServerError, `{"status":500,"error":"boom"}`
			}
			return http.StatusOK, `{"acknowledged":true}`
		}
		return http.StatusInternalServerError, `{"error":"unexpected request: ` + r.Method + ` ` + r.Path + `"}`
	})
	return c, fake, &puts
}

// TestReconcileMappingOncePerProcess pins the per-client half of the caching:
// repeated EnsureIndex calls on one client must not repeat the PUT. The
// cross-client claim (a fresh client per MCP tool call) is
// TestMappingReconcileIsProcessWide.
func TestReconcileMappingOncePerProcess(t *testing.T) {
	c, fake, _ := mappingCase(t, 0)
	for i := 0; i < 3; i++ {
		if err := c.EnsureIndex(context.Background()); err != nil {
			t.Fatalf("EnsureIndex #%d: %v", i, err)
		}
	}
	puts := fake.requestsFor(http.MethodPut, "/velruna/_mapping")
	if len(puts) != 1 {
		t.Fatalf("mapping puts = %d, want 1 (cached after success)", len(puts))
	}
	if !strings.Contains(puts[0].Body, `"keywords":{"type":"keyword"}`) {
		t.Errorf("mapping body lacks the keywords field: %s", puts[0].Body)
	}
}

// TestReconcileMappingRetriesAfterFailure pins the other half: a refused PUT
// must not mark the reconcile done, or a long-lived process keeps the degraded
// dynamic mapping until it restarts.
func TestReconcileMappingRetriesAfterFailure(t *testing.T) {
	c, fake, _ := mappingCase(t, 1)
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex must stay best effort: %v", err)
	}
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex retry: %v", err)
	}
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex after success: %v", err)
	}
	puts := fake.requestsFor(http.MethodPut, "/velruna/_mapping")
	if len(puts) != 2 {
		t.Fatalf("mapping puts = %d, want 2 (one failure, one retry, then cached)", len(puts))
	}
}

// TestMappingReconcileIsProcessWide: the MCP server builds a client per tool
// call, so the "already reconciled" claim has to outlive the client. Two
// clients for the same store must send the mapping PUT once.
func TestMappingReconcileIsProcessWide(t *testing.T) {
	c1, fake, _ := mappingCase(t, 0)
	if err := c1.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("first EnsureIndex: %v", err)
	}
	c2, err := NewClient(c1.cfg)
	if err != nil {
		t.Fatalf("second NewClient: %v", err)
	}
	if err := c2.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("second EnsureIndex: %v", err)
	}
	if puts := fake.requestsFor(http.MethodPut, "/velruna/_mapping"); len(puts) != 1 {
		t.Fatalf("mapping puts = %d, want 1 (the claim is per store, not per client)", len(puts))
	}
}

// TestMappingReconcileFailureIsReleasedForTheNextClient: a refused PUT must not
// leave the store marked done for the whole process, or the degraded dynamic
// mapping stays until a restart.
func TestMappingReconcileFailureIsReleasedForTheNextClient(t *testing.T) {
	c1, fake, _ := mappingCase(t, 1) // the first PUT fails
	if err := c1.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex must stay best effort: %v", err)
	}
	c2, err := NewClient(c1.cfg)
	if err != nil {
		t.Fatalf("second NewClient: %v", err)
	}
	if err := c2.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("second EnsureIndex: %v", err)
	}
	if puts := fake.requestsFor(http.MethodPut, "/velruna/_mapping"); len(puts) != 2 {
		t.Fatalf("mapping puts = %d, want 2 (the failed pass must not claim the store)", len(puts))
	}
}

// TestReconcileMapping403IsExternallyManaged: a read-only or DLS role cannot
// manage mappings. That is a final verdict, not a retryable failure — without
// this the gate stays open and every read pays a refused PUT.
func TestReconcileMapping403IsExternallyManaged(t *testing.T) {
	puts := 0
	c1, fake := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.IndexReplicas = 0
	}, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodHead && r.Path == "/velruna":
			return http.StatusOK, ""
		case r.Method == http.MethodGet && r.Path == "/velruna/_mapping":
			return http.StatusOK, `{}`
		case r.Method == http.MethodPut && r.Path == "/velruna/_mapping":
			puts++
			return http.StatusForbidden, `{"status":403,"error":"no permission"}`
		}
		return http.StatusInternalServerError, `{"error":"unexpected request: ` + r.Method + ` ` + r.Path + `"}`
	})
	if err := c1.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex must stay best effort: %v", err)
	}
	c2, err := NewClient(c1.cfg)
	if err != nil {
		t.Fatalf("second NewClient: %v", err)
	}
	if err := c2.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("second EnsureIndex: %v", err)
	}
	if got := len(fake.requestsFor(http.MethodPut, "/velruna/_mapping")); got != 1 {
		t.Fatalf("mapping puts = %d, want 1 (403 closes the gate: externally managed)", got)
	}
	if puts != 1 {
		t.Fatalf("handler saw %d mapping puts, want 1", puts)
	}
}

// TestMappingReconcileWaitsForTheFirstPass mirrors the journal-infra wait test:
// a second caller must not proceed while the first PUT is in flight, or a write
// could race the additive mapping and pin a field dynamically.
func TestMappingReconcileWaitsForTheFirstPass(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	c1, _ := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.IndexReplicas = 0
	}, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodHead && r.Path == "/velruna":
			return http.StatusOK, ""
		case r.Method == http.MethodPut && r.Path == "/velruna/_mapping":
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return http.StatusOK, `{"acknowledged":true}`
		}
		return http.StatusInternalServerError, `{"error":"unexpected"}`
	})
	c2, err := NewClient(c1.cfg)
	if err != nil {
		t.Fatalf("second NewClient: %v", err)
	}

	firstDone := make(chan struct{})
	go func() {
		_ = c1.reconcileMapping(context.Background())
		close(firstDone)
	}()
	<-started

	secondDone := make(chan struct{})
	go func() {
		_ = c2.reconcileMapping(context.Background())
		close(secondDone)
	}()
	select {
	case <-secondDone:
		t.Fatal("the second caller returned while the first mapping PUT was in flight (skip, not wait)")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-firstDone
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the second caller never returned after the first PUT finished")
	}
}
