package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

func linesOf(content string) []string { return strings.Split(content, "\n") }

func staticLineReader(files map[string]string) lineReader {
	return func(relPath string) ([]string, bool) {
		content, ok := files[relPath]
		if !ok {
			return nil, false
		}
		return linesOf(content), true
	}
}

// rustDispatch is shaped like the real fix site that motivated this: a long
// dispatch function whose definition line is nowhere near the call, with the call
// buried under an `if let` / `match` / `else if` chain.
const rustDispatch = `pub fn expression(expr: &Expr, checker: &mut Checker) {
    match expr {
        Expr::Call(call) => {
            if let Expr::Attribute(ast::ExprAttribute { value, attr, .. }) = func.as_ref() {
                let attr = attr.as_str();
                if let Expr::StringLiteral(ast::ExprStringLiteral {
                    value: string_value,
                    ..
                }) = value.as_ref()
                {
                    if attr == "join" {
                        // "...".join(...) — not the site; also mentions target(
                        noop();
                    } else if attr == "format" {
                        match FormatSummary::try_from(string_value.to_str()) {
                            Err(e) => {
                                report(e);
                            }
                            Ok(summary) => {
                                if checker.enabled(Rule::Target) {
                                    rules::target(
                                        checker, call, &summary,
                                    );
                                }
                            }
                        }
                    }
                }
            }
        }
        _ => {}
    }
}`

func TestResolveCallSiteReportsCallLineNotContainerStart(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		path      string
		content   string
		token     string
		spanStart int
		spanEnd   int
		wantLine  int
		wantExtra int
	}{
		{
			name:      "rust dispatch reports the call, not the fn line",
			path:      "src/expression.rs",
			content:   rustDispatch,
			token:     "target",
			spanStart: 1,
			spanEnd:   len(linesOf(rustDispatch)),
			wantLine:  21,
		},
		{
			name: "go method call inside a long switch",
			path: "server.go",
			content: "package app\n\nfunc (s *Server) Handle(kind string) {\n" +
				"\tswitch kind {\n\tcase \"a\":\n\t\treturn\n\tcase \"b\":\n" +
				"\t\ts.dispatch(kind)\n\t}\n}\n",
			token:     "dispatch",
			spanStart: 3,
			spanEnd:   10,
			wantLine:  8,
		},
		{
			name: "python indented call",
			path: "handler.py",
			content: "def handle(event):\n" +
				"    if event.kind == \"a\":\n" +
				"        return None\n" +
				"    for item in event.items:\n" +
				"        if item.valid:\n" +
				"            process(item)\n",
			token:     "process",
			spanStart: 1,
			spanEnd:   6,
			wantLine:  6,
		},
		{
			name:      "several call sites report the first and count the rest",
			path:      "multi.go",
			content:   "package app\n\nfunc Outer() {\n\thelper()\n\tif true {\n\t\thelper()\n\t}\n\thelper()\n}\n",
			token:     "helper",
			spanStart: 3,
			spanEnd:   9,
			wantLine:  4,
			wantExtra: 2,
		},
		{
			name:      "turbofish and macro call forms still count as calls",
			path:      "generic.rs",
			content:   "fn outer() {\n    let a = 1;\n    convert::<u32>(a);\n}\n",
			token:     "convert",
			spanStart: 1,
			spanEnd:   4,
			wantLine:  3,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			read := staticLineReader(map[string]string{testCase.path: testCase.content})
			site, ok := resolveCallSite(read, testCase.path, testCase.token, testCase.spanStart, testCase.spanEnd, 0)
			if !ok {
				t.Fatalf("resolveCallSite reported no site")
			}
			if site.Line != testCase.wantLine {
				t.Fatalf("call line = %d, want %d", site.Line, testCase.wantLine)
			}
			if site.AdditionalSites != testCase.wantExtra {
				t.Fatalf("additional sites = %d, want %d", site.AdditionalSites, testCase.wantExtra)
			}
			if site.Line == testCase.spanStart && testCase.wantLine != testCase.spanStart {
				t.Fatalf("call line degenerated to the container start line")
			}
		})
	}
}

