import { test, expect, describe, beforeEach, afterEach } from "bun:test";
import { act } from "react";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { testRender } from "@opentui/react/test-utils";
import { App } from "../../src/cli/app.js";
import {
  useDaintreeController,
  type DaintreeController,
} from "../../src/ui/hooks/useDaintreeController.js";
import { HOST_TERMINAL_CLEAR } from "../../src/cli/terminalClear.js";

const tick = (ms = 40) => new Promise((r) => setTimeout(r, ms));

/** Mounts the controller and publishes the latest value to the caller's holder. */
function Harness({
  app,
  onController,
  renderer,
}: {
  app: App;
  onController: (c: DaintreeController) => void;
  // Optional stub renderer; the controller takes it by param (not useRenderer) so a
  // test can drive the /clear full-repaint resync without the native renderer.
  renderer?: Parameters<typeof useDaintreeController>[2];
}) {
  const controller = useDaintreeController(app, undefined, renderer);
  onController(controller);
  return null;
}

/** A minimal stand-in for the OpenTUI renderer that records the resync side effects. */
function makeStubRenderer() {
  const calls = {
    currentCleared: 0,
    nextCleared: 0,
    rendered: 0,
    splitReset: 0,
    forceFullRepaintRequested: false,
  };
  const renderer = {
    // Split-footer's scrollback-replay reset — dropped so a stale saved-line record
    // can't redraw the old conversation after the host-scrollback wipe.
    resetSplitFooterForReplay: () => {
      calls.splitReset += 1;
    },
    currentRenderBuffer: {
      clear: () => {
        calls.currentCleared += 1;
      },
    },
    nextRenderBuffer: {
      clear: () => {
        calls.nextCleared += 1;
      },
    },
    requestRender: () => {
      calls.rendered += 1;
    },
    get forceFullRepaintRequested() {
      return calls.forceFullRepaintRequested;
    },
    set forceFullRepaintRequested(v: boolean) {
      calls.forceFullRepaintRequested = v;
    },
  };
  return { renderer: renderer as unknown as Parameters<
    typeof useDaintreeController
  >[2], calls };
}

function makeOfflineApp() {
  const stateDir = fs.mkdtempSync(path.join(os.tmpdir(), "dt-clr-"));
  const app = App.create({
    overrides: {
      offline: true,
      stateDir,
      projectPath: stateDir,
      tier: "operator",
    },
  });
  return { app, stateDir };
}

/**
 * Capture writes to the real process.stdout. The controller now writes the
 * scrollback-wipe escape straight to `process.stdout` (no Ink-managed
 * `useStdout()` stub anymore), so we spy on the real stream and restore both the
 * original `write` and `isTTY` after each test. `clearHostTerminal` only emits on
 * a TTY, so the TTY gate is driven by toggling `process.stdout.isTTY`.
 */
let writes: string[] = [];
let origWrite: typeof process.stdout.write;
let origIsTTY: boolean | undefined;

beforeEach(() => {
  writes = [];
  origWrite = process.stdout.write.bind(process.stdout);
  origIsTTY = process.stdout.isTTY;
  // Tee every stdout write into our buffer; swallow the actual emission so the
  // wipe escape doesn't clobber the test runner's own terminal. Return true to
  // satisfy the WriteStream contract.
  (process.stdout as unknown as { write: unknown }).write = ((
    chunk: unknown,
  ) => {
    writes.push(String(chunk));
    return true;
  }) as typeof process.stdout.write;
});

afterEach(() => {
  (process.stdout as unknown as { write: typeof process.stdout.write }).write =
    origWrite;
  (process.stdout as unknown as { isTTY: boolean | undefined }).isTTY =
    origIsTTY;
});

const written = () => writes.join("");

