package sem

import (
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

// A language Graph parses with tree-sitter but leaves out of the table would
// emit leaves with no identifier ever flagged, silently: every parsed language
// needs an entry, and every listed type must be a named node of one of the
// language's grammars, so a typo cannot hide a whole class of names.
func TestTokenClassTableCoversTreeSitterLanguages(t *testing.T) {
	grammars := tokenGrammars()
	for language := range grammars {
		if _, ok := tokenClassTable[language]; !ok {
			t.Errorf("%s is parsed with tree-sitter but has no token classification", language)
		}
	}
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
		for _, typ := range append(append([]string{}, classes.identifiers...), classes.comments...) {
			if !named[typ] {
				t.Errorf("%s: %q is not a named node of its grammars", language, typ)
			}
		}
		if !slices.IsSorted(classes.identifiers) || !slices.IsSorted(classes.comments) {
			t.Errorf("%s: keep the lists sorted so the doc mirror is stable", language)
		}
	}
}

// docs/snapshot-format.md states, per language, the node types read as
// identifiers and comments. It mirrors the code table exactly.
func TestTokenClassificationDocMirrorsTable(t *testing.T) {
	var table strings.Builder
	table.WriteString("| Language | Identifier leaf types | Comment node types |\n| --- | --- | --- |\n")
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
		table.WriteString("| " + language + " | " + quote(classes.identifiers) + " | " + quote(classes.comments) + " |\n")
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "snapshot-format.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), table.String()) {
		t.Errorf("docs/snapshot-format.md does not mirror tokenClassTable; expected table:\n%s", table.String())
	}
}

// The Protocol Buffers view inserts a synthetic syntax line: no row may
// describe it, and the rows still tile the file as given.
func TestFileTokensSkipProtocolBuffersSyntheticLine(t *testing.T) {
	source := "message Item {\n  string name = 1;\n}\n"
	spec := resolveProfile(ProfileSyntaxOnly)
	spec.outline, spec.tokens = true, true
	_, _, _, outline, tokens := parseWithOutlineTokens(spec, treeSitterLanguages[".proto"], "a.proto", source)
	if outline == nil || tokens == nil {
		t.Fatal("missing outline or tokens")
	}
	pos := 0
	for _, row := range tokens.Leaves {
		if strings.TrimSpace(source[pos:row[1]]) != "" {
			t.Fatalf("bytes %q not covered", source[pos:row[1]])
		}
		if row[2] > len(source) || row[3] < 1 {
			t.Fatalf("row %v outside the file as given", row)
		}
		pos = row[2]
	}
	if strings.TrimSpace(source[pos:]) != "" || tokens.Leaves[0][3] != 1 || source[tokens.Leaves[0][1]:tokens.Leaves[0][2]] != "message" {
		t.Fatalf("first row %v, tail %q", tokens.Leaves[0], source[pos:])
	}
}
