/**
 * The single-column operations cockpit. This is the product surface in narrow
 * (Daintree-sidebar) widths — not an accessory panel. Chat is one section inside
 * it, not the whole UI.
 *
 * Home shows, in human-priority order: header → now/confirm → attention →
 * watchers → terminals → timers → audit → recent → composer. Slash commands open
 * focus pages (details on demand); Esc returns home.
 */
import { Box, Text, useInput } from "ink";
import type { App as DaintreeApp } from "../../cli/app.js";
import type { DaintreeController } from "../hooks/useDaintreeController.js";
import { useTerminalPreview } from "../hooks/useTerminalPreview.js";
import { buildSidebarModel, densityFor } from "./model.js";
import { SidebarHeader } from "./SidebarHeader.js";
import { NowCard } from "./NowCard.js";
import { AttentionSection } from "./AttentionSection.js";
import {
  AuditStrip,
  RecentSection,
  TerminalSection,
  TimerSection,
  WatcherSection,
} from "./sections.js";
import { InlineConfirmCard } from "./InlineConfirmCard.js";
import { Composer } from "../components/Composer.js";
import { cellBudget } from "../../utils/text.js";
import { theme } from "../theme.js";

const PANEL_TITLE: Record<string, string> = {
  watchers: "Watchers",
  inbox: "Needs attention",
  timers: "Timers",
  audit: "Audit",
  help: "Help",
};

const HELP_ROWS: Array<[string, string]> = [
  ["/status", "connection · models · tier"],
  ["/inbox", "attention queue"],
  ["/watchers", "active watchers"],
  ["/timers", "scheduled timers"],
  ["/audit", "recent tool calls"],
  ["/recipes", "loaded · reload · clear"],
  ["/compact", "summarize the session"],
  ["/quit", "exit"],
  ["Esc", "back to home"],
  ["^C", "shut down cleanly"],
];

export function SidebarShell({
  app,
  controller,
  columns,
  rows,
}: {
  app: DaintreeApp;
  controller: DaintreeController;
  columns: number;
  rows: number;
}) {
  const previews = useTerminalPreview(app, controller.dashboard.watchers);
  const density = densityFor(columns, rows);
  const width = cellBudget(columns);
  const model = buildSidebarModel(controller.dashboard, controller.timeline, previews, {
    columns,
    rows,
    now: Date.now(),
    project: app.config.projectPath.split("/").pop() || app.config.projectPath,
    tier: app.config.tier,
    busy: controller.busy,
    pendingConfirm: controller.pendingConfirm,
  });

  // A pending confirm always wins the surface: it must stay mounted (so its Y/N
  // handler resolves) and visible (so a risky action is never hidden behind a
  // focus page).
  const panel = controller.pendingConfirm ? null : controller.activePanel;

  // Esc leaves a focus page; only active while one is open so it never competes
  // with the composer on the home screen.
  useInput(
    (_input, key) => {
      if (key.escape) controller.setActivePanel(null);
    },
    { isActive: panel !== null },
  );

  const headerHeight = 2;
  const composerHeight = 3;
  const bodyHeight = Math.max(3, rows - headerHeight - composerHeight);

  return (
    <Box flexDirection="column" height={rows} width={columns}>
      <SidebarHeader status={model.status} />
      <Box flexGrow={1} flexDirection="column" overflow="hidden">
        {panel ? (
          <FocusPage panel={panel} model={model} bodyHeight={bodyHeight} />
        ) : (
          <Home
            model={model}
            density={density}
            width={width}
            bodyHeight={bodyHeight}
            confirm={controller.pendingConfirm}
            onResolve={controller.resolveConfirm}
          />
        )}
      </Box>
      <Composer
        busy={controller.busy}
        focus={!controller.busy && !controller.pendingConfirm}
        onSubmit={controller.sendUserMessage}
      />
    </Box>
  );
}

/**
 * Render the home surface within an explicit row budget. Ink garbles rows that
 * overflow a fixed-height box, so we allocate lines top-down in priority order
 * and slice each section to fit — the low-value tail (recent → audit → timers)
 * collapses first, exactly as the cockpit should under pressure.
 */
