//go:build windows

package sem

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// Why: Windows locks are mandatory; pinning a read must still permit reading
// through both its handle and a second reader, then permit pruning on close.
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
		if unlock, removable := lockArtifactForRemoval(root, entry.relative); removable {
			unlock()
			t.Fatal("an open read did not pin the artifact")
		}
	}
	for _, file := range readers {
		file.Close()
	}
	unlock, removable := lockArtifactForRemoval(root, entry.relative)
	if !removable {
		t.Fatal("closed reads still pin the artifact")
	}
	defer unlock()
	if err := removeLockedArtifact(root, entry.relative, unlock); err != nil {
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

// Why: A directory flush must use the admitted object after a namespace move.
func TestWindowsCacheDirectorySyncAfterRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "directory")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(path, path+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := syncCacheDirectory(root); err != nil {
		t.Fatal(err)
	}
}

// Why: Republishing equal-size bytes with equal mtime must invalidate memory
// and evidence receipts by file identity, not just by metadata.
func TestWindowsCacheIdentityDistinguishesReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
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
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	fresh, err := os.Open(path)
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
	for _, outside := range []string{
		"file://foreign/share/file.cpp", uri[:8] + otherDrive + uri[9:],
		lspFileURI(root + "-sibling/file.cpp"), lspFileURI(root) + "/../file.cpp",
		lspFileURI(root) + "/child/../file.cpp",
	} {
		if got := lspOriginalPath(outside, root); got != "" {
			t.Fatalf("outside URI %q admitted as %q", outside, got)
		}
	}
}
