# Cockpit Interaction & Liveness — Authoritative UX Spec (Go / Bubble Tea)

> This is a **build-to** spec for the Bubble Tea cockpit. It was written against the
> TypeScript version's weaknesses, but the Go rewrite is greenfield: **we build the
> end-state directly.** Where the TS guidance describes a 3-release rollout (derive
> stage → progress callbacks → ordered steps), the Go cockpit ships the final design
> from the first commit. No coarse `busy/thinking` model ever exists in Go.
>
> This must NOT disturb the split-footer / native-scrollback / normal-screen-buffer
> architecture (see `ui-cockpit.md`). Liveness lives entirely in the **live footer**;
> sealed turns commit to scrollback unchanged.

## The core idea

The active turn is driven by an **explicit run lifecycle**, not inferred from
`streaming && assistantText.length===0 && activities.length===0`. That inference is
the bug that makes spinners vanish during legitimate work (e.g. after a tool finishes,
before the next model token). Replace it with a first-class phase value.

## 1. Explicit run phase (build first)

```go
type RunPhase int
const (
    PhaseReceived RunPhase = iota // submission accepted
    PhaseAnalyzing                // waiting for first model output
    PhaseGenerating               // visible response tokens arriving
    PhaseToolQueued               // tool batch announced, none running yet
    PhaseToolRunning              // a tool is executing
    PhaseAwaitingApproval         // confirmation sheet up
    PhaseIntegrating              // tools done, model called again, no token yet
    PhaseCancelling               // Esc pressed, abort propagating
    PhaseComplete
    PhaseFailed
    PhaseCancelled
)
```

A `LiveRunStatus` component renders from this phase. Drive it from the phase value,
**never** from "is assistantText empty".

### Status vocabulary (use the most precise available state)

| Phase / situation                         | Text                                   |
| ----------------------------------------- | -------------------------------------- |
| Submission accepted                       | `Received`                             |
| Waiting for first model output            | `Analyzing request…`                   |
| Generic unknown activity (fallback)       | `Processing…`  (NOT "Thinking")        |
| Visible response tokens arriving          | no separate label (or `Generating…`)   |
| Reading / searching                       | `Inspecting project…`                  |
| Tool execution                            | tool-specific verb                     |
| All tools done, model called again        | `Integrating results…`                 |
| Approval needed                           | `Waiting for approval…`                |
| Escape pressed                            | `Cancelling…`                          |

- `Processing…` is the generic fallback. **Never** "Thinking" (too vague) and never
  "Generating" during tool use or approval (inaccurate).
- Do **not** use one label for the whole turn.

## 2. Acknowledgement line

Immediately on submit, synchronously show:

```text
◆ DAINTREE · received
⠋ Analyzing request · 0.4s
```

- Do **not** print `OK Daintree` / `OK, Daintree` — reads like the user talking to it.
- The ack must appear synchronously **but disappear the instant** a token or tool call
  arrives. **Never hold back real output to keep the ack visible.**

Then, as work emerges:

```text
├─ ✓ Read       src/ui/ControlRoom.tsx              38ms
├─ ⠋ Search     UI event handling                  1.2s
╰─ ◦ Read       src/agent/loop.ts                  queued
```

After tools finish, before the next token:

```text
⠋ Integrating results · 0.3s
```

Once prose streams, drop the separate status line — the moving text + caret carry
liveness. Stream completed lines as markdown, keep the unfinished line live with a
caret `▌`. **No typewriter effect, no artificial delay** — flush coalesced tokens
(16–33ms) straight through.

## 3. Announce the whole tool batch immediately

The model can return several tool calls. The agent loop must NOT reveal them one-at-a-time
as sequential dispatch begins. Instead:

1. Parse **every** returned tool call.
2. Emit a `toolBatch` event with all calls as `queued`.
3. Execute in the existing safe sequence (no unsafe parallelism).
4. Promote each `queued → active`.
5. Resolve each `done | failed | waiting`.

```text
├─ ⠋ Read       package.json                  active
├─ ◦ Search     "assistantToken"              queued
╰─ ◦ Read       TurnCellView.tsx              queued
```