// A name written in a comment or a string is not a call. Masking must remove
// both before matching, or the reported line points at prose.
func TestResolveCallSiteIgnoresCommentsAndStringLiterals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		path     string
		content  string
		wantLine int
	}{
		{
			name:     "line comment mention",
			path:     "a.go",
			content:  "package a\n\nfunc Outer() {\n\t// calls target(x) eventually\n\ttarget(x)\n}\n",
			wantLine: 5,
		},
		{
			name:     "string literal mention",
			path:     "b.go",
			content:  "package b\n\nfunc Outer() {\n\tlog(\"target(x) failed\")\n\ttarget(x)\n}\n",
			wantLine: 5,
		},
		{
			name:     "block comment mention",
			path:     "c.rs",
			content:  "fn outer() {\n    /* target(x) is deprecated */\n    target(x);\n}\n",
			wantLine: 3,
		},
		{
			name:     "python hash comment mention",
			path:     "d.py",
			content:  "def outer():\n    # target(x) used to live here\n    target(x)\n",
			wantLine: 3,
		},
		{
			name:     "python docstring mention",
			path:     "e.py",
			content:  "def outer():\n    \"\"\"Calls target(x).\"\"\"\n    target(x)\n",
			wantLine: 3,
		},
		{
			// `#` is program text in Rust and C, so it must not start a comment
			// there or the attribute line would be blanked away.
			name:     "rust attribute is not a comment",
			path:     "f.rs",
			content:  "fn outer() {\n    #[allow(unused)]\n    let x = target(1);\n}\n",
			wantLine: 3,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			read := staticLineReader(map[string]string{testCase.path: testCase.content})
			site, ok := resolveCallSite(read, testCase.path, "target", 1, len(linesOf(testCase.content)), 0)
			if !ok {
				t.Fatalf("resolveCallSite reported no site")
			}
			if site.Line != testCase.wantLine {
				t.Fatalf("call line = %d, want %d (matched a comment or literal?)", site.Line, testCase.wantLine)
			}
		})
	}
}

// A `'` is not always a literal. Rust lifetimes and loop labels are program text,
// and treating one as an unterminated char literal would blank the rest of the
// line — losing any call written on it.
func TestResolveCallSiteHandlesLifetimesAndLabels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		path     string
		content  string
		wantLine int
	}{
		{
			name:     "lifetime on the call's own line",
			path:     "a.rs",
			content:  "fn outer<'a>(x: &'a str) { target(x); }\n",
			wantLine: 1,
		},
		{
			name:     "single lifetime with no partner quote",
			path:     "b.rs",
			content:  "fn outer() {\n    let v: Vec<&'static str> = target();\n}\n",
			wantLine: 2,
		},
		{
			name:     "loop label",
			path:     "c.rs",
			content:  "fn outer() {\n    'outer: loop { target(1); }\n}\n",
			wantLine: 2,
		},
		{
			// A real char literal must still be masked.
			name:     "char literal mentioning the callee is not a call",
			path:     "d.rs",
			content:  "fn outer() {\n    let c = 't';\n    target(c);\n}\n",
			wantLine: 3,
		},
		{
			// In a language where `'` opens a STRING, a long single-quoted string
			// must still be masked — the lifetime exemption must not leak there.
			name:     "long single-quoted javascript string is still masked",
			path:     "e.js",
			content:  "function outer() {\n  log('target(x) failed with a long explanatory message');\n  target(x);\n}\n",
			wantLine: 3,
		},
		{
			name:     "long single-quoted python string is still masked",
			path:     "f.py",
			content:  "def outer():\n    log('target(x) failed with a long explanatory message')\n    target(x)\n",
			wantLine: 3,
		},
		{
			name:     "long single-quoted php string is still masked",
			path:     "g.php",
			content:  "function outer() {\n  log('target($x) failed with a long explanatory message');\n  target($x);\n}\n",
			wantLine: 3,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			read := staticLineReader(map[string]string{testCase.path: testCase.content})
			site, ok := resolveCallSite(read, testCase.path, "target", 1, len(linesOf(testCase.content)), 0)
			if !ok {
				t.Fatalf("resolveCallSite reported no site (a lifetime blanked the line?)")
			}
			if site.Line != testCase.wantLine {
				t.Fatalf("call line = %d, want %d", site.Line, testCase.wantLine)
			}
		})
	}
}

