package sem

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem/pgsql"
	sitter "github.com/smacker/go-tree-sitter"
	treesittertsx "github.com/smacker/go-tree-sitter/typescript/tsx"
)

// tokenGrammars lists every grammar each language label can be parsed with:
// the extension table plus the routes ParseWithStatus takes (SQL through
// pgsql, Flow JavaScript through TSX).
func tokenGrammars() map[string][]*sitter.Language {
	grammars := map[string][]*sitter.Language{}
	add := func(language string, grammar *sitter.Language) {
		if !slices.Contains(grammars[language], grammar) {
			grammars[language] = append(grammars[language], grammar)
		}
	}
	for _, spec := range treeSitterLanguages {
		if spec.grammar != nil && spec.language != "Groovy" {
			add(spec.language, spec.grammar)
		}
	}
	add("SQL", pgsql.GetLanguage())
	add("JavaScript", treesittertsx.GetLanguage())
	return grammars
}

// Every declared language is one Graph parses with tree-sitter, and every
// listed type must be a named node of one of its grammars, so a typo cannot
// hide a whole class of names or of literal content. A parsed language that is
// not declared gets a record saying so (TestFileTokensUndeclaredLanguage).
func TestTokenClassTableCoversTreeSitterLanguages(t *testing.T) {
	grammars := tokenGrammars()
	for language, classes := range tokenClassTable {
		languageGrammars, ok := grammars[language]
		if !ok {
			t.Errorf("%s is classified but not parsed with tree-sitter", language)
			continue
		}
		named := map[string]bool{}
		for _, grammar := range languageGrammars {
			for i := uint32(0); i < grammar.SymbolCount(); i++ {
				if grammar.SymbolType(sitter.Symbol(i)) == sitter.SymbolTypeRegular {
					named[grammar.SymbolName(sitter.Symbol(i))] = true
				}
			}
		}
		for _, typ := range slices.Concat(classes.identifiers, classes.comments, classes.content) {
			if !named[typ] {
				t.Errorf("%s: %q is not a named node of its grammars", language, typ)
			}
		}
		if !slices.IsSorted(classes.identifiers) || !slices.IsSorted(classes.comments) || !slices.IsSorted(classes.content) {
			t.Errorf("%s: keep the lists sorted so the doc mirror is stable", language)
		}
	}
}

// docs/snapshot-format.md states, per language, the node types read as
// identifiers and comments. It mirrors the code table exactly.
func TestTokenClassificationDocMirrorsTable(t *testing.T) {
	var table strings.Builder
	table.WriteString("| Language | Identifier leaf types | Comment node types | Content node types |\n| --- | --- | --- | --- |\n")
	for _, language := range TokenLanguages() {
		classes := tokenClassTable[language]
		quote := func(types []string) string {
			if len(types) == 0 {
				return "none"
			}
			quoted := make([]string, len(types))
			for i, typ := range types {
				quoted[i] = "`" + typ + "`"
			}
			return strings.Join(quoted, ", ")
		}
		table.WriteString("| " + language + " | " + quote(classes.identifiers) + " | " + quote(classes.comments) + " | " + quote(classes.content) + " |\n")
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "snapshot-format.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), table.String()) {
		t.Errorf("docs/snapshot-format.md does not mirror tokenClassTable; expected table:\n%s", table.String())
	}
}

// Protocol Buffers is parsed through a view with a synthetic syntax line
// inserted. It is not declared yet, so its record says so and carries no rows
// (the view mapping that keeps the line out of rows is in place for when a
// literal-whitespace case declares it).
func TestFileTokensProtocolBuffersNotDeclared(t *testing.T) {
	record := fileTokensFor(t, "a.proto", "message Item {\n  string name = 1;\n}\n")
	if record.Status != tokensLanguageNotCovered || record.Leaves != nil {
		t.Fatalf("protocol buffers: %#v", record)
	}
}

func fileTokensFor(t *testing.T, path, source string) *FileTokensRecord {
	t.Helper()
	spec := resolveProfile(ProfileSyntaxOnly)
	spec.outline, spec.tokens = true, true
	langSpec, ok := languageForPath(path)
	if !ok {
		t.Fatalf("%s: no language", path)
	}
	_, _, status, _, tokens := parseWithOutlineTokens(spec, langSpec, path, source)
	if tokens == nil {
		t.Fatalf("%s: no tokens record (%+v) for %q", path, status, source)
	}
	return tokens
}

