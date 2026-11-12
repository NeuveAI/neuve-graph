package sem

import (
	"bufio"
	"encoding/json"
	"path"
	"regexp"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// Import target classification: one extra evidence entry per specifier on
// every IMPORTS relation of a provider record stream, saying where the
// specifier resolves in the snapshot's own tree. The entry is an ordinary
// five-field Evidence, so the compact v1 relation and evidence arities and the
// schema version stay as they are.
const (
	importTargetFile            = "import_target_file"
	importTargetDeclaredPackage = "import_target_declared_package"
	importTargetBuiltin         = "import_target_builtin"
	importTargetUnresolved      = "import_target_unresolved"
	importTargetNotClassified   = "import_target_not_classified"
)

// importTargetLanguages are the languages whose external specifiers are
// classified. A file target is reported for every language; an external one
// of any other language is importTargetNotClassified.
var importTargetLanguages = []string{"Go", "JavaScript", "Python", "TypeScript"}

// ImportTargetLanguages lists the languages whose IMPORTS relations carry a
// declared-package, builtin or unresolved classification.
func ImportTargetLanguages() []string { return append([]string(nil), importTargetLanguages...) }

func isImportTargetEvidence(kind string) bool { return strings.HasPrefix(kind, "import_target_") }

// importTargetIndex holds the package manifests of one snapshot's tree, read
// through the snapshot's validated content reader. It is built once before
// relation workers start and is read-only afterwards.
type importTargetIndex struct {
	packageJSON    map[string]jsManifest   // directory -> nearest package.json
	workspaceNames map[string]string       // package name -> its package.json
	goMod          map[string]goManifest   // directory -> go.mod
	python         map[string][]pyManifest // directory -> pyproject.toml / requirements*.txt
	pythonOwn      map[string]string       // normalized own project name -> pyproject.toml
	tsAliases      []tsPathMapping         // root tsconfig.json paths
}

type jsManifest struct {
	path     string
	declared map[string]bool
}

type goManifest struct {
	path     string
	module   string
	required []string
}

type pyManifest struct {
	path  string
	names map[string]bool // PEP 503 normalized distribution names
}

var requirementsFilePattern = regexp.MustCompile(`^requirements[^/]*\.txt$`)

// newImportTargetIndex reads every package.json, go.mod, pyproject.toml and
// requirements*.txt the listing holds.
func newImportTargetIndex(paths []string, read contentReader, tsAliases []tsPathMapping) *importTargetIndex {
	index := &importTargetIndex{
		packageJSON: map[string]jsManifest{}, workspaceNames: map[string]string{},
		goMod: map[string]goManifest{}, python: map[string][]pyManifest{}, pythonOwn: map[string]string{},
		tsAliases: tsAliases,
	}
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	for _, manifest := range sorted {
		manifest = path.Clean(strings.ReplaceAll(manifest, "\\", "/"))
		base, dir := path.Base(manifest), path.Dir(manifest)
		switch {
		case base == "package.json":
			content, ok := read(manifest)
			if !ok {
				continue
			}
			parsed := jsManifest{path: manifest, declared: map[string]bool{}}
			var data map[string]json.RawMessage
			if json.Unmarshal([]byte(content), &data) != nil {
				index.packageJSON[dir] = parsed
				continue
			}
			for _, field := range []string{"dependencies", "devDependencies", "peerDependencies", "optionalDependencies"} {
				var deps map[string]json.RawMessage
				if raw, ok := data[field]; ok && json.Unmarshal(raw, &deps) == nil {
					for name := range deps {
						parsed.declared[name] = true
					}
				}
			}
			index.packageJSON[dir] = parsed
			if name := parsePackageJSONName(content); name != "" {
				if _, seen := index.workspaceNames[name]; !seen {
					index.workspaceNames[name] = manifest
				}
			}
		case base == "go.mod":
			content, ok := read(manifest)
			if !ok {
				continue
			}
			index.goMod[dir] = goManifest{path: manifest, module: parseGoModulePath(content), required: parseGoModRequires(content)}
		case base == "pyproject.toml":
			content, ok := read(manifest)
			if !ok {
				continue
			}
			names, own := parsePyProjectDependencies(content)
			index.python[dir] = append(index.python[dir], pyManifest{path: manifest, names: names})
			if own != "" {
				if _, seen := index.pythonOwn[own]; !seen {
					index.pythonOwn[own] = manifest
				}
			}
		case requirementsFilePattern.MatchString(base):
			content, ok := read(manifest)
			if !ok {
				continue
			}
			index.python[dir] = append(index.python[dir], pyManifest{path: manifest, names: parseRequirementsNames(content)})
		}
	}
	return index
}

// nearestDir walks from the importing file's directory to the root and
// returns the first directory the lookup accepts.
func nearestDir(importingPath string, has func(dir string) bool) (string, bool) {
	dir := path.Dir(path.Clean(strings.ReplaceAll(importingPath, "\\", "/")))
	for {
		if has(dir) {
			return dir, true
		}
		if dir == "." || dir == "/" || dir == "" {
			return "", false
		}
		dir = path.Dir(dir)
	}
}

// classify answers for a specifier that did not resolve to a file in the
// tree. It returns the evidence kind and, for a declared package, the
// manifest that declares it.
func (index *importTargetIndex) classify(importingPath, specifier string) (string, string) {
	switch strings.ToLower(path.Ext(importingPath)) {
	case ".js", ".jsx", ".ts", ".tsx":
		return index.classifyJS(importingPath, specifier)
	case ".go":
		return index.classifyGo(importingPath, specifier)
	case ".py":
		return index.classifyPython(importingPath, specifier)
	}
	return importTargetNotClassified, ""
}

func (index *importTargetIndex) classifyJS(importingPath, specifier string) (string, string) {
	if specifier == "" || strings.HasPrefix(specifier, ".") || strings.HasPrefix(specifier, "/") {
		return importTargetUnresolved, ""
	}
	// Exact names only: node:<core module> (or one of the node:-only modules),
	// an unprefixed core module, and Bun's own modules. An unknown name under
	// either prefix is unresolved, never builtin.
	if name, prefixed := strings.CutPrefix(specifier, "node:"); prefixed && (nodeBuiltinModules[name] || nodePrefixOnlyModules[name]) {
		return importTargetBuiltin, ""
	}
	if nodeBuiltinModules[specifier] || bunModules[specifier] {
		return importTargetBuiltin, ""
	}
	if strings.HasPrefix(specifier, "node:") || strings.HasPrefix(specifier, "bun:") {
		return importTargetUnresolved, ""
	}
	// A root tsconfig.json alias that did not resolve to a file is unresolved,
	// never a package of the same name.
	for _, alias := range index.tsAliases {
		if tsAliasMatches(alias.Pattern, specifier) {
			return importTargetUnresolved, ""
		}
	}
	name, ok := jsPackageName(specifier)
	if !ok {
		return importTargetUnresolved, ""
	}
	if dir, found := nearestDir(importingPath, func(dir string) bool { _, ok := index.packageJSON[dir]; return ok }); found {
		manifest := index.packageJSON[dir]
		if manifest.declared[name] || manifest.declared[jsTypesPackageName(name)] {
			return importTargetDeclaredPackage, manifest.path
		}
	}
	if manifest, ok := index.workspaceNames[name]; ok {
		return importTargetDeclaredPackage, manifest
	}
	return importTargetUnresolved, ""
}

func tsAliasMatches(pattern, specifier string) bool {
	prefix, suffix, wildcard := strings.Cut(pattern, "*")
	if !wildcard {
		return pattern == specifier
	}
	return len(specifier) >= len(prefix)+len(suffix) && strings.HasPrefix(specifier, prefix) && strings.HasSuffix(specifier, suffix)
}

// jsPackageName is the package a bare specifier names: `@scope/name` for a
// scoped specifier, otherwise its first segment, so `pkg/sub` and
// `@scope/pkg/sub` are declared by their packages.
func jsPackageName(specifier string) (string, bool) {
	segments := strings.Split(specifier, "/")
	name := segments[0]
	if strings.HasPrefix(name, "@") {
		if len(segments) < 2 || len(name) < 2 || segments[1] == "" {
			return "", false
		}
		if !validJSPackageSegment(name[1:]) || !validJSPackageSegment(segments[1]) {
			return "", false
		}
		return name + "/" + segments[1], true
	}
	return name, validJSPackageSegment(name)
}

func validJSPackageSegment(segment string) bool {
	if segment == "" || strings.HasPrefix(segment, ".") || strings.HasPrefix(segment, "_") {
		return false
	}
	for _, r := range segment {
		if r <= ' ' || strings.ContainsRune("~'!()*#:@\\%\"<>?{}|^`", r) {
			return false
		}
	}
	return true
}

// jsTypesPackageName is the DefinitelyTyped package TypeScript resolves a
// package's types from: @types/name, or @types/scope__name.
func jsTypesPackageName(name string) string {
	if scope, rest, scoped := strings.Cut(strings.TrimPrefix(name, "@"), "/"); scoped && strings.HasPrefix(name, "@") {
		return "@types/" + scope + "__" + rest
	}
	return "@types/" + name
}

func (index *importTargetIndex) classifyGo(importingPath, specifier string) (string, string) {
	if dir, found := nearestDir(importingPath, func(dir string) bool { _, ok := index.goMod[dir]; return ok }); found {
		manifest := index.goMod[dir]
		if goPathWithin(specifier, manifest.module) {
			return importTargetDeclaredPackage, manifest.path
		}
		for _, module := range manifest.required {
			if goPathWithin(specifier, module) {
				return importTargetDeclaredPackage, manifest.path
			}
		}
	}
	// "C" is cgo's pseudo-package. A standard-library path is builtin only when
	// it names an importable package; internal/ and vendor/ standard packages
	// cannot be imported from outside the standard library, so they, like any
	// path not in the list, are unresolved.
	if specifier == "C" || goStandardPackages[specifier] {
		return importTargetBuiltin, ""
	}
	return importTargetUnresolved, ""
}

func goPathWithin(specifier, module string) bool {
	return module != "" && (specifier == module || strings.HasPrefix(specifier, module+"/"))
}

func (index *importTargetIndex) classifyPython(importingPath, specifier string) (string, string) {
	if specifier == "" || strings.HasPrefix(specifier, ".") {
		return importTargetUnresolved, ""
	}
	top, _, dotted := strings.Cut(specifier, ".")
	if pythonStdlibModules[top] {
		// The relation's specifier for `from m import n` is "m.n", so a dotted
		// name is builtin when it is a standard module or names something in
		// one (its parent is a standard module); a name inside a module is not
		// checked, in any language. A deeper path under no standard module is
		// unresolved rather than taken on its top package's word.
		parent := specifier
		if i := strings.LastIndex(specifier, "."); i >= 0 {
			parent = specifier[:i]
		}
		if !dotted || pythonStdlibSubmodules[specifier] || parent == top || pythonStdlibSubmodules[parent] {
			return importTargetBuiltin, ""
		}
		return importTargetUnresolved, ""
	}
	name := normalizePythonDistribution(top)
	if dir, found := nearestDir(importingPath, func(dir string) bool { return len(index.python[dir]) > 0 }); found {
		for _, manifest := range index.python[dir] {
			if manifest.names[name] {
				return importTargetDeclaredPackage, manifest.path
			}
		}
	}
	if manifest, ok := index.pythonOwn[name]; ok {
		return importTargetDeclaredPackage, manifest
	}
	// A distribution's import name need not be its own name (PyYAML provides
	// yaml), so a bare name no manifest declares is not classified, never
	// unresolved.
	return importTargetNotClassified, ""
}

var pythonDistributionSeparators = regexp.MustCompile(`[-_.]+`)

// normalizePythonDistribution is PEP 503 normalization. An import name matches
// a distribution only when the two are equal under it.
func normalizePythonDistribution(name string) string {
	return strings.ToLower(pythonDistributionSeparators.ReplaceAllString(strings.TrimSpace(name), "-"))
}

var pep508Name = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._-]*)`)

func pep508DistributionName(requirement string) string {
	if match := pep508Name.FindStringSubmatch(requirement); match != nil {
		return normalizePythonDistribution(match[1])
	}
	return ""
}

func parseRequirementsNames(content string) map[string]bool {
	names := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-") || strings.Contains(line, "://") {
			continue
		}
		if name := pep508DistributionName(line); name != "" {
			names[name] = true
		}
	}
	return names
}

// parsePyProjectDependencies reads PEP 621 dependencies and optional
// dependencies, PEP 735 dependency groups and Poetry dependency tables, plus
// the project's own name.
func parsePyProjectDependencies(content string) (map[string]bool, string) {
	names := map[string]bool{}
	var data struct {
		Project struct {
			Name                 string              `toml:"name"`
			Dependencies         []string            `toml:"dependencies"`
			OptionalDependencies map[string][]string `toml:"optional-dependencies"`
		} `toml:"project"`
		DependencyGroups map[string][]any `toml:"dependency-groups"`
		Tool             struct {
			Poetry struct {
				Name            string                    `toml:"name"`
				Dependencies    map[string]any            `toml:"dependencies"`
				DevDependencies map[string]any            `toml:"dev-dependencies"`
				Group           map[string]map[string]any `toml:"group"`
			} `toml:"poetry"`
		} `toml:"tool"`
	}
	if toml.Unmarshal([]byte(content), &data) != nil {
		return names, ""
	}
	add := func(requirement string) {
		if name := pep508DistributionName(requirement); name != "" {
			names[name] = true
		}
	}
	for _, requirement := range data.Project.Dependencies {
		add(requirement)
	}
	for _, group := range data.Project.OptionalDependencies {
		for _, requirement := range group {
			add(requirement)
		}
	}
	for _, group := range data.DependencyGroups {
		for _, entry := range group {
			if requirement, ok := entry.(string); ok {
				add(requirement)
			}
		}
	}
	poetryTables := []map[string]any{data.Tool.Poetry.Dependencies, data.Tool.Poetry.DevDependencies}
	for _, group := range data.Tool.Poetry.Group {
		if deps, ok := group["dependencies"].(map[string]any); ok {
			poetryTables = append(poetryTables, deps)
		}
	}
	for _, table := range poetryTables {
		for name := range table {
			if !strings.EqualFold(name, "python") {
				add(name)
			}
		}
	}
	own := data.Project.Name
	if own == "" {
		own = data.Tool.Poetry.Name
	}
	if own != "" {
		own = normalizePythonDistribution(own)
	}
	return names, own
}

// parseGoModRequires returns the module paths of a go.mod's require
// directives, single-line and block form.
func parseGoModRequires(content string) []string {
	var modules []string
	inBlock := false
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if inBlock {
			if fields[0] == ")" {
				inBlock = false
				continue
			}
			modules = append(modules, strings.Trim(fields[0], `"`))
			continue
		}
		if fields[0] != "require" {
			continue
		}
		if len(fields) >= 2 && fields[1] == "(" {
			inBlock = true
			continue
		}
		if len(fields) >= 2 {
			modules = append(modules, strings.Trim(fields[1], `"`))
		}
	}
	return modules
}

