import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { vi } from "vitest";
import { render } from "ink-testing-library";
import { App } from "../../src/cli/app.js";
import {
  useDaintreeController,
  type DaintreeController,
} from "../../src/ui/hooks/useDaintreeController.js";

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
  const stateDir = fs.mkdtempSync(path.join(os.tmpdir(), "dt-ctl-"));
  const app = App.create({
    overrides: { offline: true, stateDir, projectPath: stateDir, tier: "operator" },
  });
  return { app, stateDir };
}

describe("useDaintreeController queue + cancel (#45)", () => {
  it("queues a follow-up while busy and drains it in order once the turn ends", async () => {
    const { app, stateDir } = makeOfflineApp();
    const calls: Array<{ input: string; signal?: AbortSignal }> = [];
    let resolveCurrent: (() => void) | undefined;
    // Replace the real streaming turn with a deferred we control, so we can hold a
    // turn "in flight" and observe what the second submit does.
    (app.session as unknown as { send: unknown }).send = vi.fn(
      (input: string, opts: { signal?: AbortSignal } = {}) => {
        calls.push({ input, signal: opts.signal });
        return new Promise<string>((res) => {
          resolveCurrent = () => res("done");
        });
      },
    );

    let controller!: DaintreeController;
    const { unmount } = render(
      <Harness app={app} onController={(c) => (controller = c)} />,
    );
    await tick();

    // First message starts a turn.
    expect(controller.sendUserMessage("first")).toBe(true);
    await tick();
    expect(calls.map((c) => c.input)).toEqual(["first"]);

    // Second message arrives while busy → accepted (true) and queued, NOT sent yet.
    expect(controller.sendUserMessage("second")).toBe(true);
    await tick();
    expect(calls.map((c) => c.input)).toEqual(["first"]);

    // Finishing the first turn drains the queued follow-up automatically.
    resolveCurrent?.();
    await tick();
    expect(calls.map((c) => c.input)).toEqual(["first", "second"]);
    // Each turn gets its own abort signal.
    expect(calls[0].signal).toBeInstanceOf(AbortSignal);
    expect(calls[1].signal).toBeInstanceOf(AbortSignal);
    expect(calls[1].signal).not.toBe(calls[0].signal);

    unmount();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });

  it("cancelTurn aborts the in-flight turn's signal", async () => {
    const { app, stateDir } = makeOfflineApp();
    let captured: AbortSignal | undefined;
    (app.session as unknown as { send: unknown }).send = vi.fn(
      (_input: string, opts: { signal?: AbortSignal } = {}) => {
        captured = opts.signal;
        return new Promise<string>(() => {}); // never resolves on its own
      },
    );

    let controller!: DaintreeController;
    const { unmount } = render(
      <Harness app={app} onController={(c) => (controller = c)} />,
    );
    await tick();

    controller.sendUserMessage("long running");
    await tick();
    expect(captured?.aborted).toBe(false);

    controller.cancelTurn();
    await tick();
    expect(captured?.aborted).toBe(true);

    unmount();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });
});
