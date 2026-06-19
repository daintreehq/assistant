import { describe, it, expect } from "vitest";
import { AgentSession, rehydrateSession, CLEAR_MARKER } from "../src/agent/loop.js";
import { RecipeRegistry } from "../src/recipes/registry.js";
import { Db } from "../src/storage/db.js";
import { ToolRegistry } from "../src/tools/registry.js";
import type { ModelRouter } from "../src/models/router.js";
import type { ToolContext } from "../src/tools/types.js";
import type { MainPromptContext } from "../src/models/prompts/runtimeContext.js";
import type { ConversationMessageRecord } from "../src/schemas.js";

/** Build a conversation row with sensible defaults for the fields under test. */
function rec(over: Partial<ConversationMessageRecord> & { seq: number }): ConversationMessageRecord {
  return {
    id: `msg_${over.seq}`,
    sessionId: "ses_x",
    role: "user",
    content: "",
    createdAt: 1,
    ...over,
  };
}

/** The three control rows every healthy session writes first (seq 0,1,2). */
function controlRows(): ConversationMessageRecord[] {
  return [
    rec({ seq: 0, role: "system", content: "You are Daintree Assistant" }),
    rec({ seq: 1, role: "system", content: "# Runtime context" }),
    rec({ seq: 2, role: "system", content: "# Loaded recipes" }),
  ];
}

const promptContext: MainPromptContext = {
  tier: "operator",
  projectPath: "/proj",
  mcpConnected: true,
  mcpStatusLine: "connected",
  largeModel: "L",
  smallModel: "S",
  schedulerActive: true,
};

function makeSession(
  db: Db,
  sessionId: string,
  extra: { restoredMessages?: ReturnType<typeof rehydrateSession> } = {},
) {
  const router = {
    json: async () => ({ recipeIds: [], confidence: 0, reason: "", taskType: "qa", keepExisting: false }),
    stream: async () => ({ content: "ok", reasoning: "", toolCalls: [], finishReason: "stop" }),
    chat: async () => ({ content: "SUMMARY", reasoning: "", toolCalls: [], finishReason: "stop" }),
  } as unknown as ModelRouter;
  const ctx = { db, actor: "main" } as unknown as ToolContext;
  const restore = extra.restoredMessages;
  return new AgentSession({
    router,
    registry: new ToolRegistry(),
    recipeRegistry: new RecipeRegistry(),
    ctx,
    promptContext,
    sessionId,
    restoredMessages: restore?.restoredMessages,
    initialSeq: restore?.initialSeq,
  });
}

