# Neuve Graph

Neuve Graph is the code-graph engine behind [Neuve](https://github.com/NeuveAI/neuve), open-source code review for code that agents write. It gives Neuve a precomputed map of a Git repository: ranked code search, definitions, callers and callees, types, routes and change impact, each with `file:line` locations. Neuve uses it to frame a change, connect each part to the code around it, and ground every finding in source.

It runs entirely on your machine. It parses the repository with tree-sitter and makes no network requests, model calls or API-key lookups.

## Credit

Neuve Graph is a fork of [Entire Graph](https://github.com/entireio/entire-graph), made by Entire Inc. and released under the MIT License. Most of what this engine does comes from their work, and we're grateful for it. Their copyright notice is kept in [LICENSE](LICENSE).

Neuve Graph is not affiliated with or endorsed by Entire Inc. Some internal names still carry the upstream name, such as the `entire-graph` binary, the `entire graph` command family and the `.entire/` folder. They're kept so the fork stays easy to compare with upstream; they don't refer to Entire's products.

## What it does

- **Ranked code search** from a plain-language task description, combining lexical matches across source, identifiers, signatures and paths with graph relationships.
- **Definitions and relationships:** callers, callees, inheritance, type usage, field access and supported framework routes.
- **Change impact:** direct and transitive callers, callees, type consumers, data flow, and files that historically change together.
- **Semantic diffs between Git revisions:** added, removed, renamed, signature-changed and body-changed symbols.
- **Working-tree queries** that include uncommitted edits by default, with committed-tree queries and reusable caches.
- **Graph export** as versioned NDJSON snapshots with stable symbol identifiers, plus experimental SCIP export.
- **Machine-readable coverage, exclusions, warnings and partial failures**, with relation confidence.
- **Semantic parsing for 36 languages,** including Go, Python, JavaScript, TypeScript, Java, Rust, C, C++, C#, Ruby, PHP, Swift and Kotlin, plus inventory support for many more filetypes. `entire-graph capabilities --json` reports the installed build's coverage.

## Using it

Neuve installs and runs Neuve Graph for you: each [Neuve release](https://github.com/NeuveAI/neuve/releases) ships the engine, and the one-line install sets it up. You don't need to install it separately.

To build it from source, you need [Go](https://go.dev) and [mise](https://mise.jdx.dev):

```sh
mise run build
mise run test
```

Inside a Neuve checkout, the engine sits at `apps/neuve-graph` as a Git submodule. Neuve's [development guide](https://github.com/NeuveAI/neuve/blob/main/docs/development.md) covers `bun run setup:graph` and `bun run build:graph`.

## Working tree and cache

The interactive queries (`query`, `def`, `explain`, `neighbors` and `impact`) read the working tree by default, so uncommitted edits are visible. Add `--head` to ask about the committed tree. Bulk streams (`snapshot`, `symbols`, `edges`) and revision analysis (`diff`, `commit`) default to committed state.

Queries can write a derivative local cache. They never modify repository files. Cache locations, key inputs and prewarming are documented in [operations](docs/operations.md#cache).

## Limits

Static analysis is heuristic. Calls through interfaces, reflection, dynamic dispatch, and generated or runtime-wired code can be missed. Dependent counts are guidance for inspection, not compiler facts. Files the parser can't handle surface as machine-readable partial failures rather than silent gaps. See [language support](docs/language-support.md) and [trust and security](docs/trust-and-security.md).

## Documentation

- [All documentation](docs/README.md)
- [Command reference](docs/commands.md)
- [Search results and ranking](docs/search.md)
- [Operations: installs, cache, troubleshooting](docs/operations.md)
- [Trust and security](docs/trust-and-security.md)
- [Language support](docs/language-support.md)

Report problems in [GitHub Issues](https://github.com/NeuveAI/neuve-graph/issues), or open a pull request.

## License

Neuve Graph is distributed under the [MIT License](LICENSE), as was Entire Graph.
