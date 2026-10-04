# Explicit prepared-only reads

Status: implemented first preparation/read separation slice; incremental Git ingestion remains pending.

## Decision

`query` and `neighbors` accept `--prepared-only` with `--head` and caching enabled. The capability `prepared_snapshot_query` declares this contract. An applicable complete snapshot must already exist; absence, invalidity or incompatibility is an error. The read cannot call extraction, parsing, relationship resolution or a fallback build. Source listing, lexical selection/ranking and exact committed text reads remain query work.

The default query path remains compatible. A warm optional cache may still derive a selective graph and rerun relationship resolution. It does not satisfy this stricter contract. The retained compact `snapshot-query` filters symbols and relationships but does not implement the unchanged natural-language ranking, so it is not substituted for query.

| Operation | Ordinary path | Prepared-only path |
| --- | --- | --- |
| Missing artifact | May build | Explicit `missing` error |
| Selective relationship graph | May derive and resolve | Uses retained complete graph |
| Registration-handler preselection | May parse a candidate definition | Reads retained definition identity |
| Same tree, different commit/history | May reuse and rebind cached provenance | Exact preparation commit required |
| Presearch or session answer echo | Existing configured behavior | Bypassed; requested question is evaluated |
| Worktree or no-cache flag | Existing behavior | Rejected at invocation boundary |

Complete prepared graph scope can produce different relation expansion from the ordinary selective graph. Structured query statistics and neighbor responses declare `analysis_scope: "complete-prepared"`; this is graph scope, not complete language/source coverage or evidence sufficiency. Existing completeness, warnings, partial failures and query budgets remain visible.

## Qualification and failures

The loader reuses the existing cache's repository/scope, executable build, provider/schema/profile, captured policy, tree and private-metadata qualification. It additionally requires both retained commit fields to equal the exact serving commit. This deliberately refuses same-tree history rewrites because a full snapshot includes history-derived co-change. It does not relabel those facts by replacing the commit header. Explicit `index --force` can prepare that revision until history-derived and tree-derived artifacts have distinct identities.

The typed `PreparedSnapshotError` retains its cause in Go and exposes a fixed reason: `incompatible-read-options`, `source-unavailable`, `policy-unavailable`, `artifact-unavailable`, `missing`, `invalid-artifact`, `incompatible-artifact`, `revision-not-prepared` or `source-changed-during-read`. Cancellation retains its cancellation cause. Failed reads emit no success payload and never repair the cache.

Cache decoding and producer/source qualification are the existing local derivative trust boundary, not cryptographic authentication of cached semantic content. An immutable generation manifest with artifact digest, producer lineage and durable publication belongs to the later ingestion slice. Stage A does not add a durable queue or completion receipt, and a query must not create either one implicitly.

## Evidence

Native fixtures prepare real committed source. Positive controls observe actual extraction, AST parsing, complete build and relation-resolution entry points during preparation. Fresh questions and neighbor invocations then leave all four cumulative process counters unchanged. The counters are diagnostic process observations: isolate the interval; they do not attribute concurrent work, measure billed usage or confer approval.

The cases compare retained complete snapshots including private binding metadata and compare complete prepared ranking/neighbor outputs with freshly built complete facts under the same warm lexical selection policy. Existing default-depth warm retrieval uses Git-selected files for corpus scores; cold exhaustive retrieval scores the whole corpus. Those existing modes do not have identical scores, so these cases do not claim cold/warm score equality. Literal obligations cover an imported exact call, a shadowed import that must not resolve to that target, ambiguous candidates that must not become exact calls, required ranked callables and command-registration aliases. Missing/corrupt artifacts, contradictory flags, exact revision/history changes, cancellation and answer-replay controls exercise failure admission. These bounded cases do not establish retrieval quality for every repository or equality with the former selective graph.

## Next slice

Git events must enqueue exact revisions outside the query path. Provider-owned per-blob extraction reuse, positive/negative binding-dependency invalidation, history identity, durable publication and retries are required for incremental delivery. Calling full `index` after every commit is not that delivery. LSP and embeddings remain separately qualified preprocessing stages; this read mode does not start them.
