import { describe, it, expect, vi } from "vitest";
import {
  FireworksClient,
  contentToText,
  hasImageContent,
  imageDataPart,
  textPart,
  type ChatMessage,
} from "../src/models/fireworks.js";
import type { AppConfig } from "../src/config.js";

const CFG = {
  offline: false,
  fireworksApiKey: "test-key",
  fireworksBaseUrl: "https://example.invalid/v1",
} as unknown as AppConfig;

/** A non-streaming chat fake that records the payload sent to the OpenAI SDK. */
function chatRecorder() {
  const calls: Array<Record<string, unknown>> = [];
  const create = vi.fn(async (payload: Record<string, unknown>) => {
    calls.push(payload);
    return {
      choices: [{ message: { content: "ok", tool_calls: undefined }, finish_reason: "stop" }],
      usage: undefined,
    };
  });
  const fw = new FireworksClient(CFG);
  (fw as unknown as { client: unknown }).client = { chat: { completions: { create } } };
  return { fw, calls };
}

describe("multimodal content helpers", () => {
  it("imageDataPart wraps base64 as a data URI defaulting to PNG, no detail field", () => {
    const part = imageDataPart("AAAA");
    expect(part).toEqual({
      type: "image_url",
      image_url: { url: "data:image/png;base64,AAAA" },
    });
    expect((part.image_url as Record<string, unknown>).detail).toBeUndefined();
  });

  it("imageDataPart honours a custom mime type", () => {
    expect(imageDataPart("ZZ", "image/jpeg").image_url.url).toBe(
      "data:image/jpeg;base64,ZZ",
    );
  });

  it("textPart builds a text content part", () => {
    expect(textPart("hi")).toEqual({ type: "text", text: "hi" });
  });

  it("hasImageContent detects image parts only", () => {
    expect(hasImageContent([{ role: "user", content: "plain" }])).toBe(false);
    expect(
      hasImageContent([{ role: "user", content: [textPart("just text")] }]),
    ).toBe(false);
    expect(
      hasImageContent([
        { role: "user", content: [textPart("look"), imageDataPart("x")] },
      ]),
    ).toBe(true);
  });

  it("contentToText flattens, collapsing images to a marker", () => {
    expect(contentToText(null)).toBe("");
    expect(contentToText("hello")).toBe("hello");
    expect(
      contentToText([textPart("Describe this"), imageDataPart("bigbase64")]),
    ).toBe("Describe this\n[image omitted]");
  });
});

describe("toWireMessages multimodal passthrough", () => {
  it("forwards a user content-part array to the SDK unchanged", async () => {
    const { fw, calls } = chatRecorder();
    const messages: ChatMessage[] = [
      { role: "user", content: [textPart("What is in this screenshot?"), imageDataPart("abc123")] },
    ];
    await fw.chat({ model: "m", messages });
    expect(calls[0].messages).toEqual([
      {
        role: "user",
        content: [
          { type: "text", text: "What is in this screenshot?" },
          { type: "image_url", image_url: { url: "data:image/png;base64,abc123" } },
        ],
      },
    ]);
  });

  it("leaves plain string content untouched", async () => {
    const { fw, calls } = chatRecorder();
    await fw.chat({ model: "m", messages: [{ role: "user", content: "hi" }] });
    expect(calls[0].messages).toEqual([{ role: "user", content: "hi" }]);
  });
});
