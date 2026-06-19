/**
 * A full-width approval sheet that sits directly above the composer — in the
 * user's eyeline, not a floating modal. It defaults visually to DECLINE (the
 * safe choice), and leads with the *consequence* of approving — what gets
 * touched and whether it's reversible — in plain language, not the raw risk
 * class. The tool name stays as a dim secondary label; `V` reveals the raw
 * reason + args. It stays understandable with color stripped.
 */
import { useEffect, useState } from "react";
import { Box, Text, useInput } from "ink";
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

/** The consequence to lead with: the tool's own phrasing, else the risk fallback. */
function consequenceFor(req: ConfirmRequest): string {
  return req.consequence ?? RISK_CONSEQUENCE[req.risk];
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
    <Text wrap="truncate">
      <Text dimColor>{("  " + label).padEnd(10)}</Text>
      <Text dimColor={dim}>{value}</Text>
    </Text>
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
  // a fresh prompt never inherits the previous one's expanded raw view.
  useEffect(() => setShowArgs(false), [pending.id]);
  useInput((input, key) => {
    if (/^y$/i.test(input)) onResolve(true);
    else if (/^n$/i.test(input) || key.escape) onResolve(false);
    else if (/^v$/i.test(input)) setShowArgs((v) => !v);
  });

  const req = pending.request;
  const set = glyphs();
  return (
    <Box
      flexDirection="column"
      borderStyle="round"
      borderColor={ui.color.warning}
      paddingX={1}
      width={width}
    >
      <Text color={ui.color.warning} bold wrap="truncate">
        {set.attention} {titleFor(req)}
      </Text>
      <Field label="affects" value={consequenceFor(req)} />
      <Field label="tool" value={req.toolName} dim />
      {showArgs ? (
        <>
          <Field label="reason" value={req.summary} dim />
          <Field label="args" value={compactArgs(req.args, width - 12)} dim />
        </>
      ) : null}
      <Box marginTop={1}>
        {/* One line (wrap=truncate) so the sheet height stays predictable.
            Default visually to decline: it's inverse, approve is plain. */}
        <Text wrap="truncate">
          <Text color={ui.color.accent}>Y approve</Text>
          <Text dimColor> · </Text>
          <Text inverse color={ui.color.danger}>
            {" "}
            N decline{" "}
          </Text>
          <Text dimColor> · V inspect · Esc</Text>
        </Text>
      </Box>
    </Box>
  );
}
