package client

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/NemoNobody/velruna/internal/model"
)

func TestExtractTitle(t *testing.T) {
	body := `<html><head><title>Hello &amp; World</title></head><body>x</body></html>`
	if got := ExtractTitle(body); got != "Hello & World" {
		t.Errorf("ExtractTitle = %q, want %q", got, "Hello & World")
	}
	if got := ExtractTitle("<html><body>x</body></html>"); got != "" {
		t.Errorf("missing title = %q, want empty", got)
	}
}

func TestExtractSkipsDoctypeHeadComments(t *testing.T) {
	// Live-shape page: a doctype, a head with a title, a comment. None of
	// their text may reach the sections (a doctype line once became a
	// document's heading; head text is noise — ExtractTitle owns the title).
	body := `<!doctype html><html lang=en><head><meta charset=utf-8>` +
		`<title>Doc Title</title><style>b{}</style></head>` +
		`<body><!-- an author note, long enough to survive the min-line filter -->` +
		`<h1>Real Head</h1><p>Real paragraph text here.</p></body></html>`
	secs, _, _ := ExtractSections(body)
	if len(secs) != 1 {
		t.Fatalf("sections = %d, want 1: %+v", len(secs), secs)
	}
	if secs[0].Heading != "Real Head" {
		t.Errorf("heading = %q, want the real h1 (doctype/head leaked?)", secs[0].Heading)
	}
	all := secs[0].Heading + secs[0].Text
	for _, bad := range []string{"doctype", "Doc Title", "author note"} {
		if strings.Contains(all, bad) {
			t.Errorf("%q leaked into sections: %q", bad, all)
		}
	}
}

func TestExtractSectionsBasics(t *testing.T) {
	body := `<html><body>` +
		`<style>h1{display:none}</style>` +
		`<script>var s="<h1>fake</h1>";</script>` +
		`<h1>Main</h1><p>Intro &amp; co.</p>` +
		`<h2>Second</h2><ul><li>alpha item</li><li>beta item</li></ul>` +
		`<img src="/img/a.png" alt="Pic" title="t1">` +
		`<img src="data:image/png;base64,AAA" alt="skip">` +
		`<div>nav</div></body></html>`

	secs, imgs, trunc := ExtractSections(body)
	if trunc {
		t.Error("truncated = true, want false for a small page")
	}
	if len(secs) != 2 {
		t.Fatalf("sections = %d, want 2: %+v", len(secs), secs)
	}
	if secs[0].Heading != "Main" || secs[0].Text != "Intro & co." {
		t.Errorf("sec[0] = %+v", secs[0])
	}
	if secs[1].Heading != "Second" || secs[1].Text != "alpha item\nbeta item" {
		t.Errorf("sec[1] = %+v", secs[1])
	}
	all := secs[0].Text + secs[1].Text
	if strings.Contains(all, "display:none") || strings.Contains(all, "fake") {
		t.Errorf("script/style content leaked into text: %q", all)
	}
	if len(imgs) != 1 {
		t.Fatalf("images = %d, want 1 (data: skipped): %+v", len(imgs), imgs)
	}
	want := model.ImageRef{URL: "/img/a.png", Alt: "Pic", Title: "t1"}
	if imgs[0] != want {
		t.Errorf("image = %+v, want %+v", imgs[0], want)
	}
}

func TestExtractManySectionsAreKept(t *testing.T) {
	var b strings.Builder
	for i := range 40 {
		fmt.Fprintf(&b, "<h1>Section %d</h1><p>paragraph text line</p>", i)
	}
	secs, _, trunc := ExtractSections(b.String())
	if len(secs) != 40 {
		t.Errorf("sections = %d, want all 40 (a section count is not a limit)", len(secs))
	}
	if trunc {
		t.Error("truncated = true; nothing was cut")
	}
}

func TestExtractPlainTextSplitsLongInput(t *testing.T) {
	body := strings.Repeat("word ", 2000) // one huge paragraph, ~10k runes
	secs, trunc := ExtractPlainText(body)
	if len(secs) < 2 {
		t.Fatalf("sections = %d, want the paragraph split into blocks", len(secs))
	}
	if trunc {
		t.Error("splitting must not report truncation")
	}
	var got strings.Builder
	for _, s := range secs {
		got.WriteString(s.Text)
	}
	if want := collapseText(body); got.String() != want {
		t.Errorf("blocks do not reconstruct the text: got %d runes, want %d", utf8.RuneCountInString(got.String()), utf8.RuneCountInString(want))
	}
	if secs[0].Heading != "Part (1/2)" {
		t.Errorf("heading = %q, want a part label", secs[0].Heading)
	}
}

// Unicode case folding changes byte length (U+2126 Ω shrinks, U+023A grows). The
// tokenizer decides what is text now, but ExtractTitle still scans raw bytes and
// shares a lowercased copy with the source by offset; these characters appear in
// ordinary technical prose, and neither path may panic or slice at shifted
// offsets.
func TestExtractUnicodeLoweringStable(t *testing.T) {
	for _, u := range []string{"\u2126", "\u0130", "\u212A", "\u023A", "\u023E", "\u1E9E"} {
		body := "<p>" + strings.Repeat(u, 200) + "x</p>"
		secs, _, _ := ExtractSections(body)
		if len(secs) == 0 {
			t.Fatalf("%q: no sections", u)
		}
		if !strings.Contains(secs[0].Text, "x") {
			t.Fatalf("%q: text lost: %q", u, secs[0].Text)
		}
	}
}

func TestExtractTitleUnicodeLoweringStable(t *testing.T) {
	title := ExtractTitle("<title>\u2126 \u0130 page</title>")
	if title != "\u2126 \u0130 page" {
		t.Fatalf("title mangled by lowering: %q", title)
	}
}

// An unclosed raw-text element (<script>, <style>) consumes the rest of the
// page — that is what the HTML5 tokenizer and every browser do, and the text is
// not rendered there either. A self-closed non-void element does not.
func TestUnclosedDropElementMatchesRendering(t *testing.T) {
	secs, _, _ := ExtractSections("<p>alpha</p><script src=x>tail-not-rendered</p>")
	joined := strings.Join(textOfSections(secs), " ")
	if strings.Contains(joined, "tail-not-rendered") {
		t.Errorf("script body surfaced as text: %q", joined)
	}
	if !strings.Contains(joined, "alpha") {
		t.Errorf("text before the script was lost: %q", joined)
	}

	secs, _, _ = ExtractSections("<p>alpha</p><svg/>tail2-kept</p>")
	joined = strings.Join(textOfSections(secs), " ")
	if !strings.Contains(joined, "tail2-kept") {
		t.Fatalf("content after self-closed <svg/> deleted: %q", joined)
	}
}

// TestDropElementsLinear is the DoS regression: a large page made of drop
// elements (legal under fetch_max_bytes) must not cost quadratic time.
func TestDropElementsLinear(t *testing.T) {
	const n = 50000
	done := make(chan struct{}, 1)
	go func() {
		gen := strings.Repeat("<style>x</style>", n) + "<h2>Real section</h2><p>Real body text here.</p>"
		secs, _, _ := ExtractSections(gen)
		if len(secs) != 1 || secs[0].Heading != "Real section" {
			t.Errorf("sections = %+v, want the one real section", secs)
		}
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("extraction on many drop elements is not linear")
	}
}

// TestExtractTagSoupLinear guards the invariant for the text loop: a giant run
// of unterminated "<a" must not re-scan the tail per tag.
func TestExtractTagSoupLinear(t *testing.T) {
	const n = 200000
	var b strings.Builder
	b.Grow(n * 2)
	for i := 0; i < n; i++ {
		b.WriteString("<a")
	}
	done := make(chan struct{}, 1)
	go func() {
		ExtractSections(b.String())
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ExtractSections on a '<a' soup is not linear")
	}
}

