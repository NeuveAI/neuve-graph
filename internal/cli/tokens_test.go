package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/entireio/entire-graph/internal/sem"
)

// tokensStream runs symbols --outline --tokens on a working tree and returns
// the tokens records by path, after checking each follows its own outline.
func tokensStream(t *testing.T, repo string, extra ...string) (map[string]sem.FileTokensRecord, sem.SnapshotSummary) {
	t.Helper()
	args := append([]string{"symbols", "--repo", repo, "--worktree", "--profile", "syntax-only", "--outline", "--tokens"}, extra...)
	output := runOutlineCLI(t, args...)
	records := map[string]sem.FileTokensRecord{}
	var summary sem.SnapshotSummary
	lastOutline := ""
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte("\n")) {
		var head struct {
			RecordType string `json:"record_type"`
			FilePath   string `json:"file_path"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			t.Fatal(err)
		}
		switch head.RecordType {
		case "file_outline":
			lastOutline = head.FilePath
			continue
		case "file_tokens":
			if head.FilePath != lastOutline {
				t.Fatalf("tokens for %s does not follow its outline (last outline %q)", head.FilePath, lastOutline)
			}
			if _, duplicate := records[head.FilePath]; duplicate {
				t.Fatalf("duplicate tokens record for %s", head.FilePath)
			}
			var record sem.FileTokensRecord
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatal(err)
			}
			records[head.FilePath] = record
		case "summary":
			if err := json.Unmarshal(line, &summary); err != nil {
				t.Fatal(err)
			}
		}
		lastOutline = ""
	}
	return records, summary
}

type tokenLeaf struct {
	typ, text, ancestors     string
	start, end, line, endRow int
	flags, opens             int
}

func tokenLeaves(record sem.FileTokensRecord, src []byte) []tokenLeaf {
	leaves := make([]tokenLeaf, 0, len(record.Leaves))
	for _, row := range record.Leaves {
		leaves = append(leaves, tokenLeaf{
			typ: record.LeafTypes[row[0]], text: string(src[row[1]:row[2]]), ancestors: record.Ancestors[row[6]],
			start: row[1], end: row[2], line: row[3], endRow: row[4], flags: row[5], opens: row[7],
		})
	}
	return leaves
}

func onlySpace(segment []byte) bool {
	segment = bytes.TrimPrefix(segment, []byte{0xEF, 0xBB, 0xBF})
	for len(segment) > 0 {
		r, size := utf8.DecodeRune(segment)
		if !unicode.IsSpace(r) {
			return false
		}
		segment = segment[size:]
	}
	return true
}

// The consumer slices each leaf's text from the file, so every non-whitespace
// byte must lie in exactly one row, rows must come in source order, and each
// row's lines must be the lines its bytes are on (a multi-line leaf carries
// both). One fixture per grammar the consumer relies on, each with multi-line
// leaves (template text, block comments, raw strings, docstrings).
func TestSymbolsTokensTileTheFile(t *testing.T) {
	repo := t.TempDir()
	files := map[string]string{
		"a.ts":  "import { A } from './a';\n/* block\n   comment */\nconst s = `one\n  two ${A} three`;\nlet x = a?.b ?? c!;\n",
		"b.tsx": "export const C = () => (\n  <div className=\"x\">\n    some text {/* note */}\n    {`t ${v}`}\n  </div>\n);\n",
		"c.js":  "/* two\n lines */\nconst re = /a+b/g;\nconst o = { 'k': 1, [k]: 2, ...rest };\nlabel: for (;;) { break label; }\n",
		"d.go":  "package p\n\nimport \"fmt\"\n\n// Doc\nfunc F() {\n\tfmt.Println(`raw\nstring`, 'x', 1.5i)\n}\n",
		"e.py":  "def f(a, *b, **c):\n    \"\"\"doc\n    string\"\"\"\n    return f\"{a!r:>10} and {b}\"\n",
		"f.rs":  "/// Outer doc\n//! inner\nfn main() { let s = r#\"raw\nmore\"#; /* b */ println!(\"{}\", s); 'l: loop { break 'l; } }\n",
	}
	for path, content := range files {
		write(t, repo, path, content)
	}
	records, _ := tokensStream(t, repo)
	if len(records) != len(files) {
		t.Fatalf("got %d tokens records, want %d", len(records), len(files))
	}
	for path, content := range files {
		record := records[path]
		src := []byte(content)
		pos, multiLine := 0, false
		for i, leaf := range tokenLeaves(record, src) {
			if leaf.start < pos || leaf.end < leaf.start {
				t.Fatalf("%s: row %d [%d,%d) out of order after %d", path, i, leaf.start, leaf.end, pos)
			}
			if !onlySpace(src[pos:leaf.start]) {
				t.Fatalf("%s: bytes %q at %d not covered by any row", path, src[pos:leaf.start], pos)
			}
			wantStart := bytes.Count(src[:leaf.start], []byte("\n")) + 1
			wantEnd := wantStart
			if leaf.end > leaf.start {
				wantEnd = bytes.Count(src[:leaf.end-1], []byte("\n")) + 1
			}
			if leaf.line != wantStart || leaf.endRow != wantEnd {
				t.Fatalf("%s: row %q lines %d-%d, want %d-%d", path, leaf.text, leaf.line, leaf.endRow, wantStart, wantEnd)
			}
			multiLine = multiLine || leaf.endRow > leaf.line
			pos = leaf.end
		}
		if !onlySpace(src[pos:]) {
			t.Fatalf("%s: tail %q not covered", path, src[pos:])
		}
		if !multiLine {
			t.Errorf("%s: fixture has no multi-line leaf", path)
		}
		if record.HasErrors {
			t.Errorf("%s: has_errors on a clean file", path)
		}
		// A covered language with no import statement says [] (extracted, none),
		// never an absent field that reads as "not extracted".
		if path == "b.tsx" && (record.ImportBindings != "extracted" || record.Imports == nil || len(record.Imports) != 0) {
			t.Errorf("b.tsx: import_bindings %q imports %#v, want extracted and []", record.ImportBindings, record.Imports)
		}
	}
}

// Contextual keywords: the tree, not the text, decides. Each word appears once
// as a keyword (an anonymous leaf, never an identifier) and once as a name (an
// identifier leaf). The table pins the line of each use.
func TestSymbolsTokensContextualKeywords(t *testing.T) {
	keywords := "import type { A } from \"./a\";\n" + // 1 type, from
		"import { B as C } from \"./b\";\n" + // 2 as
		"for (const v of list) {}\n" + // 3 of
		"class K {\n" + // 4
		"  get g() { return 1; }\n" + // 5 get
		"  set g(v) {}\n" + // 6 set
		"  static s = 1;\n" + // 7 static
		"  readonly r = 1;\n" + // 8 readonly
		"  declare d: number;\n" + // 9 declare
		"  override o() {}\n" + // 10 override
		"  accessor acc = 1;\n" + // 11 accessor
		"}\n" + // 12
		"async function af() { await p; }\n" + // 13 async, await
		"const sat = x satisfies T;\n" + // 14 satisfies
		"type Inf<T> = T extends (infer U)[] ? U : never;\n" + // 15 infer
		"type Ko = keyof T;\n" + // 16 keyof
		"function isS(v: unknown): v is string { return true; }\n" + // 17 is
		"function asr(v: unknown): asserts v {}\n" + // 18 asserts
		"using res = open();\n" // 19 using
	keywordLine := map[string]int{"type": 1, "from": 1, "as": 2, "of": 3, "get": 5, "set": 6, "static": 7, "readonly": 8, "declare": 9, "override": 10, "accessor": 11, "async": 13, "await": 13, "satisfies": 14, "infer": 15, "keyof": 16, "is": 17, "asserts": 18, "using": 19}
	// As names: a property name for every word, and a plain variable for the
	// words the grammar accepts as one (`as`, `await`, `satisfies` and `using`
	// parse as keywords in that position).
	var names strings.Builder
	words := make([]string, 0, len(keywordLine))
	for word := range keywordLine {
		words = append(words, word)
	}
	slices.Sort(words)
	nameLine := map[string]int{}
	line := 0
	for _, word := range words {
		names.WriteString("o." + word + " = 1;\n")
		line++
		nameLine[word] = line
	}
	for _, word := range []string{"type", "from", "of", "get", "set", "async", "static", "readonly", "declare", "infer", "keyof", "is", "asserts", "override", "accessor"} {
		names.WriteString("let " + word + " = 1;\n")
	}
	repo := t.TempDir()
	write(t, repo, "keywords.ts", keywords)
	write(t, repo, "names.ts", names.String())
	records, summary := tokensStream(t, repo)
	if len(summary.PartialFailures) != 0 {
		t.Fatalf("fixtures must parse cleanly: %#v", summary.PartialFailures)
	}
	keywordLeaves := tokenLeaves(records["keywords.ts"], []byte(keywords))
	nameLeaves := tokenLeaves(records["names.ts"], []byte(names.String()))
	for _, word := range words {
		found := false
		for _, leaf := range keywordLeaves {
			if leaf.text == word && leaf.line == keywordLine[word] {
				found = true
				if leaf.flags&(1|8) != 0 || leaf.typ != word {
					t.Errorf("%s used as a keyword: type %q flags %d, want an anonymous non-identifier leaf", word, leaf.typ, leaf.flags)
				}
			}
		}
		if !found {
			t.Errorf("%s: no keyword leaf on line %d", word, keywordLine[word])
		}
		found = false
		for _, leaf := range nameLeaves {
			if leaf.text == word && leaf.line == nameLine[word] {
				found = true
				if leaf.flags&1 == 0 {
					t.Errorf("%s used as a name: type %q flags %d, want an identifier", word, leaf.typ, leaf.flags)
				}
			}
		}
		if !found {
			t.Errorf("%s: no name leaf on line %d", word, nameLine[word])
		}
		if !slices.Contains(records["names.ts"].IdentifierNames, word) {
			t.Errorf("%s missing from identifier_names of the name file", word)
		}
		if word != "type" && slices.Contains(records["keywords.ts"].IdentifierNames, word) {
			t.Errorf("%s as a keyword reached identifier_names", word)
		}
	}
}

// identifier_names must hold every use of a name, since the consumer reads
// "absent" as "unused". One imported name in each position, one per line; a
// name only in a comment or a string must not count.
func TestSymbolsTokensIdentifierNamesAreComplete(t *testing.T) {
	source := "import { Used } from \"./m\";\n" + // 1 import
		"// CommentOnly\n" + // 2
		"const s = \"StringOnly\";\n" + // 3
		"Used();\n" + // 4 call
		"let a: Used;\n" + // 5 type annotation
		"const e = <Used prop={1} />;\n" + // 6 JSX element
		"const e2 = <div Used={1} />;\n" + // 7 JSX attribute
		"const o = { Used };\n" + // 8 property shorthand
		"const t = `x ${Used} y`;\n" + // 9 template expression
		"type Q = typeof Used;\n" + // 10 typeof
		"@Used class D {}\n" + // 11 decorator
		"export { Used };\n" + // 12 export list
		"const g = f<Used>();\n" // 13 generic argument
	repo := t.TempDir()
	write(t, repo, "p.tsx", source)
	records, _ := tokensStream(t, repo)
	record := records["p.tsx"]
	lines := map[int]bool{}
	for _, leaf := range tokenLeaves(record, []byte(source)) {
		if leaf.text == "Used" && leaf.flags&1 != 0 {
			lines[leaf.line] = true
		}
	}
	for line := 4; line <= 13; line++ {
		if !lines[line] {
			t.Errorf("no identifier leaf Used on line %d", line)
		}
	}
	if !slices.Contains(record.IdentifierNames, "Used") {
		t.Error("identifier_names misses Used")
	}
	for _, hidden := range []string{"CommentOnly", "StringOnly"} {
		if slices.Contains(record.IdentifierNames, hidden) {
			t.Errorf("%s is only in a comment or string but reached identifier_names", hidden)
		}
	}
	if !slices.IsSorted(record.IdentifierNames) {
		t.Error("identifier_names not sorted")
	}
}

// Equal tokens are not the same tree: `return` before a line break returns
// nothing, and a postfix `++` on its own line applies to the next statement.
// The non-comment leaves must then differ in ancestors or opens while types
// and texts agree. A layout-only change (a wrapped argument list, an added
// comment) must give equal leaves.
func TestSymbolsTokensAncestorsTellTreesApart(t *testing.T) {
	cases := []struct {
		name, base, head string
		sameTree         bool
	}{
		{"return line break", "function f(x: number) {\n  return x + 1;\n}\n", "function f(x: number) {\n  return\n  x + 1;\n}\n", false},
		{"postfix on its own line", "let a = 1, b = 2;\na++\nb;\n", "let a = 1, b = 2;\na\n++b;\n", false},
		{"wrapped arguments", "const r = run(first, second, third);\n", "const r = run(\n    first,\n    second,\n    third\n);\n", true},
		{"comment added", "if (ready) {\n  go();\n}\n", "if (ready) {\n  // why\n  go();\n}\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sides := [2][]tokenLeaf{}
			for i, content := range []string{tc.base, tc.head} {
				repo := t.TempDir()
				write(t, repo, "a.ts", content)
				records, _ := tokensStream(t, repo)
				record, ok := records["a.ts"]
				if !ok || record.HasErrors {
					t.Fatalf("side %d: missing or erroneous tokens record", i)
				}
				for _, leaf := range tokenLeaves(record, []byte(content)) {
					if leaf.flags&4 != 0 {
						t.Fatalf("side %d: error leaf %q", i, leaf.text)
					}
					if leaf.flags&2 == 0 {
						sides[i] = append(sides[i], leaf)
					}
				}
			}
			key := func(leaves []tokenLeaf, shape bool) []string {
				out := []string{}
				for _, leaf := range leaves {
					entry := leaf.typ + "\x00" + leaf.text
					if shape {
						entry += "\x00" + leaf.ancestors + "\x00" + string(rune('0'+leaf.opens))
					}
					out = append(out, entry)
				}
				return out
			}
			if tc.sameTree {
				if !reflect.DeepEqual(key(sides[0], true), key(sides[1], true)) {
					t.Fatal("a layout-only change produced different leaves")
				}
				return
			}
			if !reflect.DeepEqual(key(sides[0], false), key(sides[1], false)) {
				t.Fatal("fixture should keep types and texts equal; only the tree differs")
			}
			if reflect.DeepEqual(key(sides[0], true), key(sides[1], true)) {
				t.Fatal("ancestors and opens failed to tell two different trees apart")
			}
		})
	}
}

// A file whose tree has an error gets no outline today, so it gets no tokens
// record either, and the existing partial failure says why. A file read
// through a rewritten view keeps the tiling, marks the rewritten span as an
// error and still lists the names inside it.
func TestSymbolsTokensErrorsAndRewrittenSpans(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "broken.ts", "export function broken( {\n")
	rewritten := "interface Fn {\n  <T>(value: HiddenName): T;\n}\ntype M = typeof import(\"./m\");\n"
	write(t, repo, "masked.ts", rewritten)
	records, summary := tokensStream(t, repo)
	if _, ok := records["broken.ts"]; ok {
		t.Error("a file with no outline got a tokens record")
	}
	if len(summary.PartialFailures) != 1 || summary.PartialFailures[0].FilePath != "broken.ts" || summary.PartialFailures[0].Code != "E_PARSE_ERROR" {
		t.Errorf("parse failure changed: %#v", summary.PartialFailures)
	}
	record, ok := records["masked.ts"]
	if !ok {
		t.Fatal("masked file lost its tokens record")
	}
	src := []byte(rewritten)
	pos, rewrittenRows := 0, 0
	for _, leaf := range tokenLeaves(record, src) {
		if !onlySpace(src[pos:leaf.start]) {
			t.Fatalf("bytes %q not covered", src[pos:leaf.start])
		}
		if leaf.flags&16 != 0 {
			rewrittenRows++
			if leaf.flags&4 == 0 || leaf.flags&1 != 0 || leaf.typ != "" {
				t.Errorf("rewritten row %q: type %q flags %d", leaf.text, leaf.typ, leaf.flags)
			}
		}
		pos = leaf.end
	}
	if rewrittenRows != 2 {
		t.Errorf("got %d rewritten rows, want 2 (the call signature and the type query)", rewrittenRows)
	}
	if !slices.Contains(record.IdentifierNames, "HiddenName") {
		t.Errorf("a name inside a rewritten span is missing: %v", record.IdentifierNames)
	}
}

// Import bindings for every form the consumer distinguishes, per language;
// re-exports, dynamic import() and require() calls are not import statements.
func TestSymbolsTokensImportBindings(t *testing.T) {
	type binding = sem.ImportBindingRecord
	type statement = sem.ImportStatementRecord
	b := func(imported, local string, typeOnly bool) binding {
		return binding{Imported: imported, Local: local, TypeOnly: typeOnly}
	}
	cases := map[string]struct {
		source string
		want   []statement
	}{
		"a.ts": {
			"import D from \"d\";\nimport { n, o as p } from \"np\";\nimport * as ns from \"ns\";\nimport type T from \"t\";\n" +
				"import type { U, V as W } from \"uv\";\nimport { type X, Y } from \"xy\";\nimport \"side\";\nimport M, { m } from \"mixed\";\n" +
				"import {\n  multi,\n} from \"lines\";\nimport req = require(\"req\");\nimport alias = Space.Inner;\n" +
				"export { n } from \"np\";\nexport * from \"star\";\nconst lazy = import(\"lazy\");\n",
			[]statement{
				{StartLine: 1, EndLine: 1, Specifier: "d", Bindings: []binding{b("default", "D", false)}},
				{StartLine: 2, EndLine: 2, Specifier: "np", Bindings: []binding{b("n", "n", false), b("o", "p", false)}},
				{StartLine: 3, EndLine: 3, Specifier: "ns", Bindings: []binding{b("*", "ns", false)}},
				{StartLine: 4, EndLine: 4, Specifier: "t", Bindings: []binding{b("default", "T", true)}},
				{StartLine: 5, EndLine: 5, Specifier: "uv", Bindings: []binding{b("U", "U", true), b("V", "W", true)}},
				{StartLine: 6, EndLine: 6, Specifier: "xy", Bindings: []binding{b("X", "X", true), b("Y", "Y", false)}},
				{StartLine: 7, EndLine: 7, Specifier: "side", Bindings: []binding{}, SideEffectOnly: true},
				{StartLine: 8, EndLine: 8, Specifier: "mixed", Bindings: []binding{b("default", "M", false), b("m", "m", false)}},
				{StartLine: 9, EndLine: 11, Specifier: "lines", Bindings: []binding{b("multi", "multi", false)}},
				{StartLine: 12, EndLine: 12, Specifier: "req", Bindings: []binding{b("=", "req", false)}},
			},
		},
		"b.js": {
			"import D, * as all from './d.js';\nimport { a as b } from './a.js';\nimport './side.js';\nconst c = require('c');\nexport { b } from './a.js';\n",
			[]statement{
				{StartLine: 1, EndLine: 1, Specifier: "./d.js", Bindings: []binding{b("default", "D", false), b("*", "all", false)}},
				{StartLine: 2, EndLine: 2, Specifier: "./a.js", Bindings: []binding{b("a", "b", false)}},
				{StartLine: 3, EndLine: 3, Specifier: "./side.js", Bindings: []binding{}, SideEffectOnly: true},
			},
		},
		"c.go": {
			"package p\n\nimport \"fmt\"\nimport str \"strings\"\nimport (\n\t_ \"embed\"\n\t. \"math\"\n\t\"gopkg.in/yaml.v3\"\n\t\"example.com/mod/v2\"\n)\n",
			[]statement{
				{StartLine: 3, EndLine: 3, Specifier: "fmt", Bindings: []binding{{Imported: "*", Local: "fmt", LocalInferred: true}}},
				{StartLine: 4, EndLine: 4, Specifier: "strings", Bindings: []binding{b("*", "str", false)}},
				{StartLine: 6, EndLine: 6, Specifier: "embed", Bindings: []binding{}, SideEffectOnly: true},
				{StartLine: 7, EndLine: 7, Specifier: "math", Bindings: []binding{}, NamesUnknown: true},
				{StartLine: 8, EndLine: 8, Specifier: "gopkg.in/yaml.v3", Bindings: []binding{{Imported: "*", Local: "yaml", LocalInferred: true}}},
				{StartLine: 9, EndLine: 9, Specifier: "example.com/mod/v2", Bindings: []binding{{Imported: "*", Local: "mod", LocalInferred: true}}},
			},
		},
		"d.py": {
			"import a\nimport a.b as c\nimport x.y, z\nfrom a import b as c2, d\nfrom q import *\nfrom . import sib\nfrom ..pkg.m import (e,\n    f as g)\n",
			[]statement{
				{StartLine: 1, EndLine: 1, Specifier: "a", Bindings: []binding{b("*", "a", false)}},
				{StartLine: 2, EndLine: 2, Specifier: "a.b", Bindings: []binding{b("*", "c", false)}},
				{StartLine: 3, EndLine: 3, Specifier: "x.y", Bindings: []binding{b("*", "x", false)}},
				{StartLine: 3, EndLine: 3, Specifier: "z", Bindings: []binding{b("*", "z", false)}},
				{StartLine: 4, EndLine: 4, Specifier: "a", Bindings: []binding{b("b", "c2", false), b("d", "d", false)}},
				{StartLine: 5, EndLine: 5, Specifier: "q", Bindings: []binding{}, NamesUnknown: true},
				{StartLine: 6, EndLine: 6, Specifier: ".", Bindings: []binding{b("sib", "sib", false)}},
				{StartLine: 7, EndLine: 8, Specifier: "..pkg.m", Bindings: []binding{b("e", "e", false), b("f", "g", false)}},
			},
		},
	}
	repo := t.TempDir()
	for path, tc := range cases {
		write(t, repo, path, tc.source)
	}
	write(t, repo, "e.rs", "use std::fmt;\nfn main() {}\n")
	records, _ := tokensStream(t, repo)
	for path, tc := range cases {
		record := records[path]
		if record.ImportBindings != "extracted" {
			t.Errorf("%s: import_bindings %q", path, record.ImportBindings)
		}
		if !reflect.DeepEqual(record.Imports, tc.want) {
			got, _ := json.Marshal(record.Imports)
			want, _ := json.Marshal(tc.want)
			t.Errorf("%s imports\n got %s\nwant %s", path, got, want)
		}
	}
	// A language outside the binding coverage says so, never "no imports".
	if record := records["e.rs"]; record.ImportBindings != "not_extracted" || record.Imports != nil {
		t.Errorf("rust: import_bindings %q imports %#v", record.ImportBindings, record.Imports)
	}
}

// Adding --tokens only adds file_tokens lines: removing them gives back the
// --outline stream byte for byte, and --outline alone is pinned against the
// pre-outline stream by TestSymbolsOutlineRecords.
func TestSymbolsTokensOnlyAddRecords(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "a.ts", "import { x } from './x';\nexport const y = x;\n")
	write(t, repo, "b.go", "package b\n\nfunc B() {}\n")
	write(t, repo, "broken.py", "def (\n")
	for _, profile := range []string{"syntax-only", "fast", "full"} {
		args := []string{"symbols", "--repo", repo, "--worktree", "--profile", profile, "--no-cache", "--outline"}
		plain := runOutlineCLI(t, args...)
		withTokens := runOutlineCLI(t, append(args, "--tokens")...)
		var kept bytes.Buffer
		count := 0
		for _, line := range bytes.SplitAfter(withTokens, []byte("\n")) {
			if bytes.HasPrefix(line, []byte(`{"record_type":"file_tokens"`)) {
				count++
				continue
			}
			kept.Write(line)
		}
		if !bytes.Equal(kept.Bytes(), plain) {
			t.Errorf("%s: --tokens changed bytes beyond its own records", profile)
		}
		if count != 2 {
			t.Errorf("%s: %d tokens records, want 2", profile, count)
		}
	}
}

func TestSymbolsTokensFlagAndCapabilities(t *testing.T) {
	repo := t.TempDir()
	err := Run(t.Context(), Options{Stdout: io.Discard}, []string{"symbols", "--repo", repo, "--tokens"})
	if err == nil || !strings.Contains(err.Error(), "--tokens requires --outline") {
		t.Errorf("--tokens without --outline: %v", err)
	}
	err = Run(t.Context(), Options{Stdout: io.Discard}, []string{"snapshot", "--repo", repo, "--outline", "--tokens"})
	if err == nil || !strings.Contains(err.Error(), "only valid for symbols") {
		t.Errorf("snapshot accepted --outline --tokens: %v", err)
	}
	var capabilities sem.CapabilityReport
	if err := json.Unmarshal(runOutlineCLI(t, "capabilities", "--json"), &capabilities); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"symbols_outline_tokens", "symbols_outline_import_bindings"} {
		if !capabilities.OptionalLocalOnlyFeatures[key] {
			t.Errorf("capabilities omit %s", key)
		}
	}
	if got := capabilities.FeatureLanguages["symbols_outline_import_bindings"]; !reflect.DeepEqual(got, []string{"Go", "JavaScript", "Python", "TypeScript"}) {
		t.Errorf("import binding languages %v", got)
	}
	tokenLanguages := capabilities.FeatureLanguages["symbols_outline_tokens"]
	for _, language := range []string{"Go", "JavaScript", "Python", "Rust", "TypeScript"} {
		if !slices.Contains(tokenLanguages, language) {
			t.Errorf("token languages miss %s: %v", language, tokenLanguages)
		}
	}
	// No tree (JSON, Groovy, Markdown), or a tree but no whitespace proof
	// (YAML, Lua, Swift, Bash) or a disproof (PHP, Kotlin): not declared.
	for _, language := range []string{"JSON", "Groovy", "Markdown", "YAML", "Lua", "Swift", "Bash", "PHP", "Kotlin"} {
		if slices.Contains(tokenLanguages, language) {
			t.Errorf("token languages claim %s", language)
		}
	}
}

// A record carries leaves exactly when its language is declared; an outlined
// file in an undeclared language says language_not_covered and carries none.
func TestSymbolsTokensDeclaredLanguagesMatchOutput(t *testing.T) {
	repo := t.TempDir()
	sources := map[string]string{"a.rb": "def f; end\n", "b.java": "class B {}\n", "c.yaml": "a: 1\n", "d.json": "{}\n", "e.lua": "local v = 1\n"}
	for path, content := range sources {
		write(t, repo, path, content)
	}
	records, _ := tokensStream(t, repo)
	declared := sem.TokenLanguages()
	for path, record := range records {
		if slices.Contains(declared, record.Language) != (record.Status == "extracted") {
			t.Errorf("%s: %s declared %v but status %q", path, record.Language, slices.Contains(declared, record.Language), record.Status)
		}
		if record.Status != "extracted" && (record.Status != "language_not_covered" || record.Leaves != nil || record.IdentifierNames != nil || record.Imports != nil) {
			t.Errorf("%s: undeclared record %#v", path, record)
		}
	}
	for _, path := range []string{"c.yaml", "e.lua"} {
		if records[path].Status != "language_not_covered" {
			t.Errorf("%s: status %q", path, records[path].Status)
		}
	}
	if _, ok := records["d.json"]; ok {
		t.Error("an inventory-only file got tokens")
	}
	if _, err := os.Stat(filepath.Join(repo, "d.json")); err != nil {
		t.Fatal(err)
	}
}

// The leaves have a size limit in the engine: a file up to 512 KiB and 100,000
// leaves is extracted, one byte or leaf more gets a record that says
// over_size_limit with no leaves, names or imports, and its outline is the
// same either way.
func TestSymbolsTokensSizeLimit(t *testing.T) {
	const limit = 512 << 10
	build := func(size int) string {
		var source strings.Builder
		source.WriteString("import { x } from './x';\n")
		for i := 0; source.Len() < size-64; i++ {
			source.WriteString("export const value" + strings.Repeat("a", 8) + "_" + string(rune('a'+i%26)) + " = x;\n")
		}
		source.WriteString("// ")
		source.WriteString(strings.Repeat("p", size-source.Len()-1))
		source.WriteString("\n")
		if source.Len() != size {
			t.Fatalf("built %d bytes, want %d", source.Len(), size)
		}
		return source.String()
	}
	repo := t.TempDir()
	write(t, repo, "at.ts", build(limit))
	write(t, repo, "over.ts", build(limit+1))
	// Dense tables can pass the byte limit and still be too large: 100,000
	// leaves is the row limit. One number and its comma per line (a single long
	// line would read as minified and get no outline) are two leaves per three
	// bytes; the declaration adds seven more.
	dense := func(numbers int) string {
		return "const t = [\n" + strings.Repeat("1,\n", numbers) + "];\n"
	}
	write(t, repo, "dense-at.ts", dense(49_997))
	write(t, repo, "dense-over.ts", dense(50_100))
	records, _ := tokensStream(t, repo)
	if got := records["dense-at.ts"]; got.Status != "extracted" || len(got.Leaves) > 100_000 {
		t.Errorf("dense file under the leaf limit: status %q, %d leaves", got.Status, len(got.Leaves))
	}
	if got := records["dense-over.ts"]; got.Status != "over_size_limit" || got.Leaves != nil {
		t.Errorf("dense file over the leaf limit: status %q, %d leaves", got.Status, len(got.Leaves))
	}
	at, over := records["at.ts"], records["over.ts"]
	if at.Status != "extracted" || len(at.Leaves) == 0 || len(at.Imports) != 1 {
		t.Errorf("at the limit: status %q, %d leaves, %d imports", at.Status, len(at.Leaves), len(at.Imports))
	}
	if over.Status != "over_size_limit" || over.Leaves != nil || over.LeafTypes != nil || over.Ancestors != nil || over.IdentifierNames != nil || over.Imports != nil || over.ImportBindings != "not_extracted" {
		t.Errorf("over the limit: %#v", over)
	}
	plain := runOutlineCLI(t, "symbols", "--repo", repo, "--worktree", "--profile", "syntax-only", "--outline")
	withTokens := runOutlineCLI(t, "symbols", "--repo", repo, "--worktree", "--profile", "syntax-only", "--outline", "--tokens")
	if !bytes.Contains(plain, []byte(`"file_path":"over.ts"`)) {
		t.Fatal("the over-limit file lost its outline")
	}
	var kept bytes.Buffer
	for _, line := range bytes.SplitAfter(withTokens, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte(`{"record_type":"file_tokens"`)) {
			kept.Write(line)
		}
	}
	if !bytes.Equal(kept.Bytes(), plain) {
		t.Error("the size limit changed the outline stream")
	}
}

// A byte-order mark at byte 0 is never a row, in any declared language, and
// the rows after it still tile the file.
func TestSymbolsTokensByteOrderMarkIsNoRow(t *testing.T) {
	repo := t.TempDir()
	files := map[string]string{"a.go": "\uFEFFpackage p\n", "a.ts": "\uFEFFconst v = 1;\n", "a.py": "\uFEFFv = 1\n"}
	for path, content := range files {
		write(t, repo, path, content)
	}
	records, _ := tokensStream(t, repo)
	for path, content := range files {
		record, ok := records[path]
		if !ok || record.Status != "extracted" || len(record.Leaves) == 0 {
			t.Fatalf("%s: %#v", path, record)
		}
		if first := record.Leaves[0]; first[1] < len("\uFEFF") {
			t.Errorf("%s: first row %v covers the byte-order mark", path, first)
		}
		pos := len("\uFEFF")
		for _, row := range record.Leaves {
			if !onlySpace([]byte(content[pos:row[1]])) {
				t.Fatalf("%s: %q not covered", path, content[pos:row[1]])
			}
			pos = row[2]
		}
	}
}
