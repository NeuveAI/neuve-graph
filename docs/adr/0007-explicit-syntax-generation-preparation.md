# Explicit syntax-generation preparation and retained reads

Status: implemented narrow syntax-only integration; independent review pending. Full-profile incremental binding products and Git scheduling remain pending.

## Decision

The existing `index` command already owns explicit preparation. Add `--incremental --commit <exact-commit-OID> --profile syntax-only` rather than a second preparation command. The target is an exact lowercase Git commit OID, not a branch or moving HEAD. Ordinary `index` keeps its existing HEAD, full/fast/syntax and force behavior. Incremental mode rejects missing targets, other profiles, working tree, force and health.

The existing prepared-query contract accepts the syntax-only profile, whose declared relationship set is `CONTAINS` and `DEFINES`. A complete retained inventory at that profile can serve this existing contract. It does not claim call resolution, semantic binding completeness, LSP coverage or full review-context retrieval.

| Operation | Syntax-only incremental integration | Full/fast behavior |
| --- | --- | --- |
| Explicit `index --incremental --commit ...` | Reuse qualified per-path extraction and publish exact revision generation | Reject unsupported profile |
| `query --head --prepared-only` | Read qualified generation first, preserving the question and existing ranking | Existing complete search-cache read |
| `neighbors --head --prepared-only` | Read qualified generation, preserving profile and relationship limits | Existing complete search-cache read |
| Absent generation | May read compatible existing legacy syntax preparation | No generation lookup |
| Present invalid/nonregular generation | Typed refusal; do not read legacy cache, repair or rebuild | Existing strict cache admission |
| Ordinary query | Existing optional cache/build behavior | Existing optional cache/build behavior |

`analysis_scope: complete-prepared` describes retained inventory scope. The response's `profile: syntax-only`, provider relation support and existing warnings/completeness remain visible. Neither CLI nor callers may relabel this as full-profile evidence. No query writes a preparation artifact or queues a job.

## Typed boundary and qualification

Preparation and retrieval share one private generation binding and decoder in `internal/sem`. They qualify producer executable/version, schema/profile, captured ignore/include/secret policy, limits, host build eligibility, repository common-directory/scope, exact commit and tree. Retrieval preserves Stage A's `PreparedSnapshotError` reasons. Invalid retained generation maps to `invalid-artifact`; missing exact preparation fails if no compatible legacy syntax artifact exists. There is no fallback parsing or resolution.

Generation artifacts remain in their own family. They are not copied into the full search cache. An absent generation is the sole admission for reading the compatible legacy syntax format. After an open reports a missing target, the confined boundary checks the artifact name without following its final symlink. A present dangling entry is invalid, not compatible absence. This is a namespace observation, not an atomic reservation against another same-authority process subsequently replacing names. A same-tree new commit has a different generation identity; it does not inherit an earlier preparation receipt. Source identity changes and other producer/policy profiles cannot reuse that artifact.

The existing immutable publication, private-mode artifact, checksum, opened regular-file, POSIX nonblocking FIFO and cancellation/publication-race boundaries remain. Cancellation observed by the final pre-link check prevents publication; racing after that check can coexist with completed publication. Directory sync can report failure after a valid link exists. Retry validates the retained immutable artifact. No exactly-once job acknowledgement or all-filesystem interruptible-I/O guarantee follows.

## Machine receipt

The additive `incremental_preparation` object in index JSON contains:

| Field | Meaning |
| --- | --- |
| `generation_id` | Exact source/policy-qualified immutable artifact identity |
| `extracted_files`, `reused_files`, `uncacheable_files` | Unique per-path extraction work observed for this invocation; an exact ready-artifact retry reports zero extraction work |
| `supported_relation_types` | The snapshot's declared relation set, currently `CONTAINS`, `DEFINES` |
| `relationship_resolution` | The snapshot's declared resolution depth, currently `none` |
| `unchanged_content_identification` | Algorithm limitation `sha256-content-read`; new preparation still reads admitted unchanged contents |
| `source_enumeration` | Algorithm limitation `target-tree`; new preparation still lists the target tree |

These last two fields declare preparation method and limits, not I/O counters for an exact retained retry. Existing top-level commit/tree/profile/counts/completeness/warnings/partial failures remain. `index_cache_hit` means an existing exact ready generation was used in this mode. A concurrent publisher can retain that winning artifact after completing preparation, so a hit does not imply zero invocation work. The losing publisher keeps its own observed path-work receipt; an immediate ready-artifact retry has zero work. Capabilities declare `incremental_syntax_preparation: true` and `incremental_preparation_profiles: [syntax-only]`; the existing relation-support map declares its depth.

## Proof and remaining obligations

Native public-command fixtures prepare real committed TypeScript and Go files. A changed TS file extracts/parses once; unchanged facts reuse; exact retry has zero structural work. Fresh natural queries and containment neighbors compare with independently prepared legacy syntax facts and have zero extraction/parse/build/resolution counter changes. Wrong revision, policy, producer, source clone and profile fail without structural work. A present corrupt generation cannot use a valid legacy syntax cache. A writerless FIFO also refuses before decoding without blocking or fallback. Cancellation and the earlier real subprocess crash/publication controls remain separate evidence obligations.

The current prototype reads/hashes all admitted contents during new preparation, may reread cold misses, and reruns source listing, ordered structural reduction and name census. The fixture does not prove incremental full-profile dependency closure, repository-scale latency/cost, end-to-end Neuve collection or Gemini briefing quality. Neuve integration requires explicit normalized profile support in graph-client before callers can consume these syntax receipts. That caller work is outside this provider change.

Git-event scheduling and durable freshness/coverage receipts belong outside queries. Full-profile local scan reuse and positive/negative binding-dependency invalidation are still prerequisites for full ingestion. LSP, embeddings, historical enrichment and model economics remain separate work.
