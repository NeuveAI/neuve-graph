package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

func preparationPlanCommand(t *testing.T, repo, cache, commit, expected string) indexPreparationPlanResponse {
	t.Helper()
	args := []string{"index", "--repo", repo, "--cache-dir", cache, "--incremental", "--commit", commit, "--profile", "syntax-only", "--plan", "--format", "json"}
	if expected != "" {
		args = append(args, "--expected-generation", expected)
	}
	var output bytes.Buffer
	if err := Run(t.Context(), Options{Version: "test", Stdout: &output}, args); err != nil {
		t.Fatal(err)
	}
	var result indexPreparationPlanResponse
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPreparationPlanAbsentReadyAndGuardedExecution(t *testing.T) {
	repo, cache := incrementalCommandFixture(t)
	old := incrementalCommandCommit(t, repo)
	git(t, repo, "commit", "--allow-empty", "-m", "same tree different commit")
	newer := incrementalCommandCommit(t, repo)
	write(t, repo, "review.ts", "export function dirtyReplacement() { return 'uncommitted'; }\n")
	beforeWorktrees, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	before := sem.StructuralWorkSnapshot()
	absent := preparationPlanCommand(t, repo, cache, old, "")
	newerPlan := preparationPlanCommand(t, repo, cache, newer, "")
	if absent.PreparationPlan.Availability != "absent" || absent.Commit != old || absent.Tree != newerPlan.Tree || absent.PreparedRead.GenerationID == newerPlan.PreparedRead.GenerationID || sem.StructuralWorkSnapshot() != before {
		t.Fatalf("plan changed source or structural work: %#v / %#v", absent, newerPlan)
	}
	build, err := sem.ExecutableBuildIdentity()
	if err != nil {
		t.Fatal(err)
	}
	common, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if absent.PreparedRead.ArtifactFamily != "syntax-generation" || absent.PreparedRead.ProducerBuildID != build || len(absent.PreparedRead.PolicyID) != 64 || absent.PreparedRead.Source.CommonDir != common || absent.PreparedRead.Source.Scope != "" || absent.PreparedRead.ExternalPolicyOrigin != "serving-directory-captured" || absent.PreparedRead.GitignoreOrigin != "target-commit" {
		t.Fatalf("plan qualification=%#v", absent.PreparedRead)
	}
	if files, err := os.ReadDir(cache); err != nil || len(files) != 0 {
		t.Fatalf("plan wrote cache: %v %v", files, err)
	}
	var output bytes.Buffer
	err = Run(t.Context(), Options{Version: "test", Stdout: &output}, []string{"index", "--repo", repo, "--cache-dir", cache, "--incremental", "--commit", old, "--profile", "syntax-only", "--expected-generation", absent.PreparedRead.GenerationID, "--format", "json"})
	if err != nil {
		t.Fatal(err)
	}
	var prepared indexResponse
	if err = json.Unmarshal(output.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.Commit != old || prepared.PreparedRead == nil || !reflect.DeepEqual(*prepared.PreparedRead, absent.PreparedRead) || prepared.IncrementalPreparation.ExtractedFiles != 2 {
		t.Fatalf("execution lost selected qualification: %#v", prepared)
	}
	before = sem.StructuralWorkSnapshot()
	ready := preparationPlanCommand(t, repo, cache, old, absent.PreparedRead.GenerationID)
	if ready.PreparationPlan.Availability != "ready" || !reflect.DeepEqual(ready.PreparedRead, absent.PreparedRead) || sem.StructuralWorkSnapshot() != before {
		t.Fatalf("ready plan=%#v", ready)
	}
	afterWorktrees, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil || !bytes.Equal(beforeWorktrees, afterWorktrees) || incrementalCommandCommit(t, repo) != newer {
		t.Fatal("plan/execution changed HEAD or worktree list")
	}
	contents, err := os.ReadFile(filepath.Join(repo, "review.ts"))
	if err != nil || !bytes.Contains(contents, []byte("dirtyReplacement")) {
		t.Fatal("plan/execution modified dirty source")
	}
	if !sem.Capabilities().OptionalLocalOnlyFeatures["incremental_preparation_plan"] {
		t.Fatal("plan capability missing")
	}
}

func TestPreparationPlanAdmissionAndCancellationDoNotPublish(t *testing.T) {
	repo, cache := incrementalCommandFixture(t)
	commit := incrementalCommandCommit(t, repo)
	valid := []string{"--incremental", "--commit", commit, "--profile", "syntax-only"}
	bad := [][]string{
		{"--plan"}, {"--expected-generation", strings.Repeat("a", 64)},
		append(append([]string{}, valid...), "--plan", "--head"),
		append(append([]string{}, valid...), "--plan", "--report", "report.md"),
		append(append([]string{}, valid...), "--plan", "--force"),
		append(append([]string{}, valid...), "--plan", "--plan"),
		append(append([]string{}, valid...), "--commit", commit),
		append(append([]string{}, valid...), "--expected-generation", strings.Repeat("a", 64), "--expected-generation", strings.Repeat("a", 64)),
		append(append([]string{}, valid...), "--expected-generation", "HEAD"),
		{"--incremental", "--commit", "HEAD", "--profile", "syntax-only", "--plan"},
		{"--incremental", "--commit", strings.Repeat("A", 40), "--profile", "syntax-only", "--plan"},
		append(append([]string{}, valid...), "--plan", "--worktree"),
	}
	before := sem.StructuralWorkSnapshot()
	for _, flags := range bad {
		var output bytes.Buffer
		args := append([]string{"index", "--repo", repo, "--cache-dir", cache, "--format", "json"}, flags...)
		if err := Run(t.Context(), Options{Version: "test", Stdout: &output}, args); err == nil || output.Len() != 0 {
			t.Fatalf("invalid flags admitted %v: %v %s", args, err, &output)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var output bytes.Buffer
	if err := Run(ctx, Options{Version: "test", Stdout: &output}, append([]string{"index", "--repo", repo, "--cache-dir", cache, "--plan"}, valid...)); !errors.Is(err, context.Canceled) || output.Len() != 0 {
		t.Fatalf("cancelled plan emitted receipt %v %s", err, &output)
	}
	if sem.StructuralWorkSnapshot() != before {
		t.Fatal("admission/cancellation performed structural work")
	}
	if files, err := os.ReadDir(cache); err != nil || len(files) != 0 {
		t.Fatal("admission/cancellation wrote cache")
	}
}
