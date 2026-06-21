# The Bubble Tea cockpit — architecture

The interactive cockpit is built on **Bubble Tea v2** (`charm.land/bubbletea/v2`, with
`bubbles/v2`, `lipgloss/v2`, and `glamour/v2` for markdown). This document is the
authoritative contract for how it renders and behaves. It replaces the old OpenTUI port
doc; the deeper build-to specs live in `docs/port/ui-transcript.md`,
`docs/port/ui-input.md`, and `docs/port/_interaction-ux.md`.

Everything that imports Bubble Tea lives under `internal/ui`. That is the UI boundary:
the runtime emits structured `agent.AgentEvent`s and the cockpit consumes them.

## 1. The inline, normal-screen-buffer model

The cockpit is **inline on the terminal's NORMAL screen buffer** — the Claude Code model,
deliberately *not* a full-screen takeover.

- A growing transcript lives in the **host terminal's own native scrollback**. The host
  owns the wheel, the scrollbar, selection, and copy/paste.
- A small **live footer** pinned to the bottom holds ONLY the in-flight turn, the status
  line, and the composer. The footer is the only region that repaints.
- Finished turns and the masthead are **committed once** to native scrollback — they
  become real terminal lines, scroll up, and never re-render.

`internal/ui/run.go` builds the program with **no alt screen and no mouse capture**
(bracketed paste on):

```go
prog = tea.NewProgram(
    m,
    tea.WithContext(ctx),
    tea.WithInput(os.Stdin),
    tea.WithOutput(os.Stdout),
)
```

In Bubble Tea v2 the model's `View()` returns a `tea.View` whose `AltScreen` field is
left `false` and whose `MouseMode` is the zero value `tea.MouseModeNone`. The `View()`
string is ONLY the live footer — never the whole transcript.

### Why not render the whole tree into a viewport

The TypeScript cockpit first tried OpenTUI `main-screen`: it repaints the entire tree
into a FIXED viewport and does NOT spill overflow into native scrollback, so the instant
the tree grew taller than the terminal the layout garbled. The Go equivalent of that
failure mode is "render the entire transcript into the `View()` string every frame" —
**do not do that.** Sealed content is committed to scrollback and dropped from `View()`.

## 2. The scrollback commit-queue protocol

Implemented in `internal/ui/scrollback.go`. Native scrollback is **append-only**: once a
row is printed it can never be edited or reordered. So commits happen **strictly in
transcript order, one at a time, with an ack before the next starts.**

Each committed thing is an immutable `ScrollbackBlock` (`BlockMasthead` | `BlockTurn` |
`BlockNote` | `BlockCommand`) carrying the styled `Rendered` string, a `Plain` fallback,
and the `Width` it was rendered at (load-bearing: a resize re-renders fresh at the new
width, never reflows in place).

The `scrollbackQueue` owns the frontier and the one-in-flight discipline:

- `headerDone bool` — has the masthead committed?
- `committed int` — how many transcript cells (from the front) are in scrollback;
  everything at index `>= committed` stays **live** in the footer.
- `resetKey int` — a monotonic `clearNonce + redrawNonce`; when it changes, reset
  `committed = 0` and `headerDone = false`. (Length alone can't detect a clear, because a
  `/clear` drops a fresh confirmation card that can make the new length equal the old
  committed count — so the reset is keyed, not inferred.)
- `inFlight bool` — a commit is awaiting its ack.

**The protocol:**

1. **Determine the head.** A cell is eligible once `isSealed` (a turn that left
   `TurnActive`; notes and command cells are immutable on arrival). The queue head is the
   masthead first (if not committed), then sealed transcript cells in index order from
   `committed` forward. The first non-sealed (active) cell blocks the frontier.
2. **Commit the head** with `commitCmd`: `tea.Sequence(tea.Println(text), …)` — prefer
   `Rendered`, fall back to `Plain`. `tea.Println` prints **above** the live program and
   persists across renders — that *is* the native-scrollback commit.
3. **Wait for the ack.** The sequence emits a `ScrollbackCommittedMsg{ID}` after the
   print flushes.
4. **On ack** (`ack`): clear `inFlight`; if the id is `"__header__"`, set `headerDone`;
   otherwise advance `committed++` (the sealed cell leaves the footer). Then start the
   next block, if any.

**Commit order is exact:** masthead first (so it lands on top of scrollback and scrolls
away above all history), then sealed cells in index order, one at a time.

## 3. The event pump and token coalescer

`internal/ui/pump.go` bridges the runtime to Bubble Tea. The agent runs on its own
goroutine and emits through an `agent.EventSink`; the pump implements that sink by
pushing each event onto a buffered channel.

Bubble Tea has no "subscribe to a channel" primitive, so the cockpit uses the **re-armed
`waitEvent` command**: a `tea.Cmd` blocks on the channel, returns the next event as a
`tea.Msg`, and `Update` re-arms an identical command after handling it — a self-sustaining
pump that never blocks the render loop.

A **token coalescer** sits in the pump: streamed assistant tokens are buffered and flushed
on a short tick (16–33 ms) or immediately before any non-token event, so the footer
repaints at a sane rate without a typewriter delay. **No artificial typing effect** —
flush coalesced tokens straight through.

## 4. Liveness: explicit RunPhase, ordered TurnSteps

Cross-reference: `docs/port/_interaction-ux.md` is the authoritative liveness spec.

The active turn is driven by a first-class `domain.RunPhase`, **never** inferred from
`streaming && assistantText == ""`:

```
Received → Analyzing → Generating → ToolQueued → ToolRunning
         → AwaitingApproval → Integrating → Cancelling
         → Complete | Failed | Cancelled
```

