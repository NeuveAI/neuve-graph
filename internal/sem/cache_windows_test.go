//go:build windows

package sem

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// Why: Windows locks are mandatory; pinning a read must still permit reading
// through both its handle and a second reader, then permit pruning on close.
// Read-only artifacts must remain removable, as with os.Root.Remove.
func TestWindowsCacheReadHoldAndRemoval(t *testing.T) {
	cacheDir := t.TempDir()
	entry, err := newCacheEntry(cacheDir, "search", searchSnapshotCacheVersion, key('d'))
	if err != nil {
		t.Fatal(err)
	}
	if err := entry.write("snapshot", map[string]string{"value": "readable"}); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var readers []*os.File
	for range 2 {
		file, err := entry.open()
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		readers = append(readers, file)
		reader, err := gzip.NewReader(file)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil || string(data) != "{\"value\":\"readable\"}\n" {
			t.Fatalf("held artifact read = %q, %v", data, err)
		}
		if lock, removable := lockArtifactForRemoval(root, entry.relative); removable {
			lock.unlock()
			t.Fatal("an open read did not pin the artifact")
		}
	}
	for _, file := range readers {
		file.Close()
	}
	if err := root.Chmod(entry.relative, 0o400); err != nil {
		t.Fatal(err)
	}
	lock, removable := lockArtifactForRemoval(root, entry.relative)
	if !removable {
		t.Fatal("closed reads still pin the artifact")
	}
	defer lock.unlock()
	if _, err := lock.remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Stat(entry.relative); !os.IsNotExist(err) {
		t.Fatalf("removed artifact stat = %v", err)
	}
}

