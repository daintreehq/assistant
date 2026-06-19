import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { render } from "ink-testing-library";
import { App } from "../../src/cli/app.js";
import {
  useDaintreeController,
  type DaintreeController,
} from "../../src/ui/hooks/useDaintreeController.js";
import { HOST_TERMINAL_CLEAR } from "../../src/cli/terminalClear.js";

const tick = (ms = 40) => new Promise((r) => setTimeout(r, ms));

/** Mounts the controller and publishes the latest value to the caller's holder. */
function Harness({
  app,
  onController,
}: {
  app: App;
  onController: (c: DaintreeController) => void;
}) {
  const controller = useDaintreeController(app);
  onController(controller);
  return null;
}

function makeOfflineApp() {
  const stateDir = fs.mkdtempSync(path.join(os.tmpdir(), "dt-clr-"));
  const app = App.create({
    overrides: {
      offline: true,
      stateDir,
      projectPath: stateDir,
      tier: "operator",
    },
  });
  return { app, stateDir };
}

const written = (frames: string[]) => frames.join("");

describe("useDaintreeController /clear wipes host scrollback (#137)", () => {
  it("emits the host-terminal clear sequence and resets the transcript", async () => {
    const { app, stateDir } = makeOfflineApp();

    let controller!: DaintreeController;
    const r = render(
      <Harness app={app} onController={(c) => (controller = c)} />,
    );
    // The library's stdout stub has no isTTY; force it so the TTY gate lets the
    // escape through (same technique as useAttentionSignal.test.tsx).
    (r.stdout as unknown as { isTTY: boolean }).isTTY = true;
    await tick();

    const before = r.stdout.frames.length;
    expect(controller.sendUserMessage("/clear")).toBe(true);
    await tick();

    // The scrollback wipe reached stdout...
    expect(written(r.stdout.frames.slice(before))).toContain(
      HOST_TERMINAL_CLEAR,
    );
    // ...and the transcript was reset to just the single confirmation card.
    expect(controller.transcript).toHaveLength(1);

    r.unmount();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });

  it("writes no clear escape when stdout is not a TTY", async () => {
    const { app, stateDir } = makeOfflineApp();

    let controller!: DaintreeController;
    const r = render(
      <Harness app={app} onController={(c) => (controller = c)} />,
    );
    // Leave isTTY unset → the TTY gate suppresses the escape (piped stdout).
    await tick();

    const before = r.stdout.frames.length;
    controller.sendUserMessage("/clear");
    await tick();

    expect(written(r.stdout.frames.slice(before))).not.toContain(
      HOST_TERMINAL_CLEAR,
    );
    // The logical clear still happened regardless of the TTY gate.
    expect(controller.transcript).toHaveLength(1);

    r.unmount();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });
});
