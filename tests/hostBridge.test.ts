import { HostBridge, redactArgs, resultToAudit } from "../src/host/bridge.js";
import type { HostEvent } from "../src/host/protocol.js";
import type { ToolResult } from "../src/schemas.js";

function makeBridge(opts: Partial<{ riskOf: (n: string) => any; approvalTimeoutMs: number }> = {}) {
  const events: HostEvent[] = [];
  let clock = 1000;
  const bridge = new HostBridge({
    sessionId: "ses_test",
    post: (e) => events.push(e),
    riskOf: opts.riskOf,
    now: () => clock,
    approvalTimeoutMs: opts.approvalTimeoutMs ?? 0,
  });
  return { events, bridge, tick: (ms: number) => (clock += ms) };
}

const types = (events: HostEvent[]) => events.map((e) => e.type);

describe("HostBridge turn lifecycle", () => {
  it("brackets a user exchange with an instantaneous user turn", () => {
    const { events, bridge } = makeBridge();
    bridge.startExchange();
    expect(types(events)).toEqual(["turn:start", "turn:end"]);
    const [start, end] = events as [Extract<HostEvent, { type: "turn:start" }>, any];
    expect(start.role).toBe("user");
    expect(start.turnId).toBe(end.turnId);
    expect(start.startedAt).toBe(end.endedAt);
  });

  it("collapses multiple assistantStart calls in one send() into a single assistant turn", () => {
    const { events, bridge } = makeBridge();
    bridge.startExchange();
    events.length = 0;
    bridge.sink.assistantStart();
    bridge.sink.assistantToken("hel");
    bridge.sink.assistantStart(); // second model iteration — must not open a new turn
    bridge.sink.assistantToken("lo");
    bridge.sink.assistantEnd("hello");
    expect(types(events)).toEqual(["turn:start", "turn:token", "turn:token", "turn:end"]);
    const starts = events.filter((e) => e.type === "turn:start");
    expect(starts).toHaveLength(1);
    expect(starts[0]).toMatchObject({ role: "assistant" });
    const end = events.find((e) => e.type === "turn:end") as Extract<HostEvent, { type: "turn:end" }>;
    expect(end.outcome).toBe("answered");
  });

  it("marks an empty final response as unknown outcome", () => {
    const { events, bridge } = makeBridge();
    bridge.sink.assistantStart();
    bridge.sink.assistantEnd("   ");
    const end = events.find((e) => e.type === "turn:end") as Extract<HostEvent, { type: "turn:end" }>;
    expect(end.outcome).toBe("unknown");
  });

  it("settleTurn closes a turn the loop left dangling, but no-ops once closed", () => {
    const { events, bridge } = makeBridge();
    bridge.sink.assistantStart();
    bridge.settleTurn("answered");
    expect(types(events)).toEqual(["turn:start", "turn:end"]);
    bridge.settleTurn("answered"); // already closed
    expect(types(events)).toEqual(["turn:start", "turn:end"]);
  });

  it("emits host:error and closes the turn on a loop error", () => {
    const { events, bridge } = makeBridge();
    bridge.sink.assistantStart();
    events.length = 0;
    bridge.sink.error("model exploded");
    expect(types(events)).toEqual(["host:error", "turn:end"]);
  });
});

