# Syntax leaves, import bindings and import targets

Status: provider increment implemented with focused native tests. Downstream client integration and independent review remain separate gates.

## Decision

Neuve's review decides three rules from code alone (a change that alters no token, a change that touches only imports, families of sections with one pattern), and language analysis lives in Graph. Graph therefore gives:

- `symbols --outline --tokens`: one opt-in `file_tokens` record after each `file_outline`, from the same tree. It holds every leaf of the tree as positional rows, the file's identifier names and, for TypeScript, JavaScript, Go and Python, its import statements with their bindings. The record shape and the per-language classification table are in [the snapshot format](../snapshot-format.md#file-tokens-records).
- On every `IMPORTS` relation of `snapshot` and `edges`: one evidence entry per specifier, `import_target_file`, `import_target_declared_package`, `import_target_builtin`, `import_target_unresolved` or `import_target_not_classified` ([import targets](../snapshot-format.md#import-targets)).

## Why these shapes

The tokens record is opt-in and uncached like the outline, so without `--tokens` no stream changes. Rows are positional integers with per-file tables of types and ancestor hashes because a file has roughly one leaf per four source bytes; the record still runs to about six to eight bytes per source byte.

The import classification is an extra `Evidence` entry rather than a new relation field. An entry has the same five fields as every other, so compact v1 relation and evidence arities hold, a v1 reader decodes it, the schema stays `1.3` and the header does not change. A new relation field would have needed a thirteenth compact column and a schema minor. The entries are classification, not provenance, so the `fast` profile keeps them while dropping other evidence. A second specifier that reaches a file an earlier one reached used to be dropped as a duplicate relation; its entry now joins the first relation, so no specifier is lost and the relation count does not move.

Manifests are read from the snapshot's tree through its content reader, so `--commit` classifies against that commit. Interactive and search snapshots do not classify, so their caches and answers are unchanged; the records cache version moves to `provider-records-v15` so cached streams gain the entries.

## Limits

A tree with an error node gets no outline and so no tokens record. TypeScript aliases come from the root `tsconfig.json` only; an alias the engine cannot follow is `import_target_unresolved`, never a declared package. A nested Go module's packages do not resolve to files, so they read as declared by the nested `go.mod`. A Python name no manifest declares by identity is not classified. The declared package's name is not carried; it is the specifier's package part, or for Go the required module that prefixes it.
