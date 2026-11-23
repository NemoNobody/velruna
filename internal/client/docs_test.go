package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/model"
)

// TestFetchDocNeverShrinksUnchangedSnapshot: a smaller budget must not replace a
// larger stored copy of the same page. Two agents fetching one URL with
// different max_chars would otherwise race the text away.
func TestFetchDocNeverShrinksUnchangedSnapshot(t *testing.T) {
	ft := newFetchTarget(t)
	indexExists := false
	var stored string
	respond := func(r recordedRequest) (int, string) {
		switch {
		case r.Path == "/velruna-docs" && r.Method == "HEAD":
			if indexExists {
				return 200, ""
			}
			return 404, ""
		case r.Path == "/velruna-docs" && r.Method == "PUT":
			indexExists = true
			return 200, `{"acknowledged":true}`
		case strings.HasPrefix(r.Path, "/velruna-docs/_update/"):
			return 200, `{"result":"updated"}`
		case strings.HasPrefix(r.Path, "/velruna-docs/_doc/"):
			if r.Method == "GET" {
				if stored == "" {
					return 404, "{}"
				}
				return 200, `{"found":true,"_source":` + stored + `}`
			}
			stored = r.Body
			return 200, `{"result":"created"}`
		}
		return 404, "{}"
	}
	c, _ := newFakeClientCfg(t, docsMutate, respond)
	ctx := context.Background()
	url := ft.srv.URL + "/page"

	big, _, _, err := c.FetchDocWithin(ctx, url, "", false, ExtractLimits{BlockChars: 8000, MaxChars: 0}, nil)
	if err != nil {
		t.Fatalf("FetchDocWithin(full): %v", err)
	}
	// A refresh with a tiny budget re-downloads the same page and extracts less.
	small, _, _, err := c.FetchDocWithin(ctx, url, "", true, ExtractLimits{BlockChars: 8000, MaxChars: 5}, nil)
	if err != nil {
		t.Fatalf("FetchDocWithin(smaller budget): %v", err)
	}
	if small.Size != big.Size {
		t.Errorf("returned %d chars, want the stored %d (a fetch must not shrink)", small.Size, big.Size)
	}
	var storedDoc model.Doc
	if err := json.Unmarshal([]byte(stored), &storedDoc); err != nil {
		t.Fatalf("stored doc not decodable: %v", err)
	}
	if storedDoc.Size != big.Size {
		t.Errorf("store now holds %d chars, want %d", storedDoc.Size, big.Size)
	}
}

func docsMutate(cfg *config.Config) {
	cfg.DocsIndexName = "velruna-docs"
	cfg.FetchAllowPrivate = true // fetch targets are httptest loopback servers
}

func TestBuildDocsIndexBodyHTMLNotIndexed(t *testing.T) {
	props := buildDocsIndexBody(0)["mappings"].(map[string]interface{})["properties"].(map[string]interface{})
	htmlProp, ok := props["html"].(map[string]interface{})
	if !ok || htmlProp["index"] != false {
		t.Errorf("html mapping = %+v, want type text with index:false", props["html"])
	}
	for _, field := range []string{"sections", "images", "final_url", "content_hash", "size_raw"} {
		if _, ok := props[field]; !ok {
			t.Errorf("mapping lacks %q", field)
		}
	}
}

func TestBuildDocSearchBody(t *testing.T) {
	b := buildDocSearchBody("q", "", 0)
	if size, ok := b["size"].(int); !ok || size != 5 {
		t.Errorf("default size = %v, want 5", b["size"])
	}
	src := b["_source"].(map[string]interface{})
	ex, ok := src["excludes"].([]string)
	if !ok || len(ex) != 1 || ex[0] != "html" {
		t.Errorf("_source excludes = %v, want [html] — the snapshot must never ride along", src["excludes"])
	}
	must := b["query"].(map[string]interface{})["bool"].(map[string]interface{})["must"].(map[string]interface{})
	fields := must["multi_match"].(map[string]interface{})["fields"].([]string)
	if len(fields) != 2 || fields[0] != "title" || fields[1] != "sections.text" {
		t.Errorf("fields = %v, want [title sections.text]", fields)
	}

	capped := buildDocSearchBody("q", "proj", 100)
	if size := capped["size"].(int); size != 20 {
		t.Errorf("capped size = %d, want 20", size)
	}
	filters := capped["query"].(map[string]interface{})["bool"].(map[string]interface{})["filter"].([]map[string]interface{})
	if len(filters) != 1 {
		t.Fatalf("filters = %d, want 1 (project)", len(filters))
	}
	term := filters[0]["term"].(map[string]interface{})
	if term["project"] != "proj" {
		t.Errorf("project filter = %v", term)
	}
}

