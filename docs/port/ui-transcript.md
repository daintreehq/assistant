# Cockpit Rendering & Transcript — Port Spec (Go / Bubble Tea)

> **Scope.** This spec covers the **rendering & transcript half** of the cockpit:
> the split-footer / native-scrollback architecture, the scrollback commit queue,
> the transcript data model, the activity tree, the status line, the masthead, and
> markdown rendering. The **composer, keybindings, operations/help views, and
> splash** are covered by a separate spec — they are referenced here only where the
> rendering boundary touches them.
>
> It is written against the real TypeScript source under `src/ui/` of the
> `daintree-assistant` repo (`scrollback.tsx`, `runApp.tsx`, `DaintreeApp.tsx`,
> `ControlRoom.tsx`, `components/Transcript.tsx`, `components/TurnCellView.tsx`,
> `components/ActivityTree.tsx`, `components/Header.tsx`, `components/StatusLine.tsx`,
> `components/LiveRunStatus.tsx`, `markdown.ts`, `theme.ts`, the hooks
> `useScrollbackTranscript.tsx` / `useFooterHeight.ts` / `useResizeRedraw.ts` /
> `useAttentionSignal.ts` / `useDaintreeController.ts`, and `cli/terminalClear.ts`).
>
> **Cross-reference.** `docs/port/_interaction-ux.md` is the authoritative liveness
> spec and defines the `RunPhase` enum and the ordered `[]TurnStep` turn model. The
> transcript model below MUST be compatible with it: where this spec describes the TS
> `TurnCell` (a flat `assistantText` + a separate `activities` slice), the Go build
> uses the **ordered `[]TurnStep`** model from `_interaction-ux.md` §5 instead. The
> mapping between the two is called out inline.

---

## 1. The split-footer / native-scrollback architecture (as Bubble Tea behavior)

The TS cockpit runs on OpenTUI's native renderer in **`split-footer`** screen mode.
The behavior — independent of OpenTUI — is the **Claude Code inline model**:

- A growing transcript lives in the **terminal's own native scrollback**. The host
  terminal owns the wheel, the scrollbar, selection, and copy/paste.
- A small **live footer** pinned to the bottom holds ONLY the in-flight turn, the
  status line, and the composer. That footer is the only region that repaints.
- Finished turns and the masthead are **committed once** to native scrollback — they
  become real terminal lines, scroll up, and never re-render.

### Hard rules (carry forward verbatim)

- **Normal screen buffer only.** `AltScreen = false`. The alternate screen is
  forbidden — it would kill the host's wheel/selection/copy-paste.
- **No mouse capture.** TS sets `useMouse:false`; Go sets nothing that captures the
  mouse (Bubble Tea: do NOT enable `WithMouseAllMotion` / `WithMouseCellMotion`).
- **The host owns scroll.** Never implement an internal scrollback viewport, never
  raw-parse SGR mouse mode, never repaint history.

### Why not "render the whole tree into a viewport"

The TS code tried OpenTUI `main-screen` first: it repaints the entire React tree into
a FIXED viewport and does NOT spill overflow into native scrollback. The instant the
tree grew taller than the terminal, the layout math overflowed and text garbled /
interleaved. `split-footer` is the fix. The Go equivalent of the failure mode is
"render the entire transcript into the Bubble Tea view string every frame" — **do not
do that.** The transcript is committed to scrollback and dropped from the live view.

### Bubble Tea mapping of the core mechanism

| TS / OpenTUI                                  | Bubble Tea                                                       |
| --------------------------------------------- | --------------------------------------------------------------- |
| `createScrollbackSurface().commitRows()`      | `tea.Println` / persistent-print (println-above-program) command |
| `renderer.root` = live footer (`ControlRoom`) | the program's `View()` string (kept short, footer-sized)        |
| `writeToScrollback` plain-text fallback       | `tea.Println(plainText)`                                        |
| host wheel / scrollbar / selection            | unchanged — host terminal owns it (normal buffer, no mouse)     |

The Go `View()` returns ONLY the live footer (in-flight turn + status + composer).
Everything sealed is emitted with a persistent-print command and never appears in
`View()` again.

---

## 2. The scrollback COMMIT QUEUE model

Scrollback is **append-only**: once a row is printed to native scrollback, it can
never be edited or reordered. So commits must happen **strictly in transcript order**,
**one at a time**, with an ack before the next one starts. The TS layer
(`useScrollbackTranscript.tsx` + `scrollback.tsx`) implements exactly this; the Go
build keeps the discipline but drops the OpenTUI surface machinery (see DELETE list).

### Block model

Each thing committed to scrollback is an **immutable block**:

```go
type ScrollbackKind int
const (
    BlockMasthead ScrollbackKind = iota
    BlockTurn      // a sealed TurnCell
    BlockNote      // a standalone NoteCell
    BlockCommand   // a CommandCell (slash-command result)
)

type ScrollbackBlock struct {
    ID       string         // stable cell id (or "__header__" for the masthead)
    Kind     ScrollbackKind
    Rendered string         // the full-fidelity, styled, width-wrapped string
    Plain    string         // plain-text fallback (never lost if styling path fails)
    Width    int            // the terminal cell width this block was rendered AT
}
```

`Width` is load-bearing: a block is rendered at a specific width and then frozen.
After a resize, blocks are **re-rendered fresh at the new width** (see §11 /clear &
resize), they are never reflowed in place.