describe("useDaintreeController /clear wipes host scrollback (#137)", () => {
  test("emits the host-terminal clear sequence and resets the transcript", async () => {
    const { app, stateDir } = makeOfflineApp();
    // Force a TTY so the clear gate lets the escape through (same intent as the
    // old ink-testing-library stub override).
    (process.stdout as unknown as { isTTY: boolean }).isTTY = true;

    let controller!: DaintreeController;
    const t = await testRender(
      <Harness app={app} onController={(c) => (controller = c)} />,
      { width: 80, height: 24 },
    );
    await t.flush();
    await tick();

    const before = writes.length;
    let accepted = false;
    await act(async () => {
      accepted = controller.sendUserMessage("/clear");
    });
    expect(accepted).toBe(true);
    await tick();

    // The scrollback wipe reached stdout...
    expect(writes.slice(before).join("")).toContain(HOST_TERMINAL_CLEAR);
    // ...and the transcript was reset to just the single confirmation card.
    expect(controller.transcript).toHaveLength(1);

    // A second /clear emits the escape again — the wipe is not accidentally de-duped.
    const before2 = writes.length;
    await act(async () => {
      controller.sendUserMessage("/clear");
    });
    await tick();
    expect(writes.slice(before2).join("")).toContain(HOST_TERMINAL_CLEAR);

    t.renderer.destroy?.();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });

  test("forces a full OpenTUI repaint after the clear commits", async () => {
    const { app, stateDir } = makeOfflineApp();
    (process.stdout as unknown as { isTTY: boolean }).isTTY = true;
    const { renderer, calls } = makeStubRenderer();

    let controller!: DaintreeController;
    const t = await testRender(
      <Harness
        app={app}
        renderer={renderer}
        onController={(c) => (controller = c)}
      />,
      { width: 80, height: 24 },
    );
    await t.flush();
    await tick();

    await act(async () => {
      controller.sendUserMessage("/clear");
    });
    await tick();

    // The resync ran post-commit: split-footer's saved scrollback record was reset,
    // both shadow buffers blanked, the forced-repaint latch set, and a render
    // requested — the fix for the blank-header gap.
    expect(calls.splitReset).toBeGreaterThan(0);
    expect(calls.currentCleared).toBeGreaterThan(0);
    expect(calls.nextCleared).toBeGreaterThan(0);
    expect(calls.forceFullRepaintRequested).toBe(true);
    expect(calls.rendered).toBeGreaterThan(0);

    t.renderer.destroy?.();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });

  test("requestRedraw resets the split-footer surface + forces a repaint, but keeps the transcript", async () => {
    const { app, stateDir } = makeOfflineApp();
    (process.stdout as unknown as { isTTY: boolean }).isTTY = true;
    const { renderer, calls } = makeStubRenderer();

    let controller!: DaintreeController;
    const t = await testRender(
      <Harness
        app={app}
        renderer={renderer}
        onController={(c) => (controller = c)}
      />,
      { width: 80, height: 24 },
    );
    await t.flush();
    await tick();

    // Seed a transcript cell (a log note) so we can prove the redraw does NOT clear it
    // (unlike /clear). Going through the bridge keeps this offline-deterministic.
    await act(async () => {
      controller.bridge.emit({
        type: "log",
        level: "info",
        message: "seed-line",
      });
    });
    await tick();
    const lenBefore = controller.transcript.length;
    const hasSeed = () =>
      controller.transcript.some(
        (c) => c.kind === "note" && c.text === "seed-line",
      );
    expect(lenBefore).toBeGreaterThan(0);
    expect(hasSeed()).toBe(true);

    const before = writes.length;
    await act(async () => {
      controller.requestRedraw();
    });
    await tick();

    // The renderer-owned reset ran: split replay record reset, both shadow buffers
    // blanked, the forced-repaint latch set, a render requested. resetSplitFooterForReplay
    // itself erases viewport + scrollback (in the real renderer), so the resize path does
    // NOT emit the raw clearHostTerminal escape when a renderer is present — asserting that
    // here guards the "no extra blank line above the header" fix.
    expect(calls.splitReset).toBeGreaterThan(0);
    expect(calls.currentCleared).toBeGreaterThan(0);
    expect(calls.nextCleared).toBeGreaterThan(0);
    expect(calls.forceFullRepaintRequested).toBe(true);
    expect(calls.rendered).toBeGreaterThan(0);
    expect(writes.slice(before).join("")).not.toContain(HOST_TERMINAL_CLEAR);
    // ...and the transcript is untouched — a resize re-commits the SAME cells (proven by
    // the seed note surviving), it doesn't clear the conversation.
    expect(controller.transcript).toHaveLength(lenBefore);
    expect(hasSeed()).toBe(true);

    t.renderer.destroy?.();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });

  test("requestRedraw falls back to the raw host-clear escape when no renderer is available", async () => {
    const { app, stateDir } = makeOfflineApp();
    (process.stdout as unknown as { isTTY: boolean }).isTTY = true;

    // No renderer passed: the resize path can't use resetSplitFooterForReplay, so it must
    // fall back to the raw clearHostTerminal wipe so scrollback is still cleared.
    let controller!: DaintreeController;
    const t = await testRender(
      <Harness app={app} onController={(c) => (controller = c)} />,
      { width: 80, height: 24 },
    );
    await t.flush();
    await tick();

    const before = writes.length;
    await act(async () => {
      controller.requestRedraw();
    });
    await tick();

    expect(writes.slice(before).join("")).toContain(HOST_TERMINAL_CLEAR);

    t.renderer.destroy?.();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });

  test("writes no clear escape when stdout is not a TTY", async () => {
    const { app, stateDir } = makeOfflineApp();
    // Leave isTTY false → the TTY gate suppresses the escape (piped stdout).
    (process.stdout as unknown as { isTTY: boolean }).isTTY = false;

    let controller!: DaintreeController;
    const t = await testRender(
      <Harness app={app} onController={(c) => (controller = c)} />,
      { width: 80, height: 24 },
    );
    await t.flush();
    await tick();

    const before = writes.length;
    await act(async () => {
      controller.sendUserMessage("/clear");
    });
    await tick();

    expect(writes.slice(before).join("")).not.toContain(HOST_TERMINAL_CLEAR);
    // The logical clear still happened regardless of the TTY gate.
    expect(controller.transcript).toHaveLength(1);

    t.renderer.destroy?.();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });
});
