import { EventEmitter } from "node:events";
import { createElement } from "react";
import { render, Text } from "ink";
import { describe, expect, it, vi } from "vitest";
import {
  estimateReflowedFrameLineCount,
  installInkResizeReflowGuard,
  installResizeReflowGuardOnInkInstance,
} from "../../src/ui/inkResizeReflowGuard.js";

function reflowPlainFrame(output: string, terminalColumns: number): string[] {
  const safeColumns = Math.max(1, terminalColumns - 1);
  const physical: string[] = [];
  for (const line of output.split("\n")) {
    if (line.length === 0) {
      physical.push("");
      continue;
    }
    for (let i = 0; i < line.length; i += safeColumns) {
      physical.push(line.slice(i, i + safeColumns));
    }
  }
  return physical;
}

function eraseBottomRows(lines: string[], count: number): string[] {
  return lines.slice(0, Math.max(0, lines.length - count));
}

function outputWithLineCount(lineCount: number): string {
  return lineCount <= 0 ? "" : "\n".repeat(lineCount - 1);
}

function eraseLineSequenceCount(output: string): number {
  return output.match(/\x1B\[2K/g)?.length ?? 0;
}

class TestStdout extends EventEmitter {
  isTTY = true;
  columns = 80;
  rows = 24;
  writable = true;
  destroyed = false;
  writableEnded = false;
  writes: string[] = [];

  write = (data: string): boolean => {
    this.writes.push(data);
    return true;
  };
}

class TestStdin extends EventEmitter {
  isTTY = true;
  setEncoding() {}
  setRawMode() {}
  resume() {}
  pause() {}
  ref() {}
  unref() {}
}

describe("Ink resize reflow guard", () => {
  it("models the stale status row left by logical-height clearing", () => {
    const wideFrame = [
      "Standing by · SYSTEM · MCP",
      "",
      "─".repeat(80),
      "› Ask Daintree to supervise, delegate, or inspect…",
      "─".repeat(80),
      "/ commands · ↑ history · ^O inspect ops",
      "agents 0 · tmr 0",
      "",
    ].join("\n");

    const logicalLineCount = wideFrame.split("\n").length;
    const physical = reflowPlainFrame(wideFrame, 58);
    const oldInkClearRemainder = eraseBottomRows(physical, logicalLineCount);
    expect(oldInkClearRemainder.join("\n")).toContain("Standing by");

    const guardedLineCount = estimateReflowedFrameLineCount(wideFrame, 58);
    const guardedRemainder = eraseBottomRows(physical, guardedLineCount);
    expect(guardedRemainder).toEqual([]);
  });

  it("teaches Ink the reflowed physical height before shrink clears", () => {
    const wideFrame = [
      "Standing by · SYSTEM · MCP",
      "",
      "─".repeat(80),
      "› Ask Daintree to supervise, delegate, or inspect…",
      "─".repeat(80),
      "/ commands · ↑ history · ^O inspect ops",
      "agents 0 · tmr 0",
      "",
    ].join("\n");
    const sync = vi.fn();
    const clear = vi.fn();
    const instance = {
      log: { clear, sync },
      lastOutput: "",
      lastOutputToRender: wideFrame,
      lastTerminalWidth: 80,
    };

    expect(installResizeReflowGuardOnInkInstance(instance, { columns: 58 })).toBe(
      true,
    );
    instance.log.clear();

    const expectedLineCount = estimateReflowedFrameLineCount(wideFrame, 58);
    expect(sync).toHaveBeenCalledWith(outputWithLineCount(expectedLineCount));
    expect(clear).toHaveBeenCalledTimes(1);
  });

  it("does not alter normal clears when the terminal did not shrink", () => {
    const sync = vi.fn();
    const clear = vi.fn();
    const instance = {
      log: { clear, sync },
      lastOutput: "",
      lastOutputToRender: "Standing by · SYSTEM · MCP\n",
      lastTerminalWidth: 58,
    };

    installResizeReflowGuardOnInkInstance(instance, { columns: 80 });
    instance.log.clear();

    expect(sync).not.toHaveBeenCalled();
    expect(clear).toHaveBeenCalledTimes(1);
  });

  it("installs on the live Ink instance for a stdout stream", async () => {
    const stdout = new TestStdout();
    const stdin = new TestStdin();
    const instance = render(createElement(Text, null, "hello"), {
      stdout: stdout as unknown as NodeJS.WriteStream,
      stdin: stdin as unknown as NodeJS.ReadStream,
      stderr: stdout as unknown as NodeJS.WriteStream,
      exitOnCtrlC: false,
      patchConsole: false,
    });

    try {
      expect(
        await installInkResizeReflowGuard(
          stdout as unknown as NodeJS.WriteStream,
        ),
      ).toBe(true);
      expect(
        await installInkResizeReflowGuard(
          stdout as unknown as NodeJS.WriteStream,
        ),
      ).toBe(false);
    } finally {
      instance.unmount();
      instance.cleanup();
    }
  });

  it("expands Ink's real resize clear to the old frame's reflowed height", async () => {
    const wideFrame = [
      "Standing by · SYSTEM · MCP",
      "",
      "─".repeat(80),
      "› Ask Daintree to supervise, delegate, or inspect…",
      "─".repeat(80),
      "/ commands · ↑ history · ^O inspect ops",
      "agents 0 · tmr 0",
    ].join("\n");
    const stdout = new TestStdout();
    const stdin = new TestStdin();
    const instance = render(createElement(Text, null, wideFrame), {
      stdout: stdout as unknown as NodeJS.WriteStream,
      stdin: stdin as unknown as NodeJS.ReadStream,
      stderr: stdout as unknown as NodeJS.WriteStream,
      exitOnCtrlC: false,
      patchConsole: false,
    });

    try {
      expect(
        await installInkResizeReflowGuard(
          stdout as unknown as NodeJS.WriteStream,
        ),
      ).toBe(true);
      stdout.writes = [];
      stdout.columns = 58;
      stdout.emit("resize");

      const eraseCount = eraseLineSequenceCount(stdout.writes.join(""));
      expect(eraseCount).toBeGreaterThan(wideFrame.split("\n").length);
      expect(eraseCount).toBeGreaterThanOrEqual(
        estimateReflowedFrameLineCount(`${wideFrame}\n`, 58),
      );
    } finally {
      instance.unmount();
      instance.cleanup();
    }
  });
});