// cleanText strips control bytes AFTER entity unescaping: html.UnescapeString
// turns &#27; into ESC, so untested markup (and stored pages) can inject ANSI
// escapes into titles/sections that every later terminal render executes.
func TestCleanTextStripsControl(t *testing.T) {
	title := ExtractTitle("<title>Head &#27;[31mRED&#27;[0m</title>")
	if strings.ContainsRune(title, 0x1b) {
		t.Errorf("ESC survived into the title: %q", title)
	}
	if title != "Head [31mRED[0m" {
		t.Errorf("unexpected title: %q", title)
	}
	secs, _, _ := ExtractSections("<p>plain &#0;and&#128; control</p>")
	joined := strings.Join(textOfSections(secs), " ")
	if strings.ContainsRune(joined, 0x00) || strings.ContainsRune(joined, 0x80) {
		t.Errorf("control bytes survived in section text: %q", joined)
	}
}

func textOfSections(secs []model.Section) []string {
	var out []string
	for _, s := range secs {
		out = append(out, s.Text)
	}
	return out
}

// TestJunkFilterKeepsCJK: the junk filter counts RUNES. A byte limit silently
// ate CJK content (one ideograph = 3 bytes); ASCII keeps the short-menu-debris
// threshold.
func TestJunkFilterKeepsCJK(t *testing.T) {
	secs, _, _ := ExtractSections("<p>安装</p><p>说明文档</p><p>FAQ</p><p>→</p><p>ok</p>")
	if len(secs) != 1 {
		t.Fatalf("sections = %+v, want one", secs)
	}
	joined := secs[0].Text
	for _, want := range []string{"安装", "说明文档"} {
		if !strings.Contains(joined, want) {
			t.Errorf("CJK line %q was dropped: %q", want, joined)
		}
	}
	for _, junk := range []string{"FAQ", "→", "ok"} {
		if strings.Contains(joined, junk) {
			t.Errorf("junk %q survived the filter: %q", junk, joined)
		}
	}
}

// TestExtractPrefersMainContent pins the content-root rule: when the markup
// names the article (<main>, role="main", <article>), the sidebar prose in front
// of it is neither extracted nor counted against the document budget. The
// sidebar is a plain div on purpose: elements dropped as chrome would disappear
// even without the content root.
func TestExtractPrefersMainContent(t *testing.T) {
	page := `<html><body>
<div class="sidebar"><h2>Menu</h2><p>Sidebar prose that is not a link at all.</p></div>
<main><h1>Guide</h1><p>The real content of the page.</p>
<h2>Details</h2><p>More detail here.</p></main>
<footer><h2>Footer</h2><p><a href="/x">X</a></p></footer>
</body></html>`
	secs, _, _ := ExtractSections(page)
	var headings []string
	for _, s := range secs {
		headings = append(headings, s.Heading)
	}
	if strings.Join(headings, ",") != "Guide,Details" {
		t.Errorf("headings = %v, want the main content only (Guide, Details)", headings)
	}
}

// TestExtractPrefersRoleMainNested covers a main container that is a plain div
// with nested divs: the span must follow the matching close tag, not the first
// </div> in sight — the section after the nested div is inside the root and
// must survive.
func TestExtractPrefersRoleMainNested(t *testing.T) {
	page := `<html><body>
<div class="sidebar"><h2>Nav</h2><p><a href="/a">A</a></p></div>
<div role="main"><div class="wrap"><h2>Article</h2>
<p>Body text that matters and is long enough to keep.</p></div>
<div class="pager"><p><a href="/next">Next</a></p></div>
<h2>After</h2><p>Text after the nested div must survive the span.</p></div>
</body></html>`
	secs, _, _ := ExtractSections(page)
	var headings []string
	for _, s := range secs {
		headings = append(headings, s.Heading)
	}
	if strings.Join(headings, ",") != "Article,After" {
		t.Fatalf("headings = %v, want Article,After", headings)
	}
	if !strings.Contains(secs[0].Text, "Body text that matters") {
		t.Errorf("article text missing: %q", secs[0].Text)
	}
}

// TestExtractDropsShortLinkChrome: without a content root, a short block that
// is mostly anchor text is a rail, not documentation.
func TestExtractDropsShortLinkChrome(t *testing.T) {
	page := `<html><body>
<div><h2>Home</h2><p><a href="/">Home</a> <a href="/docs">Docs</a> <a href="/news">News</a></p></div>
<div><h2>Tutorial</h2><p>` + strings.Repeat("real documentation text ", 20) + `</p></div>
</body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 1 || secs[0].Heading != "Tutorial" {
		t.Fatalf("sections = %+v, want only the Tutorial section", secs)
	}
}

// TestExtractLinkRatioIgnoresWhitespace pins the denominator of the chrome
// ratio: block tags inject a newline per element, and counting those padded the
// block until a rail of links looked like prose.
func TestExtractLinkRatioIgnoresWhitespace(t *testing.T) {
	page := `<html><body>
<div><h2>Chapters</h2><p><a href="/a">Alpha</a></p><p><a href="/b">Beta</a></p><p><a href="/c">Gamma</a></p></div>
<div><h2>Body</h2><p>` + strings.Repeat("substantive text ", 20) + `</p></div>
</body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 1 || secs[0].Heading != "Body" {
		t.Fatalf("sections = %+v, want only the Body section", secs)
	}
}

// TestExtractBrokenAnchorsWrappingBlocks: two unclosed <a href> tags make the
// parser wrap the following paragraphs in anchors. A link that wraps block
// content is formatting, not a menu label, so the section stays.
func TestExtractBrokenAnchorsWrappingBlocks(t *testing.T) {
	page := `<html><body><h2>Intro</h2><p>A real introduction paragraph with plenty of prose.</p>` +
		`<h2>Install</h2><a href="/a">Download<p>Read the manual first, it explains every flag.</p>` +
		`<a href="/b">Changelog<p>Updated weekly.</p></body></html>`
	secs, _, tr := ExtractSections(page)
	if len(secs) != 2 || secs[1].Heading != "Install" || tr {
		t.Fatalf("sections = %+v (truncated=%v), want Intro and Install", secs, tr)
	}
}

// TestExtractOneBrokenAnchorIsNotAMenu: the parser re-opens a single unclosed
// <a href> in every following block. Those are one link, not a menu, and the
// paragraphs they wrap are still prose.
func TestExtractOneBrokenAnchorIsNotAMenu(t *testing.T) {
	page := `<html><body><h2>Install</h2><a href="/docs">Docs` +
		`<p>First real paragraph of the page.</p><p>Second real paragraph of the page.</p>` +
		`</body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 1 || secs[0].Heading != "Install" {
		t.Fatalf("sections = %+v, want Install kept", secs)
	}
}

// TestExtractAnchorTargetsAreNotLinks: <a id> without href is an anchor target.
// Counting it as a link would turn a paragraph of anchors into a menu.
func TestExtractAnchorTargetsAreNotLinks(t *testing.T) {
	page := `<html><body><h2>Targets</h2><p><a id="a1">one</a> <a id="a2">two</a> <a id="a3">three</a></p>` +
		`<h2>Body</h2><p>Real body text of the page.</p></body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 2 || secs[0].Heading != "Targets" {
		t.Fatalf("sections = %+v, want Targets and Body", secs)
	}
}