// consumerNoTokenChange is the consumer's rule: the non-comment leaves of both
// sides are equal in number and pairwise in type, text, ancestors and opens,
// and the directive comment leaves pairwise in text and position among them,
// with no error leaf and has_errors false on both sides.
func consumerNoTokenChange(t *testing.T, path, base, head string) bool {
	t.Helper()
	key := func(source string) ([]string, bool) {
		record := fileTokensFor(t, path, source)
		if record.Status != tokensExtracted {
			t.Fatalf("%s: status %s", path, record.Status)
		}
		bad := record.HasErrors
		var out []string
		for _, row := range record.Leaves {
			if row[5]&tokenFlagError != 0 {
				bad = true
			}
			switch {
			case row[5]&tokenFlagComment == 0:
				out = append(out, record.LeafTypes[row[0]]+"\x00"+source[row[1]:row[2]]+"\x00"+record.Ancestors[row[6]]+"\x00"+string(rune('0'+row[7])))
			case row[5]&tokenFlagDirective != 0:
				// A directive comment is compared by text, in its place among
				// the compared leaves.
				out = append(out, "directive\x00"+source[row[1]:row[2]])
			}
		}
		return out, bad
	}
	baseKey, baseBad := key(base)
	headKey, headBad := key(head)
	return slices.Equal(baseKey, headKey) && !baseBad && !headBad
}

// literalForm is one string-like form of a language: a file template with one
// %s for the literal and the literal template with %s for its content.
type literalForm struct {
	name, literal string
	pairs         [][2]string
}

var (
	// Whitespace changes inside one-line literals: the empty literal against
	// a space, more or fewer spaces, a tab against a space, at either end.
	oneLineWhitespace = [][2]string{{"", " "}, {" ", "  "}, {"a b", "a  b"}, {"a", "a "}, {" a", "a"}, {"a\tb", "a b"}}
	// Multi-line literals add line breaks and indentation as content.
	multiLineWhitespace = append(append([][2]string{}, oneLineWhitespace...), [][2]string{{"a\nb", "a\n  b"}, {"a\n", "a\n\n"}, {"\n", " \n"}}...)
	charWhitespace      = [][2]string{{" ", "\t"}}
)

