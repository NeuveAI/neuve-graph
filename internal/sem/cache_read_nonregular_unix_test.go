//go:build !windows

package sem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func requireFIFOReadReturns(t *testing.T, path string, read func() error) error {
	t.Helper()
	returned := make(chan error, 1)
	go func() { returned <- read() }()
	select {
	case err := <-returned:
		return err
	case <-time.After(2 * time.Second):
		// Release a regressed blocking read-open before failing; no goroutine
		// should remain parked just because the bounded control caught a bug.
		if descriptor, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = syscall.Close(descriptor)
		}
		select {
		case <-returned:
		case <-time.After(2 * time.Second):
		}
		t.Fatal("retained artifact read blocked on a writerless FIFO")
		return nil
	}
}

func TestCacheEntryReadRejectsFIFOAfterRegularPathSwap(t *testing.T) {
	for _, family := range []string{"extractions", "generations"} {
		t.Run(family, func(t *testing.T) {
			entry, err := newCacheEntry(t.TempDir(), family, "v1", strings.Repeat("d", 64))
			if err != nil {
				t.Fatal(err)
			}
			if err := entry.write("regular-control", map[string]string{"value": "retained"}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(entry.root, entry.relative)
			before, err := os.Stat(path)
			if err != nil || !before.Mode().IsRegular() {
				t.Fatalf("regular positive control=%v, %v", before, err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			err = requireFIFOReadReturns(t, path, func() error {
				file, err := entry.open()
				if file != nil {
					_ = file.Close()
					return errors.New("special file returned as a readable artifact")
				}
				return err
			})
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("swapped FIFO read error=%v", err)
			}
		})
	}
}

func TestCacheRootNonblockingOpenerReachesFIFOHandleValidation(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "artifact")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	err = requireFIFOReadReturns(t, path, func() error {
		// No preliminary path stat: the production opener itself must reach
		// handle validation even when the name becomes a FIFO before open.
		file, err := openRootBoundedRegularFile(root, "artifact")
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		regular, err := openedFileIsRegular(file, info)
		if err != nil {
			return err
		}
		if regular || info.Mode()&os.ModeNamedPipe == 0 {
			return errors.New("opened FIFO was reported as a regular cache artifact")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIncrementalNonregularArtifactsRefuseWithoutParsingOrPublication(t *testing.T) {
	for _, family := range []string{"extraction", "generation"} {
		t.Run(family, func(t *testing.T) {
			repo, cache := incrementalFixture(t), t.TempDir()
			initial := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
			entry, err := newCacheEntry(cache, "generations", preparedGenerationVersion, initial.ID)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(cache, entry.relative)
			if family == "extraction" {
				paths, err := filepath.Glob(filepath.Join(cache, "extractions", extractionCacheVersion, "*.json.gz"))
				if err != nil || len(paths) != 6 {
					t.Fatalf("extraction controls=%v, %v", paths, err)
				}
				path = paths[0]
				git(t, repo, "commit", "--allow-empty", "-m", "require retained extraction")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			before := StructuralWorkSnapshot()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			err = requireFIFOReadReturns(t, path, func() error {
				_, err := PrepareIncrementalProviderSnapshot(ctx, repo, incrementalHead(t, repo), "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
				return err
			})
			var failure *IncrementalPreparationError
			if !errors.As(err, &failure) || !strings.Contains(errCauseText(err), "not a regular file") {
				t.Fatalf("nonregular artifact typed refusal=%v", err)
			}
			if StructuralWorkSnapshot().Parses != before.Parses {
				t.Fatal("nonregular artifact triggered fallback parsing")
			}
			generations, err := filepath.Glob(filepath.Join(cache, "generations", preparedGenerationVersion, "*.json.gz"))
			if err != nil || len(generations) != 1 {
				t.Fatalf("nonregular candidate published readiness: %v, %v", generations, err)
			}
			cancel()
			_, err = PrepareIncrementalProviderSnapshot(ctx, repo, incrementalHead(t, repo), "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled artifact read lost cancellation cause: %v", err)
			}
		})
	}
}

func errCauseText(err error) string {
	var causes []string
	for err != nil {
		causes = append(causes, err.Error())
		err = errors.Unwrap(err)
	}
	return strings.Join(causes, ": ")
}