// TestExtractUnclosedAnchorDoesNotReachNextSection: an unclosed <a href> is
// re-opened by the parser in the following blocks; the text of the next section
// is not link text there.
func TestExtractUnclosedAnchorDoesNotReachNextSection(t *testing.T) {
	page := `<html><body><h2>Install</h2><p>Read the guide first.<a href="/docs">Docs` +
		`<h2>Verify</h2><p>Check that status reports the version.</p></body></html>`
	secs, _, _ := ExtractSections(page)
	var headings []string
	for _, s := range secs {
		headings = append(headings, s.Heading)
	}
	if strings.Join(headings, ",") != "Install,Verify" {
		t.Fatalf("headings = %v, want Install,Verify", headings)
	}
}

// TestExtractBudgetCutsOnBlockBoundary pins the storage budget: it drops whole
// blocks from the end and reports the loss, never cutting a block in half.
func TestExtractBudgetCutsOnBlockBoundary(t *testing.T) {
	var b strings.Builder
	for i := range 10 {
		fmt.Fprintf(&b, "<p>para %02d %s</p>", i, strings.Repeat("word ", 170))
	}
	secs, _, tr, _ := ExtractSectionsWithin("<h2>One</h2>"+b.String(),
		ExtractLimits{BlockChars: 1000, MaxChars: 3000})
	if !tr {
		t.Fatal("a document over the budget must report truncation")
	}
	if len(secs) != 3 {
		t.Fatalf("sections = %d, want 3 whole blocks within the budget", len(secs))
	}
	if !strings.HasPrefix(secs[0].Text, "para 00 ") || !strings.HasSuffix(secs[0].Heading, "(1/10)") {
		t.Errorf("first block = %q heading %q, want a whole paragraph with a part label",
			secs[0].Text[:min(len(secs[0].Text), 20)], secs[0].Heading)
	}
	total := 0
	for _, s := range secs {
		total += utf8.RuneCountInString(s.Text)
	}
	if total > 3000 {
		t.Errorf("stored %d runes, want at most the budget (whole blocks only)", total)
	}
}

// TestExtractBudgetNeverStoresNothing: a budget smaller than one block still
// stores that block: an empty document would hide the loss.
func TestExtractBudgetNeverStoresNothing(t *testing.T) {
	secs, _, tr, _ := ExtractSectionsWithin("<h2>One</h2><p>"+strings.Repeat("word ", 500)+"</p>",
		ExtractLimits{BlockChars: 2000, MaxChars: 50})
	if len(secs) != 1 || utf8.RuneCountInString(secs[0].Text) < 100 {
		t.Fatalf("sections = %+v, want one whole block", secs)
	}
	if !tr {
		t.Error("the rest of the section was dropped and the flag must say so")
	}
}

// TestExtractBudgetKeptWholePageIsNotTruncation: when the block kept over the
// budget is the whole page, nothing was lost and the flag stays false.
func TestExtractBudgetKeptWholePageIsNotTruncation(t *testing.T) {
	secs, _, tr, _ := ExtractSectionsWithin("<p>short page text</p>",
		ExtractLimits{BlockChars: 8000, MaxChars: 5})
	if len(secs) != 1 {
		t.Fatalf("sections = %+v, want the page kept whole", secs)
	}
	if tr {
		t.Error("nothing was dropped: the kept block is the whole page")
	}
}

// TestExtractTitleOpaqueContainers: a <title> inside a raw-text element, a
// foreign content container or a CDATA bogus comment is not the page title.
func TestExtractTitleOpaqueContainers(t *testing.T) {
	cases := []string{
		`<noframes><title>Fake</title></noframes><title>Real</title>`,
		`<![CDATA[<title>Fake</title>]]><title>Real</title>`,
		`<template><template><title>Fake</title></template><title>Also fake</title></template><title>Real</title>`,
	}
	for _, page := range cases {
		if got := ExtractTitle(page); got != "Real" {
			t.Errorf("page %q: title = %q, want Real", page, got)
		}
	}

	for _, page := range []string{
		`<svg><!-- <svg> --></svg><title>Real</title>`,
		`<svg><script>var s = "</svg>";</script><title>Fake</title></svg><title>Real</title>`,
		`<svg><!-- a > <svg> --></svg><title>Real</title>`,
		`<svg><!--></svg><title>Real</title>`,
		`<svg><!---></svg><title>Real</title>`,
		`<svg><script/></svg><title>Real</title>`,
		`<svg><textarea><svg></textarea></svg><title>Real</title>`,
		`<template><svg><template></svg></template><title>Real</title>`,
		`<svg><![CDATA[<svg>]]></svg><title>Real</title>`,
		// In foreign content a CDATA section ends only at "]]>", not at the first
		// ">": the tags inside it are text.
		`<svg><![CDATA[<b> </svg> <title>Fake</title> ]]></svg><title>Real</title>`,
		// A mismatched close tag inside svg closes the elements it names, the way
		// the tree parser recovers, so the svg ends where the browser ends it.
		`<svg><g><svg></g></svg><title>Real</title>`,
		// A dash is part of a tag name: these are custom elements, not title/script.
		`<title-x>Fake</title-x><title>Real</title>`,
		`<script-widget>x</script-widget><title>Real</title><script>y</script>`,
		// A processing instruction ends at the first ">": a title inside the
		// comment is not the page title.
		`<?target <title>Fake</title> ?><title>Real</title>`,
		// Form feed is whitespace in HTML, so it terminates a tag name.
		`<title>Real</title` + "\f" + `><title>Lost</title>`,
		// A quote inside an unquoted attribute value is a plain character: the tag
		// ends at the first ">" and the title after it is real.
		`<a href=/x?q="><title>Real</title>">x</a><title>Lost</title>`,
	} {
		if got := ExtractTitle(page); got != "Real" {
			t.Errorf("page %q: title = %q, want Real", page, got)
		}
	}

	// <template/> opens a template (HTML ignores the slash), so both titles sit
	// inside the outer inert template: there is no page title.
	if got := ExtractTitle(`<template><template/><title>Fake</title></template><title>Real</title>`); got != "" {
		t.Errorf("nested template: title = %q, want empty", got)
	}

	// plaintext never ends, so everything after it is text: no title at all.
	if got := ExtractTitle(`<plaintext><title>Fake</title></plaintext><title>Real</title>`); got != "" {
		t.Errorf("plaintext: title = %q, want empty", got)
	}

	// noscript is raw text with scripting on: the "</template>" inside it is text,
	// so the rest of the page stays inside the inert template.
	if got := ExtractTitle(`<template><noscript></template><title>Real</title>`); got != "" {
		t.Errorf("noscript in template: title = %q, want empty", got)
	}

	// A "<title>" inside an attribute value is text, so the first real title wins.
	in := `<a href` + "\f" + `=` + "\f" + `"x><title>Hidden</title>" data-y=1><title>Real</title>`
	if got := ExtractTitle(in); got != "Real" {
		t.Errorf("addressed title: title = %q, want Real", got)
	}

	// A <title> inside MathML is a MathML element, not the page title: the scan
	// skips the whole math container, same answer the tree gives.
	if got := ExtractTitle(`<math><title>Fake</title></math><title>Real</title>`); got != "Real" {
		t.Errorf("title inside math = %q, want Real", got)
	}
}

// TestExtractImageCapReportsTruncation pins the other side of the flag: an image
// dropped by the cap is a loss and must be reported, while exactly the cap is
// not.
func TestExtractImageCapReportsTruncation(t *testing.T) {
	page := func(n int) string {
		var b strings.Builder
		b.WriteString("<html><body><h2>Pictures</h2><p>Text next to the images.</p>")
		for i := 0; i < n; i++ {
			b.WriteString(`<img src="/i`)
			b.WriteString(strings.Repeat("x", i%5+1))
			b.WriteString(`.png">`)
		}
		b.WriteString(`</body></html>`)
		return b.String()
	}
	imgs, _, tr := imagesOf(t, page(maxImages))
	if len(imgs) != maxImages || tr {
		t.Errorf("exactly the cap: %d images, truncated=%v; want %d, false", len(imgs), tr, maxImages)
	}
	imgs, _, tr = imagesOf(t, page(maxImages+1))
	if len(imgs) != maxImages || !tr {
		t.Errorf("one over the cap: %d images, truncated=%v; want %d, true", len(imgs), tr, maxImages)
	}
}

