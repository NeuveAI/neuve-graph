# AGENTS.md — operating guide for coding agents

## Neuve workspace integration

When this repository is checked out at `apps/neuve-graph` inside the Neuve
workspace, the parent repository's `CONSTITUTION.md` and `AGENTS.md` apply to
Neuve-wide product ownership, evidence, and review. Start engine work with
`apps/neuve-graph` as the working directory, then apply this guide and the
generated `.entire/agent-guide.md` for repository-specific Go, graph, schema,
identity, no-egress, and verification rules. Surface conflicts between the
parent and local guidance rather than silently choosing one.

The local `package.json` is a Bun-workspace task shim only. It delegates to
`mise.toml`; Go and mise remain authoritative for builds and verification.

Hand this to any coding agent working in a repo where the `entire graph` plugin
is installed. It moves initial code-location work from broad grep/read
exploration to targeted graph queries; token impact depends on the task and
model, and no end-to-end savings claim is current.

Two guidance surfaces coexist here on purpose. `.entire/agent-guide.md` is the
generated activation artifact — regenerated in full by `init-agents`, so never
edit it by hand — and is what agents load through the managed block at the end
of this file. This document is the maintained long-form reference; its examples
pass flags explicitly (such as `--format text`) for readability rather than
relying on command defaults.

## What this gives you

A precomputed code graph is available through the `entire graph` command — functions, classes, methods, types, routes, and the calls/inheritance/field/service relations between them, parsed with tree-sitter. Built-in analysis is local and no-egress (no network, no model, no keys). Use it to **LOCATE** and **UNDERSTAND** code when additional discovery is needed. The same repository view and options yield the same graph, but static relations can be heuristic or incomplete; inspect focused source and verify the resulting change. Some commands write derivative caches or explicitly requested setup/report files; inspect command help when filesystem writes matter.

Default flags to remember: pass `--repo .` when you're not inside an Entire session. The interactive query family (`query`, `def`, `explain`, `neighbors`, and `impact`) reads your **working tree by default** so uncommitted edits are visible; `--head` switches those commands to committed-tree semantics. Other command families have different defaults.

---

## The parts of the graph

Reach for the smallest tool that answers your question.

### 🔍 query — *find the code for a task* (when discovery is needed)
Ranked source regions for a plain-language description, with the source and `file:line` inline. Hybrid ranking over bodies, identifiers (camelCase/snake_case aware), signatures, paths, and graph neighbors. Output is byte-budgeted to drop straight into context.

```sh
entire graph query --repo . --query "<the task or bug in one plain sentence>" --format text --top-k 8
```

- `--format agent` for compact ranked output with latency telemetry; `json`/`ndjson` for the full schema (completeness, partial failures, diagnostics).
- `--top-k N` result count; `--max-context-bytes N` byte budget (`0` = unbounded).
- Working tree by default (always rebuilt and never cached); add `--head` for cached committed-tree semantics.
- `--head --prepared-only` requires an existing complete snapshot for the exact commit and matching producer/policy/profile. It never parses, resolves or rebuilds; missing or incompatible preparation is an error. It preserves the question and skips presearch/session answer echoes. See [the prepared-read contract](docs/adr/0005-prepared-only-reads.md).
- `--prepared-only --commit <exact-commit-OID>` reads retained preparation for that historical source without a checkout/worktree. It excludes explicit `--head`/`--worktree`; source, lexical selection, Gitignore and snippets use that target. External `.graphignore` and ordered include/ignore policy come from the serving directory, not recovered historical policy. Structured receipts identify the artifact family, producer, source scope and policy binding. See [the exact prepared-read contract](docs/adr/0008-exact-commit-prepared-reads.md).
- `--profile syntax-only|fast|full` (default `fast`); `--index-all-files` or `--max-indexed-files N` to widen/bound cold-search parsing.

**When:** additional code discovery is needed; reuse locations already supplied by the task or Brain brief.

