package main

// Drift guards: every long review round on this repo re-found the same class
// of bug — a doc surface (README table, env list, phrase, pin, compose claim)
// stating something the code does not do, usually as a stale tail after a
// semantic fix. These tests encode the recurring invariants so the manual
// sweep becomes a regression check: each one is deterministic, offline, and
// cheap enough to run in `make ci`.
//
// When a check fails after an intentional change, update the doc surface or
// the curated lists below — the point is that the change is a decision, not
// a silent drift.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/NemoNobody/velruna/cmd"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// docFiles: every user-facing .md in the repo (walked recursively so a new
// docs subdir is covered the day it appears), minus the vendored _ref tree,
// .git, and hack/ (scratch scripts, not a published surface).
func docFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == ".git" || p == "_ref" || p == "hack" {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".md") && !strings.HasPrefix(d.Name(), ".") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk for doc files: %v", err)
	}
	sort.Strings(files)
	return files
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestNoRetiredPhrases fails if a phrase that contradicts current behavior
// survives anywhere in user-facing prose (CHANGELOG history sections are
// exempt: they record what was believed at release time). Grow the map when
// a semantic fix retires wording — the map IS the conveyor's memory.
func TestNoRetiredPhrases(t *testing.T) {
	retired := map[string]string{
		"notes/source move when supplied": "source is recorded from the calling edge, not paired with notes",
		"notes/source when supplied":      "same as above (README/CHANGELOG wording)",
		"pattern/size":                    "redaction rows carry chars (model.Redaction.Chars)",
		"field/pattern/size":              "same as above",
		"pattern name and size":           "same as above",
		"field, pattern, size":            "same as above",
		"OPENSEARCH_PATH_REPO":            "compose sets the dotted path.repo env form; the entrypoint drops the uppercase one",
		"one bounded read":                "links check is one bounded scan plus a batched probe round",
		"shorter than the one written":    "the guard marker can be longer than the span it replaced — the stored text differs, the row is the signal",
		"shorter than what was written":   "same as above (setup.md wording)",
		"shorter than what you sent":      "same as above (rules wording)",
		"live run pending":                "the secure profile is exercised by the dispatch smoke workflow",
		"the last two are filtered":       "three of the six KPI tiles are filtered",
		"infrastructure ready, live run":  "same secure-profile wording",
	}
	for _, f := range docFiles(t) {
		body := readFile(t, f)
		if filepath.Base(f) == "CHANGELOG.md" {
			// Only the Unreleased section describes current behavior.
			if i := strings.Index(body, "\n## v"); i >= 0 {
				body = body[:i]
			}
		}
		for phrase, why := range retired {
			if strings.Contains(body, phrase) {
				t.Errorf("%s: retired phrase %q (%s)", f, phrase, why)
			}
		}
	}
}

