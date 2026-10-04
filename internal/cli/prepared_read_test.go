package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

func TestPreparedNeighborsCommandMatchesFreshWithoutStructuralWork(t *testing.T) {
	repo, cache := t.TempDir(), t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Prepared Neighbors Test")
	git(t, repo, "config", "user.email", "prepared@example.invalid")
	write(t, repo, "calls.ts", "class Built {}\nfunction helper() {}\nfunction focus() { helper(); new Built(); unknown(); }\nfunction caller() { focus(); }\nfunction shadow(helper: () => void) { helper(); }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "prepared neighbors")
	var prepared sem.ProviderSnapshot
	var err error
	prepared, _, err = sem.PreindexProviderSnapshot(t.Context(), repo, "test", sem.ProviderSnapshotOptions{Profile: sem.ProfileFull}, cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{"focus", "helper", "unknown", "absentFunction", prepared.Symbols[0].ID} {
		args := []string{"neighbors", "--repo", repo, "--head", "--profile", "full", "--format", "json", "--symbol", selector, "--direction", "both", "--depth", "2"}
		var freshOut, preparedOut bytes.Buffer
		if err := Run(t.Context(), Options{Version: "test", Stdout: &freshOut}, append(append([]string(nil), args...), "--no-cache")); err != nil {
			t.Fatal(err)
		}
		before := sem.StructuralWorkSnapshot()
		if err := Run(t.Context(), Options{Version: "test", Stdout: &preparedOut}, append(append([]string(nil), args...), "--cache-dir", cache, "--prepared-only")); err != nil {
			t.Fatal(err)
		}
		if sem.StructuralWorkSnapshot() != before {
			t.Fatal("prepared neighbors parsed, resolved, or built graph facts")
		}
		var fresh, actual neighborResponse
		if err := json.Unmarshal(freshOut.Bytes(), &fresh); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(preparedOut.Bytes(), &actual); err != nil {
			t.Fatal(err)
		}
		if actual.AnalysisScope != "complete-prepared" || !actual.IndexCacheHit {
			t.Fatalf("missing prepared qualification: %s", &preparedOut)
		}
		if actual.PreparedRead == nil || actual.PreparedRead.ArtifactFamily != "search-snapshot" || actual.PreparedRead.GenerationID != "" || len(actual.PreparedRead.PolicyID) != 64 || actual.PreparedRead.ExternalPolicyOrigin != "serving-directory-captured" {
			t.Fatal("prepared neighbors lost legacy artifact-family qualification")
		}
		actual.PreparedRead = nil
		fresh.IndexCacheHit, fresh.IndexCacheDisabled = false, false
		actual.IndexCacheHit, actual.IndexCacheDisabled = false, false
		fresh.IndexLatencyMS, fresh.QueryLatencyMS, fresh.TotalLatencyMS = 0, 0, 0
		actual.IndexLatencyMS, actual.QueryLatencyMS, actual.TotalLatencyMS = 0, 0, 0
		actual.AnalysisScope = ""
		if !reflect.DeepEqual(actual, fresh) {
			t.Fatalf("prepared neighbors differ for %q:\nactual=%s\nfresh=%s", selector, &preparedOut, &freshOut)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	err = Run(ctx, Options{Version: "test", Stdout: &out}, []string{"neighbors", "--repo", repo, "--head", "--prepared-only", "--cache-dir", cache, "--symbol", "focus"})
	if !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatalf("cancellation emitted output or lost cause: output=%q err=%v", &out, err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	priorHook := sourceReaderOpenHook
	sourceReaderOpenHook = cancel
	defer func() { sourceReaderOpenHook = priorHook }()
	before := sem.StructuralWorkSnapshot()
	out.Reset()
	err = Run(ctx, Options{Version: "test", Stdout: &out}, []string{"neighbors", "--repo", repo, "--head", "--prepared-only", "--cache-dir", cache, "--symbol", "focus"})
	if !errors.Is(err, context.Canceled) || out.Len() != 0 || sem.StructuralWorkSnapshot() != before {
		t.Fatalf("source-read cancellation emitted success or did structural work: output=%q err=%v", &out, err)
	}
}

func TestPreparedCommandsRejectContradictoryFlagsAndMissingArtifact(t *testing.T) {
	for _, args := range [][]string{
		{"query", "--prepared-only", "--query", "focus"},
		{"query", "--head", "--prepared-only", "--no-cache", "--query", "focus"},
		{"neighbors", "--prepared-only", "--symbol", "focus"},
		{"neighbors", "--head", "--prepared-only", "--no-cache", "--symbol", "focus"},
	} {
		var out bytes.Buffer
		before := sem.StructuralWorkSnapshot()
		if err := Run(t.Context(), Options{Stdout: &out}, args); err == nil {
			t.Fatalf("invalid flags accepted: %v", args)
		}
		if out.Len() != 0 || sem.StructuralWorkSnapshot() != before {
			t.Fatalf("invalid flags did work or emitted output: %v", args)
		}
	}
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Prepared Missing Test")
	git(t, repo, "config", "user.email", "prepared@example.invalid")
	write(t, repo, "focus.go", "package focus\nfunc Focus() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "missing artifact")
	for _, command := range []string{"query", "neighbors"} {
		args := []string{command, "--repo", repo, "--head", "--prepared-only", "--cache-dir", t.TempDir()}
		if command == "query" {
			args = append(args, "--query", "Focus")
		} else {
			args = append(args, "--symbol", "Focus")
		}
		var out bytes.Buffer
		before := sem.StructuralWorkSnapshot()
		err := Run(t.Context(), Options{Version: "test", Stdout: &out}, args)
		var failure *sem.PreparedSnapshotError
		if !errors.As(err, &failure) || failure.Reason != "missing" {
			t.Fatalf("%s missing = %v", command, err)
		}
		if out.Len() != 0 || sem.StructuralWorkSnapshot() != before {
			t.Fatalf("%s missing performed graph work", command)
		}
	}
}

func TestPreparedQueryCannotReplaySessionOrPresearchPayload(t *testing.T) {
	repo, cache := t.TempDir(), t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Prepared Replay Test")
	git(t, repo, "config", "user.email", "prepared@example.invalid")
	write(t, repo, "focus.go", "package focus\nfunc Focus() {}\nfunc Other() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "prepared replay")
	presearch, session := filepath.Join(t.TempDir(), "presearch"), filepath.Join(t.TempDir(), "session")
	if err := os.WriteFile(presearch, []byte("unqualified prior answer"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := EntireEnv{PresearchPath: presearch, SearchSession: session, MaxSearches: "1"}
	base := []string{"query", "--repo", repo, "--head", "--prepared-only", "--cache-dir", cache, "--profile", "full", "--format", "json", "--query"}
	var out bytes.Buffer
	err := Run(t.Context(), Options{Version: "test", Env: env, Stdout: &out}, append(append([]string(nil), base...), "Focus"))
	var failure *sem.PreparedSnapshotError
	if !errors.As(err, &failure) || failure.Reason != "missing" || out.Len() != 0 {
		t.Fatalf("missing artifact replayed payload: output=%q err=%v", &out, err)
	}
	if _, _, err := sem.PreindexProviderSnapshot(t.Context(), repo, "test", sem.ProviderSnapshotOptions{Profile: sem.ProfileFull}, cache); err != nil {
		t.Fatal(err)
	}
	before := sem.StructuralWorkSnapshot()
	for _, question := range []string{"Focus", "Other"} {
		out.Reset()
		if err := Run(t.Context(), Options{Version: "test", Env: env, Stdout: &out}, append(append([]string(nil), base...), question)); err != nil {
			t.Fatal(err)
		}
		var response sem.SearchResponse
		if err := json.Unmarshal(out.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Query != question || response.Stats.AnalysisScope != "complete-prepared" {
			t.Fatalf("replayed prior question: %s", &out)
		}
	}
	if sem.StructuralWorkSnapshot() != before {
		t.Fatal("prepared query performed structural work")
	}
	if _, err := os.Stat(session); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepared query wrote session state: %v", err)
	}
}
