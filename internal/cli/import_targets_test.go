package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

// importTargetFixture is one commit's tree with every classification each
// covered language can give, and a later commit that would answer several of
// them differently, so a test reading the first commit by --commit proves the
// answers come from that commit's tree, never from HEAD or the working tree.
func importTargetFixture(t *testing.T) (repo, first string) {
	t.Helper()
	repo = t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Graph test")
	git(t, repo, "config", "user.email", "graph@example.com")
	files := map[string]string{
		"package.json":  `{"name":"root","workspaces":["web","shared"]}`,
		"tsconfig.json": `{"compilerOptions":{"baseUrl":".","paths":{"@app/*":["web/src/*"]}}}`,
		"web/package.json": `{"name":"@acme/web","dependencies":{"react":"1","@scope/pkg":"1"},` +
			`"devDependencies":{"@types/lodash":"1"},"peerDependencies":{"peer":"1"},"optionalDependencies":{"opt":"1"}}`,
		"shared/package.json":      `{"name":"@acme/shared"}`,
		"web/nested/tsconfig.json": `{"compilerOptions":{"paths":{"lib/*":["../src/*"]}}}`,
		"web/src/util.ts":          "export const u = 1;\n",
		"web/src/types.ts":         "export type T = number;\nexport const K = 1;\n",
		"web/src/main.ts":          "import { u } from \"./util\";\nimport { u as again } from \"./util.ts\";\nimport type { T } from \"./types\";\nimport React from \"react\";\nimport { jsx } from \"react/jsx-runtime\";\nimport { sub } from \"@scope/pkg/sub\";\nimport p from \"peer\";\nimport o from \"opt\";\nimport _ from \"lodash\";\nimport s from \"@acme/shared\";\nimport fs from \"node:fs\";\nimport path from \"path\";\nimport promises from \"fs/promises\";\nimport { test } from \"bun:test\";\nimport { $ } from \"bun\";\nimport left from \"left-pad\";\nimport gone from \"./gone\";\nimport missing from \"@app/missing\";\nimport aliased from \"lib/thing\";\n",
		"web/src/legacy.js":        "import React from 'react';\nimport x from 'undeclared-js';\n",
		"go.mod":                   "module example.com/svc\n\ngo 1.22\n\nrequire (\n\tgithub.com/dep/one v1.0.0 // indirect\n)\n\nrequire golang.org/x/text v0.1.0\n",
		"gosvc/util/util.go":       "package util\n\nfunc U() {}\n",
		"gosvc/main.go":            "package main\n\nimport (\n\t\"fmt\"\n\t\"net/http\"\n\t\"example.com/svc/gosvc/util\"\n\t\"example.com/svc/internal/absent\"\n\t\"github.com/dep/one/sub\"\n\t\"golang.org/x/text\"\n\t\"github.com/undeclared/x\"\n)\n\nfunc main() { fmt.Println(http.StatusOK); util.U() }\n",
		"nested/go.mod":            "module example.com/nested\n\ngo 1.22\n",
		"nested/a/a.go":            "package a\n\nfunc A() {}\n",
		"nested/main.go":           "package main\n\nimport \"example.com/nested/a\"\n\nfunc main() { a.A() }\n",
		"py/pyproject.toml":        "[project]\nname = \"acme-tool\"\ndependencies = [\"requests>=2\", \"PyYAML\"]\n[project.optional-dependencies]\nfast = [\"ujson\"]\n",
		"py/requirements-dev.txt":  "pytest>=8  # tests\n-r other.txt\n",
		"py/pkg/__init__.py":       "",
		"py/pkg/helper.py":         "def h():\n    return 1\n",
		"py/pkg/app.py":            "import os\nimport requests\nimport ujson\nimport pytest\nimport yaml\nimport numpy\nfrom . import helper\nfrom .gone import thing\n",
		"rust/lib.rs":              "use std::fmt;\nfn main() {}\n",
	}
	for path, content := range files {
		write(t, repo, path, content)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "fixture")
	first = rev(t, repo, "HEAD")
	// HEAD and the working tree now disagree with the first commit: left-pad
	// and numpy become declared, react is no longer declared, ./gone exists.
	write(t, repo, "web/package.json", `{"name":"@acme/web","dependencies":{"left-pad":"1"}}`)
	write(t, repo, "web/src/gone.ts", "export default 1;\n")
	write(t, repo, "py/requirements-dev.txt", "numpy\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "later")
	write(t, repo, "py/requirements-dev.txt", "yaml\n")
	return repo, first
}

type importTarget struct{ kind, path string }

// importTargets reads the snapshot's IMPORTS relations as (file, specifier) ->
// classification, checking each relation carries exactly one class.
func importTargets(t *testing.T, output []byte) (map[[2]string]importTarget, int) {
	t.Helper()
	targets := map[[2]string]importTarget{}
	relations := 0
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte("\n")) {
		var record sem.RelationRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.RecordType != "relation" || record.Type != "IMPORTS" {
			continue
		}
		relations++
		from := record.FromID[strings.LastIndex(record.FromID, ":file:")+len(":file:"):]
		kinds := map[string]bool{}
		for _, evidence := range record.Evidence {
			if !strings.HasPrefix(evidence.Kind, "import_target_") {
				continue
			}
			kinds[evidence.Kind] = true
			if evidence.StartLine != 0 || evidence.EndLine != 0 {
				t.Errorf("%s %s: classification carries lines", from, evidence.Detail)
			}
			targets[[2]string{from, evidence.Detail}] = importTarget{evidence.Kind, evidence.FilePath}
		}
		// One class per relation, except that a Python relative from-import's
		// own module (".", "..") rides on the relation of its first name.
		if len(kinds) != 1 && !(strings.HasSuffix(from, ".py") && len(kinds) == 2) {
			t.Errorf("%s -> %s: %d classifications, want exactly one", from, record.ToID, len(kinds))
		}
	}
	return targets, relations
}

