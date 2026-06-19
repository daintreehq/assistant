import { artifactTools } from "../src/tools/artifactTools.js";
import type { ToolContext } from "../src/tools/types.js";

const readTool = artifactTools.find((t) => t.name === "artifact.read")!;

/** Minimal context carrying only the artifact store the handler reads. */
function ctxWith(store?: Map<string, string>): ToolContext {
  return { artifactStore: store } as unknown as ToolContext;
}

describe("artifact.read", () => {
  it("is registered as a read-only, read-risk tool", () => {
    expect(readTool).toBeDefined();
    expect(readTool.readOnly).toBe(true);
    expect(readTool.risk).toBe("read");
  });

  it("reads the first range and reports remaining bytes", async () => {
    const store = new Map([["artifact_a", "0123456789"]]);
    const res = await readTool.handler(
      { artifactId: "artifact_a", offset: 0, limit: 4 },
      ctxWith(store),
    );
    expect(res.ok).toBe(true);
    expect(res.result).toMatchObject({
      content: "0123",
      offset: 0,
      totalChars: 10,
      nextOffset: 4,
      eof: false,
    });
  });

  it("reads a tail range and reports eof at the end", async () => {
    const store = new Map([["artifact_a", "0123456789"]]);
    const res = await readTool.handler(
      { artifactId: "artifact_a", offset: 8, limit: 100 },
      ctxWith(store),
    );
    expect(res.result).toMatchObject({ content: "89", nextOffset: 10, eof: true });
  });

  it("clamps an offset past the end to empty content at eof", async () => {
    const store = new Map([["artifact_a", "0123456789"]]);
    const res = await readTool.handler(
      { artifactId: "artifact_a", offset: 999 },
      ctxWith(store),
    );
    expect(res.ok).toBe(true);
    expect(res.result).toMatchObject({ content: "", offset: 10, eof: true });
  });

  it("defaults offset to 0 when omitted", async () => {
    const store = new Map([["artifact_a", "abcdef"]]);
    const res = await readTool.handler({ artifactId: "artifact_a" }, ctxWith(store));
    expect(res.result).toMatchObject({ offset: 0, content: "abcdef", eof: true });
  });

  it("fails with ARTIFACT_NOT_FOUND for an unknown id", async () => {
    const res = await readTool.handler({ artifactId: "nope" }, ctxWith(new Map()));
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("ARTIFACT_NOT_FOUND");
  });

  it("fails with ARTIFACT_UNAVAILABLE when no store is in context", async () => {
    const res = await readTool.handler({ artifactId: "x" }, ctxWith(undefined));
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("ARTIFACT_UNAVAILABLE");
  });
});
