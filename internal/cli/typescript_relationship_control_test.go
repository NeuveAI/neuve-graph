package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A clean worktree is not necessarily byte-identical to the committed tree:
// checkout filters may rewrite source while their inverse clean filter keeps
// `git status` empty. The public relationship command must still resolve a
// --head call from Git's committed bytes, including when another file exports
// a same-named callable that would be a valid lexical decoy.
func TestTypeScriptNeighborsHeadUsesCommittedImportInsteadOfCleanSmudgedDecoy(t *testing.T) {
	repo := newTypeScriptCheckoutFilterControl(t, false)
	requireTypeScriptCommittedAndWorktreeTargets(t, repo)
}

// Phase 3 preserves the unchanged engine baseline. Enable this eval explicitly
// when developing Phase 4: its correct endpoint expectations expose the current
// inability to carry an imported TypeScript member through a local alias.
func TestTypeScriptAliasedImportRelationshipEval(t *testing.T) {
	if os.Getenv("ENTIRE_GRAPH_RUN_PHASE4_TYPESCRIPT_ALIAS_EVAL") != "1" {
		t.Skip("Phase 4 engine development eval; set ENTIRE_GRAPH_RUN_PHASE4_TYPESCRIPT_ALIAS_EVAL=1 to expose the current alias-resolution gap")
	}
	repo := newTypeScriptCheckoutFilterControl(t, true)
	requireTypeScriptCommittedAndWorktreeTargets(t, repo)
}

func newTypeScriptCheckoutFilterControl(t *testing.T, aliasedImport bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses sed as a checkout filter; committed-tree selection is platform-independent")
	}

	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Tests")
	git(t, repo, "config", "user.email", "tests@entire.local")
	git(t, repo, "config", "filter.call-redirect.smudge", `sed 's#from "./real"#from "./decoy"#'`)
	git(t, repo, "config", "filter.call-redirect.clean", `sed 's#from "./decoy"#from "./real"#'`)
	git(t, repo, "config", "filter.call-redirect.required", "true")

	entrySource := `import { queryRepository } from "./real";

export function main(): string {
  return queryRepository();
}
`
	if aliasedImport {
		entrySource = `import { queryRepository as executeQueryRepository } from "./real";

export function main(): string {
  return executeQueryRepository();
}
`
	}
	write(t, repo, ".gitattributes", "src/entry.ts filter=call-redirect\n")
	write(t, repo, "src/entry.ts", entrySource)
	write(t, repo, "src/real.ts", `export function queryRepository(): string {
  return "real";
}
`)
	write(t, repo, "src/decoy.ts", `export function queryRepository(): string {
  const resultLimit = "queryRepository result limit applied";
  const contextLimit = "queryRepository context limit applied";
  return resultLimit + contextLimit;
}
`)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "TypeScript relationship control")

	entryPath := filepath.Join(repo, "src", "entry.ts")
	if err := os.Remove(entryPath); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "checkout", "--", "src/entry.ts")
	entry, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(entry), `from "./decoy"`) {
		t.Fatalf("checkout filter did not create the control: %q", entry)
	}
	status := exec.Command("git", "status", "--porcelain=v1", "--untracked-files=all")
	status.Dir = repo
	statusOutput, err := status.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, statusOutput)
	}
	if len(statusOutput) != 0 {
		t.Fatalf("checkout-filtered worktree must remain clean, got %q", statusOutput)
	}
	return repo
}

func requireTypeScriptCommittedAndWorktreeTargets(t *testing.T, repo string) {
	t.Helper()
	cacheDir := t.TempDir()
	head := decodeTypeScriptCallControl(t, runSourceViewCommand(t, repo, cacheDir, "",
		"neighbors", "--symbol", "main", "--file", "src/entry.ts",
		"--relation", "CALLS", "--direction", "out", "--profile", "full",
		"--head", "--format", "json"))
	requireOnlyTypeScriptCallTarget(t, head, "src/real.ts")
	if head.Commit != rev(t, repo, "HEAD") {
		t.Fatalf("--head commit = %q, want %q", head.Commit, rev(t, repo, "HEAD"))
	}

	worktree := decodeTypeScriptCallControl(t, runSourceViewCommand(t, repo, cacheDir, "",
		"neighbors", "--symbol", "main", "--file", "src/entry.ts",
		"--relation", "CALLS", "--direction", "out", "--profile", "full",
		"--format", "json"))
	requireOnlyTypeScriptCallTarget(t, worktree, "src/decoy.ts")
}

func decodeTypeScriptCallControl(t *testing.T, output string) neighborResponse {
	t.Helper()
	var response neighborResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("decode neighbors JSON: %v\n%s", err, output)
	}
	if len(response.Matches) != 1 {
		t.Fatalf("main matches = %d, want exactly 1: %#v", len(response.Matches), response.Matches)
	}
	return response
}

func requireOnlyTypeScriptCallTarget(t *testing.T, response neighborResponse, wantPath string) {
	t.Helper()
	edges := response.Matches[0].Outgoing
	if len(edges) != 1 {
		t.Fatalf("main CALLS = %d, want exactly 1: %#v", len(edges), edges)
	}
	got := edges[0]
	if got.Relation != "CALLS" || got.Endpoint.Name != "queryRepository" || got.Endpoint.FilePath != wantPath {
		t.Fatalf("main call target = %s %s#%s, want CALLS %s#queryRepository", got.Relation, got.Endpoint.FilePath, got.Endpoint.Name, wantPath)
	}
}
