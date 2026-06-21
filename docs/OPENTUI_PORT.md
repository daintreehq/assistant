# OpenTUI port — canonical contract

This is the **single source of truth** for migrating the cockpit from Ink to
OpenTUI (`@opentui/react`). Every component/test port must follow it so nothing is
hallucinated. The native resize/scroll fix lives entirely in OpenTUI's Zig core;
our job is a faithful, mechanical-where-possible transform of the React tree.

## Runtime & toolchain (already wired)

- **Runtime is Bun** for the cockpit: OpenTUI's renderer is native FFI and needs
  Bun (or Node ≥26.3.0 `--experimental-ffi`; this repo targets Bun). `npm run dev`
  → `bun run src/cli/index.ts`. `ui:gallery` → `bun run …`.
- **SQLite is runtime-adaptive** (`src/storage/sqliteDriver.ts`): `bun:sqlite`
  under Bun, `node:sqlite` under Node. `db.ts` imports `DatabaseSync` from it. Do
  not import `node:sqlite` directly anywhere.
- **Tests split by runtime**: cockpit UI tests render through the native FFI
  renderer → they run under **`bun test`** (`tests/ui/**`). Everything else stays
  on **vitest/Node** (`vitest.config.ts` excludes `tests/ui/**`). `npm test` runs
  both.
- **JSX**: `tsconfig.json` sets `"jsxImportSource": "@opentui/react"`. Author
  `.tsx` with the lowercase OpenTUI intrinsics; import React hooks from `"react"`.

## Screen-mode architecture (the no-alt-screen invariant)

OpenTUI offers `screenMode: "alternate-screen" | "main-screen" | "split-footer"`.

- **We use `split-footer`** — a growing transcript committed to the terminal's
  native scrollback plus a small LIVE FOOTER (`renderer.root`) pinned at the bottom
  for the in-flight turn, the status line and the composer. This is OpenTUI's true
  equivalent of Ink's inline `<Static>` model. Never `alternate-screen` (it would
  kill xterm's native wheel-scroll / selection / copy-paste — the hard requirement).
- **Why NOT `main-screen`** (the original port's mistake): `main-screen` renders the
  WHOLE React tree into a FIXED viewport and repaints it in place — it does **not**
  spill overflow into native scrollback the way Ink's inline printing did. The moment
  the tree grew taller than the terminal the layout math overflowed and the cockpit
  garbled/interleaved. `split-footer` is the only mode that gives the inline-scroll
  behaviour we want.
- The host (xterm in Daintree) owns scrolling: wheel-where-you-hover, scrollbar,
  selection all come free because committed turns are REAL terminal scrollback lines.
- **How finished content reaches scrollback** (`src/ui/scrollback.tsx`): a *sealed*
  cell (finished turn / standalone note / command result) and the masthead are
  rendered ONCE into a `createScrollbackSurface()` via React `createPortal` (full
  fidelity — native `<markdown>`, spans, tone colors), `settle()`d (markdown's
  tree-sitter highlight is async) and `commitRows()`'d, then the surface is destroyed.
  `useScrollbackTranscript` drives this in transcript order, one commit at a time. The
  scrollback APIs (`createScrollbackSurface` / `writeToScrollback`) THROW unless the
  renderer is created with BOTH `screenMode: "split-footer"` AND
  `externalOutputMode: "capture-stdout"` — both are required in `runApp`.
- **Footer sizing** (`useFooterHeight`): `footerHeight` is the reserved row count and
  does NOT auto-track content, so we measure the live tree's height each frame (via
  `renderer.setFrameCallback`, not a React layout effect — the native layout lags a
  React commit) and set `footerHeight` to it, clamped to the terminal height. The
  footer's root box is `flexShrink={0}` so it keeps its natural height and can grow
  the footer back after a shrink; a shrink forces one full repaint (OpenTUI doesn't
  always clear the rows a shrunk footer vacates).
- **Content insets**: keep the one-column left inset (`LEFT_PAD = 1`) and the
  right gutter (`reservedColumns`, default 2 when embedded) so nothing touches the
  edges or the host overlay scrollbar / DECAWM column.
- **`/clear`**: after wiping the host scrollback, call
  `renderer.resetSplitFooterForReplay({ clearSavedLines: true })` so OpenTUI drops its
  stale saved-line record; `useScrollbackTranscript` then re-commits the masthead on
  top of the fresh scrollback.

## Bootstrap (replaces `runInkApp.tsx`)