// TestReadmeCommandTableMatchesCobra: every command row in the README table
// must name a registered command, and every --flag it lists must exist in
// that command's subtree (the table is the CLI's face; flag drift here is the
// most reported doc bug).
func TestReadmeCommandTableMatchesCobra(t *testing.T) {
	root := cmd.NewRootCmd()
	subtreeFlags := map[string]map[string]bool{}
	subtreeShorts := map[string]map[string]bool{}
	childNames := map[string]map[string]bool{}
	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		flags := map[string]bool{}
		shorts := map[string]bool{}
		kids := map[string]bool{}
		for _, add := range []*pflag.FlagSet{c.Flags(), c.PersistentFlags(), c.InheritedFlags()} {
			add.VisitAll(func(fl *pflag.Flag) {
				flags[fl.Name] = true
				if fl.Shorthand != "" {
					shorts[fl.Shorthand] = true
				}
			})
		}
		seen := subtreeFlags[path]
		if seen == nil {
			seen = map[string]bool{}
			subtreeFlags[path] = seen
		}
		seenShort := subtreeShorts[path]
		if seenShort == nil {
			seenShort = map[string]bool{}
			subtreeShorts[path] = seenShort
		}
		for k := range flags {
			seen[k] = true
		}
		for k := range shorts {
			seenShort[k] = true
		}
		for _, child := range c.Commands() {
			kids[child.Name()] = true
			childPath := child.Name()
			if path != "" {
				childPath = path + " " + child.Name()
			}
			walk(child, childPath)
			for k := range subtreeFlags[childPath] {
				seen[k] = true
			}
			for k := range subtreeShorts[childPath] {
				seenShort[k] = true
			}
		}
		childNames[path] = kids
	}
	walk(root, "")

	line := regexp.MustCompile("^\\|\\s*`velruna ([a-z-]+)([^`]*)")
	flag := regexp.MustCompile(`--([a-z][a-z-]*)`)
	subcmd := regexp.MustCompile(`^[a-z][a-z-]*$`)
	seenCmd := map[string]bool{}
	for _, l := range strings.Split(readFile(t, "README.md"), "\n") {
		m := line.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		name := m[1]
		flags, ok := subtreeFlags[name]
		if !ok {
			// creds set|status|rm and dashboards push|gen-fields name
			// subcommands in the second group; the top name must still exist.
			t.Errorf("README command table lists unknown command %q", name)
			continue
		}
		seenCmd[name] = true
		// The first token of the command cell is the subcommand (or the
		// `set\|status\|rm` alternative chain — captured whole by the `[^`]*`
		// group, then split on the normalized pipe; write the chain without
		// spaces or Fields() will break it apart). A typo like "pulsh" or a
		// stale alternative is exactly the drift this guard exists to catch.
		// Positional/flag tokens never match `subcmd` and are skipped.
		// Known limit: subcommands named in the DESCRIPTION cell (e.g.
		// dashboards gen-fields) are not validated — prose parsing invites
		// false positives.
		cell := strings.ReplaceAll(m[2], `\|`, "|")
		if toks := strings.Fields(cell); len(toks) > 0 {
			for _, alt := range strings.Split(toks[0], "|") {
				if alt == "" || !subcmd.MatchString(alt) {
					continue
				}
				if !childNames[name][alt] {
					t.Errorf("README table: %q listed under %q but %s has no such subcommand", alt, name, name)
				}
			}
		}
		for _, fm := range flag.FindAllStringSubmatch(l, -1) {
			if !flags[fm[1]] {
				t.Errorf("README table: --%s listed for %q but not registered in its subtree", fm[1], name)
			}
		}
		// Shorthands are validated on the command cell only (the description
		// column's prose is full of hyphens; the cell is where flags live).
		// A manual scan (no lookahead in Go regexp) so adjacent flags like
		// "[-p -q]" are both checked.
		for i := 0; i+1 < len(cell); i++ {
			if cell[i] != '-' || !isASCIILetter(cell[i+1]) {
				continue
			}
			if i > 0 {
				c := cell[i-1]
				switch c {
				case ' ', '\t', '[', '(', '|', ',', '/', ']':
				default:
					continue
				}
			}
			sh := cell[i+1 : i+2]
			if !subtreeShorts[name][sh] {
				t.Errorf("README table: -%s listed for %q but no registered flag in its subtree has that shorthand", sh, name)
			}
		}
	}

	// And no registered top-level command may be missing from the table
	// (dashboards was invisible there once; a new command must be documented).
	for _, c := range root.Commands() {
		if c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		if !seenCmd[c.Name()] {
			t.Errorf("command %q is registered but missing from the README command table", c.Name())
		}
	}
}

// docOnlyEnv: environment variables documented in setup.md that no code in
// this repo and no compose interpolation consumes — legitimate extras
// (make-time inputs, external tools' knobs). Keep this map tiny: a new
// runtime knob belongs in code, not here.
var docOnlyEnv = map[string]bool{
	"OPENSEARCH_INITIAL_ADMIN_PASSWORD": true, // a container env the secure profile SETS, never interpolates
}

