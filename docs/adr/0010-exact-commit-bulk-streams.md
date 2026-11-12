# Exact-commit bulk streams

Status: provider increment implemented with focused native tests. Downstream client integration and independent review remain separate gates.

## Decision

`snapshot`, `symbols` and `edges` read HEAD or, with `--worktree`, the working tree. A reviewer of a commit range needs one whole-tree read of each side, and neither side is usually HEAD. Add `--commit <exact-commit-OID>` to all three, for every output format they accept (`ndjson`, `compact-ndjson`, `scip`) and for the filtered `--to`/`--from`/`--relation` path. It builds: unlike `query/neighbors --prepared-only --commit` ([ADR 0008](0008-exact-commit-prepared-reads.md)) it needs no retained preparation and does not accept the prepared-only flag.

The build reads the target's tree with `git ls-tree` and its blobs with `git cat-file`. There is no checkout, no worktree and no change to the repository; HEAD may be any commit, and a bare or `--no-checkout` clone works. The header's `commit` and `tree` are the target's. Everything downstream of source selection already reads that one commit and tree: content reads, nested `.gitignore` vendored-path rules, the root `tsconfig.json` aliases, registration tables, the SCIP project version and the `FILE_CHANGES_WITH` co-change history (reachable from the target, not from HEAD).

In Go the selector is `ProviderSnapshotOptions.WithExactCommit`, a constructor over the existing unexported build target that syntax-only incremental preparation already used. It is not an exported field because the HEAD-keyed search and preindex caches take the same options type; they refuse a set target instead of storing it under HEAD's key. `PreparedCommit` keeps its read-only meaning and is still refused for builds.

## Selector rules

| Input | Result |
| --- | --- |
| Lowercase 40- or 64-character hex, supplied once, that Git resolves to itself as a commit | Build that commit |
| Abbreviated, uppercase, empty, a ref name, `HEAD` | Refused while parsing flags, before any Git process |
| `--commit` twice, or with `--worktree` in either order | Refused while parsing flags |
| A tag object's id, a tree or blob id, an unknown object, unsafe Git metadata | Error, non-zero exit, nothing on stdout |

There is no fallback to HEAD or the working tree in any row.

## Cache identity

The whole-stream records cache was keyed on HEAD's commit and tree. With `--commit` it is keyed on the target's commit and tree, so a second read of one commit with the same mode, profile and policy is a cache hit wherever HEAD has moved, and a different commit is a different entry. `Store` still refuses a header whose commit or tree disagrees with the key. A HEAD read and a `--commit <HEAD's OID>` read produce byte-identical streams, so they share one entry. The filtered path stays uncached, as before.

The cache version is now `provider-records-v14`, and the key also carries whether coverage warnings were requested.

## Policy origin

As for prepared reads: `.graphignore`, `--ignore-file` and `--include-file` come from the serving directory, captured once; `.gitignore` files come from the target commit. An old commit does not recover the external policy it had when it was current.

## Source omission warnings

Two silent omissions made a consumer read "no records" as "nothing there". The bulk stream summaries now name them as info-severity `warnings` (not `partial_failures`, so `stats.partial_failures`, `completeness_level` and health are unchanged):

| Code | Per | Detail |
| --- | --- | --- |
| `W_SUBMODULE_NOT_READ` | Committed-tree gitlink not excluded by ignore or vendored-path rules | The pinned commit |
| `W_GO_BUILD_EXCLUDED` | Go file outside the host's default build; still dropped, with no file record | Host `GOOS/GOARCH` and the rule (build constraint or file-name suffix) |

They are produced only when `ProviderSnapshotOptions.CoverageWarnings` is set, which the three bulk commands do. Interactive query surfaces leave it off, so their diagnostics, agent coverage lines and answer replay are unchanged. A `--worktree` stream does not report gitlinks: its listing comes from a fallback-capable index walk that does not separate gitlinks from symlinks today. On this repository the Go warnings add 55 entries and about 19 KB to a 30 KB summary.

Capabilities `optional_local_only_features.exact_commit_bulk_streams` and `source_omission_warnings` feature-detect the selector and the warnings.

## Identity revision 5

The same change gates `block` symbols to HCL. Other grammars name statement bodies `block`, and a Rust function body ending in an identifier became a `block` symbol on the function's own lines. Removing them changes emitted entity membership for Rust (and any grammar with a named `block` child), so `identity_revision` is `"5"`; see [ADR 0001](0001-ga-schema-contract.md#parser-identity-corrections-and-consumer-upgrades).

## What is not promised

No historical external policy, producer settings or host environment are recovered. Go build exclusion follows the host that built the snapshot. Cost is a full build of the target tree each time the cache misses; there is no time budget and none is added. Same-tree distinct commits remain distinct cache entries: the key carries the commit, and their co-change history can differ. This does not add a revision registry, scheduling, incremental full-profile builds or any network access, and it does not establish relationship completeness, sufficiency for review or approval.
