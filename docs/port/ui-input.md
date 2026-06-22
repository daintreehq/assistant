# Port-spec: UI input, navigation, panels & runtime-integration

Faithful Go (Bubble Tea) port reference for the INPUT / NAVIGATION / PANELS /
RUNTIME-INTEGRATION half of the cockpit. Source: `daintree-assistant` TS,
`src/ui/components/{Composer,MultilineInput,OperationsView,ApprovalSheet,StartupSplash}.tsx`,
`src/ui/{DaintreeApp,runApp}.tsx`, `src/ui/hooks/useDaintreeController.ts`,
`src/ui/presentation/operations.ts`, `src/ui/bridge.ts`, `src/cli/commandData.ts`.

Sibling spec (transcript / status / scrollback / markdown / theme) is covered
separately — this file does NOT re-derive those.

WIP branch — **no backward compat required**. Shapes below are *behavioral
reference*; the Go design favors clean Go-native models over a literal port of
the React state machine. Where the TS comments call out a bug/race the fix
guarded against, that rationale is preserved so the Go port doesn't re-open it.

---

## 0. Orientation: who owns what

- **Composer buffer** is owned by the composer itself in TS (lifting it would
  re-render the whole tree per keystroke). In Bubble Tea everything is one model,
  but keep the same *discipline*: the composer's buffer/cursor/kill-ring/history
  live in a dedicated `composer.Model`, and the parent never reaches in except via
  one explicit `Restore(text)` path (the pull-back, §1.6).
- **App chords** (`Ctrl-C`, `Ctrl-O`, `Ctrl-X`, off-home `Esc`) are handled by the
  shell, NOT the composer. In TS `useKeyboard` is global with no per-widget focus
  gate, so each handler early-returns on `!focus`. In Bubble Tea you route in
  `Update` by current `view` + focus instead (cleaner — there is no global key bus).
- **Single UI mode** at a time: `home | operations | help`. The composer is
  focusable only when `view == home && pendingConfirm == nil`.

---

## 1. Composer key contract (the full keymap)

Source: `MultilineInput.tsx` (the editor body) wrapped by `Composer.tsx` (slash
palette, history record, busy cue, hint row). The editor was originally written
against Ink's `(input, key)` shape and adapted from OpenTUI `KeyEvent` via
`adaptKey`. The Go port keeps **all** of this logic and only swaps the key source.