```tsx
import { createCliRenderer } from "@opentui/core"
import { createRoot } from "@opentui/react"

const renderer = await createCliRenderer({
  screenMode: "split-footer",          // inline scrollback + live footer; never alt-screen
  externalOutputMode: "capture-stdout", // REQUIRED — the scrollback-commit APIs throw without it
  footerHeight: process.stdout.rows,    // seeded full-height; useFooterHeight shrinks it to fit
  exitOnCtrlC: false,                  // the app owns Ctrl-C shutdown
  useMouse: false,                     // let the HOST own mouse/scroll; don't capture it
  targetFps: 30,
})
const root = createRoot(renderer)
root.render(<DaintreeApp app={app} />)
// shutdown: root.unmount(); renderer.destroy?.(); await app.shutdown()
```

`createRoot(renderer).render(node)` is the mount API (the old standalone
`render()` is deprecated). `renderer.destroy()` tears the native renderer down.

## Intrinsic elements (the tag mapping)

| Ink | OpenTUI | notes |
| --- | --- | --- |
| `<Box>` | `<box>` | flexbox container (Yoga). Children, borders, padding, gap. |
| `<Text>` | `<text>` | text leaf. Inline styling via `<span>`/`<b>`/`<i>`/`<u>`. |
| `<Text bold>` | `<b>…</b>` or `<text attributes>` | see styling below. |
| `<Static>` | — | NOT used in v1 (whole tree is live in main-screen). |
| `<Text color=>` | `<text fg=>` | `fg` / `bg`, hex or `RGBA`. |
| `ink-text-input` / `MultilineInput` | `<input>` / `<textarea>` | native input renderables. |
| `useInput` | `useKeyboard` | global key handler. |
| `useApp().exit` | app-owned shutdown (Ctrl-C handler calls `app.shutdown`) | |
| `useStdout`/`useStdin` | `useRenderer()` | the `CliRenderer`. |
| `useWindowSize` (`{columns,rows}`) | `useTerminalDimensions()` (`{width,height}`) | rename fields. |
| `ink-spinner` | frame-stepping `<text>` (setInterval) or `useTimeline` | no dep. |
| markdown via `marked-terminal` → string in `<Text>` | `<markdown content=>` (native) OR keep ANSI string in `<text>` | prefer native `<markdown>` where feasible; ANSI-string fallback is acceptable. |

### Box props (`<box>`)

`flexDirection`, `flexGrow`, `flexShrink`, `width` (`number | "N%" | "auto"`),
`height`, `minWidth`, `maxWidth`, `padding`/`paddingLeft|Right|Top|Bottom`,
`margin*`, `gap`/`rowGap`/`columnGap`, `alignItems`, `justifyContent`,
`backgroundColor`, `border` (`true | BorderSides[]`), `borderStyle`
(`"single"|"double"|"rounded"|…`), `borderColor`, `title`/`titleAlignment`. Layout
props may also be nested under a `style={{…}}` prop — both work; prefer flat props
for parity with the Ink source.

### Text props (`<text>` / `<span>`)

`fg` (foreground color), `bg`, `attributes` (bold/dim/italic/underline bitflags via
core helpers), and children. For Ink's `dimColor` use a dim foreground (e.g.
`ui.color.muted`) or the dim attribute; for `bold` wrap in `<b>` or use the bold
attribute. Compose styled runs with `<span fg=…>`, `<b>`, `<i>`, `<u>` children of
a `<text>`.

> Color values: hex strings (`"#67E8F9"`) and the named `"gray"` etc. work as
> `fg`/`bg`/`borderColor`. The existing `ui.color.*` / `toneColor()` palette in
> `theme.ts` is unchanged and reused verbatim.

### The exact Ink `<Text>` → OpenTUI rule (memorize this)

`import { TextAttributes } from "@opentui/core"` → `{ NONE:0, BOLD:1, DIM:2,
ITALIC:4, UNDERLINE:8, … }` (combine with `|`).

| Ink | OpenTUI |
| --- | --- |
| `<Text color={c}>` | `<text fg={c}>` |
| `<Text dimColor>` | `<text attributes={TextAttributes.DIM}>` |
| `<Text bold>` | `<text attributes={TextAttributes.BOLD}>` |
| `<Text bold dimColor color={c}>` | `<text fg={c} attributes={TextAttributes.BOLD \| TextAttributes.DIM}>` |
| `<Text wrap="truncate">` | `<text truncate>` |
| **nested inline runs** `<Text><Text bold>a</Text><Text dimColor>b</Text></Text>` | `<text><span attributes={TextAttributes.BOLD}>a</span><span attributes={TextAttributes.DIM}>b</span></text>` |

