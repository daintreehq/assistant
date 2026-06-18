import { useState } from "react";
import { render } from "ink-testing-library";
import { MultilineInput } from "../../src/ui/components/MultilineInput.js";

/** Strip ANSI so assertions read the plain glyphs. */
const plain = (s: string | undefined) =>
  (s ?? "").replace(/\[[0-9;]*m/g, "");

const ESC = "\u001B";
const UP = "\u001B[A";
const BACKSPACE = "\u007F";
const ENTER = "\r";

function Harness({
  onSubmit = () => {},
  onCancel = () => {},
  initial = "",
}: {
  onSubmit?: (v: string) => void;
  onCancel?: () => void;
  initial?: string;
}) {
  const [value, setValue] = useState(initial);
  return (
    <MultilineInput
      value={value}
      onChange={setValue}
      onSubmit={onSubmit}
      onCancel={onCancel}
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
    const { stdin, lastFrame } = render(
      <Harness onSubmit={(v) => (submitted = v)} />,
    );
    stdin.write("ab\\");
    await delay();
    stdin.write(ENTER);
    await delay();
    stdin.write("cd");
    await delay();
    expect(submitted).toBeNull(); // Enter did not submit
    const lines = plain(lastFrame()).split("\n");
    expect(lines.find((l) => l.includes("ab"))).toMatch(/›\s+ab$/); // backslash gone
    expect(lines.find((l) => l.includes("cd"))).toMatch(/^\s{2}cd/); // new line
  });

  it("backspace deletes the character before the cursor", async () => {
    const { stdin, lastFrame } = render(<Harness />);
    stdin.write("abc");
    await delay();
    stdin.write(BACKSPACE);
    await delay();
    expect(plain(lastFrame())).toContain("ab");
    expect(plain(lastFrame())).not.toMatch(/abc/);
  });

  it("up arrow moves the cursor onto the previous line", async () => {
    const { stdin, lastFrame } = render(<Harness />);
    stdin.write("aa\nbb");
    await delay();
    stdin.write(UP); // cursor now on the first line
    await delay(); // let the cursor move commit before the next keystroke
    stdin.write("X"); // inserted on the first line
    await delay();
    const lines = plain(lastFrame()).split("\n");
    expect(
      lines.some(
        (l) => l.includes("aaX") || l.includes("aXa") || l.includes("Xaa"),
      ),
    ).toBe(true);
  });
});