// The guard chain is the highest-value part of the answer: it is what makes a
// patch provably safe. It must be verbatim source, in source order, and must
// include the pattern bindings that constrain the call's inputs.
func TestEnclosingGuardLinesSurfaceTheConditionChainVerbatim(t *testing.T) {
	t.Parallel()
	read := staticLineReader(map[string]string{"src/expression.rs": rustDispatch})
	site, ok := resolveCallSite(read, "src/expression.rs", "target", 1, len(linesOf(rustDispatch)), 0)
	if !ok {
		t.Fatal("resolveCallSite reported no site")
	}
	wantContains := []string{
		"if let Expr::Attribute(",
		"if let Expr::StringLiteral(",
		"} else if attr == \"format\" {",
		"Ok(summary) => {",
		"if checker.enabled(Rule::Target) {",
	}
	joined := ""
	previous := 0
	for _, guard := range site.Guards {
		if guard.Line <= previous {
			t.Fatalf("guards not in source order: %v", site.Guards)
		}
		previous = guard.Line
		if guard.Text != linesOf(rustDispatch)[guard.Line-1] {
			t.Fatalf("guard %d is not verbatim source: %q", guard.Line, guard.Text)
		}
		joined += guard.Text + "\n"
	}
	for _, want := range wantContains {
		if !strings.Contains(joined, want) {
			t.Fatalf("guard chain omitted %q:\n%s", want, joined)
		}
	}
	// No synthesized claim about what the guards imply may appear: a wrong
	// invariant is worse than none.
	if strings.Contains(joined, "INVARIANT") {
		t.Fatalf("guard chain synthesized an invariant claim:\n%s", joined)
	}
}

func TestEnclosingGuardLinesIndentedLanguage(t *testing.T) {
	t.Parallel()
	const source = "def handle(event):\n" +
		"    setup()\n" +
		"    if event.kind == \"a\":\n" +
		"        for item in event.items:\n" +
		"            if item.valid:\n" +
		"                process(item)\n"
	read := staticLineReader(map[string]string{"handler.py": source})
	site, ok := resolveCallSite(read, "handler.py", "process", 1, len(linesOf(source)), 0)
	if !ok {
		t.Fatal("resolveCallSite reported no site")
	}
	var got []int
	for _, guard := range site.Guards {
		got = append(got, guard.Line)
	}
	if len(got) != 3 || got[0] != 3 || got[1] != 4 || got[2] != 5 {
		t.Fatalf("python guard lines = %v, want [3 4 5]", got)
	}
}

// Over the cap, the INNERMOST conditions are kept (the outer ones are the
// function's dispatch skeleton) and the omission is stated, never hidden.
func TestGuardCapKeepsInnermostAndReportsOmission(t *testing.T) {
	t.Parallel()
	var builder strings.Builder
	builder.WriteString("fn outer() {\n")
	for depth := 0; depth < callContextGuardLimit+3; depth++ {
		builder.WriteString(strings.Repeat("    ", depth+1) + "if cond" + string(rune('a'+depth)) + " {\n")
	}
	builder.WriteString(strings.Repeat("    ", callContextGuardLimit+4) + "target(1);\n")
	source := builder.String()
	read := staticLineReader(map[string]string{"deep.rs": source})
	site, ok := resolveCallSite(read, "deep.rs", "target", 1, len(linesOf(source)), 0)
	if !ok {
		t.Fatal("resolveCallSite reported no site")
	}
	if len(site.Guards) != callContextGuardLimit {
		t.Fatalf("guards kept = %d, want %d", len(site.Guards), callContextGuardLimit)
	}
	if site.GuardsOmitted != 3 {
		t.Fatalf("guards omitted = %d, want 3", site.GuardsOmitted)
	}
	// The kept guards must be the deepest ones, i.e. the ones closest to the call.
	if !strings.Contains(site.Guards[len(site.Guards)-1].Text, "cond"+string(rune('a'+callContextGuardLimit+2))) {
		t.Fatalf("innermost guard not kept: %#v", site.Guards)
	}
	rendered := renderCallContext(&site)
	if !strings.Contains(rendered, "+3 outer conditions omitted") {
		t.Fatalf("render hid the omission:\n%s", rendered)
	}
}

