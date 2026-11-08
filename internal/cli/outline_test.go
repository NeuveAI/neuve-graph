package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

// Exact records pin statement boundaries, source lines, identifier ordering,
// type names, deduplication, and exclusion of comment and literal contents.
func TestSymbolsOutlineRecords(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "a.ts", "import { Effect, Schema } from 'effect'\n// hiddenComment\nexport function run(value: Schema): Effect {\n  return Effect.succeed(value)\n}\ndescribe('hiddenString', () => { it('hiddenTest', run); });\nconst text = `hiddenTemplate ${hiddenInterpolation}`;\n")
	write(t, repo, "b.py", "import os\nfrom typing import List\n# hiddenComment\ndef run(value: List):\n    text = f'hiddenString {hiddenInterpolation}'\n    return os.path.join(value, value)\n")
	write(t, repo, "c.go", "package sample\nimport \"fmt\"\n// hiddenComment\ntype Item struct { Value string }\nfunc Run(value Item) {\n    fmt.Println(value, value, \"hiddenString\")\n}\n")
	write(t, repo, "d.c", "#include <stdio.h>\nint run(int value) { return value; }\n")
	write(t, repo, "empty.ts", "// hiddenComment\n42;\n")
	write(t, repo, "broken.ts", "export function broken( {\n")
	write(t, repo, "inventory.json", "{\"hiddenIdentifier\": true}\n")
	want := map[string]string{
		"a.ts":     `{"record_type":"file_outline","file_path":"a.ts","language":"TypeScript","top_level":[{"start_line":1,"end_line":1,"type":"import_statement","imports":true},{"start_line":3,"end_line":5,"type":"export_statement","imports":false},{"start_line":6,"end_line":6,"type":"expression_statement","imports":false},{"start_line":7,"end_line":7,"type":"lexical_declaration","imports":false}],"identifiers":[[1,"Effect","Schema"],[3,"run","value","Schema","Effect"],[4,"Effect","succeed","value"],[6,"describe","it","run"],[7,"text"]]}`,
		"b.py":     `{"record_type":"file_outline","file_path":"b.py","language":"Python","top_level":[{"start_line":1,"end_line":1,"type":"import_statement","imports":true},{"start_line":2,"end_line":2,"type":"import_from_statement","imports":true},{"start_line":4,"end_line":6,"type":"function_definition","imports":false}],"identifiers":[[1,"os"],[2,"typing","List"],[4,"run","value","List"],[5,"text"],[6,"os","path","join","value"]]}`,
		"c.go":     `{"record_type":"file_outline","file_path":"c.go","language":"Go","top_level":[{"start_line":1,"end_line":1,"type":"package_clause","imports":false},{"start_line":2,"end_line":2,"type":"import_declaration","imports":true},{"start_line":4,"end_line":4,"type":"type_declaration","imports":false},{"start_line":5,"end_line":7,"type":"function_declaration","imports":false}],"identifiers":[[1,"sample"],[4,"Item","Value","string"],[5,"Run","value","Item"],[6,"fmt","Println","value"]]}`,
		"d.c":      `{"record_type":"file_outline","file_path":"d.c","language":"C","top_level":[{"start_line":2,"end_line":2,"type":"function_definition","imports":false}],"identifiers":[[2,"run","value"]]}`,
		"empty.ts": `{"record_type":"file_outline","file_path":"empty.ts","language":"TypeScript","top_level":[{"start_line":2,"end_line":2,"type":"expression_statement","imports":false}],"identifiers":[]}`,
	}
	for _, profile := range []string{"syntax-only", "fast", "full"} {
		t.Run(profile, func(t *testing.T) {
			args := []string{"symbols", "--repo", repo, "--worktree", "--profile", profile, "--no-cache"}
			baseline := runOutlineCLI(t, args...)
			output := runOutlineCLI(t, append(args, "--outline")...)
			var legacy bytes.Buffer
			got := map[string]string{}
			var summary sem.SnapshotSummary
			for _, line := range bytes.Split(bytes.TrimSpace(output), []byte("\n")) {
				var record struct {
					RecordType string `json:"record_type"`
					FilePath   string `json:"file_path"`
				}
				if err := json.Unmarshal(line, &record); err != nil {
					t.Fatal(err)
				}
				if record.RecordType == "file_outline" {
					if _, duplicate := got[record.FilePath]; duplicate {
						t.Fatalf("duplicate outline for %s", record.FilePath)
					}
					var outline sem.FileOutlineRecord
					if err := json.Unmarshal(line, &outline); err != nil {
						t.Fatal(err)
					}
					for i := range outline.TopLevel {
						outline.TopLevel[i].Children = nil
						outline.TopLevel[i].Label = ""
						outline.TopLevel[i].Callback = false
					}
					legacyOutline, err := json.Marshal(outline)
					if err != nil {
						t.Fatal(err)
					}
					got[record.FilePath] = string(legacyOutline)
					continue
				}
				legacy.Write(line)
				legacy.WriteByte('\n')
				if record.RecordType == "summary" {
					if err := json.Unmarshal(line, &summary); err != nil {
						t.Fatal(err)
					}
				}
			}
			if !reflect.DeepEqual(got, want) {
				for path, expected := range want {
					if got[path] != expected {
						t.Errorf("%s outline = %s\nwant %s", path, got[path], expected)
					}
				}
				if len(got) != len(want) {
					t.Errorf("got %d outlines, want %d", len(got), len(want))
				}
			}
			if !bytes.Equal(legacy.Bytes(), baseline) {
				t.Error("adding outlines changed the existing stream bytes")
			}
			if len(summary.PartialFailures) != 1 || summary.PartialFailures[0].FilePath != "broken.ts" || summary.PartialFailures[0].Code != "E_PARSE_ERROR" {
				t.Errorf("parse failure changed: %#v", summary.PartialFailures)
			}
		})
	}
}

