package sem

import (
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