// Falling back to the definition line is correct when the call cannot be located;
// inventing a line would be worse than reporting the one the graph knows.
func TestResolveCallSiteFallsBackWhenUnresolvable(t *testing.T) {
	t.Parallel()
	files := map[string]string{"a.go": "package a\n\nfunc Outer() {\n\tother()\n}\n"}
	read := staticLineReader(files)
	cases := []struct {
		name  string
		path  string
		token string
		start int
		end   int
	}{
		{name: "token absent from the body", path: "a.go", token: "target", start: 3, end: 5},
		{name: "unreadable file", path: "missing.go", token: "target", start: 1, end: 10},
		{name: "empty token", path: "a.go", token: "", start: 1, end: 5},
		{name: "no span", path: "a.go", token: "other", start: 0, end: 0},
		{name: "span past end of file", path: "a.go", token: "other", start: 99, end: 120},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := resolveCallSite(read, testCase.path, testCase.token, testCase.start, testCase.end, 0); ok {
				t.Fatalf("resolveCallSite claimed a site it could not know")
			}
		})
	}
}

func TestCallTokenFromDetail(t *testing.T) {
	t.Parallel()
	cases := []struct{ detail, fallback, want string }{
		{detail: "string_dot_format_extra", want: "string_dot_format_extra"},
		{detail: "diagnostic.try_set_fix", want: "try_set_fix"},
		{detail: "pyflakes::rules::target", want: "target"},
		{detail: "Foo<T>::bar()", want: "bar"},
		{detail: "obj->method(arg)", want: "method"},
		{detail: "$this->run()", want: "run"},
		{detail: "", fallback: "Fallback", want: "Fallback"},
		{detail: "(anonymous)", fallback: "Named", want: "Named"},
		{detail: "", fallback: "", want: ""},
		{detail: "1bad", fallback: "", want: ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.detail+"/"+testCase.fallback, func(t *testing.T) {
			t.Parallel()
			if got := callTokenFromDetail(testCase.detail, testCase.fallback); got != testCase.want {
				t.Fatalf("callTokenFromDetail(%q, %q) = %q, want %q",
					testCase.detail, testCase.fallback, got, testCase.want)
			}
		})
	}
}

// Never inline a caller's body: a dispatch function can be thousands of lines,
// and a window is orders of magnitude cheaper while answering the same question.
func TestCallContextIsWindowedAndBudgeted(t *testing.T) {
	t.Parallel()
	var builder strings.Builder
	builder.WriteString("fn giant() {\n")
	for index := 0; index < 1400; index++ {
		builder.WriteString("    let filler = " + strings.Repeat("x", 40) + ";\n")
	}
	builder.WriteString("    target(1);\n")
	for index := 0; index < 1400; index++ {
		builder.WriteString("    let more = " + strings.Repeat("y", 40) + ";\n")
	}
	builder.WriteString("}\n")
	source := builder.String()
	total := len(linesOf(source))
	read := staticLineReader(map[string]string{"giant.rs": source})
	site, ok := resolveCallSite(read, "giant.rs", "target", 1, total, 0)
	if !ok {
		t.Fatal("resolveCallSite reported no site")
	}
	if len(site.Window) > callContextWindowLines {
		t.Fatalf("window = %d lines, cap %d", len(site.Window), callContextWindowLines)
	}
	rendered := renderCallContext(&site)
	if len(rendered) > callContextBudget {
		t.Fatalf("one call context used %d bytes, budget %d", len(rendered), callContextBudget)
	}
	if len(rendered) > len(source)/50 {
		t.Fatalf("call context is not dramatically cheaper than the body: %d vs %d bytes",
			len(rendered), len(source))
	}

	// Across an answer, the shared budget caps both block count and bytes.
	budget := newCallContextBudgeter()
	var out bytes.Buffer
	for index := 0; index < 10; index++ {
		writeCallContext(&out, &site, budget)
	}
	if out.Len() > callContextBudget {
		t.Fatalf("call contexts used %d bytes, budget %d", out.Len(), callContextBudget)
	}
	if blocks := strings.Count(out.String(), "conditions in force at this call"); blocks > callContextBlocks {
		t.Fatalf("rendered %d context blocks, cap %d", blocks, callContextBlocks)
	}
}

