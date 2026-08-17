# The Bubble Tea cockpit — architecture

The interactive cockpit is built on **Bubble Tea v2** (`charm.land/bubbletea/v2`, with
`bubbles/v2`, `lipgloss/v2`, and `glamour/v2` for markdown). This document is the
authoritative contract for how it renders and behaves.

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

### The pre-program splash hand-off

The boot splash is painted with **raw ANSI, before `tea.NewProgram` exists**
(`internal/ui/run.go` → `playBootSplash`). The final splash frame *is* the complete
cockpit — masthead above a live footer — so Bubble Tea adopts an already-painted screen
as its live region and there is never a blank or footer-only frame. Two pieces of state
carry the hand-off into the program:

- `m.queue.headerDone = true` — the masthead is already on screen, so the commit queue
  must not print a duplicate.
- `m.handoffCols` / `m.handoffRows` — the dimensions it was painted at. The first
  `WindowSizeMsg` validates them, so a genuine resize in the narrow hand-off→program gap
  triggers a full redraw instead of preserving stale wrapping.

**Consequence for anyone reading `model.go`:** the in-program 3-gate boot lock
(`booting` / `startupSettled` / `animationDone` / `projectSettled`, `completeBoot`,
`BootCapMsg`) is **legacy and inert in production** — `NewModel` sets `booting: false`
(`model.go:247`). Every `!m.booting` guard therefore fires on every launch. Don't reason
about startup as if that lock still runs; don't delete it without checking the non-TTY
and test paths that still construct a model directly.

### Why not render the whole tree into a viewport

Repainting the entire transcript into a FIXED viewport — the whole transcript in the
`View()` string every frame — does NOT spill overflow into native scrollback, so the
instant the tree grows taller than the terminal the layout garbles. **Do not do that.**
Sealed content is committed to scrollback and dropped from `View()`.

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

## 3. The incremental flush — the ACTIVE turn streams into scrollback

`internal/ui/flush.go`. The commit queue above handles *sealed* cells. But a long turn
would still grow the footer without bound before it seals, and a tall live footer is
exactly what triggers the `tea.Println` corruption class below. So the **active** turn
also flushes, continuously:

- `finalizedStepCount` finds the turn's IMMUTABLE leading steps. A prose/note step is
  final once it is no longer the last step. A contiguous tool run is final only when
  every activity in it is terminal **and** it is closed by a following non-tool step — so
  a branch tree commits atomically and is never split across the flush frontier.
- Beyond that frontier, the live prose step commits **line by line**: every wrapped row
  except the still-mutable last one (greedy word-wrap closes earlier rows). A
  markdown-risky tail falls back to paragraph-level commit. Prose therefore flows into
  scrollback token by token and the footer holds only the partial last line plus the
  live status.

Two rules keep the flushed bytes byte-identical to what the seal would emit, so nothing
is ever duplicated:

- Live-ness rides ONLY the position of the last step — an earlier prose step renders as
  final markdown, so the flush never freezes a half-rendered paragraph.
- The **reflow guard**: before splicing `final[FlushedRows:target]`, the already-flushed
  prefix must still render identically this frame (`t.flushedRowsText`). A rare markdown
  re-wrap makes the flush HOLD and lets the seal commit the tail instead — misaligned
  content beneath a prefix already in scrollback is unrecoverable.
- `sealTail` strips the flushed prefix at a row boundary on seal (exact text match, with
  a row-count fallback).

`LeftPad` is applied at commit and at footer assembly, never inside the row builders, so
a flushed row lines up column-for-column with the live tail.

## 4. Chunked commits — never print taller than the viewport

