package sem

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// directiveFlags reports, for the comment whose text is comment in source,
// whether its leaves are flagged directive (all of them, or none).
func directiveFlags(t *testing.T, path, source, comment string) bool {
	t.Helper()
	start := strings.Index(source, comment)
	if start < 0 {
		t.Fatalf("%s: %q not in source", path, comment)
	}
	end := start + len(comment)
	record := fileTokensFor(t, path, source)
	if record.Status != tokensExtracted || record.HasErrors {
		t.Fatalf("%s: status %s, has_errors %v for %q", path, record.Status, record.HasErrors, source)
	}
	seen, flagged := 0, 0
	for _, row := range record.Leaves {
		if row[1] >= end || row[2] <= start || row[1] == row[2] {
			continue
		}
		if row[5]&tokenFlagComment == 0 {
			t.Fatalf("%s: %q holds a non-comment leaf %q", path, comment, source[row[1]:row[2]])
		}
		seen++
		if row[5]&tokenFlagDirective != 0 {
			flagged++
		}
	}
	if seen == 0 || flagged != 0 && flagged != seen {
		t.Fatalf("%s: %q has %d comment leaves, %d flagged", path, comment, seen, flagged)
	}
	return flagged > 0
}

// Known directives are flagged in every declared language, prose comments are
// not, and the shape rule flags what does not read as prose. Each entry is a
// file and the comment in it.
func TestDirectiveComments(t *testing.T) {
	type entry struct{ path, source, comment string }
	line := func(path, comment, after string) entry { return entry{path, comment + "\n" + after, comment} }
	goFile := "package p\n"
	directives := []entry{
		// Go: //go: directives, the old +build form, //line, //export,
		// nolint, example output, and the cgo preamble.
		line("a.go", "//go:build linux", "\n"+goFile), line("a.go", "//go:embed x.txt", "var x string\n"),
		line("a.go", "//go:generate stringer -type=T", goFile), line("a.go", "//go:linkname a b", "var a int\n"),
		line("a.go", "//go:noinline", "func f() {}\n"), line("a.go", "//go:nosplit", "func f() {}\n"),
		line("a.go", "//go:wasmimport env f", "func f()\n"), line("a.go", "// +build linux", "\n"+goFile),
		line("a.go", "//line a.go:10", goFile), line("a.go", "//export F", "func F() {}\n"),
		line("a.go", "//nolint", goFile), line("a.go", "// nolint", goFile), line("a.go", "//nolint:errcheck", goFile),
		line("a.go", "// Output: 42", goFile), line("a.go", "// Unordered output: 1", goFile),
		{"a.go", "package p\n\n// #include <stdio.h>\n// int add(int a, int b);\nimport \"C\"\n", "// int add(int a, int b);"},
		{"a.go", "package p\n\n/*\nint twice(int a);\n*/\nimport \"C\"\n", "/*\nint twice(int a);\n*/"},
		// TypeScript and JavaScript.
		line("a.ts", "// @ts-ignore", "f();"), line("a.ts", "// @ts-expect-error", "f();"), line("a.ts", "// @ts-nocheck", "f();"),
		line("a.ts", "// @ts-check", "f();"), line("a.ts", `/// <reference path="a.d.ts" />`, "f();"),
		line("a.ts", `/// <amd-module name="x"/>`, "f();"), line("a.tsx", "/** @jsx h */", "f();"),
		line("a.tsx", "/** @jsxImportSource preact */", "f();"), line("a.tsx", "/* @jsxRuntime classic */", "f();"),
		line("a.tsx", "/** @jsxFrag Fragment */", "f();"), {"a.ts", "const x = /*#__PURE__*/ f();\n", "/*#__PURE__*/"},
		{"a.ts", "const x = /*@__PURE__*/ f();\n", "/*@__PURE__*/"}, line("a.ts", "/*#__NO_SIDE_EFFECTS__*/", "function f() {}"),
		{"a.js", "import(/* webpackChunkName: \"x\" */ \"./x\");\n", "/* webpackChunkName: \"x\" */"},
		{"a.js", "import(/* @vite-ignore */ u);\n", "/* @vite-ignore */"}, line("a.js", "//# sourceMappingURL=a.js.map", ""),
		line("a.js", "//# sourceURL=a.js", ""), line("a.ts", "// eslint-disable-next-line no-console", "f();"),
		line("a.ts", "/* eslint-disable */", "f();"), line("a.ts", "// prettier-ignore", "f();"),
		line("a.ts", "// biome-ignore lint/style: reason", "f();"), line("a.ts", "/* istanbul ignore next */", "f();"),
		line("a.ts", "/* c8 ignore next */", "f();"), line("a.ts", "/* v8 ignore next */", "f();"),
		line("a.ts", "/*! @license MIT */", "f();"), line("a.ts", "/** @preserve Keep this */", "f();"),
		line("a.ts", "/**\n * Adds.\n * @param a first\n */", "function f(a) {}"),
		// Python: encoding cookies, type comments and tool pragmas.
		line("a.py", "# -*- coding: utf-8 -*-", "x = 1"), line("a.py", "# vim: set fileencoding=latin-1 :", "x = 1"),
		line("a.py", "# coding=utf-8", "x = 1"), {"a.py", "x = []  # type: list[int]\n", "# type: list[int]"},
		line("a.py", "# type: ignore", "x = 1"), {"a.py", "import os  # noqa\n", "# noqa"},
		{"a.py", "import os  # noqa: F401\n", "# noqa: F401"}, line("a.py", "# pragma: no cover", "x = 1"),
		line("a.py", "# pylint: disable=invalid-name", "x = 1"), line("a.py", "# mypy: ignore-errors", "x = 1"),
		line("a.py", "# fmt: off", "x = 1"), line("a.py", "# isort: skip_file", "x = 1"), line("a.py", "# ruff: noqa", "x = 1"),
		line("a.py", "#!/usr/bin/env python3", "x = 1"),
		// Ruby: magic comments and tool markers.
		line("a.rb", "# frozen_string_literal: true", "x = 1"), line("a.rb", "# encoding: utf-8", "x = 1"),
		line("a.rb", "# coding: utf-8", "x = 1"), line("a.rb", "# -*- coding: utf-8 -*-", "x = 1"),
		line("a.rb", "# warn_indent: true", "x = 1"), line("a.rb", "# shareable_constant_value: literal", "x = 1"),
		line("a.rb", "# rubocop:disable Style/Foo", "x = 1"), line("a.rb", "# typed: strict", "x = 1"),
		// Rust: every documentation comment.
		line("a.rs", "/// Adds two numbers.", "fn f() {}"), line("a.rs", "//! Crate docs.", "fn f() {}"),
		line("a.rs", "/** Block docs. */", "fn f() {}"), line("a.rs", "/*! Inner block docs. */", "fn f() {}"),
		// C markers.
		line("a.c", "// NOLINT", "int x;"), line("a.c", "// NOLINTNEXTLINE(bugprone-x)", "int x;"),
		line("a.c", "/* clang-format off */", "int x;"), line("a.c", "/* fallthrough */", "int x;"),
		line("a.c", "/* FALLTHROUGH */", "int x;"), line("a.c", "/* NOTREACHED */", "int x;"),
		// C#: XML documentation (read under -doc) and tool markers.
		line("a.cs", "/// <summary>Adds.</summary>", "class A {}"), line("a.cs", "/// Adds two numbers.", "class A {}"),
		line("a.cs", "// ReSharper disable once UnusedMember.Global", "class A {}"), line("a.cs", "// <auto-generated/>", "class A {}"),
		line("a.cs", "// NOSONAR", "class A {}"),
		// Java markers and the javadoc @deprecated tag javac reads.
		line("A.java", "//noinspection unchecked", "class A {}"), line("A.java", "// CHECKSTYLE:OFF", "class A {}"),
		line("A.java", "// NOSONAR", "class A {}"), line("A.java", "// @formatter:off", "class A {}"),
		line("A.java", "/** @deprecated use B */", "class A {}"),
		// Elixir: tool markers (its compiler reads no comment).
		line("a.ex", "# credo:disable-for-next-line", "x = 1"), line("a.ex", "# coveralls-ignore-start", "x = 1"),
		// Go example output: every line in the group after "// Output:".
		{"a.go", "package p\n\nfunc ExampleF() {\n\tF()\n\t// Output:\n\t// hello world\n}\n", "// hello world"},
		{"a.go", "package p\n\nfunc ExampleF() {\n\tF()\n\t// Unordered output:\n\t// b first\n\t// a second\n}\n", "// a second"},
		// Go analysistest expectations and generated-file banners.
		{"a.go", "package p\n\nvar x = 1 // want \"unused variable\"\n", `// want "unused variable"`},
		line("a.go", "// Code generated by stringer; DO NOT EDIT.", goFile),
		// Python coverage pragmas, any case, with or without the colon.
		{"a.py", "if x:  # pragma no cover\n    y = 1\n", "# pragma no cover"},
		{"a.py", "if x:  # PRAGMA: NO COVER\n    y = 1\n", "# PRAGMA: NO COVER"},
		// Fall-through comments in any case and spelling, where compilers and
		// linters read them.
		line("a.c", "/* Fall through. */", "int x;"), line("a.c", "/* Falls through */", "int x;"), line("a.c", "// fall thru", "int x;"),
		line("A.java", "// Fall through.", "class A {}"), line("A.java", "/* FALLS THROUGH */", "class A {}"),
		line("a.js", "// falls through", "f();"), line("a.js", "// Fall thru to the next case.", "f();"),
		line("a.ts", "/* Fall-through intended. */", "f();"), line("a.ts", "// falling through", "f();"),
		// JetBrains inspections, SonarQube and generated banners everywhere.
		line("a.go", "// noinspection GoUnusedExportedFunction", goFile), line("a.py", "# noinspection PyUnresolvedReferences", "x = 1"),
		line("a.rb", "# noinspection RubyResolve", "x = 1"), line("a.ts", "// noinspection JSUnusedGlobalSymbols", "f();"),
		line("a.c", "// noinspection CppUnusedIncludeDirective", "int x;"), line("a.cs", "// noinspection CSharpWarnings", "class A {}"),
		line("a.rs", "// noinspection RsUnresolvedReference", "fn f() {}"), line("a.ex", "# noinspection ElixirUnused", "x = 1"),
		line("a.py", "# This file is generated. DO NOT EDIT.", "x = 1"),
		// The shape rule: not prose, so flagged.
		line("a.go", "// TODO: retry", goFile), line("a.go", "//no space after the marker", goFile),
		line("a.ts", "// x=1 keeps it", "f();"), line("a.ts", "// 3rd attempt", "f();"), line("a.py", "# [tool] marker", "x = 1"),
		line("a.rb", "# $DEBUG only", "x = 1"), line("a.c", "/**/", "int x;"),
		// A single bare word is no prose, whatever the word.
		line("a.go", "// explain", goFile), line("a.ts", "// explain", "f();"), line("a.ts", "/* explain */", "f();"),
		line("a.py", "# nosec", "x = 1"), line("a.rb", "# nocov", "x = 1"), line("a.go", "// nosemgrep", goFile),
		line("A.java", "// skipcq", "class A {}"), line("a.c", "/* Done. */", "int x;"),
		line("a.ts", "/**\n * Word\n */", "f();"),
		// Four slashes are no Rust doc comment, but the body starts with "/".
		line("a.rs", "//// Four slashes.", "fn f() {}"),
	}
	prose := []entry{
		line("a.go", "// This loop retries.", goFile), line("a.go", "// Retry until done, then stop.", goFile),
		line("a.ts", "// This loop retries.", "f();"), line("a.ts", "/** Returns the sum. */", "f();"),
		line("a.ts", "/* Plain block comment. */", "f();"), line("a.ts", "/**\n * Multi line prose.\n * Second line here.\n */", "f();"),
		line("a.js", "// However, we wait.", "f();"), line("a.py", "# This loop retries.", "x = 1"), line("a.py", "# It's fine.", "x = 1"),
		line("a.rb", "# Retries the call.", "x = 1"), line("a.rs", "// Plain prose here.", "fn f() {}"),
		line("a.rs", "/* Plain block. */", "fn f() {}"),
		line("a.c", "// Plain prose here.", "int x;"), line("a.c", "/** Doxygen prose. */", "int x;"),
		line("a.cs", "// Plain prose.", "class A {}"), line("a.cs", "/** Plain doc prose. */", "class A {}"),
		line("A.java", "// Plain prose.", "class A {}"), line("A.java", "/** Plain javadoc prose. */", "class A {}"),
		line("a.ex", "# Plain prose.", "x = 1"),
		// Prose that only resembles a marker stays prose.
		line("a.c", "// The value falls back to zero.", "int x;"), line("a.ts", "// We want the retry bounded.", "f();"),
		// A Go comment group that is not directly above import "C".
		{"a.go", "package p\n\n// Plain prose here.\n\nimport \"C\"\n", "// Plain prose here."},
	}
	for _, e := range directives {
		if !directiveFlags(t, e.path, e.source, e.comment) {
			t.Errorf("%s: %q is not flagged directive", e.path, e.comment)
		}
	}
	for _, e := range prose {
		if directiveFlags(t, e.path, e.source, e.comment) {
			t.Errorf("%s: prose %q is flagged directive", e.path, e.comment)
		}
	}
}