// composeEnvNames: every ${NAME} interpolation in docker/opensearch.yml
// (forms ${V}, ${V:-d}, ${V-d}, ${V?e}) — compose consumes those itself, so
// setup.md may document them even though no Go code reads them.
func composeEnvNames(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	re := regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]{3,})[:}?-]`)
	for _, m := range re.FindAllStringSubmatch(readFile(t, "docker/opensearch.yml"), -1) {
		names[m[1]] = true
	}
	return names
}

// codeEnvNames: every literal passed to os.Getenv / os.LookupEnv across the
// repo's own Go packages (cmd, internal/...; tests and the vendored _ref
// tree excluded). Known blind spot: env names passed through variables or
// constants are invisible to this scan — every current call site is
// literal; if a refactor indirections them, move the name into a const and
// extend this scan to const literals rather than dropping the guard.
func codeEnvNames(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	fset := token.NewFileSet()
	var dirs []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == ".git" || p == "_ref" || p == "docs" || p == "rules" || p == "docker" || p == ".github" {
				return fs.SkipDir
			}
			if (p == "cmd" || p == "internal") || strings.HasPrefix(p, "internal/") || strings.HasPrefix(p, "cmd/") {
				dirs = append(dirs, p)
			}
			return nil
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk for go packages: %v", err)
	}
	found := 0
	for _, dir := range dirs {
		pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", dir, err)
		}
		for _, pkg := range pkgs {
			for _, file := range pkg.Files {
				ast.Inspect(file, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					pkgID, ok := sel.X.(*ast.Ident)
					if !ok || pkgID.Name != "os" {
						return true
					}
					if sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv" {
						return true
					}
					if len(call.Args) != 1 {
						return true
					}
					lit, ok := call.Args[0].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						return true
					}
					names[strings.Trim(lit.Value, `"`)] = true
					found++
					return true
				})
			}
		}
	}
	if found == 0 {
		t.Fatal("code env scan parsed zero os.Getenv/LookupEnv calls — the scan itself broke")
	}
	return names
}

func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// goFilesUnder returns the non-test .go files under dir (sorted).
func goFilesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".go") && !strings.HasSuffix(d.Name(), "_test.go") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

// proseAfterVelruna: English words that legitimately follow the project name
// in prose ("velruna assumes..."), not command references. Adding a word here
// is a conscious decision, like the retired-phrase map — if the test fires on
// a real hint, the hint is wrong, not the guard.
var proseAfterVelruna = map[string]bool{
	"assumes": true, "before": true, "index": true, "indices": true,
}

// nonToolTokens: velruna_-prefixed identifiers that are stored field/key
// names, not MCP tool references.
var nonToolTokens = map[string]string{
	"velruna_schema": "mappings._meta.velruna_schema, the stored schema version key",
}

// TestHintReferencesExist: user-facing strings in cmd/ and internal/ name CLI
// commands (`velruna <cmd>`) and MCP tools (`velruna_<tool>`) — every such name
// must exist. Round 22 lost a round to a hint that told users to run `velruna
// patch`, which the CLI has never had; comments may discuss anything, so only
// string literals are scanned.
func TestHintReferencesExist(t *testing.T) {
	cliCmds := map[string]bool{}
	for _, c := range cmd.NewRootCmd().Commands() {
		cliCmds[c.Name()] = true
	}
	tools := map[string]bool{}
	mcpName := regexp.MustCompile(`Name:\s*"(velruna_[a-z_]+)"`)
	for _, path := range goFilesUnder(t, "cmd") {
		for _, m := range mcpName.FindAllStringSubmatch(readFile(t, path), -1) {
			tools[m[1]] = true
		}
	}
	if len(tools) == 0 {
		t.Fatal("no MCP tool registrations parsed — the registration format changed and this check went blind")
	}
	cliRef := regexp.MustCompile(`\bvelruna ([a-z][a-z-]*)`)
	toolRef := regexp.MustCompile(`\bvelruna_[a-z][a-z_]*`)
	files := append(goFilesUnder(t, "cmd"), goFilesUnder(t, "internal")...)
	for _, path := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, m := range cliRef.FindAllStringSubmatch(s, -1) {
				if proseAfterVelruna[m[1]] {
					continue
				}
				if !cliCmds[m[1]] {
					t.Errorf("%s: string names `velruna %s`, not a registered command", path, m[1])
				}
			}
			for _, name := range toolRef.FindAllString(s, -1) {
				if nonToolTokens[name] != "" {
					continue
				}
				if !tools[name] {
					t.Errorf("%s: string names %s, not a registered MCP tool", path, name)
				}
			}
			return true
		})
	}
}

