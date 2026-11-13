# Snapshot format

This page is the canonical description of the NDJSON artifacts emitted by
`snapshot`, `symbols`, and `edges`, the compact snapshot variant, and the
schema compatibility rules consumers must follow. The schema contract itself
is [ADR 0001](adr/0001-ga-schema-contract.md).

## Schema versioning

Provider output uses `schema_version` in `major.minor` form.

- Consumers refuse unknown major versions.
- Consumers may ignore unknown fields within a supported major version.
- If the provider emits a newer supported-major minor version, consumers
  should warn that some facts may have been skipped.
- Unknown relation types should use an extension namespace, such as
  `X-provider-name:RELATION`.

Provider records are typed. Representative data records:

```json
{"record_type":"file","id":"gh/org/repo:file:internal/auth/token.go","path":"internal/auth/token.go","blob":"..."}
{"record_type":"external","id":"external:import:net/http","kind":"import","value":"net/http"}
{"record_type":"symbol","id":"...","kind":"function","name":"ValidateToken"}
{"record_type":"relation","from_id":"...","to_id":"...","type":"CALLS"}
```

Relation endpoints may point to file records, symbol records, or external
endpoint records. Consumers should ignore unknown record types within the
supported major schema version, and should not assume every relation target
is a symbol.

## Static callable ownership

Graph relationships describe static source dependencies. `resolution: exact`
means that the target symbol identity was resolved exactly; it does not prove
that a call executes, executes synchronously, or executes in every invocation.

| Source shape | Symbol and ownership | Observed relationship |
| --- | --- | --- |
| `const api = { stop: () => helper() }` | Named object `api` contains method `api.stop` | `api.stop CALLS helper` |
| `return { stop: () => defer(function () { helper() }) }` inside `make` | `make` lexically contains property method `make.stop` | `make.stop CALLS defer` and `make.stop CALLS helper` under the existing named lexical source policy |
| `const api = { value: initialize(), stop: () => helper() }` | The object initializer and the named property are separate sources | `api CALLS initialize`; `api.stop CALLS helper` |

An anonymous callback without its own symbol stays within the nearest named
callable's source dependency observation. This preserves useful lexical context
but cannot establish that the enclosing callable invokes the callback. Existing
named enclosing functions can also retain nested source observations; this is
not an exhaustive runtime call graph.

Named JavaScript/TypeScript function declarations can supply a direct self
relationship from the existing shared AST scope scan. This bounded correction
does not imply universal recursion coverage across language adapters.

| Source observation | Self relationship |
| --- | --- |
| A direct invocation in the declaration's own body, with its original binding visible | One `CALLS` record to the same symbol ID; evidence points at the first proven invocation, not an exhaustive call-site inventory |
| Declaration header, construction, callback reference or nested callable's invocation | Does not prove a direct self call owned by this declaration |
| A shadowing binding or an explicit visible assignment/update to the name, including wrapped targets and destructuring leaves | No new exact self attribution; no value-flow or execution-order inference |
| Syntax-error recovery, parse timeout or depth-truncated scope state | No new exact self attribution; original diagnostics remain visible |
| A site under `with` dynamic-scope ambiguity | That site cannot prove the original binding; a later unshadowed site outside it can still supply the witness |

The proof does not cover named expressions, arrow bindings, receiver/property
recursion or namespace declaration merging. Other adapters retain their existing
resolution policies and known omissions; absent output is not proof of absent
source dependency. Symbol IDs, schema and static-dependency meaning stay unchanged.

A property's qualified name on an anonymous object records its lexical context.
For example, a returned object inside a class's construction expression can
produce `Service.stop`; this is not a declared static method of `Service`.
`CONTAINS` makes that lexical context queryable. Such anonymous-object properties
are excluded from named receiver resolution, and function-local named objects
must remain within their lexical source scope. Statically named objects such as
`api` can supply actual receiver paths. Computed property names and indirect
callable values remain outside this bounded extraction.

## Call-site evidence

A `CALLS`, `CONSTRUCTS` or `ASYNC_CALLS` relation made by a call carries one
evidence entry of kind `call_site` (a call resolved to a symbol),
`imported_call_site` (a call resolved to an imported external symbol),
`async_call_site` (an awaited, spawned or `go` call) or `top_level_call_site`
(a call made at file level, outside every symbol's source, which includes every
call inside a test callback such as `test("...", () => { subject() })`). Its
`detail` is the callee as written there (`helper`, `store.save`,
`new Store().save`).

What the entry's `start_line` and `end_line` mean depends on the caller's
language. For a caller whose file is in a language that
`feature_languages.call_site_lines` lists (Go, JavaScript, Python and
TypeScript), the same for all four kinds:

- Both are one line: the line of the caller's file where the callee's name is
  written in the call that produced the relation. For a call spanning lines
  (`store\n  .save()`, `fn(\n  arg,\n)`) that is the line of the callee's
  name, the one a reader needs.
- When the caller writes that spelling more than once, it is the first in
  source order. One relation per caller, target and family stays the rule; a
  target the caller reaches through two spellings (`helper()` and
  `this.helper()`) keeps the entry the engine emitted first, with that
  spelling's line.
- A symbol caller's line lies inside the symbol's span. A file-level caller's
  line is outside every symbol's source.
- Both are absent when the engine cannot tell where the call is: the text scan
  found the name in something that is not a call (a method declaration or
  signature, a word in template or JSX text, a regular expression, a private
  member call `this.#name(` that a bare name cannot resolve to with certainty,
  a Python or Go name whose `(` opens the next statement). The relation is
  kept; its entry never carries the caller's span in its place.
- `form`, when present, is `reference`: the target's name is handed over at
  that line as a value (`xs.map(double)`, `useCallback(handler)`) and not
  called there. It is set only when the caller never calls the target; a
  caller that both passes and calls it reports the first call, unmarked.
  Absent, a call or construction is written at that line. In TypeScript and
  JavaScript the only shape that gives a call relation without a call is a
  name passed as a call argument, which is the one marked; in Python and Go no
  scanner turns a name handed over as a value into a call relation, so an
  unmarked entry is a call in all four.

For a caller in any other language the entry keeps the earlier meaning: a
call from a symbol carries the caller's own span (`start_line` to `end_line`
of the calling symbol, not of the call), a file-level call carries no line,
and `form` is never set. A consumer treats an engine without capability
`call_site_lines`, and a language it does not list, as giving no call line.
Relations of kind `route_endpoint_match` and `graphql_schema_resolver_match`
are not calls written in the caller and are unchanged. See
[ADR 0013](adr/0013-call-site-lines.md).

