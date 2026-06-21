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
}: {
  app: App;
  onController: (c: DaintreeController) => void;
}) {
  const controller = useDaintreeController(app);
  onController(controller);
  return null;
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