// TestEnvListsMatchTheLoader: the env-var lists in README and setup.md must
// name exactly what our code and compose consume — no ghost variables (the
// OPENSEARCH_CLUSTER omission survived one full round) and no consumed
// variable left undocumented.
func TestEnvListsMatchTheLoader(t *testing.T) {
	consumed := codeEnvNames(t)
	compose := composeEnvNames(t)
	envToken := regexp.MustCompile("`([A-Z][A-Z0-9_]{3,})`")

	for _, f := range []string{"README.md", "docs/setup.md"} {
		documented := map[string]bool{}
		for _, m := range envToken.FindAllStringSubmatch(readFile(t, f), -1) {
			name := m[1]
			if strings.HasPrefix(name, "OPENSEARCH_") || strings.HasPrefix(name, "VELRUNA_") {
				documented[name] = true
			}
		}
		if len(documented) == 0 {
			t.Fatalf("%s: no backticked env tokens parsed — the doc format changed and this check went blind", f)
		}
		for name := range documented {
			if !consumed[name] && !compose[name] && !docOnlyEnv[name] {
				t.Errorf("%s documents %s but neither our Go code nor docker/opensearch.yml consumes it", f, name)
			}
		}
	}
	// setup.md claims its list is complete: every repo-consumed knob var
	// (our two prefixes; ambient names like DBUS_* are not our contract)
	// must appear.
	setup := readFile(t, "docs/setup.md")
	for name := range consumed {
		if !strings.HasPrefix(name, "OPENSEARCH_") && !strings.HasPrefix(name, "VELRUNA_") {
			continue
		}
		if !strings.Contains(setup, "`"+name+"`") {
			t.Errorf("setup.md env list is complete but misses %s (read by our Go code)", name)
		}
	}
}

// TestImagePinsAgree: every ghcr pin named in the docs must be the same
// published tag — the round where README said :0.8.9 while the advice said
// :0.8.10 (and one commit shipped :0.9.0, a tag with no release).
func TestImagePinsAgree(t *testing.T) {
	pin := regexp.MustCompile(`ghcr\.io/nemonobody/velruna:([A-Za-z0-9._-]+)`)
	want := ""
	for _, f := range docFiles(t) {
		for _, m := range pin.FindAllStringSubmatch(readFile(t, f), -1) {
			tag := m[1]
			if tag == "dev" { // the local-build escape hatch
				continue
			}
			if want == "" {
				want = tag
			} else if tag != want {
				t.Errorf("%s pins ghcr ...velruna:%s while other docs pin :%s", f, tag, want)
			}
		}
	}
	if want == "" {
		t.Fatal("no image pins found in the docs — did the pin pattern rot?")
	}
}

// TestComposeProfilesDocumented: every compose profile must appear in
// docs/setup.md — the profile tables drift when a stack profile is added
// (secure-dash appeared in compose before its docs line).
func TestComposeProfilesDocumented(t *testing.T) {
	body := readFile(t, "docker/opensearch.yml")
	re := regexp.MustCompile(`profiles:\s*\[([^\]]*)\]`)
	profiles := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		for _, p := range strings.Split(m[1], ",") {
			p = strings.Trim(strings.TrimSpace(p), `"`)
			if p != "" {
				profiles[p] = true
			}
		}
	}
	if len(profiles) == 0 {
		t.Fatal("no compose profiles parsed — did the compose format change?")
	}
	setup := readFile(t, "docs/setup.md")
	for p := range profiles {
		if !strings.Contains(setup, "`"+p+"`") {
			t.Errorf("compose profile %q is not named in docs/setup.md", p)
		}
	}
}
