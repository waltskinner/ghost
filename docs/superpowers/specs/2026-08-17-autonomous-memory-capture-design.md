# Autonomous memory creation: in-session transcript capture

**Status:** Revised 2026-08-17 (twice) — added client scope and one flagged open
question; then corrected the SEP citations for MCP sampling's deprecation and added a
live reproduction showing `ghost_resolve`'s sampling call currently fails in this
environment. Pending re-review; the sampling-vs-CLIClient direction for `Extract` is
unresolved.
**Author:** Wayne (wcatz)
**Builds on:** #297 "feat: autonomous memory lifecycle — auto reflect, supersede, and resolve"

---

## 1. Problem

Ghost has a fully working memory *maintenance* story (reflect/resolve/supersede)
and an explicit memory *creation* path (`ghost_memory_save` etc.), but **creation is
entirely on the assistant to remember**. Nothing creates memories on its own. The
stop hook only *nudges* — it scans the transcript, counts saves, and if a tool-using
turn ends with zero saves it blocks once with "you used tools but saved nothing".
The nudge relies on the model deciding *what* to save, and it can't distinguish
"nothing worth saving" from "five important discoveries the model forgot".

Three concrete gaps:

1. **No autonomous creation.** Memories appear only when the model explicitly calls a
   save tool. A long session that never saves loses everything it learned.
2. **Detail is lost at compaction/session-end.** There is no "save before you forget"
   point. Compaction summarizes context away; session end discards the transcript.
3. **Lifecycle runs on the wrong event.** `spawnResolveIfConfigured` and
   `spawnSupersedeIfConfigured` live in the `Stop` hook, which fires **every turn** —
   their full prologue (config load → `sql.Open` → `ResolveProject` → PID check) runs
   per turn when enabled. That work is semantically session-scoped and belongs on the
   once-per-session `SessionEnd` event.

---

## 2. Client scope

This design is written entirely against Claude Code's own hook contract, not the MCP
specification. The mechanism below depends on Claude-Code-specific stdin/stdout fields —
`transcript_path`, `stop_hook_active`, `additionalContext`, `decision: "block"` — and on
a brand-new `SessionEnd` event that only Claude Code fires. None of this is part of MCP
itself; it is the same Claude Code extension surface the existing `SessionStart`/`Stop`
hooks already use.

This runs deeper than naming. `transcript_path` exists because Claude Code persists every
session as a durable on-disk transcript and hands hooks its path — that durability is what
lets `ghost_capture` extract independently of the calling model's own memory or diligence.
A client without an equivalent durable, hook-delivered transcript has no substitute to
drop in: the obvious-looking fix, having the calling model pass its own recent
conversation as a tool argument instead of a file path, just re-creates "Model-driven
notes" below under a different name — the model would be authoring that content from its
own context rather than Ghost reading an independent record, the exact diligence problem
this design exists to avoid. So this isn't an oversight to patch later; the design
intentionally trades portability for independence from the calling model.

Concretely, on opencode, Cursor, or any other MCP client, today:

- `ghost_capture` is a normal MCP tool, registered and callable everywhere Ghost runs —
  but nothing here nudges a non-Claude-Code model to call it, and there is no transcript
  for it to read even if invoked manually.
- The lifecycle spawns relocated to `SessionEnd` (below) do not run at all — `SessionEnd`
  is a new Claude Code hook event with no MCP-standard equivalent.
- Ghost's portable baseline is unaffected: tools/resources/prompts and the MCP
  `Instructions` field keep working on every client regardless of this design.

Two open dependencies worth investigating before assuming permanent Claude-Code-only
status, neither of which this spec resolves:

