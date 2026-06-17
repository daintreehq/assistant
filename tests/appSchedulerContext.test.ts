import { describe, it, expect, beforeEach, afterEach } from "vitest";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { App } from "../src/cli/app.js";

let stateDir = "";
function makeApp(): App {
  stateDir = fs.mkdtempSync(path.join(os.tmpdir(), "dt-sched-"));
  return App.create({
    overrides: {
      offline: true,
      stateDir,
      projectPath: stateDir,
      tier: "operator",
    },
  });
}

describe("App scheduler lifecycle wiring", () => {
  let app: App;
  beforeEach(() => {
    app = makeApp();
  });
  afterEach(async () => {
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });

  it("reports schedulerActive=false and a dormant note before startScheduler", () => {
    expect(app.promptContext().schedulerActive).toBe(false);
    const runtimeMsg = String(app.session.getMessages()[1].content);
    expect(runtimeMsg).toContain("the scheduler is NOT running");
  });

  it("flips schedulerActive=true and clears the dormant note after startScheduler", () => {
    app.startScheduler();
    expect(app.promptContext().schedulerActive).toBe(true);
    const runtimeMsg = String(app.session.getMessages()[1].content);
    expect(runtimeMsg).not.toContain("the scheduler is NOT running");
  });
});