> **RECOMMENDATION: write a dedicated Go composer model.** Bubbles' `textarea`
> almost certainly cannot satisfy this contract (logical-line arrow nav with
> column memory + history recall at line edges, a kill-ring with `Ctrl-Y` yank,
> the trailing-backslash newline fallback, modifier+Enter newline, bracketed
> multi-line paste inserted verbatim, slash-command Tab completion, and "Esc
> clears / Esc-empty-while-busy cancels" semantics). Build `ui/composer/` with an
> explicit `buffer string`, `cursor int` (a flat rune/byte offset), `history []string`,
> `histIndex *int`, `killRing string`, and reuse only Bubbles' `key`/`help`
> primitives for binding tables and the hint row.

### 1.1 Buffer model

- The buffer is a single flat string with embedded `\n`. The cursor is a flat
  offset (`cursor int`), clamped to `[0, len(buffer)]` on every mutation.
- **Logical lines** = `strings.Split(buffer, "\n")`. "Line" everywhere below means
  a logical line, never a visually-wrapped row.
- Helpers to port verbatim from `MultilineInput.tsx`:
  - `locate(value, offset) -> {row, col}`: row = count of `\n` before offset; col
    = offset - (index after the last `\n`).
  - `offsetOf(lines, row, col)`: sum of `len(line)+1` for rows above, plus
    `min(col, len(lines[row]))`. Column is **clamped to the target line length** —
    this is what gives up/down its "keep the column, snap if shorter" behavior.
  - `lineStartOf(value, offset)` = index after the previous `\n` (or 0).
  - `lineEndOf(value, offset)` = index of the next `\n` (or `len`).
- **NOTE on units.** TS indexes by UTF-16 code unit. Go must pick rune-based
  offsets (iterate runes, not bytes) so multi-byte input, paths, and pasted
  Unicode don't corrupt the cursor. All the offset helpers above must be
  rune-aware in Go.

### 1.2 Enter / newline (handled FIRST, before any chord)

Order matters: Enter is matched before the ctrl/meta editing chords so the
modifier+Enter combos aren't swallowed by a chord branch.

1. **Modifier+Enter inserts a newline** when the terminal reports a modifier on
   the Return key — `Shift+Enter`, `Alt/Option+Enter`, or `Ctrl+Enter`. TS:
   `if (key.shift || key.meta || key.ctrl) insert("\n")`. This depends on the
   terminal surfacing the modifier (e.g. the kitty keyboard protocol). Bubble Tea:
   match on `tea.KeyMsg` with the corresponding modifier bits; not every terminal
   delivers them, hence the fallback below.
2. **Trailing-backslash + Enter newline fallback** (terminal-independent, same as
   Claude Code): if the char immediately left of the cursor is `\`, Enter replaces
   that `\` with `\n` and keeps the cursor in place. TS:
   `commit(value.slice(0,cur-1) + "\n" + value.slice(cur), cur)`. This always works
   regardless of terminal modifier support — it is the portable newline gesture.
3. **Plain Enter submits**: calls `onSubmit(value)` with the *raw, untrimmed*
   buffer; the Composer wrapper trims and rejects empties (§1.7).
4. Keypad-Enter and a bare line-feed also count as Enter (`adaptKey` maps
   `kpenter`/`linefeed` → return). Port: treat those key names as submit too.

### 1.3 Escape

The editor's own `onCancel` (from `MultilineInput`) just calls the prop. The
*meaning* is assigned one level up in `Composer.tsx`:

- **Nonempty draft (any whitespace-trimmed content): Esc clears the buffer.**
  `setValue("")`. The long-standing cancel-edit gesture.
- **Empty draft while busy: Esc delegates to the controller's cancel/pull-back.**
  TS: `if (busy && value.trim() === "") onCancel?.()`. A whitespace-only buffer
  counts as empty so a stray space doesn't swallow the cancel gesture.
  `onCancel` is wired to `controller.pullBackTurn` (§1.6).
- **Empty + idle: Esc is a no-op** (clears an already-empty buffer).

### 1.4 Cursor motion & history recall

Handled before the ctrl/meta passthrough so modified arrows / editing chords
aren't swallowed.

| Key | Action |
|---|---|
| `←` / `→` | move one rune; clamp to `[0,len]` |
| `Ctrl+←` / `Alt+←` | `prevWord` (move one word left) |
| `Ctrl+→` / `Alt+→` | `nextWord` (move one word right) |
| `Alt+B` | `prevWord`; `Alt+F` | `nextWord` (Emacs word motion) |
| `Home` / `Ctrl+A` | `lineStartOf(cur)` — start of the **logical** line |
| `End`  / `Ctrl+E` | `lineEndOf(cur)` — end of the **logical** line |
| `↑` | move up a logical line keeping column; **at the top line → history back** |
| `↓` | move down a logical line keeping column; **at the bottom line → history fwd** |

**Word boundaries** are whitespace-delimited: a word is a maximal run of
non-whitespace. `prevWord` skips trailing whitespace then the word; `nextWord`
skips leading whitespace then the word (port `prevWord`/`nextWord` verbatim;
`isSpace` = the unicode-space test). Predictable for prose, paths, identifiers.

**Up/down across logical lines** (`moveUp`/`moveDown`):
- If not on the first/last line, move to `offsetOf(lines, row±1, col)` — column is
  clamped to the destination line's length, so the caret snaps to EOL on a shorter
  line (the standard editor behavior).
- **History recall at the edges** (only when a `history` list is supplied):
  - `↑` on the **top** line: if `histIndex == nil`, stash the current draft
    (`draft = value`) and jump to the newest history entry (`len-1`); else step
    toward older (`max(0, histIndex-1)`). `recall(history[idx])` replaces the whole
    buffer and parks the cursor at end. With no history, `↑` on top just moves to
    offset 0.
  - `↓` on the **bottom** line: if `histIndex == nil`, move cursor to EOF (no-op
    recall). If at/past the newest entry, clear `histIndex` and `recall(draft)` —
    restoring the in-progress draft. Otherwise step to `history[idx+1]`.
  - This is standard shell recall: walk back through prior prompts, walk forward,
    and the live draft returns when you step past the newest entry.

### 1.5 Deletion, kill-ring, and yank

| Key | Action |
|---|---|
| `Backspace` | delete the rune left of the cursor |
| `Delete` / `Ctrl+D` | delete the rune right of the cursor (forward delete) |
| `Ctrl+W` / `Alt+Backspace` (Option+Backspace) | kill previous word → `killRange(prevWord(cur), cur)` |
| `Alt+D` | kill next word → `killRange(cur, nextWord(cur))` |
| `Ctrl+K` | kill to end of line; **at EOL it eats the `\n`** (joins the next line): `killRange(cur, end==cur && cur<len ? cur+1 : end)` |
| `Ctrl+U` / `Cmd+Backspace` (super+Backspace) | kill the whole logical line (`killLine` = `killRange(lineStartOf, lineEndOf)`) |
| `Ctrl+Y` | yank — insert the last killed text at the cursor |

- **`killRange(from, to)`** normalizes order, no-ops if empty, stores the removed
  slice into `killRing`, removes it, and parks the cursor at the lower bound.
- The **kill-ring is a single slot** (last kill only), not an Emacs ring. `Ctrl+Y`
  inserts `killRing` verbatim; no rotation. Keep it that simple in Go.
- Backspace dispatch reads modifiers in priority order: `super` → killLine,
  `meta`(Alt) → kill prev word, else plain char delete. Match that precedence.

### 1.6 Pull-back vs. cancel (the Esc-empty-while-busy target)

`Composer.onCancel` → `controller.pullBackTurn` (`useDaintreeController.ts`). The
controller decides between **pull-back** and **plain cancel** via
`pullbackCandidate(transcript)`:

- **Pull-back** (the just-sent turn is still *pre-stream*): the most-recent `turn`
  cell is `state=="active" && !streaming && assistantText=="" && activities.length==0`.
  Note the `activities` guard is load-bearing — a `tool:call` resets `streaming` to
  false without setting text, so a turn that already spawned an agent/timer is NOT
  pre-stream and must not be erased. When pulling back:
  1. Drop any queued follow-ups (`queuedInput = []`, `queueDepth = 0`) so the drain
     can't fire a new turn while the user edits the restored text.
  2. Dispatch `user:pullback` (reducer removes that turn cell *synchronously* — by
     index, so a background attention note that landed after it survives).
  3. `abortController.abort()` — the resulting terminal `assistant:cancelled` finds
     no active turn and is a harmless no-op (no phantom).
  4. `composerRef.restore(turn.userText)` — push the original text back into the
     composer buffer for editing (cursor parks at end).
- **Plain cancel** (anything streamed/ran already, or no candidate): `cancelTurn` —
  set the active turn's phase to `cancelling` *synchronously* (so the cockpit
  acknowledges Esc instantly) then `abortController.abort()`. The turn stays in the
  transcript, later marked `cancelled` with a one-line note. No-op when idle.

Go mapping: `pullbackCandidate` is a pure function over the transcript slice — port
it as-is. `Restore` is the ONE sanctioned parent→composer write; model it as a
`tea.Msg` (`RestoreDraftMsg{Text}`) the composer handles in `Update`, or a direct
method call if the composer is an embedded struct the parent owns.

### 1.7 Submit, history record, history cap

In `Composer.submit(text)`:
- `trimmed = strings.TrimSpace(text)`; **empty → return** (no submit, buffer
  untouched).
- `onSubmit(trimmed)` is the controller's `sendUserMessage`. **It returns `false`
  synchronously when it rejects** — but note: in the current wiring it never
  rejects for "busy" (follow-ups queue, §3). A `false` result means keep the text;
  any other result (accepted / queued) clears the buffer.
- On acceptance, record into prompt history: **collapse an immediate duplicate**
  (`if last == trimmed skip`), then append and **cap at `HISTORY_LIMIT = 200`**
  (`slice(-200)` — keep the newest 200). Then clear the buffer.
- History is **session-scoped** (lives in the composer, seeded empty each launch);
  it is NOT persisted. Keep that — do not wire it to the DB conversation log.

### 1.8 Printable input & paste

- A printable key inserts its glyph at the cursor, normalizing `\r\n?` → `\n`.
  TS guards with `isPrintable` (rejects any rune `< 0x20` or `== 0x7f`) so a raw
  escape sequence (arrows, PgUp, F-keys) can never leak into the buffer. The Go
  port gets this for free if it only inserts on `tea.KeyMsg` of type
  `KeyRunes`/printable — but keep an explicit control-char guard as defense.
- Ctrl/Alt chords that fall through (`Ctrl+C`, `Ctrl+O`, `Ctrl+X`, and any other
  meta chord that isn't a defined editing op) are **never inserted as text** —
  they return without inserting, leaving them for the app-level handlers.
- **Bracketed multi-line paste** (`usePaste` in TS, decoding `event.bytes` via
  TextDecoder, or `event.text` when present) is inserted **verbatim** with
  `\r\n?`→`\n` normalization, so a multi-line clipboard lands as multiple buffer
  lines in one operation. Bubble Tea delivers paste as a `tea.KeyMsg` with
  `Paste == true` (enable bracketed paste in the program); insert its `Runes`
  verbatim with the same newline normalization. Do NOT run paste content through
  the per-key chord logic.

### 1.9 Slash-command palette + Tab completion

`Composer.tsx`:
- A palette opens only when the draft starts with `/` AND the composer is focused
  AND not busy. `suggestionsFor(value)` filters `COMMAND_SUGGESTIONS`
  (`paletteEntries()` from the shared command registry — keep this single source of
  truth so the palette can't drift from the handlers) by: command name starts with
  the query, OR description contains the query (case-insensitive). Cap **5** rows.
- **`Tab` completes the top match**: `onTab` sets the buffer to
  `suggestions[0][0] + " "` (the command plus a trailing space) when the palette
  is non-empty; otherwise Tab does nothing. There is no cycle-through-matches.
- The palette renders above the input as dim rows (name padded to 14 + dim desc),
  truncated. It's purely visual; it does not capture arrows (history/cursor still
  win). Port as a derived list rendered above the composer line.

### 1.10 Composer stays editable while a turn runs

The composer's `focus` is `view == home && pendingConfirm == nil` — crucially it is
**NOT gated on `busy`**. You can keep typing (and submit) while a turn streams; the
submission queues as a follow-up (§3). The busy cue and hint row update, but the
input never locks. Preserve this — it is the core of the "typing stays live"
behavior (issue #45). The Go composer keeps focus through a running turn; the
*controller* decides queue-vs-run.

### 1.11 Busy cue + adaptive hint row (Composer chrome)

- While `busy`, a one-line cue under the input shows a `ThinkingDot` + the precise
  **stage** label (`Analyzing request / Generating / Delegating / Integrating
  results / Waiting for approval / Cancelling …`) + `· N queued` when
  `queueDepth > 0`. The stage comes from the controller (`deriveStage`), not a
  generic "Thinking".
- The hint row's *order* adapts but the *set* is stable (promotion, not new
  chrome). `cancelActive = cancellable ?? busy`. `leadWithOps = attentionPending &&
  !cancelActive`. Build hints: if `cancelActive` push `Esc cancel`; if `leadWithOps`
  push `^O inspect ops`; always `/ commands`, `↑ history`; if not `leadWithOps`
  push `^O inspect ops` last. So `^O` appears exactly once, promoted to the front
  only when actionable attention is pending and no cancellable turn is in flight.
  Cancel takes precedence over attention.

---

## 2. Global key bindings (shell-level)

Source: `DaintreeApp.tsx` `useKeyboard`. In Go these live in the **root model's**
`Update`, dispatched by `view` and the splash gate. There is no global key bus, so
route explicitly: when the composer is focused, give it the key first and only fall
through to these for the app chords; off-home the composer is unfocused so the
shell owns all keys.

| Key | When | Action |
|---|---|---|
| `Ctrl+C` | always (incl. during splash) | `exit()` — orderly shutdown (§5) |
| `Ctrl+O` | not booting | toggle operations view: if `view==operations` → home; else clear `activePanel` and open the full ops deck |
| `Ctrl+X` | not booting | toggle `expanded` (raw tool detail in transcript — sibling spec owns rendering) |
| `Esc` | not booting, `view != home` | `returnHome()` (set view=home, clear activePanel) |
| `Esc` | `view == home` | NOT handled here — the focused composer owns it (§1.3) to avoid double-firing |
| any other | during splash | **swallowed** — only `Ctrl+C` is live while booting |

- **During splash, only `Ctrl+C` is active.** TS early-returns from the global
  handler when `controller.booting` (after the `Ctrl+C` check). This stops a stray
  `^O`/`^X` from leaving the cockpit in a non-home state once boot finishes. Port:
  in `booting` state, the root `Update` handles only `Ctrl+C`.
- `returnHome()` = `setView("home"); setActivePanel(null)`. A `/panel` command sets
  `activePanel` which an effect maps to a view (`help` → help view, else operations
  view); `Ctrl+O` clears `activePanel` first so the deck isn't still filtered.

---

## 3. Operations view

Source: `OperationsView.tsx` + `presentation/operations.ts`. The product
differentiator. Five sections in strict human-priority order; **empty sections
render nothing** (vanish entirely) so urgency owns the space.

| Section | Source data | Populates with |
|---|---|---|
| **NOW** | `buildAgentRows(...)` | the single most-active agent: the first row classified `still_working`, else the top row by urgency. Shows its badge + goal/title, elapsed (`now-startedAt`), and id + last preview line. "Standing by" when none. |
| **NEEDS ATTENTION** | `dashboard.inbox` (`queue.digest({severityAtLeast:"attention", maxItems:30})`) | every urgent queue event: severity-toned title, `×count` when deduped >1, and a dim summary line with an **epistemic tag**. |
| **AGENTS** | `buildAgentRows(...)`, capped at 6 (`+N more`) | every supervised agent (one row per watcher, merged with its terminal preview). Badge + title + elapsed; second line = epistemic tag + id + `agentState` + last preview line. |
| **SCHEDULED** | `dashboard.timers` (`db.listTimers("scheduled")`), cap 4 | clock time (`fireAt`) + title. |
| **RECENT** | `dashboard.audit`, cap 5 | done/failed glyph + tool name + `durationMs`. Lowest-priority detail. |

### 3.1 Agent rows: merge watchers + terminal previews

`buildAgentRows(watchers, previews)` — **one AGENT row = one watcher merged with
its watched terminal's preview.** The user thinks "one supervised agent doing one
job", not "a watcher" and "a terminal" separately. Per watcher:
- Parse `targetsJson` → terminal ids; find the first preview whose `terminalId`
  matches → `preview`.
- `id` = `preview.terminalId ?? targets[0] ?? watcher.id` (prefer the terminal).
- `badge = watcherBadge(lastClassification)` (theme — sibling spec owns colors).
- `epistemicKind = lastEpistemicKind ?? classificationEpistemicKind(lastClassification)`
  (fallback for rows persisted before the field existed).
- `preview` = last non-empty line of the preview tail; `agentState =
  preview.agentState ?? preview.runtimeStatus`; `startedAt = watcher.createdAt`;
  `needsAttention = badge.priority <= 1`.

### 3.2 Sort: urgency then recency

`rows.sort((a,b) => a.badge.priority - b.badge.priority || b.startedAt - a.startedAt)`
— **lower badge priority = more urgent (needs-input first), ties broken by
most-recent `startedAt`.** Port verbatim; an urgent approval must never get the
same weight as idle work.

### 3.3 Epistemic provenance labels (issue #85)

Each attention/agent row can carry an `EpistemicTag` — a colored glyph + 3-letter
tag telling the user whether the row's state is an **observed** fact, a model
**inferred** verdict, or **unverified**. Sourced from `epistemicMark(kind)`
(theme; renders nothing when the kind is absent, so legacy rows are unchanged).
The tag carries its own color so it stays legible inside a dim line. Keep the
provenance distinction in the Go view-model (`EpistemicKind` on the row) — it is a
trust signal, not decoration.

### 3.4 Focused panels (`/watchers /inbox /timers /audit`)

A `/panel` command sets `activePanel` (one of `watchers | inbox | timers | audit |
help`) and opens the operations view **filtered to that one section** — there is no
native scroll-to, so "focus" is a *filter*, not a viewport jump. The mapping
(`OperationsView.activePanel`): `watchers→AGENTS`, `inbox→NEEDS ATTENTION`,
`timers→SCHEDULED`, `audit→RECENT`. `help` is rendered by ControlRoom separately
(the help view, sibling-spec adjacent — it's the help overlay). When a focused
section is empty, render an honest `Nothing here yet.` line, not a blank body.
`null` activePanel shows the full five-section deck. Re-running the same command
re-opens it.

---

## 4. Approval sheet

Source: `ApprovalSheet.tsx`. A full-width bordered card sitting directly **above
the composer** (in the eyeline, not a floating modal). It leads with the
*consequence* of approving in plain language, defaults visually to **DECLINE**, and
stays understandable with color stripped.

**Fields** (one truncating line each, so the sheet height never overflows):
- **title** — a risk-specific question via `titleFor(req)`: push→"Push branch to
  origin?", commit→"Commit changes?", terminal→"Send input to terminal?",
  worktree→"Create worktree?", git→"Run a git action?", external→"Run an external
  action?", system→"Run a system-level action?", else "Approve this action?".
- **affects** — the consequence to lead with: `req.consequence` (the tool's own
  phrasing) **or** the `RISK_CONSEQUENCE[req.risk]` fallback. Use `||` not `??` so a
  blank string also falls back. Fallbacks cover all 8 risk classes (terminal,
  project, git, external, system, read, local, ui).
- **tool** — `req.toolName`, dim secondary label.
- **reason** + **args** — shown only when expanded (`V` toggles): `req.summary` and
  `compactArgs(req.args, width-12)` (compact/redacted arg preview). Collapsed by
  default; **reset to collapsed whenever a different request (`pending.id`) takes
  the sheet** so a fresh prompt never inherits the prior raw view. (TS does this by
  comparing `shownFor` to `pending.id` during render — port as: reset `showArgs`
  when the pending id changes.)
- **action row** — `Y approve · N decline · V inspect · Esc`. **Decline is the
  default**: it's rendered inverse (highlighted), approve is plain.

**Keys** (`useKeyboard` while the sheet is up):
- `y` / `Y` → `onResolve(true)` (approve)
- `n` / `N` **or** `Esc` → `onResolve(false)` (decline — **Esc declines**, it does
  NOT just dismiss)
- `v` / `V` → toggle the inspect panel

**Pending-confirm lifecycle** (controller): a `confirm` bridge event sets
`pendingConfirm` and dispatches phase `awaiting_approval` so the cockpit doesn't
look frozen. `resolveConfirm(approved)` calls `pending.resolve(approved)`, clears
the sheet, and dispatches phase `tool_running` (the next tool:result/assistant:start
refines it). **While the sheet is up the composer is unfocused** (`composerFocus =
... && !pendingConfirm`), so `y/n/v/Esc` reach the sheet, not the buffer.

**Shutdown / interrupt rejects all pending.** On teardown the controller calls
`bridge.settlePendingConfirms(false)` and rewires the confirm hook to an
auto-decline (`app.setHooks({ confirm: async () => false })`) so a dispatch can
never block on a modal with no UI subscriber. Port: on shutdown, resolve every
outstanding confirm as `false` and make any *future* confirm request auto-decline.

Go mapping: model `pendingConfirm` as `*PendingConfirm` on the root model; the
confirm request arrives as a `tea.Msg` carrying a resolve channel (the runtime
blocks on it). The sheet is a child component returning a `bool` decision; the root
forwards keys to it only while it's non-nil.

---

## 5. Boot splash

Source: `StartupSplash.tsx` + `splash/frames.ts` (generated by `scripts/gen-splash.py`).
The Daintree mark "drawing itself in" (trunk → roots → canopy arch) while the
session connects in the background, then dissolved into the cockpit.

- **Grid:** `SPLASH_WIDTH = 48`, `SPLASH_HEIGHT = 18`. **20 frames**
  (`SPLASH_FRAMES`), each a `\n`-joined block of 18 rows × 48 cols of block glyphs
  (`█`) on spaces — an anti-aliased ASCII coverage ramp.
- **Timing:** `fps = 28` default → ~0.7s draw over 20 frames; on the last frame
  hold `lingerMs = 420` (~0.42s) before signalling done → ~1.1s total. Advance:
  `setTimeout(next, 1000/fps)`; at `index >= last`, `setTimeout(fireOnce, lingerMs)`.
- **Layout — inline, NOT full-screen:** draws at its **natural height** (not screen
  height) after **2 blank lines of top breathing room** (`marginTop={2}`),
  **horizontally centered** within `columns - 1` (one column shy of the edge so the
  mark's right edge never hits the autowrap column and ghosts a frame). It does NOT
  vertically center (`rows` is accepted only for call-site compat). `flexDirection`
  must be `row` so `justifyContent:center` centers horizontally (OpenTUI box
  defaults to column — N/A in Go, but the *centering axis* matters: center across
  the terminal width).
- **Row gradient:** per-row green interpolation, `TOP = #8febc4` (canopy crown,
  lighter) → `BASE = #36ce94` (brand green, base): `t = row/(rows-1)`, channel =
  `round(top + (base-top)*t)`. Implies depth down the mark.