// Under the consumer's rule a changed, added or removed directive comment is
// a token change, while an added prose comment of several words is not.
func TestDirectiveCommentChangesAreTokenChanges(t *testing.T) {
	changes := []struct{ path, base, head string }{
		{"a.go", "//go:build linux\n\npackage p\n", "//go:build darwin\n\npackage p\n"},
		{"a.go", "//go:build linux\n\npackage p\n", "// go:build linux\n\npackage p\n"},
		{"a.ts", "f();\n", "// @ts-ignore\nf();\n"},
		{"a.py", "# -*- coding: utf-8 -*-\nx = 1\n", "# -*- coding: latin-1 -*-\nx = 1\n"},
		{"a.rb", "# frozen_string_literal: true\nx = 1\n", "# frozen_string_literal: false\nx = 1\n"},
		{"a.go", "package p\n\n// int one(void);\nimport \"C\"\n", "package p\n\n// int two(void);\nimport \"C\"\n"},
	}
	for _, c := range changes {
		if consumerNoTokenChange(t, c.path, c.base, c.head) {
			t.Errorf("%s: %q -> %q read as no token change", c.path, c.base, c.head)
		}
	}
	same := []struct{ path, base, head string }{
		{"a.ts", "function f() {\n  return 1;\n}\n", "function f() {\n  // Explain why the retry is bounded.\n  return 1;\n}\n"},
		{"a.go", "package p\n\n// Old prose.\nvar x = 1\n", "package p\n\n// New prose here.\nvar x = 1\n"},
	}
	for _, c := range same {
		if !consumerNoTokenChange(t, c.path, c.base, c.head) {
			t.Errorf("%s: a prose comment change read as a token change", c.path)
		}
	}
}