### The ack protocol (one block in flight)

The TS code mounts exactly ONE `ScrollbackCommit` (the head of its queue) and only
advances when that commit reports `onCommitted`. Two commits can never race and
interleave their rows. Reproduce this as an explicit queue + ack:

1. **Determine the commit frontier.** A cell becomes eligible ("sealed") when it will
   never change again (see §3 `isSealed`). The queue head is: the masthead (if not yet
   committed), then sealed transcript cells in index order that are past the committed
   cursor.
2. **Commit the head.** Emit a persistent-print command for `block.Rendered` (falling
   back to `block.Plain` if rendering produced nothing).
3. **Wait for the committed-msg.** Bubble Tea delivers a message when the print has
   been flushed above the program. Treat that as the ack.
4. **On ack:** remove the block from the queue head, advance the committed cursor (so
   that sealed cell is removed from the live `View()` — it now lives in scrollback),
   and start the next block if any.

### Commit order (exact)

1. **Masthead first** (`BlockMasthead`, id `"__header__"`) — it must land on top of
   scrollback so it scrolls away above all history.
2. **Then sealed transcript cells, in index order**, one at a time from the committed
   cursor forward.

### State the queue owner tracks (mirror of `useScrollbackTranscript`)

- `headerDone bool` — whether the masthead has been committed. Re-armed to `false` on
  `/clear` or resize redraw (see §11).
- `committed int` — how many cells (from the front) are in scrollback. Everything at
  index `>= committed` stays **live** in the footer. Clamp `committed` down if the
  transcript ever shrinks below it (belt-and-braces; `/clear`/resize use the explicit
  reset, not length).
- `resetKey` — a single monotonically-rising integer = `clearNonce + redrawNonce`.
  When it changes, reset `committed = 0` and `headerDone = false`. Length alone cannot
  detect a clear (a `/clear` drops a fresh confirmation card so the new length can equal
  the old committed count); a monotonic key makes the reset deterministic.

> **Liveness/footer split.** Live footer cells = `transcript[committed:]` (the active
> turn plus any sealed cell still draining this frame). A sealed cell leaves the footer
> the frame its commit acks.

---

## 3. Transcript model

The transcript is **run-oriented**: a flat event stream is folded into turns. A turn
is one request → decision → delegated work → outcome. Three cell kinds make up the
transcript (TS `TranscriptCell = TurnCell | NoteCell | CommandCell`).

### `isSealed` — the commit gate

```go
func isSealed(c TranscriptCell) bool {
    // A turn seals when it leaves the active state. Standalone notes and command
    // results are immutable the moment they arrive.
    if t, ok := c.(*TurnCell); ok {
        return t.State != TurnActive
    }
    return true
}
```

### TurnCell

The TS shape (faithful, for reference):

```go
type TurnState int
const ( TurnActive TurnState = iota; TurnComplete; TurnFailed; TurnCancelled )

type TurnCell struct {
    ID             string
    UserText       string      // empty for system-origin turns (e.g. scheduled run)
    AssistantText  string      // TS: flat accumulated prose — SEE NOTE BELOW
    Streaming      bool        // last line is still live (caret)
    Activities     []Activity  // TS: separate slice — SEE NOTE BELOW
    Notes          []SystemNote
    State          TurnState
    Phase          RunPhase    // fine-grained live phase (drives LiveRunStatus)
    PhaseStartedAt int64       // epoch ms; drives the live "· 0.4s" elapsed
    Queued         bool        // a follow-up typed while busy: dimmed, promoted in place
    Ts             int64
}
```

> **GO DEVIATION — use ordered steps, not flat text+activities.** Per
> `_interaction-ux.md` §5, the Go `TurnCell` replaces the flat `AssistantText` +
> separate `Activities` slice with an ordered `Steps []TurnStep` (kinds:
> `StepStatus`, `StepProse`, `StepTool`, `StepNote`). The TS layout renders
> `preamble → tools → conclusion` as `all prose → all tools` because prose is one
> accumulated string above one activity block; the ordered model preserves the true
> chronological narrative (prose, then a tool batch, then more prose). When prose
> resumes after a tool batch, **append a new `StepProse`**, do not merge into the
> earlier one. Keep `State`, `Phase`, `PhaseStartedAt`, `Queued`, `UserText`, `Ts`.
> The rendering rules below (markdown, streaming caret, activity glyphs) apply
> per-step instead of to the flat fields.

### NoteCell

A standalone operational note (MCP connect, attention) outside any turn.

```go
type NoteCell struct {
    ID    string
    Level NoteLevel // info | warn | error
    Text  string
    Ts    int64
}
```

Render: one line, leading `marginTop=1` (own the blank line ABOVE, never below).
Glyph + tone by level: `error → ✗ / danger`, `warn → ! / warning`,
`info → · / active`. Prefixed by the `continuation` glyph (`│ `).

### CommandCell

The result of a slash command rendered into the transcript.

```go
type CommandCell struct {
    ID    string
    Title string
    Text  string
    Ts    int64
}
```

Render: a column, `marginTop=1`, `paddingLeft=1`. Title in info/cyan + BOLD (when
present); body dim (when present).

### Shared cell layout rule

**Each cell owns the single blank line ABOVE it** (a leading `marginTop=1`), never
below. A leading blank is deterministic when the whole tree reflows, so exactly one
blank line precedes every cell and margins never double with a neighbour's.

