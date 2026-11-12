package sem

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The scanner's list is kept whole and first; the tree adds only what it
// lacks, in statement order, once each, with the scanner's Python convention.
// Reason: this is the union rule every snapshot path applies, and its
// order is what keeps the old relations first and the bytes deterministic.
func TestWithTreeImportsKeepsTheScannerListFirst(t *testing.T) {
	scanned := []string{"./a", "./z"}
	statements := []treeImportStatement{
		{Specifier: "./z"}, {Specifier: "./m"}, {Specifier: "./b"}, {Specifier: "./m"}, {Specifier: ""},
	}
	got := withTreeImports(scanned, statements)
	if want := []string{"./a", "./z", "./m", "./b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("union %v, want %v", got, want)
	}
	if !reflect.DeepEqual(scanned, []string{"./a", "./z"}) {
		t.Fatalf("the scanner's list was modified: %v", scanned)
	}
	if got := withTreeImports(scanned, []treeImportStatement{{Specifier: "./a"}}); &got[0] != &scanned[0] {
		t.Error("a union that adds nothing must return the scanner's list itself")
	}
	if got := withTreeImports(nil, nil); got != nil {
		t.Errorf("no statements, no scanner list: %v", got)
	}
	python := withTreeImports([]string{"os"}, []treeImportStatement{
		{Specifier: "os"},
		{Specifier: "json"},
		{Specifier: "os.path"},
		{Specifier: "m", PythonFrom: true, Names: []string{"a", "b"}},
		{Specifier: "p.q", PythonFrom: true, Names: []string{"r"}},
		{Specifier: ".", PythonFrom: true, Names: []string{"x"}},
		{Specifier: "..", PythonFrom: true},
		{Specifier: ".pkg", PythonFrom: true, Names: []string{"y"}},
		{Specifier: "w", PythonFrom: true},
	})
	if want := []string{"os", "json", "os.path", "m", "m.a", "m.b", "p.q", ".x", "..", ".pkg", "w"}; !reflect.DeepEqual(python, want) {
		t.Fatalf("python union %v, want %v", python, want)
	}
}

// The relation side's statements are the tokens record's, from one parse and
// one node list: the same specifiers in the same order. Reason: the promise a
// consumer relies on joins exactly these two lists, so they must not drift.
func TestTreeImportStatementsAreTheTokensRecordStatements(t *testing.T) {
	spec := resolveProfile(ProfileFull)
	spec.outline, spec.tokens = true, true
	fixtures := map[string]string{
		"a.ts":  "import first from \"./first\";\nimport {\n  a,\n  b,\n} from \"./multi\";\nimport type {\n  T,\n} from \"./types\";\nimport x = require(\"./req\");\ndeclare module \"m\" {\n  import inner from \"./inner\";\n}\n",
		"b.tsx": "import React from 'react';\nexport const C = () => <div/>;\n",
		"c.js":  "import {\n  x\n} from './x.js';\nimport './side.js';\nfunction f() { return require('./r.js'); }\n",
		"d.go":  "package d\n\nimport (\n\t/* note */ \"fmt\"\n\tstr \"strings\"\n\t_ \"embed\"\n\t. \"math\"\n\t`os`\n)\n\nfunc F() { fmt.Println(str.ToUpper(\"\"), Pi, os.Args) }\n",
		"e.py":  "import os, json\nimport os . path\nfrom . import x\nfrom .. pkg import y\nfrom m import (\n    a,\n    b as c,\n)\nfrom w import *\ndef f():\n    import inner\n",
	}
	for path, source := range fixtures {
		langSpec, ok := languageForPath(path)
		if !ok {
			t.Fatalf("%s: no language", path)
		}
		_, _, status, _, tokens, imports := parseWithOutlineTokensImports(spec, langSpec, path, source)
		if status.ParseError || tokens == nil || tokens.ImportBindings != importBindingsExtracted {
			t.Fatalf("%s: tokens record %+v, status %+v", path, tokens, status)
		}
		if len(tokens.Imports) == 0 {
			t.Fatalf("%s: tokens record has no import statements", path)
		}
		var fromTokens, fromRelations []string
		for _, statement := range tokens.Imports {
			fromTokens = append(fromTokens, statement.Specifier)
		}
		for _, statement := range imports {
			fromRelations = append(fromRelations, statement.Specifier)
		}
		if !reflect.DeepEqual(fromTokens, fromRelations) {
			t.Errorf("%s: tokens record %q, relation side %q", path, fromTokens, fromRelations)
		}
		// Without --outline or --tokens the same statements come from the plain parse.
		_, _, _, _, _, plain := parseWithOutlineTokensImports(resolveProfile(ProfileFull), langSpec, path, source)
		if !reflect.DeepEqual(plain, imports) {
			t.Errorf("%s: statements depend on --tokens: %+v vs %+v", path, plain, imports)
		}
	}
}

