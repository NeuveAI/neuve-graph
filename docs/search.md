# Search results and ranking

`entire graph query` turns a plain-language task description into ranked
source regions. This page describes what comes back and how to read it,
verified against current output. `entire graph query --help` documents the
flags. Supply the query with `--query`, or as one quoted argument after all flags:

```sh
entire graph query --repo . --format text "where is token refresh handled?"
```

Use one form at a time. Queries beginning with `-` require `--query`.

## Ranking

Ranking is hybrid: lexical scoring over bodies, identifiers (camelCase and
snake_case aware), signatures, and paths, expanded through the code graph to
callers, callees, usage sites, and same-container neighbors of strong lexical
candidates. Each result carries a `signals` array naming why it ranked (for
example `path`, `body`, `symbol-name`, `graph:callers`, `complete-symbol`), so
a surprising hit can be audited instead of trusted.

Results are byte-budgeted to drop into an agent's context: top hits carry full
snippets, later hits shrink to locators, and `--max-context-bytes` bounds the
total (`0` removes the bound). `--top-k` selects the base primary ranking.
Explicitly requested primary enrichment such as `--callee-hop` may then add
candidate fix sites, followed by up to four independently ordered `related`
locators and one final `covering-test` entry. To remain inside the byte ceiling,
those supplements may replace eligible redundant primary tail locators. They
never reorder retained primary evidence, displace the protected head, or remove
a file's last locator, so the final primary count may be below its
pre-supplement count. Rank is the authoritative policy order; `score` reports
the underlying relevance evidence and need not descend after stable policy
interleaving or additive enrichment.

## What a response contains

The default format is JSON, one object per query:

- `results`: retained base primary hits, requested primary enrichment, and then
  any bounded supplements. Each has `file_path`, `start_line`/`end_line`, a
  `focus_line`, the symbol's `kind`, `qualified_name`, and `signature` when the
  hit is a known symbol, the `signals` that ranked it, and a source `snippet`.
  Trailing entries in `section: "related"` are zero-scored callers, siblings,
  or near duplicates. At most four appear, ordered by the related-site policy.
  A final `section: "covering-test"` entry may follow and is capped at one.
- `literal_cluster`: occurrences of a distinctive literal from the top hits
  across the repository, tagged by role (edit site vs consumer).
- `verify_command`: a suggested check, with the evidence it was derived from
  and a `tier`: `narrow` (a focused test), a suite fallback, a build check, or
  a residual floor when nothing narrower can be derived. Text and agent
  formats render it as a `VERIFY:` line. It is a suggestion constructed from
  repository contents; read it before running it (see
  [trust and security](trust-and-security.md)).
- `stats`: counts, byte accounting, latency, and `index_cache_hit`.
- `warnings`, `partial_failures`, `completeness`: machine-readable coverage:
  which languages and how many files/symbols/relations backed this answer, and
  what was skipped or failed.

## Formats

- `json` (default): everything above; what the installed agent guide's
  command produces.
- `ndjson`: the same data as a record stream with a trailing
  `search_summary`.
- `text`: human-readable tiers with full snippets for top hits, terse locators
  after, then the `VERIFY:` line. Does not report cache state.
- `agent`: compact ranked output opening with a latency/cache header
  (`Index: cache-hit (…ms) | Query: …ms | …`), degrading gracefully under
  tight byte budgets.

## Profiles

`query` defaults to `--profile fast` (shallow, high-precision call
resolution). The installed agent guide asks for `--profile full`, which
enables the complete relation set and deeper graph expansion. Profile is part
of the cache key, so mixing profiles across runs builds separate cache
entries. See the [operations cache guide](operations.md#cache).