// nodeBuiltinModules are Node's core modules that may be imported without the
// node: prefix (require("module").builtinModules of Node 24, prefix-only and
// underscore-internal modules left out).
var nodeBuiltinModules = setOf(
	"assert", "assert/strict", "async_hooks", "buffer", "child_process", "cluster", "console", "constants",
	"crypto", "dgram", "diagnostics_channel", "dns", "dns/promises", "domain", "events", "fs", "fs/promises",
	"http", "http2", "https", "inspector", "inspector/promises", "module", "net", "os", "path", "path/posix",
	"path/win32", "perf_hooks", "process", "punycode", "querystring", "readline", "readline/promises", "repl",
	"stream", "stream/consumers", "stream/promises", "stream/web", "string_decoder", "sys", "timers",
	"timers/promises", "tls", "trace_events", "tty", "url", "util", "util/types", "v8", "vm", "wasi",
	"worker_threads", "zlib",
)

// nodePrefixOnlyModules are Node core modules that exist only with the node:
// prefix (Node 24).
var nodePrefixOnlyModules = setOf("sea", "sqlite", "test", "test/reporters")

// bunModules are Bun's own runtime modules (require("module").builtinModules of
// Bun 1.4.4 that start with bun).
var bunModules = setOf("bun", "bun:ffi", "bun:jsc", "bun:sqlite", "bun:test")

