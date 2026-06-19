import { describe, it, expect } from "vitest";
import { parseMcpArray, parseMcpString } from "../src/mcp/resultHelpers.js";

describe("parseMcpArray", () => {
  it("reads the array from structuredContent", () => {
    const res = { structuredContent: { terminals: [{ id: "a" }] }, text: "" };
    expect(parseMcpArray(res, "terminals")).toEqual([{ id: "a" }]);
  });

  it("falls back to a JSON-encoded text body (Daintree's real shape)", () => {
    const res = {
      structuredContent: undefined,
      text: JSON.stringify({ terminals: [{ id: "a" }, { id: "b" }] }),
    };
    expect(parseMcpArray(res, "terminals")).toEqual([{ id: "a" }, { id: "b" }]);
  });

  it("merges entries from BOTH sources when each is populated", () => {
    const res = {
      structuredContent: { terminals: [{ id: "a" }] },
      text: JSON.stringify({ terminals: [{ id: "b" }] }),
    };
    expect(parseMcpArray(res, "terminals")).toEqual([{ id: "a" }, { id: "b" }]);
  });

  it("returns [] when neither source has the field", () => {
    expect(parseMcpArray({ structuredContent: {}, text: "" }, "terminals")).toEqual([]);
    expect(parseMcpArray({ structuredContent: { other: [1] }, text: "" }, "terminals")).toEqual([]);
  });

  it("ignores non-JSON text without throwing", () => {
    expect(parseMcpArray({ text: "not json at all" }, "terminals")).toEqual([]);
  });

  it("ignores a JSON text whose field is not an array", () => {
    expect(
      parseMcpArray({ text: JSON.stringify({ terminals: "nope" }) }, "terminals"),
    ).toEqual([]);
  });

  it("ignores a non-object JSON text (array / number / null)", () => {
    expect(parseMcpArray({ text: "[1,2,3]" }, "terminals")).toEqual([]);
    expect(parseMcpArray({ text: "42" }, "terminals")).toEqual([]);
    expect(parseMcpArray({ text: "null" }, "terminals")).toEqual([]);
  });

  it("ignores a non-array structured field", () => {
    expect(
      parseMcpArray({ structuredContent: { terminals: "nope" }, text: "" }, "terminals"),
    ).toEqual([]);
  });
});

describe("parseMcpString", () => {
  it("reads the string from structuredContent", () => {
    expect(parseMcpString({ structuredContent: { content: "hello" }, text: "fallback" }, "content")).toBe(
      "hello",
    );
  });

  it("falls back to the raw text body (Daintree's real shape)", () => {
    expect(parseMcpString({ structuredContent: undefined, text: "scrollback" }, "content")).toBe(
      "scrollback",
    );
  });

  it("prefers structuredContent over text when both present", () => {
    expect(parseMcpString({ structuredContent: { content: "structured" }, text: "raw" }, "content")).toBe(
      "structured",
    );
  });

  it("does NOT JSON-parse the text fallback — returns it raw", () => {
    // Terminal scrollback is a raw string, not a JSON document. Even if the text
    // happens to look like JSON, it is returned verbatim.
    expect(parseMcpString({ text: '{"content":"x"}' }, "content")).toBe('{"content":"x"}');
  });

  it("returns undefined when neither source provides a string", () => {
    expect(parseMcpString({ structuredContent: { content: 123 } }, "content")).toBeUndefined();
    expect(parseMcpString({ structuredContent: {} }, "content")).toBeUndefined();
    expect(parseMcpString({}, "content")).toBeUndefined();
  });

  it("treats an empty-string text body as a valid (empty) value, not undefined", () => {
    expect(parseMcpString({ text: "" }, "content")).toBe("");
  });
});
