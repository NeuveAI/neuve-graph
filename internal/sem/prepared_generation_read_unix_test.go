//go:build !windows

package sem

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPreparedSyntaxGenerationFIFORefusalCannotUseLegacyCache(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	options := ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}
	if _, _, err := PreindexProviderSnapshot(t.Context(), repo, "test", options, cache); err != nil {
		t.Fatal(err)
	}
	generation := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	entry, err := newCacheEntry(cache, "generations", preparedGenerationVersion, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cache, entry.relative)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	before := StructuralWorkSnapshot()
	err = requireFIFOReadReturns(t, path, func() error {
		_, err := LoadPreparedProviderSnapshot(t.Context(), repo, "test", options, cache)
		return err
	})
	requirePreparedFailure(t, err, "invalid-artifact")
	if StructuralWorkSnapshot() != before {
		t.Fatal("FIFO generation read did replacement structural work")
	}
}
