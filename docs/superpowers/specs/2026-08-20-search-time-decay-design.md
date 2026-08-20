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
within a small epsilon (1e-4). Grid:

- categories: preference, convention, fact, pattern, architecture, decision,
  gotcha, dependency
- ages in days: 0, 1, 7, 30, 45, 90, 180, 365, 1000
- pinned: false, true

Clock discipline: `DecayRankingSQL` uses `julianday('now')` — SQLite's wall
clock at query time. To keep the comparison exact:

1. Capture a single reference `now := time.Now().UTC()` before the query loop.
2. Backdate each row's `created_at` in Go from that reference
   (`now.Add(-ageDays*24h).Format("2006-01-02 15:04:05")`) and insert it.
3. Run the SQL fragment for the row and compare its result against
   `decayFactor(category, pinned, ageDays)` computed with the **same
   reference `now`** — never re-derived after the query, so sub-second
   wall-clock drift between the Go timestamp and SQLite's `julianday('now')`
   cannot skew the comparison.
4. Insert each row with `importance = 1.0`, so the SQL fragment's result
   (`importance * CASE...`) is exactly the decay factor, isolating it from the
   importance multiplier.

This is the enforcement that keeps the two implementations from drifting when
either side changes in the future.

### 3. Apply decay in ranking, before truncation

In `fuseAndRank` and `SearchHybridAll`, multiply each candidate's fused RRF
score by `decayFactor(category, pinned, ageDays)` **before** sort + truncate.
This makes decay affect both membership (which memories are returned) and
ordering, matching `GetTopMemories` semantics — a fresh-but-just-below-cutoff
memory can be rescued, a stale one dropped.

Pipeline consequence: `fuseAndRank` currently hydrates (`GetByIDs`) only the
already-truncated `limit` window. Decay needs each candidate's
category/pinned/created_at, so hydration must move **before** truncation —
hydrate all `limit*2` candidates, compute `fused × decay`, sort, then
truncate. This is a slightly larger read but bounded by the existing `limit*2`
candidate pool; no schema change.

The FTS-only fallback paths (`SearchHybrid`/`SearchHybridAll` with no query
vector, and `SearchHybridParams`' FTS-only returns) must apply the same decay
**before** truncation, not after. Today they truncate-then-`applyRecency`
(`recencyRerank`); the staleness suite probes exactly this FTS-only path, so a
decay that only lived in the fused path would leave the suite measuring
nothing. Replace `recencyRerank` with a decay-aware variant that ranks
candidates by synthesized base score × decay, then truncates. Note that
`internal/memory/decisions.go:138`'s comment references `recencyRerank`'s
"truncate first" invariant — update that comment when the function's semantics
change (the decisions code itself is unaffected).

Age reads `created_at`, never `updated_at` (Upsert's strengthen path bumps the
latter). Unparseable `created_at` → treated as ancient (existing
`parseCreatedAt` behavior), so a malformed timestamp can never spuriously win.

The `applyRecency` prior and its `RecencyWeight`/`RecencyTau` fields are
removed from `SearchParams` — the decay factor subsumes it. The bench
staleness/recency harnesses (which currently sweep `RecencyWeight`) re-point at
the decay behavior (a toggle to compare decay-on vs decay-off).

### 4. Defaults

Decay is always on at the production default — no knob exposed to config or
callers. `DefaultSearchParams` no longer carries `RecencyWeight`/`RecencyTau`;
it gains a single `DecayEnabled bool` (default `true`) so the bench harness
can measure decay-on vs decay-off for the graded dataset and the replaced
sweep proofs. Production callers never set it to `false`.

`SearchParams` fields after this change:

```go
type SearchParams struct {
    FTSWeight      float64
    VecWeight      float64
    RRFK           int
    DecayEnabled   bool    // default true; bench-only toggle
    SupersedeDemote bool
}
```

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
3. Staleness/recency suites — **fixture category fix required first** (see
   below); fresh-versions-lift must not regress
4. Parity test passes (new)

### Bench fixture category fix (required)

The staleness suite (`internal/bench/staleness.go:108`), the recency-trap
suite (`internal/bench/recencytrap.go:88,99`), and the graded-bench
perturbation test all seed `Category: "fact"`. Under the decay design,
`fact` is a **never-decay** category (factor 1.0 always), so those suites
cannot observe decay at all — they would pass/fail identically before and
after this change, making the evaluation meaningless. Fix the fixtures:

- Switch the staleness suite's seeded memories from `"fact"` to a decaying
  category (`"decision"` or `"gotcha"`), so fresh-vs-stale lift is measurable.
- Switch the recency-trap suite's correct/trap memories the same way, so a
  stale-but-correct old memory's ability to outrank fresh traps is measurable
  against decay.
- Re-run both suites under the new decay ranking and record the before/after
  numbers in the PR description. The suites' existing assertions
  (`TestStalenessReport`, `TestRecencyTrapAtDefault`) must still pass; the
  proofs that sweep `RecencyWeight` are replaced per below.

### Replaced tests (RecencyWeight sweep proofs)

The following tests sweep `SearchParams.RecencyWeight`, which is removed by
this change, and their premises no longer hold:

- `TestStalenessRecencyProof` (`staleness_test.go:103`) — replaces with a
  decay-aware proof: with a `decision`/`gotcha`-category fixture, fresh-wins
  under decay must exceed fresh-wins under no-decay, and reach a majority.
- `TestRecencyFrontier` (`recencytrap_test.go:51`) — the tradeoff curve
  (staleness-fresh vs trap-correct vs recency weight) becomes a decay-on/off
  comparison. Replace the weight sweep with a decay-enabled vs decay-disabled
  sweep and record the frontier.
- `TestRecencyDoesNotPerturbGradedBench` (`staleness_test.go:145`) — asserts
  recency-on NDCG == recency-off NDCG because the graded dataset has uniform
  `created_at`. Replace with the equivalent claim that decay **cannot**
  reorder a dataset where every memory shares the same `created_at` (all
  factors equal → identical ranking), which is the property that makes the
  graded benchmark unaffected by decay.

The `RecencyWeight`/`RecencyTau` field removal breaks compilation in
`internal/bench/staleness_test.go`, `recencytrap_test.go`, and
`vector_test.go` (`TestApplyRecency`) — all updated in this change.

## Files touched

- `internal/memory/vector.go` — `decayFactor`, apply in `fuseAndRank` /
  `SearchHybridAll` and the FTS-only fallback paths, remove `applyRecency` +
  `RecencyWeight`/`RecencyTau`, replace `recencyRerank`, add `DecayEnabled`
- `internal/memory/decisions.go` — update the comment referencing
  `recencyRerank`'s truncate-first invariant (code unchanged)
- `internal/memory/vector_test.go` — update `TestApplyRecency`; new decay tests
- `internal/memory/store_test.go` (or new test file) — SQL-vs-Go parity test
- `internal/bench/staleness.go` — switch seeded category to a decaying one
- `internal/bench/recencytrap.go` — switch seeded category to a decaying one
- `internal/bench/staleness_test.go` — replace `TestStalenessRecencyProof`,
  `TestRecencyDoesNotPerturbGradedBench`
- `internal/bench/recencytrap_test.go` — replace `TestRecencyFrontier`
- `internal/memory/schema.go` — none (no schema change)