func imagesOf(t *testing.T, page string) ([]model.ImageRef, []model.Section, bool) {
	t.Helper()
	secs, imgs, tr := ExtractSections(page)
	return imgs, secs, tr
}

// TestExtractLinkWrappingBlockThroughInline: a card link may wrap inline markup
// around its block content; it is still formatting, not a menu label.
func TestExtractLinkWrappingBlockThroughInline(t *testing.T) {
	page := `<html><body><h2>Cards</h2>` +
		`<a href="/a"><span><div>Card one prose here.</div></span></a>` +
		`<a href="/b"><span><div>Card two prose here.</div></span></a>` +
		`<h2>Body</h2><p>Real body text of the page.</p></body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 2 || secs[0].Heading != "Cards" {
		t.Fatalf("sections = %+v, want Cards and Body", secs)
	}
}

// TestExtractTitleCap pins MaxTitleChars.
func TestExtractTitleCap(t *testing.T) {
	title := ExtractTitle("<title>" + strings.Repeat("x", MaxTitleChars+50) + "</title>")
	if got := utf8.RuneCountInString(title); got != MaxTitleChars {
		t.Errorf("title = %d runes, want %d", got, MaxTitleChars)
	}
}

// TestExtractKeepsSingleLinkParagraph: one link wrapping a paragraph is a link,
// not a rail — the tokenizer re-opens an unclosed <a href> inside every following
// block, and a link-count blind heuristic dropped those sections.
func TestExtractKeepsSingleLinkParagraph(t *testing.T) {
	page := `<html><body><h2>Install</h2><p><a href="/docs">Run the installer and point it at the archive.</a></p>
<h2>Verify</h2><p><a href="/docs">Check that status reports the version.</a></p>
<h2>Body</h2><p>Real body text of the page, long enough to stand alone.</p>
</body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 3 {
		t.Fatalf("sections = %+v, want Install, Verify and Body", secs)
	}
}

// TestExtractKeepsChromeWhenEverythingIsChrome: a page that is nothing but
// links is still a page — dropping every section would store an empty document.
func TestExtractKeepsChromeWhenEverythingIsChrome(t *testing.T) {
	page := `<html><body><div><h2>Index</h2><p><a href="/a">Alpha</a> <a href="/b">Beta</a></p></div></body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) == 0 {
		t.Fatal("an all-link page must keep its sections")
	}
}

// TestExtractStripsNavAsideFooter pins that the chrome elements are removed
// even when the page has no content root to prefer.
func TestExtractStripsNavAsideFooter(t *testing.T) {
	page := `<html><body>
<nav><h2>Menu</h2><p>nav text</p></nav>
<aside><p>related reading</p></aside>
<h2>Body</h2><p>the article</p>
<footer><p>copyright</p></footer>
</body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 1 || secs[0].Heading != "Body" {
		t.Fatalf("sections = %+v, want only Body", secs)
	}
	for _, s := range secs {
		for _, bad := range []string{"nav text", "related reading", "copyright"} {
			if strings.Contains(s.Text, bad) {
				t.Errorf("chrome %q survived: %+v", bad, s)
			}
		}
	}
}

// TestExtractAnchorTargetDoesNotPoisonSections: <a id="x"/> is an anchor
// target, not an opener. Counting it as one turned the rest of the paragraph
// into link text and dropped a real section without a truncation flag.
func TestExtractAnchorTargetDoesNotPoisonSections(t *testing.T) {
	page := `<html><body>
<h2>Install</h2><p><a id="after-install"/>Run the installer and point it at the archive.</p>
<h2>Verify</h2><p>Check that status reports the version.</p>
<h2>Usage</h2><p>Fetch a page and read the stored sections.</p>
</body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 3 {
		t.Fatalf("sections = %+v, want Install, Verify and Usage", secs)
	}
	if secs[1].Heading != "Verify" || secs[2].Heading != "Usage" {
		t.Errorf("headings = %q, %q; want Verify, Usage", secs[1].Heading, secs[2].Heading)
	}
}

// TestExtractEmptyRootFallsThrough: an empty container is not the article; the
// next candidate (here a lone <article>) still gets its turn.
func TestExtractEmptyRootFallsThrough(t *testing.T) {
	page := `<html><body>
<div class="sidebar"><h2>Side</h2><p>Sidebar prose that is not a link at all.</p></div>
<main></main>
<article><h2>Article</h2><p>Article body text.</p></article>
</body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 1 || secs[0].Heading != "Article" {
		t.Fatalf("sections = %+v, want just Article", secs)
	}
}

// TestExtractUnclosedAnchorStopsAtSection pins the blast radius of a broken
// anchor: hand-written HTML leaves <a> unclosed, and one such tag must not
// reach past the section it sits in.
func TestExtractUnclosedAnchorStopsAtSection(t *testing.T) {
	page := `<html><body>
<h2>Install</h2><p>Unclosed anchor follows.<a href="/docs">Docs
<h2>Verify</h2><p>Check that status reports the version.</p>
</body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 2 || secs[1].Heading != "Verify" {
		t.Fatalf("sections = %+v, want Install and Verify", secs)
	}
}

// TestExtractIgnoresCommentedCloseTag: a commented-out </main> is markup prose
// inside a comment, not the end of the article.
func TestExtractIgnoresCommentedCloseTag(t *testing.T) {
	page := `<html><body><main>
<h2>First</h2><p>First paragraph with enough text to keep.</p>
<!-- </main> -->
<h2>Second</h2><p>Second paragraph that must survive extraction.</p></main></body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 2 || secs[1].Heading != "Second" {
		t.Fatalf("sections = %+v, want First and Second", secs)
	}
}