describe("rehydrateSession (#77)", () => {
  it("returns undefined for an empty session (fresh start)", () => {
    expect(rehydrateSession([])).toBeUndefined();
  });

  it("returns undefined when seq values are not unique (corruption guard)", () => {
    // Fingerprint of the pre-fix bug: a second control triplet re-wrote seq 0,1,2.
    const rows = [...controlRows(), ...controlRows()];
    expect(rehydrateSession(rows)).toBeUndefined();
  });

  it("restores working history after the 3 control rows and seeds the next seq", () => {
    const rows = [
      ...controlRows(),
      rec({ seq: 3, role: "user", content: "[system event]\nhello" }),
      rec({ seq: 4, role: "assistant", content: "hi there" }),
    ];
    const out = rehydrateSession(rows)!;
    expect(out.initialSeq).toBe(5);
    expect(out.restoredMessages).toHaveLength(2);
    expect(out.restoredMessages[0]).toMatchObject({ role: "user", content: "[system event]\nhello" });
    expect(out.restoredMessages[1]).toMatchObject({ role: "assistant", content: "hi there" });
  });

  it("keeps only rows after the last compaction marker", () => {
    const rows = [
      ...controlRows(),
      rec({ seq: 3, role: "user", content: "[system event]\nold turn" }),
      rec({ seq: 4, role: "system", content: "[conversation compacted — earlier turns dropped from context]" }),
      rec({ seq: 5, role: "user", content: "[compacted summary of earlier conversation]\nSUMMARY" }),
      rec({ seq: 6, role: "assistant", content: "after compaction" }),
    ];
    const out = rehydrateSession(rows)!;
    expect(out.initialSeq).toBe(7);
    // The marker row and the pre-compaction turn are gone; the summary + later turn remain.
    expect(out.restoredMessages).toHaveLength(2);
    expect(out.restoredMessages[0].content).toContain("compacted summary");
    expect(out.restoredMessages[1].content).toBe("after compaction");
    expect(out.restoredMessages.some((m) => m.content?.includes("old turn"))).toBe(false);
  });

  it("rehydrates a complete tool-call exchange", () => {
    const rows = [
      ...controlRows(),
      rec({ seq: 3, role: "user", content: "do it" }),
      rec({
        seq: 4,
        role: "assistant",
        content: "",
        toolCallsJson: JSON.stringify([
          { id: "call_1", type: "function", function: { name: "fs.read", arguments: "{}" } },
        ]),
      }),
      rec({ seq: 5, role: "tool", content: "result", toolCallId: "call_1" }),
    ];
    const out = rehydrateSession(rows)!;
    expect(out.restoredMessages).toHaveLength(3);
    expect(out.restoredMessages[1].tool_calls).toHaveLength(1);
    expect(out.restoredMessages[1].content).toBeNull();
    expect(out.restoredMessages[2]).toMatchObject({ role: "tool", tool_call_id: "call_1" });
  });

  it("drops an orphaned trailing assistant tool-call turn (no matching result)", () => {
    const rows = [
      ...controlRows(),
      rec({ seq: 3, role: "user", content: "do it" }),
      rec({
        seq: 4,
        role: "assistant",
        content: "",
        toolCallsJson: JSON.stringify([
          { id: "call_1", type: "function", function: { name: "fs.read", arguments: "{}" } },
        ]),
      }),
      // shut down before the tool result was written
    ];
    const out = rehydrateSession(rows)!;
    // The incomplete assistant turn is trimmed; only the user turn survives.
    expect(out.restoredMessages).toHaveLength(1);
    expect(out.restoredMessages[0]).toMatchObject({ role: "user", content: "do it" });
    // seq still advances past the stored rows so new writes never collide.
    expect(out.initialSeq).toBe(5);
  });

  it("keeps a message whose tool-call JSON is malformed, dropping only the calls", () => {
    const rows = [
      ...controlRows(),
      rec({ seq: 3, role: "assistant", content: "text", toolCallsJson: "{not json" }),
    ];
    const out = rehydrateSession(rows)!;
    expect(out.restoredMessages).toHaveLength(1);
    expect(out.restoredMessages[0]).toMatchObject({ role: "assistant", content: "text" });
    expect(out.restoredMessages[0].tool_calls).toBeUndefined();
  });

  it("drops an orphan tool result left by a malformed parent tool-call row", () => {
    const rows = [
      ...controlRows(),
      // Parent assistant's tool-call JSON is corrupt, so its calls are lost…
      rec({ seq: 3, role: "assistant", content: "text", toolCallsJson: "{not json" }),
      // …leaving this tool result with no declared parent id (Fireworks rejects it).
      rec({ seq: 4, role: "tool", content: "result", toolCallId: "call_1" }),
    ];
    const out = rehydrateSession(rows)!;
    expect(out.restoredMessages).toHaveLength(1);
    expect(out.restoredMessages[0]).toMatchObject({ role: "assistant", content: "text" });
    expect(out.restoredMessages.some((m) => m.role === "tool")).toBe(false);
  });

  it("trims a whole multi-tool batch when only some results were persisted", () => {
    const rows = [
      ...controlRows(),
      rec({ seq: 3, role: "user", content: "do both" }),
      rec({
        seq: 4,
        role: "assistant",
        content: "",
        toolCallsJson: JSON.stringify([
          { id: "call_1", type: "function", function: { name: "fs.read", arguments: "{}" } },
          { id: "call_2", type: "function", function: { name: "fs.list", arguments: "{}" } },
        ]),
      }),
      rec({ seq: 5, role: "tool", content: "r1", toolCallId: "call_1" }),
      // call_2 result never written — the assistant turn is incomplete.
    ];
    const out = rehydrateSession(rows)!;
    // The incomplete assistant turn AND its partial result are trimmed.
    expect(out.restoredMessages).toHaveLength(1);
    expect(out.restoredMessages[0]).toMatchObject({ role: "user", content: "do both" });
  });

  it("keeps only the latest summary across two compaction cycles", () => {
    const rows = [
      ...controlRows(),
      rec({ seq: 3, role: "user", content: "turn one" }),
      rec({ seq: 4, role: "system", content: "[conversation compacted — earlier turns dropped from context]" }),
      rec({ seq: 5, role: "user", content: "[compacted summary of earlier conversation]\nFIRST" }),
      rec({ seq: 6, role: "user", content: "turn two" }),
      rec({ seq: 7, role: "system", content: "[conversation compacted — earlier turns dropped from context]" }),
      rec({ seq: 8, role: "user", content: "[compacted summary of earlier conversation]\nSECOND" }),
    ];
    const out = rehydrateSession(rows)!;
    expect(out.initialSeq).toBe(9);
    expect(out.restoredMessages).toHaveLength(1);
    expect(out.restoredMessages[0].content).toContain("SECOND");
    expect(out.restoredMessages.some((m) => m.content?.includes("FIRST"))).toBe(false);
  });

  it("recognises the exact CLEAR_MARKER as the durable-log breadcrumb (#114)", () => {
    // Canary: rehydrateSession() matches the clear marker by the same constant
    // clear() writes, so the two sides cannot drift. If the marker text changes,
    // this fails loudly rather than silently breaking post-clear resume.
    const rows = [
      ...controlRows(),
      rec({ seq: 3, role: "user", content: "[system event]\nold" }),
      rec({ seq: 4, role: "system", content: CLEAR_MARKER }),
    ];
    expect(rehydrateSession(rows)!.restoredMessages).toHaveLength(0);
  });

  it("restores an empty history after a clear marker (#114)", () => {
    const rows = [
      ...controlRows(),
      rec({ seq: 3, role: "user", content: "[system event]\nbefore clear" }),
      rec({ seq: 4, role: "system", content: "[conversation cleared — context reset to initial state]" }),
    ];
    const out = rehydrateSession(rows)!;
    expect(out.initialSeq).toBe(5);
    // Nothing lives after a clear marker: a resumed session starts genuinely fresh.
    expect(out.restoredMessages).toHaveLength(0);
    expect(out.restoredMessages.some((m) => m.content?.includes("before clear"))).toBe(false);
  });

  it("restores only post-clear turns when a clear is followed by new activity (#114)", () => {
    const rows = [
      ...controlRows(),
      rec({ seq: 3, role: "user", content: "[system event]\nold" }),
      rec({ seq: 4, role: "system", content: "[conversation cleared — context reset to initial state]" }),
      rec({ seq: 5, role: "user", content: "[system event]\nafter clear" }),
    ];
    const out = rehydrateSession(rows)!;
    expect(out.restoredMessages).toHaveLength(1);
    expect(out.restoredMessages[0].content).toContain("after clear");
    expect(out.restoredMessages.some((m) => m.content?.includes("old"))).toBe(false);
  });

  it("treats the last marker as the boundary regardless of kind — clear after compact (#114)", () => {
    const rows = [
      ...controlRows(),
      rec({ seq: 3, role: "user", content: "turn one" }),
      rec({ seq: 4, role: "system", content: "[conversation compacted — earlier turns dropped from context]" }),
      rec({ seq: 5, role: "user", content: "[compacted summary of earlier conversation]\nSUMMARY" }),
      rec({ seq: 6, role: "system", content: "[conversation cleared — context reset to initial state]" }),
    ];
    const out = rehydrateSession(rows)!;
    // The clear is the LAST marker, so even the compaction summary is dropped.
    expect(out.restoredMessages).toHaveLength(0);
    expect(out.restoredMessages.some((m) => m.content?.includes("SUMMARY"))).toBe(false);
  });

  it("returns an empty history (not undefined) for a session with only control rows", () => {
    const out = rehydrateSession(controlRows())!;
    expect(out).toBeDefined();
    expect(out.restoredMessages).toHaveLength(0);
    expect(out.initialSeq).toBe(3);
  });

  it("seeds initialSeq correctly over a large row set (no spread overflow)", () => {
    const rows = controlRows();
    for (let i = 3; i < 1500; i++) rows.push(rec({ seq: i, role: "user", content: `n${i}` }));
    const out = rehydrateSession(rows)!;
    expect(out.initialSeq).toBe(1500);
    expect(out.restoredMessages).toHaveLength(1497);
  });
});

