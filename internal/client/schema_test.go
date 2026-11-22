package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// schemaResponder serves the minimum EnsureIndex touches for a store that
// already exists, with the mapping body supplied by the test.
func schemaResponder(mapping string, puts *[]string) func(recordedRequest) (int, string) {
	return func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodHead && r.Path == "/velruna":
			return http.StatusOK, ""
		case r.Method == http.MethodGet && r.Path == "/velruna/_mapping":
			return http.StatusOK, mapping
		case r.Method == http.MethodPut && r.Path == "/velruna/_mapping":
			*puts = append(*puts, r.Body)
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_cat/indices"):
			return http.StatusOK, `[]`
		case r.Method == http.MethodGet && r.Path == "/_nodes/stats/fs":
			return http.StatusOK, `{"nodes":{}}`
		}
		return http.StatusOK, `{}`
	}
}

// A store written by a newer binary must be refused before anything is written.
func TestEnsureIndexRefusesNewerSchema(t *testing.T) {
	var puts []string
	c, _ := newFakeClient(t, schemaResponder(
		`{"velruna":{"mappings":{"_meta":{"velruna_schema":99},"properties":{"key":{"type":"keyword"}}}}}`, &puts))

	err := c.EnsureIndex(context.Background())
	if err == nil {
		t.Fatal("EnsureIndex accepted a store with a newer schema")
	}
	if !strings.Contains(err.Error(), "newer velruna") {
		t.Errorf("error = %v, want it to name the schema mismatch", err)
	}
	if len(puts) != 0 {
		t.Errorf("wrote a mapping (%v) into a store it cannot understand", puts)
	}
}

// An unreadable mapping is not a verdict: the reconcile PUT also stamps the
// schema marker, so a blind stamp could downgrade a store written by a newer
// binary. The mapping write must be skipped, reads must still work.
func TestEnsureIndexDoesNotStampWhenSchemaUnreadable(t *testing.T) {
	var puts []string
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodHead && r.Path == "/velruna":
			return http.StatusOK, ""
		case r.Method == http.MethodGet && r.Path == "/velruna/_mapping":
			return http.StatusInternalServerError, `{"status":500,"error":"boom"}`
		case r.Method == http.MethodPut && r.Path == "/velruna/_mapping":
			puts = append(puts, r.Body)
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_cat/indices"):
			return http.StatusOK, `[]`
		case r.Method == http.MethodGet && r.Path == "/_nodes/stats/fs":
			return http.StatusOK, `{"nodes":{}}`
		}
		return http.StatusOK, `{}`
	})

	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	if len(puts) != 0 {
		t.Errorf("stamped the schema marker without reading the stored version: %v", puts)
	}
}

// An older store (no marker: every 0.8.x store) is adopted, and the mapping
// write carries the current schema marker.
func TestEnsureIndexAdoptsUnversionedStore(t *testing.T) {
	var puts []string
	c, _ := newFakeClient(t, schemaResponder(
		`{"velruna":{"mappings":{"properties":{"key":{"type":"keyword"}}}}}`, &puts))

	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	if len(puts) == 0 {
		t.Fatal("no mapping write happened: the schema marker would never be stamped")
	}
	if !strings.Contains(puts[len(puts)-1], `"velruna_schema":1`) {
		t.Errorf("mapping write carries no schema marker: %s", puts[len(puts)-1])
	}
}