// goStandardPackageList holds the importable standard-library packages: `go list
// std` of Go 1.27.0 without internal/ and vendor/ paths, which code outside
// the standard library cannot import. Sorted; TestGoStandardPackagesPinned
// keeps it so.
var goStandardPackageList = []string{
	"archive/tar", "archive/zip", "bufio", "bytes", "cmp", "compress/bzip2", "compress/flate", "compress/gzip",
	"compress/lzw", "compress/zlib", "container/heap", "container/list", "container/ring", "context", "crypto",
	"crypto/aes", "crypto/cipher", "crypto/des", "crypto/dsa", "crypto/ecdh", "crypto/ecdsa", "crypto/ed25519",
	"crypto/elliptic", "crypto/fips140", "crypto/hkdf", "crypto/hmac", "crypto/hpke", "crypto/md5",
	"crypto/mldsa", "crypto/mlkem", "crypto/mlkem/mlkemtest", "crypto/pbkdf2", "crypto/rand", "crypto/rc4",
	"crypto/rsa", "crypto/sha1", "crypto/sha256", "crypto/sha3", "crypto/sha512", "crypto/subtle", "crypto/tls",
	"crypto/x509", "crypto/x509/pkix", "database/sql", "database/sql/driver", "debug/buildinfo", "debug/dwarf",
	"debug/elf", "debug/gosym", "debug/macho", "debug/pe", "debug/plan9obj", "embed", "encoding",
	"encoding/ascii85", "encoding/asn1", "encoding/base32", "encoding/base64", "encoding/binary",
	"encoding/csv", "encoding/gob", "encoding/hex", "encoding/json", "encoding/json/jsontext",
	"encoding/json/v2", "encoding/pem", "encoding/xml", "errors", "expvar", "flag", "fmt", "go/ast", "go/build",
	"go/build/constraint", "go/constant", "go/doc", "go/doc/comment", "go/format", "go/importer", "go/parser",
	"go/printer", "go/scanner", "go/token", "go/types", "go/version", "hash", "hash/adler32", "hash/crc32",
	"hash/crc64", "hash/fnv", "hash/maphash", "html", "html/template", "image", "image/color",
	"image/color/palette", "image/draw", "image/gif", "image/jpeg", "image/png", "index/suffixarray", "io",
	"io/fs", "io/ioutil", "iter", "log", "log/slog", "log/syslog", "maps", "math", "math/big", "math/bits",
	"math/cmplx", "math/rand", "math/rand/v2", "mime", "mime/multipart", "mime/quotedprintable", "net",
	"net/http", "net/http/cgi", "net/http/cookiejar", "net/http/fcgi", "net/http/httptest",
	"net/http/httptrace", "net/http/httputil", "net/http/pprof", "net/mail", "net/netip", "net/rpc",
	"net/rpc/jsonrpc", "net/smtp", "net/textproto", "net/url", "os", "os/exec", "os/signal", "os/user", "path",
	"path/filepath", "plugin", "reflect", "regexp", "regexp/syntax", "runtime", "runtime/cgo",
	"runtime/coverage", "runtime/debug", "runtime/metrics", "runtime/pprof", "runtime/race", "runtime/trace",
	"slices", "sort", "strconv", "strings", "structs", "sync", "sync/atomic", "syscall", "testing",
	"testing/cryptotest", "testing/fstest", "testing/iotest", "testing/quick", "testing/slogtest",
	"testing/synctest", "text/scanner", "text/tabwriter", "text/template", "text/template/parse", "time",
	"time/tzdata", "unicode", "unicode/utf16", "unicode/utf8", "unique", "unsafe", "uuid", "weak",
}