// The byte ceiling must bind independently of the block count: three blocks of
// deeply nested, wide source would otherwise be allowed through on block count
// alone and blow the answer's size.
func TestCallContextByteBudgetBindsBeforeBlockCount(t *testing.T) {
	t.Parallel()
	var builder strings.Builder
	builder.WriteString("fn outer() {\n")
	for depth := 0; depth < callContextGuardLimit; depth++ {
		builder.WriteString(strings.Repeat("    ", depth+1) +
			"if condition_" + string(rune('a'+depth)) + " == " + strings.Repeat("w", 60) + " {\n")
	}
	indent := strings.Repeat("    ", callContextGuardLimit+1)
	builder.WriteString(indent + "target(1);\n")
	for index := 0; index < 12; index++ {
		builder.WriteString(indent + "let after_" + string(rune('a'+index)) + " = " + strings.Repeat("v", 60) + ";\n")
	}
	source := builder.String()
	read := staticLineReader(map[string]string{"wide.rs": source})
	site, ok := resolveCallSite(read, "wide.rs", "target", 1, len(linesOf(source)), 0)
	if !ok {
		t.Fatal("resolveCallSite reported no site")
	}
	blockSize := len(renderCallContext(&site))
	if blockSize*callContextBlocks <= callContextBudget {
		t.Fatalf("fixture too small to exercise the byte cap: %d bytes x %d blocks", blockSize, callContextBlocks)
	}
	budget := newCallContextBudgeter()
	var out bytes.Buffer
	for index := 0; index < callContextBlocks; index++ {
		writeCallContext(&out, &site, budget)
	}
	if out.Len() > callContextBudget {
		t.Fatalf("byte cap did not bind: %d bytes, budget %d", out.Len(), callContextBudget)
	}
	blocks := strings.Count(out.String(), "conditions in force at this call")
	if blocks >= callContextBlocks {
		t.Fatalf("emitted %d blocks; the byte cap should have stopped before the block cap", blocks)
	}
	if blocks == 0 {
		t.Fatalf("byte cap suppressed every block:\n%s", out.String())
	}
}

// A very long line must not be able to spend the whole budget on itself.
func TestCallContextTruncatesOverlongLines(t *testing.T) {
	t.Parallel()
	source := "fn outer() {\n    let x = " + strings.Repeat("z", 5000) + ";\n    target(1);\n}\n"
	read := staticLineReader(map[string]string{"wide.rs": source})
	site, ok := resolveCallSite(read, "wide.rs", "target", 1, len(linesOf(source)), 0)
	if !ok {
		t.Fatal("resolveCallSite reported no site")
	}
	for _, line := range strings.Split(renderCallContext(&site), "\n") {
		if len(line) > callContextLineWidth+32 {
			t.Fatalf("rendered line of %d bytes exceeds the width cap: %q", len(line), line[:80])
		}
	}
}

func TestCallEvidenceSpanReadsCallSiteEvidence(t *testing.T) {
	t.Parallel()
	// A language outside call_site_lines: the entry is the caller's span and
	// the call is looked for in it.
	java := neighborEndpoint{Kind: "method", Language: "Java", StartLine: 24, EndLine: 1694}
	path, start, end, callLine, token, ok := callEvidenceSpan([]sem.Evidence{
		{Kind: "import", FilePath: "other.go", StartLine: 1},
		{Kind: "call_site", FilePath: "Caller.java", StartLine: 24, EndLine: 1694, Detail: "target"},
	}, java)
	if !ok || path != "Caller.java" || start != 24 || end != 1694 || callLine != 0 || token != "target" {
		t.Fatalf("callEvidenceSpan = (%q, %d, %d, %d, %q, %v)", path, start, end, callLine, token, ok)
	}
	if _, _, _, _, _, ok := callEvidenceSpan([]sem.Evidence{{Kind: "import", StartLine: 3}}, java); ok {
		t.Fatal("callEvidenceSpan accepted non-call evidence")
	}
}

// For a caller in a language call_site_lines lists, the entry's line is the
// call: it is reported as is, and the span searched for the other calls and
// the guards is the caller's own -- the whole file for a file-level caller.
// Reason: the entry no longer carries the caller's span, so reading it as one
// would clip the window and the guards to the call's single line.
func TestCallEvidenceSpanTakesTheEnginesCallLine(t *testing.T) {
	t.Parallel()
	goCaller := neighborEndpoint{Kind: "function", Language: "Go", StartLine: 3, EndLine: 900}
	path, start, end, callLine, token, ok := callEvidenceSpan([]sem.Evidence{
		{Kind: "call_site", FilePath: "dispatch.go", StartLine: 806, EndLine: 806, Detail: "Target"},
	}, goCaller)
	if !ok || path != "dispatch.go" || start != 3 || end != 900 || callLine != 806 || token != "Target" {
		t.Fatalf("callEvidenceSpan = (%q, %d, %d, %d, %q, %v)", path, start, end, callLine, token, ok)
	}
	file := neighborEndpoint{Kind: "file", Language: "TypeScript"}
	path, start, end, callLine, _, ok = callEvidenceSpan([]sem.Evidence{
		{Kind: "top_level_call_site", FilePath: "a.test.ts", StartLine: 6, EndLine: 6, Detail: "helper"},
	}, file)
	if !ok || path != "a.test.ts" || start != 1 || end != 0 || callLine != 6 {
		t.Fatalf("file-level callEvidenceSpan = (%q, %d, %d, %d, %v)", path, start, end, callLine, ok)
	}
	// An entry with no line is a call the engine could not place: no site.
	if _, _, _, _, _, ok := callEvidenceSpan([]sem.Evidence{{Kind: "call_site", FilePath: "a.ts", Detail: "helper"}}, neighborEndpoint{Kind: "function", Language: "TypeScript", StartLine: 1, EndLine: 9}); ok {
		t.Fatal("an entry with no line produced a call site")
	}
}

