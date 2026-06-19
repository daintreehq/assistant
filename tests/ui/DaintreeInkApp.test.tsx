import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { render } from "ink-testing-library";
import { App } from "../../src/cli/app.js";
import { DaintreeInkApp } from "../../src/ui/DaintreeInkApp.js";

const tick = (ms = 40) => new Promise((r) => setTimeout(r, ms));

describe("DaintreeInkApp (full mount, offline)", () => {
  it("renders the single-column cockpit: header, status line, composer", async () => {
    const stateDir = fs.mkdtempSync(path.join(os.tmpdir(), "dt-ink-"));
    const app = App.create({
      overrides: { offline: true, stateDir, projectPath: stateDir, tier: "operator" },
    });

    const { lastFrame, unmount } = render(<DaintreeInkApp app={app} />);
    await tick(); // let mount effects (connect + scheduler + first poll) settle

    const frame = lastFrame() ?? "";
    expect(frame).toContain("Daintree Assistant"); // brand masthead wordmark
    expect(frame).toContain("›"); // the composer prompt glyph
    // Offline → the connection badge degrades and the mount log lands in the
    // transcript (which is why the empty hint is gone by now).
    expect(frame).toContain("DEGRADED");
    expect(frame).toContain("not connected");
    // The operations surface is never inline now — it lives behind ^O / a
    // /panel command, covered by OperationsView.test.

    unmount();
    // After teardown, a confirm requested by an in-flight tool call must
    // auto-decline rather than hang on a modal with no subscriber.
    const declined = await app.buildContext("main").confirm({
      toolName: "git.commit",
      risk: "git",
      summary: "post-teardown",
      args: {},
    });
    expect(declined).toBe(false);

    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });
});
