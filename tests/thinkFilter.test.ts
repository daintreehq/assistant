import { ThinkFilter } from "../src/models/fireworks.js";

describe("ThinkFilter", () => {
  it("strips <think>…</think> from a single push", () => {
    const filter = new ThinkFilter();
    filter.push("a<think>secret</think>b");
    filter.end();
    expect(filter.visible).toBe("ab");
    expect(filter.reasoning).toBe("secret");
  });

  it("separates think content across chunk boundaries that split tags", () => {
    const filter = new ThinkFilter();
    filter.push("a<thi");
    filter.push("nk>x");
    filter.push("</thi");
    filter.push("nk>b");
    filter.end();
    expect(filter.visible).toBe("ab");
    expect(filter.reasoning).toBe("x");
  });

  it("returns only newly visible text from each push", () => {
    const filter = new ThinkFilter();
    expect(filter.push("a<think>secret</think>")).toBe("a");
    expect(filter.push("b")).toBe("b");
    expect(filter.end()).toBe("");
  });

  it("keeps text in reasoning when the think block never closes", () => {
    const filter = new ThinkFilter();
    filter.push("a<think>unterminated reasoning");
    const flushed = filter.end();
    expect(flushed).toBe("");
    expect(filter.visible).toBe("a");
    expect(filter.reasoning).toBe("unterminated reasoning");
  });
});
