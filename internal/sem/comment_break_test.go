package sem

import (
	"strings"
	"testing"
)

// A line break inside a block comment ends the statement in TypeScript and
// Go (tsc parses `return` alone; Go inserts a semicolon and the function lacks
// its return value), yet both grammars give equal leaves and the comment is
// prose, so the leaf rule alone reads no change. A consumer's gap rule sees it
// only by reading the comment's bytes; this pins the facts it relies on: the
// non-comment leaves are equal, the comment is a comment and not a directive,
// and its row spans the line feed. JavaScript's grammar already sees the break.
func TestBlockCommentLineBreakFacts(t *testing.T) {
	cases := []struct {
		path, base, head string
		treeSeesBreak    bool
	}{
		{"a.ts", "function f() { return /* see the note below */ 1; }\n", "function f() { return /* see the note\nbelow */ 1; }\n", false},
		{"a.go", "package p\n\nfunc f() int { return /* see the note below */ 1 }\n", "package p\n\nfunc f() int { return /* see the note\nbelow */ 1 }\n", false},
		{"a.js", "function f() { return /* see the note below */ 1; }\n", "function f() { return /* see the note\nbelow */ 1; }\n", true},
	}
	for _, tc := range cases {
		noChange := consumerNoTokenChange(t, tc.path, tc.base, tc.head)
		if noChange == tc.treeSeesBreak {
			t.Errorf("%s: leaves read as no change = %v; the grammar seeing the break was expected to be %v", tc.path, noChange, tc.treeSeesBreak)
		}
		record := fileTokensFor(t, tc.path, tc.head)
		start := strings.Index(tc.head, "/*")
		found := false
		for _, row := range record.Leaves {
			if row[1] != start {
				continue
			}
			found = true
			text := tc.head[row[1]:row[2]]
			if row[5]&tokenFlagComment == 0 || row[5]&tokenFlagDirective != 0 {
				t.Errorf("%s: comment row %q flags %d, want comment and not directive", tc.path, text, row[5])
			}
			if !strings.Contains(text, "\n") || !strings.HasSuffix(text, "*/") {
				t.Errorf("%s: comment row %q does not span the line feed", tc.path, text)
			}
		}
		if !found {
			t.Errorf("%s: no row starts at the comment", tc.path)
		}
	}
}