describe("HostBridge tool events", () => {
  it("emits tool:started/settled with danger from risk and a real duration", () => {
    const { events, bridge, tick } = makeBridge({ riskOf: (n) => (n === "git.commit" ? "git" : "read") });
    bridge.sink.assistantStart();
    events.length = 0;
    bridge.sink.toolCall({ id: "tc1", name: "git.commit", args: { message: "x" }, startedAt: 1000 });
    tick(42);
    bridge.sink.toolResult({ id: "tc1", name: "git.commit", result: { ok: true, summary: "done" }, endedAt: 1042 });
    const started = events.find((e) => e.type === "tool:started") as Extract<HostEvent, { type: "tool:started" }>;
    const settled = events.find((e) => e.type === "tool:settled") as Extract<HostEvent, { type: "tool:settled" }>;
    expect(started.danger).toBe(true);
    expect(started.toolCallId).toBe("tc1");
    expect(settled.durationMs).toBe(42);
    expect(settled.result).toBe("success");
    expect(settled.severity).toBe("info");
  });

  it("treats read-only tools as non-danger", () => {
    const { events, bridge } = makeBridge({ riskOf: () => "read" });
    bridge.sink.toolCall({ id: "tc2", name: "fs.read", args: {}, startedAt: 1 });
    const started = events.find((e) => e.type === "tool:started") as Extract<HostEvent, { type: "tool:started" }>;
    expect(started.danger).toBe(false);
  });
});

describe("HostBridge approvals", () => {
  it("emits approval:requested and resolves true on approval", async () => {
    const { events, bridge } = makeBridge();
    const decision = bridge.confirm({ toolName: "git.push", summary: "push to main" });
    const req = events.find((e) => e.type === "approval:requested") as Extract<
      HostEvent,
      { type: "approval:requested" }
    >;
    expect(req.toolId).toBe("git.push");
    bridge.resolveApproval(req.approvalId, "approved");
    await expect(decision).resolves.toBe(true);
    expect(types(events)).toContain("approval:decided");
  });

  it("resolves false on rejection and on timeout-drain", async () => {
    const { bridge } = makeBridge();
    const rejected = bridge.confirm({ toolName: "x", summary: "y" });
    const req2 = bridge.confirm({ toolName: "a", summary: "b" });
    // settlePendingApprovals rejects everything outstanding (shutdown drain).
    bridge.settlePendingApprovals("rejected");
    await expect(rejected).resolves.toBe(false);
    await expect(req2).resolves.toBe(false);
  });
});

describe("HostBridge interrupt", () => {
  it("closes the active turn and suppresses further tokens", () => {
    const { events, bridge } = makeBridge();
    bridge.sink.assistantStart();
    bridge.sink.assistantToken("partial");
    bridge.interrupt();
    events.length = 0;
    bridge.sink.assistantToken("dropped");
    bridge.sink.assistantStart();
    expect(events).toHaveLength(0);
  });
});

describe("resultToAudit", () => {
  const cases: Array<[ToolResult, string, string]> = [
    [{ ok: true, summary: "" }, "success", "info"],
    [{ ok: false, summary: "", error: { code: "CONFIRMATION_REQUIRED", message: "", recoverable: true } }, "confirmation-pending", "notice"],
    [{ ok: false, summary: "", error: { code: "UNAUTHORIZED", message: "", recoverable: true } }, "unauthorized", "warning"],
    [{ ok: false, summary: "", error: { code: "RATE_LIMITED", message: "", recoverable: true } }, "rate_limited", "warning"],
    [{ ok: false, summary: "", error: { code: "SOMETHING_ELSE", message: "", recoverable: true } }, "error", "error"],
  ];
  it.each(cases)("maps %o", (res, result, severity) => {
    const audit = resultToAudit(res);
    expect(audit.result).toBe(result);
    expect(audit.severity).toBe(severity);
  });
});

describe("redactArgs", () => {
  it("collapses long strings and nested values, keeping short scalars", () => {
    const long = "z".repeat(200);
    const out = redactArgs({ short: "ok", long, nested: { a: 1 }, list: [1, 2] });
    expect(out).toContain('"short":"ok"');
    expect(out).toContain("<string: 200 chars>");
    expect(out).toContain("<object>");
    expect(out).toContain("<array>");
  });

  it("redacts a bare long string and handles nullish", () => {
    expect(redactArgs("a".repeat(100))).toBe("<string: 100 chars>");
    expect(redactArgs(null)).toBe("");
    expect(redactArgs(undefined)).toBe("");
  });
});
