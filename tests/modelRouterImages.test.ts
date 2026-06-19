import { describe, it, expect, vi } from "vitest";
import { ModelRouter } from "../src/models/router.js";
import {
  FireworksClient,
  ImageInputNotSupportedError,
  imageDataPart,
  textPart,
  type ChatMessage,
} from "../src/models/fireworks.js";
import type { AppConfig } from "../src/config.js";

const CFG = {
  offline: false,
  fireworksApiKey: "test-key",
  fireworksBaseUrl: "https://example.invalid/v1",
  largeModel: "minimax-m3",
  mediumModel: "minimax-m3",
  smallModel: "deepseek-v4-flash",
} as unknown as AppConfig;

/** A FireworksClient whose three model paths are spies returning a stub result. */
function fakeClient() {
  const result = { content: "ok", reasoning: "", toolCalls: [], finishReason: "stop" };
  const chat = vi.fn(async () => result);
  const chatStream = vi.fn(async () => result);
  const json = vi.fn(async () => ({}));
  const fw = {
    chat,
    chatStream,
    json,
  } as unknown as FireworksClient;
  return { fw, chat, chatStream, json };
}

const IMG_MSGS: ChatMessage[] = [
  { role: "user", content: [textPart("describe"), imageDataPart("x")] },
];

describe("ModelRouter image tier gate", () => {
  it("allows image content on the large tier and forwards it undistorted", async () => {
    const { fw, chat } = fakeClient();
    const router = new ModelRouter(CFG, fw);
    await router.chat("large", { messages: IMG_MSGS });
    expect(chat).toHaveBeenCalledOnce();
    const sent = chat.mock.calls[0][0] as { model: string; messages: ChatMessage[] };
    expect(sent.model).toBe("minimax-m3");
    // The router must not mutate the content parts on the way through.
    expect(sent.messages).toEqual(IMG_MSGS);
  });

  it("forwards image content undistorted on the large tier via stream()", async () => {
    const { fw, chatStream } = fakeClient();
    const router = new ModelRouter(CFG, fw);
    await router.stream("large", { messages: IMG_MSGS });
    expect((chatStream.mock.calls[0][0] as { messages: ChatMessage[] }).messages).toEqual(
      IMG_MSGS,
    );
  });

  it("rejects image content on the small tier before any wire call", async () => {
    const { fw, chat } = fakeClient();
    const router = new ModelRouter(CFG, fw);
    await expect(router.chat("small", { messages: IMG_MSGS })).rejects.toBeInstanceOf(
      ImageInputNotSupportedError,
    );
    expect(chat).not.toHaveBeenCalled();
  });

  it("rejects image content on the medium tier even though it routes to large", async () => {
    const { fw, chat } = fakeClient();
    const router = new ModelRouter(CFG, fw);
    await expect(router.chat("medium", { messages: IMG_MSGS })).rejects.toBeInstanceOf(
      ImageInputNotSupportedError,
    );
    expect(chat).not.toHaveBeenCalled();
  });

  it("gates stream() and json() the same way on small and medium", async () => {
    const { fw, chatStream, json } = fakeClient();
    const router = new ModelRouter(CFG, fw);
    for (const tier of ["small", "medium"] as const) {
      await expect(router.stream(tier, { messages: IMG_MSGS })).rejects.toBeInstanceOf(
        ImageInputNotSupportedError,
      );
      await expect(
        router.json(tier, { messages: IMG_MSGS }, { parse: (x: unknown) => x } as never),
      ).rejects.toBeInstanceOf(ImageInputNotSupportedError);
    }
    expect(chatStream).not.toHaveBeenCalled();
    expect(json).not.toHaveBeenCalled();
  });

  it("carries a clear code and name on the error", async () => {
    const { fw } = fakeClient();
    const router = new ModelRouter(CFG, fw);
    const err = await router.chat("small", { messages: IMG_MSGS }).catch((e) => e);
    expect(err).toBeInstanceOf(ImageInputNotSupportedError);
    expect(err.code).toBe("IMAGE_INPUT_NOT_SUPPORTED");
    expect(err.name).toBe("ImageInputNotSupportedError");
  });

  it("does not gate plain-text messages on the small tier", async () => {
    const { fw, chat } = fakeClient();
    const router = new ModelRouter(CFG, fw);
    await router.chat("small", { messages: [{ role: "user", content: "hi" }] });
    expect(chat).toHaveBeenCalledOnce();
  });
});
