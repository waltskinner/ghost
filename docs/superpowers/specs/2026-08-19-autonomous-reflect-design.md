# Autonomous Reflect — Design

**Status:** Draft for review (2026-08-19).
**Author:** Wayne
**Builds on:** #297 "feat: autonomous memory lifecycle — auto reflect, supersede, and resolve"; #312 "fix(resolve): fall back to claude CLI when MCP sampling is unavailable".

## Context

`ghost reflect` consolidates a project's memories through a tiered consolidator
(`internal/reflection/consolidator.go`): an LLM tier (direct Anthropic API when
`ANTHROPIC_API_KEY` is set, or `claude -p` when the `claude` binary is on PATH),
falling back to a mechanical Jaccard dedup tier (`internal/reflection/tier_sqlite.go`)
that can only merge near-duplicates, not summarize, restructure, or re-scope.

On this machine the LLM path is unreachable: no `ANTHROPIC_API_KEY`, no `claude`
binary, and no Ollama (so embeddings are also off). The only LLM runtime present is
the `opencode` CLI (`~/.opencode/bin/opencode`), whose `run --format json` subcommand
emits a JSON-lines stream (`step_start` / `text` / `step_finish` events) that carries
the model's answer in the `text` events. MCP sampling is not an option either —
opencode's client does not implement `sampling/createMessage` (tracked as
`anomalyco/opencode#11948`), the same reason `ghost_resolve` gained a CLI fallback
in #312.

Separately, `ghost reflect` has no autonomous trigger. `ghost resolve` and
`ghost supersede` both spawn detached `--apply` processes from the Stop hook
(`spawnResolveIfConfigured` / `spawnSupersedeIfConfigured` in
`internal/mcpinit/stophook.go`, gated by `reflection.auto_resolve` /
`reflection.auto_supersede`). Reflect — the third leg of #297's lifecycle — has no
equivalent, so consolidation only happens when a user runs `ghost reflect` by hand.

This spec closes both gaps: a generalized subprocess-LLM client that can drive either
`claude` or `opencode`, and an `auto_reflect` Stop-hook spawn mirroring the two that
already exist.

## Goals

- Add an `opencode` LLM path for consolidation, generalized alongside the existing
  `claude` path behind one resolver so `ghost reflect` "just works" on machines that
  have either binary.
- Wire the resolver into the reflect tier system so `--tier auto` prefers haiku (API)
  → CLI (claude → opencode) → sqlite, and so `--tier cli` / `--tier opencode` select a
  specific backend explicitly.
- Add `reflection.auto_reflect` (default `false`) and a `spawnReflectIfConfigured`
  Stop-hook spawn that mirrors resolve/supersede exactly: pid-file serialization,
  detached `ghost reflect <project> --apply`, output to `reflect.log`.
- Make autonomous reflect a no-op when no real LLM tier is available, so it never
  fires a Jaccard-only consolidation (which would rewrite every non-manual memory for
  no quality gain).

## Non-goals

- No change to resolve/supersede. #312 already gave them a sampling→claude fallback;
  this spec does not add opencode to their fallback chain (the generalized client is
  usable there later, but nothing here rewires them).
- No in-session capture. `ghost_capture` / transcript extraction is a separate design
  (`docs/superpowers/specs/2026-08-17-autonomous-memory-capture-design.md`) on its own
  branch.
- No `SessionEnd` hook. The capture spec proposes relocating the lifecycle spawns to a
  once-per-session event; this spec deliberately mirrors the existing Stop-hook pattern
  instead, and defers any relocation to that work.
- No Jaccard / embedding improvement. Ollama is not running here, and the LLM tier is
  the requested fix; the sqlite tier is unchanged and remains the final fallback.
- No new config surface beyond `reflection.auto_reflect`. Backend selection is
  availability-driven (PATH), with explicit `--tier` overrides only.

## Design

### 1. Generalized subprocess-LLM client (`internal/ai/`)