- **Skip when too narrow:** `tooSmall = columns <= SPLASH_WIDTH (48)`. When too
  small, render nothing and fire the draw-done gate **immediately** (a clipped logo
  looks broken). No longer gates on rows.

**Go:** embed the generated frames via `//go:embed` (or a `//go:generate` step that
ports `gen-splash.py` to produce a Go data file) — **do not reimplement the Python
generator at runtime.** A `splash.go` component steps an index on a `tea.Tick`
(`time.Second/28`), holds 420ms on the last frame, emits a `SplashDoneMsg`, and
tints rows with the green gradient. Keep `WIDTH=48`, `HEIGHT=18`, the `columns<=48`
skip, and the 2-line top margin.

### 5.1 Splash MUST NOT gate interaction

Critical: in the current cockpit the splash is a *visual* gate over a background
load, dismissed by a 3-way readiness gate (`finishBootIfReady` = startup settled &&
animation done && project name settled; 8s `bootCap` backstop). **But the design
intent for the Go port is stronger: the composer must render and accept input
before MCP/project resolve.** Do NOT block the input model on the splash or on the
connect. Render the composer immediately; let the splash be a transient overlay
that dismisses on its own timer (and `Ctrl+C` still quits during it). A slow MCP
connect must never hold the user out of typing.

