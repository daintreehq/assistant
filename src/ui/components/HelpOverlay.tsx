import { TextAttributes } from "@opentui/core";
import { BrandMark, KeyHint } from "../primitives.js";
import { ui } from "../theme.js";
import { overlayEntries } from "../../commandRegistry.js";

// Derived from the shared command registry so the overlay can't drift from the
// commands the handlers actually accept (issue #50).
const COMMANDS: Array<[string, string]> = overlayEntries();

const KEYS: Array<[string, string]> = [
  ["^O", "operations surface"],
  ["^X", "expand tool detail in the transcript"],
  ["Tab", "complete a slash command"],
  ["Esc", "return home"],
  ["^C", "shut down cleanly"],
];

// Composer editing — the readline/native-text-field set the input understands.
const EDIT_KEYS: Array<[string, string]> = [
  ["↑ ↓", "recall previous prompts (at line edges)"],
  ["⌥← ⌥→", "move by word (also ^← ^→)"],
  ["Home End", "start / end of line (also ^A ^E)"],
  ["⌥⌫", "delete previous word (also ^W)"],
  ["^U", "delete the whole line (also ⌘⌫)"],
  ["^K", "delete to end of line"],
  ["^Y", "restore the last killed text"],
  ["\\ ⏎", "newline without sending"],
];

export function HelpOverlay({ width = 72 }: { width?: number }) {
  return (
    // Size by yoga against the LIVE terminal, not the numeric `width` prop. The
    // overlay renders in the repainting region in place of the composer, and
    // `width` derives from ControlRoom's lagged `columns` prop. An explicit
    // `width={width}` would briefly exceed a just-shrunk terminal while Daintree
    // animates the pane (#138), wrap the bordered frame, and orphan a stale copy
    // into scrollback. `width="100%"` tracks the live width; `maxWidth` keeps the
    // numeric prop as the readability cap.
    <box
      flexDirection="column"
      borderStyle="rounded"
      borderColor={ui.color.accent}
      paddingLeft={2}
      paddingRight={2}
      paddingTop={1}
      paddingBottom={1}
      width="100%"
      maxWidth={width}
    >
      <BrandMark label="DAINTREE — help" />
      <box marginTop={1} flexDirection="column">
        {COMMANDS.map(([k, v]) => (
          // The Ink original nested two <Text> runs in one row; a native <text>
          // may not contain another <text>, so the runs become <span> children.
          <text key={k}>
            <span fg={ui.color.info}>{k.padEnd(20)}</span>
            <span attributes={TextAttributes.DIM}>{v}</span>
          </text>
        ))}
      </box>
      <box marginTop={1} flexDirection="column">
        {KEYS.map(([k, v]) => (
          // Ink `<Box>` defaults to a row; OpenTUI `<box>` defaults to a column,
          // so the key/description pair needs an explicit `flexDirection="row"`
          // to sit side by side instead of stacking.
          <box key={k} flexDirection="row">
            <box width={20}>
              <KeyHint keyName={k} action="" />
            </box>
            <text attributes={TextAttributes.DIM}>{v}</text>
          </box>
        ))}
      </box>
      <box marginTop={1} flexDirection="column">
        <text fg={ui.color.muted}>editing</text>
        {EDIT_KEYS.map(([k, v]) => (
          <box key={k} flexDirection="row">
            <box width={20}>
              <KeyHint keyName={k} action="" />
            </box>
            <text attributes={TextAttributes.DIM}>{v}</text>
          </box>
        ))}
      </box>
      <box marginTop={1}>
        <text attributes={TextAttributes.DIM}>
          I supervise Daintree and delegate to visible agents — I never edit
          files directly.
        </text>
      </box>
    </box>
  );
}
