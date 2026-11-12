# Import statements from the syntax tree reach IMPORTS

Status: provider increment implemented with focused native tests. Downstream client integration and independent review remain separate gates.

## Decision

The engine had two readers of a file's imports that did not agree. The tokens record ([ADR 0011](0011-syntax-leaves-and-import-targets.md)) reads every TypeScript, JavaScript, Go and Python import statement from the syntax tree. `IMPORTS` relations came from per-language text scanners, which miss multi-line imports, a Go spec beside a comment or with a raw-string path, the second module of Python's `import a, b` and a Python name spaced around its dots. On the consumer's reference ranges only 76.2%, 81.1% and 83.0% of statements had an import-target entry, so its rule "this change only adds imports that resolve" had no answer for about one statement in five, and the same file-to-file imports were missing for every reader of `IMPORTS`.

A file's import list is now the scanner's list as it was, followed by each specifier the tree's import statements give that the list does not hold, in statement order. Nothing the scanner finds is dropped or moved: it also reads `require()`, dynamic `import()` and `export … from`, which are not import statements. A Python `from` statement gives what the scanner gives for it (`m`, `m.n` for an absolute module without a dot, `.n` for a bare relative module), so a statement the scanner read adds nothing and no relation is duplicated; the bare relative module's own entry still rides on its first name's relation, now also for statements only the tree read, and `from . import *` makes `.` a specifier of its own.

## One extraction

The statements are the tokens record's: one preorder walk collects the tree's import statement nodes, and both the tokens record and the relation side read them with the same per-node readers, from the parse that produces the file's symbols. Without `--tokens` the walk still runs, so the relations do not depend on the flag; for Go it enters only top-level import declarations, the one place the grammar allows a spec. The tree is read only when it has no error and the parse completed. A file with a syntax error, over the parser's input limit, minified, or in another language keeps the scanner's answer alone.

The statements travel on the file record, off the wire, into the relation phase. Every path that builds relations reads them there: the bulk streams, interactive builds, the search snapshot cache (which keeps them in its internals) and the selective view derived from it, and the per-file extraction cache. No path parses a file a second time for them.

## What changes

Only `IMPORTS` relations are added; no relation is removed and every other family is unchanged. The `symbols` stream's records are unchanged with and without `--outline` and `--tokens`; its header's `identity_revision` and its summary's relation counts, which count the build's relations, move. The call resolver does not read this list (it has its own imported-name scanners, which already read multi-line named imports: on the Neuve head they lack 20 of the tree's 25,069 TypeScript bindings and none in the other three languages), so calls resolve as before. Feeding it the tree's bindings would be a separate change of meaning. On the Neuve repository's 420-commit head 1,914 relations are added to 9,999 (1,614 to files, 157 to declared packages, 134 to builtins, 9 unresolved) and none changes; on this repository nothing changes, since gofmt'd Go imports are what the scanner reads.

Because the snapshot of an unchanged tree differs, `identity_revision` is `"6"` ([ADR 0001](0001-ga-schema-contract.md#parser-identity-corrections-and-consumer-upgrades)), and the records (`provider-records-v16`), search (`search-snapshot-v21`) and extraction (`file-extraction-v3`) cache versions move, so an entry an earlier build wrote is never read as this build's answer.

Capability `optional_local_only_features.import_targets_cover_statements`, with its languages in `feature_languages`, declares the promise: for a file whose tokens record says its imports were extracted, every statement's specifier has an import-target entry on an `IMPORTS` relation of that file in the snapshot of the same commit. On the consumer's reference ranges every statement now has one: 1,271 of 1,271, 3,019 of 3,019 and 6,969 of 6,969.

## Limits

A file with no tokens record (its tree has an error) promises nothing beyond the scanner. A relative specifier whose name already holds a dot (`./records.server` for `records.server.ts`) is unresolved, as it was when the scanner read it. A scanner reading that is wrong but harmless stays, as in Python's `import os . path`, which also keeps the scanner's `os`.