> NOTE on TS reality vs. Go intent: TS *does* hold the whole cockpit behind the
> splash until the project name resolves (the masthead freezes into scrollback on
> first paint, so a late name can't be patched in). In Go, prefer rendering chrome
> with a provisional project name (directory leaf) and upgrading it live, so the
> splash never has to gate on the name fetch.

---

## 6. Bubble Tea runtime integration

The TS controller is a React effect graph: a `UiBridge` (EventEmitter) republishes
the agent loop's `AgentEventSink` + the confirm hook, and a `subscribe` effect
reduces every event into transcript/dashboard/usage state. Bubble Tea's single
`Update` loop replaces all of that — but the *shape* of the data flow must survive.

### 6.1 The event pump (runtime AgentEvent → tea.Msg)

The runtime (agent loop, scheduler, watchers) emits events on its own goroutines.
Bridge them into Bubble Tea with a **receive-only channel + a re-armed `waitEvent`
command** — the idiomatic pattern, preferred over `Program.Send` from runtime
goroutines:

```go
// once, at startup:
events := make(chan AgentEvent, 256)   // buffered; runtime publishes here
// command that blocks on the channel and yields one msg:
func waitEvent(ch <-chan AgentEvent) tea.Cmd {
    return func() tea.Msg { return AgentEventMsg(<-ch) }
}
// Init returns waitEvent(events); Update, on every AgentEventMsg,
// processes it AND returns waitEvent(events) again to re-arm.
```

- The bridge's `UiBridgeEvent` union (`bridge.ts`) is the message vocabulary:
  `assistant:start | assistant:token | assistant:end | assistant:cancelled |
  tool:call | tool:result | log | confirm | usage | attention`. Port each to a Go
  `tea.Msg` type (§7).
- `Program.Send` is acceptable as a fallback (e.g. from a callback you can't easily
  channel), but **prefer the channel + re-armed command** so backpressure is
  explicit (the buffered channel) and ordering is the channel's FIFO. Never call
  `Program.Send` in a hot loop (per-token) — that's what the coalescer is for.
- The confirm hook is special: it must block the runtime goroutine until the user
  decides. Send a `ConfirmMsg{Request, Resolve chan bool}`; the runtime goroutine
  reads the channel. On shutdown, drain & resolve-false every outstanding one
  (§4, the `settlePendingConfirms` equivalent).

### 6.2 The token coalescer

The model streams `assistant:token` events one rune-chunk at a time. Delivering
each as its own `Update` would melt the render loop. **Coalesce adjacent token
chunks** before they reach `Update`:

- Buffer consecutive `assistant:token` payloads in the pump (between `Update`
  deliveries), concatenating in arrival order.
- **Flush** the buffer as ONE `AssistantTokensMsg{Text}` when either: a flush
  timer of **16–33 ms** elapses (one frame at 30–60fps), OR **any non-token event
  arrives** (`assistant:end`, `assistant:cancelled`, `assistant:start`,
  `tool:call`, `tool:result`, error, etc.) — flush the pending tokens **before**
  emitting that event so order is preserved.
- **Invariants:** never drop, never reorder, preserve byte order exactly; always
  flush before `end` / `cancel` / `error` (a partial trailing chunk must land in
  the transcript before the turn seals). Target **≤30–60 messages/sec** into
  `Update` regardless of token rate.
- This lives in the pump goroutine (or a small coalescer goroutine between the
  runtime and the events channel), NOT in `Update` — `Update` must stay cheap and
  see already-batched text. The flush timer is a `time.Timer` reset on each token;
  a non-token event cancels it and forces an immediate flush.

### 6.3 Three-priority work serialization

The controller runs **one unit of work at a time** behind a synchronous lock
(`inFlight ref`, NOT React state — state is async and can't gate same-tick
submits). Three priorities, drained strictly in order when the lock frees:

1. **Current turn** — the in-flight `Session.Send` (user turn, slash command, or a
   wake). Single-flight: nothing else runs until it completes.
2. **User follow-ups** — typed while busy, queued **FIFO** (`queuedInput`). Drained
   first when the lock frees (a human-queued message beats a background reaction).
3. **Autonomous wakes** — attention events the scheduler surfaces while idle
   (`pendingWake`), fed as a `readOnly` turn so the model can inspect a
   finished/blocked terminal and report. Drained only after the user queue empties.

`drainPending()` (called in every turn's `finally`): if locked, return; else shift
the next `queuedInput` item and re-run it as a turn; if the user queue is empty and
`pendingWake` is non-empty, fire the wake reactor. Update the visible `queueDepth`
from `len(queuedInput)` **before** the drained item re-enters (so it never momentarily
reads "1 queued" while that item is already the active turn — issue #95).

- **Wake specifics** (`reactToWake`): drains `pendingWake` into one `Session.Send`
  with `{readOnly: true}` (a turn the user didn't initiate must only inspect &
  report, never mutate). On success, reset the per-burst retry budget and record
  the summarized terminal ids (so later lifecycle events of the same terminal
  downgrade to a one-line ack, not a repeat summary — guard on a non-failure reply,
  since `send()` returns a sentinel string on model failure rather than throwing).
  On failure, emit a log and re-queue **once** (`wakeRetried` budget) so a transient
  outage isn't stranded but a persistent failure can't spin the loop. A burst
  starting from empty resets the retry budget.

### 6.4 Concurrency rules

- **Never mutate the model outside `Update`.** All state transitions happen in
  `Update` in response to a `tea.Msg`. Runtime goroutines only *send* (via the
  channel/`waitEvent`), never touch model fields.
- **Single-flight `Session.Send`.** Exactly one outstanding `Session.Send` at a
  time, gated by the in-flight lock — model it as a bool/mutex on the model, set on
  dispatch and cleared when the turn-complete msg lands. A `cancel`
  (`context.CancelFunc`, the Go analog of `AbortController`) is created per user
  turn and cleared when it completes, so a queued follow-up never inherits an
  already-cancelled context.

---

## 7. Go mapping proposal

### 7.1 Packages

```
ui/
  composer/            # the dedicated editor model (§1) — buffer, cursor,
    composer.go        #   killRing, history, palette, paste. Reuses Bubbles
    keymap.go          #   key/help only. Exposes Update/View + Restore(text).
    palette.go         #   slash-command suggestions + Tab completion
  components/
    operations.go      # the 5-section ops view (§3) + buildAgentRows + sort
    approval.go        # the approval sheet (§4) — Y/N/V/Esc, decline default
    splash.go          # boot splash (§5) — go:embed frames, tick, gradient
  messages.go          # the tea.Msg vocabulary (below)
  model.go             # root model: view state, focus, work serialization,
                       #   event pump wiring, single-flight Session.Send
```

`presentation/operations.go` holds the pure `BuildAgentRows` + `AgentRow` (port of
`presentation/operations.ts`) — keep it pure & unit-testable, no Bubble Tea import,
mirroring how the TS file is renderer-free.

### 7.2 `messages.go` vocabulary (from `bridge.ts` `UiBridgeEvent` + controller)

```go
type AssistantStartMsg struct{}
type AssistantTokensMsg struct{ Text string }   // COALESCED (§6.2), not per-token
type AssistantEndMsg struct{ Content string }
type AssistantCancelledMsg struct{ Content string }
type ToolCallMsg struct{ ID, Name string; Args any; StartedAt int64 }
type ToolResultMsg struct{ ID, Name string; Result ToolResult; EndedAt int64 }
type LogMsg struct{ Level, Message string }
type ConfirmMsg struct{ Request ConfirmRequest; Resolve chan bool }
type UsageMsg struct{ Usage AgentUsageEvent }
type AttentionMsg struct{ Events []QueueEvent }   // also feeds wake queue (§6.3)
// UI-internal:
type RestoreDraftMsg struct{ Text string }        // pull-back → composer (§1.6)
type SplashDoneMsg struct{}
type RedrawMsg struct{}                            // resize "nuclear redraw" nonce
type TickMsg struct{}                              // 1s dashboard poll / splash tick
```

The controller-internal actions (`user:add`, `user:pullback`, `command:add`,
`phase`, `transcript:clear`) are NOT runtime events — they're transcript-reducer
inputs the sibling spec owns; the root model dispatches them inline in `Update`.

### 7.3 Root model state (input/nav/serialization slice)

```go
type view int // home | operations | help
type Model struct {
    view        view
    expanded    bool          // ^X raw tool detail
    booting     bool
    activePanel *PanelKey     // /watchers,/inbox,/timers,/audit,/help
    composer    composer.Model
    pending     *PendingConfirm
    // serialization (§6.3):
    inFlight    bool
    cancel      context.CancelFunc
    queuedInput []string      // FIFO user follow-ups
    pendingWake []QueueEvent  // autonomous wakes
    queueDepth  int           // == len(queuedInput), for the busy cue
    // ...transcript/dashboard/usage owned by sibling spec
}
```

`composerFocus = view==home && pending==nil`. Route keys: if `composerFocus`, the
composer gets the key first; the app chords (`Ctrl+C/O/X`, off-home `Esc`) are
checked in the root `Update` per §2.

---

## 8. DELETE list (OpenTUI / React machinery with NO Go equivalent)

These exist only because of the OpenTUI/React/native-renderer substrate. Bubble
Tea handles their concerns natively — **do not port them**, delete outright:

- **`useKeyboard` (`@opentui/react`)** — the global key bus + per-handler `!focus`
  early-returns (`MultilineInput`, `DaintreeApp`, `ApprovalSheet`). Replaced by
  routing keys in `Update` by `view`/focus. There is no global key bus in Bubble Tea.
- **`usePaste` + `adaptKey`** — the OpenTUI `KeyEvent`→Ink-`(input,key)` adapter and
  the `PasteEvent.bytes`/TextDecoder decode. Bubble Tea delivers `tea.KeyMsg`
  (with `Paste`) directly; the keymap reads it natively.
- **`useFooterHeight` / `setFrameCallback` / `flexShrink` footer sizing** — the
  split-footer reserved-height-per-frame machinery. Bubble Tea composes the view as
  one string per frame; there is no separate native footer region to size.
- **`useResizeRedraw` + `requestRedraw` + `resyncCockpitSurface` +
  `resetSplitFooterForReplay` + `forceFullRepaintRequested` + `clearHostTerminal` +
  the `clearNonce`/`redrawNonce` nonces** — the entire "nuclear redraw" apparatus
  that works around OpenTUI's split-footer leaving stale footer rows in scrollback
  on resize. Bubble Tea re-renders the full frame on `WindowSizeMsg` with no shadow
  buffer to desync; this whole subsystem **vanishes**.
- **`createCliRenderer` / `createRoot` / `screenMode:"split-footer"` /
  `externalOutputMode:"capture-stdout"` / `createScrollbackSurface` / `commitRows`
  / the `<Static>`-equivalent scrollback commit** — replaced by Bubble Tea's
  program + (optionally) its own scroll handling. (Scrollback strategy is the
  sibling spec's call; the *input* half just notes these `runApp.tsx` calls don't
  port.)
- **`react-reconciler` / `react-reconciler/constants`** — the React custom renderer
  OpenTUI builds on (and the bare import that fails Node's ESM resolver). No analog.
- **All `@opentui/*` imports** (`@opentui/core`, `@opentui/react`,
  `@opentui/react/test-utils`) and the `<box>/<text>/<span>` JSX intrinsics,
  `TextAttributes`, `useRenderer`, `useTerminalDimensions` (→ `tea.WindowSizeMsg`),
  `useImperativeHandle`/`ComposerHandle` (→ a `RestoreDraftMsg` or direct method),
  `useReducer`/`useState`/`useEffect`/`useLayoutEffect`/`useRef` (→ fields + `Update`).
- **The Bun runtime requirement + `bun:sqlite`/`node:sqlite` adaptive driver +
  re-exec-under-Bun** (`resolveBunPath`) — Go is a single static binary; the FFI
  native renderer that forced Bun is gone, so the whole runtime-bootstrap dance goes
  with it.

> Keep, do NOT delete (they survive the port as plain logic): `pullbackCandidate`,
> `buildAgentRows`, the `prevWord`/`nextWord`/`locate`/`offsetOf`/`lineStartOf`/
> `lineEndOf` editor helpers, `suggestionsFor`/`paletteEntries`, `titleFor`/
> `consequenceFor`/`RISK_CONSEQUENCE`, the `HISTORY_LIMIT=200` cap, the splash
> constants (48/18/20/28fps/420ms), and the 3-priority drain logic.