## Streaming NDJSON

The `snapshot`/`symbols`/`edges` commands stream records to stdout as they are
produced, so the stream does not hold full relation payloads, their evidence,
or file contents in memory. Peak memory is bounded by the symbol/index
metadata plus the relation dedup set, which holds one compact 64-bit key per
unique relation. The stream is emitted in this order:

1. exactly one header line (a record with `schema_version`),
2. `file` records, then `symbol` records (emitted per file as parsing
   progresses, before any relation is resolved),
3. `relation` records and the `external` endpoint records they reference,
4. exactly one trailing `summary` record (`record_type: "summary"`).

**The first header is intentionally lean.** It carries identity (`provider`,
`provider_version`, `repo_root`, `repo_key`, `commit`, `tree`),
`schema_version`, `capabilities`, `schema_features`, `language_versions`, and
the **profile metadata** — `profile`, `profile_limits`, `relation_set`, and
`skipped_relation_families`. Its `languages`, `language_tiers`, `warnings`,
`partial_failures`, `stats`, and `completeness` are empty/zero — those totals
are not known until the whole repository has been processed, and the header
is emitted before that so consumers can begin work immediately. The profile
metadata is **header-only**: it is known up front and is therefore not
repeated in the summary.

**`commit` and `tree` name the source that was read.** By default that is
HEAD's commit and root tree. With `--commit <exact-commit-OID>` they are the
target commit and its tree, read from Git objects without a checkout; nothing
in the stream (files, symbols, relations, `FILE_CHANGES_WITH` co-change
history, TypeScript `tsconfig.json` aliases, nested `.gitignore` rules) comes
from HEAD or the working tree. Both fields are empty for a `--worktree` stream.
See [ADR 0010](adr/0010-exact-commit-bulk-streams.md).

**`repo_key` is the symbol-ID namespace, not a repository identity.** Remote
URLs are checked in the provider's established compatibility order: the last
configured `remote.origin.url` is checked first, then every non-origin remote
URL in Git config order. The first URL in that order matching a supported
github.com form yields `gh/<owner>/<name>`. A repository with no such URL — no
remote, gitlab, bitbucket, self-hosted — instead yields `local/<basename>`,
which is **not** globally unique. Two unrelated repositories whose directories
are both named `tools` publish the same `local/tools` in their headers and in
every symbol ID. A consumer must therefore use `repo_key` as a necessary and
not a sufficient identity test: a mismatch proves a foreign snapshot, a match
does not prove a native one. Pair it with `repo_root`, `commit` and `tree` — or
with the consumer's own storage key — for identity. `graph doctor --json`
advertises the same `repo_key` and `repo_root` before a snapshot is built, so
the pair can be checked up front.

**Symbol ID fields are not escaped, so an ID must never be split positionally.**
An ID is `<repo_key>:<language>:<file_path>:<kind>:<qualified_name>` joined with
`:`, and nothing escapes the fields. Two of them can carry a `:` of their own: a
`local/<basename>` key inherits whatever the directory is called, and a file path
may contain one on any POSIX filesystem. `local/weird:name:Python:od:d/mod.py:class:Cache`
is a well-formed ID that splits into seven fields, not five. The trailing fields
already do this routinely — `external:import:std::collections::HashMap`. A
consumer that reads `split(id, ":")[2]` as the path therefore mis-attributes
every record in such a repository, silently and without an error.

The four safe reads, all of which entire-graph uses internally and pins in its
own tests: compare a whole ID; anchor on the LAST separator for a trailing
segment; cut at the FIRST separator only inside the `external:<kind>:<value>`
namespace, whose kind cannot contain a `:`; and take a file path from the
record's own `file_path` field rather than from the ID.

**The final `summary` record is authoritative for aggregate metadata.** It
carries the real `languages`, `language_tiers` (each present language
classified `semantic` or `inventory-only`), `warnings`, `partial_failures`,
`stats` (including the `relations` count and `completeness_level`), and the
`completeness` breakdown. It does **not** carry profile metadata; consumers
should read that from the lean header and must not expect it in the summary
unless a future schema version adds it.

**Merging the two.** A consumer that wants one fully populated header should
take the lean header and overlay the summary's aggregate fields (`languages`,
`language_tiers`, `warnings`, `partial_failures`, `stats`, `completeness`) on
top of it — summary wins for any field both records carry, the header wins
for the profile metadata the summary omits. For any aggregate total,
including per-language tier, read the summary, never the lean header.

**Ordering.** For a fixed input and profile the stream is deterministic and
stable (file, symbol, and relation order are reproducible across runs), but
it is not globally sorted. Consumers should key on record `id`/identity, not
on stream position.

**Unknown record types.** Consumers must ignore record types they do not
recognize within a supported major schema version, and must not assume every
line is a known type. A consumer that reads only the header and relations
should skip `file`, `symbol`, `external`, and `summary` lines rather than
erroring on them.

## Source omission warnings

The `snapshot`, `symbols` and `edges` summaries name source that the listing
saw but the stream did not read, so a consumer does not read "no records" as
"nothing there". Both are summary `warnings` with severity `info`, not
`partial_failures`: they leave `stats.partial_failures`,
`stats.completeness_level` and `completeness.health` unchanged.

| Code | One warning per | `file_path` | `detail` |
| --- | --- | --- | --- |
| `W_SUBMODULE_NOT_READ` | Gitlink (mode 160000) in the committed tree that was read, unless the ignore or vendored-path rules exclude its path | The gitlink's path | `gitlink pins commit <OID>` |
| `W_GO_BUILD_EXCLUDED` | Go file outside the host's default build (build constraint or `_GOOS`/`_GOARCH` file-name suffix); it has no file record, symbols or relations | The Go file's path | The host `GOOS/GOARCH` and which rule excluded it |

A `--worktree` stream does not report gitlinks. Go exclusion depends on the
host that built the snapshot, as it always has. Capability
`optional_local_only_features.source_omission_warnings` feature-detects both
codes: on an engine without it, an absent warning proves nothing. Interactive
query surfaces (`query`, `neighbors`, `def`, `impact`) do not add these
warnings to their diagnostics.

## File tokens records