// Python's leading whitespace is syntax the tree does not check: these pairs
// parse cleanly and give equal leaves although CPython rejects the second
// file. The engine does not judge indentation; it declares Python
// indentation-significant, and a consumer requires byte-identical leading
// whitespace there.
func TestPythonIndentationIsDeclaredSignificant(t *testing.T) {
	pairs := [][2]string{
		{"if a:\n    x()\ny()\n", "if a:\n    x()\n  y()\n"},         // inconsistent dedent
		{"x()\ny()\n", "x()\n    y()\n"},                             // over-indented, no block to open
		{"if a:\n    x()\n    y()\n", "if a:\n    x()\n      y()\n"}, // mixed widths in one block
	}
	for _, pair := range pairs {
		if !tokensAvailable(t, "a.py", pair[1]) || !consumerNoTokenChange(t, "a.py", pair[0], pair[1]) {
			t.Logf("the tree now tells %q apart; the declaration still stands", pair[1])
		}
	}
	if !slices.Equal(IndentationSignificantLanguages(), []string{"Python"}) {
		t.Fatalf("indentation-significant languages %v", IndentationSignificantLanguages())
	}
	for _, language := range IndentationSignificantLanguages() {
		if _, ok := tokenClassTable[language]; !ok {
			t.Errorf("%s is declared indentation-significant but not a tokens language", language)
		}
	}
}