### 🕸️ neighbors — *who calls this / what does it call* (targeted relations)
Direct incoming/outgoing relations for **one** symbol, with definition locations, plus bounded two-hop paths at `--depth 2`. For the full blast radius of a change, prefer `impact`; use `neighbors` when you want one specific relation/direction. Never `edges` for this (full stream).

```sh
entire graph neighbors --repo . --symbol NAME --relation CALLS --direction in   # who calls NAME
entire graph neighbors --repo . --symbol NAME --relation CALLS --direction out  # what NAME calls
```

Neighbor selection defaults to the declared relation family: `CALLS` includes
`CONSTRUCTS`. Pass `--selection exact` when only the requested relation type is
needed. Capabilities declare the modes and families; JSON echoes the selection
and selected types. Selection does not change an edge's resolution confidence.

- `--file path` — **required** when the symbol name is ambiguous (multiple defs); the graph returns definitions only until you disambiguate.
- `--relation CALLS` (default is the call family) — pick another relation to follow it instead.
- `--direction both|in|out`, `--depth 1|2`, `--limit N`.
- `--internal-only` drops unresolved external endpoints; `--exclude-tests` drops test-only neighbors.
- `--format agent|text|json`; `--head` for cached committed-tree; `--profile fast` for shallow call resolution (default `full` favors correctness).
- `--head --prepared-only` observes retained complete facts without parsing, relationship re-resolution or fallback indexing. Match the profile and cache variant used during preparation.
- `--prepared-only --commit <exact-commit-OID>` selects exact historical preparation without modifying HEAD or creating a worktree. The same profile, producer/source and captured policy must match.

**When:** "what breaks if I change X", "who uses this", tracing a call chain — after query has given you a concrete symbol name.

### 💥 impact — *one-shot blast radius for a change*
Everything the graph knows about changing **one** symbol, in a single bounded explanation: direct + transitive callers (depth ≤ 2), callees, type consumers (`USES_TYPE`/`PARAM_TYPE`/`RETURNS_TYPE`), data flows, files that historically change together with the symbol's file, and same-container siblings. Text output is sectioned, `file:line` per entry, capped per section and ~4 KB total.

```sh
entire graph impact --repo . --symbol NAME [--file path] [--depth 1|2] [--format text|json]
```

- Ambiguous names return the definition list — rerun with `--file` to pick one.
- `--depth` 1|2 (default: 2); `--limit N` per-section entry cap; `--max-context-bytes N` total text budget; `--exclude-tests`; `--head` / `--profile` as in `neighbors`.

**When:** before changing behavior of a specific function/type — "you're changing ordering: here is every place results are ordered, limited, or consumed downstream" — one command instead of chaining neighbors + edges + git log.

### 📇 symbols — *definitions*
Full stream of symbol records (stable `compound-v1` ID, kind, qualified name, source range, signature, language, container). This is a **bulk NDJSON stream of the whole repo**, filtered to the symbol record type — there is **no positional name argument** and no server-side name filter; grep the stream client-side, or prefer `query`/`neighbors` for a targeted single-symbol lookup.

```sh
entire graph symbols --repo . --format ndjson [--worktree|--commit <exact-commit-OID>] [--outline]
```

`--commit <exact-commit-OID>` streams that exact commit (lowercase 40 or 64 hex
characters) from Git objects, with no checkout or worktree and whatever HEAD is;
the header's `commit` and `tree` are the target's. It excludes `--worktree`, needs
no preparation, and a target that is not a commit is an error with no output and
no HEAD fallback. External `.graphignore` and `--ignore-file`/`--include-file`
come from the serving directory. The same flag and rules apply to `edges` and
`snapshot`. The summary's `W_SUBMODULE_NOT_READ` and `W_GO_BUILD_EXCLUDED`
warnings name listed source that was not read. See
[the exact-commit bulk stream contract](docs/adr/0010-exact-commit-bulk-streams.md).