// A file with no usable tree keeps the scanner's answer alone. Reason: the
// tree is read only where it is trustworthy, as for the tokens record.
func TestTreeImportStatementsNeedAUsableTree(t *testing.T) {
	spec := resolveProfile(ProfileFull)
	cases := map[string]string{
		"broken.ts": "import {\n  a,\n} from \"./multi\";\nexport const = ;\n",
		"broken.py": "import os, json\ndef f(:\n",
		"lib.rs":    "use std::fmt;\nfn main() {}\n",
		"A.java":    "import java.util.List;\nclass A {}\n",
	}
	for path, source := range cases {
		langSpec, ok := languageForPath(path)
		if !ok {
			t.Fatalf("%s: no language", path)
		}
		_, _, _, _, _, imports := parseWithOutlineTokensImports(spec, langSpec, path, source)
		if imports != nil {
			t.Errorf("%s: statements %+v from a file with no usable tree", path, imports)
		}
	}
}

func importsByFile(snapshot ProviderSnapshot) map[string][]string {
	out := map[string][]string{}
	for _, relation := range snapshot.Relations {
		if relation.Type != "IMPORTS" {
			continue
		}
		from := relation.FromID[strings.LastIndex(relation.FromID, ":file:")+len(":file:"):]
		out[from] = append(out[from], relation.ToID)
	}
	return out
}

func nonImportRelations(snapshot ProviderSnapshot) []RelationRecord {
	var out []RelationRecord
	for _, relation := range snapshot.Relations {
		if relation.Type != "IMPORTS" {
			out = append(out, relation)
		}
	}
	return out
}