// A warm legacy record cache must never suppress outlines. Committed outlines
// describe HEAD even after working-tree edits; outline runs never change caches.
func TestSymbolsOutlineBypassesCommittedCache(t *testing.T) {
	repo, cache := t.TempDir(), t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Graph test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "a.ts", "export const committedName = 1;\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "fixture")
	for _, profile := range []string{"syntax-only", "fast", "full"} {
		args := []string{"symbols", "--repo", repo, "--profile", profile, "--cache-dir", cache}
		baseline := runOutlineCLI(t, args...)
		if cached := runOutlineCLI(t, args...); !bytes.Equal(cached, baseline) {
			t.Fatal("legacy cache replay changed bytes")
		}
		before := outlineCacheFiles(t, cache)
		if len(before) == 0 {
			t.Fatal("control failed to populate the committed cache")
		}
		write(t, repo, "a.ts", "export const worktreeName = 2;\n")
		output := runOutlineCLI(t, append(args, "--outline")...)
		if !bytes.Contains(output, []byte(`"record_type":"file_outline"`)) || !bytes.Contains(output, []byte(`"identifiers":[[1,"committedName"]]`)) || bytes.Contains(output, []byte("worktreeName")) {
			t.Fatalf("committed outline missing or stale: %s", output)
		}
		if after := outlineCacheFiles(t, cache); !reflect.DeepEqual(before, after) {
			t.Fatal("outline run wrote committed cache files")
		}
		worktree := runOutlineCLI(t, append(args, "--outline", "--worktree")...)
		if !bytes.Contains(worktree, []byte(`"identifiers":[[1,"worktreeName"]]`)) {
			t.Fatalf("working-tree outline missed edit: %s", worktree)
		}
	}
}

func outlineCacheFiles(t *testing.T, directory string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(path)
		files[path] = string(contents)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestSymbolsOutlineFlagContracts(t *testing.T) {
	repo := t.TempDir()
	for _, filter := range []string{"--to", "--from", "--relation"} {
		err := Run(t.Context(), Options{Stdout: io.Discard}, []string{"symbols", "--repo", repo, "--outline", filter, "target"})
		if err == nil || !strings.Contains(err.Error(), "filter relations") {
			t.Errorf("%s no longer refused: %v", filter, err)
		}
	}
	for _, command := range []string{"edges", "snapshot"} {
		err := Run(t.Context(), Options{Stdout: io.Discard}, []string{command, "--outline"})
		if err == nil || !strings.Contains(err.Error(), "only valid for symbols") {
			t.Errorf("%s accepted --outline: %v", command, err)
		}
	}
	output := runOutlineCLI(t, "capabilities", "--json")
	var capabilities sem.CapabilityReport
	if err := json.Unmarshal(output, &capabilities); err != nil {
		t.Fatal(err)
	}
	if !capabilities.OptionalLocalOnlyFeatures["symbols_outline"] {
		t.Fatal("capabilities omitted symbols_outline")
	}
}

func runOutlineCLI(t *testing.T, args ...string) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := Run(t.Context(), Options{Version: "outline-test", Stdout: &output, Stderr: io.Discard}, args); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestSymbolsOutlineUsesTerminalSafeJSON(t *testing.T) {
	repo := t.TempDir()
	path := "unsafe\u009b.ts"
	write(t, repo, path, "const café = \"unsafe\u009b\";\n")
	output := runOutlineCLI(t, "symbols", "--repo", repo, "--worktree", "--outline", "--profile", "syntax-only")
	if bytes.Contains(output, []byte("\u009b")) || !bytes.Contains(output, []byte(`unsafe\u009b.ts`)) {
		t.Fatalf("outline pathname bypassed terminal-safe encoding: %q", output)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte("\n")) {
		var record sem.FileOutlineRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.RecordType == "file_outline" {
			if record.FilePath != path || record.TopLevel[0].Label != "unsafe\u009b" || !reflect.DeepEqual(record.Identifiers, [][]any{{float64(1), "café"}}) {
				t.Fatalf("escaping changed outline data: %#v", record)
			}
			return
		}
	}
	t.Fatal("missing outline")
}
