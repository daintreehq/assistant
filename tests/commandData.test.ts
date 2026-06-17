import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { App } from "../src/cli/app.js";
import { handleUiCommand } from "../src/cli/commandData.js";

let lastStateDir = "";
function makeApp(): App {
  lastStateDir = fs.mkdtempSync(path.join(os.tmpdir(), "dt-cmd-"));
  return App.create({
    overrides: {
      offline: true,
      stateDir: lastStateDir,
      projectPath: lastStateDir,
      tier: "operator",
    },
  });
}

describe("handleUiCommand (structured slash commands)", () => {
  let app: App;
  beforeEach(() => {
    app = makeApp();
  });
  afterEach(async () => {
    await app.shutdown();
    fs.rmSync(lastStateDir, { recursive: true, force: true });
  });

  it("/status reports MCP + config", async () => {
    const r = await handleUiCommand("/status", app);
    expect(r.handled).toBe(true);
    expect(r.title).toBe("Status");
    expect(r.text).toContain("Daintree MCP");
    expect(r.text).toContain("disconnected");
  });

  it("/permissions <tier> switches tier", async () => {
    const r = await handleUiCommand("/permissions supervisor", app);
    expect(r.text).toContain("supervisor");
    expect(app.config.tier).toBe("supervisor");
  });

  it("/permissions rejects an unknown tier", async () => {
    const before = app.config.tier;
    const r = await handleUiCommand("/permissions wizard", app);
    expect(r.text).toContain("Unknown tier");
    expect(app.config.tier).toBe(before);
  });

  it("/inbox switches to the inbox panel", async () => {
    const r = await handleUiCommand("/inbox", app);
    expect(r.switchPanel).toBe("inbox");
    expect(r.title).toContain("Inbox");
  });

  it("/quit signals exit", async () => {
    const r = await handleUiCommand("/quit", app);
    expect(r.quit).toBe(true);
  });

  it("unknown command is reported, not crashed", async () => {
    const r = await handleUiCommand("/frobnicate", app);
    expect(r.title).toBe("Unknown command");
  });

  it("/tools lists the registry", async () => {
    const r = await handleUiCommand("/tools", app);
    expect(r.title).toMatch(/^Tools/);
    expect((r.text ?? "").length).toBeGreaterThan(0);
  });
});