---

## 4. Activity tree (the brand signature)

A turn's delegated work renders as a **branch tree** — each activity is one branch
carrying a real relationship (request → decision → agent → watcher → outcome). Source:
`components/ActivityTree.tsx` + the verb map in `presentation/tools.ts`.

### Activity model

```go
type ActivityState int
const ( ActQueued ActivityState = iota; ActActive; ActDone; ActFailed; ActWaiting )

type Activity struct {
    ID        string        // the model's tool-call id; results match against this
    Name      string        // internal tool name, e.g. "fs.read"
    Label     string        // human verb, e.g. "Read", "Delegated" (from presentTool)
    Detail    string        // target of the verb (e.g. relative path), optional
    Args      any           // raw args, kept for the expanded (^X) detail view only
    Summary   string        // result summary once resolved, optional
    State     ActivityState
    StartedAt int64
    EndedAt   int64         // 0 = unset
}
```

### State glyphs and tones

| State    | Glyph (unicode / ascii) | Tone     | Color  |
| -------- | ----------------------- | -------- | ------ |
| queued   | `◦` / `o`               | neutral  | gray   |
| active   | `◌` / `*` (ANIMATED)    | active   | cyan   |
| waiting  | `◷` / `~`               | warning  | yellow |
| done     | `✓` / `+`               | success  | green  |
| failed   | `×` / `x`               | danger   | red    |

The **active** glyph is an animated spinner (`ThinkingDot`) with a **live elapsed
clock**, but ONLY on the live turn. A committed/scrollback render passes `live=false`
so no timer animates into frozen scrollback. (Active activities only exist on a live
turn anyway.)

### Row layout

```
├─ ✓ Read       src/ui/ControlRoom.tsx              38ms
├─ ◌ Search     UI event handling                  1.2s
╰─ ◦ Read       src/agent/loop.ts                 queued
```

- Branch glyph: `├─` for all but the last row, `└─` (square, NOT the arc `╰`) for the
  last. Trailing space. (`╰` is missing from many monospace fonts and shifts the row.)
- `PREFIX_COLS = 5` (`"├─ ✓ "`), `LABEL_WIDTH = 11` (verbs padded up to this so
  details align), `DURATION_COLS = 8` reserved on the right for the duration token.
- Detail column budget: `detailRoom = max(8, width - PREFIX_COLS - labelCols -
  DURATION_COLS)` where `labelCols = max(len(label)+1, LABEL_WIDTH)`. Detail is
  **truncated** to that budget so it never collides with the right-aligned duration.
  The row is `justify=space-between` (label/detail on the left, duration on the right).
- Duration is shown when known (`EndedAt` set, or live elapsed for an active row),
  dim, formatted by `formatDuration` (see §10).

### Default detail vs failure rendering

- Default detail = `Detail`, or the result `Summary` once `done`.
- **On failure, surface the failure summary even when a target detail exists** — the
  outcome must never hide behind the original "Reading foo.ts" target:
  `detail = detail ? detail + " · " + summary : summary`.

### Expanded mode (`^X`)

Below each row, indented `paddingLeft=3`, dim, **truncated**:
`<name> args: <compactArgs(args, max(20, width-12))>` and, if a summary exists,
`result: <truncate(summary, width-12)>`. Truncation matters: an expanded row must not
out-run a just-shrunk terminal and orphan a wrapped copy into scrollback.

### Tool → human-verb mapping (`presentTool`, COMPLETE list)

Known first-party tools map to an operator-readable `verb + target`; unknown tools
fall back to the internal name (never raw `fn({...})` syntax). Detail is truncated to
48 chars.

| Tool name                       | Label (verb)            | Detail source                       |
| ------------------------------- | ----------------------- | ----------------------------------- |
| `fs.read`                       | Read                    | relative path                       |
| `fs.list`                       | Listed                  | relative path (or `.`)              |
| `fs.search`                     | Searched                | query                               |
| `tool.search`                   | Searched tools          | query                               |
| `context.snapshot`              | Snapshotted             | "workspace context"                 |
| `context.summarize`             | Summarized              | terminalId                          |
| `agentTask.spawnForEdits`       | Delegated               | title (or goal)                     |
| `watcher.terminal.create`       | Watching                | goal / title / terminalIds          |
| `watcher.list`                  | Listed watchers         | —                                   |
| `watcher.cancel`                | Stopped watcher         | id                                  |
| `timer.schedule`                | Scheduled               | title                               |
| `timer.list`                    | Listed timers           | —                                   |
| `timer.cancel`                  | Cancelled timer         | id                                  |
| `terminal.focus`                | Focused                 | terminalId                          |
| `terminal.read`                 | Read                    | terminalId                          |
| `terminal.extract`              | Extracted               | terminalId                          |
| `terminal.extract.async`        | Extracting              | terminalId                          |
| `terminal.summarize`            | Summarized              | terminalId                          |
| `queue.publish`                 | Raised                  | title                               |
| `queue.digest`                  | Read inbox              | —                                   |
| `queue.resolve`                 | Resolved                | id                                  |
| `recipe.list`                   | Listed recipes          | —                                   |
| `recipe.run`                    | Ran recipe              | recipeId                            |
| `skill.step.advance`            | Advanced step           | `<skillId> · step <n>`              |
| `skill.run.get`                 | Checked skill progress  | skillId                             |
| `worktree.createWithRecipe`     | Created worktree        | recipeId                            |
| `forge.getIssue`                | Read issue              | issueNumber                         |
| `forge.listIssues`              | Listed issues           | —                                   |
| `forge.listPRs`                 | Listed PRs              | —                                   |
| `workflow.startWorkOnIssue`     | Started work            | issueNumber / title                 |
| `workflow.prepBranchForReview`  | Prepping branch         | branch / worktreeId                 |
| `grant.create`                  | Granted automation      | —                                   |
| `grant.list`                    | Listed grants           | —                                   |
| `grant.revoke`                  | Revoked grant           | id                                  |
| `daintree.status`               | Checked status          | — (summary already says "Daintree") |
| `daintree.listTools`            | Listed tools            | —                                   |
| `daintree.call`                 | Called                  | toolName / name                     |
| *(unknown)*                     | *(internal tool name)*  | —                                   |