1. **Does the client support MCP sampling (`CreateMessage`) at all?** Not a new risk —
   `ghost_resolve` already depends on this today, on every client — and no longer
   theoretical: live-tested against this exact Claude Code session while writing this
   revision, `ghost_resolve` fails with `mcp sampling: calling "sampling/createMessage":
   Method not found`. That is a plain JSON-RPC "unimplemented method" response from the
   client, not the version-gated rejection `assertServerInitiatedRequestAllowed` would
   produce server-side (see Guardrails) — evidence this session's negotiated protocol
   predates the 2026-07-28 cutover and the client simply never answers this method,
   rather than Ghost being on the wrong side of a version negotiation. Root cause (this
   Claude Code build, the VSCode-extension surface specifically, or something
   environment-local) not further isolated. Separately, opencode's MCP client does not
   implement sampling either (tracked upstream as `anomalyco/opencode#11948`, open). The
   new `Extract` method (Components, below) would inherit the same failure mode; see
   Guardrails for the two related SEPs bearing on whether this is likely to improve.
2. **Does the client expose any session-boundary or pre-continuation event Ghost could
   hook into?** Unknown for opencode and others — not investigated here.

Neither resolves to "no" — only "not attempted." If either turns out favorably for a
given client, capture and the `SessionEnd` lifecycle could plausibly be ported; until
then, read this as a Claude Code feature, not a Ghost-wide one.

---

## 3. Core concept: the session's own model reads its own transcript

