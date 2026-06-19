/**
 * Standalone visual bake-off for the user-message redesign:
 *   npx tsx src/ui/dev/userMessageVariants.tsx
 *
 * Renders the same two sample messages under each candidate treatment plus a bit
 * of Daintree prose underneath, so the "who said what" contrast is visible. This
 * file is a throwaway design aid — delete it once a variant is chosen.
 */
import { render, Box, Text } from "ink";
import { createElement } from "react";
import { ui } from "../theme.js";

const SHORT = "Fix the watcher tests and tell me when the branch is ready.";
const LONG =
  "Fix the watcher tests, then once they pass open a PR against main with a\n" +
  "summary of the race condition we found, link the failing run, and schedule\n" +
  "a follow-up to prune stale terminals tomorrow morning.";

const BG = "#181D26"; // a few % above terminal black
const TEXT = "#E5E7EB"; // light, ~AA contrast on BG
const BAR = "#6B7280"; // cool neutral, brighter than the fill

function Prose() {
  return (
    <Box flexDirection="column" marginBottom={1}>
      <Text bold color={ui.color.accent}>
        ◆ DAINTREE
      </Text>
      <Text>On it — delegating the repair and attaching a watcher.</Text>
    </Box>
  );
}

function Label({ children }: { children: string }) {
  return (
    <Text color={ui.color.info} bold>
      {children}
    </Text>
  );
}

/** A) Left bar ▏ + subtle fill + light text, NO label (the Claude-Code look). */
function VariantA({ text }: { text: string }) {
  const lines = text.split("\n");
  return (
    <Box flexDirection="row" marginBottom={1}>
      <Box flexDirection="column" flexShrink={0}>
        {lines.map((_, i) => (
          <Text key={i} color={BAR}>
            ▏
          </Text>
        ))}
      </Box>
      <Box flexDirection="column" paddingX={1} backgroundColor={BG} flexShrink={0}>
        {lines.map((line, i) => (
          <Text key={i} color={TEXT}>
            {line}
          </Text>
        ))}
      </Box>
    </Box>
  );
}

/** Bar column + filled text block, reused by the YOU-label variants below. */
function BarBlock({ text }: { text: string }) {
  const lines = text.split("\n");
  return (
    <Box flexDirection="row">
      <Box flexDirection="column" flexShrink={0}>
        {lines.map((_, i) => (
          <Text key={i} color={BAR}>
            ▏
          </Text>
        ))}
      </Box>
      <Box flexDirection="column" paddingX={1} backgroundColor={BG} flexShrink={0}>
        {lines.map((line, i) => (
          <Text key={i} color={TEXT}>
            {line}
          </Text>
        ))}
      </Box>
    </Box>
  );
}

/** B1) Very subtle YOU — dim gray, stacked above the bar. The quietest label. */
function VariantB1({ text }: { text: string }) {
  return (
    <Box flexDirection="column" marginBottom={1}>
      <Text dimColor bold>
        YOU
      </Text>
      <BarBlock text={text} />
    </Box>
  );
}

/** B2) YOU in the bar's own color — a touch more present than dim gray. */
function VariantB2({ text }: { text: string }) {
  return (
    <Box flexDirection="column" marginBottom={1}>
      <Text color={BAR} bold>
        YOU
      </Text>
      <BarBlock text={text} />
    </Box>
  );
}

/** B3) Inline YOU tag — sits on the bar line itself, left of the fill. Compact. */
function VariantB3({ text }: { text: string }) {
  const lines = text.split("\n");
  return (
    <Box flexDirection="row" marginBottom={1}>
      <Box flexDirection="column" flexShrink={0}>
        {lines.map((_, i) => (
          <Text key={i} color={BAR}>
            ▏{i === 0 ? <Text bold> YOU</Text> : "    "}
          </Text>
        ))}
      </Box>
      <Box flexDirection="column" paddingX={1} backgroundColor={BG} flexShrink={0}>
        {lines.map((line, i) => (
          <Text key={i} color={TEXT}>
            {line}
          </Text>
        ))}
      </Box>
    </Box>
  );
}

/** C) Left bar ▏ + light text, NO background (the "remove the fill" idea). */
function VariantC({ text }: { text: string }) {
  const lines = text.split("\n");
  return (
    <Box flexDirection="row" marginBottom={1}>
      <Box flexDirection="column" flexShrink={0}>
        {lines.map((_, i) => (
          <Text key={i} color={BAR}>
            ▏
          </Text>
        ))}
      </Box>
      <Box flexDirection="column" paddingLeft={1} flexShrink={0}>
        {lines.map((line, i) => (
          <Text key={i} color={TEXT}>
            {line}
          </Text>
        ))}
      </Box>
    </Box>
  );
}

/** D) Claude-Code echo: leading › chevron + subtle fill, no bar, no label. */
function VariantD({ text }: { text: string }) {
  const lines = text.split("\n");
  return (
    <Box flexDirection="column" paddingX={1} backgroundColor={BG} marginBottom={1}>
      {lines.map((line, i) => (
        <Text key={i} color={TEXT}>
          {i === 0 ? <Text color={BAR}>{"› "}</Text> : "  "}
          {line}
        </Text>
      ))}
    </Box>
  );
}

/** Current: full rounded box + fill + YOU label (what we're replacing). */
function Current({ text }: { text: string }) {
  const lines = text.split("\n");
  return (
    <Box flexDirection="column" marginBottom={1}>
      <Text dimColor bold>
        YOU
      </Text>
      <Box
        flexDirection="column"
        borderStyle="round"
        borderColor="#374151"
        paddingX={1}
        backgroundColor="#1F2937"
      >
        {lines.map((line, i) => (
          <Text key={i} color="#D1D5DB">
            {line}
          </Text>
        ))}
      </Box>
    </Box>
  );
}

const VARIANTS = [
  { label: "A   — bar ▏ + subtle fill, NO label (current)", C: VariantA },
  { label: "B1  — + very subtle YOU (dim gray, stacked)", C: VariantB1 },
  { label: "B2  — + YOU in the bar color (a touch more present)", C: VariantB2 },
  { label: "B3  — + inline YOU tag on the bar line (compact)", C: VariantB3 },
  { label: "C   — bar ▏ + light text, NO fill", C: VariantC },
  { label: "D   — › chevron + subtle fill (no bar)", C: VariantD },
  { label: "CURRENT-OLD — full box + YOU label (what we replaced)", C: Current },
];

function App() {
  return (
    <Box flexDirection="column" paddingX={2} paddingY={1}>
      <Text bold>User-message design bake-off — same content under each treatment</Text>
      <Text dimColor>(dark theme; run with DAINTREE_THEME=ansi or =none to check fallbacks)</Text>
      <Box height={1} />
      {VARIANTS.map((v) => (
        <Box key={v.label} flexDirection="column" marginBottom={1}>
          <Label>{v.label}</Label>
          <Box height={1} />
          <v.C text={SHORT} />
          <v.C text={LONG} />
          <Prose />
          <Text dimColor>{"─".repeat(64)}</Text>
        </Box>
      ))}
    </Box>
  );
}

render(createElement(App), { exitOnCtrlC: true });