### Compaction (`✓ Inspected 6 files · 412ms`)

This is the **target end-state** from `_interaction-ux.md` §6 ("after completion a
batch may compact to `✓ Inspected 6 files · 412ms`"). It is **not yet implemented** in
the TS source — the TS tree renders every row. The Go build SHOULD implement it: once
a tool batch completes, optionally collapse a homogeneous batch (e.g. several `fs.read`)
to a single summary row (`✓ Inspected N files · <total ms>`), with `^X`/expanded mode
revealing the individual rows again. Treat the per-row tree above as the expanded form
and the compacted summary as the default for finished homogeneous batches.

---

## 5. Live run status line (under the DAINTREE marker)

`components/LiveRunStatus.tsx` + `runStatus.ts`. The in-flight turn shows a precise
phase label ONLY for the "silent work" gaps the activity tree / streaming prose can't
self-explain. Format: `⠋ Analyzing request · 0.4s` (animated spinner + label +
elapsed). Renders **nothing** when the phase is self-evident.

`liveStatusLabel(turn)` returns a label only when `turn.State == active` AND the phase
is one of:

| Phase               | Label                  |
| ------------------- | ---------------------- |
| `analyzing`         | `Analyzing request`    |
| `integrating`       | `Integrating results`  |
| `awaiting_approval` | `Waiting for approval` |
| `cancelling`        | `Cancelling`           |

All other phases return `null` (no line):

- `generating` — the streaming prose + caret already communicate it.
- `tool_running` — the activity tree already shows each tool live.
- `received` — shown inline on the DAINTREE marker as `· received`, not a line.
- `complete` / `failed` / `cancelled` — nothing live.

Elapsed (`now - PhaseStartedAt`) is appended as `· <formatDuration>` ONLY once it
reaches **300ms** (below that, no elapsed token — avoids a 0ms flicker).

`runStageLabel(turn)` is the composer's busy label (separate spec owns the composer)
and maps the same phases plus a tool-verb → present-progressive map
(`Delegated→Delegating`, `Watching→Watching`, `Read/Listed/Searched/Extracted →
Inspecting project`, `Scheduled→Scheduling`, unmapped → `Running <verb>`, none →
`Processing`). The generic fallback is **`Processing`**, never "Thinking", never
"Generating" during tool use / approval.

### The DAINTREE marker

Shown the instant the turn is active OR once it has said anything
(`turn.State == active || turn.AssistantText != ""`). Layout:

```
◆ DAINTREE · received      ← "· received" (dim) only while phase == received
<prose / streaming prose>
⠋ Integrating results · 0.3s   ← LiveRunStatus, only for silent-work phases
```

- `◆ DAINTREE` in accent green + BOLD. The brand glyph is `◆` (`#` in ascii).
- `· received` is a separate DIM span so it doesn't inherit the bold-accent styling;
  present only while `phase == received`.

---

## 6. Status line (the compact live rollup)

`components/StatusLine.tsx`. Speaks ONLY when it has something to say; renders
**nothing** when idle with nothing to report (no "Standing by" placeholder). It is one
truncated line of `· `-joined segments. Width is capped to **`LIVE_CHROME_MAX_WIDTH =
56`** (`liveChrome.ts`) — keep the live status row narrow so it never wraps to 2 rows.

### Fields, in order

1. **Active-agent badge** (when an agent is working): tone-tinted `<glyph> LABEL`
   (uppercased, e.g. `◌ WORKING`) + dim ` <id>[ · <goal>][ <duration>]`. The active
   agent is the first `still_working` watcher, else the first agent. Goal is truncated
   to the room left after the right-side rollup (`activeGoalRoom`), shown only if room
   `>= 6`.
2. **`CTX <n>%`** — context pressure = `round(contextTokens / contextThreshold * 100)`,
   shown only once a usage event has arrived (`contextThreshold > 0`). Dim by default;
   **yellow at `>= 75%`**, **red at `>= 90%`**.
3. **Cost** (`$0.004` etc., `formatCost`) — idle-only (only when no active agent).
4. **Model id** (`minimax-m3`) — idle-only, shown only when `width >= 62` AND it fits
   the 56-col chrome budget after the required segments.
5. **Attention count** `!<n>` — tinted by the worst inbox severity (`topSeverity`).
   The inbox is already filtered to actionable severities at the controller boundary,
   so its length IS the actionable count.
6. **`agents <n>`** — dim; shown only if it fits the chrome budget alongside the
   active-agent block + required rollup.
7. **`DEGRADED`** — yellow; shown ONLY when the MCP link is down (by exception). There
   is NO steady-state "MCP" badge for a healthy link.

### Ordering & truncation rules

- Segments are built into an array, then joined with dim ` · ` separators — so the
  FIRST visible token never carries a dangling leading separator.
- Width budgeting (`seg(s) = len(s)+3` for the separator) conservatively reserves room
  so the line never wraps; over-reserving only ever drops an OPTIONAL token (model /
  agents) early. The **required** rollup (`CTX`, attention, DEGRADED) is never dropped.
- Cost/model are idle-only context; during an active run the left side carries the
  agent and the right side stays terse.
- The whole thing is one truncated line, capped to 56 cells.
- **Render nothing when there are no segments.**

> Permission tier is NOT in the status line — it lives in the masthead (§7).

---

## 7. Header / masthead

`components/Header.tsx`. Deliberately plain text (Claude-Code model). Committed ONCE
to native scrollback so it scrolls away with the history — it does NOT reflect any live
cue (e.g. `destructivePending` escalation surfaces on the approval sheet in the footer,
not here). Layout, top to bottom:

1. **Identity line:** `Daintree Assistant` (BOLD) + ` v<version>` (dim). Truncated;
   `minWidth=0` so a briefly-narrow terminal can't detonate the wordmark into a
   vertical char stack.
2. **Project name** (dim, truncated) — the bound project's name, when known.
3. *(optional)* **runTitle** (dim, truncated) — the in-flight run's intent. Live shell
   doesn't pass it for the committed masthead.
4. **Tier line:** `tier ` (dim) + `<tier>` (dim; **red only** when
   `destructivePending`) + ` · <gloss>` (dim). Tier gloss (`tierGloss`):
   - `supervisor` → "read & UI only"
   - `operator` → "terminals, projects, external"
   - `system` → "full access (git, system)"
   The tier line stays QUIET (dim, no color) at rest for **every** tier including
   `system` — a steady red `system` capsule is alarm fatigue. Red is reserved for a
   destructive (git/system) action awaiting confirmation.
5. **A fixed-width rule** closing the identity band — see the reflow note below.
6. **Debug-log badge** (when `logging`), BELOW the rule: `◌ logging` (warning/yellow,
   pinned so it's never clipped to "loggin") + ` · <logFile>` (dim, truncated).

### NO full-width (reflow-unsafe) rule — IMPORTANT

The masthead is committed to scrollback as a **fixed snapshot that does NOT reflow**.
A flex `width:'100%'` rule would be wrapped by the host on a narrow resize and break
the historical layout. So the closing rule MUST take a **fixed width** (the masthead's
`columns`) — a fixed-length run of `─` (`-` in ascii) that snapshots cleanly. In Go:
render the rule as `strings.Repeat("─", width)` at commit width; never emit a
"fill-to-terminal-width" rule into scrollback. (This is the same reasoning as the
reflow-safe styling rules in §9.)

The masthead is NOT the startup logo — brand identity at startup is the separate splash
(covered by the other spec). Once the cockpit is up the header is a quiet label.

---

## 8. Markdown rendering

TS source: `markdown.ts` (legacy ANSI-string path) + the native `<markdown>` renderable
configured in `components/TurnCellView.tsx`. Assistant prose is markdown; the cockpit
shows it styled (bold, `code`, headings, lists) rather than printing raw markers.

### Streaming vs finalized

- **Finalized turn** (`state != active`): the whole `assistantText` renders as styled
  markdown in one pass.
- **Active/streaming turn** (`StreamingProse`): split at the **last newline**. The
  stable block (everything before the last `\n`) renders as styled markdown; the
  trailing in-progress line renders as **raw text + a dim caret `▌`**. As each newline
  lands, that line joins the stable block and styles. This shows markdown AS produced,
  and is cheap (the stable block is byte-identical between tokens within a line, so it
  re-parses once per completed line, not per token). When the pending line is empty
  (text just ended on a newline), render nothing — no lone caret on its own row.
- **No typewriter effect, no artificial delay.** Flush coalesced tokens (16–33ms)
  straight through (per `_interaction-ux.md`).

### Color / theme handling

- Color is decided up-front (`colorize = terminalThemeMode() != "none" && !NO_COLOR`).
- `NO_COLOR` env (any value) → strip all color.
- `DAINTREE_THEME` / `DAINTREE_TERMINAL_THEME` → `dark` (default) | `light` | `ansi` |
  `none`. `none` drops color entirely and leans on concealed markers.
- `DAINTREE_ASCII=1` (or a non-UTF locale) → ASCII glyph fallback (see §9 / theme).

### Semantic styling map (mirror onto glamour styles)

| Markdown construct        | Style                                      |
| ------------------------- | ------------------------------------------ |
| body prose / paragraphs   | **terminal default foreground** (never forced white) |
| headings                  | accent green (`#6EE7B7`) + bold            |
| inline `code` / codespan  | info cyan (`#67E8F9`)                       |
| fenced code block         | syntax-highlighted; cyan fallback if highlight fails |
| link / url                | info cyan + underline                      |
| bold / italic / strike    | bold / italic / strikethrough              |
| blockquote                | dim                                        |
| **tables**                | **width-agnostic record list** (see below) |

- **Body prose stays on the terminal's own foreground** — the "never force white"
  rule. Only semantic spans get a hue.
- **Tables → record list.** marked-terminal renders tables as a fixed-width grid that
  shreds in the narrow inline cockpit. The TS code overrides the table renderer to emit
  a width-agnostic list: first column → a bulleted heading (`· <cell>`), remaining
  columns → indented `Header: value` lines (empty cells skipped, inline styling
  preserved). The Go build must do the same (glamour does not table-wrap safely at
  narrow widths either): never emit a fixed-grid table into the inline transcript.
- **Wrapping is width-based, owned by the renderer** — `reflowText:false` in TS hands
  wrapping to the layout engine at the live cockpit width. In Go, pass glamour the
  current content width (`WithWordWrap(width)`), never a hard 80-col wrap.
- **Security:** strip any pre-existing ANSI escapes from the *input* before parsing
  (untrusted model output could inject SGR / OSC-8 links); when color is off, strip
  the *output* too. Trailing blank lines marked appends are trimmed.
- **Plain fallback:** empty finalized prose falls back to a plain text node (a bare
  empty markdown render produces nothing, and we never want an empty hole).

### Go markdown stack

Use **glamour v2** for the markdown → styled-string render (it owns word-wrap at the
given width). Add **chroma** only if glamour's fenced-code highlighting is
insufficient. Build a dark/light/ansi/none style set mirroring the table above; gate on
`NO_COLOR` / `DAINTREE_THEME` / `DAINTREE_ASCII`.

### Caching

Cache rendered markdown keyed by **`(content, width, theme, expanded)`**. The render is
pure given those inputs, and the scrollback commit re-renders blocks on resize (new
width → new cache key → fresh render). The cache avoids re-parsing identical
(content,width) blocks across frames and across the live→sealed transition.

---

## 9. Reflow-safe historical styling rules

Anything that becomes immutable scrollback (the masthead + every sealed cell) is a
**frozen snapshot that the host will reflow on resize**. To keep that reflow from
shearing the layout, historical content obeys these rules (sources: `ControlRoom.tsx`
insets, `Header.tsx` fixed rule, `UserMessageCard.tsx`, `liveChrome.ts`):

1. **Cap prose width at `CONTENT_MAX = 100` cells.** Long lines that run a maximized
   window are hard to read; prose and run cells wrap at a comfortable measure.
   `contentWidth = min(chromeWidth, 100)`.
2. **One-column left inset + a right gutter.** `LEFT_PAD = 1` (left). The right gutter
   = `max(1, config.reservedColumns)` (default 1; raised to 2 under a Daintree xterm
   whose overlay scrollbar covers the rightmost cells). `chromeWidth = columns - gutter
   - LEFT_PAD`. Content never touches either terminal edge.
3. **Never land glyphs in the autowrap (DECAWM) last column.** The right gutter
   reserves it. A glyph in the last column triggers a deferred wrap on the host and
   orphans a stray row into scrollback.
4. **Measure by terminal-cell width, not rune count.** Truncation/wrap budgets are in
   display cells (wide CJK = 2, combining marks = 0). In Go use
   `github.com/mattn/go-runewidth` (or `x/term` cell width) for every width
   measurement and truncation — never `len(string)` or `len([]rune)`.
5. **Fixed-width rules in committed content** (§7): never a fill-to-width rule.
6. **Live chrome stays narrow (`<= 56`).** The status line is capped to
   `LIVE_CHROME_MAX_WIDTH = 56` so a resize can't make it gain physical rows and
   orphan a duplicate.
7. **Shrinkable filled cards.** The user-message card's filled body must stay
   shrinkable + truncate its lines, so a stale-wide card (width prop lags a live
   resize) can't overflow the live edge and autowrap the *filled* row into scrollback.

### User-message card rendering (`UserMessageCard`)

The human's turn: a dim `YOU` label (DIM+BOLD) above a left **accent bar** (`▏`,
U+258F — sits flush at the cell's left edge) over a *subtle* theme-aware fill — not a
four-sided box. `inner = max(10, width - 4)` (bar col + padding + right margin). Long
messages collapse to head / `+N lines` snip rule / tail (Claude-Code abbreviation).
One bar glyph per rendered row. The fill (`backgroundColor`) and text/bar colors come
from `userMessageSurface(mode)`:

- `dark`: bar `#6B7280`, text `#E5E7EB`, fill `#181D26`.
- `light`: bar `#94A3B8`, text `#1F2937`, fill `#EAEDF1`.
- `ansi`: bar gray, no fill (16-color backgrounds clash unpredictably).
- `none`: bar muted, dim text, no fill.

Daintree's own prose stays bare by contrast (no bar, no fill).

### Theme & glyph fallback (`theme.ts`)

- Semantic palette (`ui.color`): accent green `#6EE7B7`, brand green `#36CE94` (masthead
  only), info cyan `#67E8F9`, warning `#F6C85F`, danger `#FB7185`, blocked `#C4B5FD`,
  muted gray. Colors carry MEANING, not decoration.
- Glyph fallback: `unicodeOk()` is false when `DAINTREE_ASCII=1` or a non-UTF locale
  (`LC_ALL`/`LC_CTYPE`/`LANG` without "utf"). The ASCII set replaces every signature
  glyph (`◆→#`, `◌→*`, `✓→+`, `×→x`, `├─→|-`, `└─→\`-`, `│ →| `, `·→-`, …). Keep the
  full map; branch/badge alignment depends on shape-parallel fallbacks.

---

## 10. Duration formatting (`formatDuration`)

Shared by the activity tree, the live status line, and the status line agent badge:

```
ms < 0 or NaN  → ""
ms < 1000      → "<round(ms)>ms"        e.g. 38ms
secs < 60      → "<secs>s"              e.g. 18s   (secs = round(ms/1000))
otherwise      → "MM:SS"                e.g. 02:05 (mins floor, rem zero-padded)
```

Cost (`formatCost`, status line): `<= 0 → "$0.000"`, `< 0.01 → 4dp`, `< 1 → 3dp`,
else `2dp`.

---

## 11. /clear, resize redraw, attention BEL, window title

### `/clear` — the ONLY path that clears host scrollback

`/clear` resets the session to its initial controls. Sequence
(`useDaintreeController` + `cli/terminalClear.ts`):

1. **Drop the live transcript** (reducer `transcript:clear` → `[]`). Rows already in
   native scrollback STAY there until step 2 wipes them (same as a shell `clear`).
2. **After the cleared (empty) tree is committed**, wipe the host terminal's screen +
   scrollback by writing the escape sequence **`\x1b[2J\x1b[3J\x1b[H`** straight to
   stdout (`HOST_TERMINAL_CLEAR`): `2J` erase viewport, `3J` erase scrollback (the one
   that matters), `H` cursor home. TTY-only, never throws. Do NOT touch the alternate
   buffer (`\x1b[?1049h`).
3. **Force a clean repaint** so the masthead reappears with no gap, then **bump
   `clearNonce`** so the scrollback commit cursor + masthead re-arm (`resetKey` change,
   §2) and re-commit the masthead + the fresh confirmation card.

Ordering is critical: the wipe + repaint must run AFTER the cleared tree commits, or it
repaints the OLD conversation back into scrollback. In TS this rides a `useLayoutEffect`
(post-commit, pre-paint). **In Go:** the `/clear` flow is (a) clear transcript model →
(b) on the next update, issue the `\x1b[2J\x1b[3J\x1b[H` write (a raw stdout side-channel
or a custom `tea.Cmd`) → (c) bump `clearNonce` so the queue owner re-commits the masthead.

### Resize redraw

This whole "nuclear redraw" exists because OpenTUI's split-footer freezes stale footer
rows into scrollback on resize. **The Go / Bubble Tea port does NOT have that bug** —
the OpenTUI replay record, shadow-buffer diff, `resetSplitFooterForReplay`, and
`forceFullRepaintRequested` are OpenTUI internals (DELETE list). But the *width-change
re-render* it also performs IS still needed:

- On a settled `tea.WindowSizeMsg` (debounce a drag storm — TS uses **150ms** via
  `useResizeRedraw`; one redraw also fires once on boot→cockpit handoff), the committed
  blocks were rendered at the OLD width and the host has now reflowed them. **Re-commit
  the masthead + whole transcript fresh at the new width:** bump `redrawNonce` (folds
  into `resetKey` → resets `committed=0` + `headerDone=false`), clear host scrollback
  (`\x1b[2J\x1b[3J\x1b[H` — Go HAS to do the explicit wipe since there is no OpenTUI
  tracked-writer reset), and re-run the commit queue against the new width. `redrawNonce`
  is SEPARATE from `clearNonce` so a resize never reads as a logical "conversation
  cleared" and the transcript model is left intact.

### Attention BEL + window title (`useAttentionSignal`)

Out-of-band cues that survive a focus change (TTY-only, guarded, never throw):

- **BEL `\x07`** on each fresh attention batch the scheduler delivers (the bridge
  `attention` event with `events.length > 0`) — terminal dings/flashes even when
  backgrounded. Fire on the *event*, not an inbox-count increment (count can stay flat
  while events are replaced).
- **OSC 2 window title** mirroring the unresolved inbox count:
  `\x1b]2;Daintree ⚠ <N>\x07` when `N > 0`, else `\x1b]2;Daintree\x07`. Restore a clean
  `Daintree` title on cockpit exit.
- Do NOT implement focus-reporting (`\x1b[?1004h`) or desktop-notification escapes
  (OSC 9 / 777) — out of scope for v1.

### Footer sizing — NOT NEEDED in Go

TS sizes `footerHeight` per-frame via `setFrameCallback` (`useFooterHeight`) because
OpenTUI reserves a fixed number of footer rows that don't auto-track content. Bubble
Tea's `View()` is the footer and its height is whatever string you return — there is no
reserved-row count to track. **Delete `useFooterHeight` entirely.**

---

## 12. Go package & type mapping proposal

```
ui/
  program.go        Bubble Tea program wiring (AltScreen=false, no mouse capture),
                    the boot/exit lifecycle equivalent of runApp.tsx.
  model.go          The root tea.Model: owns transcript []TranscriptCell, RunPhase,
                    dashboard, sessionUsage, view mode, expanded flag, clearNonce,
                    redrawNonce, width/height. Equivalent of DaintreeApp + controller.
  scrollback.go     ScrollbackBlock, the commit queue + ack protocol (§2). Emits
                    persistent-print cmds, tracks headerDone/committed/resetKey, decides
                    liveCells = transcript[committed:]. (Replaces useScrollbackTranscript
                    + scrollback.tsx.)
  theme.go          ui colors, Tone→color/glyph, glyph sets + ASCII fallback, terminal
                    theme mode, userMessageSurface, severity/watcher mapping. (theme.ts)
  markdown.go       glamour-backed render(content,width,theme,expanded) with the
                    (content,width,theme,expanded) cache + table→record-list override
                    + ANSI-strip security. (markdown.ts)
  runstatus.go      RunPhase, liveStatusLabel, runStageLabel, tool→verb stage map.
  duration.go       formatDuration / formatCost.
  components/
    transcript.go   CellView dispatch + TurnCell / NoteCell / CommandCell renderers
                    (renders to styled strings, per ordered TurnStep). (Transcript.tsx
                    + TurnCellView.tsx)
    activitytree.go Branch-tree renderer + presentTool verb map. (ActivityTree.tsx +
                    presentation/tools.ts)
    header.go       Masthead with the fixed-width rule + tier gloss + log badge.
    statusline.go   The ≤56-cell compact rollup. (StatusLine.tsx + liveChrome.go)
    usermsg.go      The YOU card (bar + fill + collapse). (UserMessageCard.tsx)
  attention.go      BEL + OSC2 title cues. (useAttentionSignal.ts)
  hostclear.go      HOST_TERMINAL_CLEAR = "\x1b[2J\x1b[3J\x1b[H" writer. (terminalClear.ts)
```

Key shared constants: `ContentMax = 100`, `LeftPad = 1`, `LiveChromeMaxWidth = 56`,
`ResizeRedrawDelay = 150ms`, `LabelWidth = 11`, `PrefixCols = 5`, `DurationCols = 8`,
`HostTerminalClear = "\x1b[2J\x1b[3J\x1b[H"`.

Width math (once, in the model): `gutter = max(1, reservedColumns)`,
`chromeWidth = max(1, columns - gutter - LeftPad)`,
`contentWidth = min(chromeWidth, ContentMax)`. The masthead and the live footer share
this measure.

> The transcript renderers take `width int` + `expanded bool` + `now int64` and return a
> styled string (lipgloss/glamour), which the scrollback queue commits via persistent
> print or the model embeds in `View()` for live cells. Per §3, `TurnCell.Steps
> []TurnStep` drives ordered rendering, not flat text + activities.

---

## 13. Explicit DELETE list (OpenTUI/React machinery that does NOT port)

These exist only to fight OpenTUI's renderer; Bubble Tea's persistent-print model makes
them unnecessary. **Do not port any of them.**

- **OpenTUI shadow / diff buffers.** `currentRenderBuffer` / `nextRenderBuffer`,
  `forceFullRepaintRequested`, `requestRender`. Bubble Tea re-renders `View()` each
  update; there is no shadow buffer to resync.
- **React portals + off-screen surfaces.** `createPortal`, `createScrollbackSurface`,
  `ScrollbackSurface`, `surface.settle()`, `surface.commitRows()`, `surface.destroy()`,
  `writeToScrollback`, and the whole `ScrollbackCommit` component. Replace with a queue
  that emits persistent-print commands (§2).
- **`settle()` async-highlight wait.** glamour renders synchronously; no async
  tree-sitter convergence to await before snapshotting rows.
- **Footer-height measurement.** `useFooterHeight`, `setFrameCallback` /
  `removeFrameCallback`, `renderer.footerHeight`, the `rootRef` height read, the
  `flexShrink={0}` footer-grow-back workaround. Bubble Tea's `View()` string IS the
  footer (§11).
- **Resize "nuclear redraw" / split-footer replay.** `resetSplitFooterForReplay`,
  `clearSavedLines`, the OpenTUI replay record, `resyncCockpitSurface`. Keep ONLY the
  *width-change re-commit* (re-render committed blocks at the new width, §11) — drop the
  OpenTUI-specific reset plumbing.
- **`screenMode: "split-footer"` / `externalOutputMode: "capture-stdout"` /
  `useMouse:false` renderer options.** Replaced by Bubble Tea defaults: normal screen
  buffer (no `tea.WithAltScreen`), no mouse-capture option.
- **`createCliRenderer` / `createRoot` / `useRenderer` / `useKeyboard` /
  `useTerminalDimensions` / `useLayoutEffect`-driven commit timing.** Replaced by the
  Bubble Tea `Init/Update/View` loop, `tea.WindowSizeMsg`, and `tea.KeyMsg`.
- **OpenTUI `<box>`/`<text>`/`<span>`/`<markdown>` intrinsics + `flexDirection`/
  `flexShrink`/`paddingLeft` yoga layout.** Replaced by lipgloss styles + manual
  string composition. (The yoga `flexDirection:"column"` default note is moot.)

### KEEP (behavioral, not OpenTUI-specific)

The append-only one-at-a-time **commit order + ack discipline** (§2); the **fixed-width
masthead rule** and all **reflow-safe styling rules** (§7, §9 — the host still reflows
committed scrollback on resize, so these stay load-bearing); the **width-based
re-commit on resize** (§11); the **`\x1b[2J\x1b[3J\x1b[H` host wipe on /clear**; the
**BEL + OSC2 title** cues; the **150ms resize debounce**; **cell-width (not rune)
measurement**.