A `LiveRunStatus` component renders from the phase value (e.g. `Analyzing request…`,
`Integrating results…`, `Cancelling…` — the generic fallback is `Processing…`, never
"Thinking"). A synchronous `◆ DAINTREE · received` ack appears the instant a turn is
submitted and yields the moment a real token or tool call arrives.

A turn is an **ordered `[]TurnStep`** (`StepStatus` | `StepProse` | `StepTool` |
`StepNote`), NOT a flat `assistantText` string plus a separate `activities` slice. The
flat model renders `preamble → tools → conclusion` as `all prose → all tools`; the ordered
model preserves the true chronological narrative. When prose resumes after a tool batch,
**append a new `StepProse`** rather than merging into the earlier one.

The agent loop announces the **whole tool batch** as `queued` (one `ToolBatch` event)
before sequential dispatch, then promotes each `queued → active → done|failed|waiting`.
Activity glyphs are visually distinct (`◦ queued`, `⠋ active`, `◇ waiting`, `✓ complete`,
`✗ failed`); active rows animate the spinner and show live elapsed time.

## 5. The dedicated composer

`internal/ui/composer/` is a purpose-built editor model (not Bubbles' `textarea`). It owns
its buffer (a flat rune string with embedded `\n`), cursor offset, history, and kill-ring,
and the parent reaches in only through one explicit `Restore(text)` pull-back path. It
implements the full keymap: logical-line arrow nav with column memory, history recall at
line edges, `Ctrl-Y` yank, the trailing-backslash + Enter newline fallback,
modifier+Enter newline, verbatim bracketed paste, slash-command Tab completion, and "Esc
clears / Esc-empty-while-busy cancels". App chords (`Ctrl-C`, `Ctrl-O`, `Ctrl-X`, off-home
`Esc`) are handled by the shell in `Update`, routed by current view + focus (there is no
global key bus). See `docs/port/ui-input.md`.

## 6. The no-alt-screen / no-mouse contract (enforced)

These are hard rules; `internal/ui/view_test.go` enforces them:

- `TestViewOptions_NoAltScreenNoMouse` asserts the program's `tea.View` reports
  `AltScreen == false` and `MouseMode == tea.MouseModeNone`.
- `assertNoForbiddenEscapes` fails if `View()` ever contains `\x1b[?1049h` (alt-screen
  enter) or a mouse-tracking enable.
- A width test asserts a live footer line never lands in the autowrap column (it stays
  within `columns - gutter`).

Never enable `WithMouseAllMotion` / `WithMouseCellMotion`, never raw-parse SGR mouse mode,
never implement an internal scrollback viewport. The host owns scroll.

## 7. `/clear` — the only scrollback wipe

`internal/terminal/clear.go` holds the **only** sanctioned host-scrollback wipe:
`ClearHost` writes `\x1b[2J\x1b[3J\x1b[H` (erase viewport, erase scrollback, cursor home),
TTY-gated and error-swallowing. It is used by `/clear` (and, conceptually, a resize
redraw); it never touches the alternate buffer. Everything else goes through Bubble Tea's
managed render path — there is **no** raw per-frame painting.

## Package map (`internal/ui`)

| File / dir            | Responsibility                                                        |
| --------------------- | --------------------------------------------------------------------- |
| `run.go`              | cockpit entry (`Run(ctx, *app.App)`), builds the `tea.Program`        |
| `model.go`            | the root model, dependency wiring                                     |
| `update.go` / `update_handlers.go` | the `Update` reducer + message handlers (single-flight Send) |
| `view.go`             | the `tea.View` (live footer only) + program options                   |
| `pump.go`             | event pump (`EventSink` → channel → re-armed `waitEvent`) + coalescer  |
| `scrollback.go`       | the commit-queue protocol (`tea.Println`, one-in-flight, ack)         |
| `transcript_types.go` | `TranscriptCell` / `TurnCell` / `TurnStep` / `isSealed`               |
| `runstatus.go`        | `LiveRunStatus`, driven by `RunPhase`                                  |
| `render_*.go`         | turn / activity / chrome / approval / operations rendering            |
| `splash.go`           | the static embedded splash wordmark (cosmetic, Ctrl-C-skippable)      |
| `widths.go`           | width budget / gutter math                                            |
| `controller.go`       | confirm-hook + runtime callbacks into the program                     |
| `composer/`           | the dedicated editor model                                            |
| `theme/`              | colors, styles, splash gradient (`DAINTREE_THEME`)                    |
| `markdown/`           | glamour-backed markdown rendering                                     |
| `terminal/` (sibling) | the TTY-gated raw `/clear` wipe                                        |

## Forbidden anti-patterns

- **No alternate screen.** `AltScreen` stays false; it would kill host wheel/selection.
- **No mouse capture.** Never `WithMouseAllMotion` / `WithMouseCellMotion`.
- **No full-transcript viewport.** Never render the whole transcript into `View()`;
  sealed content lives in native scrollback only.
- **No resize replay.** Don't reflow committed blocks in place; re-render fresh at the new
  width (or rely on the host's native reflow). There is no OpenTUI-style replay record.
- **Never render markdown async from `View`.** `View()` is pure and synchronous; do
  markdown/glamour work in `Update` (or once when a cell seals), never as a side effect of
  rendering.
- **Single-flight `Send`.** Only one turn runs at a time; a follow-up typed while busy is
  a visible dimmed queued turn, promoted in place — not a second concurrent `Send`.
- **Never mutate the model outside `Update`.** All state changes flow through messages.
  The one sanctioned cross-goroutine path is `Program.Send` (the confirm-hook callback).