function Home({
  model,
  density,
  width,
  bodyHeight,
  confirm,
  onResolve,
}: {
  model: ReturnType<typeof buildSidebarModel>;
  density: ReturnType<typeof densityFor>;
  width: number;
  bodyHeight: number;
  confirm: DaintreeController["pendingConfirm"];
  onResolve: DaintreeController["resolveConfirm"];
}) {
  const comfortable = density === "comfortable";
  // Leave a line of slack so the body never packs to the exact overflow boundary
  // (Ink drops/garbles rows there). The body flex-grows to fill the rest.
  let budget = Math.max(3, bodyHeight - 1);

  // Now / confirm — always the top of the surface.
  budget -= confirm ? 8 : comfortable ? 4 : 3;

  // Greedily take up to `count` rows at `perRow` lines each, after a 2-line
  // section header (blank + label). Returns how many rows fit.
  const cap = (count: number, perRow: number): number => {
    if (count <= 0 || budget < 2 + perRow) return 0;
    budget -= 2;
    let n = 0;
    while (n < count && budget >= perRow) {
      budget -= perRow;
      n++;
    }
    return n;
  };

  // Attention is always visible (its empty state is reassuring, not noise).
  const attnPerRow = comfortable ? 4 : density === "compact" ? 2 : 1;
  let attention: typeof model.attention | null = null;
  if (model.attention.length === 0) {
    if (budget >= 3) {
      budget -= 3;
      attention = [];
    }
  } else {
    const n = cap(model.attention.length, attnPerRow);
    if (n > 0) attention = model.attention.slice(0, n);
  }

  const watcherN = cap(model.watchers.length, 1);
  const termN = cap(model.terminals.length, comfortable ? 2 : 1);
  const timerN = density === "dense" ? 0 : cap(model.timers.length, 1);
  const auditN = comfortable ? cap(model.audit.length, 1) : 0;
  const recentN = comfortable ? cap(model.recent.length, 1) : 0;

  return (
    <>
      {confirm ? (
        <InlineConfirmCard pending={confirm} onResolve={onResolve} width={width} />
      ) : (
        <NowCard now={model.now} compact={!comfortable} />
      )}
      {attention ? <AttentionSection rows={attention} density={density} /> : null}
      {watcherN > 0 ? (
        <WatcherSection rows={model.watchers.slice(0, watcherN)} density={density} />
      ) : null}
      {termN > 0 ? (
        <TerminalSection rows={model.terminals.slice(0, termN)} density={density} />
      ) : null}
      {timerN > 0 ? <TimerSection rows={model.timers.slice(0, timerN)} /> : null}
      {auditN > 0 ? <AuditStrip rows={model.audit.slice(0, auditN)} /> : null}
      {recentN > 0 ? <RecentSection rows={model.recent.slice(0, recentN)} /> : null}
    </>
  );
}

function FocusPage({
  panel,
  model,
  bodyHeight,
}: {
  panel: string;
  model: ReturnType<typeof buildSidebarModel>;
  bodyHeight: number;
}) {
  // The body still clips with overflow="hidden", so slice focus content to fit
  // rather than overflow (help is ~10 rows, inbox can be multi-line per item).
  const avail = Math.max(1, bodyHeight - 1); // minus the title row
  const list = (overhead: number) => Math.max(0, avail - overhead);
  return (
    <Box flexDirection="column">
      <Box justifyContent="space-between">
        <Text bold color={theme.brand}>
          {PANEL_TITLE[panel] ?? panel}
        </Text>
        <Text dimColor>Esc home</Text>
      </Box>
      {panel === "watchers" ? (
        <WatcherSection rows={model.watchers.slice(0, list(2))} density="comfortable" />
      ) : panel === "inbox" ? (
        <AttentionSection
          rows={model.attention.slice(0, Math.floor(list(2) / 4))}
          density="comfortable"
        />
      ) : panel === "timers" ? (
        <TimerSection rows={model.timers.slice(0, list(2))} />
      ) : panel === "audit" ? (
        <AuditStrip rows={model.audit.slice(0, list(2))} />
      ) : panel === "help" ? (
        <Box flexDirection="column" marginTop={1}>
          {HELP_ROWS.slice(0, list(1)).map(([k, v]) => (
            <Text key={k}>
              <Text color={theme.info}>{k.padEnd(11)}</Text>
              <Text dimColor>{v}</Text>
            </Text>
          ))}
        </Box>
      ) : null}
    </Box>
  );
}
