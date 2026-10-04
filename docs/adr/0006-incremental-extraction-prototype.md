# Incremental syntax extraction prototype

Status: internal Stage B1 library and bounded native proof. Not exposed through a command or advertised capability. Git ingestion and full semantic generations remain pending.

## Ownership and admission

The provider owns parsing and the metadata that later relationship resolution consumes. Reusing normalized caller observations cannot preserve that private metadata. `internal/sem.PrepareIncrementalProviderSnapshot` therefore composes the existing provider worker and reducer instead of adding an independent extractor in Neuve CLI or Graph client.

| Step | Existing owner reused | New behavior |
| --- | --- | --- |
| Admit a target | Git metadata/source boundary | Require an exact lowercase commit OID; reject worktree, force, selective files, `fast` and `full` |
| Qualify facts | Search snapshot policy/source identity | Bind producer executable, schema/profile, source common directory and scope, ignore/include and secret policies, limits and host build eligibility |
| Extract each admitted path | Provider file worker | Reuse a sealed artifact for the same policy, path and content digest; parse misses with the unchanged worker |
| Assemble the snapshot | Existing ordered provider reducer | Recompute structural relationships and registration-name census using retained private metadata |
| Publish readiness | Existing confined cache writer | Publish one complete generation artifact with no replacement, qualified by exact commit and tree |
| Retry | Generation loader | Read and validate the same immutable artifact; never rebuild a corrupt published value |

Syntax-only supports every language already admitted by the provider, with the same omissions and partial-failure reporting. It produces `DEFINES` and `CONTAINS`, not resolved call graphs. It is not sufficient for production TypeScript/full review-context retrieval. No prepared query silently invokes this operation, and its generation family does not replace the prepared-query search cache.

## Reuse and invalidation

A fact includes the file record, symbols, import metadata, extraction failures and private scope/signature/range/ownership metadata. The private metadata codec is shared with the existing search cache. The search cache version advances because C/C++ ownership and linkage metadata are now retained too.

Path is part of the extraction identity: a rename must re-extract to produce the correct symbol IDs and ownership. Deleted paths are excluded by the newly listed target tree. New and removed competing definitions invalidate registration alias attribution through the complete name census, including negative lookups; unchanged source is not reparsed for that census. This is not dependency closure for TypeScript imports, calls or other full-profile bindings. Those profiles are rejected rather than falling back to a full semantic rebuild.

Policy and producer changes cold-miss the extraction namespace. Exact commit is part of the generation identity even if two commits have the same tree. Syntax-only adds no history-derived co-change relationships; this preserves provenance but does not implement incremental historical enrichment or a parent-generation lineage model.

The present lookup reads admitted file contents through the existing bounded source reader to calculate a content digest. It reduces parsing, not whole-corpus content I/O or tree listing. Unchanged Git blob OIDs should be obtained through the existing bounded metadata boundary in the next slice. There is no quantitative large-repository speedup claim.

## Publication and retry

File facts are independent reusable artifacts; none is a ready graph. Cancellation observed during preparation, parse timeout or file-read failure refuses generation publication. Completed facts remain reusable. A native subprocess crash before final publication leaves no ready generation; a retry can reuse those facts.

Artifacts are checksum sealed, private-mode files. The held-directory cache write boundary rejects descendant symlink redirection. Retained reads validate the opened handle as a regular file. The existing platform-qualified opener uses `O_NONBLOCK` on POSIX so a regular-to-FIFO namespace swap cannot park before that validation; Windows uses the existing disk-handle check. The caller-selected cache root may remain a symlink, and reads retain `os.Root` confinement. These checks do not make every filesystem syscall interruptible by context cancellation or establish a deadline for regular-file I/O on a stalled filesystem.

Publication syncs the temporary file, hard-links it into the final name without replacing an existing value, and syncs the held directory. A competing publisher must validate the winning value. Corrupt retained facts or generations fail explicitly without silent replacement parsing. Cancellation observed by the final pre-link check prevents publication. Cancellation racing after that check can coexist with a completed artifact and an ambiguous acknowledgement. A retry reads and validates the exact immutable artifact; it does not replace it. Directory-sync failure after the link can report failure even though a valid artifact is visible; retry validates the retained artifact. This is not an exactly-once acknowledgement protocol.

The checksum detects accidental corruption, not authentication against an actor able to rewrite both the artifact and its checksum. Existing cache-root relocation assumptions remain. Native evidence covers the current authoritative Go toolchain and host filesystem, not every filesystem/platform, power-loss durability of newly created ancestor directories, cache retention/garbage collection, scheduler leases or durable event delivery.

## Evidence and next boundary

Real committed six-path fixtures contain Go, TypeScript, Python, Erlang, C++ and a command registration. Complete snapshots, including private metadata, equal independently uncached snapshots at the same exact target. Changed Go and TypeScript files parse once; unchanged paths do not parse. Native cases cover renamed/deleted paths, alias ambiguity and removal, policy/producer changes, same-tree new commits, old commits while HEAD advances, cancellation/retry, abrupt subprocess death, competing publishers, corrupt artifacts, invalid targets and descendant symlink rejection. These bounded cases do not establish extraction correctness for every supported language or full-profile binding completeness.

The next implementation must eliminate unchanged-content reads through bounded Git object metadata; capture dependencies of full-profile resolution, including absence/ambiguity, before invalidating affected products; compare those products with uncached full facts; and integrate exact-revision generations with the prepared reader. Git-event scheduling belongs outside queries. LSP and embedding stages need separate version/configuration/coverage identities and are not started by this prototype.
