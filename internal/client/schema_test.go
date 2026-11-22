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

// A role that may write but not read mappings cannot produce a verdict: the
// mapping write (and its _meta stamp) must be skipped, and reads must work.
func TestEnsureIndexSkipsStampWhenMappingsUnreadable(t *testing.T) {
	var puts []string
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodHead && r.Path == "/velruna":
			return http.StatusOK, ""
		case r.Method == http.MethodGet && r.Path == "/velruna/_mapping":
			return http.StatusForbidden, `{"status":403,"error":"no permission"}`
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
		t.Errorf("stamped the schema without a verdict: %v", puts)
	}
}

// A 401 is a credential problem (rotated secret, security plugin restarting),
// not a role verdict: the check must retry instead of caching "unreadable"
// for the life of the process.
func TestEnsureIndexRetriesSchemaAfter401(t *testing.T) {
	calls := 0
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodHead && r.Path == "/velruna":
			return http.StatusOK, ""
		case r.Method == http.MethodGet && r.Path == "/velruna/_mapping":
			calls++
			if calls == 1 {
				return http.StatusUnauthorized, `{"status":401,"error":"unauthorized"}`
			}
			return http.StatusOK, `{"velruna":{"mappings":{"_meta":{"velruna_schema":99},"properties":{}}}}`
		}
		return http.StatusOK, `{}`
	})

	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("first EnsureIndex (401 must not be fatal): %v", err)
	}
	if err := c.EnsureIndex(context.Background()); err == nil || !strings.Contains(err.Error(), "newer velruna") {
		t.Fatalf("second EnsureIndex = %v, want the newer-schema refusal after the retry", err)
	}
}

// The lost-create-race path must adopt the index: a newer schema (created by
// another binary) must be refused before any mapping write.
func TestEnsureIndexCreateRaceChecksSchema(t *testing.T) {
	var puts []string
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodHead && r.Path == "/velruna":
			return http.StatusNotFound, `{"status":404,"error":"index_not_found_exception"}`
		case r.Method == http.MethodPut && r.Path == "/velruna":
			return http.StatusBadRequest, `{"error":{"type":"resource_already_exists_exception","reason":"index [velruna/uuid] already exists"},"status":400}`
		case r.Method == http.MethodGet && r.Path == "/velruna/_mapping":
			return http.StatusOK, `{"velruna":{"mappings":{"_meta":{"velruna_schema":99},"properties":{}}}}`
		case r.Method == http.MethodPut && r.Path == "/velruna/_mapping":
			puts = append(puts, r.Body)
			return http.StatusOK, `{"acknowledged":true}`
		}
		return http.StatusOK, `{}`
	})

	if err := c.EnsureIndex(context.Background()); err == nil {
		t.Fatal("the create-race path accepted a newer store")
	}
	if len(puts) != 0 {
		t.Errorf("wrote a mapping into a store it cannot understand: %v", puts)
	}
}

// A marker that exists but is not an integer (hand-edited or a future
// scheme) is an unknown verdict, not "unversioned": stamping 1 would
// overwrite a newer marker with a lie.
func TestEnsureIndexTreatsForeignMarkerAsUnknown(t *testing.T) {
	var puts []string
	c, _ := newFakeClient(t, schemaResponder(
		`{"velruna":{"mappings":{"_meta":{"velruna_schema":"2"},"properties":{}}}}`, &puts))

	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	if len(puts) != 0 {
		t.Errorf("stamped over a marker it could not read: %v", puts)
	}
}

// An unparsable mapping body is not a verdict either.
func TestEnsureIndexTreatsUnparsableMappingAsUnknown(t *testing.T) {
	var puts []string
	c, _ := newFakeClient(t, schemaResponder(`not json at all`, &puts))

	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	if len(puts) != 0 {
		t.Errorf("stamped without reading the version: %v", puts)
	}
}

// A 403 is a role verdict and is cached: a read-only role must not pay a
// refused GET on every tool call.
func TestEnsureIndexCachesUnreadableMappings(t *testing.T) {
	gets := 0
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodHead && r.Path == "/velruna":
			return http.StatusOK, ""
		case r.Method == http.MethodGet && r.Path == "/velruna/_mapping":
			gets++
			return http.StatusForbidden, `{"status":403,"error":"no permission"}`
		case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_cat/indices"):
			return http.StatusOK, `[]`
		case r.Method == http.MethodGet && r.Path == "/_nodes/stats/fs":
			return http.StatusOK, `{"nodes":{}}`
		}
		return http.StatusOK, `{}`
	})

	for i := 0; i < 3; i++ {
		if err := c.EnsureIndex(context.Background()); err != nil {
			t.Fatalf("EnsureIndex #%d: %v", i, err)
		}
	}
	if gets != 1 {
		t.Errorf("mapping GETs = %d, want 1 (403 is cached as unreadable)", gets)
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
