# Command reference

Day to day, Entire Graph is driven by a coding agent following the guide that
`init-agents` installs. The commands below are the same surface used directly
for manual inspection, debugging a result the agent acted on, or scripting.
`entire graph help` lists every public command for your installed version, and
`entire graph <command> --help` is authoritative for flags; this page groups
the commands by task and records the defaults that matter most.

Pass `--repo .` (or a path) when running outside an Entire session.

## Set up

| Command | What it does |
| --- | --- |
| `init-agents` | Writes `.entire/graph-agent.md` and managed blocks in `AGENTS.md`/`CLAUDE.md`. See [agent activation](agents.md). |
| `agent-guide` | Prints the operating guide `init-agents` installs, for inspection or piping elsewhere. |
| `index` | Prewarms one committed-tree cache variant before a batch of `--head` queries. Defaults to `--profile full`; see the [operations cache guide](operations.md#cache). |
| `capabilities` | Reports semantic vs inventory-only languages, relation types, profiles, and features as JSON. Feature-detect with this before relying on a relation family. |

## Inspect

These five form the interactive query family. They read the **working tree by
default**; `--head` switches to the committed tree.

| Command | What it does |
| --- | --- |
| `query` | Ranked source regions for a plain-language query. Defaults: `--format json`, `--profile fast` (the installed agent guide asks for `--profile full`). Formats: `json`, `ndjson`, `text`, `agent`. See [search results](search.md). |
| `def` | One name's declaration, fields, and method surface. Default format is text. |
| `explain` | Resolves symbols named by a failing build or test into definitions and context. |
| `neighbors` | Direct relations of one symbol (`--relation`, `--direction`, `--depth 1\|2`). Ambiguous names return a definition list; disambiguate with `--file`. |
| `impact` | One-shot blast radius for a symbol: direct and transitive callers (depth ≤ 2), callees, type consumers, data flows, co-change files, siblings. |

`neighbors --selection family` is the default: `--relation CALLS` follows
`CALLS` and `CONSTRUCTS`. Use `--selection exact` to follow only the requested
relation type. Capabilities declare `relation_selection_modes` and
`relation_families`; JSON echoes `selection` and `selected_relations` for both
directions and depth modes. Diagnostics and truncation describe the selected
traversal regardless of the selection mode.

Add `--summary frontier --depth 1 --direction in|out --format json` to an
explicit neighbor request to obtain counts for its retained endpoints.
Capabilities declare `optional_local_only_features.neighbor_frontier_summary`.
Omitting the option leaves the neighbor payload unchanged. Other depth,
direction, and format combinations with this option are rejected.

| Summary observation | Meaning and limit |
| --- | --- |
| `direct.eligible_edges`, `emitted_edges`, `omitted_edges` | Selected renderable relation records before and after the direct entry cap. Eligible equals emitted plus omitted. |
| `frontier[].counts` | Selected relation records incident to that retained endpoint in the requested direction, grouped by relation and resolution. These are not distinct downstream entities or runtime calls. |
| `missing_endpoint_edges` | Selected records whose other endpoint is absent from the snapshot. Counted separately because its eligibility cannot be established. |
| `external_edges` | Eligible records whose other endpoint is external, also included in the resolution bins. |
| `returns_to_focus`, `self_edges` | Observed recurrence within those records. They can overlap and do not establish global cycle membership. |
| `kind: unavailable` | External, inventory-only, or unclassified retained endpoints cannot supply a structural count; no zero is invented. |

The summary echoes its mode, selection, selected relations, direction and depth.
It uses the enclosing response's repository, commit, tree, profile and diagnostics,
and applies the request's endpoint filters; `indexed_coverage` retains the snapshot
health status. A missing or
ambiguous focus produces `kind: unresolved_focus`. An observed summary lists
each retained endpoint once, even when several direct records connect it.
Second-hop endpoint identities and source are not returned. Counts come from
the same loaded snapshot with one additional scan and no persistent store.
Partial coverage and zero observed counts do not prove no dependencies or
sufficient evidence. Choose any subsequent neighbor request explicitly.

Cache visibility differs by command and format. JSON from `query`, `impact`,
and `neighbors` includes cache fields. Text output from `impact` and
`neighbors` starts with an `Index: cache-hit` or `cache-miss` header. Agent
output from `query` and `neighbors` normally uses that header, compacting it
to `I:hit`/`I:miss` under a tight byte budget and potentially omitting it under
an extreme cap. `query --format text` and `explain` report no cache state.
`def` and `explain` also skip the per-user fallback cache directory that the
other query commands use; details are in the
[operations cache guide](operations.md#cache).

## Analyze changes

| Command | What it does |
| --- | --- |
| `commit <ref>` | Entity-level change list for a commit vs its first parent, with heuristic dependent counts. |
| `diff --base A --head B` | The same between two refs. `analyze` is an alias of `diff`. |
| `checkpoint <id>` | Analyzes the commit behind an Entire-Checkpoint trailer. |
| `verify` | Runs a caller-provided test command and returns an adjudicated verdict. This executes the command you pass it; see [trust and security](trust-and-security.md). |

## Export

Bulk NDJSON streams. These default to the **committed tree**; pass
`--worktree` to stream the working tree instead.

| Command | What it does |
| --- | --- |
| `snapshot` | The whole graph: header, files, externals, symbols, relations, summary. Also supports `--format compact-ndjson` and the experimental `--format scip`. |
| `symbols` | Symbol records only. There is no name filter; grep the stream, or use `query`/`def` for a targeted lookup. |
| `edges` | Relation records, filterable server-side with `--to`, `--from`, `--relation`. |
| `snapshot-query` | Queries a saved compact snapshot without rebuilding the graph. |

Stream and record formats are specified in
[snapshot format](snapshot-format.md).

## Report

`stats` is a human-facing, read-only report of graph vs grep/read usage in
local coding-agent transcripts, with an explicitly modeled (not measured)
token-savings estimate. It is not part of the agent workflow; see
[trust and security](trust-and-security.md) for what it reads.