// Every specifier of every covered language gets the class its commit's tree
// gives it, at full and fast profile, read by --commit with HEAD elsewhere.
func TestSnapshotImportTargetsAtExactCommit(t *testing.T) {
	repo, first := importTargetFixture(t)
	const (
		file, declared, builtin, unresolved, notClassified = "import_target_file", "import_target_declared_package", "import_target_builtin", "import_target_unresolved", "import_target_not_classified"
	)
	want := map[[2]string]importTarget{
		{"web/src/main.ts", "./util"}:                   {file, "web/src/util.ts"},
		{"web/src/main.ts", "./util.ts"}:                {file, "web/src/util.ts"}, // a second specifier for one file keeps its own entry
		{"web/src/main.ts", "./types"}:                  {file, "web/src/types.ts"},
		{"web/src/main.ts", "react"}:                    {declared, "web/package.json"},
		{"web/src/main.ts", "react/jsx-runtime"}:        {declared, "web/package.json"}, // subpath
		{"web/src/main.ts", "@scope/pkg/sub"}:           {declared, "web/package.json"}, // scoped subpath
		{"web/src/main.ts", "peer"}:                     {declared, "web/package.json"},
		{"web/src/main.ts", "opt"}:                      {declared, "web/package.json"},
		{"web/src/main.ts", "lodash"}:                   {declared, "web/package.json"},    // through @types/lodash
		{"web/src/main.ts", "@acme/shared"}:             {declared, "shared/package.json"}, // workspace package name
		{"web/src/main.ts", "node:fs"}:                  {builtin, ""},
		{"web/src/main.ts", "path"}:                     {builtin, ""},
		{"web/src/main.ts", "fs/promises"}:              {builtin, ""},
		{"web/src/main.ts", "bun:test"}:                 {builtin, ""},
		{"web/src/main.ts", "bun"}:                      {builtin, ""},
		{"web/src/main.ts", "left-pad"}:                 {unresolved, ""}, // undeclared bare specifier at this commit
		{"web/src/main.ts", "./gone"}:                   {unresolved, ""}, // unresolved relative import
		{"web/src/main.ts", "@app/missing"}:             {unresolved, ""}, // root alias with no target file
		{"web/src/main.ts", "lib/thing"}:                {unresolved, ""}, // nested-tsconfig alias the engine cannot follow
		{"web/src/legacy.js", "react"}:                  {declared, "web/package.json"},
		{"web/src/legacy.js", "undeclared-js"}:          {unresolved, ""},
		{"gosvc/main.go", "fmt"}:                        {builtin, ""},
		{"gosvc/main.go", "net/http"}:                   {builtin, ""},
		{"gosvc/main.go", "example.com/svc/gosvc/util"}: {file, "gosvc/util/util.go"},
		// IMPORTS resolves packages to files only for the root go.mod's module
		// today; a nested module's own package is declared by its go.mod.
		{"nested/main.go", "example.com/nested/a"}:           {declared, "nested/go.mod"},
		{"gosvc/main.go", "example.com/svc/internal/absent"}: {declared, "go.mod"}, // the module's own path
		{"gosvc/main.go", "github.com/dep/one/sub"}:          {declared, "go.mod"}, // required module, block form, subpackage
		{"gosvc/main.go", "golang.org/x/text"}:               {declared, "go.mod"}, // single-line require
		{"gosvc/main.go", "github.com/undeclared/x"}:         {unresolved, ""},
		{"py/pkg/app.py", "os"}:                              {builtin, ""},
		{"py/pkg/app.py", "requests"}:                        {declared, "py/pyproject.toml"},
		{"py/pkg/app.py", "ujson"}:                           {declared, "py/pyproject.toml"}, // optional dependency
		{"py/pkg/app.py", "pytest"}:                          {declared, "py/requirements-dev.txt"},
		{"py/pkg/app.py", "yaml"}:                            {notClassified, ""}, // PyYAML is not the identity mapping
		{"py/pkg/app.py", "numpy"}:                           {notClassified, ""}, // undeclared, but a distribution may provide it
		{"py/pkg/app.py", ".helper"}:                         {file, "py/pkg/helper.py"},
		{"py/pkg/app.py", ".gone"}:                           {unresolved, ""},
		{"rust/lib.rs", "std::fmt"}:                          {notClassified, ""}, // a language outside coverage
	}
	if first == rev(t, repo, "HEAD") {
		t.Fatal("fixture must read a commit that is not HEAD")
	}
	for _, profile := range []string{"full", "fast"} {
		t.Run(profile, func(t *testing.T) {
			output := runOutlineCLI(t, "snapshot", "--repo", repo, "--commit", first, "--format", "ndjson", "--profile", profile, "--no-cache")
			got, _ := importTargets(t, output)
			for key, expected := range want {
				if got[key] != expected {
					t.Errorf("%s %q: got %v, want %v", key[0], key[1], got[key], expected)
				}
			}
			for key := range got {
				if _, ok := want[key]; !ok && strings.HasPrefix(key[0], "web/src/main.ts") {
					t.Errorf("unexpected classified specifier %v", key)
				}
			}
		})
	}
	// The same entries ride on edges, so a consumer of either stream reads them.
	edges := runOutlineCLI(t, "edges", "--repo", repo, "--commit", first, "--format", "ndjson", "--relation", "IMPORTS", "--no-cache")
	if got, _ := importTargets(t, edges); got[[2]string{"web/src/main.ts", "react"}] != want[[2]string{"web/src/main.ts", "react"}] {
		t.Errorf("edges stream lost the classification: %v", got[[2]string{"web/src/main.ts", "react"}])
	}
}