`--outline` adds one `file_outline` record per successfully tree-sitter-parsed
file, with root named-child ranges and grammar types in `top_level`, import
classification from Graph's import scanners, and distinct identifier names per
line in `identifiers`. Comments and strings are excluded from identifiers.
Nodes spanning at least two lines may have recursive `children`, while fewer
than four non-pass-through levels are above the node, with a hard cap of sixteen
child edges below `top_level`. A chosen list is a pass-through when exactly one
child spans at least 256 bytes and all others span fewer than 256 bytes, matching
the harness's small-unit threshold. Each list comes from the descendant within
twelve tree edges with at least two named, non-comment children and the greatest
summed byte span, provided that sum covers at least half the original node. Ties
follow source preorder. This can select expression operands or signature parts,
not just statements. Identifiers are the only source text beyond the existing
range, grammar type, import classification, path and language fields.
Inventory-only and failed parses have no outline. All profiles support the flag;
committed reads bypass caches so outlines always describe the selected source.

**When:** you need the complete definition inventory (e.g. ingesting into a store), not a single lookup.

### 🔗 edges — *relations*
Full stream of relation records across all 30 types (`CALLS`, `IMPORTS`, `EXTENDS`, `HANDLES_ROUTE`, …), each tagged with resolution and confidence. The stream is whole-repo by default; `--to`, `--from`, and `--relation` filter it server-side. For one symbol's callers/callees, prefer `neighbors` because it annotates resolved definitions and call sites from source.

```sh
entire graph edges --repo . --format ndjson [--worktree|--commit <exact-commit-OID>] [--to ID|NAME] [--from ID|NAME] [--relation TYPE[,TYPE...]]
```

`--commit` selects an exact commit as for `symbols`, including with the filters.

**When:** you want every relation (bulk export / ingestion) or a filtered relation stream. For a targeted one-symbol question, use `neighbors`.

### 🗺️ snapshot — *the whole graph*
One header record, then file, external-endpoint, symbol, and relation records, streamed so memory stays bounded. Superset of `symbols` + `edges` + files.

```sh
entire graph snapshot --repo . --format ndjson [--worktree|--commit <exact-commit-OID>]
entire graph snapshot --repo . --format compact-ndjson --commit <exact-commit-OID> > graph.compact.ndjson
entire graph snapshot --repo . --format scip > index.scip 2> index.scip.omissions.json
```

`--commit` selects an exact commit as for `symbols`, in every format. Whole-graph
output is cached per commit, mode, profile and policy, so a second read of one
commit is a cache hit wherever HEAD is.

The experimental `scip` format is a complete-snapshot binary projection for
standard SCIP consumers. Native NDJSON remains the lossless default; SCIP
retains one complete index in memory and reserves stderr for its JSON omission
note, so it cannot be combined with `--progress`. Its SCIP package version is the
project's own declared version from the root manifest (`0` when none declares
one), never the commit, so a symbol keeps one identity across commits; worktree
provenance is marked in the note rather than in the version.

**When:** ingesting the full graph into agent memory or a store such as Entire Brain, or exporting a navigation-oriented index to a SCIP consumer.

### 🧬 diff / analyze / commit / checkpoint — *what changed + risk*
Entity-level change list (added / removed / renamed / signature-changed / body-changed) with a heuristic **dependent count**, so a signature change with many dependents stands out.

```sh
entire graph commit HEAD --json                     # a commit vs its first parent
entire graph diff --base main --head HEAD --json    # between two refs (analyze is an alias)
entire graph checkpoint <id> --json                 # the commit behind an Entire-Checkpoint trailer
```

**When:** judging whether a change is safe to keep / revert / continue, or reviewing a branch/PR. High dependent counts on a signature change = run tests first.

### 🏗️ index — *build / warm one cache variant*
Prebuilds a durable, complete committed-tree snapshot and verifies it was
written before latency-sensitive work. Reuse is cache-variant-specific: a
later `--head` query finds the entry only when caching is enabled and it
resolves the same cache directory, profile, ordered ignore/include paths and
contents, and `.graphignore`.

