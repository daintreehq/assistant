# Render Audit — Go/Bubble Tea cockpit vs. original TS/OpenTUI cockpit

Read-only audit. Compares the live Go cockpit (`internal/ui/**`) against the
original TypeScript/OpenTUI cockpit (`../assistant/src/ui/**`, branch
`feat/cockpit-revamp` — buggy-but-intent-obvious) and against a real screenshot
of the intended look. Goal: reach visual parity with the screenshot, then improve
— without violating the inline contract (normal screen buffer, no full-width rules
that wrap on reflow, measure by cells).

**Headline:** the Go port is already a faithful, in many places *better* render of
the TS intent. There is no structural gap. The deltas are small, concrete polish
items (a handful of wrong/inconsistent glyph + color choices, two label mismatches,
and a couple of alignment niceties). All three of the hard behaviors the OpenTUI
build could never land — composer persistence, in-transcript queued turns, and the
live `◆ DAINTREE` status line — are correctly wired and render right in Go.

Two greens run through both products and matter for every comparison below:
- **accent** `#6EE7B7` — Daintree's voice: `◆ DAINTREE`, headings, the done `✓`,
  focus. The DAINTREE marker is accent **+ bold**.
- **brand** `#36CE94` — masthead/splash identity base only (splash gradient base).

Two faint styles also matter and are easy to conflate:
- `Dim()` = faint attribute, **no hue** (theme-neutral, survives `none` mode).
- `Muted()` = gray `#9CA3AF` **+ faint** (separators, ids, durations, connectors).

---

## 1. Parity table

Legend for the **fix** column: file paths are relative to
`internal/ui/` unless noted; `(TS)` marks the original for reference only.