func TestGetDocByHashExcludesHTML(t *testing.T) {
	c, fake := newFakeClientCfg(t, docsMutate, func(r recordedRequest) (int, string) {
		if r.Path == "/velruna-docs/_doc/h1" && r.Method == "GET" {
			return 200, `{"found":true,"_source":{"url":"https://x","url_hash":"h1","title":"T","sections":[{"order":0,"heading":"A","text":"a"}]}}`
		}
		return 404, "{}"
	})
	doc, err := c.GetDocByHash(context.Background(), "h1")
	if err != nil {
		t.Fatalf("GetDocByHash: %v", err)
	}
	if doc.Title != "T" || len(doc.Sections) != 1 {
		t.Errorf("doc = %+v", doc)
	}
	reqs := fake.requestsFor("GET", "/velruna-docs/_doc/h1")
	if len(reqs) != 1 {
		t.Fatalf("GET requests = %d, want 1", len(reqs))
	}
	if got := reqs[0].Query.Get("_source_excludes"); got != "html" {
		t.Errorf("_source_excludes = %q, want html", got)
	}
}

func TestPutDocRefreshesAndWait(t *testing.T) {
	c, fake := newFakeClientCfg(t, docsMutate, func(r recordedRequest) (int, string) {
		return 200, `{"result":"created"}`
	})
	doc := &model.Doc{URLHash: "h1", Sections: []model.Section{{Order: 0, Text: "a"}}, HTML: "<h1>x</h1>"}
	if err := c.PutDoc(context.Background(), doc); err != nil {
		t.Fatalf("PutDoc: %v", err)
	}
	var rec *recordedRequest
	for i, r := range fake.requests {
		if strings.HasPrefix(r.Path, "/velruna-docs/_doc/h1") {
			rec = &fake.requests[i]
			break
		}
	}
	if rec == nil {
		t.Fatalf("no write to /velruna-docs/_doc/h1; requests: %+v", fake.requests)
	}
	if rec.Method != "PUT" {
		t.Errorf("method = %s, want PUT", rec.Method)
	}
	if rec.Query.Get("refresh") != "wait_for" {
		t.Errorf("refresh = %q, want wait_for (read-after-write)", rec.Query.Get("refresh"))
	}
	if !strings.Contains(rec.Body, `"html"`) || !strings.Contains(rec.Body, "sections") {
		t.Errorf("stored body lacks html/sections: %s", rec.Body)
	}
}

func TestSearchDocsContract(t *testing.T) {
	c, fake := newFakeClientCfg(t, docsMutate, func(r recordedRequest) (int, string) {
		return 200, `{"hits":{"hits":[]}}`
	})
	docs, _, err := c.SearchDocs(context.Background(), "q", "proj", 3)
	if err != nil {
		t.Fatalf("SearchDocs: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("docs = %d, want 0", len(docs))
	}
	reqs := fake.requestsFor("POST", "/velruna-docs/_search")
	if len(reqs) != 1 {
		t.Fatalf("search requests = %d, want 1", len(reqs))
	}
	for _, want := range []string{`"size":3`, `"excludes":["html"]`, `"project":"proj"`} {
		if !strings.Contains(reqs[0].Body, want) {
			t.Errorf("body lacks %s: %s", want, reqs[0].Body)
		}
	}
}

// The docs index is created lazily on the first fetch: searching before any
// fetch must return an empty result, not an index_not_found error.
func TestSearchDocsMissingIndexIsEmpty(t *testing.T) {
	c, _ := newFakeClientCfg(t, docsMutate, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodPost && r.Path == "/velruna-docs/_search" {
			return http.StatusNotFound, `{"status":404,"error":{"type":"index_not_found_exception","reason":"no such index [velruna-docs]"}}`
		}
		return http.StatusInternalServerError, "unexpected " + r.Path
	})
	docs, meta, err := c.SearchDocs(context.Background(), "q", "proj", 3)
	if err != nil {
		t.Fatalf("SearchDocs on missing index: %v", err)
	}
	if len(docs) != 0 || meta.Total != 0 {
		t.Errorf("docs=%d total=%d, want 0/0", len(docs), meta.Total)
	}
}

