import {
  HOST_TERMINAL_CLEAR,
  clearHostTerminal,
} from "../src/cli/terminalClear.js";

/** A minimal writable stub that records every chunk and exposes a togglable isTTY. */
function fakeStdout(opts: { isTTY?: boolean; throws?: boolean } = {}) {
  const chunks: string[] = [];
  return {
    isTTY: opts.isTTY,
    write(chunk: string) {
      if (opts.throws) throw new Error("pipe broken");
      chunks.push(chunk);
      return true;
    },
    chunks,
  } as unknown as NodeJS.WriteStream & { chunks: string[] };
}

describe("clearHostTerminal", () => {
  it("HOST_TERMINAL_CLEAR is erase-viewport + erase-scrollback + cursor-home", () => {
    expect(HOST_TERMINAL_CLEAR).toBe("\x1b[2J\x1b[3J\x1b[H");
  });

  it("writes exactly the clear sequence on a real TTY", () => {
    const stdout = fakeStdout({ isTTY: true });
    clearHostTerminal(stdout);
    expect((stdout as unknown as { chunks: string[] }).chunks).toEqual([
      HOST_TERMINAL_CLEAR,
    ]);
  });

  it("writes nothing when stdout is undefined", () => {
    expect(() => clearHostTerminal(undefined)).not.toThrow();
  });

  it("writes nothing when isTTY is false", () => {
    const stdout = fakeStdout({ isTTY: false });
    clearHostTerminal(stdout);
    expect((stdout as unknown as { chunks: string[] }).chunks).toEqual([]);
  });

  it("writes nothing when isTTY is unset (piped / non-interactive)", () => {
    const stdout = fakeStdout({});
    clearHostTerminal(stdout);
    expect((stdout as unknown as { chunks: string[] }).chunks).toEqual([]);
  });

  it("swallows a failing write so a broken pipe can't crash the caller", () => {
    const stdout = fakeStdout({ isTTY: true, throws: true });
    expect(() => clearHostTerminal(stdout)).not.toThrow();
  });
});