describe("AgentSession resume integration (#77)", () => {
  it("rebuilds controls fresh, restores prior turns, and does not re-persist controls", () => {
    const db = new Db(":memory:");
    const sid = "ses_resume";

    // First run: three notes of real history land after the 3 control rows.
    const first = makeSession(db, sid);
    first.injectNote("one");
    first.injectNote("two");
    const firstRows = db.listMessages(sid);
    const firstMax = Math.max(...firstRows.map((r) => r.seq));

    // Resume: a fresh session restored from the persisted rows.
    const restore = rehydrateSession(db.listMessages(sid));
    const resumed = makeSession(db, sid, { restoredMessages: restore });

    const msgs = resumed.getMessages();
    // 3 fresh control messages + the 2 restored notes.
    expect(msgs).toHaveLength(5);
    expect(msgs[1].content).toContain("# Runtime context");
    expect(msgs[3].content).toContain("one");
    expect(msgs[4].content).toContain("two");

    // The DB did NOT gain a second control triplet on resume.
    expect(db.listMessages(sid)).toHaveLength(firstRows.length);

    // A new note appends past the prior max seq — no overlap with stored rows.
    resumed.injectNote("three");
    const after = db.listMessages(sid);
    expect(after).toHaveLength(firstRows.length + 1);
    expect(Math.max(...after.map((r) => r.seq))).toBe(firstMax + 1);
  });

  it("resumes a compacted session to controls + summary only", () => {
    const db = new Db(":memory:");
    const sid = "ses_resume_compact";

    const first = makeSession(db, sid);
    first.injectNote("pre-compaction turn");
    first.compact("goals: X. open: none. next: Y.");

    const restore = rehydrateSession(db.listMessages(sid));
    const resumed = makeSession(db, sid, { restoredMessages: restore });

    const msgs = resumed.getMessages();
    // 3 controls + the single summary note; the pre-compaction turn is gone.
    expect(msgs).toHaveLength(4);
    expect(msgs[3].content).toContain("compacted summary");
    expect(msgs[3].content).toContain("goals: X");
    expect(msgs.some((m) => m.content?.includes("pre-compaction turn"))).toBe(false);
    // The compaction marker row is never replayed as a model message.
    expect(msgs.some((m) => m.content?.includes("earlier turns dropped"))).toBe(false);
  });

  it("resumes a cleared session to controls only — no restored turns (#114)", () => {
    const db = new Db(":memory:");
    const sid = "ses_resume_clear";

    const first = makeSession(db, sid);
    first.injectNote("pre-clear turn");
    first.clear();

    const restore = rehydrateSession(db.listMessages(sid));
    const resumed = makeSession(db, sid, { restoredMessages: restore });

    const msgs = resumed.getMessages();
    // Just the 3 fresh control messages; the cleared turn is gone, no summary note.
    expect(msgs).toHaveLength(3);
    expect(msgs.some((m) => m.content?.includes("pre-clear turn"))).toBe(false);
    expect(msgs.some((m) => m.content?.includes("compacted summary"))).toBe(false);
    // The clear marker row is never replayed as a model message.
    expect(msgs.some((m) => m.content?.includes("context reset to initial state"))).toBe(false);
  });

  it("is idempotent: resuming twice yields the same history without growing the DB", () => {
    const db = new Db(":memory:");
    const sid = "ses_resume_idem";

    const first = makeSession(db, sid);
    first.injectNote("only note");
    const baseline = db.listMessages(sid).length;

    const a = makeSession(db, sid, { restoredMessages: rehydrateSession(db.listMessages(sid)) });
    expect(db.listMessages(sid)).toHaveLength(baseline);
    const b = makeSession(db, sid, { restoredMessages: rehydrateSession(db.listMessages(sid)) });
    expect(db.listMessages(sid)).toHaveLength(baseline);

    expect(a.getMessages().length).toBe(b.getMessages().length);
  });
});
