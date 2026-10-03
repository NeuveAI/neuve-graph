package sem

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func cacheSourceRepository(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "view")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Cache Test")
	git(t, repo, "config", "user.email", "cache@example.com")
	return repo
}

func cacheSourceFunctions(snapshot ProviderSnapshot) []string {
	var names []string
	for _, symbol := range snapshot.Symbols {
		if symbol.Kind == "function" {
			names = append(names, symbol.Name)
		}
	}
	sort.Strings(names)
	return names
}

func TestSearchCacheFreshLinkedViewsRebindBeforeSourceReads(t *testing.T) {
	repo := cacheSourceRepository(t)
	write(t, repo, "calls.go", "package sample\nfunc Callee(value int) int { return value }\nfunc Caller() int { return Callee(7) }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "source")
	old := filepath.Join(t.TempDir(), "view")
	git(t, repo, "worktree", "add", "--detach", old, "HEAD")
	cache := t.TempDir()
	options := ProviderSnapshotOptions{Profile: ProfileFull}
	cold, hit, err := PreindexProviderSnapshot(t.Context(), old, "dev", options, cache)
	if err != nil || hit {
		t.Fatalf("cold preindex hit=%v err=%v", hit, err)
	}
	if got := cacheSourceFunctions(cold); !reflect.DeepEqual(got, []string{"Callee", "Caller"}) {
		t.Fatalf("cold functions=%v", got)
	}
	git(t, repo, "worktree", "remove", old)
	git(t, repo, "commit", "--allow-empty", "-m", "same tree different serving commit")
	fresh := filepath.Join(t.TempDir(), "view")
	git(t, repo, "worktree", "add", "--detach", fresh, "HEAD")
	t.Cleanup(func() { git(t, repo, "worktree", "remove", "--force", fresh) })
	warm, hit, err := LoadOrBuildProviderSnapshot(t.Context(), fresh, "dev", options, cache, false)
	if err != nil || !hit {
		t.Fatalf("fresh linked view hit=%v err=%v", hit, err)
	}
	if warm.Header.RepoRoot != fresh || warm.Header.Commit == cold.Header.Commit || warm.Header.Tree != cold.Header.Tree {
		t.Fatalf("serving provenance=%+v", warm.Header)
	}
	if warm.Header.Commit != gitInput(t, fresh, "", "rev-parse", "HEAD") {
		t.Fatalf("serving commit=%q", warm.Header.Commit)
	}
	if !reflect.DeepEqual(cold.Symbols, warm.Symbols) || !reflect.DeepEqual(cold.Relations, warm.Relations) || !reflect.DeepEqual(cold.Files, warm.Files) {
		t.Fatal("cache round trip changed source metadata, private parser metadata, or relations")
	}
	var caller, callee string
	for _, symbol := range warm.Symbols {
		if symbol.Name == "Caller" {
			caller = symbol.ID
		}
		if symbol.Name == "Callee" {
			callee = symbol.ID
			if !symbol.parameterNamesKnown || !reflect.DeepEqual(symbol.parameterNames, []string{"value"}) || symbol.sourceEndByte <= symbol.sourceStartByte {
				t.Fatalf("callee private metadata=%+v", symbol)
			}
		}
	}
	callFound := false
	for _, relation := range warm.Relations {
		if relation.Type == "CALLS" && relation.FromID == caller && relation.ToID == callee {
			callFound = len(relation.Evidence) > 0 && relation.Evidence[0].FilePath == "calls.go"
		}
	}
	if !callFound {
		t.Fatal("warm snapshot lacks literal Caller -> Callee call with source evidence")
	}
	encoded, err := json.Marshal(warm)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), old) {
		t.Fatal("retired serving root survived rebinding")
	}
	selective := options
	selective.OnlyFiles = []string{"calls.go"}
	derived, hit, err := LoadOrBuildProviderSnapshot(t.Context(), fresh, "dev", selective, cache, false)
	if err != nil || !hit || !reflect.DeepEqual(cacheSourceFunctions(derived), []string{"Callee", "Caller"}) {
		t.Fatalf("derived source read hit=%v err=%v functions=%v", hit, err, cacheSourceFunctions(derived))
	}
}