// resolveCallSite reports the engine's line even when another call of the
// same name comes first in the span, and counts that one as an additional
// site. Reason: the engine's line is the call the relation came from.
func TestResolveCallSiteKeepsTheEnginesLine(t *testing.T) {
	t.Parallel()
	source := "func caller() {\n\tif ok {\n\t\ttarget()\n\t}\n\ttarget()\n}\n"
	read := func(string) ([]string, bool) { return linesOf(source), true }
	site, ok := resolveCallSite(read, "caller.go", "target", 1, 6, 5)
	if !ok || site.Line != 5 || site.AdditionalSites != 1 {
		t.Fatalf("site = %+v, %v; want line 5 with one additional site", site, ok)
	}
	site, ok = resolveCallSite(read, "caller.go", "target", 1, 6, 3)
	if !ok || site.Line != 3 || len(site.Guards) != 1 || site.Guards[0].Line != 2 {
		t.Fatalf("site = %+v, %v; want line 3 under the guard on line 2", site, ok)
	}
}

// TestRepoLineReaderConfinesReadsToRepoRoot pins repoRoot as a boundary rather than a prefix.
// Every case here READ SUCCESSFULLY before os.Root was introduced, because filepath.Join
// normalizes `..` away and os.Lstat only guarded the final path component. Nothing feeds this
// an untrusted relPath today; these assertions are what keep that from mattering.
func TestRepoLineReaderConfinesReadsToRepoRoot(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	outside := filepath.Join(parent, "outside")
	for _, dir := range []string{repo, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "inside.go"), []byte("package a\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	read := newRepoLineReader(repo)

	if lines, ok := read("inside.go"); !ok || len(lines) == 0 || lines[0] != "package a" {
		t.Fatalf("ordinary in-repo read broke: lines=%q ok=%v", lines, ok)
	}

	for _, name := range []string{
		"../outside/secret.txt",
		"./../outside/secret.txt",
		"sub/../../outside/secret.txt",
		filepath.Join(outside, "secret.txt"),
	} {
		if lines, ok := read(name); ok {
			t.Errorf("read(%q) escaped the repo root and returned %q", name, lines)
		}
	}
}

// TestRepoLineReaderRefusesSymlinkEscapes covers both halves: a symlinked FINAL component was
// already refused by the Lstat check and must stay refused, while a symlinked INTERMEDIATE
// component was silently followed by the read that came after it.
func TestRepoLineReaderRefusesSymlinkEscapes(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	outside := filepath.Join(parent, "outside")
	for _, dir := range []string{repo, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "outside"), filepath.Join(repo, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join("..", "outside", "secret.txt"), filepath.Join(repo, "direct.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	read := newRepoLineReader(repo)
	if lines, ok := read("escape/secret.txt"); ok {
		t.Errorf("intermediate symlink was followed out of the repo: %q", lines)
	}
	if lines, ok := read("direct.txt"); ok {
		t.Errorf("symlinked final component was read: %q", lines)
	}
}

// TestRepoLineReaderKeepsSizeAndBinaryRefusals guards the behavior that already existed, since
// the read now goes through a limit reader rather than os.ReadFile.
func TestRepoLineReaderKeepsSizeAndBinaryRefusals(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "binary.go"), []byte("package a\x00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "huge.go"), make([]byte, callSiteMaxFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	read := newRepoLineReader(repo)
	if _, ok := read("binary.go"); ok {
		t.Error("NUL-bearing file was accepted")
	}
	if _, ok := read("huge.go"); ok {
		t.Error("oversized file was accepted")
	}
}