**Critical:** OpenTUI `<text>` may NOT contain nested `<text>`. An Ink `<Text>`
that wraps other `<Text>` runs becomes one `<text>` whose children are `<span>`
(carrying `fg`/`attributes`). A standalone Ink `<Text>` line → `<text>`. Use the
shared `primitives.tsx` wrappers (`Dim`, `Muted`, etc., ported alongside) instead
of hand-repeating `attributes={…}` everywhere.

## Hooks

- `useKeyboard((key: KeyEvent) => void, { release? })` — global. `key.name`
  (e.g. `"c"`, `"escape"`, `"o"`, `"x"`, `"return"`), `key.ctrl`, `key.shift`,
  `key.alt`, `key.repeated`, `key.eventType`. Replaces `useInput((input, key)=>…)`.
  Note: there is no Ink-style per-component focus gating of global keys — gate in
  the handler by current view/mode (as `DaintreeInkApp` already does).
- `useRenderer(): CliRenderer`, `useTerminalDimensions(): {width,height}`.
- `useTimeline(opts): Timeline` — for animation (the splash). Also fine to keep the
  existing `setTimeout` frame-stepping.
- `<input>`/`<textarea>` props: `value`, `placeholder`, `focused`, `onInput`,
  `onChange`, `onSubmit(value)`, plus (textarea) `onKeyDown`, `onContentChange`.

## Testing pattern (replaces `ink-testing-library`)

`tests/ui/**`, run with `bun test`. Use `bun:test` globals and OpenTUI's harness:

```tsx
import { test, expect } from "bun:test"
import { testRender } from "@opentui/react/test-utils"

test("renders the masthead", async () => {
  const t = await testRender(<Header project="demo" tier="operator" /* … */ />, {
    width: 60, height: 12,
  })
  await t.flush()
  const frame = t.captureCharFrame()          // ≈ ink-testing-library lastFrame()
  expect(frame).toContain("DAINTREE")
  t.resize(40, 12); await t.flush()           // resize assertions
  // t.mockInput.* to drive keys; t.mockMouse.* for mouse
})
```

- `captureCharFrame(): string` is the plain-text frame — assert with
  `.toContain`/regex like the old `lastFrame()`.
- Strip ANSI is unnecessary (captureCharFrame is already text). For color
  assertions use `captureSpans()`.
- Drive keys with `t.mockInput` (the `createMockKeys` API). Replace
  `stdin.write("\r")` etc.
- `bun:test` API ≈ vitest: `test`/`describe`/`expect`/`mock`. Use `mock()` for
  spies (not `vi.fn`).

## Per-file port checklist

Each component: swap intrinsics + props per the table, keep ALL logic/strings/
glyphs/layout identical, import `theme.ts` helpers unchanged. Preserve every
prop name and the exact visible output (glyphs, labels, spacing) so behaviour is
equivalent. Keep the dense "why" comments.

Pure-logic modules that DO NOT import Ink (no change beyond verifying):
`theme.ts`, `presentation/operations.ts`, `presentation/tools.ts`, `types.ts`,
`liveChrome.ts`, `markdown.ts` (if it only produces strings).

Components to port (all under `src/ui/`):
`primitives.tsx`, `components/Header.tsx`, `components/StatusLine.tsx`,
`components/Composer.tsx`, `components/MultilineInput.tsx`,
`components/Transcript.tsx`, `components/TurnCellView.tsx`,
`components/UserMessageCard.tsx`, `components/ApprovalSheet.tsx`,
`components/OperationsView.tsx`, `components/HelpOverlay.tsx`,
`components/ActivityTree.tsx`, `components/ThinkingDot.tsx`,
`components/StartupSplash.tsx`, `ControlRoom.tsx`, `DaintreeInkApp.tsx`
(→ rename concept to the app shell), `runInkApp.tsx` (→ bootstrap),
`dev/gallery.tsx`, `dev/UiGallery.tsx`, hooks as needed.

## Splash animation (must stay equivalent)

`splash/frames.ts` is unchanged (the `SPLASH_FRAMES` block array). `StartupSplash`
keeps: frame-step via `setTimeout(1000/fps)`, hold final frame `lingerMs`, fire
`onComplete` once, skip when `columns <= SPLASH_WIDTH`, per-row green gradient
(`rowColor`), natural height, horizontally centered within `columns - 1`. Render
each row as `<text fg={rowColor(i)}>{line}</text>` inside a centered `<box>`.
