import { test, expect, describe } from "bun:test";
import { act, useState } from "react";
import { testRender } from "@opentui/react/test-utils";
import { useResizeRedraw } from "../../src/ui/hooks/useResizeRedraw.js";

const tick = (ms: number) => new Promise((r) => setTimeout(r, ms));

// A comfortably wide debounce window so the burst-coalescing check is robust: the three
// rapid state changes land well inside DELAY even under CI load, and each settled
// assertion waits > DELAY before checking. Tests stay sub-second regardless.
const DELAY = 120;

type State = { enabled: boolean; columns: number; rows: number };

let drive: ((s: State) => void) | null = null;
let redraws = 0;

function Harness({ initial }: { initial: State }) {
  const [s, setS] = useState(initial);
  drive = setS;
  useResizeRedraw({
    enabled: s.enabled,
    columns: s.columns,
    rows: s.rows,
    onRedraw: () => {
      redraws += 1;
    },
    delayMs: DELAY,
  });
  return null;
}

describe("useResizeRedraw", () => {
  test("disabled is inert; enable + resizes each fire one debounced redraw; bursts coalesce", async () => {
    redraws = 0;
    const t = await testRender(
      <Harness initial={{ enabled: false, columns: 80, rows: 24 }} />,
      { width: 80, height: 24 },
    );
    await t.flush();
    await tick(DELAY * 2);
    // Booting (disabled): nothing is scheduled even on mount.
    expect(redraws).toBe(0);

    // A resize WHILE disabled must stay inert — the splash owns the screen.
    await act(async () => drive!({ enabled: false, columns: 100, rows: 30 }));
    await tick(DELAY * 2);
    expect(redraws).toBe(0);

    // Boot hand-off: the first enabled render fires exactly one redraw (clears splash
    // residue, commits the masthead on a clean scrollback).
    await act(async () => drive!({ enabled: true, columns: 100, rows: 30 }));
    await tick(DELAY * 2);
    expect(redraws).toBe(1);

    // A single resize fires exactly one more.
    await act(async () => drive!({ enabled: true, columns: 120, rows: 30 }));
    await tick(DELAY * 2);
    expect(redraws).toBe(2);

    // A burst of resizes inside one debounce window coalesces into a SINGLE redraw —
    // no tick between them, so the prior timer is cleared each time.
    await act(async () => drive!({ enabled: true, columns: 121, rows: 31 }));
    await act(async () => drive!({ enabled: true, columns: 122, rows: 32 }));
    await act(async () => drive!({ enabled: true, columns: 123, rows: 33 }));
    await tick(DELAY * 2);
    expect(redraws).toBe(3);

    // Re-rendering with the SAME size schedules nothing (no phantom redraw).
    await act(async () => drive!({ enabled: true, columns: 123, rows: 33 }));
    await tick(DELAY * 2);
    expect(redraws).toBe(3);

    // A pending redraw is CANCELLED if we disable before its timer fires — the same
    // cleanup path React runs on unmount/teardown, so a settled redraw can't fire against
    // a torn-down renderer. Change size (schedules a timer) then disable within the window.
    await act(async () => drive!({ enabled: true, columns: 140, rows: 40 }));
    await act(async () => drive!({ enabled: false, columns: 140, rows: 40 }));
    await tick(DELAY * 2);
    expect(redraws).toBe(3); // still 3 — the pending redraw was cancelled, not fired

    t.renderer.destroy?.();
  });
});
