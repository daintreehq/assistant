import { render } from "ink-testing-library";
import { UiBridge } from "../../src/ui/bridge.js";
import { useAttentionSignal } from "../../src/ui/hooks/useAttentionSignal.js";

const title = (t: string) => `\x1b]2;${t}\x07`;

/** Minimal host so the hook runs inside an Ink render tree (useStdout needs it). */
function Harness({
  bridge,
  inboxCount,
}: {
  bridge: UiBridge;
  inboxCount: number;
}) {
  useAttentionSignal({ bridge, inboxCount });
  return null;
}

/**
 * ink-testing-library's Stdout stub has no `isTTY`, so the hook's TTY gate
 * suppresses every write by default. Tests that want to observe the escapes opt
 * in by forcing it true on the captured stream before driving the hook.
 */
function renderSignal(bridge: UiBridge, inboxCount: number, tty = true) {
  const r = render(<Harness bridge={bridge} inboxCount={inboxCount} />);
  if (tty) (r.stdout as unknown as { isTTY: boolean }).isTTY = true;
  return r;
}

const written = (frames: string[]) => frames.join("");

/**
 * Count *bare* BEL bytes — OSC 2 title escapes are also `\x07`-terminated, so a
 * plain `includes("\x07")` would let a stray title write masquerade as a ding.
 * Strip the title escapes first, then count what remains.
 */
const countBells = (s: string) =>
  (s.replace(/\x1b\]2;[^\x07]*\x07/g, "").match(/\x07/g) ?? []).length;

describe("useAttentionSignal", () => {
  it("dings (one bare BEL) on a fresh attention batch", () => {
    const bridge = new UiBridge();
    const { stdout } = renderSignal(bridge, 0);
    const before = stdout.frames.length;

    bridge.emit({ type: "attention", events: [{ title: "Tests failed" }] });

    expect(countBells(written(stdout.frames.slice(before)))).toBe(1);
  });

  it("rings exactly once for a multi-event batch", () => {
    const bridge = new UiBridge();
    const { stdout } = renderSignal(bridge, 0);
    const before = stdout.frames.length;

    bridge.emit({
      type: "attention",
      events: [{ title: "a" }, { title: "b" }, { title: "c" }],
    });

    expect(countBells(written(stdout.frames.slice(before)))).toBe(1);
  });

  it("rings once per batch across successive batches", () => {
    const bridge = new UiBridge();
    const { stdout } = renderSignal(bridge, 0);
    const before = stdout.frames.length;

    bridge.emit({ type: "attention", events: [{ title: "first" }] });
    bridge.emit({ type: "attention", events: [{ title: "second" }] });

    expect(countBells(written(stdout.frames.slice(before)))).toBe(2);
  });

  it("does not ding on an empty attention batch", () => {
    const bridge = new UiBridge();
    const { stdout } = renderSignal(bridge, 0);
    const before = stdout.frames.length;

    bridge.emit({ type: "attention", events: [] });

    expect(countBells(written(stdout.frames.slice(before)))).toBe(0);
  });

  it("writes the OSC 2 title badge with the inbox count when non-empty", () => {
    const bridge = new UiBridge();
    // The stub gains isTTY only after render(), so drive the badge via an update
    // (the effect re-runs on count change). On a real TTY the mount write fires
    // the same effect body — the mount/update distinction is not a separate path.
    const { stdout, rerender } = renderSignal(bridge, 0);
    const before = stdout.frames.length;

    rerender(<Harness bridge={bridge} inboxCount={2} />);

    expect(written(stdout.frames.slice(before))).toContain(
      title("Daintree ⚠ 2"),
    );
  });

  it("resets the title to plain when the inbox drains to zero", () => {
    const bridge = new UiBridge();
    const { stdout, rerender } = renderSignal(bridge, 2);

    const before = stdout.frames.length;
    rerender(<Harness bridge={bridge} inboxCount={0} />);

    expect(written(stdout.frames.slice(before))).toContain(title("Daintree"));
  });

  it("restores a clean title on unmount", () => {
    const bridge = new UiBridge();
    const { stdout, unmount } = renderSignal(bridge, 3);

    const before = stdout.frames.length;
    unmount();

    expect(written(stdout.frames.slice(before))).toContain(title("Daintree"));
  });

  it("writes nothing when stdout is not a TTY", () => {
    const bridge = new UiBridge();
    // tty=false leaves isTTY unset, mirroring a piped/non-interactive stdout.
    const { stdout } = renderSignal(bridge, 2, false);
    const before = stdout.frames.length;

    bridge.emit({ type: "attention", events: [{ title: "x" }] });

    const out = written(stdout.frames.slice(before));
    expect(out).not.toContain("\x07");
    expect(out).not.toContain("\x1b]2;");
  });

  it("swallows a failing stdout.write so a broken pipe can't crash the cockpit", () => {
    const bridge = new UiBridge();
    const { stdout, rerender } = renderSignal(bridge, 0);
    const real = stdout.write;
    (stdout as unknown as { write: () => void }).write = () => {
      throw new Error("pipe broken");
    };

    try {
      expect(() => {
        bridge.emit({ type: "attention", events: [{ title: "boom" }] });
        rerender(<Harness bridge={bridge} inboxCount={4} />);
      }).not.toThrow();
    } finally {
      // Restore the real writer so Ink's own teardown render doesn't hit the
      // throwing stub and surface an unhandled error.
      (stdout as unknown as { write: typeof real }).write = real;
    }
  });

  it("stops dinging after unmount (subscription cleaned up)", () => {
    const bridge = new UiBridge();
    const { stdout, unmount } = renderSignal(bridge, 0);
    unmount();
    const before = stdout.frames.length;

    bridge.emit({ type: "attention", events: [{ title: "late" }] });

    expect(countBells(written(stdout.frames.slice(before)))).toBe(0);
  });
});
