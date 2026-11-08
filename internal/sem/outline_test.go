package sem

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Rewritten trees can contain synthetic identifiers at otherwise valid source
// offsets. Only unchanged, authored leaves may become identifier names.
func TestFileOutlineMaskedIdentifiers(t *testing.T) {
	for _, fixture := range []struct {
		path, source string
		want         [][]any
	}{
		{"a.ts", "type Item = import(\"./secret-module\").Thing;\nconst after = value;\n", [][]any{{1, "Item", "Thing"}, {2, "after", "value"}}},
		{"a.swift", "public var label: String {\n  \"hello\"\n}\nlet after = value\n", [][]any{{4, "after", "value"}}},
		{"a.cpp", "FMT_BEGIN_NAMESPACE\nint run(int value) { return value; }\nFMT_END_NAMESPACE\nint after;\n", [][]any{{2, "run", "value"}, {4, "after"}}},
	} {
		for _, profile := range []Profile{ProfileSyntaxOnly, ProfileFast, ProfileFull} {
			t.Run(fixture.path+"/"+string(profile), func(t *testing.T) {
				spec := resolveProfile(profile)
				spec.outline = true
				lang, _ := languageForContent(fixture.path, fixture.source)
				_, _, status, outline := parseWithOutline(spec, lang, fixture.path, fixture.source)
				if status.ParseError || status.Partial || outline == nil {
					t.Fatalf("outline missing: %#v", status)
				}
				if !reflect.DeepEqual(outline.Identifiers, fixture.want) {
					t.Fatalf("identifiers = %#v, want %#v", outline.Identifiers, fixture.want)
				}
				last := outline.TopLevel[len(outline.TopLevel)-1]
				wantLine := fixture.want[len(fixture.want)-1][0].(int)
				if last.StartLine != wantLine || last.EndLine != wantLine {
					t.Fatalf("mask moved trailing authored statement: %#v", last)
				}
			})
		}
	}
}

func TestFileOutlineElixirBinaryAndLiterals(t *testing.T) {
	source := "plain = value\nencoded = <<value::binary>>\n<<first, rest::binary>> = payload\ntext = \"hidden #{hiddenInterpolation}\"\nchars = 'hidden #{hiddenChars}'\n"
	want := [][]any{{1, "plain", "value"}, {2, "encoded", "value", "binary"}, {3, "first", "rest", "binary", "payload"}, {4, "text"}, {5, "chars"}}
	for _, profile := range []Profile{ProfileSyntaxOnly, ProfileFast, ProfileFull} {
		t.Run(string(profile), func(t *testing.T) {
			spec := resolveProfile(profile)
			spec.outline = true
			_, _, status, outline := parseWithOutline(spec, treeSitterLanguages[".ex"], "a.ex", source)
			if status.ParseError || status.Partial || outline == nil {
				t.Fatalf("outline missing: %#v", status)
			}
			if !reflect.DeepEqual(outline.Identifiers, want) {
				t.Fatalf("identifiers = %#v, want %#v", outline.Identifiers, want)
			}
		})
	}
}

// The Kotlin generic-call mask can relocate a newline inside its replacement.
// Suppress rewritten root children in that case and keep authored coordinates
// for unchanged leaves and statements, without a second parse.
func TestFileOutlineMaskMovesNewline(t *testing.T) {
	source := "val result = buildList<String>\n{\n value\n}\nval after = value\n"
	spec := resolveProfile(ProfileSyntaxOnly)
	spec.outline = true
	_, _, status, outline := parseWithOutline(spec, treeSitterLanguages[".kt"], "a.kt", source)
	if status.ParseError || status.Partial || outline == nil {
		t.Fatalf("outline missing: %#v", status)
	}
	want := []OutlineNode{{StartLine: 5, EndLine: 5, Type: "property_declaration"}}
	if !reflect.DeepEqual(outline.TopLevel, want) {
		t.Fatalf("top_level = %#v, want %#v", outline.TopLevel, want)
	}
	wantIDs := [][]any{{1, "result", "buildList"}, {3, "value"}, {5, "after", "value"}}
	if !reflect.DeepEqual(outline.Identifiers, wantIDs) {
		t.Fatalf("identifiers = %#v, want %#v", outline.Identifiers, wantIDs)
	}
}

func TestFileOutlineImportsKeepFileContext(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "imports.ts", "const target = \"./target\";\nconst dep = require(target);\nconst ignored = require(\"   \");\n")
	for _, profile := range []Profile{ProfileSyntaxOnly, ProfileFast, ProfileFull} {
		t.Run(string(profile), func(t *testing.T) {
			var got *FileOutlineRecord
			var foundImport bool
			err := StreamSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Worktree: true, Profile: profile, Outline: true}, func(record any) error {
				switch r := record.(type) {
				case FileOutlineRecord:
					got = &r
				case RelationRecord:
					if r.Type == "IMPORTS" && r.ToID == "external:import:./target" {
						foundImport = true
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if profile == ProfileFull && !foundImport {
				t.Fatal("control: full-profile IMPORTS evidence missing")
			}
			want := []OutlineNode{{StartLine: 1, EndLine: 1, Type: "lexical_declaration"}, {StartLine: 2, EndLine: 2, Type: "lexical_declaration", Imports: true}, {StartLine: 3, EndLine: 3, Type: "lexical_declaration"}}
			if got == nil || !reflect.DeepEqual(got.TopLevel, want) {
				t.Fatalf("outline = %#v, want nodes %#v", got, want)
			}
		})
	}
}

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
	if len(outline.TopLevel) != 1 || !reflect.DeepEqual(outline.TopLevel[0], OutlineNode{StartLine: 1, EndLine: 3, Type: "message", Children: []OutlineNode{{StartLine: 1, EndLine: 1, Type: "message_name"}, {StartLine: 1, EndLine: 3, Type: "message_body"}}}) {
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