// TestExtractRoleMainVariants: role is an attribute with a token list — spacing
// around '=' and an extra token are still the main landmark, and data-role is a
// different attribute that must not capture the root.
func TestExtractRoleMainVariants(t *testing.T) {
	sidebar := `<div class="sidebar"><h2>Side</h2><p>Sidebar prose that is not a link at all.</p></div>`
	for _, tc := range []struct {
		name  string
		inner string
		want  string
	}{
		{"spaced equals", `<div role = "main"><h2>Article</h2><p>Article text here.</p></div>`, "Article"},
		{"token list", `<div role="main navigation"><h2>Article</h2><p>Article text here.</p></div>`, "Article"},
		{"newline after tag name", "<div\nrole=\"main\"><h2>Article</h2><p>Article text here.</p></div>", "Article"},
		{"tab after tag name", "<div\trole=\"main\"><h2>Article</h2><p>Article text here.</p></div>", "Article"},
		{"boolean attribute first", `<div hidden role="main"><h2>Article</h2><p>Article text here.</p></div>`, "Article"},
		{"data-role is not role", `<div data-role="main"><h2>Wrapper</h2><p>Wrapper prose here.</p></div>`, "Side,Wrapper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secs, _, _ := ExtractSections(`<html><body>` + sidebar + tc.inner + `</body></html>`)
			var headings []string
			for _, s := range secs {
				headings = append(headings, s.Heading)
			}
			if got := strings.Join(headings, ","); got != tc.want {
				t.Errorf("headings = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUnclosedChromeSwallowsTail documents the one case where the tokenizer is
// stricter than the old scanner: an unclosed <nav> owns the rest of the page in
// the tree (browsers agree), so its content is chrome and is dropped. A page
// that is nothing but chrome keeps it — an empty document is worse than noise.
func TestUnclosedChromeSwallowsTail(t *testing.T) {
	secs, _, _ := ExtractSections(`<html><body><p>alpha text</p><nav>beta text`)
	joined := secs[0].Heading + " " + strings.Join(textOfSections(secs), " ")
	if !strings.Contains(joined, "alpha text") {
		t.Fatalf("text before the unclosed nav was lost: %q", joined)
	}

	secs, _, _ = ExtractSections(`<html><body><nav>nav only text</nav></body></html>`)
	if len(secs) == 0 {
		t.Fatal("a page that is entirely chrome must still yield sections")
	}
}

// TestExtractNestedCandidateIsNotTheRoot: an inner candidate that closes first
// must not cut the article it sits in — the outer element is the content root.
func TestExtractNestedCandidateIsNotTheRoot(t *testing.T) {
	sidebar := `<div class="sidebar"><h2>Side</h2><p>Sidebar prose that is not a link at all.</p></div>`

	roleMain := `<div role="main"><h2>Before</h2><p>Text before the nested main.</p>` +
		`<main><h2>Inner</h2><p>Nested main text.</p></main>` +
		`<h2>After</h2><p>Text after the nested main.</p></div>`
	secs, _, _ := ExtractSections(`<html><body>` + sidebar + roleMain + `</body></html>`)
	var headings []string
	for _, s := range secs {
		headings = append(headings, s.Heading)
	}
	if got := strings.Join(headings, ","); got != "Before,Inner,After" {
		t.Errorf("role=main with a nested <main>: headings = %q, want Before,Inner,After", got)
	}

	posts := `<article><h2>Post</h2><p>Post body text.</p>` +
		`<article><h2>Comment</h2><p>Comment body text.</p></article>` +
		`<h2>More</h2><p>Text after the nested article.</p></article>`
	secs, _, _ = ExtractSections(`<html><body>` + sidebar + posts + `</body></html>`)
	headings = nil
	for _, s := range secs {
		headings = append(headings, s.Heading)
	}
	if got := strings.Join(headings, ","); got != "Post,Comment,More" {
		t.Errorf("article with a nested <article>: headings = %q, want Post,Comment,More", got)
	}
}

// TestExtractSeveralArticlesMeanListing: an index page has one <article> per
// teaser; extracting the first one would store a single teaser as the document.
// A lone <article> is still the article.
func TestExtractSeveralArticlesMeanListing(t *testing.T) {
	sidebar := `<div class="sidebar"><h2>Side</h2><p>Sidebar prose that is not a link at all.</p></div>`
	three := `<article><h2>Article one</h2><p>First teaser text.</p></article>` +
		`<article><h2>Article two</h2><p>Second teaser text.</p></article>` +
		`<article><h2>Article three</h2><p>Third teaser text.</p></article>`
	secs, _, _ := ExtractSections(`<html><body>` + sidebar + three + `</body></html>`)
	if len(secs) != 4 {
		t.Fatalf("sections = %+v, want the listing (Side and three teasers)", secs)
	}

	one := `<div><h2>Comments</h2><p>Reader comments area.</p></div>` +
		`<article><h2>Post</h2><p>Post body text.</p></article>`
	secs, _, _ = ExtractSections(`<html><body>` + sidebar + one + `</body></html>`)
	if len(secs) != 1 || secs[0].Heading != "Post" {
		t.Fatalf("sections = %+v, want just Post", secs)
	}

	// A comment thread nests <article> inside <article>; that is still one
	// article on the page, not a listing.
	nested := `<article><h2>Post</h2><p>Post body text.</p>` +
		`<article><h2>Comment</h2><p>Comment body text.</p></article></article>`
	secs, _, _ = ExtractSections(`<html><body>` + sidebar + nested + `</body></html>`)
	if len(secs) != 2 || secs[0].Heading != "Post" || secs[1].Heading != "Comment" {
		t.Fatalf("sections = %+v, want Post and Comment only", secs)
	}
}

// TestExtractDroppedChromeIsNotTruncation: the flag means "text was cut", not
// "blocks were dropped" — a stored document that lost nothing must not claim
// it is a prefix.
func TestExtractDroppedChromeIsNotTruncation(t *testing.T) {
	const sections = 40
	var b strings.Builder
	b.WriteString("<html><body>")
	for i := 0; i < sections; i++ {
		b.WriteString(`<div><h2>Rail</h2><p><a href="/x">Alpha</a> <a href="/y">Beta</a></p></div>`)
		fmt.Fprintf(&b, `<div><h2>Section %d</h2><p>%s</p></div>`, i, strings.Repeat("real text ", 10))
	}
	b.WriteString("</body></html>")
	secs, _, truncated := ExtractSections(b.String())
	if len(secs) != sections {
		t.Fatalf("sections = %d, want %d", len(secs), sections)
	}
	if truncated {
		t.Error("nothing was cut: the dropped blocks were rails, not text")
	}
}

// TestExtractBudgetReportsTruncation is the other side of the same flag: a block
// that does not fit the budget is a real loss.
func TestExtractBudgetReportsTruncation(t *testing.T) {
	var b strings.Builder
	b.WriteString("<html><body>")
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, `<div><h2>Section %d</h2><p>%s</p></div>`, i, strings.Repeat("real text ", 10))
	}
	b.WriteString("</body></html>")
	secs, _, truncated, _ := ExtractSectionsWithin(b.String(), ExtractLimits{BlockChars: 8000, MaxChars: 300})
	if !truncated {
		t.Error("sections past the budget were dropped and the flag must say so")
	}
	total := 0
	for _, s := range secs {
		total += utf8.RuneCountInString(s.Text)
	}
	if total > 300 {
		t.Errorf("stored %d runes, want at most the budget", total)
	}
}

// TestExtractContentScanLinear guards the scans against unbounded regressions: a
// raw-text rescan per candidate, or a tokenizer pass that revisits the tail. The
// wall-clock budget cannot catch a change that is merely "depth times candidates"
// — the parser caps depth at 512, so that shape costs milliseconds at the 2 MB
// fetch cap; the shape of the algorithm itself is pinned by the behaviour tests
// (nested candidates, listing pages) instead.
func TestExtractContentScanLinear(t *testing.T) {
	cases := []struct {
		name string
		page string
	}{
		{"unclosed main", strings.Repeat("<main>", 400)},
		{"candidates in a stack", strings.Repeat("<div>", 400) + "<main>" +
			strings.Repeat("<article>x</article>", 60000) + "</main>"},
		{"deeper than the parser cap", strings.Repeat("<div>", 50000) +
			strings.Repeat("</span>", 200000)},
		{"open tags only", strings.Repeat("<div>", 300000)},
		{"open tags under an open head", "<head><template>" + strings.Repeat("<div>", 300000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{}, 1)
			go func() {
				ExtractSections(tc.page)
				done <- struct{}{}
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("extraction is not linear on this input")
			}
		})
	}
}

// TestSplitBlocks: splitting is lossless and prefers natural boundaries, so a
// long section becomes readable blocks instead of one cut paragraph.
func TestSplitBlocks(t *testing.T) {
	text := strings.Join([]string{strings.Repeat("a", 900), strings.Repeat("b", 900), "tail"}, "\n")
	blocks := splitBlocks(text, 1000)
	if strings.Join(blocks, "") != text {
		t.Fatal("blocks do not reconstruct the text")
	}
	if blocks[0] != strings.Repeat("a", 900)+"\n" {
		t.Errorf("first block does not end at the line break: %d runes", utf8.RuneCountInString(blocks[0]))
	}

	// No line break: cut at a space, still lossless.
	long := strings.Repeat("word ", 500)
	blocks = splitBlocks(long, 1000)
	if strings.Join(blocks, "") != long {
		t.Fatal("space splitting lost text")
	}
	for _, b := range blocks {
		if n := utf8.RuneCountInString(b); n > 1000 {
			t.Errorf("block of %d runes exceeds the target", n)
		}
	}

	// No boundary at all: the hard cut still keeps every byte.
	min := strings.Repeat("x", 2500)
	blocks = splitBlocks(min, 1000)
	if strings.Join(blocks, "") != min {
		t.Fatal("hard splitting lost text")
	}

	if got := splitBlocks("short", 1000); len(got) != 1 || got[0] != "short" {
		t.Errorf("short text = %v, want one block", got)
	}
}

// TestExtractSectionSplitAtBlock: a long section becomes several blocks joined
// without loss, and nothing is flagged truncated.
func TestExtractSectionSplitAtBlock(t *testing.T) {
	para := func(i int) string { return fmt.Sprintf("paragraph %02d ", i) + strings.Repeat("word ", 170) }
	var body strings.Builder
	body.WriteString("<h2>Section</h2>")
	paras := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		p := collapseText(para(i))
		paras = append(paras, p)
		body.WriteString("<p>")
		body.WriteString(p)
		body.WriteString("</p>")
	}
	secs, _, truncated := ExtractSections(body.String())
	if truncated {
		t.Fatal("splitting a section must not be reported as truncation")
	}
	if len(secs) < 2 {
		t.Fatalf("sections = %d, want the section split into blocks", len(secs))
	}
	var got strings.Builder
	for _, s := range secs {
		got.WriteString(s.Text)
	}
	if want := strings.Join(paras, "\n"); got.String() != want {
		t.Errorf("blocks do not reconstruct the section text")
	}
	if !strings.HasSuffix(secs[0].Heading, "(1/2)") && !strings.Contains(secs[0].Heading, "(1/") {
		t.Errorf("heading = %q, want a part label", secs[0].Heading)
	}
}

// TestExtractPreVerbatim: a code block keeps its newlines and indentation, and
// short lines inside it ("}") are not junk-filtered away.
func TestExtractPreVerbatim(t *testing.T) {
	page := "<h2>Example</h2><p>Look:</p><pre>func main() {\n\n\tif x &gt; 0 {\n\t\treturn y\n\t}\n}</pre>"
	secs, _, _ := ExtractSections(page)
	if len(secs) != 1 {
		t.Fatalf("sections = %+v, want one", secs)
	}
	want := "Look:\nfunc main() {\n\n\tif x > 0 {\n\t\treturn y\n\t}\n}"
	if secs[0].Text != want {
		t.Errorf("text = %q, want %q", secs[0].Text, want)
	}
}

// TestExtractPreInsideLink: a code sample inside a section whose prose is a
// pair of links is still documentation — the section is kept, and a second real
// section proves the unfiltered fallback is not what saved it.
func TestExtractPreInsideLink(t *testing.T) {
	page := `<html><body><h2>Example</h2><p><a href="/run">Run it now</a> <a href="/test">Test it now</a></p><pre>x := 1; y := 2</pre>` +
		`<h2>Details</h2><p>Details of the example, long enough to stand alone.</p></body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 2 || secs[0].Heading != "Example" {
		t.Fatalf("sections = %+v, want Example and Details", secs)
	}
	if !strings.Contains(secs[0].Text, "x := 1; y := 2") {
		t.Errorf("the code sample was lost: %q", secs[0].Text)
	}
}

// TestExtractShortProseWithLinksIsNotARail: a sentence carrying links is prose,
// not a menu — however many tools it mentions, however long the anchors are and
// however short the sentence. The tell is the text OUTSIDE the links.
func TestExtractShortProseWithLinksIsNotARail(t *testing.T) {
	cases := []string{
		`<h2>Prerequisites</h2><p>Requires <a href="/go">Go 1.22</a> and <a href="/make">Make</a>.</p>`,
		`<h2>Setup</h2><p>Then <a href="/install">install the package</a> and <a href="/start">start the service</a>.</p>`,
		`<h2>CI</h2><p>Run <a href="/a">go test</a>, <a href="/b">go vet</a>, <a href="/c">gofmt</a>, <a href="/d">staticcheck</a> and <a href="/e">golangci-lint</a> before pushing.</p>`,
		`<h2>Tools</h2><p>You need <a href="/a">Go</a>, <a href="/b">Make</a>, <a href="/c">Git</a>, <a href="/d">curl</a>, <a href="/e">jq</a> and <a href="/f">Docker</a> installed.</p>`,
		`<h2>Deps</h2><p>You need <a href="/a">Go</a>, <a href="/b">Make</a>, <a href="/c">Git</a>, <a href="/d">curl</a>, <a href="/e">jq</a>, <a href="/f">Docker</a>, <a href="/g">Podman</a> and <a href="/h">gcc</a> installed.</p>`,
		`<h2>Imports</h2><p>See <a href="/a">fmt</a>, <a href="/b">os</a>, <a href="/c">io</a>, <a href="/d">net</a>, <a href="/e">http</a>, <a href="/f">log</a>, <a href="/g">time</a>, <a href="/h">sync</a>, <a href="/i">sort</a> and <a href="/j">math</a>.</p>`,
		`<h2>Flags</h2><ul><li><a href="/v">--verbose</a> — print more output</li><li><a href="/q">--quiet</a> — print less output</li></ul>`,
		`<h2>FAQ</h2><p>Why does it fail? See <a href="/t">troubleshooting</a>.</p><p>How do I upgrade? See <a href="/u">upgrade guide</a>.</p>`,
		`<h2>Note</h2><p>Remember to back up the database before you start the migration.</p><p><a href="/b">backup</a> <a href="/r">restore</a></p>`,
		`<h2>Choose</h2><p>Choose <a href="/f">foo</a> <a href="/b">bar</a> to continue.</p>`,
	}
	for _, section := range cases {
		page := `<html><body>` + section + `<h2>Next</h2><p>Continue with the next chapter when ready.</p></body></html>`
		secs, _, _ := ExtractSections(page)
		if len(secs) != 2 {
			t.Errorf("page %q: sections = %+v, want the first section kept", section, secs)
		}
	}
}

// TestExtractBreadcrumbIsARail: a breadcrumb of any length is labels and
// separators — no word between two links — so the length does not save it.
func TestExtractBreadcrumbIsARail(t *testing.T) {
	page := `<html><body><div><a href="/">Home</a> :: <a href="/d">Docs</a> :: ` +
		`<a href="/g">Guide</a> → <a href="/a">API</a> • <a href="/x">About</a></div>` +
		`<h2>Body</h2><p>Real body text of the page.</p></body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 1 || secs[0].Heading != "Body" {
		t.Fatalf("sections = %+v, want just Body", secs)
	}
}

// TestExtractLinkMenuIsARail: a block of links and separators holds no word
// outside the links, so it is a menu however many labels it has.
func TestExtractLinkMenuIsARail(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<html><body><div><p>`)
	for i := 0; i < 20; i++ {
		label := "v" + strings.Repeat("x", 4) + string(rune('a'+i))
		b.WriteString(`<a href="/`)
		b.WriteString(label)
		b.WriteString(`">`)
		b.WriteString(label)
		b.WriteString(`</a> / `)
	}
	b.WriteString(`</p></div><h2>Body</h2><p>Real body text of the page.</p></body></html>`)
	secs, _, _ := ExtractSections(b.String())
	if len(secs) != 1 || secs[0].Heading != "Body" {
		t.Fatalf("sections = %+v, want just Body", secs)
	}
}