// Coverage follows proof. For every covered language, each of its string-like
// forms (quote styles, raw, multi-line, byte and char literals, templates,
// heredocs where the language has them) must read a whitespace change inside
// the literal as a token change under the consumer's rule, including the
// all-whitespace case, while a layout change outside literals must not. The
// table's keys must be exactly these languages: a language without its cases
// here is not declared. Lua and Swift were tried and are not declared: Lua's
// grammar folds the whitespace before `=` into that token, and Swift reads
// spacing around `=` as a token change, so layout cannot be told from tokens.
// PHP is not declared: whitespace-only text outside its tags is output with no
// node. Kotlin is not declared: its grammar parses `a\n- b` and `f\n(a)` as one
// expression, though a Kotlin line break ends the statement there.
func TestFileTokensLiteralWhitespaceIsContent(t *testing.T) {
	cases := map[string]struct {
		path, file, layout string
		forms              []literalForm
	}{
		"TypeScript": {"a.ts", "const v = %s;\n", "const   v   =\n    %s ;\n", []literalForm{
			{"double", `"%s"`, oneLineWhitespace}, {"single", `'%s'`, oneLineWhitespace},
			{"template", "`%s`", multiLineWhitespace}, {"template with substitution", "`%s${x}%s`", multiLineWhitespace},
			{"regex", "/x%sy/", [][2]string{{"", " "}, {" ", "  "}, {"\t", " "}}},
		}},
		"TSX": {"a.tsx", "const v = %s;\n", "const   v   =\n    %s ;\n", []literalForm{
			{"double", `"%s"`, oneLineWhitespace}, {"single", `'%s'`, oneLineWhitespace},
			{"template", "`%s`", multiLineWhitespace},
			{"jsx text", "<b>%s</b>", [][2]string{{"", " "}, {" ", "  "}, {"a b", "a  b"}, {"a", "a "}}},
			{"jsx attribute string", `<b c="%s" />`, oneLineWhitespace},
		}},
		"JavaScript": {"a.js", "const v = %s;\n", "const   v   =\n    %s ;\n", []literalForm{
			{"double", `"%s"`, oneLineWhitespace}, {"single", `'%s'`, oneLineWhitespace},
			{"template", "`%s`", multiLineWhitespace}, {"template with substitution", "`%s${x}%s`", multiLineWhitespace},
			{"regex", "/x%sy/", [][2]string{{"", " "}, {" ", "  "}, {"\t", " "}}},
		}},
		"Go": {"a.go", "package p\n\nvar v = %s\n", "package p\n\nvar   v   =   %s\n", []literalForm{
			{"interpreted", `"%s"`, oneLineWhitespace}, {"interpreted with escape", `"\n%s\t"`, oneLineWhitespace},
			{"raw", "`%s`", multiLineWhitespace}, {"rune", "'%s'", charWhitespace},
		}},
		"Python": {"a.py", "v = %s\n", "v   =   %s\n", []literalForm{
			{"double", `"%s"`, oneLineWhitespace}, {"single", `'%s'`, oneLineWhitespace},
			{"triple double", `"""%s"""`, multiLineWhitespace}, {"triple single", `'''%s'''`, multiLineWhitespace},
			{"bytes", `b"%s"`, oneLineWhitespace}, {"raw", `r"%s"`, oneLineWhitespace}, {"raw bytes", `rb'%s'`, oneLineWhitespace},
			{"f-string", `f"%s{x}%s"`, oneLineWhitespace}, {"triple f-string", `f"""%s{x}%s"""`, multiLineWhitespace},
		}},
		"Rust": {"a.rs", "fn f() {\n    let v = %s;\n}\n", "fn f() {\n    let   v   =\n        %s ;\n}\n", []literalForm{
			{"string", `"%s"`, multiLineWhitespace}, {"string with escape", `"\n%s\t"`, oneLineWhitespace},
			{"raw", `r"%s"`, multiLineWhitespace}, {"raw hashed", `r#"%s"#`, multiLineWhitespace},
			{"byte string", `b"%s"`, multiLineWhitespace}, {"raw byte string", `br"%s"`, multiLineWhitespace},
			{"char", "'%s'", charWhitespace}, {"byte char", "b'%s'", charWhitespace},
		}},
	}
	for name, tc := range map[string]struct {
		path, file, layout string
		forms              []literalForm
	}{
		"C": {"a.c", "void f(void) {\n  v = %s;\n}\n", "void f(void) {\n  v   =\n    %s ;\n}\n", []literalForm{
			{"string", `"%s"`, oneLineWhitespace}, {"string with escape", `"\n%s\t"`, oneLineWhitespace},
			{"wide string", `L"%s"`, oneLineWhitespace}, {"char", "'%s'", charWhitespace},
		}},
		"Java": {"A.java", "class A {\n  Object v = %s;\n}\n", "class A {\n  Object   v   =\n    %s ;\n}\n", []literalForm{
			{"string", `"%s"`, oneLineWhitespace}, {"string with escape", `"\n%s\t"`, oneLineWhitespace},
			{"text block", "\"\"\"\n%s\"\"\"", multiLineWhitespace}, {"char", "'%s'", charWhitespace},
		}},
		"C#": {"a.cs", "class A {\n  object v = %s;\n}\n", "class A {\n  object   v   =\n    %s ;\n}\n", []literalForm{
			{"string", `"%s"`, oneLineWhitespace}, {"verbatim", `@"%s"`, multiLineWhitespace},
			{"interpolated", `$"%s{x}%s"`, oneLineWhitespace}, {"raw", "\"\"\"\n%s\n\"\"\"", multiLineWhitespace},
			{"char", "'%s'", charWhitespace},
		}},
		"Ruby": {"a.rb", "v = %s\n", "v   =   %s\n", []literalForm{
			{"double", `"%s"`, multiLineWhitespace}, {"single", `'%s'`, multiLineWhitespace},
			{"percent q", `%%q(%s)`, multiLineWhitespace}, {"interpolated", `"%s#{x}%s"`, oneLineWhitespace},
			{"symbol", `:"%s"`, oneLineWhitespace}, {"regex", `/x%sy/`, [][2]string{{"", " "}, {" ", "  "}}},
			{"heredoc", "<<~EOS\n%s\nEOS", multiLineWhitespace},
		}},
		"Elixir": {"a.ex", "v = %s\n", "v   =   %s\n", []literalForm{
			{"string", `"%s"`, multiLineWhitespace}, {"interpolated", `"%s#{x}%s"`, oneLineWhitespace},
			{"charlist", `'%s'`, multiLineWhitespace}, {"heredoc", "\"\"\"\n%s\n\"\"\"", multiLineWhitespace},
			{"sigil", `~s(%s)`, multiLineWhitespace},
		}},
	} {
		cases[name] = tc
	}
	// Interpolations: whitespace inside one is content unless the language is
	// on the layout list, where each pair below must read as no change and a
	// literal inside the interpolation still as a change. Off the list, every
	// pair must read as a change, the printed ones first: Python's `=` form
	// and format specifiers, C#'s alignment and format clauses.
	interpolations := map[string]struct {
		path, file string
		change     [][2]string
		layout     [][2]string
	}{
		"TypeScript": {"a.ts", "const v = %s;\n", [][2]string{{"`${\" \"}`", "`${\"\"}`"}, {"`${f(\"a b\")}`", "`${f(\"a  b\")}`"}},
			[][2]string{{"`${x}`", "`${ x }`"}, {"`${a+b}`", "`${a + b}`"}, {"`${f(a,b)}`", "`${f( a, b )}`"}, {"`${x}`", "`${\n  x\n}`"}}},
		"JavaScript": {"a.js", "const v = %s;\n", [][2]string{{"`${\" \"}`", "`${\"\"}`"}},
			[][2]string{{"`${x}`", "`${ x }`"}, {"`${a+b}`", "`${a + b}`"}}},
		"Python": {"a.py", "v = %s\n", [][2]string{
			{`f"{x=}"`, `f"{x = }"`}, {`f"{x=}"`, `f"{x= }"`}, {`f"{x=:>4}"`, `f"{x = :>4}"`}, {`f"{x:{w}}"`, `f"{x:{w} }"`},
			{`f"{x:>4}"`, `f"{x: >4}"`}, {`f"{x}"`, `f"{ x }"`}, {`f"{x!r}"`, `f"{x !r}"`},
		}, nil},
		"C#": {"a.cs", "class A {\n  object v = %s;\n}\n", [][2]string{
			{`$"{x,10}"`, `$"{x, 10}"`}, {`$"{x,10}"`, `$"{x ,10}"`}, {`$"{x:F2}"`, `$"{x: F2}"`}, {`$"{x:F2}"`, `$"{x:F2 }"`},
			{`$"{x,10:F2}"`, `$"{x,10: F2}"`}, {`$"{x}"`, `$"{ x }"`},
		}, nil},
		"Ruby":   {"a.rb", "v = %s\n", [][2]string{{`"#{x}"`, `"#{ x }"`}, {`"#{a+b}"`, `"#{a + b}"`}}, nil},
		"Elixir": {"a.ex", "v = %s\n", [][2]string{{`"#{x}"`, `"#{ x }"`}, {`"#{a+b}"`, `"#{a + b}"`}}, nil},
	}
	for name, tc := range interpolations {
		language := tokenLanguageFor(t, tc.path)
		t.Run(name+" interpolation", func(t *testing.T) {
			for _, pair := range tc.change {
				if consumerNoTokenChange(t, tc.path, fmt.Sprintf(tc.file, pair[0]), fmt.Sprintf(tc.file, pair[1])) {
					t.Errorf("%s -> %s: whitespace that matters inside an interpolation read as no token change", pair[0], pair[1])
				}
			}
			for _, pair := range tc.layout {
				if !consumerNoTokenChange(t, tc.path, fmt.Sprintf(tc.file, pair[0]), fmt.Sprintf(tc.file, pair[1])) {
					t.Errorf("%s -> %s: layout inside an interpolation read as a token change", pair[0], pair[1])
				}
			}
			if layout := tokenClassTable[language].interpolationLayout; layout != (len(tc.layout) > 0) {
				t.Errorf("%s: interpolationLayout %v without matching layout pairs", language, layout)
			}
		})
	}
	for language, classes := range tokenClassTable {
		if !classes.interpolationLayout {
			continue
		}
		found := false
		for _, tc := range interpolations {
			found = found || tokenLanguageFor(t, tc.path) == language && len(tc.layout) > 0
		}
		if !found {
			t.Errorf("%s treats interpolation whitespace as layout without the pairs that prove it", language)
		}
	}
	// Strings whose value depends on where the closing delimiter sits (Java
	// text blocks, C# raw strings, Elixir heredocs, Ruby heredocs, and any
	// multi-line literal): moving only the delimiter's indentation, and
	// reindenting the whole block with it, must each read as a change. The
	// literal's first %s is the content line's indentation, the second the
	// closing delimiter's.
	delimited := map[string]struct {
		path, file, literal string
	}{
		"Java text block":      {"A.java", "class A {\n  String s = %s;\n}\n", "\"\"\"\n%sx\n%s\"\"\""},
		"C# raw string":        {"a.cs", "class A {\n  string s = %s;\n}\n", "\"\"\"\n%sx\n%s\"\"\""},
		"Elixir heredoc":       {"a.ex", "s = %s\n", "\"\"\"\n%sx\n%s\"\"\""},
		"Elixir charlist":      {"a.ex", "s = %s\n", "'''\n%sx\n%s'''"},
		"Elixir sigil heredoc": {"a.ex", "s = %s\n", "~S\"\"\"\n%sx\n%s\"\"\""},
		"Ruby squiggly":        {"a.rb", "s = %s\n", "<<~EOS\n%sx\n%sEOS"},
		"Ruby dash heredoc":    {"a.rb", "s = %s\n", "<<-EOS\n%sx\n%sEOS"},
		"Python triple":        {"a.py", "s = %s\n", "\"\"\"\n%sx\n%s\"\"\""},
		"TypeScript template":  {"a.ts", "const s = %s;\n", "`\n%sx\n%s`"},
		"JavaScript template":  {"a.js", "const s = %s;\n", "`\n%sx\n%s`"},
		"Rust string":          {"a.rs", "fn f() {\n    let s = %s;\n}\n", "\"\n%sx\n%s\""},
		"Go raw":               {"a.go", "package p\n\nvar s = %s\n", "`\n%sx\n%s`"},
		"C string":             {"a.c", "const char *s = %s;\n", "\"\\\n%sx\\\n%s\""},
	}
	for name, tc := range delimited {
		t.Run(name+" closing delimiter", func(t *testing.T) {
			base := fmt.Sprintf(tc.file, fmt.Sprintf(tc.literal, "    ", "    "))
			heads := []string{
				fmt.Sprintf(tc.file, fmt.Sprintf(tc.literal, "    ", "  ")),       // only the delimiter moves
				fmt.Sprintf(tc.file, fmt.Sprintf(tc.literal, "      ", "      ")), // the whole block reindents
			}
			for _, head := range heads {
				if !tokensAvailable(t, tc.path, head) {
					continue // no extracted record: never read as "no change"
				}
				if consumerNoTokenChange(t, tc.path, base, head) {
					t.Errorf("%q -> %q read as no token change", base, head)
				}
			}
		})
	}
	languages := map[string]bool{}
	for name, tc := range cases {
		language := tokenLanguageFor(t, tc.path)
		languages[language] = true
		t.Run(name, func(t *testing.T) {
			for _, form := range tc.forms {
				holes := strings.Count(form.literal, "%s")
				fill := func(content string) string {
					args := make([]any, holes)
					for i := range args {
						args[i] = content
					}
					return fmt.Sprintf(form.literal, args...)
				}
				for _, pair := range form.pairs {
					base := fmt.Sprintf(tc.file, fill(pair[0]))
					head := fmt.Sprintf(tc.file, fill(pair[1]))
					if consumerNoTokenChange(t, tc.path, base, head) {
						t.Errorf("%s %q -> %q: whitespace inside the literal read as no token change", form.name, pair[0], pair[1])
					}
				}
				// Layout outside the literal is not a token change.
				literal := fill(form.pairs[0][1])
				if !consumerNoTokenChange(t, tc.path, fmt.Sprintf(tc.file, literal), fmt.Sprintf(tc.layout, literal)) {
					t.Errorf("%s: layout outside the literal read as a token change", form.name)
				}
			}
		})
	}
	for language := range tokenClassTable {
		if !languages[language] {
			t.Errorf("%s is declared without its literal-whitespace cases", language)
		}
	}
	for language := range languages {
		if _, ok := tokenClassTable[language]; !ok {
			t.Errorf("%s has cases but is not in the table", language)
		}
	}
}