var goStandardPackages = setOf(goStandardPackageList...)

// pythonStdlibModules are the top-level standard-library modules
// (sys.stdlib_module_names of CPython 3.14) plus those removed since 3.11
// (PEP 594, distutils, imp, lib2to3), which older code still imports.
var pythonStdlibModules = setOf(
	"__future__", "_abc", "_aix_support", "_android_support", "_apple_support", "_ast", "_ast_unparse", "_asyncio",
	"_bisect", "_blake2", "_bz2", "_codecs", "_codecs_cn", "_codecs_hk", "_codecs_iso2022", "_codecs_jp", "_codecs_kr",
	"_codecs_tw", "_collections", "_collections_abc", "_colorize", "_compat_pickle", "_contextvars", "_csv", "_ctypes",
	"_curses", "_curses_panel", "_datetime", "_dbm", "_decimal", "_elementtree", "_frozen_importlib",
	"_frozen_importlib_external", "_functools", "_gdbm", "_hashlib", "_heapq", "_hmac", "_imp", "_interpchannels",
	"_interpqueues", "_interpreters", "_io", "_ios_support", "_json", "_locale", "_lsprof", "_lzma", "_markupbase",
	"_md5", "_multibytecodec", "_multiprocessing", "_opcode", "_opcode_metadata", "_operator", "_osx_support",
	"_overlapped", "_pickle", "_posixshmem", "_posixsubprocess", "_py_abc", "_py_warnings", "_pydatetime", "_pydecimal",
	"_pyio", "_pylong", "_pyrepl", "_queue", "_random", "_remote_debugging", "_scproxy", "_sha1", "_sha2", "_sha3",
	"_signal", "_sitebuiltins", "_socket", "_sqlite3", "_sre", "_ssl", "_stat", "_statistics", "_string", "_strptime",
	"_struct", "_suggestions", "_symtable", "_sysconfig", "_thread", "_threading_local", "_tkinter", "_tokenize",
	"_tracemalloc", "_types", "_typing", "_uuid", "_warnings", "_weakref", "_weakrefset", "_winapi", "_wmi", "_zoneinfo",
	"_zstd", "abc", "annotationlib", "antigravity", "argparse", "array", "ast", "asyncio", "atexit", "base64", "bdb",
	"binascii", "bisect", "builtins", "bz2", "cProfile", "calendar", "cmath", "cmd", "code", "codecs", "codeop",
	"collections", "colorsys", "compileall", "compression", "concurrent", "configparser", "contextlib", "contextvars",
	"copy", "copyreg", "csv", "ctypes", "curses", "dataclasses", "datetime", "dbm", "decimal", "difflib", "dis",
	"doctest", "email", "encodings", "ensurepip", "enum", "errno", "faulthandler", "fcntl", "filecmp", "fileinput",
	"fnmatch", "fractions", "ftplib", "functools", "gc", "genericpath", "getopt", "getpass", "gettext", "glob",
	"graphlib", "grp", "gzip", "hashlib", "heapq", "hmac", "html", "http", "idlelib", "imaplib", "importlib", "inspect",
	"io", "ipaddress", "itertools", "json", "keyword", "linecache", "locale", "logging", "lzma", "mailbox", "marshal",
	"math", "mimetypes", "mmap", "modulefinder", "msvcrt", "multiprocessing", "netrc", "nt", "ntpath", "nturl2path",
	"numbers", "opcode", "operator", "optparse", "os", "pathlib", "pdb", "pickle", "pickletools", "pkgutil", "platform",
	"plistlib", "poplib", "posix", "posixpath", "pprint", "profile", "pstats", "pty", "pwd", "py_compile", "pyclbr",
	"pydoc", "pydoc_data", "pyexpat", "queue", "quopri", "random", "re", "readline", "reprlib", "resource",
	"rlcompleter", "runpy", "sched", "secrets", "select", "selectors", "shelve", "shlex", "shutil", "signal", "site",
	"smtplib", "socket", "socketserver", "sqlite3", "sre_compile", "sre_constants", "sre_parse", "ssl", "stat",
	"statistics", "string", "stringprep", "struct", "subprocess", "symtable", "sys", "sysconfig", "syslog", "tabnanny",
	"tarfile", "tempfile", "termios", "textwrap", "this", "threading", "time", "timeit", "tkinter", "token", "tokenize",
	"tomllib", "trace", "traceback", "tracemalloc", "tty", "turtle", "turtledemo", "types", "typing", "unicodedata",
	"unittest", "urllib", "uuid", "venv", "warnings", "wave", "weakref", "webbrowser", "winreg", "winsound", "wsgiref",
	"xml", "xmlrpc", "zipapp", "zipfile", "zipimport", "zlib", "zoneinfo",
	// Removed from the standard library since Python 3.11.
	"aifc", "asynchat", "asyncore", "audioop", "cgi", "cgitb", "chunk", "crypt", "distutils", "imghdr", "imp",
	"lib2to3", "mailcap", "msilib", "nis", "nntplib", "ossaudiodev", "pipes", "smtpd", "sndhdr", "spwd", "sunau",
	"telnetlib", "uu", "xdrlib",
)