The pivotal constraint, discovered during research: **MCP sampling only sees what the
server sends.** `ai.SamplingProvider.Classify` passes exactly `systemPrompt +
userContent` (and caps `MaxTokens: 16`); the sampled model has no access to the
conversation context. Therefore autonomous creation must hand the raw material to the
sampling call itself — and that raw material is the **transcript**, which the hook
already knows how to locate (`transcript_path` arrives on every hook's stdin).

The design adds `ghost_capture`: an MCP tool that reads a bounded slice of the current
session's transcript and asks the *calling session's own model* (via MCP sampling, zero
Anthropic credits) to extract memories from it. The tool supplies the transcript slice,
dedup context, and the write gate; the sampling model does the extraction.

**Trigger** is a nudge from the `Stop` hook (which supports `additionalContext`, a
non-error guidance that continues the turn). Because `Stop` fires every turn, capture
runs *as discoveries surface* — by the time compaction or session end arrives, detail is
already in the DB. This is the direct answer to the "pre-compaction vs end-of-session"
question: **neither** — capture per-turn so detail is never at risk.

---

## 4. Mechanism

### 4.1 `ghost_capture` MCP tool

Signature: `ghost_capture(project_id, transcript_path?)`. `transcript_path` is optional;
when omitted the tool reads the pending marker (see §4.3) for the latest session.

Flow:

1. **Locate the transcript.** Prefer the explicit `transcript_path` arg; otherwise read
   the pending marker (`<dataDir>/capture-pending.json`). No marker and no arg → return a
   short "nothing pending" message, not an error.
2. **Resume from the capture cursor.** A per-project state file
   (`<dataDir>/capture-state-<projectID>.json`) records the last-captured transcript
   path and byte offset. Same path → stream from the offset (incremental); different
   path (new session) → stream from 0.
3. **Bound the slice.** Stream from the cursor up to `capture.max_transcript_bytes`
   (default 64 KiB), condensing tool results (large and noisy) to a short elision so the
   slice carries *conversation and tool intent*, not megabytes of file dumps.
4. **Sampling call.** `systemPrompt` = extraction instructions with the JSON schema
   `{"memories":[{"content","category","importance","confidence"}]}` and the category
   enum; `userContent` = the existing project memories (dedup hint) + the condensed
   slice. `MaxTokens` ~2000.
5. **Parse + validate.** Clamp importance/confidence to `[0,1]`, validate category.
6. **Confidence gate.** A candidate is **auto-saved** when
   `confidence ≥ capture.confidence_threshold` **and**
   `importance ≥ capture.importance_threshold`. Everything else is returned as a
   *proposal* for the model to save explicitly. Auto-save reuses the same `Upsert`
   path `ghost_memory_save` uses, so FTS dedup, embedding, linking, and the
   un-resolve-on-write revive all fire; then `notifyProjectResource(projectID, "context")`
   like the other write tools.
7. **Empty-set guard.** Zero candidates → no write, just "nothing new to save".
8. **Commit.** Advance the capture cursor, clear the pending marker, and return a
   summary of saved vs proposed memories.

### 4.2 Refined `Stop` hook

When `capture.enabled`, `ghost hook stop` becomes:

1. `stop_hook_active` → return early (unchanged — the turn is already continuing due to a
   prior hook, so never re-nudge).
2. Scan the transcript as today: count tool calls, `ghost_memory_save`/`ghost_save_global`
   calls, **and** `ghost_capture` calls.
3. No tool calls → return.
4. Saves or captures present → clear the pending marker and return (already recorded).
5. Read the pending marker. If it is for the **same session** and already marked
   `nudged`, emit the original `decision:block` fallback and return.
6. Otherwise write the marker (`session_id`, `transcript_path`, `cwd`, `nudged: true`) and
   emit **`additionalContext`** (not `decision:block`):
   > "This session used tools but saved nothing to Ghost. Call `ghost_capture` to
   > extract discoveries from this session automatically."

The `nudged` flag is what makes "nudge once, then block" work. The immediate follow-up
`Stop` after an `additionalContext` continuation carries `stop_hook_active: true` and
returns at step 1 — so the block only fires on a *later* turn in the same session that
still recorded nothing. Any save or capture clears the marker via step 4, resetting the
cycle for the next burst of unrecorded work. The `stop_hook_active` guard plus Claude
Code's 8-consecutive-continuation cap bound both mechanisms, exactly as today.

### 4.3 `ghost hook session-end` (new event)

New `SessionEnd` hook moves the lifecycle spawns off the per-turn `Stop` path:

- `spawnResolveIfConfigured`, `spawnSupersedeIfConfigured` (and, from #297,
  `spawnReflectIfConfigured`) move from `HandleStopHook` into `HandleSessionEndHook`.
- `SessionEnd` fires once at termination and supports only side effects (no decision
  control) — which is precisely what fire-and-forget detached spawns need. Its 1.5s
  budget comfortably covers the existing read-only project lookup + `exec.Command`
  start; raise it via the hook's `timeout` field if a slow first-run proves otherwise.
- The `Stop` hook keeps the nudge (which genuinely needs per-turn + block/additionalContext);
  the lifecycle keeps its PID-file serialization and detached `--apply` semantics,
  unchanged.

### 4.4 Config

```go
type CaptureConfig struct {
    Enabled             bool    `koanf:"enabled"`               // default TRUE (opt-out)
    ConfidenceThreshold float64 `koanf:"confidence_threshold"`  // default 0.8
    ImportanceThreshold float64 `koanf:"importance_threshold"`  // default 0.5
    MaxTranscriptBytes  int     `koanf:"max_transcript_bytes"`  // default 65536
}
```

`reflection.auto_reflect` (default `false`) lands here per #297, completing the
reflect/resolve/supersede trifecta on `SessionEnd`.

**Open question: `capture.enabled` defaults to `true`, while everything else in this
config defaults to `false`.** #297 established "opt-in, not opt-out" for `auto_resolve`,
`auto_supersede`, and (per its own text) `auto_reflect` — all three spawn detached
`--apply` processes that mutate existing memories headlessly. `capture.enabled`
diverges: it's on by default even though it's the newer, less-proven mechanism. The case
for keeping it on: capture is gated (confidence ≥ 0.8 and importance ≥ 0.5 to auto-save),
routes everything below the gate to a proposal instead of a write, spends no Anthropic
credits, and a capture feature that defaults off reproduces the exact "entirely on the
assistant to remember" gap described in the Problem section for anyone who never finds
the config key — arguably a different, lower risk profile than headless `--apply`
lifecycle spawns. The case for flipping it to `false`: it's still new, unvalidated
against real transcripts, and it's the one place this draft departs from an
already-established project convention. Flagged for a decision rather than resolved
here.

---

## 5. Guardrails (hard)

- **Hooks do zero DB/LLM work.** The `Stop` hook writes a marker and emits text only.
  `SessionEnd` performs the same small read-only project lookup + detached spawn the
  existing spawn helpers already do — no LLM, no write inline.
- **Sampling only, no credits.** `ghost_capture` is constructible only with a live MCP
  session (`req.Session != nil`), mirroring `ghost_resolve`; headless invocation fails
  fast with a clear message. No Anthropic API key is ever touched by capture.
- **No blind writes.** Confidence gate + empty-set guard + existing upsert dedup. A
  low-confidence or empty extraction never mutates the store.
- **Never trap a session.** The nudge is `additionalContext`; the block is a bounded
  fallback; both are loop-protected by `stop_hook_active` and the 8-continuation cap.
- **Untrusted content stays delimited.** Extracted memory text is stored data, and the
  existing `«…»` delimiter guard at injection continues to apply. The extraction prompt
  instructs the model to treat the existing-memories block as data, not instructions.
- **Single-active-session marker.** `capture-pending.json` is one well-known file; two
  concurrent foreground sessions in different projects race on it (last-writer-wins).
  The explicit `transcript_path` arg is the escape hatch. Acceptable for a single-user
  local tool — documented here rather than silently ignored.
- **Sampling is failing live, today, not just at future risk.** Live-tested against
  the Claude Code session used to write this revision: `ghost_resolve` — the one
  shipped tool built on `ai.SamplingProvider` — currently fails with
  `mcp sampling: calling "sampling/createMessage": Method not found`. That is a
  present-tense reproduction, not a projection. Building `Extract` the same way would
  ship a second broken tool, so this must be resolved before `Extract` is implemented,
  independent of anything below. Not yet resolved as of this revision — see §2's open
  dependency above; no direction is decided in this document.
  Two SEPs bear on why this is unlikely to self-resolve, and an earlier draft of this
  bullet cited them sloppily — it called "SEP-2322" wrong and replaced it with
  SEP-2577 as if one superseded the other. Both are real, both Final, and they say
  different things:
  - **SEP-2322** ("Multi Round-Trip Requests," Final) replaces synchronous
    server-initiated requests (`CreateMessage`, `ListRoots`, `Elicit`) with an async
    `InputRequests`/`InputRequiredResult` pattern once a client negotiates protocol
    version ≥ 2026-07-28 — enforced server-side in `go-sdk@v1.7.0` by
    `assertServerInitiatedRequestAllowed`. This is what the original (accurate) test
    comment in `mcpserver_test.go` was describing. The live failure above did not
    produce this specific rejection, which is evidence (not proof) that this session's
    negotiated protocol predates the cutover and this SEP isn't what's currently
    biting — but it will start to once a connecting client negotiates the newer
    version, and `ai.SamplingProvider` has no InputRequests-shaped call path today.
  - **SEP-2577** ("Deprecate Roots, Sampling, and Logging," Final) deprecates the
    sampling capability itself, protocol-wide, as of the same 2026-07-28 version —
    wire behavior is unaffected during a deprecation window (functional at least a
    year, rolling per-version), but new implementations are told not to add support
    for it, and the SEP's stated alternative is "integrate directly with LLM provider
    APIs."
  Both push the same direction: don't build new sampling call sites. Ghost already has
  a non-deprecated, protocol-agnostic option in-tree — `ai.CLIClient` (subscription-
  billed `claude -p` subprocess, no API key, works with or without an MCP session,
  already the resolve/supersede fallback when no key is configured). It preserves the
  independent-extraction property sampling gives `Extract` — the transcript slice and
  instructions are supplied as CLI arguments to a freshly spawned process, not read
  back from the calling model's own memory — at the cost of one process spawn per call
  rather than one RPC.

---

## 6. Data flow

```
Stop hook fires (per turn)
  → scan transcript (toolCalls, saves, captures)
  → toolCalls>0 && saves==0 && captures==0 ?
       → marker already marked `nudged` for this session? → decision:block (fallback)
       → else → write marker {session_id, transcript_path, cwd, nudged:true}
              → additionalContext: "call ghost_capture"

Model calls ghost_capture(project_id)
  → read marker → transcript_path
  → read capture cursor (byte offset)
  → stream bounded slice from offset
  → sampling(extraction prompt + existing memories + slice)
  → parse candidates
  → gate: auto-save high-conf/high-importance; propose rest
  → advance cursor, clear marker, report

SessionEnd hook fires (once)
  → spawn detached reflect --apply / resolve --apply / supersede --apply (PID-serialized)
```

---

## 7. Components & boundaries

| Unit | Purpose | Depends on |
|---|---|---|
| `internal/capture` | transcript slicing, prompt build, JSON parse, confidence gate | `internal/ai` (sampling), `internal/memory` (types) |
| `ai.SamplingProvider.Extract` | configurable-`MaxTokens` sampling call (vs 16-token `Classify`) | MCP `CreateMessage` |
| `ghost_capture` tool handler | wires store + sampling + capture pkg; marker/cursor read-write | `internal/capture`, `internal/mcpserver` |
| `HandleStopHook` refine | marker write + additionalContext nudge + block fallback | `internal/mcpinit/stophook.go` |
| `HandleSessionEndHook` (new) | detached reflect/resolve/supersede spawns | `internal/mcpinit` |
| `cmd/ghost` hook wiring | `ghost hook session-end` subcommand + init/status entries + `mcp__ghost__ghost_capture` permission allowlist | `internal/mcpinit` |
| `internal/config` | `capture.*` + `reflection.auto_reflect` | koanf defaults |

Each unit is independently testable: the capture package against a fixture transcript;
the tool handler against a fake sampler + seeded store (mirroring the existing
`ghost_resolve` test); the stop/session-end hooks against canned stdin JSON.

---

## 8. Rejected alternatives

- **`PreCompact` trigger** — the natural "save before you forget" point, but the event
  supports only `decision: "block"` (it discards `systemMessage`/`continue` and has no
  `additionalContext`), so it cannot nudge the model to call a capture tool. Worse,
  blocking auto-compaction triggered by an already-returned context-limit error makes the
  current request fail. Deferred; a future "block manual `/compact` to remind the user"
  remains possible but is not autonomous.
- **Detached Anthropic-API capture** (paid, works without a live session) — rejected in
  favor of free in-session sampling; a headless path could be added later behind
  `capture.provider` without disturbing this design.
- **Model-driven notes** (`ghost_capture(notes=…)` where the model summarizes) — higher
  signal-to-noise but still depends on the model's diligence, defeating the autonomy
  goal. Transcript-driven was chosen.
- **Client-agnostic transcript delivery** (`ghost_capture(transcript_text=…)` instead of
  reading `transcript_path` server-side) — considered while scoping this design across
  MCP clients. Rejected: the calling model would have to author or paste its own recent
  conversation into the argument, which is mechanically the same thing as "Model-driven
  notes" above — the model reporting on itself rather than Ghost reading an independent
  record — plus it burns the calling model's own output tokens reproducing content it
  already lived through. Durable, hook-delivered file access isn't an incidental
  implementation choice; it's why this design gets independent extraction without
  relying on the calling model's cooperation. See "Client scope" above.
- **Daemon/cron for lifecycle** — out of scope; noted as future in #297.
- **Keeping lifecycle on `Stop`** — per-turn prologue overhead is the bug this design
  fixes; `SessionEnd` is the semantically correct event.

---

## 9. Testing

- **Capture package** — fixture transcript: bounding at `max_transcript_bytes`, cursor
  resume across calls, tool-result elision, prompt construction, JSON parse, category/
  range validation, confidence gate (auto-save vs proposal split), empty-set guard.
- **Tool handler** — fake sampler + seeded store: saved list correct, proposals returned,
  cursor advanced, marker cleared; `req.Session == nil` fails fast.
- **Stop hook** — canned stdin: marker written; nudge emitted on `toolCalls>0, saves==0,
  captures==0`; pass-through on saves/captures; block fallback on the second fire; silent
  on `stop_hook_active`.
- **Session-end hook** — detached spawns gated by PID file; no spawn when disabled or no
  matching project.
- **Config** — `capture.*` defaults (`enabled=true`, thresholds 0.8/0.5); `auto_reflect`
  default `false`.
- **Sampling capability absent or unresponsive** — a live session exists but the client
  doesn't implement `CreateMessage`, or the call times out: `ghost_capture` returns a
  clear error and never blocks or silently retries.
- `go vet ./...` before commit; feature branch + PR.