// The classification adds evidence entries only: no relation is added,
// removed, renamed or otherwise changed, and every other record is identical.
func TestSnapshotImportTargetsOnlyAddEvidence(t *testing.T) {
	repo, first := importTargetFixture(t)
	for _, profile := range []sem.Profile{sem.ProfileFull, sem.ProfileFast, sem.ProfileSyntaxOnly} {
		capture := func(classify bool) []any {
			var records []any
			options := sem.ProviderSnapshotOptions{Profile: profile, ClassifyImports: classify}
			options = options.WithExactCommit(first)
			if err := sem.StreamSnapshot(t.Context(), repo, "v", options, func(record any) error {
				records = append(records, record)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			return records
		}
		plain, classified := capture(false), capture(true)
		if len(plain) != len(classified) {
			t.Fatalf("%s: %d records without classification, %d with", profile, len(plain), len(classified))
		}
		added := 0
		for i := range plain {
			before, isRelation := plain[i].(sem.RelationRecord)
			after, _ := classified[i].(sem.RelationRecord)
			if !isRelation || before.Type != "IMPORTS" {
				if !reflect.DeepEqual(plain[i], classified[i]) {
					t.Fatalf("%s: record %d changed: %#v", profile, i, classified[i])
				}
				continue
			}
			extra := after.Evidence[len(before.Evidence):]
			after.Evidence = after.Evidence[:len(before.Evidence)]
			if len(after.Evidence) == 0 {
				after.Evidence = nil
			}
			if !reflect.DeepEqual(before, after) || len(extra) == 0 || !slices.ContainsFunc(extra, func(e sem.Evidence) bool { return strings.HasPrefix(e.Kind, "import_target_") }) {
				t.Fatalf("%s: IMPORTS relation changed beyond its classification: %#v", profile, classified[i])
			}
			added++
		}
		if profile != sem.ProfileSyntaxOnly && added == 0 {
			t.Fatalf("%s: no IMPORTS relation classified", profile)
		}
	}
}

func TestImportTargetCapability(t *testing.T) {
	var capabilities sem.CapabilityReport
	if err := json.Unmarshal(runOutlineCLI(t, "capabilities", "--json"), &capabilities); err != nil {
		t.Fatal(err)
	}
	if !capabilities.OptionalLocalOnlyFeatures["import_target_classification"] {
		t.Error("capabilities omit import_target_classification")
	}
	if got := capabilities.FeatureLanguages["import_target_classification"]; !reflect.DeepEqual(got, []string{"Go", "JavaScript", "Python", "TypeScript"}) {
		t.Errorf("import target languages %v", got)
	}
}

// Every specifier as written in a Python import statement has an entry, so a
// consumer joining the tokens record's statement specifier with the snapshot
// finds it. `from . import helper` is scanned as ".helper"; the module "."
// gets an entry of its own. `from os import getcwd` is scanned as "os" and
// "os.getcwd"; "os" is builtin, "os.getcwd" names no module and is
// unresolved, and `from os import path` gives "os.path", a module, builtin.
func TestPythonFromImportModuleHasItsOwnEntry(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Graph test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "pkg/__init__.py", "")
	write(t, repo, "pkg/helper.py", "def h():\n    return 1\n")
	write(t, repo, "pkg/app.py", "from . import helper\nfrom os import getcwd\nfrom os import path\nimport os.pathh\n")
	write(t, repo, "solo/app.py", "from . import gone\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "fixture")
	commit := rev(t, repo, "HEAD")
	records, _ := tokensStream(t, repo)
	statements := map[string]bool{}
	for _, statement := range records["pkg/app.py"].Imports {
		statements[statement.Specifier] = true
	}
	if !reflect.DeepEqual(statements, map[string]bool{".": true, "os": true, "os.pathh": true}) {
		t.Fatalf("tokens record statements %v", statements)
	}
	targets, _ := importTargets(t, runOutlineCLI(t, "snapshot", "--repo", repo, "--commit", commit, "--format", "ndjson", "--no-cache"))
	want := map[[2]string]importTarget{
		{"pkg/app.py", "."}:         {"import_target_file", "pkg/__init__.py"},
		{"pkg/app.py", ".helper"}:   {"import_target_file", "pkg/helper.py"},
		{"pkg/app.py", "os"}:        {"import_target_builtin", ""},
		{"pkg/app.py", "os.getcwd"}: {"import_target_unresolved", ""},
		{"pkg/app.py", "os.path"}:   {"import_target_builtin", ""},
		{"pkg/app.py", "os.pathh"}:  {"import_target_unresolved", ""}, // a mistyped standard submodule
		{"solo/app.py", "."}:        {"import_target_unresolved", ""}, // no __init__.py
		{"solo/app.py", ".gone"}:    {"import_target_unresolved", ""},
	}
	for key, expected := range want {
		if targets[key] != expected {
			t.Errorf("%s %q: %v, want %v", key[0], key[1], targets[key], expected)
		}
	}
	for specifier := range statements {
		if _, ok := targets[[2]string{"pkg/app.py", specifier}]; !ok {
			t.Errorf("statement specifier %q has no entry", specifier)
		}
	}
}

// "." and ".." name a directory as "./" and "../" do, so all four resolve to
// that directory's index file, and without one are unresolved relative imports.
func TestJSBareDotSpecifiersResolveToTheDirectoryIndex(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Graph test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "src/index.ts", "export default 1;\n")
	write(t, repo, "src/sub/a.ts", "import up from \"..\";\nimport upSlash from \"../\";\n")
	write(t, repo, "src/b.js", "import here from \".\";\nimport hereSlash from \"./\";\n")
	write(t, repo, "lib/c.ts", "import none from \".\";\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "fixture")
	output := runOutlineCLI(t, "snapshot", "--repo", repo, "--commit", rev(t, repo, "HEAD"), "--format", "ndjson", "--no-cache")
	targets, _ := importTargets(t, output)
	for _, key := range [][2]string{{"src/sub/a.ts", ".."}, {"src/sub/a.ts", "../"}, {"src/b.js", "."}, {"src/b.js", "./"}} {
		if got := targets[key]; got.kind != "import_target_file" || got.path != "src/index.ts" {
			t.Errorf("%s %q: %v, want the directory's index.ts", key[0], key[1], got)
		}
	}
	if got := targets[[2]string{"lib/c.ts", "."}]; got.kind != "import_target_unresolved" {
		t.Errorf("lib/c.ts \".\" with no index: %v", got)
	}
	if !bytes.Contains(output, []byte(`"UNRESOLVED_RELATIVE_IMPORT"`)) {
		t.Error("an unresolved bare-dot import lost its UNRESOLVED_RELATIVE_IMPORT warning")
	}
}

// The consumer joins a tokens-record statement's specifier with the
// snapshot's import-target entries. These forms, which the relation side's
// import scanner reads, have an entry under the specifier the tokens record
// gives.
func TestStatementSpecifiersWithAnEntry(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Graph test")
	git(t, repo, "config", "user.email", "graph@example.com")
	files := map[string]string{
		"pkg/__init__.py": "",
		"pkg/m.py":        "x = 1\n",
		"pkg/app.py":      "import os\nimport os.path\nfrom . import m\nfrom os import path\n",
		"web/a.ts":        "import first from \"./first\";\nimport { a, b } from \"node:fs\";\n",
		"web/b.js":        "import x from './x.js';\n",
		"go.mod":          "module example.com/m\n\ngo 1.22\n",
		"svc/a.go":        "package svc\n\nimport \"fmt\"\nimport str \"strings\"\n",
	}
	for path, content := range files {
		write(t, repo, path, content)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "fixture")
	targets, _ := importTargets(t, runOutlineCLI(t, "snapshot", "--repo", repo, "--commit", rev(t, repo, "HEAD"), "--format", "ndjson", "--no-cache"))
	records, _ := tokensStream(t, repo)
	for _, path := range []string{"pkg/app.py", "web/a.ts", "web/b.js", "svc/a.go"} {
		if len(records[path].Imports) == 0 {
			t.Fatalf("%s: no import statements", path)
		}
		for _, statement := range records[path].Imports {
			if _, ok := targets[[2]string{path, statement.Specifier}]; !ok {
				t.Errorf("%s: statement specifier %q has no import-target entry", path, statement.Specifier)
			}
		}
	}
	if got := targets[[2]string{"pkg/app.py", "os.path"}]; got.kind != "import_target_builtin" {
		t.Errorf("os.path: %v", got)
	}
	if got := targets[[2]string{"pkg/app.py", "."}]; got.kind != "import_target_file" || got.path != "pkg/__init__.py" {
		t.Errorf("from . import m, the module: %v", got)
	}
	if got := targets[[2]string{"pkg/app.py", ".m"}]; got.kind != "import_target_file" || got.path != "pkg/m.py" {
		t.Errorf(".m: %v", got)
	}
}

// treeImportFixture holds each form the import scanner misses, per language,
// beside forms it reads, a file the tree cannot parse and a language outside
// the four.
func treeImportFixture(t *testing.T) (repo, commit string) {
	t.Helper()
	repo = t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Graph test")
	git(t, repo, "config", "user.email", "graph@example.com")
	files := map[string]string{
		"go.mod":                   "module example.com/m\n\ngo 1.22\n",
		"web/a.ts":                 "export const first = 1;\n",
		"web/b.ts":                 "export const b = 1;\n",
		"web/types.ts":             "export type T = number;\n",
		"web/z.ts":                 "export default 1;\n",
		"web/x.js":                 "export const x = 1;\n",
		"web/multi.ts":             "import first from \"./a\";\nimport {\n  b,\n} from \"./b\";\nimport type {\n  T,\n} from \"./types\";\nimport z from \"./z\";\nexport const all = [first, b, z];\n",
		"web/multi.js":             "import {\n  x,\n} from './x.js';\nexport default x;\n",
		"web/bad.ts":               "import one from \"./a\";\nimport {\n  two,\n} from \"./b\";\nexport const = ;\n",
		"svc/comment.go":           "package svc\n\nimport (\n\t/* note */ \"fmt\"\n\t\"strings\" // tail\n)\n\nfunc C() { fmt.Println(strings.ToUpper(\"\")) }\n",
		"svc/raw.go":               "package svc\n\nimport `os`\n\nfunc R() []string { return os.Args }\n",
		"svc/group.go":             "package svc\n\nimport (\n\tstr \"strings\"\n\t_ \"embed\"\n\t. \"math\"\n)\n\nfunc G() string { _ = Pi; return str.ToLower(\"\") }\n",
		"py/multi.py":              "import os, json\n",
		"py/spaced.py":             "import os . path\n",
		"py/pkg/__init__.py":       "",
		"py/pkg/x.py":              "X = 1\n",
		"py/pkg/rel.py":            "from . import x\n",
		"py/pkg/star.py":           "from . import *\n",
		"py/top/__init__.py":       "",
		"py/top/pkg.py":            "y = 1\n",
		"py/top/inner/__init__.py": "",
		"py/top/inner/deep.py":     "from .. pkg import y\n",
		"rust/lib.rs":              "use std::fmt;\nfn main() {}\n",
		// Statements only the tree reads: the scanner skips a `from` after
		// `if True:`, so `.` gets its entry from the tree's statement.
		"py/pkg/inline.py": "if True: from . import x\n",
		// Scripts routed to their language by a shebang, with no extension.
		"bin/__init__.py": "",
		"bin/helper.py":   "H = 1\n",
		"bin/tool":        "#!/usr/bin/env python3\nfrom . import helper\nimport os\n",
		"bin/lib.js":      "export const lib = 1;\n",
		"bin/run":         "#!/usr/bin/env node\nimport {\n  lib,\n} from \"./lib.js\";\nimport fs from \"node:fs\";\n",
		// An empty specifier names nothing: Go lists the statement, TypeScript
		// and JavaScript do not.
		"svc/empty.go": "package svc\n\nimport \"\"\nimport \"fmt\"\n\nvar _ = fmt.Sprint\n",
		"web/empty.ts": "import \"\";\nimport e from \"\";\nimport r = require(\"\");\nimport ok from \"./a\";\n",
	}
	for path, content := range files {
		write(t, repo, path, content)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "fixture")
	return repo, rev(t, repo, "HEAD")
}

// statedException reports whether a statement may lack an entry under
// import_targets_cover_statements: only one whose specifier is empty.
func statedException(statement sem.ImportStatementRecord) bool {
	return statement.Specifier == ""
}

// tokensAt reads the tokens records of one exact commit.
func tokensAt(t *testing.T, repo, commit string) map[string]sem.FileTokensRecord {
	t.Helper()
	records := map[string]sem.FileTokensRecord{}
	for _, line := range bytes.Split(bytes.TrimSpace(runOutlineCLI(t, "symbols", "--repo", repo, "--commit", commit, "--format", "ndjson", "--outline", "--tokens")), []byte("\n")) {
		var record sem.FileTokensRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.RecordType == "file_tokens" {
			records[record.FilePath] = record
		}
	}
	return records
}

// Each form the scanner misses now has its relation and its entry, with the
// class its tree gives. Reason: these are the forms the consumer's rule was
// blocked on (docs/snapshot-format.md listed them as the limit).
func TestTreeImportFormsHaveRelationsAndEntries(t *testing.T) {
	repo, commit := treeImportFixture(t)
	const file, builtin = "import_target_file", "import_target_builtin"
	want := map[[2]string]importTarget{
		{"web/multi.ts", "./a"}:           {file, "web/a.ts"},
		{"web/multi.ts", "./b"}:           {file, "web/b.ts"},     // multi-line TypeScript import
		{"web/multi.ts", "./types"}:       {file, "web/types.ts"}, // import type across lines
		{"web/multi.ts", "./z"}:           {file, "web/z.ts"},
		{"web/multi.js", "./x.js"}:        {file, "web/x.js"}, // multi-line JavaScript import
		{"svc/comment.go", "fmt"}:         {builtin, ""},      // a spec beside a leading comment
		{"svc/comment.go", "strings"}:     {builtin, ""},
		{"svc/raw.go", "os"}:              {builtin, ""}, // a raw-string path
		{"svc/group.go", "strings"}:       {builtin, ""}, // aliased in a group
		{"svc/group.go", "embed"}:         {builtin, ""}, // blank identifier
		{"svc/group.go", "math"}:          {builtin, ""}, // dot import
		{"py/multi.py", "os"}:             {builtin, ""},
		{"py/multi.py", "json"}:           {builtin, ""}, // the second module of `import a, b`
		{"py/spaced.py", "os.path"}:       {builtin, ""}, // `import os . path`
		{"py/pkg/rel.py", ".x"}:           {file, "py/pkg/x.py"},
		{"py/pkg/rel.py", "."}:            {file, "py/pkg/__init__.py"},
		{"py/pkg/star.py", "."}:           {file, "py/pkg/__init__.py"}, // no name to ride on
		{"py/top/inner/deep.py", "..pkg"}: {file, "py/top/pkg.py"},      // `from .. pkg import y`
		{"rust/lib.rs", "std::fmt"}:       {"import_target_not_classified", ""},
		{"web/bad.ts", "./a"}:             {file, "web/a.ts"},
		{"py/pkg/inline.py", ".x"}:        {file, "py/pkg/x.py"},
		{"py/pkg/inline.py", "."}:         {file, "py/pkg/__init__.py"}, // a statement only the tree reads
		// Shebang scripts resolve and classify by their language.
		{"bin/tool", ".helper"}: {file, "bin/helper.py"},
		{"bin/tool", "."}:       {file, "bin/__init__.py"},
		{"bin/tool", "os"}:      {builtin, ""},
		{"bin/run", "./lib.js"}: {file, "bin/lib.js"},
		{"bin/run", "node:fs"}:  {builtin, ""},
		{"svc/empty.go", "fmt"}: {builtin, ""},
		{"web/empty.ts", "./a"}: {file, "web/a.ts"},
	}
	for _, profile := range []string{"full", "fast"} {
		targets, _ := importTargets(t, runOutlineCLI(t, "snapshot", "--repo", repo, "--commit", commit, "--format", "ndjson", "--profile", profile, "--no-cache"))
		for key, expected := range want {
			if got, ok := targets[key]; !ok || got != expected {
				t.Errorf("%s: %s %q: got %v (present %v), want %v", profile, key[0], key[1], got, ok, expected)
			}
		}
		// A file whose tree has an error keeps the scanner's answer alone.
		if got, ok := targets[[2]string{"web/bad.ts", "./b"}]; ok {
			t.Errorf("%s: a file with a parse error got a tree import: %v", profile, got)
		}
	}
	records := tokensAt(t, repo, commit)
	if _, ok := records["web/bad.ts"]; ok {
		t.Error("fixture: web/bad.ts must have no tokens record")
	}
	for _, path := range []string{"bin/tool", "bin/run", "py/pkg/inline.py"} {
		if records[path].ImportBindings != "extracted" || len(records[path].Imports) == 0 {
			t.Errorf("fixture: %s must have an extracted tokens record with statements: %+v", path, records[path])
		}
	}
}

// The one stated exception to import_targets_cover_statements: a statement
// whose specifier is empty names nothing, so no relation carries an entry for
// it. Go lists such a statement; the TypeScript and JavaScript records list
// none. Reason: the promise names this exception, so it is pinned exactly.
func TestEmptySpecifierIsTheStatedException(t *testing.T) {
	repo, commit := treeImportFixture(t)
	records := tokensAt(t, repo, commit)
	goRecord, tsRecord := records["svc/empty.go"], records["web/empty.ts"]
	if goRecord.ImportBindings != "extracted" || tsRecord.ImportBindings != "extracted" {
		t.Fatalf("fixture: records %+v %+v", goRecord, tsRecord)
	}
	var goSpecifiers, tsSpecifiers []string
	for _, statement := range goRecord.Imports {
		goSpecifiers = append(goSpecifiers, statement.Specifier)
	}
	for _, statement := range tsRecord.Imports {
		tsSpecifiers = append(tsSpecifiers, statement.Specifier)
	}
	if !reflect.DeepEqual(goSpecifiers, []string{"", "fmt"}) || !reflect.DeepEqual(tsSpecifiers, []string{"./a"}) {
		t.Fatalf("statements: Go %q, TypeScript %q", goSpecifiers, tsSpecifiers)
	}
	targets, _ := importTargets(t, runOutlineCLI(t, "snapshot", "--repo", repo, "--commit", commit, "--format", "ndjson", "--no-cache"))
	for _, path := range []string{"svc/empty.go", "web/empty.ts"} {
		if got, ok := targets[[2]string{path, ""}]; ok {
			t.Errorf("%s: the empty specifier has an entry %v", path, got)
		}
	}
	if _, ok := targets[[2]string{"svc/empty.go", "fmt"}]; !ok {
		t.Error("svc/empty.go: the other statement lost its entry")
	}
}

// The promise of import_targets_cover_statements, as a property: for every
// file whose tokens record says its imports were extracted, every statement's
// specifier has an import-target entry on an IMPORTS relation of that file in
// the snapshot of the same commit. Reason: this is what a consumer may now rely
// on; it replaces the test that pinned the gap.
func TestEveryExtractedImportStatementHasAnEntry(t *testing.T) {
	treeRepo, treeCommit := treeImportFixture(t)
	targetRepo, targetCommit := importTargetFixture(t)
	for _, fixture := range []struct {
		repo, commit string
		floor        int
	}{{treeRepo, treeCommit, 15}, {targetRepo, targetCommit, 5}} {
		records := tokensAt(t, fixture.repo, fixture.commit)
		extracted := 0
		for _, record := range records {
			if record.ImportBindings == "extracted" && len(record.Imports) > 0 {
				extracted++
			}
		}
		if extracted < fixture.floor {
			t.Fatalf("only %d extracted tokens records with statements: the property was not exercised", extracted)
		}
		for _, profile := range []string{"full", "fast"} {
			targets, _ := importTargets(t, runOutlineCLI(t, "snapshot", "--repo", fixture.repo, "--commit", fixture.commit, "--format", "ndjson", "--profile", profile, "--no-cache"))
			for path, record := range records {
				if record.ImportBindings != "extracted" {
					continue
				}
				for _, statement := range record.Imports {
					if _, ok := targets[[2]string{path, statement.Specifier}]; !ok && !statedException(statement) {
						t.Errorf("%s: %s line %d: statement specifier %q has no import-target entry", profile, path, statement.StartLine, statement.Specifier)
					}
				}
			}
		}
	}
}

// The relations the scanner gives come first, unchanged and in its order; the
// tree's additions follow in statement order; a statement the scanner already
// read adds nothing, so no relation is duplicated and each specifier has one
// entry; two runs give the same bytes. Reason: old IMPORTS must stay exactly
// as they were for every existing reader.
func TestTreeImportsFollowTheScannersOnceEach(t *testing.T) {
	repo, commit := treeImportFixture(t)
	args := []string{"snapshot", "--repo", repo, "--commit", commit, "--format", "ndjson", "--no-cache"}
	first, second := runOutlineCLI(t, args...), runOutlineCLI(t, args...)
	if !bytes.Equal(first, second) {
		t.Fatal("two runs gave different bytes")
	}
	var order []string
	pairs := map[[2]string]int{}
	entries := map[[2]string]int{}
	reasons := map[string]string{}
	for _, line := range bytes.Split(bytes.TrimSpace(first), []byte("\n")) {
		var record sem.RelationRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.RecordType != "relation" || record.Type != "IMPORTS" {
			continue
		}
		from := record.FromID[strings.LastIndex(record.FromID, ":file:")+len(":file:"):]
		pairs[[2]string{from, record.ToID}]++
		if from == "py/multi.py" {
			reasons[record.ToID] = record.Reason
		}
		for _, evidence := range record.Evidence {
			if strings.HasPrefix(evidence.Kind, "import_target_") {
				entries[[2]string{from, evidence.Detail}]++
			}
		}
		if from == "web/multi.ts" {
			order = append(order, record.ToID[strings.LastIndex(record.ToID, ":file:")+len(":file:"):])
		}
	}
	// The scanner reads ./a and ./z (its list is sorted); ./b and ./types are
	// the tree's, in statement order.
	if want := []string{"web/a.ts", "web/z.ts", "web/b.ts", "web/types.ts"}; !reflect.DeepEqual(order, want) {
		t.Errorf("web/multi.ts IMPORTS order %v, want %v", order, want)
	}
	if len(pairs) == 0 {
		t.Fatal("no IMPORTS relations")
	}
	for pair, count := range pairs {
		if count != 1 {
			t.Errorf("%v: %d IMPORTS relations", pair, count)
		}
	}
	for key, count := range entries {
		if count != 1 {
			t.Errorf("%s %q: %d entries", key[0], key[1], count)
		}
	}
	// svc/comment.go's "strings" and svc/group.go's aliased and blank specs are
	// read by the scanner as well as the tree: one relation, one entry.
	for _, key := range [][2]string{{"svc/comment.go", "strings"}, {"svc/group.go", "strings"}, {"svc/group.go", "embed"}, {"py/multi.py", "os"}} {
		if entries[key] != 1 {
			t.Errorf("%s %q: %d entries, want 1", key[0], key[1], entries[key])
		}
	}
	// The scanner's relation keeps its reason; the one only the tree read
	// says so.
	if got := reasons["external:import:os"]; got != "import declaration matched by language-specific scanner" {
		t.Errorf("py/multi.py os (scanner): reason %q", got)
	}
	if got := reasons["external:import:json"]; got != "import statement read from the syntax tree" {
		t.Errorf("py/multi.py json (tree): reason %q", got)
	}
}

func TestImportStatementCoverageCapability(t *testing.T) {
	var capabilities sem.CapabilityReport
	if err := json.Unmarshal(runOutlineCLI(t, "capabilities", "--json"), &capabilities); err != nil {
		t.Fatal(err)
	}
	if !capabilities.OptionalLocalOnlyFeatures["import_targets_cover_statements"] {
		t.Error("capabilities omit import_targets_cover_statements")
	}
	if got := capabilities.FeatureLanguages["import_targets_cover_statements"]; !reflect.DeepEqual(got, []string{"Go", "JavaScript", "Python", "TypeScript"}) {
		t.Errorf("import_targets_cover_statements languages %v", got)
	}
}
