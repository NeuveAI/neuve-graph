package gitutil

import (
	"context"
	"testing"
)

// The Git processes started under an accounted context count their CPU there,
// and only there.
func TestChildCPUAccountsItsOwnProcesses(t *testing.T) {
	repo := t.TempDir()
	if _, err := run(context.Background(), repo, "git", "init", "-q"); err != nil {
		t.Fatal(err)
	}
	ctx, account := WithChildCPU(t.Context())
	other, untouched := WithChildCPU(t.Context())
	for range 5 {
		if _, err := run(ctx, repo, "git", "rev-parse", "--git-dir"); err != nil {
			t.Fatal(err)
		}
	}
	if user, system := account.Total(); user+system <= 0 {
		t.Fatalf("five Git processes accounted %v + %v", user, system)
	}
	if _, err := run(other, repo, "git", "rev-parse", "--git-dir"); err != nil {
		t.Fatal(err)
	}
	if len(untouched.cmds) != 1 || len(account.cmds) != 5 {
		t.Fatalf("accounts hold %d and %d processes, want 5 and 1", len(account.cmds), len(untouched.cmds))
	}
}