func TestFetchDocStoresSnapshotThenServesCache(t *testing.T) {
	ft := newFetchTarget(t)
	var (
		indexExists bool
		stored      string
	)
	respond := func(r recordedRequest) (int, string) {
		switch {
		case r.Path == "/velruna-docs" && r.Method == "HEAD":
			if indexExists {
				return 200, ""
			}
			return 404, ""
		case r.Path == "/velruna-docs" && r.Method == "PUT":
			indexExists = true
			return 200, `{"acknowledged":true}`
		case strings.HasPrefix(r.Path, "/velruna-docs/_doc/"):
			if r.Method == "GET" {
				if stored == "" {
					return 404, "{}"
				}
				return 200, `{"found":true,"_source":` + stored + `}`
			}
			stored = r.Body
			return 200, `{"result":"created"}`
		}
		return 404, "{}"
	}
	c, fake := newFakeClientCfg(t, docsMutate, respond)
	ctx := context.Background()
	url := ft.srv.URL + "/page"

	doc1, cached, _, err := c.FetchDoc(ctx, url, "proj", false, nil)
	if err != nil {
		t.Fatalf("FetchDoc(first): %v", err)
	}
	if cached {
		t.Error("first fetch must not be cached")
	}
	if stored == "" {
		t.Fatal("nothing was stored")
	}
	var storedDoc model.Doc
	if err := json.Unmarshal([]byte(stored), &storedDoc); err != nil {
		t.Fatalf("stored doc not decodable: %v", err)
	}
	if storedDoc.HTML == "" {
		t.Error("the snapshot html must be stored (re-extraction, shared cache)")
	}
	if storedDoc.Project != "proj" || storedDoc.ContentHash == "" || storedDoc.Via != ViaDirect {
		t.Errorf("stored audit fields: %+v", storedDoc)
	}

	doc2, cached2, _, err := c.FetchDoc(ctx, url, "proj", false, nil)
	if err != nil {
		t.Fatalf("FetchDoc(second): %v", err)
	}
	if !cached2 {
		t.Error("second fetch must hit the cache")
	}
	if doc2.Title != doc1.Title {
		t.Errorf("cached title = %q, want %q", doc2.Title, doc1.Title)
	}
	puts := 0
	for _, r := range fake.requests {
		if r.Method == "PUT" && strings.HasPrefix(r.Path, "/velruna-docs/_doc/") {
			puts++
		}
	}
	if puts != 1 {
		t.Errorf("writes = %d, want 1: a cache hit must not rewrite", puts)
	}
}