func TestSearchCacheSeparatesSameBasenameScopesAndForeignRepositories(t *testing.T) {
	repo := cacheSourceRepository(t)
	write(t, repo, "left/component/source.go", "package left\nfunc LeftLiteral() { LeftLeaf() }\nfunc LeftLeaf() {}\n")
	write(t, repo, "right/component/source.go", "package right\nfunc RightLiteral() { RightLeaf() }\nfunc RightLeaf() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "scopes")
	cache := t.TempDir()
	options := ProviderSnapshotOptions{Profile: ProfileFull}
	for _, name := range []string{"left", "right"} {
		scope := filepath.Join(repo, name, "component")
		prefix := strings.ToUpper(name[:1]) + name[1:]
		want := []string{prefix + "Leaf", prefix + "Literal"}
		for attempt := 0; attempt < 2; attempt++ {
			snapshot, hit, err := LoadOrBuildProviderSnapshot(t.Context(), scope, "dev", options, cache, false)
			if err != nil || hit != (attempt == 1) || !reflect.DeepEqual(cacheSourceFunctions(snapshot), want) {
				t.Fatalf("%s attempt=%d hit=%v err=%v functions=%v", name, attempt, hit, err, cacheSourceFunctions(snapshot))
			}
			if snapshot.Header.RepoKey != "local/component" {
				t.Fatalf("namespace=%q", snapshot.Header.RepoKey)
			}
			var caller, callee string
			for _, symbol := range snapshot.Symbols {
				if symbol.Name == prefix+"Literal" {
					caller = symbol.ID
				}
				if symbol.Name == prefix+"Leaf" {
					callee = symbol.ID
				}
			}
			call := false
			for _, relation := range snapshot.Relations {
				if relation.Type == "CALLS" && relation.FromID == caller && relation.ToID == callee {
					call = len(relation.Evidence) > 0 && relation.Evidence[0].FilePath == "source.go"
				}
			}
			if !call {
				t.Fatalf("%s scope lacks its literal call and source evidence", name)
			}
		}
	}
	foreign := filepath.Join(t.TempDir(), "view")
	git(t, filepath.Dir(foreign), "clone", "--no-local", repo, foreign)
	// A clone's default origin changes repo_key only for supported host remotes;
	// this local clone deliberately retains the same local/view namespace.
	for attempt := 0; attempt < 2; attempt++ {
		snapshot, hit, err := LoadOrBuildProviderSnapshot(t.Context(), filepath.Join(foreign, "left", "component"), "dev", options, cache, false)
		if err != nil || hit != (attempt == 1) || !reflect.DeepEqual(cacheSourceFunctions(snapshot), []string{"LeftLeaf", "LeftLiteral"}) {
			t.Fatalf("foreign attempt=%d hit=%v err=%v functions=%v", attempt, hit, err, cacheSourceFunctions(snapshot))
		}
	}
}

func TestSearchCacheLinkedViewPolicyCaptureKeepsActualRoot(t *testing.T) {
	repo := cacheSourceRepository(t)
	write(t, repo, "allow.go", "package sample\nfunc AllowedLiteral() {}\n")
	write(t, repo, "deny.go", "package sample\nfunc DeniedLiteral() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "source")
	cache := t.TempDir()
	options := ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}
	old := filepath.Join(t.TempDir(), "view")
	git(t, repo, "worktree", "add", "--detach", old, "HEAD")
	write(t, old, ".graphignore", "deny.go\n")
	first, hit, err := PreindexProviderSnapshot(t.Context(), old, "dev", options, cache)
	if err != nil || hit || !reflect.DeepEqual(cacheSourceFunctions(first), []string{"AllowedLiteral"}) {
		t.Fatalf("old policy hit=%v err=%v functions=%v", hit, err, cacheSourceFunctions(first))
	}
	git(t, repo, "worktree", "remove", "--force", old)
	fresh := filepath.Join(t.TempDir(), "view")
	git(t, repo, "worktree", "add", "--detach", fresh, "HEAD")
	t.Cleanup(func() { git(t, repo, "worktree", "remove", "--force", fresh) })
	write(t, fresh, ".graphignore", "deny.go\n")
	warm, hit, err := LoadOrBuildProviderSnapshot(t.Context(), fresh, "dev", options, cache, false)
	if err != nil || !hit || !reflect.DeepEqual(cacheSourceFunctions(warm), []string{"AllowedLiteral"}) {
		t.Fatalf("same policy hit=%v err=%v functions=%v", hit, err, cacheSourceFunctions(warm))
	}
	write(t, fresh, ".graphignore", "allow.go\n")
	changed, hit, err := LoadOrBuildProviderSnapshot(t.Context(), fresh, "dev", options, cache, false)
	if err != nil || hit || !reflect.DeepEqual(cacheSourceFunctions(changed), []string{"DeniedLiteral"}) {
		t.Fatalf("changed actual-root policy hit=%v err=%v functions=%v", hit, err, cacheSourceFunctions(changed))
	}
	if _, hit, err := LoadOrBuildProviderSnapshot(t.Context(), fresh, "dev", options, cache, false); err != nil || !hit {
		t.Fatalf("changed policy second read hit=%v err=%v", hit, err)
	}
}

func TestSearchCachePreloadCannotCrossServingScope(t *testing.T) {
	repo := cacheSourceRepository(t)
	write(t, repo, "left/component/source.go", "package left\nfunc LeftLiteral() {}\n")
	write(t, repo, "right/component/source.go", "package right\nfunc RightLiteral() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "scopes")
	cache := t.TempDir()
	options := ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}
	left := filepath.Join(repo, "left", "component")
	if _, _, err := PreindexProviderSnapshot(t.Context(), left, "dev", options, cache); err != nil {
		t.Fatal(err)
	}
	preload, hit, err := loadCachedCompleteSearchSnapshotBinding(t.Context(), left, "dev", options, cache)
	if err != nil || !hit {
		t.Fatalf("left preload hit=%v err=%v", hit, err)
	}
	options.OnlyFiles = []string{"source.go"}
	right := filepath.Join(repo, "right", "component")
	snapshot, hit, err := loadOrDeriveSelectiveSearchSnapshot(t.Context(), right, "dev", options, cache, false, preload)
	if err != nil || hit || !reflect.DeepEqual(cacheSourceFunctions(snapshot), []string{"RightLiteral"}) {
		t.Fatalf("foreign preload hit=%v err=%v functions=%v", hit, err, cacheSourceFunctions(snapshot))
	}
}

func TestSearchCacheRejectsForeignEnvelopeBinding(t *testing.T) {
	repo := cacheSourceRepository(t)
	write(t, repo, "source.go", "package sample\nfunc Literal() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "source")
	options := ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}
	cacheDir := t.TempDir()
	if _, _, err := PreindexProviderSnapshot(t.Context(), repo, "dev", options, cacheDir); err != nil {
		t.Fatal(err)
	}
	path := soleCacheArtifact(t, cacheDir)
	for _, field := range []string{"common directory", "scope", "build identity"} {
		cached := readCachedSnapshotFile(t, path)
		switch field {
		case "common directory":
			cached.Source.CommonDir = "/foreign"
		case "scope":
			cached.Source.Scope = "foreign"
		case "build identity":
			cached.BuildIdentity = strings.Repeat("0", 64)
		}
		writeCachedSnapshotFile(t, path, cached)
		snapshot, hit, err := LoadOrBuildProviderSnapshot(t.Context(), repo, "dev", options, cacheDir, false)
		if err != nil || hit || !reflect.DeepEqual(cacheSourceFunctions(snapshot), []string{"Literal"}) {
			t.Fatalf("foreign %s hit=%v err=%v functions=%v", field, hit, err, cacheSourceFunctions(snapshot))
		}
		if _, hit, err := LoadOrBuildProviderSnapshot(t.Context(), repo, "dev", options, cacheDir, false); err != nil || !hit {
			t.Fatalf("repaired %s cache hit=%v err=%v", field, hit, err)
		}
	}
}