```sh
entire graph index --repo . --head --profile full --cache-dir /path/to/cache --format json
```

**When:** once, up front, on a large repo before a batch of `--head`
searches/neighbors queries that use the same cache variant. `index` defaults to
`--profile full` while `query` defaults to `fast`, so the command above warms
neither a default `query --head` nor the default working-tree agent path.
Match the whole variant: unchanged keyed inputs and tree produce a hit, while
a changed tree or changed input selects or builds another entry.

Explicit incremental syntax preparation uses `index --incremental --commit
<exact-commit-OID> --profile syntax-only`. Matching syntax-only prepared queries
read the qualified immutable generation without rebuilding. Its relationship
coverage is only `DEFINES`/`CONTAINS`; full and fast do not consume it. New
preparation still lists the tree and reads unchanged content for identity.
See [the qualified syntax preparation contract](docs/adr/0007-explicit-syntax-generation-preparation.md).

Add `--plan` to that exact syntax index command to obtain prospective producer,
source and policy qualification plus validated `ready`/`absent` availability
without extraction or cache writes. Execute or observe the retained selection
with `--expected-generation <SHA256>`; producer/source/policy drift refuses
before preparation. A present invalid artifact is never absence. See
[the preparation selection contract](docs/adr/0009-preparation-selection-and-availability.md).

### 🧭 capabilities / version — *feature-detect*
```sh
entire graph capabilities --json    # semantic vs inventory-only languages, relation types, features
entire graph version [--json]       # provider name + plugin version
```

**When:** before assuming a language is semantically parsed, or to confirm which build is installed.

### 📊 stats — *did the graph actually save anything?* (for humans, not for you)
```sh
entire graph stats --repo . [--since 30d|7d|all] [--format text|json] [--sessions-dir path|--transcript path]
```

Local, read-only report over the coding-agent session transcripts already on disk
(`~/.claude/projects/<path-slug>/*.jsonl`; `--sessions-dir` overrides the lookup). Reports graph
calls per verb vs. exploration calls (`Read` whole-file / `Read` line-range / `Grep` / `Glob` /
shell `grep|find|cat|head|tail|sed|awk`), the bytes each path pulled into context, billed session
tokens read from transcript `usage`, a graph-first rate (share of sessions whose first locate-ish
tool call was a graph call), and an **estimated** token saving. The savings model is an explicit
assumption printed next to the number: each `query`/`neighbors`/`impact` call is credited with the
one whole-file read it replaced — on-disk size of the top-hit file it pointed at (repo median
tracked-file size when unresolvable) minus the bytes that call returned, floored at 0, at 4 bytes =
1 token. It is not a measured counterfactual. No network, no writes. `--transcript <path>` narrows
the whole report to one session (that transcript plus its `<session>/subagents/*.jsonl`), which is
what `scripts/entire-graph-statusline.sh` renders as a live Claude Code status line badge.

**When:** a human asks what the graph is buying them. Agents should not run it as part of a coding task.

---

## Repository-specific workflow

Follow `.entire/agent-guide.md`, generated from repository state. Graph-only
instructions start needed discovery with
`entire graph query --repo . --profile full --query "<task>"`.
Combined instructions begin substantive orientation with
`entire brain brief "<task>" --json` unless equivalent task context is available.
Reuse useful brief locations; do not require a redundant Graph query. Use Graph
for additional discovery, structural analysis, and semantic revision comparisons,
and Brain for retained knowledge, prior decisions, and checkpoint/session history.

Direct source inspection is appropriate when locations are sufficient. Skip
ceremonial queries for small edits and follow-ups. Current source and executed
tests establish present behavior; historical answers explain prior intent.
Investigate disagreements. Ordinary query failures do not authorize automatic
installation, configuration, or repair. See [the contract](docs/agent-coordination.md).

## Operating doctrine