// TestExtractLabelledPickerSurvives: the trade-off of the strict rule. A picker
// with captions holds words outside the links, so it is stored as a section
// rather than guessed away: one noisy section is cheaper than cutting a
// paragraph that happens to be mostly links.
func TestExtractLabelledPickerSurvives(t *testing.T) {
	page := `<html><body><div><p>Supported Versions: <a href="/v1">1.0</a> / <a href="/v2">2.0</a></p></div>` +
		`<h2>Body</h2><p>Real body text of the page.</p></body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 2 {
		t.Fatalf("sections = %+v, want the picker and Body", secs)
	}
}

// TestExtractFallbackKeepsLinesAndPre: the tokenizer path for markup the parser
// refuses keeps <br> as a line end and <pre> verbatim.
func TestExtractFallbackKeepsLinesAndPre(t *testing.T) {
	page := strings.Repeat("<div>", 600) + "<p>first<br>second</p><pre>func main() {\n    x := 1\n}</pre>"
	secs, _, _ := ExtractSections(page)
	if len(secs) != 1 {
		t.Fatalf("sections = %+v, want one", secs)
	}
	want := "first\nsecond\nfunc main() {\n    x := 1\n}"
	if secs[0].Text != want {
		t.Errorf("text = %q, want %q", secs[0].Text, want)
	}
}

// TestExtractFallbackDetails: the tokenizer path (markup deeper than the tree
// parser's stack cap) keeps inline spacing, keeps a code block whole — including
// <br> and the text that follows it — and does not collect images from chrome.
func TestExtractFallbackDetails(t *testing.T) {
	deep := func(s string) string { return strings.Repeat("<div>", 600) + s }
	cases := []struct{ name, page, want string }{
		{"inline spacing", deep(`<p>Run <code>go test</code> <code>./...</code> now.</p>`), "Run go test ./... now."},
		{"break inside code", deep(`<pre>a<br>b</pre>`), "a\nb"},
		{"text after code", deep(`<pre>x := 1</pre><p>tail after code</p>`), "x := 1\ntail after code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secs, _, _ := ExtractSections(tc.page)
			if len(secs) != 1 || secs[0].Text != tc.want {
				t.Fatalf("sections = %+v, want one section %q", secs, tc.want)
			}
		})
	}

	t.Run("chrome images are not collected", func(t *testing.T) {
		_, imgs, _ := ExtractSections(deep(`<p>real text of the page</p><nav><img src="/nav.png"></nav><img src="/real.png">`))
		if len(imgs) != 1 || imgs[0].URL != "/real.png" {
			t.Fatalf("images = %+v, want only /real.png", imgs)
		}
	})

	t.Run("chrome inside code is text", func(t *testing.T) {
		secs, _, _ := ExtractSections(deep(`<pre>line one<nav>line two</nav>line three</pre>`))
		if len(secs) != 1 || !strings.Contains(secs[0].Text, "line two") {
			t.Fatalf("sections = %+v, want the code block with line two", secs)
		}
	})

	t.Run("unmatched close does not split", func(t *testing.T) {
		secs, _, _ := ExtractSections(deep(`<p>alpha text</pre> beta text</p>`))
		if len(secs) != 1 || secs[0].Text != "alpha text beta text" {
			t.Fatalf("sections = %+v, want one line", secs)
		}
	})

	t.Run("all-chrome retry keeps no script", func(t *testing.T) {
		secs, _, _ := ExtractSections(deep(`<nav>nav text only</nav><script>var secret = 1;</script>`))
		if len(secs) == 0 {
			t.Fatal("expected the chrome text")
		}
		if strings.Contains(secs[0].Text, "secret") {
			t.Fatalf("script leaked into the document: %q", secs[0].Text)
		}
	})

	t.Run("unclosed head does not swallow the page", func(t *testing.T) {
		page := "<!doctype html><html><head><title>x</title><body>" + strings.Repeat("<div>", 600) +
			"<p>prose that must survive</p>"
		secs, _, _ := ExtractSections(page)
		if len(secs) != 1 || !strings.Contains(secs[0].Text, "prose that must survive") {
			t.Fatalf("sections = %+v, want the body prose", secs)
		}
	})

	t.Run("unclosed head without a body tag", func(t *testing.T) {
		page := "<html><head><title>t</title>" + strings.Repeat("<div>", 600) + "<p>prose that must survive</p>"
		secs, _, _ := ExtractSections(page)
		if len(secs) != 1 || !strings.Contains(secs[0].Text, "prose that must survive") {
			t.Fatalf("sections = %+v, want the prose", secs)
		}
	})

	t.Run("text closes an implicit head", func(t *testing.T) {
		page := "<html><head><title>t</title>VISIBLE" + strings.Repeat("<div>", 600) + "<p>MORE</p>"
		secs, _, _ := ExtractSections(page)
		joined := ""
		for _, s := range secs {
			joined += s.Text
		}
		if !strings.Contains(joined, "VISIBLE") || !strings.Contains(joined, "MORE") {
			t.Fatalf("sections = %+v, want both VISIBLE and MORE", secs)
		}
	})

	t.Run("a close tag inside a template does not reveal it", func(t *testing.T) {
		// Template content is inert. A "</div>" for an element below the open
		// template is ignored by the parser, so the template stays open and its
		// text stays hidden until "</template>".
		secs, _, _ := ExtractSections(deep(`<div><template></div><p>inert text</p></template>visible text`))
		joined := ""
		for _, s := range secs {
			joined += s.Text
		}
		if strings.Contains(joined, "inert text") {
			t.Errorf("template content leaked into the page: %q", joined)
		}
		if !strings.Contains(joined, "visible text") {
			t.Errorf("page text lost: %q", joined)
		}
	})

	t.Run("a second head start tag is ignored", func(t *testing.T) {
		page := deep(`<head><title>t</title><head><p>VISIBLE`)
		secs, _, _ := ExtractSections(page)
		joined := ""
		for _, s := range secs {
			joined += s.Text
		}
		if !strings.Contains(joined, "VISIBLE") {
			t.Fatalf("sections = %+v, want the text after the repeated head", secs)
		}
	})

	t.Run("image after an implicit head is collected", func(t *testing.T) {
		page := "<html><head><title>t</title>" + strings.Repeat("<div>", 600) +
			`<p>text next to the image</p><img src="/after-head.png">`
		_, imgs, _ := ExtractSections(page)
		if len(imgs) != 1 || imgs[0].URL != "/after-head.png" {
			t.Fatalf("images = %+v, want the image after the head", imgs)
		}
	})

	t.Run("obsolete void head elements close the head", func(t *testing.T) {
		for _, el := range []string{"<basefont color=red>", "<bgsound src=/x>"} {
			page := "<html><head>" + el + strings.Repeat("<div>", 600) + "<p>visible prose</p>"
			secs, _, _ := ExtractSections(page)
			joined := ""
			for _, s := range secs {
				joined += s.Text
			}
			if !strings.Contains(joined, "visible prose") {
				t.Errorf("%s: text lost: %+v", el, secs)
			}
		}
	})

	t.Run("image right after head elements is collected", func(t *testing.T) {
		page := `<head><meta charset="utf-8"><img src="/after-meta.png">` + strings.Repeat("<div>", 600) +
			`<p>text beside the image</p>`
		_, imgs, _ := ExtractSections(page)
		if len(imgs) != 1 || imgs[0].URL != "/after-meta.png" {
			t.Fatalf("images = %+v, want the image after the head", imgs)
		}
	})

	t.Run("unclosed title is not spilled as page text", func(t *testing.T) {
		page := strings.Repeat("<div>", 600) + "<head><title>secret title text"
		secs, _, _ := ExtractSections(page)
		for _, s := range secs {
			if strings.Contains(s.Text, "secret title text") {
				t.Fatalf("title text leaked into the page: %+v", secs)
			}
		}
	})

	t.Run("template in head does not leak", func(t *testing.T) {
		page := "<html><head><template><span>INERT</span></template></head>" +
			strings.Repeat("<div>", 600) + "<p>page prose</p>"
		secs, _, _ := ExtractSections(page)
		joined := ""
		for _, s := range secs {
			joined += s.Text
		}
		if strings.Contains(joined, "INERT") {
			t.Fatalf("template content leaked: %+v", secs)
		}
		if !strings.Contains(joined, "page prose") {
			t.Fatalf("page prose lost: %+v", secs)
		}
	})

	t.Run("image cap in the fallback reports truncation", func(t *testing.T) {
		page := strings.Repeat("<div>", 600) + "<p>text</p>" + strings.Repeat(`<img src="/i.png">`, maxImages+1)
		_, imgs, tr := ExtractSections(page)
		if len(imgs) != maxImages || !tr {
			t.Fatalf("images = %d truncated = %v, want %d and true", len(imgs), tr, maxImages)
		}
	})

	t.Run("all-chrome page is kept", func(t *testing.T) {
		secs, _, _ := ExtractSections(deep(`<nav>only navigation text here</nav>`))
		if len(secs) == 0 {
			t.Fatal("a page that is entirely chrome must still yield sections")
		}
	})
}

// TestExtractTitleLinear guards the title scan against two quadratic shapes: an
// unterminated comment full of "<title>" (asking "is this offset inside a
// comment" from the start of the document per hit), and a page made of comments
// closed with the legacy "--!>" (looking for "-->" and "--!>" with separate
// searches scanned the tail per comment: 2 MB cost 48 s).
func TestExtractTitleLinear(t *testing.T) {
	cases := []struct {
		name string
		page string
	}{
		{"unterminated comment", "<p>real content</p><!--" + strings.Repeat("<title>", 20000)},
		{"legacy comment closes", strings.Repeat("<!--x--!>", 200000) + "<title>Real</title>"},
		{"many short comments", strings.Repeat("<!--x-->", 20000) + "<title>Real</title>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan string, 1)
			go func() { done <- ExtractTitle(tc.page) }()
			select {
			case got := <-done:
				if tc.name != "unterminated comment" && got != "Real" {
					t.Errorf("title = %q, want Real", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("ExtractTitle is not linear on this input")
			}
		})
	}
}

// TestExtractInlineTextNotGlued: inline elements split a sentence into several
// text nodes; joining them must keep the spaces, or "go test ./..." becomes
// "go test./...".
func TestExtractInlineTextNotGlued(t *testing.T) {
	cases := []struct{ page, want string }{
		{"<p>Run <code>go test</code> <code>./...</code> now.</p>", "Run go test ./... now."},
		{"<p><b>bold</b> <b>italic</b></p>", "bold italic"},
		{"<p>See <a href=\"/a\">one</a> <a href=\"/b\">two</a> please.</p>", "See one two please."},
	}
	for _, tc := range cases {
		secs, _, _ := ExtractSections(tc.page)
		if len(secs) != 1 || secs[0].Text != tc.want {
			t.Errorf("page %q: text = %q, want %q", tc.page, secs[0].Text, tc.want)
		}
	}
}

// TestExtractDeepHeadingEndsLine: a block element's close ends the line even
// when it never opened a section (h5, or a div inside a div).
func TestExtractDeepHeadingEndsLine(t *testing.T) {
	scases := []struct{ page, want string }{
		{"<h2>S</h2><h5>Subhead</h5>Prose right after.</p>", "Subhead\nProse right after."},
		{"<h2>S</h2><div>first<div>second</div>third</div>", "first\nsecond\nthird"},
	}
	for _, tc := range scases {
		secs, _, _ := ExtractSections(tc.page)
		if len(secs) != 1 || secs[0].Text != tc.want {
			t.Errorf("page %q: text = %q, want %q", tc.page, secs[0].Text, tc.want)
		}
	}
}

// TestExtractDeepMarkupFallback: markup nested past the parser's stack cap still
// yields its text via the tokenizer path — headings are lost, the page is not.
func TestExtractDeepMarkupFallback(t *testing.T) {
	page := strings.Repeat("<div>", 600) + "<p>deep text survives</p>"
	secs, _, _ := ExtractSections(page)
	if len(secs) != 1 || !strings.Contains(secs[0].Text, "deep text survives") {
		t.Fatalf("sections = %+v, want the deep text", secs)
	}
}

// TestExtractTitleIgnoresCommentAndScript: "<title>" inside a comment or a raw
// text element is not the page title.
func TestExtractTitleIgnoresCommentAndScript(t *testing.T) {
	if got := ExtractTitle(`<!-- <title>Old</title> --><title>New</title>`); got != "New" {
		t.Errorf("title = %q, want New", got)
	}
	if got := ExtractTitle(`<script>var s = "<title>JS</title>";</script><title>Real</title>`); got != "Real" {
		t.Errorf("title = %q, want Real", got)
	}
	if got := ExtractTitle(`<a href=/x title=don't>y</a><title>Real</title>`); got != "Real" {
		t.Errorf("apostrophe in an unquoted value: title = %q, want Real", got)
	}
	if got := ExtractTitle(`<div data-x="a > <title>Fake</title>"><title>Real</title>`); got != "Real" {
		t.Errorf("'>' inside a quoted value: title = %q, want Real", got)
	}
	if got := ExtractTitle(`<!-- x --!><title>Real</title>`); got != "Real" {
		t.Errorf("legacy comment close: title = %q, want Real", got)
	}
	if got := ExtractTitle(`<svg/><title>Real</title>`); got != "Real" {
		t.Errorf("self-closed svg: title = %q, want Real", got)
	}
	// HTML ignores the slash outside foreign content, so <title/> opens a real
	// title; the text runs to the first </title>, as a browser reads it.
	if got := ExtractTitle(`<title/>Real page</title>`); got != "Real page" {
		t.Errorf("self-closed title: title = %q, want Real page", got)
	}
	if got := ExtractTitle(`<title data-x=">">Real</title>`); got != "Real" {
		t.Errorf("'>' in a title attribute: title = %q, want Real", got)
	}
	if got := ExtractTitle(`<title = "x">Real</title>`); got != "Real" {
		t.Errorf("odd attribute shape: title = %q, want Real", got)
	}
	if got := ExtractTitle(`<head><title>Cut here`); got != "Cut here" {
		t.Errorf("cut title = %q, want the text to EOF", got)
	}
}

// TestAttributeAndRoleParsing pins the forms the hand-rolled attribute scanner
// used to get wrong. The tokenizer owns them now, so this is a regression guard
// at the extraction level: CRLF-separated markup, an unquoted value followed by
// another attribute, a boolean attribute, and role spacing.
func TestAttributeAndRoleParsing(t *testing.T) {
	secs, imgs, _ := ExtractSections("<html><body><div\nhidden role = \"main\">" +
		"<img src=/a.png\rdata-x=1>" +
		"<h2>Article</h2><p>Article body text.</p></div></body></html>")
	if len(imgs) != 1 || imgs[0].URL != "/a.png" {
		t.Fatalf("images = %+v, want one with url /a.png", imgs)
	}
	if len(secs) != 1 || secs[0].Heading != "Article" {
		t.Fatalf("sections = %+v, want just Article", secs)
	}
}