`symbols --outline --tokens` adds one `file_tokens` record right after each
`file_outline` record (here for `const x = 1;\n`). A file with no outline
(inventory-only, a parse error, over the parse size limit, minified, depth
exceeded) has no tokens record either; its partial failure says why. A tree
that holds an `ERROR` or missing node gets no outline today, so `has_errors` is
false on every record this build emits; the field and the leaf `error` flag are
kept for the contract.

`status` says whether the leaves were extracted. `extracted`: every field
below is present. `language_not_covered`: the language is not declared in
`feature_languages.symbols_outline_tokens` (see the table below); the record
has `record_type`, `file_path`, `language`, `status`, `has_errors` and
`import_bindings: "not_extracted"` and nothing else. `over_size_limit`: the
file is larger than 524,288 bytes (512 KiB) as given, or its tree has more than
100,000 leaf rows; the same fields only, and the file's `file_outline` record is
unchanged. Ordinary code runs to about 6 to 11 output bytes per source byte, but
dense tables reach 23, so the row limit is what bounds a record: with at most
about 45 bytes a row it stays under a consumer's 8 MiB per-file cap, and the
byte limit bounds the read's memory.

```json
{"record_type":"file_tokens","file_path":"a.ts","language":"TypeScript","status":"extracted","has_errors":false,
 "identifier_names":["x"],"leaf_types":["const","identifier","=","number",";"],
 "ancestors":["3b361d34bcd674d7","5fa142ba74908a4f","75261478695a6d99"],
 "leaves":[[0,0,5,1,1,0,1,2],[1,6,7,1,1,9,2,1],[2,8,9,1,1,0,2,0],[3,10,11,1,1,8,2,0],[4,11,12,1,1,0,1,0]],
 "import_bindings":"extracted","imports":[]}
```

`leaves` holds every leaf of the syntax tree, whole file, in source order, each
once, named and anonymous alike, as rows of eight integers: index into
`leaf_types`, start byte, end byte (exclusive), start line, end line (1-based;
lines count `\n` only), flags, index into `ancestors`, and opens. Flags:
`1` identifier, `2` comment, `4` error (missing node or under `ERROR`, or a
rewritten span), `8` named node (otherwise an anonymous token: keyword,
operator, punctuation), `16` rewritten, `32` directive (a comment leaf, also
flagged `2`, of a comment that a compiler or a tool reads or may read; see
below). Flag bits are additive: a reader ignores bits it does not know, and a
bit never changes meaning. `ancestors` entries are the first 16
hex characters of SHA-256 over the chain from the root to the leaf's parent,
each node encoded as `n` or `a` (named or anonymous), its type's byte length as
an unsigned varint and the type's bytes. `opens` counts the ancestors whose
first row this is. Equal types, texts, ancestors and opens over a run of rows
mean the same piece of tree; `return` before a line break differs from
`return x` in ancestors, not tokens. Source positions the program can
observe (`__LINE__`, `line!()`, `column!()`, stack traces) change with layout,
and no leaf shows that.

Text a node owns between its children with no node of its own becomes one row
typed with that node, whose parent is that node, covering exactly those bytes
(never trimmed). Inside a content node (a string, raw string, char, template,
heredoc body or similar, listed per language below) every such byte is a row,
whitespace included, so `" "` and `""` differ, and so is every byte of every
node inside it: interpolations and format clauses are content too (Python's
`f"{x = }"` prints the spaces, C#'s `{x,10:F2}` alignment and format carry
them). Only TypeScript and JavaScript, whose template substitutions are plain
expressions, treat whitespace inside `${…}` as layout, each with the test pairs
that show it; a literal inside a substitution is still content. For any other
node a gap made only of space, tab, carriage return and line feed is layout and
has no row; a gap holding any other byte (form feed, vertical tab, and every
non-ASCII space: U+00A0, U+2028, U+2029, U+3000, a byte-order mark after byte
0) is a row of its own, typed with the node that owns the gap, so a change to
it reads as a token change. With that rule the rows tile the file: every byte
is in at most one row, a byte in no row is a space, tab, carriage return or
line feed outside every content node, and every literal byte is in a row.
Between two consecutive rows, and before the first and after the last, there
is nothing but space, tab, carriage return and line feed (and a byte-order mark
at byte 0), so a consumer can compare the gaps from the rows' positions alone. A
byte-order mark at byte 0 is never a row. (The JavaScript grammar itself reads
U+2028 and U+2029 as line ends, so `return`, U+2028, `x` already parses as
`return` before a line break.) Strings and templates are one row per
fragment as the grammar gives them, neither identifier nor comment.

Some languages are parsed through a same-length rewritten view of the file
(masks for constructs the grammar rejects). Where the view's bytes differ from
the file's, the rows over that span become one row of type `""` flagged
rewritten and error; identifier names inside it come from a second parse of
the file as given.

`identifier_names` is the sorted set of texts of every identifier row. A leaf
is an identifier when it is a named node of a listed type and lies under no
comment node; it is a comment when it or an ancestor is a listed comment node.
Keywords are anonymous tokens in these grammars (SQL's `keyword_*` nodes are
named and not listed), so a contextual keyword used as a keyword is never an
identifier and one used as a name is. Python docstrings are strings. A JSX
`{/* x */}` gives a comment row; its braces are punctuation rows.

A language is declared only when a test proves, for each of its string-like
forms (every quote style, raw and multi-line forms, heredocs, templates, byte
and char literals), that a whitespace change inside the literal changes the
non-comment leaves (count, type, text, ancestors, opens), the all-whitespace
case included, while layout outside literals does not. These are the declared
languages; every other parsed language says `language_not_covered`. Lua and
Swift were tried and fail the layout half (their grammars fold or read spacing
around `=` as part of a token). PHP is not declared: whitespace-only text
outside its tags is output and has no node. Kotlin is not declared: its grammar
parses `a` newline `- b` and `f` newline `(a)` as one expression, though a
Kotlin line break ends the statement there.