// pythonStdlibSubmodules are the standard library's dotted module names
// (pkgutil.walk_packages over CPython 3.14's standard library, test packages
// left out) plus the six aliases importing every standard module registers in
// sys.modules (collections.abc, os.path, pyexpat.errors, pyexpat.model,
// xml.parsers.expat.errors, xml.parsers.expat.model).
var pythonStdlibSubmodules = setOf(
	"_pyrepl.__main__", "_pyrepl._module_completer", "_pyrepl._threading_handler", "_pyrepl.base_eventqueue",
	"_pyrepl.commands", "_pyrepl.completing_reader", "_pyrepl.console", "_pyrepl.fancy_termios",
	"_pyrepl.historical_reader", "_pyrepl.input", "_pyrepl.keymap", "_pyrepl.main", "_pyrepl.pager",
	"_pyrepl.reader", "_pyrepl.readline", "_pyrepl.simple_interact", "_pyrepl.terminfo", "_pyrepl.trace",
	"_pyrepl.types", "_pyrepl.unix_console", "_pyrepl.unix_eventqueue", "_pyrepl.utils",
	"_pyrepl.windows_console", "_pyrepl.windows_eventqueue", "asyncio.__main__", "asyncio.base_events",
	"asyncio.base_futures", "asyncio.base_subprocess", "asyncio.base_tasks", "asyncio.constants",
	"asyncio.coroutines", "asyncio.events", "asyncio.exceptions", "asyncio.format_helpers", "asyncio.futures",
	"asyncio.graph", "asyncio.locks", "asyncio.log", "asyncio.mixins", "asyncio.proactor_events",
	"asyncio.protocols", "asyncio.queues", "asyncio.runners", "asyncio.selector_events", "asyncio.sslproto",
	"asyncio.staggered", "asyncio.streams", "asyncio.subprocess", "asyncio.taskgroups", "asyncio.tasks",
	"asyncio.threads", "asyncio.timeouts", "asyncio.tools", "asyncio.transports", "asyncio.trsock",
	"asyncio.unix_events", "asyncio.windows_events", "asyncio.windows_utils", "collections.abc",
	"compression._common", "compression._common._streams", "compression.bz2", "compression.gzip",
	"compression.lzma", "compression.zlib", "compression.zstd", "compression.zstd._zstdfile",
	"concurrent.futures", "concurrent.futures._base", "concurrent.futures.interpreter",
	"concurrent.futures.process", "concurrent.futures.thread", "concurrent.interpreters",
	"concurrent.interpreters._crossinterp", "concurrent.interpreters._queues", "ctypes._aix", "ctypes._endian",
	"ctypes._layout", "ctypes.macholib", "ctypes.macholib.dyld", "ctypes.macholib.dylib",
	"ctypes.macholib.framework", "ctypes.util", "ctypes.wintypes", "curses.ascii", "curses.has_key",
	"curses.panel", "curses.textpad", "dbm.dumb", "dbm.gnu", "dbm.ndbm", "dbm.sqlite3", "email._encoded_words",
	"email._header_value_parser", "email._parseaddr", "email._policybase", "email.base64mime", "email.charset",
	"email.contentmanager", "email.encoders", "email.errors", "email.feedparser", "email.generator",
	"email.header", "email.headerregistry", "email.iterators", "email.message", "email.mime",
	"email.mime.application", "email.mime.audio", "email.mime.base", "email.mime.image", "email.mime.message",
	"email.mime.multipart", "email.mime.nonmultipart", "email.mime.text", "email.parser", "email.policy",
	"email.quoprimime", "email.utils", "encodings._win_cp_codecs", "encodings.aliases", "encodings.ascii",
	"encodings.base64_codec", "encodings.big5", "encodings.big5hkscs", "encodings.bz2_codec",
	"encodings.charmap", "encodings.cp037", "encodings.cp1006", "encodings.cp1026", "encodings.cp1125",
	"encodings.cp1140", "encodings.cp1250", "encodings.cp1251", "encodings.cp1252", "encodings.cp1253",
	"encodings.cp1254", "encodings.cp1255", "encodings.cp1256", "encodings.cp1257", "encodings.cp1258",
	"encodings.cp273", "encodings.cp424", "encodings.cp437", "encodings.cp500", "encodings.cp720",
	"encodings.cp737", "encodings.cp775", "encodings.cp850", "encodings.cp852", "encodings.cp855",
	"encodings.cp856", "encodings.cp857", "encodings.cp858", "encodings.cp860", "encodings.cp861",
	"encodings.cp862", "encodings.cp863", "encodings.cp864", "encodings.cp865", "encodings.cp866",
	"encodings.cp869", "encodings.cp874", "encodings.cp875", "encodings.cp932", "encodings.cp949",
	"encodings.cp950", "encodings.euc_jis_2004", "encodings.euc_jisx0213", "encodings.euc_jp",
	"encodings.euc_kr", "encodings.gb18030", "encodings.gb2312", "encodings.gbk", "encodings.hex_codec",
	"encodings.hp_roman8", "encodings.hz", "encodings.idna", "encodings.iso2022_jp", "encodings.iso2022_jp_1",
	"encodings.iso2022_jp_2", "encodings.iso2022_jp_2004", "encodings.iso2022_jp_3", "encodings.iso2022_jp_ext",
	"encodings.iso2022_kr", "encodings.iso8859_1", "encodings.iso8859_10", "encodings.iso8859_11",
	"encodings.iso8859_13", "encodings.iso8859_14", "encodings.iso8859_15", "encodings.iso8859_16",
	"encodings.iso8859_2", "encodings.iso8859_3", "encodings.iso8859_4", "encodings.iso8859_5",
	"encodings.iso8859_6", "encodings.iso8859_7", "encodings.iso8859_8", "encodings.iso8859_9",
	"encodings.johab", "encodings.koi8_r", "encodings.koi8_t", "encodings.koi8_u", "encodings.kz1048",
	"encodings.latin_1", "encodings.mac_arabic", "encodings.mac_croatian", "encodings.mac_cyrillic",
	"encodings.mac_farsi", "encodings.mac_greek", "encodings.mac_iceland", "encodings.mac_latin2",
	"encodings.mac_roman", "encodings.mac_romanian", "encodings.mac_turkish", "encodings.mbcs", "encodings.oem",
	"encodings.palmos", "encodings.ptcp154", "encodings.punycode", "encodings.quopri_codec",
	"encodings.raw_unicode_escape", "encodings.rot_13", "encodings.shift_jis", "encodings.shift_jis_2004",
	"encodings.shift_jisx0213", "encodings.tis_620", "encodings.undefined", "encodings.unicode_escape",
	"encodings.utf_16", "encodings.utf_16_be", "encodings.utf_16_le", "encodings.utf_32", "encodings.utf_32_be",
	"encodings.utf_32_le", "encodings.utf_7", "encodings.utf_8", "encodings.utf_8_sig", "encodings.uu_codec",
	"encodings.zlib_codec", "ensurepip.__main__", "ensurepip._uninstall", "html.entities", "html.parser",
	"http.client", "http.cookiejar", "http.cookies", "http.server", "importlib._abc", "importlib._bootstrap",
	"importlib._bootstrap_external", "importlib.abc", "importlib.machinery", "importlib.metadata",
	"importlib.metadata._adapters", "importlib.metadata._collections", "importlib.metadata._functools",
	"importlib.metadata._itertools", "importlib.metadata._meta", "importlib.metadata._text",
	"importlib.metadata.diagnose", "importlib.readers", "importlib.resources", "importlib.resources._adapters",
	"importlib.resources._common", "importlib.resources._functional", "importlib.resources._itertools",
	"importlib.resources.abc", "importlib.resources.readers", "importlib.resources.simple", "importlib.simple",
	"importlib.util", "json.__main__", "json.decoder", "json.encoder", "json.scanner", "json.tool",
	"logging.config", "logging.handlers", "multiprocessing.connection", "multiprocessing.context",
	"multiprocessing.dummy", "multiprocessing.dummy.connection", "multiprocessing.forkserver",
	"multiprocessing.heap", "multiprocessing.managers", "multiprocessing.pool", "multiprocessing.popen_fork",
	"multiprocessing.popen_forkserver", "multiprocessing.popen_spawn_posix",
	"multiprocessing.popen_spawn_win32", "multiprocessing.process", "multiprocessing.queues",
	"multiprocessing.reduction", "multiprocessing.resource_sharer", "multiprocessing.resource_tracker",
	"multiprocessing.shared_memory", "multiprocessing.sharedctypes", "multiprocessing.spawn",
	"multiprocessing.synchronize", "multiprocessing.util", "os.path", "pathlib._local", "pathlib._os",
	"pathlib.types", "pydoc_data.module_docs", "pydoc_data.topics", "pyexpat.errors", "pyexpat.model",
	"re._casefix", "re._compiler", "re._constants", "re._parser", "sqlite3.__main__", "sqlite3.dbapi2",
	"sqlite3.dump", "string.templatelib", "sysconfig.__main__", "tomllib._parser", "tomllib._re",
	"tomllib._types", "turtledemo.__main__", "turtledemo.bytedesign", "turtledemo.chaos", "turtledemo.clock",
	"turtledemo.colormixer", "turtledemo.forest", "turtledemo.fractalcurves", "turtledemo.lindenmayer",
	"turtledemo.minimal_hanoi", "turtledemo.nim", "turtledemo.paint", "turtledemo.peace", "turtledemo.penrose",
	"turtledemo.planet_and_moon", "turtledemo.rosette", "turtledemo.round_dance", "turtledemo.sorting_animate",
	"turtledemo.tree", "turtledemo.two_canvases", "turtledemo.yinyang", "unittest.__main__", "unittest._log",
	"unittest.async_case", "unittest.case", "unittest.loader", "unittest.main", "unittest.mock",
	"unittest.result", "unittest.runner", "unittest.signals", "unittest.suite", "unittest.util", "urllib.error",
	"urllib.parse", "urllib.request", "urllib.response", "urllib.robotparser", "venv.__main__",
	"wsgiref.handlers", "wsgiref.headers", "wsgiref.simple_server", "wsgiref.types", "wsgiref.util",
	"wsgiref.validate", "xml.dom", "xml.dom.NodeFilter", "xml.dom.domreg", "xml.dom.expatbuilder",
	"xml.dom.minicompat", "xml.dom.minidom", "xml.dom.pulldom", "xml.dom.xmlbuilder", "xml.etree",
	"xml.etree.ElementInclude", "xml.etree.ElementPath", "xml.etree.ElementTree", "xml.etree.cElementTree",
	"xml.parsers", "xml.parsers.expat", "xml.parsers.expat.errors", "xml.parsers.expat.model", "xml.sax",
	"xml.sax._exceptions", "xml.sax.expatreader", "xml.sax.handler", "xml.sax.saxutils", "xml.sax.xmlreader",
	"xmlrpc.client", "xmlrpc.server", "zipfile.__main__", "zipfile._path", "zipfile._path.glob",
	"zoneinfo._common", "zoneinfo._tzpath", "zoneinfo._zoneinfo",
)

func setOf(values ...string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

// importTargetEvidenceOnly keeps the import target entries of an evidence list
// for profiles that drop provenance evidence.
func importTargetEvidenceOnly(evidence []Evidence) []Evidence {
	var kept []Evidence
	for _, item := range evidence {
		if isImportTargetEvidence(item.Kind) {
			kept = append(kept, item)
		}
	}
	return kept
}
