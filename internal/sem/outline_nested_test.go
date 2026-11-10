package sem

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Exact trees pin the grammar-neutral byte-span selection rather than assuming
// that named children are always statements. The large TS class exercises the
// chunker's >4 KiB case; suite callbacks, Python methods, Go table callbacks and
// Rust module functions protect the retained child lists.
func TestFileOutlineNestedRecords(t *testing.T) {
	for _, name := range []string{"nested.ts", "nested.py", "nested.go", "nested.rs"} {
		source, err := os.ReadFile(filepath.Join("testdata", "outline", name+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		if name == "nested.ts" && len(source) < 4096 {
			t.Fatal("large class fixture shrank below 4 KiB")
		}
		expected, err := os.ReadFile(filepath.Join("testdata", "outline", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var want []OutlineNode
		if err := json.Unmarshal(expected, &want); err != nil {
			t.Fatal(err)
		}
		for _, profile := range []Profile{ProfileSyntaxOnly, ProfileFast, ProfileFull} {
			t.Run(name+"/"+string(profile), func(t *testing.T) {
				got := nestedOutline(t, name, string(source), profile)
				if !reflect.DeepEqual(got.TopLevel, want) {
					actual, _ := json.MarshalIndent(got.TopLevel, "", "  ")
					t.Fatalf("top_level = %s\nwant %s", actual, expected)
				}
			})
		}
	}
}

func nestedOutline(t *testing.T, path, source string, profile Profile) *FileOutlineRecord {
	t.Helper()
	spec := resolveProfile(profile)
	spec.outline = true
	lang, ok := languageForContent(path, source)
	if !ok {
		t.Fatal("unsupported fixture")
	}
	_, _, status, outline := parseWithOutline(spec, lang, path, source)
	if status.ParseError || status.Partial || outline == nil {
		t.Fatalf("missing outline: %#v", status)
	}
	return outline
}

// Pass-through wrappers must reach the statement list of an oversized Effect
// generator. Pin every emitted level so wrapper selection cannot change quietly.
func TestFileOutlineEffectGenerator(t *testing.T) {
	for _, tc := range []struct {
		name, tail string
		levels     [][]string
	}{
		{"plain", ");\n", [][]string{
			{"export_statement"},
			{"identifier", "call_expression"},
			{"member_expression", "arguments"},
			{"formal_parameters", "statement_block"},
		}},
		{"pipe", ").pipe(Effect.withSpan(\"x\"));\n", [][]string{
			{"export_statement"},
			{"identifier", "call_expression"},
			{"member_expression", "arguments"},
			{"call_expression", "property_identifier"},
			{"member_expression", "arguments"},
			{"formal_parameters", "statement_block"},
		}},
	} {
		source := "export const x = Effect.gen(function* () {\n" + strings.Repeat("  yield* Effect.succeed(value);\n", 160) + "}" + tc.tail
		if len(source) <= 4096 {
			t.Fatal("generator fixture must exceed 4 KiB")
		}
		for _, profile := range []Profile{ProfileSyntaxOnly, ProfileFast, ProfileFull} {
			t.Run(tc.name+"/"+string(profile), func(t *testing.T) {
				got := nestedOutline(t, "a.ts", source, profile)
				nodes := got.TopLevel
				for depth, types := range tc.levels {
					actual := []string{}
					var dominant OutlineNode
					for _, n := range nodes {
						actual = append(actual, n.Type)
						if n.EndLine-n.StartLine > dominant.EndLine-dominant.StartLine {
							dominant = n
						}
					}
					if !reflect.DeepEqual(actual, types) {
						t.Fatalf("depth %d types = %v, want %v", depth, actual, types)
					}
					nodes = dominant.Children
				}
				if len(nodes) != 160 {
					t.Fatalf("depth %d statement count = %d, want 160", len(tc.levels), len(nodes))
				}
				for i, node := range nodes {
					if node.Type != "expression_statement" || node.StartLine != i+2 || node.EndLine != i+2 || len(node.Children) != 0 {
						t.Fatalf("depth %d statement %d = %#v", len(tc.levels), i, node)
					}
				}
			})
		}
	}
}

// Large single-child wrapper chains must stop at sixteen emitted edges. A
// branching expression must still stop after four non-pass-through levels.
func TestFileOutlineNestedDepth(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         int
	}{
		{"pass-through", func() string {
			source := strings.Repeat("prepare();\nverify();\n", 300)
			for range 18 {
				source = "describe('suite', () => {\n" + source + "\n});\n"
			}
			return source
		}(), 16},
		{"branching", func() string {
			source := strings.Repeat("a", 256)
			for range 6 {
				source = "(\n" + source + " +\n" + source + "\n)"
			}
			return source + ";\n"
		}(), 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nestedOutline(t, "a.ts", tc.source, ProfileSyntaxOnly)
			maxDepth := 0
			var visit func([]OutlineNode, int)
			visit = func(nodes []OutlineNode, depth int) {
				for _, n := range nodes {
					if depth > maxDepth {
						maxDepth = depth
					}
					if len(n.Children) > 0 && n.StartLine == n.EndLine {
						t.Fatalf("one-line children: %#v", n)
					}
					visit(n.Children, depth+1)
				}
			}
			visit(got.TopLevel, 0)
			if maxDepth != tc.want {
				t.Fatalf("child depth = %d, want exactly %d", maxDepth, tc.want)
			}
		})
	}
}

// Twelve descendant edges are searchable; the thirteenth must not silently
// expand a deep expression. Byte coverage counts UTF-8 bytes, not characters.
func TestFileOutlineSelectionBounds(t *testing.T) {
	for _, tc := range []struct {
		wrappers int
		want     bool
	}{{11, true}, {12, false}} {
		source := strings.Repeat("(", tc.wrappers) + "\n" + strings.Repeat("界", 60) + " + " + strings.Repeat("b", 60) + "\n" + strings.Repeat(")", tc.wrappers) + ";\n"
		got := nestedOutline(t, "a.ts", source, ProfileSyntaxOnly)
		want := []OutlineNode(nil)
		if tc.want {
			want = []OutlineNode{{StartLine: 2, EndLine: 2, Type: "identifier"}, {StartLine: 2, EndLine: 2, Type: "identifier"}}
		}
		if !reflect.DeepEqual(got.TopLevel[0].Children, want) {
			t.Fatalf("%d wrappers: children = %#v, want %#v", tc.wrappers, got.TopLevel[0].Children, want)
		}
	}
	// The long comment is excluded from siblings but still occupies node bytes,
	// leaving the two short statements below half the original node's span.
	got := nestedOutline(t, "a.ts", "{\n/*"+strings.Repeat("padding", 80)+"*/\na();\nb();\n}\n", ProfileSyntaxOnly)
	if len(got.TopLevel[0].Children) != 0 {
		t.Fatalf("insufficient coverage accepted: %#v", got.TopLevel[0])
	}
}
