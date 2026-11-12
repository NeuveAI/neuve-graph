package sem

import (
	"slices"
	"strings"
	"testing"
)

// The pinned standard-library list must stay a sorted, non-empty set of
// importable paths: no internal/ or vendor/ path, and the packages a typo
// would be mistaken for are present.
func TestGoStandardPackagesPinned(t *testing.T) {
	if len(goStandardPackageList) < 150 || !slices.IsSorted(goStandardPackageList) {
		t.Fatalf("list has %d entries, sorted %v", len(goStandardPackageList), slices.IsSorted(goStandardPackageList))
	}
	for _, path := range goStandardPackageList {
		if path == "internal" || strings.HasPrefix(path, "internal/") || strings.Contains(path, "/internal") || strings.HasPrefix(path, "vendor/") {
			t.Errorf("%s cannot be imported from outside the standard library", path)
		}
	}
	for _, path := range []string{"fmt", "unsafe", "embed", "net/http", "math/rand/v2", "iter"} {
		if !goStandardPackages[path] {
			t.Errorf("%s missing", path)
		}
	}
}

// Builtin is an answer the consumer treats as resolved, so it must never come
// from a pattern that also matches unknown names: each list is exact, and an
// unknown name under a builtin prefix is unresolved.
func TestImportTargetBuiltinsAreExact(t *testing.T) {
	index := newImportTargetIndex([]string{"go.mod", "package.json"}, func(path string) (string, bool) {
		switch path {
		case "go.mod":
			return "module example.com/m\n\nrequire github.com/dep/one v1.0.0\n", true
		case "package.json":
			return `{"name":"app","dependencies":{"left-pad":"1"}}`, true
		}
		return "", false
	}, nil)
	cases := []struct{ file, specifier, want string }{
		{"main.go", "fmt", importTargetBuiltin},
		{"main.go", "net/http", importTargetBuiltin},
		{"main.go", "math/rand/v2", importTargetBuiltin},
		{"main.go", "unsafe", importTargetBuiltin},
		{"main.go", "C", importTargetBuiltin},                                         // cgo's pseudo-package
		{"main.go", "fmtt", importTargetUnresolved},                                   // a typo of a standard package
		{"main.go", "notstd/pkg", importTargetUnresolved},                             // no dot, not standard
		{"main.go", "internal/abi", importTargetUnresolved},                           // standard but not importable
		{"main.go", "crypto/internal/fips140", importTargetUnresolved},                // standard internal
		{"main.go", "vendor/golang.org/x/net/dns/dnsmessage", importTargetUnresolved}, // standard vendor
		{"main.go", "example.com/m/sub", importTargetDeclaredPackage},
		{"main.go", "github.com/dep/one/x", importTargetDeclaredPackage},
		{"a.ts", "node:fs", importTargetBuiltin},
		{"a.ts", "node:fs/promises", importTargetBuiltin},
		{"a.ts", "node:test", importTargetBuiltin},   // exists only with the prefix
		{"a.ts", "node:fss", importTargetUnresolved}, // unknown name under node:
		{"a.ts", "fs", importTargetBuiltin},
		{"a.ts", "fs/nothing", importTargetUnresolved}, // not a core module subpath
		{"a.ts", "test", importTargetUnresolved},       // node:test has no unprefixed form
		{"a.ts", "bun", importTargetBuiltin},
		{"a.ts", "bun:test", importTargetBuiltin},
		{"a.ts", "bun:nothing", importTargetUnresolved}, // unknown name under bun:
		{"a.ts", "left-pad", importTargetDeclaredPackage},
		{"a.py", "os", importTargetBuiltin},
		{"a.py", "osx", importTargetNotClassified}, // not standard; a distribution may provide it
		{"a.py", "os.path", importTargetBuiltin},
		{"a.py", "collections.abc", importTargetBuiltin},
		{"a.py", "xml.etree.ElementTree", importTargetBuiltin},
		// A dotted name is builtin only when it is itself a standard module.
		{"a.py", "os.pathh", importTargetUnresolved},
		{"a.py", "email.mim", importTargetUnresolved},
		{"a.py", "xml.etre", importTargetUnresolved},
		{"a.py", "json.decodr", importTargetUnresolved},
		{"a.py", "email.mime", importTargetBuiltin},
		{"a.py", "json.decoder", importTargetBuiltin},
		// `from pathlib import Path` and `from os import getcwd` are scanned
		// as module.name, which is no module: unresolved. The statement's
		// own module ("pathlib", "os") has its own builtin entry.
		{"a.py", "pathlib.Path", importTargetUnresolved},
		{"a.py", "os.getcwd", importTargetUnresolved},
		{"a.py", "xml.etree.ElementTree.Element", importTargetUnresolved},
		{"main.go", "syscall/js", importTargetBuiltin}, // standard under GOOS=js
	}
	for _, tc := range cases {
		if got, _ := index.classify(tc.file, tc.specifier); got != tc.want {
			t.Errorf("%s %q: %s, want %s", tc.file, tc.specifier, got, tc.want)
		}
	}
}
