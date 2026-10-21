package sem

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/gitutil"
)

// Listing an alias group's alternatives separately must find exactly the lines
// the group finds: the committed-tree first-line scan over the split patterns
// equals one `git grep -m 1` over the combined ones, byte for byte.
func TestAliasFirstLinePatternsMatchTheCombinedGroup(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	files := map[string]string{
		"camel.go":    "package x\nvar plain = 1\nfunc newAuthToken() {}\n",
		"snake.go":    "package x\n// my_auth handler\n",
		"upper.go":    "package x\nconst AUTH = 1\nconst session = 2\n",
		"prefix.go":   "package x\nvar xAuthor = 1\nvar XAuth = 2\n",
		"session.go":  "package x\n\n\nfunc Session() {}\nfunc auth() {}\n",
		"nothing.go":  "package x\nvar unrelated = 1\n",
		"dotted.ts":   "import { auth } from './auth.ts'\n",
		"suffixed.go": "package x\nvar authz, authN = 1, 2\n",
	}
	for name, content := range files {
		write(t, repo, name, content)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "fixture")

	q := buildSearchQuery("authentication session")
	if len(q.inferredAbbreviations) == 0 {
		t.Fatal("query no longer infers an abbreviation")
	}
	combined := searchGitAliasPatternsForTerms(q, q.terms)
	split := searchGitAliasFirstLinePatterns(q, q.terms)
	if combined == nil || len(split) <= len(combined) {
		t.Fatalf("expected the alias group to split: combined %d, split %d", len(combined), len(split))
	}

	cmd := exec.Command("git", "grep", "--no-recurse-submodules", "--no-line-number", "--no-column", "--no-color", "--no-full-name", "-z", "-I", "-E", "-m", "1", "-f", "-", "HEAD", "--")
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(strings.Join(combined, "\n") + "\n")
	want, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := gitutil.GrepTreePatternLines(t.Context(), repo, "HEAD", split, func(match gitutil.GrepMatch) error {
		got.WriteString("HEAD:" + match.Path + "\x00" + match.Text + "\n")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Fatalf("split patterns differ from the combined group:\n got %q\nwant %q", got.String(), want)
	}
	if !strings.Contains(string(want), "camel.go") || !strings.Contains(string(want), "snake.go") {
		t.Fatalf("fixture no longer exercises the alias alternatives: %q", want)
	}
}