Whitespace outside literals, per declared language, as the consumer's rule
reads it (any "change" answer is the safe direction). This table is not
exhaustive: see [What the leaves do and do not
establish](#what-the-leaves-do-and-do-not-establish).

| Language | Checked | Reads as |
| --- | --- | --- |
| Python | Re-indent that changes nesting; consistent re-indent of a whole block; blank lines; a backslash line continuation | Change; no change (same meaning when written with spaces; with tabs it can raise `TabError`); no change; change |
| TypeScript, JavaScript | JSX text between expressions, added or reflowed; a line break inside an expression; `return` before a line break | Change; no change (no ASI there); change |
| Go | A blank line between statements | Change (the statement-ending line break is a token) |
| C | `#define A(x)` against `#define A (x)`; spacing in a macro body; a backslash continuation; spacing in `#include < >` | Change in each |
| Ruby | `foo -1` against `foo - 1`; `foo[1]` against `foo [1]`; `a` newline `- b`; a leading-dot chain; `%w(a b)` spacing; text after `__END__`; heredoc terminator indent; an indented `=begin` or `=end` | Change; change; change; no change (same meaning); no change (same value); change; change; change (every embedded-document comment is flagged `directive`, so the whitespace around it and its text are compared; the grammar takes an indented `=begin` or `=end` as part of a comment while Ruby rejects the first and leaves the second unterminated) |
| Elixir | `a -1` against `a - 1`; `a` newline `- b`; `f` newline `(a)` | Change in each |
| Java | Text block indentation | Change |
| C# | A line break inside an expression; spacing in `#if` | No change (insignificant); change |
| Rust | Spacing inside a macro call (`stringify!(a b)`) | No change (tokens are what the macro sees) |

A comment that a compiler or a tool reads is flagged directive (`32`) on all
its leaves, in two layers; over-flagging is the safe direction, and the flag
is best effort, not complete (see [What the leaves do and do not
establish](#what-the-leaves-do-and-do-not-establish)).

1. Known directives, per language, matched anywhere in the comment's text:
   Go `//go:` (any, written without a space), `+build`, `//line`, `//export`,
   `//extern`, `nolint`, `lint:ignore`, `nosec`, `// want` (analysistest),
   `// Code generated … DO NOT EDIT.` (the `go/ast.IsGenerated` form), every
   comment in a group holding an example's `// Output:` or
   `// Unordered output:` line (the expected output), and every comment in the
   group directly above `import "C"` (the cgo preamble; a group is comments
   separated by at most one line break);
   TypeScript and JavaScript `@ts-ignore`, `@ts-expect-error`, `@ts-nocheck`,
   `@ts-check`, `<reference`, `<amd-module`, `<amd-dependency`, `@jsx`,
   `@jsxImportSource`, `@jsxRuntime`, `@jsxFrag`, `__PURE__`,
   `__NO_SIDE_EFFECTS__`, `webpack`, `@vite-ignore`, `# sourceMappingURL`,
   `# sourceURL`, `eslint`, `prettier-ignore`, `biome-ignore`,
   `istanbul ignore`, `c8 ignore`, `v8 ignore`, `@license`, `@preserve`,
   `jshint`, `tslint:`, `global `, `@flow`, `$FlowFixMe` and a few more; Python
   `coding[:=]`, `pragma` in any case with or without a colon, `type:`,
   `noqa`, `pylint:`, `mypy:`, `fmt:`,
   `isort:`, `ruff:`, `pyright:`, `nosec`, `yapf:`; Ruby
   `frozen_string_literal`, `encoding`, `coding`, `warn_indent`,
   `shareable_constant_value`, `rubocop:`, `typed:`, `-*-`; Rust every doc
   comment (`///`, `//!`, `/**`, `/*!`, which rustdoc compiles as tests); C
   `NOLINT`, `clang-format`, `clang-tidy`, `NOTREACHED`, `lint`, and every line
   comment whose line ends in a backslash, even one followed by spaces or tabs
   (GCC and clang join the next line into the comment; tree-sitter-c does not);
   Ruby every embedded document (`=begin` … `=end`); in C, Java,
   JavaScript and TypeScript, fall-through comments in any case matching
   `fall(s|ing)?[ \t-]*thr(u|ough)` (`fallthrough`, `Fall through.`,
   `Falls through`, `fall thru`; GCC, javac and ESLint read them); C# every `///` XML doc
   comment (the compiler reads them under `-doc` or
   `GenerateDocumentationFile`), `ReSharper`, `<auto-generated`; Java
   `noinspection`, `CHECKSTYLE`, `NOSONAR`, `@formatter:`, `NOPMD`,
   `@deprecated` (javac reads it from javadoc); Elixir `credo:`,
   `coveralls-ignore`, `sobelow` (Elixir's compiler reads no comment: its
   documentation is the `@doc` and `@moduledoc` attributes, which are
   tokens). In every language: `noinspection`, `NOSONAR`, `DO NOT EDIT`,
   `@generated`. `#pragma` in C and C# is a preprocessor token, not a comment.
   The full lists are `directiveMarkers`, `directivePatterns` and
   `commonDirectiveMarkers` in `internal/sem/directives.go`.
2. Every other comment is flagged unless it reads as plain prose: after the
   comment marker (or a documentation opening the language defines, such as
   `/**`) comes at least one space or tab, and every non-empty line of the body
   (a block comment's lines without a leading `*`) begins with a word made only
   of letters (an inner apostrophe allowed), optionally ended by `.`, `,`, `;`,
   `?`, `!` or `)`, and followed by a space or the end of the line. A body of
   a single word is not prose, whatever the word (tool markers such as
   `nosec`, `nocov` or `skipcq` are often one word, and no list of them can be
   complete). So `// TODO: x`, `//nolint`, `// explain`, `# [tool]`, `// x=1`,
   `// 3rd` and an empty `/**/` are flagged, and `// This loop retries.` is
   not. A comment is also flagged, whatever its words, unless its text is
   printable text and plain layout only: valid UTF-8 in which every character
   is a letter, mark, number, punctuation or symbol (Unicode categories L, M,
   N, P, S), U+0020, tab, line feed or carriage return. Anything else flags
   it: an invalid byte sequence (Go, Python and Rust reject the whole file),
   a control character, a format character (a bidirectional control such as
   U+202E, a compile error in Rust by default; zero-width characters; U+FEFF),
   a separator other than the plain space (U+2028 and U+2029, where tsc ends a
   line, while C# also ends one at the control U+0085), a private-use or
   unassigned code point. The grammar keeps one comment in each of these
   cases, so the consumer must compare it by text. Accented and CJK letters
   and plain emoji are printable text; an emoji sequence joined by a
   zero-width joiner is flagged (the joiner is a format character), which is
   accepted over-flagging, while a variation selector is a mark and does not
   flag. In C a comment holding a trigraph (`??` followed by one of
   `= / ' ( ) ! < > -`) is flagged: `??/` is a backslash in ISO modes, so a
   line comment ending in one swallows the next line. In Java a comment
   holding a Unicode escape (`\u` with any number of `u`s and four hex
   digits) is flagged.

Capability `optional_local_only_features.symbols_outline_token_directives`,
with its languages in `feature_languages` (today the same as
`symbols_outline_tokens`), declares that the flag is set; a reader must not
read the flag's absence as "no directives" on an engine or language that does
not declare it.

A consumer that compares directive leaves by text and position among the
compared leaves sees `//go:build linux` against `//go:build darwin`, or
against `// go:build linux` (no directive with the space), as a change, and an
added prose comment as none. The Go grammar makes the line break that ends
a statement a token, so moving Go statements across lines can read as a token
change: the safe direction.

| Language | Identifier leaf types | Comment node types | Content node types |
| --- | --- | --- | --- |
| C | `field_identifier`, `identifier`, `statement_identifier`, `type_identifier` | `comment` | `char_literal`, `string_literal` |
| C# | `identifier` | `comment` | `character_literal`, `interpolated_string_expression`, `raw_string_literal`, `string_literal`, `verbatim_string_literal` |
| Elixir | `alias`, `identifier` | `comment` | `charlist`, `sigil`, `string` |
| Go | `blank_identifier`, `field_identifier`, `identifier`, `label_name`, `package_identifier`, `type_identifier` | `comment` | `interpreted_string_literal`, `raw_string_literal`, `rune_literal` |
| Java | `identifier`, `type_identifier` | `block_comment`, `line_comment` | `character_literal`, `string_literal` |
| JavaScript | `identifier`, `private_property_identifier`, `property_identifier`, `shorthand_property_identifier`, `shorthand_property_identifier_pattern`, `statement_identifier`, `type_identifier` | `comment`, `html_comment` | `jsx_text`, `regex`, `string`, `template_string` |
| Python | `identifier` | `comment` | `string` |
| Ruby | `class_variable`, `constant`, `global_variable`, `hash_key_symbol`, `identifier`, `instance_variable` | `comment` | `character`, `delimited_symbol`, `heredoc_body`, `regex`, `string` |
| Rust | `field_identifier`, `identifier`, `metavariable`, `shorthand_field_identifier`, `type_identifier` | `block_comment`, `doc_comment`, `inner_doc_comment_marker`, `line_comment`, `outer_doc_comment_marker` | `char_literal`, `raw_string_literal`, `string_literal` |
| TypeScript | `identifier`, `private_property_identifier`, `property_identifier`, `shorthand_property_identifier`, `shorthand_property_identifier_pattern`, `statement_identifier`, `type_identifier` | `comment`, `html_comment` | `jsx_text`, `regex`, `string`, `template_string` |

`import_bindings` is `extracted` for TypeScript, JavaScript, Go and Python, with
`imports` listing one object per imported specifier: `start_line`, `end_line`,
`start_byte`, `end_byte` (the statement's range in the bytes as given, end
exclusive like a leaf's, and the lines are that range's lines), `specifier` (as written, without quotes; Python relative modules keep their
dots), `bindings` (`imported`, `local`, `type_only`, and `local_inferred: true`
for a Go import without a name, whose local name is the one goimports assumes
from the path), `side_effect_only` and `names_unknown`. `imported` is the
imported name, `default`, `*` for a whole module under one name, or `=` for
TypeScript `import x = require("y")`. `type_only` is set by `import type`,
`import typeof` and per-specifier `type`. Go's grouped `import (...)` gives one
object per spec; Python's `import a, b` gives one per module. Names and
specifiers are built from the leaves that spell them, never from raw text: a
Python dotted name is its identifier leaves joined with `.` (so
`import os . path`, a name split by a backslash continuation and one split
across parentheses with comments all give `os.path`-style names), a relative
module is its dots followed by that name, and a binding's `imported` and
`local` are the text of an identifier leaf (or, for an imported name written
as a string, that string's content); an inferred Go local is the exception
(`local_inferred`).

The range starts at the start of the statement's first non-comment leaf and
ends at the end of its last non-comment leaf, the statement being the
grammar's import statement node. So a leading or trailing comment is outside
even where the grammar puts it inside the node (`import a from 'a' // tail`
gives `import a from 'a'`), a comment in the middle of the statement stays
inside, code before or after the statement on the same line is outside, and so
is the `;` of Python's `import os; import sys`, while TypeScript and JavaScript
keep the statement's own `;` and its import attribute clause
(`with { type: "json" }`) inside. For Go a declaration with
one spec and no parentheses (`import "fmt"`) is the statement, keyword
included; in a grouped `import ( … )` each spec alone is a statement, so the
`import` keyword and the parentheses belong to no statement. Python's
`import a, b` gives both objects the whole statement's range. Re-exports
(`export … from`), `import x = A.B` aliases, dynamic `import()` and `require()`
calls are not import statements. The `specifier` is the text as written, and
every statement of an `extracted` record, except one whose specifier is empty,
has an import target entry under it on an `IMPORTS` relation of the same file
(see [Import targets](#import-targets)). Python's `from m import n` also gives `m.n`, and
`from . import x` gives `.x`, whose relation carries the entry for `.` itself.
Every other language
says `not_extracted` and has no `imports` field. Capabilities `optional_local_only_features` keys
`symbols_outline_tokens` and `symbols_outline_import_bindings`, with their
languages in `feature_languages`, declare both.

## What the leaves do and do not establish

Equal leaves (type, text, `ancestors`, `opens`) mean the same tokens in the
same tree-sitter tree. They do not mean the same behaviour:

- The compiler's grammar can differ from the tree's. Where a line break
  follows certain keywords (TypeScript `abstract`, `accessor`, `asserts`,
  `using`, `interface`; JavaScript `async` before an arrow function), the
  compiler starts another statement while tree-sitter gives the same tree.
- Position matters for some comments: a directive that must start its line
  (`//go:generate`, `//line`), a comment group that must touch the next
  declaration (the cgo preamble above `import "C"`), a coding cookie that must
  be on line 1 or 2, a shebang that must be at byte 0.
- Indentation written with tabs matters where the language counts columns
  (Python raises `TabError`; tree-sitter counts a tab as eight columns).
- Leading whitespace is syntax in the languages listed under
  `feature_languages.symbols_outline_indentation_significant` (capability
  `optional_local_only_features.symbols_outline_indentation_significant`;
  today `["Python"]`). The Python tree does not check it: an inconsistent
  dedent, a line indented deeper than its block with no line opening one, and
  mixed widths inside one block all parse without error and give the same
  leaves, though CPython raises `IndentationError`. In these languages a
  consumer must not treat a change in a line's leading whitespace as layout
  unless the bytes are identical. The other declared languages are free-form;
  their column-sensitive forms (a Ruby `=begin`, `__END__` or heredoc
  terminator, an Elixir heredoc's closing delimiter) either change the tree or
  lie inside a content node.
- Source positions the program can observe (`__LINE__`, `line!()`, stack
  traces) change with layout, and so does source text the program can read
  (`Function.prototype.toString()`, Python's `inspect.getsource()`).
- Unconfirmed risks for Ruby, found in review and not checked on a current
  Ruby: a blank line before a leading-dot method chain (`x = [1]`, a blank
  line, `.size`), or a comment line in the same place (which Ruby 2.6
  rejects), may change how Ruby parses it, while the rule above reads it as
  layout.
- Identifier flags follow the grammar's node types, not the language's
  keyword list: Rust's `Self` is flagged identifier although it is a keyword,
  and Go's predeclared `nil`, `true`, `false` and `iota` are not flagged
  identifier although a program can redeclare them.
- Java translates Unicode escapes before it reads comments, so
  `// … \u000a code` turns the rest of the line into code. The tree does not
  model that; such comments are flagged directive.

Literals whose value depends on where the closing delimiter sits (Java text
blocks, C# raw strings, Elixir heredocs and sigils, Ruby heredocs, and every
multi-line literal) keep the delimiter's indentation inside their content node,
so moving it, or reindenting the whole block, changes a leaf.

A consumer that wants to decide that a change is layout only must, besides
equal leaves, compare the stretches between consecutive compared leaves, and
the file's leading stretch (before the first) and trailing stretch (after the
last). A stretch is everything between the two leaves, the bytes of any
comment left out of the comparison included. The stretch's line-break test is
about line feeds only; a comment holding any other line terminator (U+2028,
U+2029, U+0085, a form feed or a vertical tab) is flagged directive, so it is
compared by text rather than left in a stretch. Each stretch keeps its class
(nothing, spaces on one line, or at least one line break), and a line feed
anywhere in the stretch, inside a comment too, makes it a line break: a block
comment that spans a line ends a statement in TypeScript
(`function f(){ return /* see the note below */ 1; }` with a line break inside
the comment parses as `return;` in tsc, and the value is lost) and in Go (the
same `return /* … */ 1` fails with "not enough return values"), while both
grammars give equal leaves and the comment, being prose, is not flagged. The
JavaScript grammar already sees that break, so there the leaves differ. A
stretch holding a tab or carriage return is byte-identical, and the stretches
around every directive-flagged comment are byte-identical. The rows' positions
and the file's bytes are enough to compute this, since outside rows a stretch
holds only space, tab, carriage return and line feed.

Every import statement of an `extracted` record has an import-target entry,
except one whose specifier is empty (see [Import targets](#import-targets)),
so a statement's target can be read next to its bindings.

The `directive` flag is over-inclusive by design and still not complete. A
comment that is not flagged may be read by a tool: an analysistest
expectation written in an unusual form, a project's own `// lint-allow`
convention, a template engine's marker that reads as prose. A consumer must
not conclude from an unflagged comment that no tool reads it; a change to a
comment needs a reading.

## Import targets

In `snapshot` and `edges` streams (every format) every `IMPORTS` relation carries,
after its existing evidence, one entry per specifier that reached its target,
saying where that specifier resolves in the snapshot's own tree:

| `kind` | `file_path` | Meaning |
| --- | --- | --- |
| `import_target_file` | The resolved file | A file in the tree |
| `import_target_declared_package` | The declaring manifest | Named in a package manifest of the tree |
| `import_target_builtin` | (none) | The runtime's or standard library's own module |
| `import_target_unresolved` | (none) | Graph looked and found nothing |
| `import_target_not_classified` | (none) | Graph does not classify this specifier |

`detail` is the specifier (see [where specifiers come
from](#where-specifiers-come-from)), and the entries carry
no lines. Each entry classifies its own `detail`. All entries on one relation
share one kind, except that the entry for a Python relative `from` module
(`.`, `..`) rides on the relation of its first imported name and has its own
kind (`import_target_file` for the package's `__init__.py`, else
`import_target_unresolved`); a statement that names none (`from . import *`)
makes the module a specifier with a relation of its own. Several entries appear
only when several specifiers of one file reach one file (`./util` and
`./util.ts`); the relation, which keeps its first specifier's
`import_statement` evidence as before, lists each. The `fast` profile, which
drops evidence, keeps these entries. Relations, their ids and every other field
are unchanged; manifests are read from the snapshot's tree through its content
reader, so `--commit` classifies against that commit. Interactive and search
snapshots (`query`, `neighbors`, `impact`, `def`) do not carry the entries;
they carry the same `IMPORTS` relations.

| Language | Declared | Builtin | Otherwise |
| --- | --- | --- | --- |
| TypeScript, JavaScript | The package (`@scope/name` or first segment) is in `dependencies`, `devDependencies`, `peerDependencies` or `optionalDependencies` of the nearest `package.json` at or above the file, or its `@types/` package is; or it is the `name` of a `package.json` in the tree | Exact names only: Node 24's core modules with or without `node:` (and the `node:`-only `sea`, `sqlite`, `test`, `test/reporters`), and Bun's `bun`, `bun:ffi`, `bun:jsc`, `bun:sqlite`, `bun:test`; an unknown name under `node:` or `bun:` is unresolved | Unresolved, including a root `tsconfig.json` alias with no target file and an alias from any other `tsconfig.json`, which the engine does not follow |
| Go | Within the module path of the nearest `go.mod`, or of a module it requires | `C` (cgo), and the importable standard-library packages of Go 1.27.0 (`go list std` without `internal/` and `vendor/` paths, which code outside the standard library cannot import, plus `syscall/js`, the one package `go list std` lists only under a build constraint across every `go tool dist list` target) | Unresolved, including a typo of a standard package and standard `internal/` or `vendor/` paths |
| Python | The top-level module equals, under PEP 503 normalization, a distribution in the nearest directory's `pyproject.toml` (`[project]` dependencies and optional dependencies, `[dependency-groups]`, Poetry tables) or `requirements*.txt`, or the project's own name | A standard top-level module (CPython 3.14's names plus those removed since 3.11), or a dotted name that is itself a CPython 3.14 standard module (aliases such as `os.path` and `collections.abc` included) | Relative, or a dotted name that is no standard module, a mistyped submodule (`os.pathh`) as much as `pathlib.Path` from `from pathlib import Path` (that statement's own `pathlib` entry is builtin): unresolved; bare: not classified, since a distribution's import name need not be its own name |
| Any other | | | A file target says `import_target_file`; any other target is not classified |

A TypeScript or JavaScript `"."` or `".."` names a directory as `"./"` and
`"../"` do: each resolves to that directory's index file
(`index.ts`, `index.tsx`, `index.js`, `index.jsx`), and without one is an
unresolved relative import. Such an unresolved `"."` or `".."` relation now
carries the `UNRESOLVED_RELATIVE_IMPORT` warning, which it did not before
these two specifiers were read as relative.

Packages of a nested Go module resolve to files only for the root `go.mod`'s
module today, so such an import says declared by the nested `go.mod`.
Capability `optional_local_only_features.import_target_classification`, with its
languages in `feature_languages`, declares it.

### Where specifiers come from

A file's specifiers are its import scanner's, followed by each specifier its
syntax tree's import statements give that the scanner did not, in statement
order (TypeScript, JavaScript, Go and Python). The statements are the ones its
tokens record carries, read from the same parse whether or not `--tokens` is
asked for. So a multi-line import (`import type` included), a Go spec beside a
comment or with a raw-string path, every module of Python's `import a, b` and
a Python name spaced around its dots each have a relation and an entry. The
scanner also reads forms that are not import statements (`require()` and
dynamic `import()`); they keep their relations. A re-export (`export … from`)
has no `IMPORTS` relation. A Python `from`
statement gives what the scanner gives for it: the module, `m.n` for each name
of an absolute module without a dot, and, for a bare relative module, `.n` for
each name. A specifier both read has one relation and one entry. The
scanner's relations come first, in their order and unchanged, except that a
tree specifier reaching a file an earlier specifier reached adds its entry to
that relation, as a second scanner specifier does. An added external relation's
`reason` is `import statement read from the syntax tree`; the scanner's keep
theirs.

Resolution and classification go by the file's language. A script routed to
its language by its shebang (Python or Node, with no extension the engine
knows, such as `bin/tool`) resolves and classifies as a `.py` or `.js` file in
the same directory would; before, it had no `IMPORTS` at all.

The tree is read only when it has no error and the parse completed. A file
with a syntax error, over the parser's input limit or minified keeps the
scanner's specifiers alone, as does every other language. Capability
`optional_local_only_features.import_targets_cover_statements`, with its
languages in `feature_languages`, declares the promise: for a file whose
tokens record says `import_bindings` is `extracted`, every statement's
specifier has an import-target entry on an `IMPORTS` relation of that file in
the snapshot of the same commit, at the `fast` and `full` profiles. It has one
exception: a statement whose specifier is empty (Go's `import ""`) names
nothing and has no entry. TypeScript and JavaScript records list no statement
with an empty specifier, and Python's grammar has none. The promise does not
reach:

- a file with no tokens record (a tree with an error);
- a file whose record is `over_size_limit`, which lists no statements (its
  relations still gain the tree's specifiers);
- a `syntax-only` snapshot, which has no `IMPORTS` relations.

On the consumer's reference ranges (TypeScript, every changed blob side) every
statement has an entry: 1,271 at 25 commits, 3,019 at 100 and 6,969 at 420.
See [ADR 0012](adr/0012-imports-from-the-tree.md).

## Compact snapshot NDJSON v1

`snapshot --format ndjson` remains the default interoperable object stream.
`snapshot --format compact-ndjson` is a public, full-snapshot-only artifact
with a separate cache mode, `snapshot:compact-ndjson-v1`; it is rejected for
`symbols`, `edges`, and targeted `--to`/`--from`/`--relation` output. Its
first line is `["h", 1, header]`, and the version appears nowhere else.
Deterministic first-seen dictionary lines `d` precede positional `f` (file),
`x` (external), `s` (symbol), and `r` (relation) rows; a trailing `m` summary
is mandatory. A v1 relation row has the original 11 fields, plus an optional
twelfth `evidence_dropped` integer when the value is nonzero. Each evidence
entry is `[kind, file_path, start_line, end_line, detail]` (strings as
dictionary indexes, an absent line as `0`), plus, from schema 1.4, an optional
sixth `form` element when the entry has one; current readers accept five or
six. For artifacts that
declare the current or an older supported schema minor, consumers accept either
relation arity and reject every other one. For a newer minor in the supported
major, consumers warn, decode each known data row's required positional prefix,
ignore trailing additive fields, and skip unknown public data tags. The outer
`h`, `d`, and `m` arities remain exact because they are compact-envelope control
structure governed by the compact format version, not the record schema.
Consumers must also reject unknown format or schema-major versions, malformed
known field values, missing required fields, non-first or duplicate headers,
and missing summaries.

All `h`, `d`, data, and `m` bytes count as raw compact artifact bytes;
dictionary overhead must never be subtracted. Compact output is loaded only
through the production compact loader and queried with
`snapshot-query --input <file> --symbol <id-or-name> [--from <stable-id>
--relation <TYPE>] --format ndjson`, which writes deterministically ordered
native symbol/relation records. For an artifact produced by the same build, its
decoded public projection and canonical semantic SHA-256 (normalized native
records in record order) must equal the normal NDJSON snapshot; the lossless
preflight enforces both. A newer-minor reader instead hashes its known
projection and carries `W_NEWER_SCHEMA_MINOR`, because intentionally skipped
additive facts cannot equal the newer producer's full projection. Matching only
the hash is not sufficient evidence of losslessness.

## Experimental SCIP snapshot protobuf

`snapshot --format scip` is an experimental, complete-snapshot-only projection for SCIP consumers. It writes a single binary SCIP `Index` protobuf to stdout and a single JSON omission note to stderr. It is rejected for `symbols`, `edges`, targeted `--to`/`--from`/`--relation` output, and `--progress`, and it is not served from the NDJSON or compact snapshot caches. The SCIP package version is the project's own declared version, read from the root manifest (`package.json`, then `Cargo.toml`, then `pyproject.toml`'s `[project]` or `[tool.poetry]` table), falling back to `0` when none declares one -- which is the common case for Go, since `go.mod` carries a module path and not a version. A root Cargo package may declare its version literally or inherit it from `[workspace.package]` in that same root manifest. A virtual workspace with no root `[package]` remains `0`: choosing a repository-wide value from member defaults would mislabel mixed-version or non-inheriting members until SCIP package identity becomes member-aware. The version is deliberately **not** the commit: a commit there would give every symbol a new identity on every commit, so nothing downstream could match a symbol across commits, and cross-index linking (the reason SCIP has the field) could never work. Commit and worktree provenance are carried instead by the omission note's `commit` and `tree`, by `--worktree` in `ToolInfo.arguments`, and by `worktree_snapshot: true`. Note that SCIP `Metadata` has no revision field of its own -- it carries `ToolInfo`, `ProjectRoot` and an encoding -- so the note is where the revision lives, once per index rather than inside every symbol. The manifest is read from inside the snapshot build, through the same validated content reader every other manifest read uses, so it is bounded, refuses non-regular files, and is pinned to the revision the snapshot describes. A version bumped only in a dirty working tree therefore cannot reach an index that describes a commit, and the version cannot come from a different revision than the documents beside it. Consumers must therefore treat `(repo, commit)` as coming from the envelope, not from the symbol: two indexes of the same repository at different commits share symbol identities by design, and merging them into one flat store without keeping their commits distinct would conflate them. If the underlying snapshot is partial, the same stderr JSON object includes `partial_snapshot`, `completeness_level`, `warning_count`, and `partial_failure_count` instead of mixing human warning text into stderr. Unlike the NDJSON encoders, the SCIP encoder retains the complete graph until it can write the one protobuf message, because a SCIP `Index` is a single protobuf message and cannot be streamed. Its memory use therefore scales with snapshot size: measured on this repository (about 28 MB of native NDJSON, 9,770 definitions, 25,476 references) peak RSS was roughly 170 MB against roughly 115 MB for `ndjson` and roughly 119 MB for `compact-ndjson`, so about 1.5x the streaming formats. That multiplier is not a guarantee for a much larger repository, where the absolute figure is what matters; measure before exporting a monorepo.

The note also carries what SCIP itself cannot express. Its `language_tiers` repeats the header's
per-language `semantic` / `inventory-only` classification, because every discovered file becomes a
`Document`: an inventory-only file -- listed but never parsed for symbols or relations -- is
otherwise indistinguishable from a semantically extracted one, and a consumer scoping trust per
language cannot tell them apart from the protobuf alone. Its `partial_failures` carries the
summary's failure records rather than only their count, because a count says something was
skipped while only the records say which path and why.

The note carries its own contract version in `version`, currently
`entire-graph-scip-omissions/v1`, independent of the snapshot `schema_version`. Within a
version it evolves **additively only**, on the same tolerant-reader terms as
[ADR 0001](adr/0001-ga-schema-contract.md): new optional fields may appear, a consumer must
ignore fields it does not recognize, and it must not require any optional field to be present.
An absent optional field means zero or false, never unknown -- the counters are omitted when
they are zero precisely so a clean export stays quiet. Removing a field, renaming one, or
changing what an existing one counts is a breaking change and bumps the version to `v2`.

The stderr note also reports what the encoder could not carry. `unidentified_records` counts records it could not key at all (a file with no path, a symbol or external endpoint with no id), and `unlocated_symbols` counts symbols with no file path, which are emitted but land in a synthetic document with no navigable definition location. `missing_source_relations` reports implementation metadata or containment that could not be attached because its source symbol was absent; missing targets remain in `missing_target_relations`. These are provider bugs rather than expected input, but this format reports them rather than letting an incomplete index look complete.

The protobuf contains `Metadata`, one `Document` per source path, `SymbolInformation` for native symbol definitions, definition occurrences, and reference occurrences for supported reference-like relations: `IMPORTS`, `CALLS`, `CONSTRUCTS`, `ASYNC_CALLS`, `EXTENDS`, `INHERITS`, `IMPLEMENTS`, `OVERRIDES`, `USES_TYPE`, `PARAM_TYPE`, `RETURNS_TYPE`, `READS_FIELD`, `WRITES_FIELD`, and `ACCESSES`. Occurrence ranges cover the complete one-based inclusive line spans supplied by Entire Graph; the export does not invent unavailable columns. Well-formed native `DEFINES` and `CONTAINS` relations are represented by symbol metadata and definition occurrences rather than emitted as relation edges. Extension-member `CONTAINS` relations that disagree with `ContainerID`, and malformed structural relations that disagree with the defining file or reference a missing endpoint, cannot be represented and are reported in the note. The inheritance family -- `IMPLEMENTS`, `INHERITS`, `EXTENDS`, `OVERRIDES` -- additionally becomes `SymbolInformation.relationships` with `is_implementation`, because SCIP answers "Find Implementations" from relationships and not from occurrences; `emitted_implementations` in the note counts them. `is_reference` is added only for member-to-member relationships, where SCIP should merge the two members' references, never merely because the source happens to be callable. A definition occurrence marks the declaration line only, with the full declaration-through-body span carried as its `enclosing_range`, so a definition does not overlap every reference inside its own body and positional lookups stay unambiguous. Other relation families are omitted from the SCIP protobuf and counted by relation type in the stderr note:

```json
{"record_type":"scip_omissions","version":"entire-graph-scip-omissions/v1","format":"scip","unsupported_relation_counts":{"DATA_FLOWS":1},"missing_target_relations":0,"missing_evidence_relations":0,"emitted_definitions":2,"emitted_references":1}
```

The native NDJSON stream remains the lossless provider contract. The SCIP export is an interoperability artifact for navigation tools whose model is definitions and references, so consumers that need full Entire Graph relation semantics must keep using `--format ndjson` or `--format compact-ndjson`.

## Progress telemetry

The optional provider progress callback (`--progress` on the stream commands)
exposes process-local performance telemetry only. Its typed phases are
`inventory` (source preparation/file discovery), `parse` (header output,
registration aliases, file/symbol output, and index construction),
`relations` (relation resolution), and `finalize` (external output plus
trailing-summary construction and serialization). Each event carries elapsed
time since that phase began and total provider-work time since snapshot
entry. Progress lines go to stderr; stdout remains valid NDJSON. Telemetry
fields are not snapshot schema fields and never alter emitted semantic
records.