// A comment holding a character that is neither printable text nor plain
// layout is flagged directive, whatever its words: some compilers end a line
// there (tsc at U+2028 and U+2029, C# also at U+0085) while the grammar keeps
// one comment, so a consumer must compare it by text. Ordinary non-ASCII
// letters in prose are not such characters.
func TestDirectiveCommentsWithHiddenBreaksOrSpaces(t *testing.T) {
	hidden := []string{"\u2028", "\u2029", "\u0085", "\v", "\f", "\x01", "\x1b", "\u00a0", "\u2007", "\u3000", "\ufeff",
		// An invalid UTF-8 sequence (Go, Python and Rust reject the file).
		"\xc0\xaf", "\xff",
		// Bidirectional controls (Rust rejects them by default) and other
		// format characters.
		"\u061c", "\u200e", "\u200f", "\u202a", "\u202b", "\u202c", "\u202d", "\u202e",
		"\u2066", "\u2067", "\u2068", "\u2069", "\u200b", "\u200d",
		// A private-use code point.
		"\ue000",
	}
	forms := []struct{ path, before, comment, after string }{
		{"a.ts", "function f() { return ", "/* see the%snote below */", " 1; }\n"},
		{"a.cs", "class A { int x = 1; ", "// see the%snote below", "\n}\n"},
		{"a.go", "package p\n\n", "// See the%snote below.", "\nvar x = 1\n"},
		{"a.py", "x = 1  ", "# see the%snote below", "\n"},
		{"a.rs", "fn f() {} ", "// see the%snote below", "\n"},
	}
	for _, form := range forms {
		for _, char := range hidden {
			comment := fmt.Sprintf(form.comment, char)
			source := form.before + comment + form.after
			if !tokensAvailable(t, form.path, source) {
				t.Errorf("%s %q: no extracted record", form.path, char)
				continue
			}
			// A mask may rewrite the character (C# strips U+FEFF): the span is
			// then a rewritten row, flagged error, which is never decided by
			// code either.
			if rewrittenOver(t, form.path, source, comment) {
				continue
			}
			if !directiveFlags(t, form.path, source, comment) {
				t.Errorf("%s: comment holding %q is not flagged directive", form.path, char)
			}
		}
	}
	// Java: javac translates Unicode escapes before it reads comments.
	for _, comment := range []string{"// see the note \\u000a int y = 2;", "// see the note \\uuu000A here", "/* see \\u002a/ the note */"} {
		if !directiveFlags(t, "A.java", "class A {\n  "+comment+"\n}\n", comment) {
			t.Errorf("Java comment with a Unicode escape %q is not flagged", comment)
		}
	}
	// C: a trigraph; ??/ is a backslash in ISO modes, so the comment swallows
	// the next line.
	for _, comment := range []string{"// see the note ??/", "// a ??( b", "/* see ??= here */", "// ok ??! or ??-"} {
		if !directiveFlags(t, "a.c", "int x = 1; "+comment+"\nint y = 2;\n", comment) {
			t.Errorf("C comment with a trigraph %q is not flagged", comment)
		}
	}
	// An emoji sequence joined by a zero-width joiner is flagged (the joiner
	// is a format character): accepted over-flagging. A variation selector is
	// a mark, so an emoji with one alone is prose.
	if !directiveFlags(t, "a.ts", "// Family \U0001F468\u200d\U0001F469 today\nf();\n", "// Family \U0001F468\u200d\U0001F469 today") {
		t.Error("an emoji joined by U+200D is not flagged")
	}
	for _, prose := range []string{"// Café crème is served here.", "// \u65e5\u672c\u8a9e\u306e\u8aac\u660e \u3053\u3053\u306b", "// Ship it \U0001F680 today.", "// Naïve résumé of the flow.", "// Heart \u2764\ufe0f shown."} {
		if directiveFlags(t, "a.ts", prose+"\nf();\n", prose) {
			t.Errorf("prose %q with ordinary non-ASCII letters is flagged", prose)
		}
	}
}