| Element | Original-TS look | Current Go render | Gap / diff | Fix (file + change) |
|---|---|---|---|---|
| **Masthead — wordmark** | `Daintree Assistant` bold (terminal fg, no color) + ` v0.1.0` dim. | `Body().Bold(true)` "Daintree Assistant" + `Dim()` " v{ver}". | **Match.** Both bold-neutral + dim version. | none |
| **Masthead — project subtitle** | dim line, project name. | `Dim()` project line. | **Match.** | none |
| **Masthead — runTitle subtitle** | dim line under project (TS has a 3rd `runTitle` line). | Go renders project only; no separate `runTitle` row. | Minor: Go folds run-title away. Screenshot shows only project, so OK. | optional: add a `Dim()` runTitle line in `render_chrome.go renderMasthead` if a distinct run title is ever set. |
| **Masthead — tier line** | `tier ` dim + `system` (dim; red only when destructive pending) + ` · full access (git, system)` dim. | identical: `Dim("tier ")` + tier (`Dim`/`Danger` on `Destructive`) + `Dim(" · "+gloss)`. Gloss strings match exactly. | **Match.** | none |
| **Masthead — closing rule** | `Divider` fixed width (NOT 100%), muted. | `Muted().Render(strings.Repeat(Rule,width))` fixed-width `─`. | **Match** and contract-correct (fixed width, no fill). | none |
| **Logging badge** | `◌ logging` warning/amber + ` · /path` dim. (glyph `◌`, color `#F6C85F`). | `Warning().Render("◌ logging")` + `Dim(" · "+LogFile)`. | **Match.** | none |
| **MCP / note cell** | `│ ` continuation (toned) + sym + ` ` + text (terminal fg). info note sym `·`, tone active(green). Screenshot: `· Connected to Daintree MCP.` | `renderNoteCell`: `Muted(Continuation)` + toned glyph + ` ` + `Body(Text)`. info → glyph `·`, tone `accent` (green). | Minor tone diff: TS colors the **whole `│ ·` prefix** with the tone (active=green); Go colors the bar `Muted` (gray) and only the glyph toned. Screenshot shows a green-ish bar. | `render_turn.go renderInlineNote` / `render_chrome.go renderNoteCell`: tone the `Continuation` bar with the note tone (use `styleFor(th,tone,...)` for the bar) instead of `Muted()`, for info/warn/error parity. |
| **YOU — label** | `YOU` dim **+ bold**, terminal fg (NO color). | `Accent().Render("YOU")` → **green** + bold. | **DIFF:** TS YOU label is dim-bold neutral; Go makes it accent green. | `render_turn.go renderUserMessage`: change `label` to `th.Dim().Bold(true).Render("YOU")` (neutral), not `Accent()`. |
| **YOU — left bar** | bar `▏` (U+258F), gray (`#6B7280` dark) — `UserMessageSurface.Bar`. TS also has a subtle bg fill `#181D26`. | `Accent().Render(g.Bar)` → **green** bar. `UserMessageSurface` (gray bar + fill) is defined in `theme/usermsg.go` but **unused** by the renderer. | **DIFF:** Go bar is green; TS bar is gray, with an optional dark fill. | `render_turn.go renderUserMessage`: use the `theme.UserMessageSurface` bar color (gray) for `bar`, e.g. `th.userMsgBar()`/`Muted()`; wire the surface struct that already exists. (Fill is optional — see Improvements.) |
| **DAINTREE marker** | `◆ DAINTREE` accent green + bold; inline ` · received` dim while phase=received. | `Accent().Render("◆ DAINTREE")` + `Dim(" · received")` while active+received. | **Match** (glyph, color, bold, received ack). | none |
| **Live run status (under marker)** | spinner (braille) accent + dim label + ` · 0.4s` dim after 300ms. Labels via `liveStatusLabel`. | `Accent(spinner)` + `Muted(label + elapsedToken)`. | Near-match. TS label is `Dim` (no hue); Go uses `Muted` (gray+faint) — both read as faint gray; cosmetic. | optional: switch label to `Dim()` to exactly match TS neutrality. Low priority. |
| **Markdown — headings** | accent `#6EE7B7` + bold. | glamour `Heading` = Accent + Bold. | **Match.** | none |
| **Markdown — inline code** | info cyan `#67E8F9`. | glamour `Code` = Info cyan. | **Match** (screenshot's cyan `CLAUDE.md`, `npm run build`). | none |
| **Markdown — links** | info cyan + underline. | glamour `Link` = Info + Underline; `LinkText` = Info. | **Match.** | none |
| **Markdown — bullets** | marked-terminal default `-`/`·`; list items terminal fg. | glamour `Item`/`Enumeration` = Muted gray bullets; `List` text fg. | Minor: Go bullets are **muted gray**; screenshot shows plain `-` bullets at body fg. Acceptable but slightly grayer. | optional: set glamour `Item.Color` to `Text` (body fg) in `theme/glamour.go` if gray bullets look too quiet. |
| **Markdown — paragraph spacing** | blank line between blocks; margins 0 (cockpit owns insets). | glamour margins/indents pinned 0; blank lines preserved. | **Match.** | none |

### 1. Parity table (continued) — activity tree, status, composer, splash

| Element | Original-TS look | Current Go render | Gap / diff | Fix (file + change) |
|---|---|---|---|---|
| **Activity tree — connectors** | `├─ ` non-last, `└─ ` last (square corner, NOT arc `╰─`), muted gray. Continuation `│ `. | `Muted(BranchMid="├─")` / `Muted(BranchLast="└─")`. | **Match.** (The screenshot's `╰─` is an approximation; both products deliberately use square `└─`.) | none |
| **Activity tree — glyphs** | done `✓`, failed `×` (U+00D7, NOT `✗`), active `◌`/braille spinner, waiting `◷`, queued `◦`. | done `✓` accent; failed `×` danger; active braille `Spinner[]` (fallback `◌`) accent; queued `◦` muted; **waiting → `◇` (Approval glyph), tone blocked (violet)**. | **DIFF (waiting):** TS waiting = `◷` clock glyph, tone warning (yellow). Go uses `◇` + blocked violet. Two different concepts collapsed. | `render_activity.go activityGlyph`: map `ActWaiting` → `g.Waiting` (`◷`) with tone `warning`; reserve `◇`/blocked for an explicit approval-pending state. |
| **Activity tree — glyph colors** | done→accent(green), failed→danger(red), active→info **cyan**, waiting→warning(yellow), queued→muted. | done→accent(green), failed→danger(red), active→**accent(green)**, waiting→blocked, queued→muted. | **DIFF (active color):** TS active glyph is **cyan** (`info`); Go active glyph is **green** (`accent`). Cyan reads as "working", green as "done" — Go loses the live/done distinction. | `render_activity.go styleFor`/`activityGlyph`: tone `ActActive` as `info` (cyan), not `accent`. Keeps the live spinner visually distinct from a completed `✓`. |
| **Activity tree — verb column** | label padded to align (`LABEL_WIDTH=11`), terminal fg; e.g. `Read`, `Listed`, `Delegated`. | `Body().Render(padRight(verb,11))`, fg=Body. Verb map covers all tools. | **Match.** Screenshot verbs (`Listed`/`Read`/`Snapshotted`/`artifact.read`) — note `artifact.read` is an unmapped leaf. | optional: add `artifact.read`→`Read`/`Artifact` and `snapshot`→`Snapshotted` to `presentTool` so raw dotted names never leak. |
| **Activity tree — detail column** | dim, truncated with `…`; on failed: `detail · summary`. | `Dim(detail)`, truncated `…`; on failed `detail — Outcome` (em-dash). | Minor: failed join is ` — ` (em-dash) in Go vs ` · ` (bullet) in TS. Screenshot shows truncated details like `Invalid arguments for artifa…`. Cosmetic. | optional: align the failed separator (` · ` vs ` — `) — either is fine; pick one for consistency. |
| **Activity tree — elapsed column** | right-aligned, dim, `8ms`/`10ms`/`0ms`; via `space-between`. | right token `Muted(formatDuration)`, appended after detail (space-separated), not right-aligned to a fixed gutter. | **DIFF:** TS right-aligns the duration to the row's right edge (`justifyContent:"space-between"`). Go appends it inline after the (truncated) detail, so durations do **not** form a clean right column. Screenshot clearly shows a right-aligned `ms` column. | `render_activity.go renderActivityRow`: right-align the duration. Reserve `durationCols(8)` on the right, truncate `detail` to `width - prefixCols - labelWidth - durationCols`, then pad so the duration sits flush-right. (Constants `durationCols=8`,`prefixCols=5` already exist — wire them.) |
| **Status line** | `CTX 16% · $0.048` dim; CTX tinted ≥90 red / ≥75 yellow; ` · ` dim separators; cost idle-only `$0.0000`/`$0.000`/`$0.00`; capped width. | identical: `CTX N%` tinted, `Dim(" · ")` joins, `formatCost` same tiers, cap `LiveChromeMaxWidth=56`. | **Match.** | none |
| **Composer — prompt glyph** | `› ` (U+203A, when active) / `> ` (ascii). | `promptGlyph = "› "` (always). | **Match** (Unicode); Go always uses `›` (TS swaps `›`/`>` by state — negligible). | none |
| **Composer — placeholder** | `Ask Daintree to supervise, delegate, or inspect…` | `Ask Daintree to plan or supervise…` | **DIFF:** placeholder text differs. Both valid; pick the canonical one. | `view.go:148` `composerView`: set `Placeholder` to `"Ask Daintree to supervise, delegate, or inspect…"` to match TS (or keep Go's — decide canonically). |
| **Composer — caret** | block cursor (OpenTUI native). | reverse-video cell (`Body().Reverse(true)`) — real cursor isn't captured on normal buffer. | **Match in spirit** (correct for normal-buffer model). | none |
| **Composer — slash palette** | cmd `padEnd(14)` info cyan + dim desc, max 5. | `padCells(Name,14)` + desc, all `Dim()`. | **DIFF:** TS colors the command name **info cyan**; Go renders the whole row `Dim()` (name not cyan). | `composer/view.go View()` slash palette: render `c.Name` with `th.Info()` (cyan) padded to 14, desc `Dim()`. Matches the masthead/code cyan motif. |
| **Composer — busy cue** | dim `{stage}` + `{N} queued`. | `Muted(◌ + stage + " · N queued")`. | Near-match; Go prefixes a `◌` dot (nice). Cosmetic. | none |
| **Hint row** | keys info cyan + dim action, ` · ` separators: `/ commands · ↑ history · ^O inspect ops`. | keys `Accent()` (green) + plain action, `  ·  ` separators, `Dim()` row. | **DIFF (key color):** TS hint keys are **info cyan**; Go uses **accent green**. Also separator is `  ·  ` (wider) vs ` · `. | `composer/view.go renderHints`: render `h.Key` with `th.Info()` (cyan) to match TS; tighten separator to ` · ` if matching exactly. |
| **Splash** | ASCII Daintree mark, brand-green row gradient (crown `#8FEBC4` → base `#36CE94`), centered, holds ~1.1s, hidden when `columns<=width`. | `splashArt` canopy mark, `SplashRowColor` crown `#8FEBC4`→base `#36CE94`, centered on `columns-1`, fps 28 / linger 420ms, hidden when `columns<=48`. | **Match** (same gradient endpoints, same timing model). Art differs in detail; both are a green Daintree mark. | none |

---

## 2. The three behaviors

These are the behaviors the OpenTUI build documented as intent but could never land
cleanly (composer deadlocked the footer; queued turns were a dead `queued?` flag
never read by any renderer; the live status was repeatedly moved on/off the composer
— see TS commits `bd322bb`/`6a302ca`/`a16f530`). The Go port lands all three.

### A. Composer persists + stays editable after submit — **WIRED-AND-RENDERS-RIGHT**

- **Render:** `view.go footer()` unconditionally appends `m.composerView(w)` on the
  home view. There is **no `if !m.inFlight` guard** — the only early returns are
  quit / operations / help.
- **Focus is explicitly not busy-gated:** `model.go composerFocus()` returns
  `m.view == viewHome && m.pending == nil` (comment: "Crucially NOT gated on busy").
  `update.go syncComposer()` pushes `SetFocus(composerFocus())` + `SetBusy(m.inFlight)`
  each reduction; busy drives only the cue + slash-palette suppression, never editing.
- **TS parity:** matches the TS intent exactly (`sendUserMessage` runs the turn in a
  detached `void (async…)()` and returns synchronously; `composerFocus = view==="home"
  && !pendingConfirm`, never `busy`). The Go model is cleaner — no footer-clip deadlock.
- **Fix:** none.

### B. Queue messages while busy → dimmed "queued N" turn promoted in place — **WIRED-AND-RENDERS-RIGHT**

This is the behavior the TS build *aspired* to but never rendered: in TS the queue is
a raw `string[]` ref + a `queueDepth` composer count, and the `TurnCell.queued` flag is
**set but read by no renderer** — there is no dimmed in-transcript turn; the message
only appears (via `user:add`) at the moment it starts. **Go actually implements it.**

- **Submit + busy check + visible cell:** `update_handlers.go onSubmit()` — when
  `m.inFlight`, it appends a real `TranscriptCell{Turn: &TurnCell{UserText, State:
  TurnActive, Queued: true}}` *and* pushes `queuedTurn{prompt, cellID}` onto
  `m.queuedInput`. The `cellID` ties the FIFO entry to its visible cell.
- **Dimmed render:** `view.go liveCellsView()` wraps any `cell.Turn.Queued` cell in
  `m.theme.Dim()`. The composer's `· N queued` badge comes from `QueueDepth:
  len(m.queuedInput)` → `composer/view.go renderBusyCue`.
- **Promotion in place (no dup, no drop):** `update_handlers.go promoteQueued()`
  finds the cell by `cellID` and mutates it (`Queued=false`, `Phase=Received`,
  `PhaseStartedAt=now`, sets `activeTurn`) — it does **not** append a new cell.
  `drainPending()` pops `queuedInput[0]` on `onTurnComplete` (cites issue #95).
- **One cosmetic note (not a gap):** the queued cell is `State: TurnActive`, so the
  dimmed block already shows a faint `◆ DAINTREE · received` and a live status line
  before it actually starts. The whole block is dimmed so it reads as pending; this is
  consistent with "a visible dimmed turn." If it looks too busy, gate the marker/status
  on `!t.Queued` in `render_turn.go renderTurn` (see Improvements).
- **Fix:** none required.

### C. ◆ DAINTREE marker + live RunPhase status on submit, yielding to prose — **WIRED-AND-RENDERS-RIGHT**

- **RunPhase:** `domain/runphase.go`. `agent/session.go runTurn()` emits Received →
  Analyzing → (first token) Generating → ToolQueued/ToolRunning → Integrating →
  Complete/Failed/Cancelled. The pump (`pump.go`) flushes pending tokens then forwards
  each phase; `update.go applyPumpEvent` writes `t.Phase` + `t.PhaseStartedAt`.
- **Marker on submit:** `startTurn()` seeds `Phase: Received, State: TurnActive`.
  `render_turn.go renderTurn` draws the marker because `active := State==TurnActive`,
  so `◆ DAINTREE · received` shows the instant you press Enter, before any token.
- **Live status under it:** `render_turn.go renderLiveStatus` → `runstatus.go
  liveStatusLabel`: Analyzing→"Analyzing request", Integrating→"Integrating results",
  AwaitingApproval→"Waiting for approval", Cancelling→"Cancelling"; `elapsedToken`
  adds ` · 0.4s` after 300 ms. Exactly the screenshot's "received → Analyzing request…".
- **Yield to prose:** `liveStatusLabel` returns `""` for `Generating` and
  `ToolRunning`, so the moment the first token flips phase to Generating the status
  line vanishes and the ordered `StepProse` renders (streaming caret `▌`). Under
  `ToolRunning` the activity tree is the live indicator instead.
- **TS parity:** identical phase set and label mapping (`liveStatusLabel` returns null
  for received/generating/tool_running). Go matches the *final* TS design (status under
  the marker, not the composer) — the design TS only reached in its last commit.
- **Fix:** none.

---

## 3. Improvements (tasteful upgrades — keep it the same product)

All of these stay inside the inline contract: normal screen buffer, no full-width
rules that wrap on reflow, measure by cells (`truncateCells` / `ansi.Truncate`),
fixed-width masthead rule only.

1. **Right-aligned duration column in the activity tree.** The single biggest visual
   upgrade. Reserve `durationCols=8` on the right and pad the detail so every `8ms`/
   `412ms` lines up in a clean column (TS achieved this with `space-between`; Go
   currently inlines it). Makes the tree read like a real ledger. (`render_activity.go`).
2. **Cyan = live, green = done.** Tone the active activity glyph + live spinner `info`
   (cyan) and keep `✓` accent (green). Restores the at-a-glance "working vs finished"
   signal the screenshot has and Go currently flattens to all-green.
3. **In-tool progress substep.** Go already carries `ProgressMsg` on active activities
   (`render_activity.go` shows it as the detail for active rows) — lean into it: surface
   short live substeps like `Delegated  launching terminal…` / `Read  3500/14182 chars`.
   This is strictly better than the original, which had no live per-tool progress.
4. **Compaction summary line.** On `assistant:end`, collapse a finished tool batch to a
   one-line dim summary above/below the tree, e.g. `✓ Inspected 6 files · 412ms`
   (count + total elapsed from the activity slice). Cheap, and it makes long turns scan
   fast. Render it in `render_turn.go` from the completed `StepTool` group.
5. **Clearer live-status vocabulary.** Keep the explicit phase labels (already a win
   over the vague "Thinking"), but consider phase-specific tool verbs in
   `liveStatusLabel`/`runStageLabel` for `ToolRunning` (TS does this via `toolStageLabel`:
   "Delegating" / "Watching" / "Inspecting project" / "Scheduling"). Go's `runStageLabel`
   hardcodes `ToolRunning → "Inspecting project…"`; make it tool-aware so the busy cue
   reads true when delegating or scheduling (`runstatus.go`).
6. **YOU card subtle fill (optional).** TS defines a `#181D26` dark fill behind the user
   text. The `theme.UserMessageSurface` struct already exists in Go but is unused — a
   1-shade-darker fill makes the user's words pop without a box. Only do this if it
   survives all four theme modes gracefully (skip in `ansi`/`none`).
7. **Tone the note bar.** Color the `│` continuation bar with the note tone (green for
   the MCP-connected note, red for errors) instead of flat muted gray — matches the
   screenshot's greenish `· Connected to Daintree MCP.` and gives error notes a red spine.
8. **Cyan accents on interactive affordances.** Make slash-palette command names and
   hint-row keys `info` cyan (they're currently dim/green) — reinforces the "cyan =
   thing you can interact with / type" motif the code spans and links already use.

---

## 4. Prioritized punch-list (apply to internal/ui, no re-derivation needed)

Ordered: visible-parity fixes first, then polish. Each is a small, local edit.

**P0 — visible parity gaps**
1. `render_activity.go renderActivityRow`: **right-align the duration column.** Reserve
   `durationCols(8)`; truncate detail to `width-prefixCols-labelWidth-durationCols`; pad
   to push the `Muted(duration)` flush-right. (Improvement #1.)
2. `render_activity.go activityGlyph`/`styleFor`: tone **active** activity as `info`
   (cyan), not `accent` (green). (Improvement #2.)
3. `render_activity.go activityGlyph`: map **waiting** → `g.Waiting` (`◷`) tone
   `warning` (yellow); stop using `◇`/blocked for plain waiting.
4. `render_turn.go renderUserMessage`: **YOU label** → `Dim().Bold(true)` (neutral),
   **bar** → gray (`theme.UserMessageSurface` bar / `Muted()`), not `Accent()` green.
5. `composer/view.go renderHints`: hint **keys** → `th.Info()` (cyan), not `Accent()`;
   tighten separator to ` · `.
6. `composer/view.go` slash palette: command **name** → `th.Info()` (cyan), padded 14;
   desc stays `Dim()`.

**P1 — text/label canonicalization**
7. `view.go:148` `composerView`: pick the canonical **placeholder** string
   (`"Ask Daintree to supervise, delegate, or inspect…"` to match TS, or keep Go's).
8. `render_activity.go presentTool`: map `artifact.read`→`Read` (or `Artifact`),
   `snapshot`→`Snapshotted`, so raw dotted leaf names never leak into the verb column.
9. `render_chrome.go renderNoteCell` / `render_turn.go renderInlineNote`: **tone the
   `│` bar** with the note tone (green/red), not flat `Muted()`. (Improvement #7.)

**P2 — net-new polish (better than the original)**
10. `render_turn.go renderTurn`: add a **compaction summary** line for a completed tool
    group: `✓ Inspected N files · {totalElapsed}`. (Improvement #4.)
11. `runstatus.go runStageLabel`: make `PhaseToolRunning` **tool-aware** (Delegating /
    Watching / Scheduling / Inspecting project) instead of the fixed "Inspecting
    project…". (Improvement #5.)
12. `render_activity.go`: surface live `ProgressMsg` substeps for active rows
    (`Read 3500/14182 chars…`). (Improvement #3.)
13. (Optional) `render_turn.go renderTurn`: gate the marker/live-status on `!t.Queued`
    so a still-queued dimmed cell doesn't show a faint `· received` before it starts.
14. (Optional) `render_turn.go renderUserMessage` + `theme/usermsg.go`: wire the unused
    `UserMessageSurface` dark fill behind YOU text (dark mode only). (Improvement #6.)

**Contract guard (do NOT regress):** no `AltScreen`/mouse (`view_test.go` enforces);
masthead rule stays fixed-width `─` (no fill, no wrap); whole transcript never renders
into `View()` (sealed turns commit via `tea.Println`); all widths via `truncateCells`.
