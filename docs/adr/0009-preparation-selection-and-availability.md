# Selected syntax preparation and read-only availability

Status: implemented provider increment; independent review pending. Durable Git-event scheduling and normalized downstream preparation APIs remain separate work.

## Decision and ownership

The existing `index` boundary owns explicit preparation. A queued exact commit also needs a selected producer/source/policy binding before work begins: otherwise an environment change can silently prepare a different generation on retry. Add read-only `--plan` and an `--expected-generation` guard to the existing syntax-only incremental form. Use the provider's existing generation identity; do not introduce a caller hash, revision registry, job service, or query-triggered preparation.

| Invocation | Effect | Returned qualification |
| --- | --- | --- |
| `index --incremental --commit <exact-OID> --profile syntax-only --plan` | Capture the exact binding; validate generation availability; no extraction, publication, cache/report write or legacy fallback | Prospective binding with `absent`, or validated retained binding with `ready` |
| Same plan plus `--expected-generation <SHA256>` | Also refuse a changed selected binding before artifact reads | Matching selected binding and current validated availability |
| Same incremental index without `--plan`, plus `--expected-generation <SHA256>` | Refuse binding drift before artifact reads or any extraction write; otherwise prepare/reuse the same immutable generation | Validated ready generation and actual per-invocation work |
| Ordinary index / full / fast | Existing behavior | Existing ordinary index receipt; no syntax plan claims |

Exact commit is a lowercase 40- or 64-character Git commit OID. Expected generation is a lowercase 64-character SHA256 provider identity. Both new flags require incremental syntax mode. Incremental mode rejects `--head`, working-tree selectors, force, health, and duplicate non-repeatable flags. Plan additionally rejects `--report`. Ordered ignore/include flags remain repeatable. No command changes HEAD, creates a worktree, fetches or calls a model.

## Additive receipt

Capability `optional_local_only_features.incremental_preparation_plan: true` declares this boundary. Plan JSON carries `format_version: 1`, provider/version, repo root, exact commit/tree/profile, existing `prepared_read` qualification, and `preparation_plan: {availability: "ready" | "absent"}`. An absent plan has no fake snapshot counts, completeness, work receipt, or `index_cache_hit`. The `prepared_read` name reuses the existing B3 qualification shape; **on absence it is prospective selection, not a successful retained-read attestation**. Consumers must retain availability separately. Plan reads only the syntax-generation family; it never substitutes a legacy search snapshot.

Successful incremental index JSON gains `prepared_read`; existing `incremental_preparation` work and limitation fields remain unchanged. The qualification contains syntax-generation family, generation ID, opaque policy ID, actual executable SHA256, canonical Git common-directory and scope, `external_policy_origin: serving-directory-captured`, and `gitignore_origin: target-commit`. Existing top-level commit/tree/profile are authoritative. Policy is not recovered historical external configuration and must not be recomputed by downstream callers.

The shared capture computes one binding for the invocation. Planning and execution use the same policy, source and exact commit/tree selection. The generation ID already incorporates the executable/provider/schema, captured policy/limits/build eligibility, source owner/scope, and exact source identities. The expected-generation comparison runs before opening a generation or writing reusable facts. Present corruption/nonregular/dangling artifacts remain typed refusals; only genuine absence produces `absent`. Later source-owner or repository identity changes refuse before retained results are returned or a new generation is published. No old external policy bytes are restored: drift between plan and execute requires explicit reselection, not silent target replacement.

## Refusal and crash limits

`IncrementalPreparationError` retains safe reasons and wrapped causes. Selection drift is `generation-mismatch`; malformed expected identity at the native boundary is `invalid-expected-generation`. Invalid retained content is `invalid-generation`; nonregular/unopenable artifacts are `artifact-unavailable`. Source, policy, unsupported options and cancellation remain explicit. CLI flag admission emits no success receipt.

Planning reads and validates a retained artifact, including decompression and digest checks, when one exists. Zero structural counters do not mean zero CPU, zero filesystem reads, or fixed latency. A ready observation can race a later deletion; it is not a reservation. Source validation likewise is an observed boundary, not a filesystem-wide lock against a same-authority actor mutating metadata after the final check.

Publication remains immutable and atomic at the existing artifact boundary. Cancellation before the final pre-link check prevents publication; cancellation racing after that check or a directory-sync error can coexist with an artifact. Retry observes the same selected binding to reconcile that uncertainty. Extraction fragments never prove ready. A concurrent losing publisher retains its own actual extracted/reused counts even when it returns the winner's generation. No exactly-once execution or all-filesystem interruptible-I/O guarantee is added.

## Evidence and remaining work

Native fixtures cover ready/absent planning with zero structural work and no publication, nonexistent cache-root noncreation, exact older commit with dirty newer HEAD, same-tree distinct commit binding, guarded execution qualification, wrong policy/provider/source/limits before any extraction, later repository identity drift, corrupt/nonregular/dangling/FIFO artifact refusal, cancellation, duplicate/moving/invalid selectors, and existing concurrent publication/retry controls. Disposable native executable proofs separately establish actual build digest binding; synthetic provider-version tests alone do not prove executable drift.

Syntax-only remains `DEFINES`/`CONTAINS`, with no call/binding resolution. New preparation still enumerates the tree, reads/hashes admitted unchanged content, performs ordered reduction/name census, and reuses only qualified per-path extraction. This provider increment does not establish full-profile dependency invalidation, LSP, embeddings, complete history backfill, durable job scheduling, end-to-end collection, briefing quality, review approval or measured whole-product savings.