func tokenLanguageFor(t *testing.T, path string) string {
	t.Helper()
	spec, ok := languageForPath(path)
	if !ok {
		t.Fatalf("%s: no language", path)
	}
	return spec.language
}

// A parsed language that the literal-whitespace test does not cover gets a
// record that says so and carries no leaves, names or imports, so a consumer
// reads "not evaluated" from the record and the declaration alike.
func TestFileTokensUndeclaredLanguage(t *testing.T) {
	for _, path := range []string{"a.lua", "a.sh", "a.swift", "a.yaml"} {
		language := tokenLanguageFor(t, path)
		if slices.Contains(TokenLanguages(), language) {
			t.Fatalf("%s is declared; pick an undeclared language", language)
		}
		source := map[string]string{"a.lua": "local v = 1\n", "a.sh": "v=1\n", "a.swift": "let v = 1\n", "a.yaml": "v: 1\n"}[path]
		record := fileTokensFor(t, path, source)
		if record.Status != tokensLanguageNotCovered || record.Leaves != nil || record.LeafTypes != nil || record.Ancestors != nil || record.IdentifierNames != nil || record.Imports != nil || record.ImportBindings != importBindingsNotExtracted {
			t.Errorf("%s: %#v", path, record)
		}
	}
}

// The rows tile the file: every byte is in at most one row, every byte in no
// row is whitespace outside every content node, and every byte between a
// content node's start and end is in a row, interpolations included, except in
// the languages whose interpolation whitespace is proven layout. One fixture per declared language,
// with all-whitespace and whitespace-edged literals of each form.
func TestFileTokensTileContentNodes(t *testing.T) {
	fixtures := map[string]string{
		"a.ts":   "const a = \" \", b = '  x ', c = ` ${ y }  `, d = /  /;\n// c\nconst z\u00a0= 1;\n",
		"a.tsx":  "const e = <b c=\" \">  {x}  </b>;\n",
		"a.js":   "const a = \" \";\nconst t = `\n  two\n`;\n",
		"a.go":   "package p\n\nvar a, b, c, d = \" \", \"x \\n \", `\n  `, ' '\n",
		"a.py":   "a = \" \"\nb = '''\n  '''\nc = f\" { x = :>4} \"\n",
		"a.rs":   "fn f() { let a = (r\"  \", r#\" \"#, \" \", b\"  \", ' ', br\" \"); }\n",
		"a.c":    "void f(void) { g(\" \", ' ', L\"  \"); }\n",
		"A.java": "class A { String a = \" \"; String b = \"\"\"\n   \"\"\"; char c = ' '; }\n",
		"a.cs":   "class A { string a = \" \", b = @\"  \", c = $\" { x ,10: F2} \"; char d = ' '; }\n",
		"a.rb":   "a = \" \"\nb = %q(  )\nc = <<~EOS\n  \nEOS\nd = \" #{ x } \"\n",
		"a.ex":   "a = \" \"\nb = ' '\nc = ~s(  )\n",
	}
	for path, source := range fixtures {
		language := tokenLanguageFor(t, path)
		record := fileTokensFor(t, path, source)
		if record.Status != tokensExtracted {
			t.Fatalf("%s: status %s", path, record.Status)
		}
		owner := make([]int, len(source))
		for i := range owner {
			owner[i] = -1
		}
		for index, row := range record.Leaves {
			for b := row[1]; b < row[2]; b++ {
				if owner[b] >= 0 {
					t.Fatalf("%s: byte %d is in rows %d and %d", path, b, owner[b], index)
				}
				owner[b] = index
			}
		}
		content := make([]bool, len(source))
		spec, _ := languageForPath(path)
		parser := sitter.NewParser()
		parser.SetLanguage(spec.grammar)
		tree, err := parser.ParseCtx(t.Context(), nil, []byte(source))
		if err != nil {
			t.Fatal(err)
		}
		contentTypes := tokenClassTable[language].content
		contentNodes := 0
		var walk func(node *sitter.Node)
		walk = func(node *sitter.Node) {
			if node.IsNamed() && slices.Contains(contentTypes, node.Type()) {
				contentNodes++
				for b := node.StartByte(); b < node.EndByte(); b++ {
					content[b] = true
				}
				// Only where interpolation whitespace is proven layout is a child
				// with children of its own (a template substitution) exempt; any
				// literal inside it is checked on its own.
				for i := 0; i < int(node.ChildCount()) && tokenClassTable[language].interpolationLayout; i++ {
					if child := node.Child(i); child.ChildCount() > 0 {
						for b := child.StartByte(); b < child.EndByte(); b++ {
							content[b] = false
						}
					}
				}
			}
			for i := 0; i < int(node.ChildCount()); i++ {
				walk(node.Child(i))
			}
		}
		walk(tree.RootNode())
		tree.Close()
		parser.Close()
		if contentNodes == 0 {
			t.Errorf("%s: fixture has no content node", path)
		}
		for b := range source {
			if owner[b] >= 0 {
				continue
			}
			if content[b] {
				t.Errorf("%s: byte %d (%q) inside a content node is in no row", path, b, source[b])
			} else if !strings.ContainsRune(" \t\r\n", rune(source[b])) {
				t.Errorf("%s: byte %d (%q) is in no row and is not whitespace", path, b, source[b])
			}
		}
	}
}
