import { test, expect, describe, mock, beforeEach, afterEach } from "bun:test";
import { act, useState } from "react";
import { testRender } from "@opentui/react/test-utils";
import { UiBridge } from "../../src/ui/bridge.js";
import { useAttentionSignal } from "../../src/ui/hooks/useAttentionSignal.js";

const title = (t: string) => `\x1b]2;${t}\x07`;

/**
 * The ported hook writes straight to `process.stdout` (it no longer reads Ink's
 * managed stdout via `useStdout`). So instead of inspecting render frames we spy
 * on `process.stdout.write` and read the raw escape bytes it received.
 *
 * `process.stdout.isTTY` is the hook's hard gate — under `bun test` stdout is not
 * a TTY, so every test that wants to observe escapes forces `isTTY` true and the
 * suite restores the original value afterwards.
 */
let writeSpy: ReturnType<typeof mock>;
let restoreWrite: () => void;
let restoreIsTTY: () => void;

function spyStdout(tty: boolean) {
  const stream = process.stdout as NodeJS.WriteStream;
  const realWrite = stream.write.bind(stream);
  const realIsTTY = stream.isTTY;

  writeSpy = mock((_chunk: unknown) => {
    // Swallow the write — these are passive escape bytes; don't actually emit
    // them to the test runner's terminal.
    return true;
  });
  (stream as unknown as { write: unknown }).write = writeSpy;
  restoreWrite = () => {
    (stream as unknown as { write: typeof realWrite }).write = realWrite;
  };

  (stream as unknown as { isTTY: boolean | undefined }).isTTY = tty
    ? true
    : undefined;
  restoreIsTTY = () => {
    (stream as unknown as { isTTY: boolean | undefined }).isTTY = realIsTTY;
  };
}

/** All bytes the hook has written so far, concatenated. */
const written = () =>
  writeSpy.mock.calls.map((c) => String(c[0])).join("");

/**
 * Count *bare* BEL bytes — OSC 2 title escapes are also `\x07`-terminated, so a
 * plain `includes("\x07")` would let a stray title write masquerade as a ding.
 * Strip the title escapes first, then count what remains.
 */
const countBells = (s: string) =>
  (s.replace(/\x1b\]2;[^\x07]*\x07/g, "").match(/\x07/g) ?? []).length;

/** Probe component: runs the hook (with a toggle to exercise unmount cleanup). */
let setInbox: (n: number) => void = () => {};
let setMounted: (m: boolean) => void = () => {};

function Harness({
  bridge,
  inboxCount,
}: {
  bridge: UiBridge;
  inboxCount: number;
}) {
  const [count, setCount] = useState(inboxCount);
  const [mounted, setM] = useState(true);
  setInbox = setCount;
  setMounted = setM;
  return mounted ? <Probe bridge={bridge} inboxCount={count} /> : null;
}

function Probe({
  bridge,
  inboxCount,
}: {
  bridge: UiBridge;
  inboxCount: number;
}) {
  useAttentionSignal({ bridge, inboxCount });
  return null;
}

/** Mount the hook and return the test renderer; resets the spy after mount. */
async function renderSignal(
  bridge: UiBridge,
  inboxCount: number,
  tty = true,
) {
  spyStdout(tty);
  const t = await testRender(
    <Harness bridge={bridge} inboxCount={inboxCount} />,
    { width: 20, height: 4 },
  );
  await t.flush();
  return t;
}

beforeEach(() => {
  setInbox = () => {};
  setMounted = () => {};
});

afterEach(() => {
  restoreWrite?.();
  restoreIsTTY?.();
});

describe("useAttentionSignal", () => {
  test("dings (one bare BEL) on a fresh attention batch", async () => {
    const bridge = new UiBridge();
    await renderSignal(bridge, 0);
    writeSpy.mockClear();

    await act(async () => {
      bridge.emit({ type: "attention", events: [{ title: "Tests failed" }] });
    });

    expect(countBells(written())).toBe(1);
  });

  test("rings exactly once for a multi-event batch", async () => {
    const bridge = new UiBridge();
    await renderSignal(bridge, 0);
    writeSpy.mockClear();

    await act(async () => {
      bridge.emit({
        type: "attention",
        events: [{ title: "a" }, { title: "b" }, { title: "c" }],
      });
    });

    expect(countBells(written())).toBe(1);
  });

  test("rings once per batch across successive batches", async () => {
    const bridge = new UiBridge();
    await renderSignal(bridge, 0);
    writeSpy.mockClear();

    await act(async () => {
      bridge.emit({ type: "attention", events: [{ title: "first" }] });
      bridge.emit({ type: "attention", events: [{ title: "second" }] });
    });

    expect(countBells(written())).toBe(2);
  });

  test("does not ding on an empty attention batch", async () => {
    const bridge = new UiBridge();
    await renderSignal(bridge, 0);
    writeSpy.mockClear();

    await act(async () => {
      bridge.emit({ type: "attention", events: [] });
    });

    expect(countBells(written())).toBe(0);
  });

  test("writes the OSC 2 title badge with the inbox count when non-empty", async () => {
    const bridge = new UiBridge();
    await renderSignal(bridge, 0);
    writeSpy.mockClear();

    await act(async () => {
      setInbox(2);
    });

    expect(written()).toContain(title("Daintree ⚠ 2"));
  });

  test("resets the title to plain when the inbox drains to zero", async () => {
    const bridge = new UiBridge();
    await renderSignal(bridge, 2);
    writeSpy.mockClear();

    await act(async () => {
      setInbox(0);
    });

    expect(written()).toContain(title("Daintree"));
  });

  test("restores a clean title on unmount", async () => {
    const bridge = new UiBridge();
    await renderSignal(bridge, 3);
    writeSpy.mockClear();

    await act(async () => {
      setMounted(false);
    });

    expect(written()).toContain(title("Daintree"));
  });

  test("writes nothing when stdout is not a TTY", async () => {
    const bridge = new UiBridge();
    // tty=false leaves isTTY unset, mirroring a piped/non-interactive stdout.
    await renderSignal(bridge, 2, false);
    writeSpy.mockClear();

    await act(async () => {
      bridge.emit({ type: "attention", events: [{ title: "x" }] });
    });

    const out = written();
    expect(out).not.toContain("\x07");
    expect(out).not.toContain("\x1b]2;");
  });

  test("swallows a failing stdout.write so a broken pipe can't crash the cockpit", async () => {
    const bridge = new UiBridge();
    await renderSignal(bridge, 0);
    // Make every subsequent write throw, mirroring a broken pipe.
    writeSpy.mockImplementation(() => {
      throw new Error("pipe broken");
    });

    await act(async () => {
      expect(() => {
        bridge.emit({ type: "attention", events: [{ title: "boom" }] });
        setInbox(4);
      }).not.toThrow();
    });
  });

  test("stops dinging after unmount (subscription cleaned up)", async () => {
    const bridge = new UiBridge();
    await renderSignal(bridge, 0);

    await act(async () => {
      setMounted(false);
    });
    writeSpy.mockClear();

    await act(async () => {
      bridge.emit({ type: "attention", events: [{ title: "late" }] });
    });

    expect(countBells(written())).toBe(0);
  });
});