// TestFetchDocRevalidatesWithETag pins the cache-refresh contract end to end:
// a forced refresh of a stored page sends its ETag, the origin answers 304, and
// the stored snapshot is kept, only the cache metadata moved — no body
// download, no content change.
func TestFetchDocRevalidatesWithETag(t *testing.T) {
	ft := newFetchTarget(t)
	var (
		indexExists bool
		stored      string
	)
	respond := func(r recordedRequest) (int, string) {
		switch {
		case r.Path == "/velruna-docs" && r.Method == "HEAD":
			if indexExists {
				return 200, ""
			}
			return 404, ""
		case r.Path == "/velruna-docs" && r.Method == "PUT":
			indexExists = true
			return 200, `{"acknowledged":true}`
		case r.Method == "POST" && strings.Contains(r.Path, "/velruna-docs/_update/"):
			return 200, `{"result":"updated"}`
		case strings.HasPrefix(r.Path, "/velruna-docs/_doc/"):
			if r.Method == "GET" {
				if stored == "" {
					return 404, "{}"
				}
				return 200, `{"found":true,"_source":` + stored + `}`
			}
			stored = r.Body
			return 200, `{"result":"created"}`
		}
		return 404, "{}"
	}
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) {
		docsMutate(cfg)
		// The TTL drives the expires_at bump the 304 path performs.
		cfg.DocsTTLDays = 90
	}, respond)
	ctx := context.Background()
	url := ft.srv.URL + "/etag"

	doc1, cached, revalidated, err := c.FetchDoc(ctx, url, "proj", false, nil)
	if err != nil {
		t.Fatalf("FetchDoc(first): %v", err)
	}
	if cached || revalidated {
		t.Fatalf("first fetch = (cached=%v revalidated=%v), want both false", cached, revalidated)
	}
	if doc1.ETag != `"v1"` {
		t.Fatalf("stored ETag = %q, want the origin's \"v1\"", doc1.ETag)
	}

	doc2, cached2, revalidated2, err := c.FetchDoc(ctx, url, "proj", true, nil)
	if err != nil {
		t.Fatalf("FetchDoc(refresh): %v", err)
	}
	if !cached2 || !revalidated2 {
		t.Fatalf("forced refresh = (cached=%v revalidated=%v), want both true on a 304", cached2, revalidated2)
	}
	if doc2.ContentHash != doc1.ContentHash {
		t.Errorf("content changed across a 304: %q -> %q", doc1.ContentHash, doc2.ContentHash)
	}
	if doc2.FetchedAt.Before(doc1.FetchedAt) {
		t.Errorf("fetched_at went backwards: %v -> %v", doc1.FetchedAt, doc2.FetchedAt)
	}
	if !doc2.ExpiresAt.After(doc1.ExpiresAt) {
		t.Errorf("expires_at must move forward on revalidation: %v -> %v", doc1.ExpiresAt, doc2.ExpiresAt)
	}
	// A 304 must move the metadata with a partial update, never rewrite the
	// document: the copy was read with _source excludes (html), so a full
	// PutDoc would silently delete the stored raw body.
	puts, updates := 0, 0
	var updateBody string
	for _, r := range fake.requests {
		if r.Method == "PUT" && strings.HasPrefix(r.Path, "/velruna-docs/_doc/") {
			puts++
		}
		if r.Method == "POST" && strings.Contains(r.Path, "/velruna-docs/_update/") {
			updates++
			updateBody = r.Body
		}
	}
	if puts != 1 {
		t.Errorf("doc puts = %d, want 1 (the initial store); a 304 must not re-store the doc", puts)
	}
	if updates != 1 {
		t.Fatalf("doc updates = %d, want 1 (the metadata bump)", updates)
	}
	if !strings.Contains(updateBody, `"fetched_at"`) || !strings.Contains(updateBody, `"etag":"\"v2\""`) {
		t.Errorf("metadata update must carry the timestamps and the rotated validator: %s", updateBody)
	}
	if doc2.ETag != `"v2"` {
		t.Errorf("returned ETag = %q, want the rotated \"v2\" persisted", doc2.ETag)
	}
	if len(ft.inm) != 2 || ft.inm[1] != `"v1"` {
		t.Errorf("If-None-Match = %q, want the second request conditional", ft.inm)
	}
}

// TestFetchDocRevalidationClearsExpiryWhenTTLDisabled pins config-change
// coherence: with docs_ttl_days=0 a fresh snapshot stores no expiry, so a
// revalidated copy must lose its old one too — otherwise the store keeps
// calling the doc stale while the caller is told it never expires.
func TestFetchDocRevalidationClearsExpiryWhenTTLDisabled(t *testing.T) {
	ft := newFetchTarget(t)
	var (
		updateBody string
		puts       int
	)
	stored := `{"url":"` + ft.srv.URL + `/etag","title":"t",` +
		`"fetched_at":"2020-01-01T00:00:00Z","expires_at":"2021-01-01T00:00:00Z","etag":"\"v1\""}`
	respond := func(r recordedRequest) (int, string) {
		switch {
		case r.Path == "/velruna-docs" && r.Method == "HEAD":
			return 200, ""
		case r.Method == "GET" && strings.HasPrefix(r.Path, "/velruna-docs/_doc/"):
			return 200, `{"found":true,"_source":` + stored + `}`
		case r.Method == "POST" && strings.Contains(r.Path, "/velruna-docs/_update/"):
			updateBody = r.Body
			return 200, `{"result":"updated"}`
		case r.Method == "PUT" && strings.HasPrefix(r.Path, "/velruna-docs/_doc/"):
			puts++
			return 200, `{"result":"created"}`
		}
		return 404, "{}"
	}
	c, _ := newFakeClientCfg(t, docsMutate, respond)

	doc, cached, revalidated, err := c.FetchDoc(context.Background(), ft.srv.URL+"/etag", "", true, nil)
	if err != nil {
		t.Fatalf("FetchDoc: %v", err)
	}
	if !cached || !revalidated {
		t.Fatalf("(cached=%v revalidated=%v), want both true", cached, revalidated)
	}
	if !strings.Contains(updateBody, `"expires_at":null`) {
		t.Errorf("metadata update must clear the stale expiry when the TTL is off: %s", updateBody)
	}
	if !doc.ExpiresAt.IsZero() {
		t.Errorf("returned ExpiresAt = %v, want zero (never expires)", doc.ExpiresAt)
	}
	if puts != 0 {
		t.Errorf("doc puts = %d, want 0 (a 304 must not rewrite the snapshot)", puts)
	}
}