Two sibling backends implement the existing `reflector` interface
(`internal/reflection/tier_haiku.go`) and the `Provider` interface
(`internal/ai/provider.go`):

- `CLIClient` (existing) — drives `claude -p`, plain-text stdout.
- `OpenCodeClient` (new) — drives `opencode run --format json --pure <prompt>`,
  parses the JSON-lines stream, and concatenates the `text` events' `part.text`
  fields into the answer.

A resolver picks the best available backend and exposes it behind the same two
interfaces, so the reflect tier treats `CLIProvider` like any other consolidator
client:

```go
// CLIProvider reports the best available CLI backend and delegates to it.
// It satisfies reflection's reflector interface and ai.Provider, so it drops in
// wherever CLIClient is used today.
type CLIProvider struct { backend backend }

// backend is the narrow capability the two CLI clients share: a name plus the
// exported Reflect/Classify shapes each already (or will) implement.
type backend interface {
    Name() string
    Reflect(ctx context.Context, prompt string) (string, TokenUsage, error)
    Classify(ctx context.Context, systemPrompt, userContent string) (string, error)
}

func NewCLIProvider() *CLIProvider            // claude if on PATH, else opencode if on PATH, else unavailable
func NewCLIProviderBackend(name string) ...   // explicit "claude" | "opencode", errors if absent
```

`CLIProvider` implements `Reflect` (returns `TokenUsage{}`, like `CLIClient`, since
subscription calls carry no per-token cost) and `Classify`. `OpenCodeClient.Classify`
inlines `systemPrompt` into the message because `opencode run` has no
`--system-prompt` flag — the same join `anthropicClient.Classify` already performs.

`OpenCodeClient` and the resolver return an explicit "unavailable" signal when the
binary is missing rather than erroring mid-call, so tier selection and the
no-LLM guard below can decide cheaply without spawning.

### 2. Reflect tier wiring (`cmd/ghost/main.go`)

The reflect command's tier switch gains an `opencode` case:

- `--tier haiku` — unchanged (requires `ANTHROPIC_API_KEY`).
- `--tier cli` — unchanged (requires `claude` on PATH).
- `--tier opencode` — new (requires `opencode` on PATH).
- `--tier sqlite` — unchanged.
- `--tier auto` (default) — haiku if a key is set, then `CLIProvider` (claude →
  opencode) if any CLI is on PATH, then sqlite as the always-available fallback.

The usage string and any tier-name validation are updated to include `opencode`.

### 3. `reflection.auto_reflect` (`internal/config/config.go`)

Add `AutoReflect bool `koanf:"auto_reflect"`` to `ReflectionConfig`, with a
`"reflection.auto_reflect": false` default in the `defaults` map — matching
`auto_resolve` / `auto_supersede`.

### 4. `spawnReflectIfConfigured` (`internal/mcpinit/stophook.go`)

Mirrors `spawnResolveIfConfigured` / `spawnSupersedeIfConfigured` step for step:

1. Return silently unless `cfg.Reflection.AutoReflect` is set.
2. Resolve the project from `cwd` (same read-only lookup + `ResolveProject`).
3. Guard on a `reflect-<projectID>.pid` file via the same `isAlive` / `claimPidFile`
   serialization (so only one reflect runs per project at a time).
4. Spawn detached `ghost reflect <project> --apply`, stdout/stderr to `reflect.log`.

One addition over the resolve/supersede twins: the **no-LLM guard**. Before spawning,
the hook checks whether any real LLM tier is available — `ANTHROPIC_API_KEY` set,
`claude` on PATH, or `opencode` on PATH. If none is, it returns silently without
spawning (optionally logging one line to `reflect.log`). This keeps autonomous reflect
from ever running a Jaccard-only `--apply` that would rewrite non-manual memories with
no consolidation quality gained.

## Guardrails

