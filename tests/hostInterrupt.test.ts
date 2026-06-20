/**
 * Issue #141: the host `interrupt` command must actually abort the in-flight
 * turn, not just suppress its display output.
 *
 * `src/host/index.ts` runs `void main()` (and boots the full App) at import time,
 * so it can't be imported and driven directly in a unit test. Instead we mirror
 * the exact per-turn-controller wiring from `main()`'s `handleCommand` closure
 * (the `prompt` and `interrupt` cases) and drive it with a *cooperative* fake
 * session — one that genuinely watches `signal.aborted`, models a tool that
 * parks awaiting approval, and resolves with the cancel sentinel exactly as the
 * real AgentSession.send() does. That proves the wiring's end-to-end effect
 * (interrupt -> send returns -> busy clears), including the approval-deadlock
 * path, not merely that abort() was called. Keep in sync with src/host/index.ts.
 */

const CANCELLED_REPLY = "Turn cancelled";

/** Minimal stand-in for HostBridge's approval bookkeeping (confirm / settle). */
function makeFakeBridge() {
  const pending = new Map<string, (approved: boolean) => void>();
  let nextId = 0;
  let interrupts = 0;
  return {
    get interrupts() {
      return interrupts;
    },
    /** Mirrors bridge.confirm(): a promise that only settles on a decision. */
    confirm(): Promise<boolean> {
      const id = `apr_${nextId++}`;
      return new Promise<boolean>((resolve) => pending.set(id, resolve));
    },
    /** Mirrors bridge.settlePendingApprovals("rejected"). */
    settlePendingApprovals(): void {
      for (const id of [...pending.keys()]) {
        pending.get(id)!(false);
        pending.delete(id);
      }
    },
    interrupt(): void {
      interrupts += 1;
    },
    get pendingCount() {
      return pending.size;
    },
  };
}

/**
 * A fake `session.send` mirroring AgentSession's cooperative-cancel contract: it
 * stays pending until the test finishes it OR the signal aborts (resolving with
 * the cancel sentinel — never throws). When `needsApproval` is set it first
 * awaits `bridge.confirm()`; a declined/settled approval then falls through to
 * the post-dispatch signal check, exactly like registry.dispatch -> loop.ts.
 */
function makeCooperativeSession(bridge: ReturnType<typeof makeFakeBridge>) {
  let finishCurrent: (() => void) | null = null;
  const calls: Array<{ text: string; signal?: AbortSignal; needsApproval: boolean }> = [];
  const session = {
    calls,
    send(
      text: string,
      opts?: { readOnly?: boolean; signal?: AbortSignal; needsApproval?: boolean },
    ): Promise<string> {
      const needsApproval = opts?.needsApproval ?? false;
      calls.push({ text, signal: opts?.signal, needsApproval });
      const signal = opts?.signal;
      return (async () => {
        if (signal?.aborted) return CANCELLED_REPLY;
        if (needsApproval) {
          // Park awaiting approval, like a mutating tool in registry.dispatch.
          await bridge.confirm();
          // Approval settled (declined on interrupt) — loop re-checks the signal.
          if (signal?.aborted) return CANCELLED_REPLY;
        }
        return new Promise<string>((resolve) => {
          if (signal?.aborted) {
            resolve(CANCELLED_REPLY);
            return;
          }
          signal?.addEventListener("abort", () => resolve(CANCELLED_REPLY), { once: true });
          finishCurrent = () => resolve("done");
        });
      })();
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
  const bridge = makeFakeBridge();
  const session = makeCooperativeSession(bridge);
  let busy = false;
  let turnController: AbortController | null = null;

  const prompt = (
    text: string,
    opts: { needsApproval?: boolean } = {},
  ): { rejected: boolean; done: Promise<void> } => {
    if (busy) return { rejected: true, done: Promise.resolve() };
    busy = true;
    const controller = new AbortController();
    turnController = controller;
    const done = (async () => {
      try {
        await session.send(text, { signal: controller.signal, needsApproval: opts.needsApproval });
      } finally {
        if (turnController === controller) turnController = null;
        busy = false;
      }
    })();
    return { rejected: false, done };
  };

  // Mirrors src/host/index.ts `case "interrupt"` exactly.
  const interrupt = (): void => {
    turnController?.abort();
    bridge.settlePendingApprovals();
    bridge.interrupt();
  };

  return {
    session,
    bridge,
    prompt,
    interrupt,
    finish: () => session.finish(),
    get busy() {
      return busy;
    },
    get controller() {
      return turnController;
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

    await done;
    expect(h.busy).toBe(false);
    expect(h.controller).toBeNull();
    expect(h.bridge.interrupts).toBe(1); // display suppression still invoked
  });

  it("interrupt unblocks a turn parked awaiting tool approval (the #141 deadlock)", async () => {
    const h = makeHarness();
    const { done } = h.prompt("delete files", { needsApproval: true });
    // Let send() reach the confirm() await.
    await Promise.resolve();
    await Promise.resolve();
    expect(h.bridge.pendingCount).toBe(1);
    expect(h.busy).toBe(true);

    h.interrupt();

    // settlePendingApprovals declines the confirm, the post-dispatch signal
    // check sees the abort, send() returns the sentinel, and busy clears.
    await done;
    expect(h.busy).toBe(false);
    expect(h.controller).toBeNull();
    expect(h.bridge.pendingCount).toBe(0);
  });

  it("calls abort() and settles approvals before bridge.interrupt()", async () => {
    const h = makeHarness();
    const { done } = h.prompt("task");
    const signal = h.session.calls.at(-1)!.signal!;
    h.interrupt();
    expect(signal.aborted).toBe(true);
    expect(h.bridge.interrupts).toBe(1);
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
    expect(h.bridge.interrupts).toBe(1);
  });

  it("repeated interrupts on the same turn are idempotent", async () => {
    const h = makeHarness();
    const { done } = h.prompt("task");
    h.interrupt();
    h.interrupt();
    await done;
    expect(h.busy).toBe(false);
    expect(h.bridge.interrupts).toBe(2);
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

  it("the identity guard keeps a next turn's controller alive after the prior finally", async () => {
    const h = makeHarness();
    const first = h.prompt("first");
    h.interrupt();
    await first.done; // first turn's finally runs here
    const second = h.prompt("second");
    expect(h.controller).not.toBeNull(); // not nulled by the stale first finally
    expect(h.session.calls.at(-1)!.signal!.aborted).toBe(false);
    h.finish();
    await second.done;
  });

  it("rejects a second prompt while a turn is busy", () => {
    const h = makeHarness();
    h.prompt("first");
    const second = h.prompt("second");
    expect(second.rejected).toBe(true);
    expect(h.session.calls).toHaveLength(1);
  });
});
