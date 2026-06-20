/**
 * Issue #141: the host `interrupt` command must actually abort the in-flight
 * turn, not just suppress its display output.
 *
 * `src/host/index.ts` runs `void main()` (and boots the full App) at import time,
 * so it can't be imported and driven directly in a unit test. Instead we mirror
 * the exact per-turn-controller wiring from `main()`'s `handleCommand` closure
 * (the `prompt` and `interrupt` cases) and drive it with a *cooperative* fake
 * session — one that genuinely watches `signal.aborted` and resolves with the
 * cancel sentinel, exactly as the real AgentSession.send() does. That proves the
 * wiring's end-to-end effect (interrupt -> send returns promptly -> busy clears),
 * not merely that abort() was called. Keep this in sync with src/host/index.ts.
 */

const CANCELLED_REPLY = "Turn cancelled";

/**
 * A fake `session.send` that mirrors AgentSession's cooperative-cancel contract:
 * it stays pending until either the test explicitly finishes it OR the signal
 * aborts, in which case it resolves with the cancel sentinel (never throws).
 */
function makeCooperativeSession() {
  let finishCurrent: (() => void) | null = null;
  const calls: Array<{ text: string; signal?: AbortSignal }> = [];
  const session = {
    calls,
    send(text: string, opts?: { readOnly?: boolean; signal?: AbortSignal }): Promise<string> {
      calls.push({ text, signal: opts?.signal });
      return new Promise<string>((resolve) => {
        const onAbort = () => resolve(CANCELLED_REPLY);
        if (opts?.signal?.aborted) {
          onAbort();
          return;
        }
        opts?.signal?.addEventListener("abort", onAbort, { once: true });
        // Lets a test resolve a turn normally (no abort) to model completion.
        finishCurrent = () => resolve("done");
      });
    },
    finish() {
      finishCurrent?.();
      finishCurrent = null;
    },
  };
  return session;
}

/**
 * Minimal harness mirroring the prompt/interrupt wiring in src/host/index.ts.
 * `busy` + `turnController` live in the closure exactly as they do in `main()`.
 */
function makeHarness() {
  const session = makeCooperativeSession();
  let busy = false;
  let turnController: AbortController | null = null;
  let bridgeInterrupts = 0;
  let lastTurnPromise: Promise<void> | null = null;

  const prompt = (text: string): { rejected: boolean; done: Promise<void> } => {
    if (busy) return { rejected: true, done: Promise.resolve() };
    busy = true;
    const controller = new AbortController();
    turnController = controller;
    const done = (async () => {
      try {
        await session.send(text, { signal: controller.signal });
      } finally {
        if (turnController === controller) turnController = null;
        busy = false;
      }
    })();
    lastTurnPromise = done;
    return { rejected: false, done };
  };

  const interrupt = (): void => {
    turnController?.abort();
    bridgeInterrupts += 1; // stand-in for bridge.interrupt()
  };

  return {
    session,
    prompt,
    interrupt,
    finish: () => session.finish(),
    get busy() {
      return busy;
    },
    get controller() {
      return turnController;
    },
    get bridgeInterrupts() {
      return bridgeInterrupts;
    },
    get lastTurnPromise() {
      return lastTurnPromise;
    },
  };
}

describe("host interrupt wiring (issue #141)", () => {
  it("passes a non-aborted signal to send() on a fresh prompt", () => {
    const h = makeHarness();
    h.prompt("hello");
    expect(h.busy).toBe(true);
    const call = h.session.calls.at(-1)!;
    expect(call.signal).toBeInstanceOf(AbortSignal);
    expect(call.signal!.aborted).toBe(false);
  });

  it("interrupt aborts the in-flight turn so send() returns and busy clears", async () => {
    const h = makeHarness();
    const { done } = h.prompt("long task");
    expect(h.busy).toBe(true);

    h.interrupt();

    // send() resolves via the abort listener (cancel sentinel), and the finally
    // frees busy + nulls the controller — the turn does not run to completion.
    await done;
    expect(h.busy).toBe(false);
    expect(h.controller).toBeNull();
    expect(h.bridgeInterrupts).toBe(1); // display suppression still invoked
  });

  it("calls turnController.abort() before bridge.interrupt()", async () => {
    const h = makeHarness();
    const { done } = h.prompt("task");
    const signal = h.session.calls.at(-1)!.signal!;
    h.interrupt();
    expect(signal.aborted).toBe(true);
    expect(h.bridgeInterrupts).toBe(1);
    await done;
  });

  it("a second prompt after interrupt gets a fresh, non-aborted controller", async () => {
    const h = makeHarness();
    const first = h.prompt("first");
    const firstSignal = h.session.calls.at(-1)!.signal!;
    h.interrupt();
    await first.done;
    expect(firstSignal.aborted).toBe(true);

    const second = h.prompt("second");
    expect(second.rejected).toBe(false);
    const secondSignal = h.session.calls.at(-1)!.signal!;
    expect(secondSignal).not.toBe(firstSignal);
    expect(secondSignal.aborted).toBe(false);
    h.finish();
    await second.done;
    expect(h.busy).toBe(false);
  });

  it("interrupt before any prompt is a safe no-op (no controller)", () => {
    const h = makeHarness();
    expect(h.controller).toBeNull();
    expect(() => h.interrupt()).not.toThrow();
    expect(h.bridgeInterrupts).toBe(1);
  });

  it("repeated interrupts on the same turn are idempotent", async () => {
    const h = makeHarness();
    const { done } = h.prompt("task");
    h.interrupt();
    h.interrupt();
    await done;
    expect(h.busy).toBe(false);
    expect(h.bridgeInterrupts).toBe(2);
  });

  it("a normally-completed turn clears its controller via the identity guard", async () => {
    const h = makeHarness();
    const { done } = h.prompt("task");
    expect(h.controller).not.toBeNull();
    h.finish();
    await done;
    expect(h.busy).toBe(false);
    expect(h.controller).toBeNull();
  });

  it("rejects a second prompt while a turn is busy", () => {
    const h = makeHarness();
    h.prompt("first");
    const second = h.prompt("second");
    expect(second.rejected).toBe(true);
    expect(h.session.calls).toHaveLength(1);
  });
});
