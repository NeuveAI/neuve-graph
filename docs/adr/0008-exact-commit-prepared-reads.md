# Exact-commit prepared reads without source worktrees

Status: provider increment implemented and owner focused native gates passed. Downstream normalized client integration and independent review remain separate gates.

## Decision

Preparation already accepts an exact commit. Its retained facts must be readable at that same source without checkout, source worktree creation or moving HEAD. Add `query/neighbors --prepared-only --commit <exact lowercase 40/64-character commit OID>`. Reject moving names, duplicate commit selectors, explicit `--head` or `--worktree` in any flag order, no-cache and commit selection without prepared-only. Existing `--head` reads retain their behavior, including refusal of source drift during a prepared HEAD query.

The exact-target loader captures source owner, external policy and one commit/tree. That target travels through prepared admission and query source selection. Source inventory, Git grep, nested/root Gitignore interpretation, ranking and snippet readers use the same object view; advancing HEAD does not change the exact read. Metadata/source-owner changes fail instead of selecting a working-tree fallback.

| Prepared family | Admission and qualification |
| --- | --- |
| Syntax generation | Exact commit/tree, captured policy/profile, producer executable and Git common-dir/scan scope; supports only DEFINES/CONTAINS |
| Legacy search snapshot | Existing complete snapshot qualification plus exact preparation commit; no invented immutable generation ID |
| Missing/incompatible/nonregular/invalid artifact | Typed failure; no build, parser, relationship resolution, repair or preparation job |

Same-tree distinct commits remain distinct exact preparations. Syntax generations can retain both identities. The existing legacy search family remains tree-addressed and may be overwritten by another preparation of the same tree; an unavailable exact preparation is not reconstructed or rebound from another commit. Full and fast never consume syntax generations.

## Read receipt

Structured query JSON/NDJSON header and neighbors JSON carry additive `prepared_read`:

| Field | Value and meaning |
| --- | --- |
| `artifact_family` | `syntax-generation` or `search-snapshot` |
| `generation_id` | Lowercase SHA256-sized opaque generation identity for syntax-generation only; omitted for legacy family |
| `policy_id` | Lowercase SHA256-sized opaque family-qualified provider policy binding; client does not recompute private keys |
| `producer_build_id` | SHA256 of executable bytes |
| `source.common_dir`, `source.scope` | Canonical Git common directory and slash-relative scan scope, possibly empty |
| `external_policy_origin` | `serving-directory-captured` |
| `gitignore_origin` | `target-commit` |

Existing commit/tree/profile fields remain authoritative. Capability `optional_local_only_features.exact_commit_prepared_read: true` feature-detects the exact selector and this receipt. A complete-prepared scope is qualified inventory coverage at the selected profile; it does not establish complete relationship semantics, sufficiency for review or approval.

Current serving-directory `.graphignore` and ordered include/ignore bytes are bounded and captured once. The old commit does not recover historical external policy, producer settings or host environment. `policy_id` identifies the provider's actual family binding, not historical policy recovery. Source snippets use exact Git objects; source enumeration and content reads are still possible costs of a prepared query, separate from zero parser/extraction/build/resolution work.

## Verification and limits

Temporary native Git fixtures read old syntax and full preparations while HEAD differs, preserve old snippets/containment neighbors, and leave HEAD/worktree inventory unchanged. Same-tree history, missing preparation, malformed/ambiguous selectors, foreign producer/clone/scan scope, changed external policy, committed Gitignore lineage and cancellation receive literal controls. Prepared read intervals observe unchanged structural counters; existing HEAD/full tests remain distinct compatibility controls.

This adds a coherent retained-read target and qualification seam. It does not implement full incremental binding invalidation, Git scheduling, a revision registry, LSP or embeddings. Downstream callers must preserve the selected profile and receipt, compare source/producer identity and reject unsupported syntax relationship obligations before production use. No repository-scale latency, billed model savings or end-to-end review quality is inferred from these fixtures.
