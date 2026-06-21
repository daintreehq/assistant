/**
 * A full-width approval sheet that sits directly above the composer — in the
 * user's eyeline, not a floating modal. It defaults visually to DECLINE (the
 * safe choice), and leads with the *consequence* of approving — what gets
 * touched and whether it's reversible — in plain language, not the raw risk
 * class. The tool name stays as a dim secondary label; `V` reveals the raw
 * reason + args. It stays understandable with color stripped.
 *
 * OpenTUI port: Ink `<Box>`/`<Text>` become the native `<box>`/`<text>`; Ink's
 * `useInput` becomes OpenTUI's global `useKeyboard`. The bordered card uses
 * `<box border borderStyle="rounded" borderColor=…>`. Inline `<Text>` runs that
 * nested other `<Text>` become `<span>` children of one `<text>` (a native
 * `<text>` may not contain another `<text>`); `color`→`fg`, `dimColor`→the DIM
 * attribute, `bold`→the BOLD attribute, `inverse`→the inverse fg/bg swap.
 */
import { useState } from "react";
import { TextAttributes } from "@opentui/core";
import { useKeyboard } from "@opentui/react";
import type { PendingConfirm } from "../types.js";
import type { ConfirmRequest } from "../../tools/types.js";
import type { RiskClass } from "../../schemas.js";
import { compactArgs } from "../../utils/text.js";
import { glyphs, ui } from "../theme.js";

/** A risk-specific question — far clearer than a generic "Confirm action". */
function titleFor(req: ConfirmRequest): string {
  const n = req.toolName;
  if (n.includes("push")) return "Push branch to origin?";
  if (n.includes("commit")) return "Commit changes?";
  if (n.startsWith("terminal") || req.risk === "terminal")
    return "Send input to terminal?";
  if (n.includes("worktree")) return "Create worktree?";
  if (req.risk === "git") return "Run a git action?";
  if (req.risk === "external") return "Run an external action?";
  if (req.risk === "system") return "Run a system-level action?";
  return "Approve this action?";
}

/**
 * Plain-English fallback consequence per risk class — used when a tool didn't
 * supply its own `consequence` string. Covers all 8 RiskClass values so the
 * `affects` line is never empty; the five mutating classes (the ones that
 * actually reach an approval sheet) get the most care.
 */
const RISK_CONSEQUENCE: Record<RiskClass, string> = {
  terminal: "Sends input to or spawns a visible terminal.",
  project: "Creates or modifies a worktree or recipe in your project.",
  git: "Stages, commits, or pushes changes in your repository.",
  external: "Makes a network request or contacts an external service.",
  system: "Runs a broad, system-level action that may be hard to undo.",
  read: "Reads data — makes no changes.",
  local: "Updates local CLI state (timers or watchers).",
  ui: "Changes the Daintree UI state.",
};

/** The consequence to lead with: the tool's own phrasing, else the risk fallback.
 * `||` (not `??`) so an empty/blank string also falls back rather than rendering
 * a blank `affects` row. */
function consequenceFor(req: ConfirmRequest): string {
  return req.consequence?.trim() || RISK_CONSEQUENCE[req.risk];
}

function Field({
  label,
  value,
  dim = false,
}: {
  label: string;
  value: string;
  dim?: boolean;
}) {
  // One predictable line per field so the sheet's height never overflows its
  // reserved budget (which would overlap rows at narrow widths).
  return (
    <text truncate>
      <span attributes={TextAttributes.DIM}>{("  " + label).padEnd(10)}</span>
      <span attributes={dim ? TextAttributes.DIM : TextAttributes.NONE}>
        {value}
      </span>
    </text>
  );
}

export function ApprovalSheet({
  pending,
  width = 72,
  onResolve,
}: {
  pending: PendingConfirm;
  width?: number;
  onResolve: (approved: boolean) => void;
}) {
  const [showArgs, setShowArgs] = useState(false);
  // Collapse the inspect panel whenever a different request takes the sheet, so
  // a fresh prompt never inherits the previous one's expanded raw view. Done by
  // adjusting state *during render* (the React-sanctioned pattern for resetting
  // state on a prop change) rather than in an effect — an effect would let the
  // new request's first frame flash the previous one's raw args before clearing.
  const [shownFor, setShownFor] = useState(pending.id);
  if (shownFor !== pending.id) {
    setShownFor(pending.id);
    setShowArgs(false);
  }
  useKeyboard((key) => {
    if (/^y$/i.test(key.name)) onResolve(true);
    else if (/^n$/i.test(key.name) || key.name === "escape") onResolve(false);
    else if (/^v$/i.test(key.name)) setShowArgs((v) => !v);
  });

  const req = pending.request;
  const set = glyphs();
  return (
    // Size by yoga against the LIVE terminal, not the numeric `width` prop. The
    // sheet lives in the repainting region, and `width` derives from ControlRoom's
    // `columns` prop, which lags the real width by a render tick while Daintree
    // animates the pane on show/hide (#138). An explicit `width={width}` would
    // momentarily exceed a just-shrunk terminal, wrap the bordered row, and orphan
    // a stale copy into scrollback. `width="100%"` resolves against the live width
    // on every relayout; `maxWidth` keeps the numeric prop as the readability cap.
    // Fields below already `truncate`, so the body clips to match. The native Zig
    // renderer reflows the bordered card cleanly on resize.
    <box
      flexDirection="column"
      border
      borderStyle="rounded"
      borderColor={ui.color.warning}
      paddingLeft={1}
      paddingRight={1}
      width="100%"
      maxWidth={width}
    >
      <text fg={ui.color.warning} attributes={TextAttributes.BOLD} truncate>
        {set.attention} {titleFor(req)}
      </text>
      <Field label="affects" value={consequenceFor(req)} />
      <Field label="tool" value={req.toolName} dim />
      {showArgs ? (
        <>
          <Field label="reason" value={req.summary} dim />
          <Field label="args" value={compactArgs(req.args, width - 12)} dim />
        </>
      ) : null}
      <box marginTop={1}>
        {/* One line (truncate) so the sheet height stays predictable.
            Default visually to decline: it's inverse, approve is plain. */}
        <text truncate>
          <span fg={ui.color.accent}>Y approve</span>
          <span attributes={TextAttributes.DIM}> · </span>
          <span fg={ui.color.danger} attributes={TextAttributes.INVERSE}>
            {" "}
            N decline{" "}
          </span>
          <span attributes={TextAttributes.DIM}> · V inspect · Esc</span>
        </text>
      </box>
    </box>
  );
}