// Under the consumer's rule the reviewers' pairs read as a change.
func TestHiddenLineTerminatorInCommentIsTokenChange(t *testing.T) {
	tsBase := "function f() { return /* see the note below */ 1; }\n"
	csBase := "class A { int x = 1; // see the note int y = 2;\n}\n"
	pairs := []struct{ path, base, head string }{
		{"a.ts", tsBase, "function f() { return /* see the\u2028note below */ 1; }\n"},
		{"a.ts", tsBase, "function f() { return /* see the\u2029note below */ 1; }\n"},
		{"a.cs", csBase, "class A { int x = 1; // see the note\u2028int y = 2;\n}\n"},
		{"a.cs", csBase, "class A { int x = 1; // see the note\u2029int y = 2;\n}\n"},
		{"a.cs", csBase, "class A { int x = 1; // see the note\u0085int y = 2;\n}\n"},
		// An invalid UTF-8 sequence, a bidirectional control, a C trigraph.
		{"a.go", "package p\n\n// see the note\nvar x = 1\n", "package p\n\n// see the \xc0\xaf note\nvar x = 1\n"},
		{"a.rs", "// see the note\nfn f() {}\n", "// see the \u202e note\nfn f() {}\n"},
		{"a.c", "int x = 1; // see the note\nint y = 2;\n", "int x = 1; // see the note ??/\nint y = 2;\n"},
	}
	for _, p := range pairs {
		if tokensAvailable(t, p.path, p.head) && consumerNoTokenChange(t, p.path, p.base, p.head) {
			t.Errorf("%s: %q read as no token change", p.path, p.head)
		}
	}
}

func rewrittenOver(t *testing.T, path, source, part string) bool {
	t.Helper()
	start := strings.Index(source, part)
	end := start + len(part)
	for _, row := range fileTokensFor(t, path, source).Leaves {
		if row[1] < end && row[2] > start && row[5]&tokenFlagRewritten != 0 && row[5]&tokenFlagError != 0 {
			return true
		}
	}
	return false
}
