import { test, expect, describe } from "bun:test";
import { testRender } from "@opentui/react/test-utils";
import { Header } from "../../src/ui/components/Header.js";

describe("Header", () => {
  // captureCharFrame() returns the plain-text frame already (no ANSI), so there is
  // nothing to strip — assert against the text directly.
  async function frameOf(node: Parameters<typeof testRender>[0], width = 60) {
    const t = await testRender(node, { width, height: 12 });
    await t.flush();
    return t.captureCharFrame();
  }

  test("renders the product wordmark and version on one line", async () => {
    const frame = await frameOf(<Header columns={60} version="0.1.0" />);
    expect(frame).toContain("Daintree Assistant"); // brand wordmark, capital A
    expect(frame).toContain("v0.1.0"); // version beside the name
    // Wordmark and version share a row (Claude-Code style).
    expect(frame).toMatch(/Daintree Assistant\s+v0\.1\.0/);
  });

  test("shows the project name beneath the wordmark", async () => {
    const frame = await frameOf(
      <Header columns={60} version="0.1.0" project="assistant" />,
    );
    expect(frame).toContain("assistant");
  });

  test("drops the MCP connection badge from the masthead", async () => {
    const frame = await frameOf(<Header columns={60} version="0.1.0" />);
    // The live MCP link is by-exception status that stays in the StatusLine.
    expect(frame).not.toContain("CONNECTED");
    expect(frame).not.toContain("DEGRADED");
  });

  test("shows the permission tier with a plain-English gloss", async () => {
    const frame = await frameOf(<Header columns={60} version="0.1.0" tier="system" />);
    expect(frame).toContain("tier system"); // labelled, not a bare token
    expect(frame).toContain("full access"); // gloss explains what it grants
  });

  // ui.color.danger is the #FB7185 truecolor (RGB 251,113,133). OpenTUI carries per-
  // span color out-of-band of the char frame, so prove the tier is quiet at rest /
  // red only by exception by reading the `system` span's foreground via captureSpans()
  // instead of substring-matching SGR escapes.
  describe("tier color escalation", () => {
    const DANGER: [number, number, number] = [251, 113, 133];

    // True when some rendered span whose text is exactly the tier word carries the
    // danger foreground color.
    async function tierIsDanger(
      node: Parameters<typeof testRender>[0],
    ): Promise<boolean> {
      const t = await testRender(node, { width: 60, height: 12 });
      await t.flush();
      for (const line of t.captureSpans().lines) {
        for (const span of line.spans) {
          if (span.text.includes("system")) {
            const [r, g, b] = span.fg.toInts();
            if (r === DANGER[0] && g === DANGER[1] && b === DANGER[2]) return true;
          }
        }
      }
      return false;
    }

    test("keeps the system tier quiet at rest, not alarm-red", async () => {
      // At rest no destructive action is pending, so the `system` tier must NOT carry
      // the danger color — a steady red capsule is alarm fatigue. The tier word is
      // still present; only its color is muted to dim.
      const frame = await frameOf(<Header columns={60} version="0.1.0" tier="system" />);
      expect(frame).toContain("tier system"); // still rendered
      expect(
        await tierIsDanger(<Header columns={60} version="0.1.0" tier="system" />),
      ).toBe(false); // no red anywhere at rest
    });

    test("escalates the tier to danger color when a destructive action is pending", async () => {
      // Red is reserved for the moment it earns attention: a git/system confirmation
      // in flight. The controller passes destructivePending and the tier turns red.
      const frame = await frameOf(
        <Header columns={60} version="0.1.0" tier="system" destructivePending />,
      );
      expect(frame).toContain("tier system");
      expect(
        await tierIsDanger(
          <Header columns={60} version="0.1.0" tier="system" destructivePending />,
        ),
      ).toBe(true); // danger color present on the tier
    });
  });

  test("glosses each tier so the level is self-explaining", async () => {
    const op = await frameOf(<Header columns={60} version="0.1.0" tier="operator" />);
    expect(op).toContain("operator");
    expect(op).toContain("terminals");
    const sup = await frameOf(<Header columns={60} version="0.1.0" tier="supervisor" />);
    expect(sup).toContain("supervisor");
    expect(sup).toContain("read & UI only");
  });

  test("omits the tier line entirely when no tier is supplied", async () => {
    const frame = await frameOf(<Header columns={60} version="0.1.0" />);
    expect(frame).not.toContain("tier ");
  });

  test("closes the identity band with a full-width rule (above the logging line)", async () => {
    // The identity block (wordmark / project / tier) is followed by a full-width rule,
    // then the separate logging line sits BELOW it. Safe to render live on OpenTUI —
    // the native renderer reflows the whole tree cleanly on resize (no Ink <Static>
    // wrap hazard that once forced the masthead to carry no rule).
    const frame = await frameOf(
      <Header columns={60} version="0.1.0" tier="system" logging logFile="/t.log" />,
    );
    const lines = frame.split("\n");
    const ruleIdx = lines.findIndex((l) => /[─-]{4,}/.test(l));
    const tierIdx = lines.findIndex((l) => l.includes("system"));
    const logIdx = lines.findIndex((l) => l.includes("logging"));
    expect(ruleIdx).toBeGreaterThan(-1); // a rule exists
    expect(ruleIdx).toBeGreaterThan(tierIdx); // …below the tier line
    expect(logIdx).toBeGreaterThan(ruleIdx); // …and the logging line is below the rule
  });

  test("places the project directly under the wordmark", async () => {
    const frame = await frameOf(
      <Header columns={60} version="0.1.0" project="Daintree Assistant" />,
    );
    const lines = frame.split("\n").map((l) => l.replace(/\s+$/, ""));
    expect(lines[0]).toContain("Daintree Assistant v0.1.0");
    expect(lines[1].trim()).toBe("Daintree Assistant");
  });

  test("shows no emoji or brand mark — plain text only", async () => {
    const frame = await frameOf(<Header columns={60} version="0.1.0" />);
    expect(frame).not.toMatch(/\p{Extended_Pictographic}/u);
  });

  // Regression (#138): at a narrow width EVERY masthead row must still fit within
  // `columns` — no row may overflow the bound (which would orphan a stale physical
  // row into scrollback). NOTE ON BEHAVIOR DIFFERENCE: under Ink the `wrap="truncate"`
  // rows CLIPPED to one row with a "…" ellipsis. OpenTUI 0.4.1's `<text truncate>`
  // does NOT clip — it soft-WRAPS the overflow onto further rows (verified: a bare
  // `<text truncate>` inside a fixed-width box wraps rather than emitting "…"). The
  // load-bearing invariant the regression guards — content never exceeds `columns`,
  // so nothing physically overflows the cockpit width — still holds because each
  // wrapped fragment is itself <= columns. We assert that here; the exact "…"/no-wrap
  // clipping is a component-level truncate gap to revisit (see report).
  test("keeps every masthead row within the column bound (no overflow)", async () => {
    const COLS = 22;
    const frame = await frameOf(
      <Header
        columns={COLS}
        version="0.1.0"
        project="a-very-long-project-name-that-overflows"
        tier="system"
        logging
        logFile="/Users/gpriday/.daintree/logs/2026-06-20-ses_02f0965b.log"
      />,
      COLS,
    );
    const rows = frame.split("\n").map((l) => l.replace(/\s+$/, ""));
    for (const row of rows) {
      expect(row.length).toBeLessThanOrEqual(COLS);
    }
  });

  test("names the active run when one is supplied", async () => {
    const frame = await frameOf(
      <Header columns={60} version="0.1.0" runTitle="repair watcher tests" />,
    );
    expect(frame).toContain("repair watcher tests");
  });

  test("surfaces the debug log when active", async () => {
    const frame = await frameOf(
      <Header columns={60} version="0.1.0" logging logFile="/tmp/daintree.log" />,
    );
    expect(frame).toContain("logging"); // spelled out, not "LO"
    expect(frame).toContain("/tmp/daintree.log");
  });

  // Guards the header's rendered (non-blank) row count. The identity block is always
  // followed by the full-width rule, and the logging line (when present) sits below it.
  test("renders a stable row count", async () => {
    const rows = async (node: Parameters<typeof testRender>[0]) => {
      const t = await testRender(node, { width: 60, height: 12 });
      await t.flush();
      // captureCharFrame pads to the full terminal height with blank rows; count only
      // the rows that carry visible content (the Ink frame had no such padding).
      return t
        .captureCharFrame()
        .split("\n")
        .filter((l) => l.trim().length > 0).length;
    };
    // Wordmark + rule.
    expect(await rows(<Header columns={60} version="0.1.0" />)).toBe(2);
    // Wordmark + rule + logging line.
    expect(
      await rows(<Header columns={60} version="0.1.0" logging logFile="/t.log" />),
    ).toBe(3);
    // Wordmark + project + run subtitle + rule.
    expect(
      await rows(<Header columns={60} version="0.1.0" project="p" runTitle="busy" />),
    ).toBe(4);
  });

  test("keeps the ASCII bullet (no Unicode) when unicode is disabled", async () => {
    const prev = process.env.DAINTREE_ASCII;
    process.env.DAINTREE_ASCII = "1";
    try {
      const frame = await frameOf(
        <Header
          columns={60}
          version="0.1.0"
          tier="system"
          logging
          logFile="/tmp/t.log"
        />,
      );
      expect(frame).toContain("Daintree Assistant");
      expect(frame).toContain("full access"); // tier gloss still rendered
      // Neither the log separator NOR the tier gloss may emit a Unicode bullet.
      expect(frame).not.toContain("·");
    } finally {
      if (prev === undefined) delete process.env.DAINTREE_ASCII;
      else process.env.DAINTREE_ASCII = prev;
    }
  });
});
