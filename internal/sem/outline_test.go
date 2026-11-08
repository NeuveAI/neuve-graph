package sem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Reuse the real language fixtures to catch any profile bypassing tree-sitter,
// and report actual root-child shapes without inventing wrapper unwrapping.
func TestFileOutlineLanguageCoverage(t *testing.T) {
	for _, fixture := range languageMatrix {
		t.Run(fixture.dir, func(t *testing.T) {
			paths, err := filepath.Glob(filepath.Join("testdata", "langmatrix", fixture.dir, "*"))
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range paths {
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				langSpec, ok := languageForContent(path, string(content))
				if !ok {
					t.Fatalf("fixture is not supported: %s", path)
				}
				for _, profile := range []Profile{ProfileSyntaxOnly, ProfileFast, ProfileFull} {
					spec := resolveProfile(profile)
					spec.outline = true
					_, language, status, outline := parseWithOutline(spec, langSpec, path, string(content))
					if language == "Groovy" {
						if outline != nil {
							t.Fatal("non-tree-sitter parser emitted outline")
						}
						continue
					}
					if status.ParseError || status.Partial {
						t.Fatalf("%s fixture did not parse cleanly: %#v", profile, status)
					}
					if outline == nil || outline.RecordType != "file_outline" || outline.Language != language || outline.FilePath != path || len(outline.TopLevel) == 0 {
						t.Fatalf("%s omitted parsed-file outline: %#v", profile, outline)
					}
					if profile == ProfileSyntaxOnly {
						var types []string
						for _, node := range outline.TopLevel {
							types = append(types, node.Type)
						}
						t.Logf("%s root children: %s", language, strings.Join(types, ", "))
					}
				}
			}
		})
	}
}

// Proto2 parsing prepends synthetic syntax. Outlines must retain authored
// coordinates and omit that synthetic node, just as symbols do.
func TestFileOutlineProtocolBuffersSourceCoordinates(t *testing.T) {
	spec := resolveProfile(ProfileSyntaxOnly)
	spec.outline = true
	_, _, status, outline := parseWithOutline(spec, treeSitterLanguages[".proto"], "a.proto", "message Item {\n  optional string name = 1;\n}\n")
	if status.ParseError || outline == nil {
		t.Fatalf("proto2 outline missing: %#v", status)
	}
	if len(outline.TopLevel) != 1 || outline.TopLevel[0] != (OutlineNode{StartLine: 1, EndLine: 3, Type: "message"}) {
		t.Fatalf("synthetic syntax leaked: %#v", outline.TopLevel)
	}
	if len(outline.Identifiers) != 2 || outline.Identifiers[0][0] != 1 || outline.Identifiers[0][1] != "Item" || outline.Identifiers[1][0] != 2 || outline.Identifiers[1][1] != "name" {
		t.Fatalf("identifier coordinates changed: %#v", outline.Identifiers)
	}
}

// Outline collection must use the extraction tree, rather than quietly adding
// another tree-sitter parse for each file in the harness's syntax-only run.
func TestFileOutlineReusesExtractionTree(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "a.ts", "export function run(value: Thing) { return value; }\n")
	before := StructuralWorkSnapshot()
	var outlines int
	if err := StreamSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Worktree: true, Profile: ProfileSyntaxOnly, Outline: true}, func(record any) error {
		if _, ok := record.(FileOutlineRecord); ok {
			outlines++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := StructuralWorkSnapshot().Parses - before.Parses; got != 1 || outlines != 1 {
		t.Fatalf("got %d parses and %d outlines, want one of each", got, outlines)
	}
}