// Why: Store locks must exclude a second pruner and release on close.
func TestWindowsCacheStoreLockExclusion(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	unlock, err := lockCacheStoreFile(root, "lock", false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if second, err := lockCacheStoreFile(root, "lock", false); err == nil {
		second()
		t.Fatal("two exclusive store locks succeeded")
	}
	unlock()
	second, err := lockCacheStoreFile(root, "lock", false)
	if err != nil {
		t.Fatal(err)
	}
	second()
}

// Why: Windows os.OpenRoot prevents renaming its directly held directory. A normal
// immutable publication must reach the real FlushFileBuffers and propagate
// its errors; a rename fixture cannot exercise that property on Windows.
func TestWindowsCacheDirectorySyncPublication(t *testing.T) {
	entry, err := newCacheEntry(t.TempDir(), "generations", preparedGenerationVersion, key('a'))
	if err != nil {
		t.Fatal(err)
	}
	original := cacheFlushFileBuffers
	t.Cleanup(func() { cacheFlushFileBuffers = original })
	calls := 0
	cacheFlushFileBuffers = func(handle windows.Handle) error {
		calls++
		return original(handle)
	}
	if err := entry.writeImmutable(context.Background(), "publish", map[string]string{"value": "ready"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("directory flush calls = %d, want 1", calls)
	}
	failure := errors.New("directory flush failed")
	cacheFlushFileBuffers = func(windows.Handle) error { return failure }
	second, err := newCacheEntry(entry.root, "generations", preparedGenerationVersion, key('b'))
	if err != nil {
		t.Fatal(err)
	}
	if err := second.writeImmutable(context.Background(), "publish", "other"); !errors.Is(err, failure) {
		t.Fatalf("publication flush error = %v, want injected error", err)
	}
}

// Why: The path-opened flush handle must identify the admitted directory.
// Check the same comparison used before FlushFileBuffers with real handles.
func TestWindowsCacheDirectorySyncIdentityMismatch(t *testing.T) {
	var files []*os.File
	for range 2 {
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		file, err := root.Open(".")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		files = append(files, file)
	}
	first, second := windows.Handle(files[0].Fd()), windows.Handle(files[1].Fd())
	if err := verifyCacheWindowsObject(first, first); err != nil {
		t.Fatalf("same directory rejected: %v", err)
	}
	if err := verifyCacheWindowsObject(first, second); err == nil {
		t.Fatal("different directory admitted for flush")
	}
}

// Why: A publisher can replace each artifact after all unit locks were taken
// but before retirement. The replacement reader holds B, not the locked A.
// Both main and derived artifacts must retire through their original handles.
func TestWindowsCacheRetirementKeepsReplacement(t *testing.T) {
	for _, moveOriginal := range []bool{false, true} {
		mode := "replace"
		if moveOriginal {
			mode = "move"
		}
		t.Run(mode, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			names := []string{"main", "derived"}
			var locks []*cacheArtifactRemoval
			var originals, readers []*os.File
			for _, name := range names {
				if err := root.WriteFile(name, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
				original, err := root.Open(name)
				if err != nil {
					t.Fatal(err)
				}
				defer original.Close()
				originals = append(originals, original)
				lock, ok := lockArtifactForRemoval(root, name)
				if !ok {
					t.Fatalf("could not lock %s", name)
				}
				defer lock.unlock()
				locks = append(locks, lock)
			}
			// All locks are acquired, just as in removeCacheUnit. Publish B
			// now, deterministically inside the review's interleaving window.
			for _, name := range names {
				if moveOriginal {
					if err := root.Rename(name, name+".old"); err != nil {
						t.Fatal(err)
					}
				}
				if err := root.WriteFile(name+".new", []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := root.Rename(name+".new", name); err != nil {
					t.Fatal(err)
				}
				reader, err := root.Open(name)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				readers = append(readers, reader)
				if !holdArtifactForRead(root, name, reader) {
					t.Fatalf("replacement %s was not admitted", name)
				}
			}
			for index, lock := range locks {
				size, err := lock.remove()
				if err != nil || size != 3 {
					t.Fatalf("retire %s = %d, %v; want 3, nil", names[index], size, err)
				}
				info, err := cacheWindowsStandardInformation(windows.Handle(originals[index].Fd()))
				if err != nil || (info.NumberOfLinks != 0 && info.DeletePending == 0) {
					t.Fatalf("A still linked after retirement: %+v, %v", info, err)
				}
				if moveOriginal {
					if _, err := root.Stat(names[index] + ".old"); !os.IsNotExist(err) {
						t.Fatalf("moved A still named: %v", err)
					}
				}
				named, err := root.ReadFile(names[index])
				if err != nil || string(named) != "replacement" {
					t.Fatalf("B name after retirement = %q, %v", named, err)
				}
				held, err := io.ReadAll(readers[index])
				if err != nil || string(held) != "replacement" {
					t.Fatalf("B held read after retirement = %q, %v", held, err)
				}
			}
		})
	}
}

// Why: Classic disposition leaves A delete-pending while a pre-opened reader
// holds it. That reader must fail admission even after the remover closes.
func TestWindowsCacheDeletePendingReaderRejected(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("artifact", []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := root.Open("artifact")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	remover, err := openCacheArtifactForRemoval(root, "artifact")
	if err != nil {
		t.Fatal(err)
	}
	defer remover.Close()
	pending := byte(1)
	if err := windows.SetFileInformationByHandle(windows.Handle(remover.Fd()), windows.FileDispositionInfo, &pending, 1); err != nil {
		t.Fatal(err)
	}
	if err := remover.Close(); err != nil {
		t.Fatal(err)
	}
	if holdArtifactForRead(root, "artifact", reader) {
		t.Fatal("delete-pending reader admitted")
	}
	if fresh, err := root.Open("artifact"); err == nil {
		fresh.Close()
		t.Fatal("new open of delete-pending artifact succeeded")
	}
}

// Why: Republishing equal-size bytes with equal mtime must invalidate memory
// and evidence receipts by file identity, not just by metadata. Root-relative
// opens share delete access, so the fixture permits publication over old.
func TestWindowsCacheIdentityDistinguishesReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	old, err := root.Open("artifact")
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	info, err := old.Stat()
	if err != nil {
		t.Fatal(err)
	}
	before, ok := cacheFileIdentity(old, info)
	if !ok {
		t.Fatal("open file has no identity")
	}
	if err := os.WriteFile(path+".new", []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path+".new", info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := root.Rename("artifact.new", "artifact"); err != nil {
		t.Fatal(err)
	}
	fresh, err := root.Open("artifact")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	freshInfo, err := fresh.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if freshInfo.Size() != info.Size() || !freshInfo.ModTime().Equal(info.ModTime()) {
		t.Fatal("replacement did not preserve size and modification time")
	}
	after, ok := cacheFileIdentity(fresh, freshInfo)
	if !ok || before == after {
		t.Fatalf("replacement identity = %q, %v; previous %q", after, ok, before)
	}
	if identityOf(old, info) == IdentifyFile(path) {
		t.Fatal("evidence receipt missed replacement")
	}
}

// Why: LSP servers may return lower-case drives, escaped colons and long
// names even when Go's temp directory came from an 8.3 environment path.
// Foreign UNC, device and extended namespaces must be refused before lookup.
func TestWindowsLSPURIPathNormalization(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.cpp")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	uri := lspFileURI(path)
	// A drive letter is at byte 8 in file:///C:/..., not in the authority.
	if len(uri) < 10 || uri[:8] != "file:///" || uri[9] != ':' {
		t.Fatalf("not a drive file URI: %q", uri)
	}
	drive := uri[8]
	if drive >= 'A' && drive <= 'Z' {
		drive += 'a' - 'A'
	}
	encoded := uri[:8] + string(drive) + "%3A" + uri[10:]
	if got := lspOriginalPath(encoded, root); got != "file.cpp" {
		t.Fatalf("lower-case escaped drive = %q", got)
	}
	longRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	ptr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, 32768)
	n, err := windows.GetShortPathName(ptr, &buffer[0], uint32(len(buffer)))
	if err != nil || n >= uint32(len(buffer)) {
		t.Fatalf("short path = %d, %v", n, err)
	}
	shortRoot := windows.UTF16ToString(buffer[:n])
	for _, pair := range [][2]string{{longRoot, shortRoot}, {shortRoot, longRoot}} {
		if got := lspOriginalPath(lspFileURI(filepath.Join(pair[0], "file.cpp")), pair[1]); got != "file.cpp" {
			t.Fatalf("short/long path qualification = %q", got)
		}
	}
	otherDrive := "Z"
	if drive == 'z' {
		otherDrive = "Y"
	}
	foreignNamespaces := []string{
		"file:////foreign/share/file.cpp",
		"file:////./UNC/foreign/share/file.cpp",
		"file:////%3F/UNC/foreign/share/file.cpp",
		"file:////%3F/" + otherDrive + ":/foreign/file.cpp",
		"file:////./" + otherDrive + ":/foreign/file.cpp",
		"file:///%5C%5Cforeign%5Cshare%5Cfile.cpp",
		"file:///%5C%5C%3F%5CUNC%5Cforeign%5Cshare%5Cfile.cpp",
		"file:///%5C%3F%3F%5CUNC%5Cforeign%5Cshare%5Cfile.cpp",
		"file:///%5CDevice%5CMup%5Cforeign%5Cshare%5Cfile.cpp",
	}
	rootURI, err := url.Parse(lspFileURI(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, outside := range foreignNamespaces {
		parsed, err := url.Parse(outside)
		if err != nil {
			t.Fatal(err)
		}
		// This pure admission function runs before EvalSymlinks. A final
		// empty location alone would not prove absence of a network lookup.
		if _, err := lspAdmittedWindowsURIPath(parsed, rootURI); err == nil {
			t.Fatalf("namespace %q reached filesystem normalization", outside)
		}
		if got := lspOriginalPath(outside, root); got != "" {
			t.Fatalf("outside URI %q admitted as %q", outside, got)
		}
	}
	for _, outside := range []string{
		"file://foreign/share/file.cpp",
		uri[:8] + otherDrive + uri[9:],
		lspFileURI(root + "-sibling/file.cpp"), lspFileURI(root) + "/../file.cpp",
		lspFileURI(root) + "/child/../file.cpp",
	} {
		if got := lspOriginalPath(outside, root); got != "" {
			t.Fatalf("outside URI %q admitted as %q", outside, got)
		}
	}
}
