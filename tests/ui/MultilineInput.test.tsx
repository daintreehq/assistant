import { useState } from "react";
import { render } from "ink-testing-library";
import { MultilineInput } from "../../src/ui/components/MultilineInput.js";

/** Strip ANSI (the full ESC sequence, not just the code) so assertions read
 *  the plain glyphs even when Ink renders in colour. */
const plain = (s: string | undefined) =>
  (s ?? "").replace(/\[[0-9;]*m/g, "");

const ESC = "";
const UP = "[A";
const DOWN = "[B";
const BACKSPACE = "";
const ENTER = "\r";
const HOME = "[H";
const DEL_FWD = "[3~"; // the forward Delete key
const CTRL_A = "";
const CTRL_E = "";
const CTRL_K = "";
const CTRL_W = "";
const CTRL_Y = "";
const CTRL_U = "";
const CTRL_D = "";
const CTRL_C = "";
const CTRL_O = "";
const CTRL_X = "";
const CTRL_LEFT = "[1;5D"; // word-left
const ALT_BACKSPACE = ""; // Option+Backspace
const ALT_D = "d"; // delete next word

function Harness({
  onSubmit = () => {},
  onCancel = () => {},
  onValue,
  initial = "",
  history,
}: {
  onSubmit?: (v: string) => void;
  onCancel?: () => void;
  /** Spy on the controlled buffer so tests can assert the exact value. */
  onValue?: (v: string) => void;
  initial?: string;
  history?: string[];
}) {
  const [value, setValue] = useState(initial);
  return (
    <MultilineInput
      value={value}
      onChange={(v) => {
        setValue(v);
        onValue?.(v);
      }}
      onSubmit={onSubmit}
      onCancel={onCancel}
      history={history}
      prompt="› "
      placeholder="Ask…"
      focus
    />
  );
}

const delay = () => new Promise((r) => setTimeout(r, 20));

describe("MultilineInput", () => {
  it("shows the prompt gutter and placeholder when empty", () => {
    const { lastFrame } = render(<Harness />);
    expect(plain(lastFrame())).toContain("› ");
    expect(plain(lastFrame())).toContain("Ask…");
  });

  it("types printable characters", async () => {
    const { stdin, lastFrame } = render(<Harness />);
    stdin.write("hello");
    await delay();
    expect(plain(lastFrame())).toContain("hello");
  });

  it("inserts a multi-line paste as separate lines with a hanging indent", async () => {
    const { stdin, lastFrame } = render(<Harness />);
    stdin.write("one\ntwo");
    await delay();
    const lines = plain(lastFrame()).split("\n");
    const first = lines.find((l) => l.includes("one")) ?? "";
    const second = lines.find((l) => l.includes("two")) ?? "";
    // First line carries the chevron; the second aligns under it, no chevron.
    expect(first).toMatch(/›\s+one/);
    expect(second).toMatch(/^\s{2}two/);
    expect(second).not.toContain("›");
  });

  it("Enter submits the current value; Escape cancels", async () => {
    let submitted: string | null = null;
    let cancelled = false;
    const { stdin } = render(
      <Harness
        onSubmit={(v) => (submitted = v)}
        onCancel={() => (cancelled = true)}
      />,
    );
    stdin.write("hi");
    await delay();
    stdin.write(ENTER);
    await delay();
    expect(submitted).toBe("hi");
    stdin.write(ESC);
    await delay();
    expect(cancelled).toBe(true);
  });

  it("a trailing backslash turns Enter into a newline instead of a submit", async () => {
    let submitted: string | null = null;
    let latest = "";
    const { stdin } = render(
      <Harness onSubmit={(v) => (submitted = v)} onValue={(v) => (latest = v)} />,
    );
    stdin.write("ab\\");
    await delay();
    stdin.write(ENTER);
    await delay();
    stdin.write("cd");
    await delay();
    expect(submitted).toBeNull(); // Enter did not submit
    expect(latest).toBe("ab\ncd"); // backslash became a newline
  });

  it("backspace deletes the character before the cursor", async () => {
    let latest = "";
    const { stdin } = render(<Harness onValue={(v) => (latest = v)} />);
    stdin.write("abc");
    await delay();
    stdin.write(BACKSPACE);
    await delay();
    expect(latest).toBe("ab");
  });

  it("forward Delete removes the character at the cursor", async () => {
    let latest = "";
    const { stdin } = render(<Harness onValue={(v) => (latest = v)} />);
    stdin.write("abc");
    await delay();
    stdin.write(HOME); // cursor to start
    await delay();
    stdin.write(DEL_FWD); // delete the 'a' under the cursor
    await delay();
    expect(latest).toBe("bc");
  });

  it("^D deletes the character at the cursor", async () => {
    let latest = "";
    const { stdin } = render(<Harness onValue={(v) => (latest = v)} />);
    stdin.write("abc");
    await delay();
    stdin.write(CTRL_A);
    await delay();
    stdin.write(CTRL_D);
    await delay();
    expect(latest).toBe("bc");
  });

  it("Home/^A jump to line start and ^E to line end", async () => {
    let latest = "";
    const { stdin } = render(<Harness onValue={(v) => (latest = v)} />);
    stdin.write("abc");
    await delay();
    stdin.write(CTRL_A); // start
    await delay();
    stdin.write("X"); // → Xabc
    await delay();
    stdin.write(CTRL_E); // end
    await delay();
    stdin.write("Z"); // → XabcZ
    await delay();
    expect(latest).toBe("XabcZ");
  });

  it("Ctrl+← moves the cursor one word left", async () => {
    let latest = "";
    const { stdin } = render(<Harness onValue={(v) => (latest = v)} />);
    stdin.write("foo bar");
    await delay();
    stdin.write(CTRL_LEFT); // cursor to start of "bar"
    await delay();
    stdin.write("X");
    await delay();
    expect(latest).toBe("foo Xbar");
  });

  it("^W deletes the previous word", async () => {
    let latest = "";
    const { stdin } = render(<Harness onValue={(v) => (latest = v)} />);
    stdin.write("foo bar");
    await delay();
    stdin.write(CTRL_W);
    await delay();
    expect(latest).toBe("foo ");
  });

  it("Option+Backspace deletes the previous word", async () => {
    let latest = "";
    const { stdin } = render(<Harness onValue={(v) => (latest = v)} />);
    stdin.write("foo bar");
    await delay();
    stdin.write(ALT_BACKSPACE);
    await delay();
    expect(latest).toBe("foo ");
  });

  it("Alt+D deletes the next word", async () => {
    let latest = "";
    const { stdin } = render(<Harness onValue={(v) => (latest = v)} />);
    stdin.write("foo bar");
    await delay();
    stdin.write(CTRL_A); // cursor to start
    await delay();
    stdin.write(ALT_D); // kill "foo" forward
    await delay();
    expect(latest).toBe(" bar");
  });

  it("^U deletes the whole line regardless of cursor position", async () => {
    let latest = "?";
    const { stdin } = render(<Harness onValue={(v) => (latest = v)} />);
    stdin.write("hello world");
    await delay();
    stdin.write(CTRL_LEFT); // cursor into the middle (start of "world")
    await delay();
    stdin.write(CTRL_U); // whole line, not just to start/end
    await delay();
    expect(latest).toBe("");
  });

  it("^K kills to end of line and ^Y yanks it back", async () => {
    let latest = "?";
    const { stdin } = render(<Harness onValue={(v) => (latest = v)} />);
    stdin.write("abcdef");
    await delay();
    stdin.write(HOME); // cursor to start
    await delay();
    stdin.write(CTRL_K); // kill the whole line
    await delay();
    expect(latest).toBe("");
    stdin.write(CTRL_Y); // yank it back
    await delay();
    expect(latest).toBe("abcdef");
  });

  it("never inserts the app-level chords ^C/^O/^X as text", async () => {
    let latest = "";
    const { stdin } = render(<Harness onValue={(v) => (latest = v)} />);
    stdin.write("ab");
    await delay();
    stdin.write(CTRL_C);
    stdin.write(CTRL_O);
    stdin.write(CTRL_X);
    await delay();
    expect(latest).toBe("ab"); // chords fall through to the app, never typed
  });

  it("up arrow moves the cursor onto the previous line, keeping the column", async () => {
    let latest = "";
    const { stdin } = render(<Harness onValue={(v) => (latest = v)} />);
    stdin.write("aa\nbb");
    await delay();
    stdin.write(UP); // col 2 on row 0 → end of "aa"
    await delay();
    stdin.write("X");
    await delay();
    expect(latest).toBe("aaX\nbb");
  });

  it("↑/↓ recall prompt history and return to the live draft", async () => {
    let latest = "?";
    const { stdin, lastFrame } = render(
      <Harness history={["first", "second"]} onValue={(v) => (latest = v)} />,
    );
    stdin.write(UP); // newest entry
    await delay();
    expect(latest).toBe("second");
    stdin.write(UP); // older entry
    await delay();
    expect(latest).toBe("first");
    stdin.write(DOWN); // forward again
    await delay();
    expect(latest).toBe("second");
    stdin.write(DOWN); // past the newest → back to the (empty) draft
    await delay();
    expect(latest).toBe("");
    expect(plain(lastFrame())).toContain("Ask…"); // placeholder for the empty draft
  });

  it("restores a non-empty draft after history recall", async () => {
    let latest = "?";
    const { stdin } = render(
      <Harness history={["old"]} onValue={(v) => (latest = v)} />,
    );
    stdin.write("draft"); // a live, unsent draft
    await delay();
    stdin.write(UP); // recall "old", stashing the draft
    await delay();
    expect(latest).toBe("old");
    stdin.write(DOWN); // back past the newest → the draft returns
    await delay();
    expect(latest).toBe("draft");
  });
});
