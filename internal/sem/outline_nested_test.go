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
// Rust module functions cover the downstream responsibility-section inputs.
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

// Label extraction must not expose interpolation source, select a later string
// after a rejected template, include a later line, or split a Unicode character.
func TestFileOutlineLabels(t *testing.T) {
	for _, tc := range []struct {
		path, source string
		want         []string
	}{
		{"a.ts", "it(`secret ${hidden}`, run);\nit(`secret ${hidden}` + 'later', run);\nit(` plain `, run);\nit(' first ', 'second');\nit(\n 'later line', run);\n", []string{"", "", "plain", "first", ""}},
		{"a.py", "run(f'secret {hidden}')\nrun(r' plain ')\nrun(''' triple ''')\n", []string{"", "plain", "triple"}},
		{"a.lua", "run([=[ plain ]=])", []string{"plain"}},
		{"a.ml", "let label = {tag| plain |tag}\n", []string{"plain"}},
		{"a.rb", "run(\"secret #{hidden}\")\nrun(\" plain \")\n", []string{"", "plain"}},
		{"a.sh", "echo \"secret $hidden\"\necho \" plain \"\n", []string{"", "plain"}},
		{"a.go", "package sample\nvar text = ` plain `\nvar other = \" quoted \"\n", []string{"", "plain", "quoted"}},
		{"a.rs", "run(r##\" raw \"##);\nrun(\" quoted \");\n", []string{"raw", "quoted"}},
		{"a.ts", "run('  " + strings.Repeat("界", 81) + "  ');\n", []string{strings.Repeat("界", 80)}},
	} {
		t.Run(tc.path+tc.source[:3], func(t *testing.T) {
			got := nestedOutline(t, tc.path, tc.source, ProfileSyntaxOnly)
			labels := []string{}
			for _, n := range got.TopLevel {
				labels = append(labels, n.Label)
			}
			if !reflect.DeepEqual(labels, tc.want) {
				t.Fatalf("labels = %#v, want %#v", labels, tc.want)
			}
		})
	}
}

// Deep callbacks must stop at four emitted child edges even when source has
// further splittable descendants. A one-line node never emits children.
func TestFileOutlineNestedDepth(t *testing.T) {
	source := "prepare();\nverify();\n"
	for range 18 {
		source = "describe('suite', () => {\n" + source + "\n});\n"
	}
	got := nestedOutline(t, "a.ts", source, ProfileSyntaxOnly)
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
	if maxDepth != 4 {
		t.Fatalf("child depth = %d, want exactly 4", maxDepth)
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

// A selected body's enclosing literal must belong to this node, not be an
// ancestor outside it. Named declarations and standalone body nodes are not
// callbacks. A no-parameter Python lambda provides a provable positive case.
func TestFileOutlineCallback(t *testing.T) {
	got := nestedOutline(t, "a.ts", "const run = () => {\n a();\n b();\n};\nfunction named() {\n a();\n b();\n}\n", ProfileSyntaxOnly)
	var visit func([]OutlineNode)
	visit = func(nodes []OutlineNode) {
		for _, n := range nodes {
			if n.Callback {
				t.Fatalf("enclosing literal is outside node: %#v", n)
			}
			visit(n.Children)
		}
	}
	visit(got.TopLevel)
	// Use a multiline lambda so children can be emitted.
	got = nestedOutline(t, "a.py", "(\nlambda: first_long_identifier +\n second_long_identifier\n)\n", ProfileSyntaxOnly)
	if len(got.TopLevel) != 1 || !got.TopLevel[0].Callback {
		t.Fatalf("lambda callback missing: %#v", got.TopLevel)
	}
}