> **Go agent-loop implication:** the event sink needs a `ToolBatch` event (all calls,
> queued) emitted before sequential dispatch, plus per-call promote/resolve events.
> Bake this into `agent/events.go` and the loop from the start.

## 4. In-tool progress events

The event contract must expose more than start + final result. Extend `ToolContext`:

```go
type ToolProgress struct {
    Phase     string // "validating" | "awaiting_approval" | "running" | "retrying"
    Message   string
    Completed int    // optional
    Total     int    // optional
}
// In ToolContext:
ToolCallID     string
ReportProgress func(ToolProgress)
```

The registry emits standard progress automatically (validating → awaiting_approval →
running). Long tools (extraction, terminal waits, MCP retries, agent-launch
reconciliation, watcher setup) report meaningful substeps instead of looking frozen:

```text
⠋ Spawn agent      validating request
⠋ Spawn agent      waiting for approval
⠋ Spawn agent      launching terminal
⠋ Spawn agent      attaching watcher
✓ Spawn agent       term_8 · watcher wch_1
```

## 5. Chronological order within a turn — ordered `TurnStep`s

This is the structural change the TS version defers to "release 3". **Go builds it first.**
Do NOT store one accumulated `assistantText` + a separate `activities` slice — that
renders `preamble → tools → conclusion` as `all prose → all tools`. Instead:

```go
type TurnStepKind int
const ( StepStatus TurnStepKind = iota; StepProse; StepTool; StepNote )

type TurnStep struct {
    Kind      TurnStepKind
    Phase     RunPhase   // StepStatus
    Text      string     // StepProse
    Streaming bool       // StepProse (last line live)
    Activity  *Activity  // StepTool
    Note      *SystemNote// StepNote
}

type TurnCell struct {
    // ...
    Steps []TurnStep
}
```

Gives a faithful live narrative:

```text
I'll inspect the relevant UI path.

├─ ✓ Read    useDaintreeController.go
╰─ ✓ Read    transcript.go

The key issue is that the richer stage value is calculated but not rendered…
```

When prose resumes after a tool batch, append a **new** `StepProse`, don't merge into
the earlier one.

## 6. Activity state glyphs — visually distinct

```text
◦ queued
⠋ active        (animated spinner, live elapsed time)
◇ waiting for approval
✓ complete
✗ failed
```

- Active rows animate the spinner (not a static glyph) and show live elapsed time.
- On failure show **both** target and failure summary; don't let the original `detail`
  hide the outcome.
- After completion a batch may compact to: `✓ Inspected 6 files · 412ms`.
- `Ctrl-X` (expanded detail) reveals full args, results, and individual rows.

## 7. Queued follow-ups are visible turns

A follow-up typed while busy must appear **immediately** as a dimmed queued turn, not
just `N queued`:

```text
YOU · queued 1
▏ Also check the classic REPL.
```

When it starts, **promote that existing turn in place** — don't create a second entry.

## 8. Esc is synchronous

On Escape, set phase to `Cancelling…` **synchronously** before the abort propagates, so
the UI never appears to ignore the key. (Composer-empty + busy Esc = pullback/cancel per
the composer contract.)

## 9. Splash does not gate interaction

Render the composer as soon as the Bubble Tea program is ready. Do **not** block input
on MCP / project-name resolution — let connection metadata settle asynchronously into the
masthead. Splash is cosmetic and Ctrl-C-skippable; it never holds the input path.

## Go build checklist (all in the first cockpit pass)

- [ ] `RunPhase` enum + `LiveRunStatus` component driven by phase, never by emptiness checks.
- [ ] Synchronous `◆ DAINTREE · received` ack that yields instantly to first token/tool.
- [ ] `ToolBatch` event: all calls announced `queued` before sequential dispatch.
- [ ] `ToolContext.ReportProgress` + registry auto-progress (validating/approval/running/retrying).
- [ ] Ordered `[]TurnStep` turn model (interleaved prose/tool/note), not flat text+activities.
- [ ] Distinct state glyphs, animated active spinner, live elapsed, failure shows outcome.
- [ ] Visible dimmed queued follow-up turns, promoted in place.
- [ ] Synchronous `Cancelling…` on Esc.
- [ ] Composer renders before MCP/project resolve; masthead settles async.