1. **Answer the information gap.** Follow the generated workflow; query Graph when further discovery is needed.
2. **Treat results as evidence, not truth.** Read focused source around the result and widen the check when behavior, aliases, generated code, or dynamic dispatch could matter.
3. **Use graph follow-ups when they answer a real question.** `impact`, `callers`, and `neighbors` are appropriate for blast radius and related-site checks; avoid exploratory whole-graph dumps.
4. **Make the smallest complete change.** Check sibling sites and contracts when the task implies them.
5. **Verify before stopping.** Run the `VERIFY:` command when query prints one; it may be a whole-suite fallback when no narrow command exists. Never report that tests pass without having run them. If execution is unavailable, perform a bounded source-level verification and disclose the limitation.
6. **Optimize context after correctness.** Prefer precise queries and line ranges, but never trade resolution for fewer turns.
7. **Account for semantic coverage.** Inventory-only files have file records but no semantic relations; verify incomplete results against current source.

Quick mental model:

```text
locate  →  entire graph query --query "..."          (ranked code + file:line)
impact  →  entire graph impact --symbol X              (one-shot blast radius: callers, types, data flow, co-change)
callers →  entire graph neighbors --symbol X ...       (targeted callers/callees of X)
change  →  entire graph diff --base A --head B          (entity-level, with dependents)
ingest  →  entire graph snapshot --format ndjson        (whole graph)
report  →  entire graph stats --repo .                  (human-facing: graph vs grep/read usage + estimated token savings)
```

---

## Working on entire-graph itself

If your task is modifying this repository (not just using it), the build/test surface is in `mise.toml`:

```sh
mise run build   # go build -o entire-graph ./cmd/entire-graph  (needs CGO for tree-sitter)
mise run test    # go test ./...
mise run check   # fmt + vet + race tests + build
```

Contract rules that must not break: schema `1.x` is frozen and additive-only (`docs/adr/0001-ga-schema-contract.md`); the provider is **no-egress** (never add remote fetches, hosted API calls, telemetry, or runtime grammar downloads); `compound-v1` symbol IDs must stay stable across ordinary edits; unsupported/unparseable files must surface as machine-readable partial failures, never silent drops. All logic lives under `internal/` (`sem` = parsing/graph/search, `cli` = hand-rolled dispatch, `gitutil` = git subprocess); `cmd/entire-graph/main.go` is a thin entry point. The plugin manifest (`entire-plugin.yml`) registers the subcommand `graph`, so users type `entire graph ...`. This project was **previously named `entire-sem`** — do not reintroduce the old name. **Entire Brain** (`entire-brain`) is the separate downstream consumer of this provider's NDJSON — not an old name for this project.

### Working in a scratch worktree: adopt the session first

Entire binds an agent session to the worktree it started in, and links a commit to
that session through a `Entire-Checkpoint:` trailer added by its git hooks. Commits
made in a DIFFERENT worktree are not linked: no checkpoint, no trailer, and the
work is absent from `entire checkpoint list` even though the hooks are healthy and
`entire doctor` reports nothing wrong.

That is easy to hit precisely because reviewing a pull request should happen in a
scratch worktree rather than the primary one. Before the first commit in it, run
from inside the scratch worktree:

```sh
entire session adopt <session-id> --from /path/to/primary/worktree
```

`entire session current` shows the id, and `entire session info <id>` shows which
worktree a session is bound to and how many checkpoints it has. A session that has
made commits but reports `Checkpoints: 0` is the symptom.

If it is already too late, `entire session attach <session-id>` builds a checkpoint
from the transcript and links it to the last commit. Two cautions:

- It attaches to the last commit **of the current branch**, so make sure that
  branch is up to date first. A primary worktree that has only ever fetched still
  points at whatever it was on when the session started.
- Do not pass `--force` on a branch whose tip is already pushed: that flag amends
  the last commit to add the trailer, which rewrites published history. Without it
  the checkpoint is still created and the trailer is printed to paste into a later
  commit instead.

<!-- entire-agent:begin -->
Read .entire/agent-guide.md for this repository's workflow, source inspection, and verification guidance.
@.entire/agent-guide.md
<!-- entire-agent:end -->
