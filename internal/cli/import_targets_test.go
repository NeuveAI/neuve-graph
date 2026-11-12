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
		if len(kinds) != 1 {
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

// A Python relative from-import is one statement in the tokens record, whose
// specifier is the module as written ("."), while the snapshot classifies each
// imported module (".helper"). A join on (file, specifier) therefore finds
// nothing for it: the safe direction, pinned here so it is never silent.
func TestPythonRelativeFromImportSpecifiersDiffer(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Graph test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "pkg/__init__.py", "")
	write(t, repo, "pkg/helper.py", "def h():\n    return 1\n")
	write(t, repo, "pkg/app.py", "from . import helper\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "fixture")
	commit := rev(t, repo, "HEAD")
	records, _ := tokensStream(t, repo)
	imports := records["pkg/app.py"].Imports
	if len(imports) != 1 || imports[0].Specifier != "." || imports[0].Bindings[0].Imported != "helper" {
		t.Fatalf("tokens record imports %#v", imports)
	}
	targets, _ := importTargets(t, runOutlineCLI(t, "snapshot", "--repo", repo, "--commit", commit, "--format", "ndjson", "--no-cache"))
	if got := targets[[2]string{"pkg/app.py", ".helper"}]; got.kind != "import_target_file" || got.path != "pkg/helper.py" {
		t.Errorf("relation entry for .helper: %v", got)
	}
	if _, ok := targets[[2]string{"pkg/app.py", "."}]; ok {
		t.Error("the relation now uses the statement's specifier; update the docs and this test")
	}
}
