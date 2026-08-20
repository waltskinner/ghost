# Design: unify time-decay ranking into ghost_memory_search

Date: 2026-08-20
Issue: #316
Status: Approved

## Problem

Time-decay scoring (`DecayRankingSQL`, `internal/memory/store.go:723`) is
category-aware and applied by `GetTopMemories` and the session-start hook — but
not by the interactive `ghost_memory_search` tool. The hybrid RRF search path
(`fuseAndRank`, `internal/memory/vector.go:287`) has an optional recency prior,
`applyRecency` (`vector.go:243`), but `RecencyWeight` defaults to `0` — a
documented no-op.

Net effect: `ghost_memory_search` results have zero time-awareness. A stale
`decision`/`gotcha`/`dependency` memory can surface via explicit search even
though it would never make the decay-ranked, auto-injected "top memories"
summary at session start. The two retrieval surfaces disagree on relevance for
the same data.

## Design

### 1. Single Go `decayFactor` function

Add to `internal/memory/vector.go`:

```go
// decayFactor returns the category-aware time-decay multiplier for a memory
// given its age in days. It mirrors DecayRankingSQL exactly (see the parity
// test below): pinned memories and preference/convention/fact never decay
// (factor 1.0); pattern/architecture decay with tau 45 and a 0.3 floor; all
// other categories (decision, gotcha, dependency, ...) decay with tau 30 and a
// 0.15 floor.
func decayFactor(category string, pinned bool, ageDays float64) float64
```

`DecayRankingSQL` stays as-is — it remains the source of truth for
`GetTopMemories` and the session-start hook, which do their ranking in SQL.

### 2. Parity test (drift guard)

The issue explicitly calls out the current split (SQL constant vs Go recency
prior) as a drift hazard. A parity test in `internal/memory` evaluates both
`DecayRankingSQL` (via a real DB, one row per category × age × pinned
combination) and the Go `decayFactor`, asserting the factors are identical
within a small epsilon. Grid:

- categories: preference, convention, fact, pattern, architecture, decision,
  gotcha, dependency
- ages in days: 0, 1, 7, 30, 45, 90, 180, 365, 1000
- pinned: false, true

This is the enforcement that keeps the two implementations from drifting when
either side changes in the future.

### 3. Apply decay in ranking, before truncation

In `fuseAndRank` and `SearchHybridAll`, multiply each candidate's fused RRF
score by `decayFactor(category, pinned, ageDays)` **before** sort + truncate.
This makes decay affect both membership (which memories are returned) and
ordering, matching `GetTopMemories` semantics — a fresh-but-just-below-cutoff
memory can be rescued, a stale one dropped.

The FTS-only fallback paths (`SearchHybrid`/`SearchHybridAll` with no query
vector, and `SearchHybridParams`' FTS-only returns) must apply the same decay
**before** truncation, not after. Today they truncate-then-`applyRecency`
(`recencyRerank`); the staleness suite probes exactly this FTS-only path, so a
decay that only lived in the fused path would leave the suite measuring
nothing. Replace `recencyRerank` with a decay-aware variant that ranks
candidates by synthesized base score × decay, then truncates.

Age reads `created_at`, never `updated_at` (Upsert's strengthen path bumps the
latter). Unparseable `created_at` → treated as ancient (existing
`parseCreatedAt` behavior), so a malformed timestamp can never spuriously win.

The `applyRecency` prior and its `RecencyWeight`/`RecencyTau` fields are
removed from `SearchParams` — the decay factor subsumes it. The bench
staleness/recency harnesses (which currently sweep `RecencyWeight`) re-point at
the decay behavior (a toggle to compare decay-on vs decay-off).

### 4. Defaults

Decay is always on at the production default — no knob. `DefaultSearchParams`
no longer carries a recency weight. The bench sweep keeps a way to toggle decay
so the graded dataset can quantify the impact, but production always applies
it.

### 5. Not in scope

- `pinned` as a 1.5x multiplier vs its current full-exemption (factor 1.0).
  Decay keeps the existing semantics: pinned → 1.0, a no-op for
  preference/convention/fact which already never decay. (Flagged as a
  separate, narrower question in the issue.)
- Whether importance should enter search relevance. Decay applies to the fused
  RRF score only; importance stays a property of `GetTopMemories` ranking.

## Evaluation

This is a scoring change, not a pure bugfix — it reweights search relevance and
could regress recall on the graded dataset. Before merge:

1. `go vet ./...` and `go test ./...`
2. `ghost bench` — graded dataset (recall must not regress)
3. staleness/recency suites (`ghost bench` sweep) — fresh-versions-lift must not
   regress
4. Parity test passes (new)

## Files touched

- `internal/memory/vector.go` — `decayFactor`, apply in `fuseAndRank` /
  `SearchHybridAll` and the FTS-only fallback paths, remove `applyRecency` +
  `RecencyWeight`/`RecencyTau`, replace `recencyRerank`
- `internal/memory/vector_test.go` — update `TestApplyRecency`; new decay tests
- `internal/memory/store_test.go` (or new test file) — SQL-vs-Go parity test
- `internal/bench/staleness.go`, `recencytrap.go` — re-point recency knobs
- `internal/memory/schema.go` — none (no schema change)