- **Hooks do zero DB/LLM work inline.** The spawn does only the small read-only
  project lookup it already does for resolve/supersede, then forks a detached child.
  The LLM call and the write happen in the child, exactly like today.
- **No LLM, no spawn.** See §4 — the guard is the one behavioral difference from the
  resolve/supersede pattern, and it is the point of this feature.
- **Recursion isolation for the opencode subprocess.** The detached `ghost reflect`
  spawns `opencode run`; that subprocess must not load Ghost's own MCP server or
  hooks (which would open the same SQLite DB mid-consolidation or let the subprocess
  model call ghost tools). The client runs with `--pure` (no plugins) and, if needed,
  a minimal/neutral config directory; it never passes `--continue` or `--session`, so
  no session context bleeds across runs. The exact isolation mechanism (flag vs.
  minimal config dir vs. env var) is pinned down in the implementation plan.
- **Opt-in destructive write.** `auto_reflect` defaults to `false`; the `--apply` it
  spawns runs `ReplaceNonManual`, the same destructive replace resolve/supersede
  already gate behind opt-in config. Pinned and manual memories are excluded by the
  #295 fix already in main.

## Data flow

```
Stop hook fires (per turn, Claude Code)
  └─ HandleStopHook
       ├─ spawnResolveIfConfigured(cwd)     (unchanged)
       ├─ spawnSupersedeIfConfigured(cwd)   (unchanged)
       └─ spawnReflectIfConfigured(cwd)     (new)
            ├─ auto_reflect off → silent return
            ├─ no LLM on PATH/key → silent return (no Jaccard-only run)
            └─ else claim pid file → detach `ghost reflect <project> --apply`
                 └─ reflect --tier auto
                      └─ haiku (key) → CLIProvider (claude → opencode) → sqlite
```

## Testing

- `OpenCodeClient` — a fake `opencode` binary on PATH emitting fixture JSON-lines:
  text is concatenated correctly across multiple `text` events; non-JSON or truncated
  output errors cleanly; a missing binary reports unavailable; a stalled subprocess
  honors the caller's context deadline (mirroring `CLIClient`'s `defaultTimeout`
  behavior).
- `CLIProvider` — prefers `claude` over `opencode` when both are present; falls back
  to opencode when claude is absent; reports unavailable when neither is; the explicit
  backend constructor errors on an absent binary.
- `spawnReflectIfConfigured` — config-gate (off → no spawn), no-LLM guard (no key and
  no CLIs → no spawn), pid-file claim (second concurrent call skips), and a successful
  spawn path, mirroring the existing resolve/supersede hook tests.
- Reflect CLI — `--tier opencode` selects the opencode backend; `--tier auto` resolves
  to the expected tier given each availability combination; a dry-run e2e with a fake
  opencode binary returns parsed memories without writing.
- `go vet ./...` and `go test ./...` before and after, feature branch + PR.

## Rejected alternatives

- **opencode as the only new client (no resolver).** Rejected — the user wants one
  generalized path that works whether a machine has `claude` or `opencode`, not a
  hard-coded preference for one.
- **Config-driven backend selection (`reflection.llm: claude|opencode|auto`).**
  Rejected — availability on PATH is enough; an extra key is surface to manage for no
  gain, and `--tier` already provides explicit override when needed.
- **Improve Jaccard instead (embeddings/minhash).** Rejected for this pass — Ollama
  isn't running here, and the requested fix is an LLM, not a better heuristic. The
  sqlite tier remains the final fallback unchanged.
- **Trigger on `SessionEnd`.** Deferred — correct for a heavy once-per-session pass,
  but it is new hook-event work already specced in the capture design; this change
  mirrors the existing Stop-hook pattern to stay focused.
- **Reuse `ghost_resolve`'s sampling→claude fallback for reflect.** Rejected —
  reflect runs headless from a detached child process, where there is no MCP session to
  sample from; the CLI subprocess is the only LLM path available there.