// Every path that builds relations gains the tree's specifiers: a cold
// interactive build, a search snapshot read back from its cache, and a
// selective view derived from that cache. Call resolution is not one of the
// readers: with the tree's statements removed, every relation other than
// IMPORTS is the same. A file over the parse limit has no tree and keeps the
// scanner's answer. Reason: one rule everywhere unless a path has no tree,
// and the call resolver's inputs stay as they were (point 4 of the brief).
func TestTreeImportsReachEverySnapshotPath(t *testing.T) {
	repo, cacheDir := t.TempDir(), t.TempDir()
	initRepo(t, repo)
	writeFile(t, repo, "web/util.ts", "export function helper() { return 1; }\n")
	writeFile(t, repo, "web/other.ts", "export function other() { return 2; }\n")
	writeFile(t, repo, "web/main.ts", "import {\n  helper,\n} from \"./util\";\nimport { other } from \"./other\";\nexport function run() { return helper() + other(); }\n")
	writeFile(t, repo, "web/big.ts", "import {\n  helper,\n} from \"./util\";\nexport const big = \""+strings.Repeat("x", 2048)+"\";\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "fixture")
	utilID := fileID(repoKey(t.Context(), repo), "web/util.ts")
	hasUtil := func(targets []string) bool {
		for _, target := range targets {
			if target == utilID {
				return true
			}
		}
		return false
	}

	opts := ProviderSnapshotOptions{Profile: ProfileFull}
	cold, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !hasUtil(importsByFile(cold)["web/main.ts"]) {
		t.Fatalf("cold build: no IMPORTS from the multi-line import: %v", importsByFile(cold))
	}
	if _, _, err := PreindexProviderSnapshot(t.Context(), repo, "test", opts, cacheDir); err != nil {
		t.Fatal(err)
	}
	cached, hit, err := PreindexProviderSnapshot(t.Context(), repo, "test", opts, cacheDir)
	if err != nil || !hit {
		t.Fatalf("second preindex: hit=%v err=%v", hit, err)
	}
	if !reflect.DeepEqual(importsByFile(cached), importsByFile(cold)) {
		t.Fatalf("cached snapshot IMPORTS %v, cold %v", importsByFile(cached), importsByFile(cold))
	}
	selective := opts
	selective.OnlyFiles = []string{"web/main.ts", "web/util.ts"}
	derived, err := selectiveSearchSnapshotFromFull(t.Context(), repo, "test", selective, cached)
	if err != nil {
		t.Fatal(err)
	}
	coldSelective, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", selective)
	if err != nil {
		t.Fatal(err)
	}
	sortRelations := func(relations []RelationRecord) []RelationRecord {
		out := append([]RelationRecord(nil), relations...)
		sort.Slice(out, func(i, j int) bool {
			return out[i].Type+out[i].FromID+out[i].ToID < out[j].Type+out[j].FromID+out[j].ToID
		})
		return out
	}
	if !hasUtil(importsByFile(derived)["web/main.ts"]) || !reflect.DeepEqual(sortRelations(derived.Relations), sortRelations(coldSelective.Relations)) {
		t.Fatalf("selective view from the cache differs from a cold selective build:\n%v\n%v", derived.Relations, coldSelective.Relations)
	}

	// The same derivation with the tree's statements removed: only IMPORTS move.
	stripped := cached
	stripped.Files = append([]FileRecord(nil), cached.Files...)
	for index := range stripped.Files {
		stripped.Files[index].treeImports = nil
	}
	scannerOnly, err := selectiveSearchSnapshotFromFull(t.Context(), repo, "test", selective, stripped)
	if err != nil {
		t.Fatal(err)
	}
	if hasUtil(importsByFile(scannerOnly)["web/main.ts"]) {
		t.Fatal("fixture: the scanner reads the multi-line import, so it proves nothing")
	}
	if len(nonImportRelations(derived)) == 0 || !reflect.DeepEqual(nonImportRelations(derived), nonImportRelations(scannerOnly)) {
		t.Fatalf("relations other than IMPORTS depend on the tree's statements:\n%v\n%v", nonImportRelations(derived), nonImportRelations(scannerOnly))
	}

	limited := opts
	limited.MaxParseBytes = 1024
	small, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", limited)
	if err != nil {
		t.Fatal(err)
	}
	if hasUtil(importsByFile(small)["web/big.ts"]) || !hasUtil(importsByFile(small)["web/main.ts"]) {
		t.Fatalf("over the parse limit the scanner alone answers: %v", importsByFile(small))
	}
}

// f011eb2 wrote these keys. Reason: a records, search or extraction cache it
// wrote holds IMPORTS without the tree's specifiers and must never be read as
// this build's answer.
func TestCachesWrittenBeforeTreeImportsAreNotServed(t *testing.T) {
	for _, old := range []string{"provider-records-v15-5", "search-snapshot-v20-5", "file-extraction-v2-5", "prepared-generation-v1-5"} {
		for _, current := range []string{providerRecordsCacheVersion, searchSnapshotCacheVersion, extractionCacheVersion, preparedGenerationVersion} {
			if old == current {
				t.Errorf("cache version %q is still current", old)
			}
		}
	}
}
