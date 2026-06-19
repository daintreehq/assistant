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

/**
 * Replace the streaming turn with a deferred we control: each send() records its
 * input + signal and parks until we resolve it (or until its signal aborts, which
 * resolves it with the cancelled sentinel — mirroring the real loop).
 */
function deferredSession(app: App) {
  const calls: Array<{ input: string; signal?: AbortSignal }> = [];
  const resolvers: Array<() => void> = [];
  (app.session as unknown as { send: unknown }).send = vi.fn(
    (input: string, opts: { signal?: AbortSignal } = {}) => {
      calls.push({ input, signal: opts.signal });
      return new Promise<string>((res) => {
        resolvers.push(() => res("done"));
        opts.signal?.addEventListener("abort", () => res("Turn cancelled"));
      });
    },
  );
  return {
    calls,
    /** Resolve the i-th started turn. */
    finish: (i: number) => resolvers[i]?.(),
  };
}

describe("useDaintreeController queue + cancel (#45)", () => {
  it("queues a follow-up while busy and drains it in order once the turn ends", async () => {
    const { app, stateDir } = makeOfflineApp();
    const { calls, finish } = deferredSession(app);

    let controller!: DaintreeController;
    const { unmount } = render(
      <Harness app={app} onController={(c) => (controller = c)} />,
    );
    await tick();

    expect(controller.sendUserMessage("first")).toBe(true);
    await tick();
    expect(calls.map((c) => c.input)).toEqual(["first"]);

    // Arrives while busy → accepted (true) and queued, NOT sent yet.
    expect(controller.sendUserMessage("second")).toBe(true);
    await tick();
    expect(calls.map((c) => c.input)).toEqual(["first"]);

    finish(0);
    await tick();
    expect(calls.map((c) => c.input)).toEqual(["first", "second"]);
    expect(calls[0].signal).toBeInstanceOf(AbortSignal);
    expect(calls[1].signal).toBeInstanceOf(AbortSignal);
    expect(calls[1].signal).not.toBe(calls[0].signal);

    unmount();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });

  it("drains a strictly FIFO three-deep queue, each with a distinct signal", async () => {
    const { app, stateDir } = makeOfflineApp();
    const { calls, finish } = deferredSession(app);

    let controller!: DaintreeController;
    const { unmount } = render(
      <Harness app={app} onController={(c) => (controller = c)} />,
    );
    await tick();

    controller.sendUserMessage("first");
    await tick();
    controller.sendUserMessage("second");
    controller.sendUserMessage("third");
    await tick();
    // Only the first is running; the rest are queued.
    expect(calls.map((c) => c.input)).toEqual(["first"]);

    finish(0);
    await tick();
    expect(calls.map((c) => c.input)).toEqual(["first", "second"]);
    finish(1);
    await tick();
    expect(calls.map((c) => c.input)).toEqual(["first", "second", "third"]);

    // Every turn ran on its own controller.
    const signals = calls.map((c) => c.signal);
    expect(new Set(signals).size).toBe(3);

    unmount();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });

  it("cancelTurn aborts the in-flight turn's signal", async () => {
    const { app, stateDir } = makeOfflineApp();
    const { calls } = deferredSession(app);

    let controller!: DaintreeController;
    const { unmount } = render(
      <Harness app={app} onController={(c) => (controller = c)} />,
    );
    await tick();

    controller.sendUserMessage("long running");
    await tick();
    expect(calls[0].signal?.aborted).toBe(false);
    expect(controller.canCancel).toBe(true);

    controller.cancelTurn();
    await tick();
    expect(calls[0].signal?.aborted).toBe(true);

    unmount();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });

  it("after cancelling the in-flight turn, a queued follow-up still drains", async () => {
    const { app, stateDir } = makeOfflineApp();
    const { calls } = deferredSession(app);

    let controller!: DaintreeController;
    const { unmount } = render(
      <Harness app={app} onController={(c) => (controller = c)} />,
    );
    await tick();

    controller.sendUserMessage("first");
    await tick();
    controller.sendUserMessage("queued");
    await tick();
    expect(calls.map((c) => c.input)).toEqual(["first"]);

    // Cancel the in-flight turn — its send() resolves via the abort listener, the
    // finally runs, and the queued follow-up drains with a fresh, un-aborted signal.
    controller.cancelTurn();
    await tick();
    expect(calls.map((c) => c.input)).toEqual(["first", "queued"]);
    expect(calls[1].signal?.aborted).toBe(false);

    unmount();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });
});

const onlyTurns = (c: DaintreeController) =>
  c.transcript.filter((cell) => cell.kind === "turn");

describe("useDaintreeController pull-back (#61)", () => {
  it("pulls a pre-stream message back: removes the turn, aborts, clears canCancel", async () => {
    const { app, stateDir } = makeOfflineApp();
    const { calls } = deferredSession(app);

    let controller!: DaintreeController;
    const { unmount } = render(
      <Harness app={app} onController={(c) => (controller = c)} />,
    );
    await tick();

    controller.sendUserMessage("hello");
    await tick();
    // A just-sent, pre-stream turn exists and is cancellable.
    expect(onlyTurns(controller)).toHaveLength(1);
    expect(controller.canCancel).toBe(true);
    expect(calls[0].signal?.aborted).toBe(false);

    controller.pullBackTurn();
    await tick();
    // The turn is gone, the request was aborted, and the composer is idle again.
    expect(onlyTurns(controller)).toHaveLength(0);
    expect(calls[0].signal?.aborted).toBe(true);
    expect(controller.canCancel).toBe(false);

    unmount();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });

  it("clears queued follow-ups on pull-back so none drains while editing", async () => {
    const { app, stateDir } = makeOfflineApp();
    const { calls } = deferredSession(app);

    let controller!: DaintreeController;
    const { unmount } = render(
      <Harness app={app} onController={(c) => (controller = c)} />,
    );
    await tick();

    controller.sendUserMessage("first");
    await tick();
    controller.sendUserMessage("queued"); // typed while busy → queued
    await tick();
    expect(calls.map((c) => c.input)).toEqual(["first"]);

    controller.pullBackTurn();
    await tick();
    // The abort resolves "first"; the queue was cleared, so "queued" never starts —
    // the user is back to editing, not racing a drained follow-up.
    expect(calls.map((c) => c.input)).toEqual(["first"]);
    expect(onlyTurns(controller)).toHaveLength(0);

    unmount();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });

  it("falls back to plain cancel once the turn is streaming", async () => {
    const { app, stateDir } = makeOfflineApp();
    const { calls } = deferredSession(app);

    let controller!: DaintreeController;
    const { unmount } = render(
      <Harness app={app} onController={(c) => (controller = c)} />,
    );
    await tick();

    controller.sendUserMessage("hello");
    await tick();
    // Assistant output starts → the pull-back window closes.
    controller.bridge.emit({ type: "assistant:start" });
    await tick();

    controller.pullBackTurn();
    await tick();
    // Plain-cancel path: the request is aborted but the turn stays in the transcript
    // (it produced output, so it isn't silently erased).
    expect(calls[0].signal?.aborted).toBe(true);
    expect(onlyTurns(controller)).toHaveLength(1);

    unmount();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });
});
