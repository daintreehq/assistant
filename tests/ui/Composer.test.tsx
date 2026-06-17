import { render } from "ink-testing-library";
import { Composer } from "../../src/ui/components/Composer.js";

const tick = () => new Promise((r) => setTimeout(r, 20));

describe("Composer", () => {
  it("renders the prompt and idle hint", () => {
    const { lastFrame } = render(
      <Composer busy={false} onSubmit={() => {}} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("daintree ❯");
    expect(frame).toContain("help");
  });

  it("submits typed input on Enter when focused", async () => {
    let submitted: string | undefined;
    const { stdin } = render(
      <Composer busy={false} focus onSubmit={(v) => (submitted = v)} />,
    );
    stdin.write("hello");
    await tick(); // let the controlled value flush before Enter reads it
    stdin.write("\r"); // Enter
    await tick();
    expect(submitted).toBe("hello");
  });

  it("ignores keystrokes when not focused (busy / modal open)", async () => {
    let submitted: string | undefined;
    const { stdin } = render(
      <Composer busy focus={false} onSubmit={(v) => (submitted = v)} />,
    );
    stdin.write("nope");
    stdin.write("\r");
    await tick();
    expect(submitted).toBeUndefined();
  });
});
