# A call relation says where the call is

Status: provider increment implemented with focused native tests. Downstream client integration and independent review remain separate gates.

## Decision

A `CALLS`, `CONSTRUCTS` or `ASYNC_CALLS` relation carries one call-site evidence entry (`call_site`, `imported_call_site`, `async_call_site`, `top_level_call_site`). Until now its `start_line` and `end_line` were the caller's own span for a call made from a symbol, and absent for a call made at file level. On the Neuve repository's 420-commit head 39,118 of 39,154 calls from a symbol carried the caller's first line, and 4,374 of 4,374 file-level calls, every call inside a test callback among them, had no line at all. A reader could not check any of them.

For a caller whose file is in Go, JavaScript, Python or TypeScript the entry now says where the call is: one line, the line where the callee's name is written in the call that produced the relation, the first such call in source order. When the engine cannot tell, the entry has no line, never the caller's span. When the target's name is handed over at that line as a value (`xs.map(double)`) and the caller never calls it, the entry carries `form: "reference"`; absent, a call or construction is written there. [The snapshot format](../snapshot-format.md#call-site-evidence) states the meaning per kind.

Capability `optional_local_only_features.call_site_lines`, with its languages in `feature_languages`, declares both promises: for a call relation whose caller's file is in a listed language, a line on the entry is where the target's name is written in the caller, and the entry says when that is a reference and not a call. An engine without the key, and a language not listed, give no call line; their entries keep the caller's span.

## Where each emit site finds the call

Every place that emits a call relation already found the call in the caller's source; the position was dropped when the scanners reduced it to a set of names. They now keep the first position per spelling, and the emit site turns it into a line of the caller's file.

| Emit site | How the call is found | Line |
| --- | --- | --- |
| Bare-name pass (`name(`), all four languages | text scan of the caller's source (first match that is not a declaration) | offset of the first match |
| Bare-name pass, TypeScript and JavaScript | the scope scan's syntax tree: identifier calls and constructions, then callable arguments; namespace calls `A.B.f()` | byte of the first use (a visible one for an imported alias); a call with type arguments the grammar read as comparisons comes from the text scan |
| Imported external call by bare name, and the TypeScript, JavaScript and Python alias imports | same name as the bare-name pass | the bare-name line |
| Python module-qualified calls (`pkg.mod.fn()`) | text scan per dotted spelling | the first dotted call whose module the target came from |
| Async calls (`await f()`, `go f()`, spawn forms) | text scan | offset of the first match |
| Receiver calls (`recv.method()`, `new T().m()`, `f().m()` and their chains) | text scan of the caller's source | offset of the first call of that spelling's method name |
| Calls through an imported module receiver (`pkg.Fn()`) | text scan | offset of the first match |
| Go call carried from an interface method to its implementation | the interface call's relation | that relation's line |
| File-level pass | text scan of the file with every symbol's source blanked; for TypeScript and JavaScript the syntax tree's uses outside every symbol | as for the bare-name pass |
| JavaScript and TypeScript named self call | syntax tree | already had its line |
| Language-specific receiver scanners (Kotlin, Swift, Ruby, PHP, Perl, C#, Java, Dart, Objective-C, Rust paths), Erlang, Haskell, OCaml, Rust module scanners | dedicated scanners that keep no position | not placed: those languages are not listed and keep the caller's span |

`route_endpoint_match` and `graphql_schema_resolver_match` relations join a client call to a handler through a shared endpoint; they are not calls written in the caller and are unchanged.

Other languages whose calls go through the same generic scanners are not listed: their text scan cannot tell a call from a nested declaration without the syntax tree (`void run() {` inside an anonymous Java class reads as a call to `run`), and their dedicated scanners keep no position. Listing one needs that language's tree, as TypeScript and JavaScript use theirs.

## A call, or a function handed over by name

In TypeScript and JavaScript one shape gives a call relation without a call: an identifier passed as a call argument, read by the callable-argument scan and the tree's argument list. The engine knows which one it found, so the entry is marked `reference`. On the Neuve head 2,383 entries are marked (1,729 `call_site`, 550 `imported_call_site`, 104 `top_level_call_site`), all TypeScript. Many are not functions at all but local values whose name matches a symbol elsewhere (`ensureDir(home)` reaching a function `home`); the mark shows a reader that nothing calls the target there. Assignments, returns, array and object literals, method values (`obj.method`) and JSX attributes (`onClick={handler}`) give no call relation, because every scanner requires a `(` after the name.

In Python and Go no scanner turns a name handed over as a value into a call relation (a name passed as an argument, assigned, returned, held in a literal, or a method value `x.Method`): an entry with a line is a call. The capability therefore covers both promises for all four languages.

Two shapes give no relation today, so there is nothing to mark: in TypeScript and JavaScript an identifier argument after a string literal (`test("case", handler)`), and an identifier argument inside the argument list of a call the scan has already read (`it("case", () => [1].map(double))`). Giving them a relation would add relations, which this change does not do.

## What a consumer saw before, and sees now

- `neighbors` and `impact` told a user "called at line N" by looking for the callee's name in the caller's span. That picked the first line writing the name followed by `(`, which could be another function of the same name, a different spelling of the call, or a loose mention when no call matched; a file-level caller and an `ASYNC_CALLS` caller got no call line at all. They now report the engine's line for a listed language, keep searching the caller's span only to count the other calls and find the guards, and report a file-level caller's call with the test callbacks around it as its guards.
- Search's related-caller block and its callee hop took the call's line from the entry, which was the caller's declaration line; they now show and order by the call itself.
- The SCIP export turned each entry into a reference occurrence covering the caller's whole span; it is now one line at the call, a file-level call becomes an occurrence instead of a missing-evidence count, and an entry with no line is counted missing.
- The compact form carries the lines as it always did, now including a file-level entry's, and the optional `form` as a sixth evidence element.
- `query` text output and the status line read no call entry lines and are unchanged.

## What changes

No relation is added or removed, no symbol or file changes, and every field of every relation is as before except a call-site entry's two lines and the new optional `form`. On the Neuve head 42,811 of its 43,493 call relations in the four languages change; the other 682 are 680 one-line callers whose span already was the call's line and 2 file-level calls the engine still cannot place. On this repository 28,968 of 28,983 change, the other 15 being one-line callers. Because the snapshot of an unchanged tree differs, `identity_revision` is `"7"`, and the records (`provider-records-v17`) and search (`search-snapshot-v22`) caches move; every cache version carries the revision. The new field is schema `1.4` ([ADR 0001](0001-ga-schema-contract.md#evidence-form-schema-14)).

## Limits

- The line is the first call of the entry's own spelling. A target reached through two spellings keeps the entry the engine emitted first, which may not be the earlier call.
- Carrying every call's line was measured and not built: about a quarter of the entries with a line have more than one call of their spelling (10,527 of 43,370 on the Neuve head, 7,413 of 28,983 on this repository), and an array of every line would add about 0.17% and 0.37% to the NDJSON snapshot; it needs every scanner to keep every position, not only the first.
- Where a TypeScript or JavaScript file's tree has a syntax error and shows no use, or its parse failed or stopped at the depth limit, the line comes from the text scan, which masks strings and comments and declines a declaration-shaped match but cannot rule out every non-call.
- Entries with no line on the Neuve head: TypeScript 123 (`call_site` 116, `imported_call_site` 4, `top_level_call_site` 3), Python 3, Go and JavaScript 0; on this repository none. Each is a match the text scan read as a call that is not one; the relation is kept as before.
