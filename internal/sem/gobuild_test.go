package sem

import (
	"context"
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGoFileMatchesDefaultBuild(t *testing.T) {
	host := build.Default.GOOS
	hostArch := build.Default.GOARCH
	// Pick a definitely-different OS/arch for the negative cases.
	otherOS := "linux"
	if host == "linux" {
		otherOS = "windows"
	}
	otherArch := "amd64"
	if hostArch == "amd64" {
		otherArch = "arm64"
	}

	cases := []struct {
		name    string
		path    string
		content string
		want    bool
	}{
		{"plain", "foo.go", "package foo", true},
		{"host os suffix", "foo_" + host + ".go", "package foo", true},
		{"other os suffix", "foo_" + otherOS + ".go", "package foo", false},
		{"host arch suffix", "foo_" + hostArch + ".go", "package foo", true},
		{"other arch suffix", "foo_" + otherArch + ".go", "package foo", false},
		{"host os+arch", "foo_" + host + "_" + hostArch + ".go", "package foo", true},
		{"other os+arch", "foo_" + otherOS + "_" + otherArch + ".go", "package foo", false},
		{"test suffix is not a platform", "foo_test.go", "package foo", true},
		{"non-platform suffix kept", "foo_helper.go", "package foo", true},
		{"gobuild custom tag excluded", "foo.go", "//go:build binary_log\n\npackage foo", false},
		{"gobuild negated custom tag included", "foo.go", "//go:build !binary_log\n\npackage foo", true},
		{"gobuild host os included", "foo.go", "//go:build " + host + "\n\npackage foo", true},
		{"gobuild other os excluded", "foo.go", "//go:build " + otherOS + "\n\npackage foo", false},
		{"plusbuild custom tag excluded", "foo.go", "// +build binary_log\n\npackage foo", false},
		{"gobuild wins over filename", "foo_" + otherOS + ".go", "//go:build " + host + "\n\npackage foo", false}, // filename still excludes
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := goFileMatchesDefaultBuild(c.path, c.content); got != c.want {
				t.Fatalf("goFileMatchesDefaultBuild(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}

	// unix tag is satisfied on unix hosts, not on Windows/Plan9.
	wantUnix := unixGOOS[host]
	if got := goBuildCommentSatisfied("//go:build unix\n\npackage foo"); got != wantUnix {
		t.Fatalf("unix tag on %s = %v, want %v", host, got, wantUnix)
	}
}

// coverageFixtureRepo commits files into a fresh repository for the coverage
// warning tests; extra runs after the files are written and before the commit.
func coverageFixtureRepo(t *testing.T, files map[string]string, extra ...[]string) string {
	t.Helper()
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "--quiet")
	git("config", "user.name", "Entire Graph Test")
	git("config", "user.email", "graph@example.com")
	git("config", "commit.gpgsign", "false")
	for name, content := range files {
		full := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	for _, args := range extra {
		git(args...)
	}
	git("commit", "--quiet", "-m", "fixture")
	return repo
}

func coverageSummary(t *testing.T, repo string, options ProviderSnapshotOptions) SnapshotSummary {
	t.Helper()
	var summary SnapshotSummary
	if err := StreamSnapshot(context.Background(), repo, "test", options, func(record any) error {
		if s, ok := record.(SnapshotSummary); ok {
			summary = s
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return summary
}

func coverageWarnings(summary SnapshotSummary, code string) []ProviderWarning {
	var found []ProviderWarning
	for _, warning := range summary.Warnings {
		if warning.Code == code {
			found = append(found, warning)
		}
	}
	return found
}

// TestGoBuildExcludedFilesAreNamed: a Go file outside the default build is still
// dropped, but no longer silently. Reason: a consumer that finds no file record
// must be able to tell "excluded by the build" from "nothing there", and the
// warning must not move partial failures, completeness or health.
func TestGoBuildExcludedFilesAreNamed(t *testing.T) {
	otherOS := "plan9"
	if build.Default.GOOS == otherOS {
		otherOS = "windows"
	}
	suffixed := "only_" + otherOS + ".go"
	repo := coverageFixtureRepo(t, map[string]string{
		"main.go":   "package demo\n\nfunc Main() {}\n",
		suffixed:    "package demo\n\nfunc Platform() {}\n",
		"tagged.go": "//go:build ignore\n\npackage demo\n\nfunc Tagged() {}\n",
	})
	for _, worktree := range []bool{false, true} {
		plain := coverageSummary(t, repo, ProviderSnapshotOptions{Worktree: worktree})
		named := coverageSummary(t, repo, ProviderSnapshotOptions{Worktree: worktree, CoverageWarnings: true})
		if got := coverageWarnings(plain, "W_GO_BUILD_EXCLUDED"); len(got) != 0 {
			t.Fatalf("worktree=%v: warnings without CoverageWarnings: %#v", worktree, got)
		}
		got := coverageWarnings(named, "W_GO_BUILD_EXCLUDED")
		if len(got) != 2 || got[0].FilePath != suffixed || got[1].FilePath != "tagged.go" {
			t.Fatalf("worktree=%v: build-excluded warnings = %#v", worktree, got)
		}
		if !strings.Contains(got[0].Detail, "file name suffix") || !strings.Contains(got[1].Detail, "build constraint") {
			t.Fatalf("worktree=%v: reasons = %q / %q", worktree, got[0].Detail, got[1].Detail)
		}
		for _, warning := range got {
			if warning.Severity != "info" || !strings.Contains(warning.EffectOnCompleteness, "no file record") {
				t.Fatalf("worktree=%v: warning shape %#v", worktree, warning)
			}
		}
		if !reflect.DeepEqual(plain.Stats, named.Stats) || !reflect.DeepEqual(plain.Completeness, named.Completeness) || !reflect.DeepEqual(plain.PartialFailures, named.PartialFailures) {
			t.Fatalf("worktree=%v: coverage warnings moved stats/completeness/health:\nplain=%#v\nnamed=%#v", worktree, plain.Stats, named.Stats)
		}
		if named.Stats.Files != 1 || named.Stats.CompletenessLevel != "ok" {
			t.Fatalf("worktree=%v: excluded files must stay out of the snapshot: %#v", worktree, named.Stats)
		}
	}
}