`scrollbackChunkRows` + `splitRowChunks` (`flush.go`). Bubble Tea v2's `insertAbove`
scrolls the screen by the printed line count, then moves the cursor UP by that count
**plus the live-footer height**. When the printed block is taller than the rows above the
footer, that `CursorUp` clamps at the top of the viewport and the geometry desyncs —
freezing a copy of the footer into scrollback with a band of blank rows
(charmbracelet/bubbletea#1613). A large paste making the YOU card hundreds of rows is the
realistic trigger.

So **every** commit is split into viewport-sized slices. The bound is measured at PRINT
time from `m.footerRows` — the footer's actual last-RENDERED height, written by `View()`
through a shared pointer because `Model` has value-copy semantics. It lags state by
exactly one frame, which is precisely the cell-buffer height `insertAbove` will use for a
commit fired in the next `Update`. Reserve it (plus one row of margin) and every chunk
stays within one screen.

> Measuring at *selection* time instead of *print* time, or reserving a static cap, both
> reintroduce #1613 — that regression has been fixed twice. Floor the result at 1 so a
> tiny terminal still makes progress.

## 5. The footer re-pin ledger

`internal/ui/repin.go`. Ultraviolet repaints a SHRUNKEN inline view **top-anchored**: the
shorter footer is redrawn at the old frame's origin and the freed rows below are erased,
but they remain in the host's viewport as dead, scrollable blank area *below* the
composer. Growth and `tea.Println` recover it — a shrink matched by a print self-heals.

The one that does not is **the last shrink of a turn**: run-status chrome leaves the
footer after the final block has already printed (often an empty block — a fully
line-flushed turn has nothing left to commit), stranding the composer 1–3 rows above the
bottom with dead scroll area underneath after *every* response.

The fix is a small double-entry ledger reconciled on every state pass:

| Event | Ledger effect |
| --- | --- |
| Rendered footer SHRINK | accrues **debt** (dead rows below the footer) |
| An `insertAbove` we emit (queue commit, turn flush) | accrues **credit** first — its rows slide the footer over the matching shrink physically, so debt accrues only for the uncovered remainder |
| Rendered GROWTH | repays debt directly and ends the credit story |
| Small leftover debt | healed after a render-settle barrier by printing that many blank rows above the footer |
| Debt > `repinDebtCap` (4) | **forgiven** — dumping a visible blank band into scrollback is worse than the strand; the next real commit heals it organically |

At most ONE re-pin barrier is armed at a time, nonce-guarded so a stale tick can't act.
If the footer height changed while the barrier waited, it re-arms rather than printing —
the new frame needs its own settle delay before an `insertAbove` can trust the renderer's
cell-buffer height. `resetRepinLedger` is mandatory whenever host scrollback is wiped
(`/clear`, the nuclear resize redraw): stale debt must not survive into a fresh layout.

Every path that emits an `insertAbove` **must** credit the ledger (`creditRepinRows`).
A path that forgets will heal rows a second time with blank lines mid-turn.

## 6. Forcing a repaint — `tea.ClearScreen` alone is inert

Bubble Tea v2 optimizes away a pending `tea.ClearScreen` when `View.Content` and its
bounds are unchanged. Since `hostClearCmd` has *already* erased the physical terminal by
raw escape, that fast path leaves a blank screen. A host redraw therefore needs a content
change to defeat the equality check.

`redrawHostCmd()` (`internal/ui/cmds.go`) is the ONE safe sequence — used by `/clear`,
settled resize recovery, `Ctrl+L`, and the legacy boot hand-off. **Order is load-bearing:**

1. `hostClearCmd()` — erase the physical viewport + native scrollback;
2. `tea.ClearScreen` — mark Bubble Tea's managed screen for full redraw;
3. `rendererRepaintMsg` — toggles `rendererRepaintTag`, so `View()` appends a zero-cell
   SGR reset (`\x1b[0m`) and otherwise-identical frames differ;
4. `commitArmCmd()` — wait several renderer ticks before allowing `tea.Println` again.

Step 3 must land **after** step 2. Toggling the tag in the reducer that requests the wipe
renders too early, only for `hostClearCmd` to erase it afterward.

Commits stay disarmed for the whole window (`redrawPending`). `CommitArmMsg` must NOT
re-arm inside it: the arm tick from `Init` is timer-ordered, not state-ordered, and
re-arming mid-window lets a commit `Println` against pre-redraw geometry.

## 7. The event pump and token coalescer

`internal/ui/pump.go` bridges the runtime to Bubble Tea. The agent runs on its own
goroutine and emits through an `agent.EventSink`; the pump implements that sink by
pushing each event onto a buffered channel.

Bubble Tea has no "subscribe to a channel" primitive, so the cockpit uses the **re-armed
`waitEvent` command**: a `tea.Cmd` blocks on the channel, returns the next event as a
`tea.Msg`, and `Update` re-arms an identical command after handling it — a self-sustaining
pump that never blocks the render loop.

A **token coalescer** sits in the pump: streamed assistant tokens are buffered and flushed
on a short tick (16–33 ms) or immediately before any non-token event the pump FORWARDS, so
the footer repaints at a sane rate without a typewriter delay. (A sink method the cockpit
deliberately drops — `SkillLoaded` — never reaches `emit` at all, so it neither renders nor
disturbs the token boundary.) **No artificial typing effect** —
flush coalesced tokens straight through.

## 8. Liveness: explicit RunPhase, ordered TurnSteps

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

## 9. The dedicated composer

`internal/ui/composer/` is a purpose-built editor model (not Bubbles' `textarea`). It owns
its buffer (a flat rune string with embedded `\n`), cursor offset, history, and kill-ring,
and the parent reaches in only through one explicit `Restore(text)` pull-back path. It
implements the full keymap: logical-line arrow nav with column memory, history recall at
line edges, `Ctrl-Y` yank, the trailing-backslash + Enter newline fallback,
modifier+Enter newline, verbatim bracketed paste, slash-command Tab completion, and "Esc
clears / Esc-empty-while-busy cancels". App chords (`Ctrl-C`, `Ctrl-O`, `Ctrl-X`, off-home
`Esc`) are handled by the shell in `Update`, routed by current view + focus (there is no
global key bus).

## 10. The no-alt-screen / no-mouse contract (enforced)

These are hard rules; `internal/ui/view_test.go` enforces them:

- `TestViewOptions_NoAltScreenNoMouse` asserts the program's `tea.View` reports
  `AltScreen == false` and `MouseMode == tea.MouseModeNone`.
- `assertNoForbiddenEscapes` fails if `View()` ever contains `\x1b[?1049h` (alt-screen
  enter) or a mouse-tracking enable.
- A width test asserts a live footer line never lands in the autowrap column (it stays
  within `columns - gutter`).

Never enable `WithMouseAllMotion` / `WithMouseCellMotion`, never raw-parse SGR mouse mode,
never implement an internal scrollback viewport. The host owns scroll.

## 11. `/clear` — the only scrollback wipe

`internal/terminal/clear.go` holds the **only** sanctioned host-scrollback wipe:
`ClearHost` writes `\x1b[2J\x1b[3J\x1b[H` (erase viewport, erase scrollback, cursor home),
TTY-gated and error-swallowing. It never touches the alternate buffer. Everything else
goes through Bubble Tea's managed render path — there is **no** raw per-frame painting.

Callers reach it only through `redrawHostCmd()` (§6), never directly — the raw wipe on its
own leaves Bubble Tea's managed screen believing the frame is still on the terminal. The
paths that use it are `/clear`, `Ctrl+L`, and settled resize recovery. Each must also
bump `redrawNonce` (resetting the commit queue's `resetKey`, so `committed`/`headerDone`
re-arm), call `resetFlushState()` (or a re-committed turn will skip rows the wipe erased),
and call `resetRepinLedger()`.

## Package map (`internal/ui`)

| File / dir            | Responsibility                                                        |
| --------------------- | --------------------------------------------------------------------- |
| `run.go`              | cockpit entry (`Run(ctx, *app.App)`), pre-program splash hand-off, boot prefetch, builds the `tea.Program` |
| `model.go`            | the root model, dependency wiring, the re-pin ledger fields           |
| `update.go` / `update_handlers.go` | the `Update` reducer + message handlers (single-flight Send) |
| `cmds.go`             | `redrawHostCmd` (the one safe full-host redraw), `hostClearCmd`, `bellCmd` |
| `messages.go`         | the message vocabulary, incl. `rendererRepaintMsg` / `CommitArmMsg`    |
| `view.go`             | the `tea.View` (live footer only) + program options; writes `footerRows` |
| `pump.go`             | event pump (`EventSink` → channel → re-armed `waitEvent`) + coalescer  |
| `scrollback.go`       | the commit-queue protocol (`tea.Println`, one-in-flight, ack)         |
| `flush.go`            | incremental active-turn flush + the viewport-sized chunk bound         |
| `repin.go`            | the footer re-pin debt/credit ledger                                   |
| `transcript_types.go` | `TranscriptCell` / `TurnCell` / `TurnStep` / `isSealed`               |
| `runstatus.go`        | `LiveRunStatus`, driven by `RunPhase`                                  |
| `render_*.go`         | turn / activity / chrome / approval / operations rendering            |
| `splash.go` / `boot_splash.go` / `splash_vector.go` / `splash_frames.go` | the pre-program raw-ANSI boot splash + hand-off frame |
| `widths.go`           | width budget / gutter math                                            |
| `controller.go`       | confirm-hook + runtime callbacks into the program                     |
| `dashboard*.go`       | the operations deck snapshot + build                                   |
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
  width (or rely on the host's native reflow). There is no replay record.
- **Never render markdown async from `View`.** `View()` is pure and synchronous; do
  markdown/glamour work in `Update` (or once when a cell seals), never as a side effect of
  rendering.
- **Single-flight `Send`.** Only one turn runs at a time, and a follow-up typed while busy
  is never a second concurrent `Send`. It folds into the RUNNING turn: the Session buffers
  it (`InjectPrompt`, still retractable with Esc) and the cockpit mirrors the buffered text
  in `pendingInjects`, rendering it as a **queued card directly above the composer** so the
  user can see what is waiting rather than trusting a count. When the turn reaches its next
  round boundary the Session folds it in and emits `Interjection`; the card leaves the
  footer and the SAME card reappears in the transcript (`StepInterject`, blank line above
  and below) at the point the model actually read it.
- **Never mutate the model outside `Update`.** All state changes flow through messages.
  The one sanctioned cross-goroutine path is `Program.Send` (the confirm-hook callback).
- **Never `tea.Println` a block taller than the rows above the footer.** Always go through
  `chunkPrintlns` / `scrollbackChunkRows`, and measure the bound at PRINT time from the
  last-rendered `footerRows` — not at selection time, not from a static cap (§4).
- **Never emit an `insertAbove` without crediting the re-pin ledger** (`creditRepinRows`),
  or the same rows get healed twice with blank lines mid-turn (§5).
- **Never expect a bare `tea.ClearScreen` to repaint.** v2 skips it on an unchanged frame;
  use `redrawHostCmd()` so the zero-cell content tag defeats the equality fast path (§6).
- **Never flush a row that can still reflow.** Honour the reflow guard — a prefix already
  in scrollback can never be corrected (§3).
