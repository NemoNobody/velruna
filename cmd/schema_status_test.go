package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// The hint must survive the case that matters most: a store stamped by a
// newer binary maps "ok" for the fields it shares with this one, and
// swallowing the hint there hides the one verdict the operator needs.
func TestPrintSchemaHintSurvivesNewerStoreWithOkMapping(t *testing.T) {
	var b bytes.Buffer
	if err := printSchema(&b, statusReport{
		Schema:     "newer (v2)",
		Mapping:    "ok",
		SchemaHint: "this binary understands v1: upgrade velruna before using this store",
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "hint:") {
		t.Errorf("the hint was swallowed for a newer store: %q", b.String())
	}
}

// A fully clean store stays one short line with no hint.
func TestPrintSchemaCleanHasNoHint(t *testing.T) {
	var b bytes.Buffer
	if err := printSchema(&b, statusReport{Schema: "v1", Mapping: "ok"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "hint:") {
		t.Errorf("a clean store printed a hint: %q", b.String())
	}
}
