import { test, expect, describe, mock } from "bun:test";
import { act } from "react";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { testRender } from "@opentui/react/test-utils";
import { App } from "../../src/cli/app.js";
import { DaintreeApp } from "../../src/ui/DaintreeApp.js";

// Real timers, matching the rest of the OpenTUI animation suite: the shell's mount
// effects (connect + scheduler + first poll) and the splash self-advance off
// setTimeout, so we wait wall-clock between flushes rather than pumping frames.
const tick = (ms = 40) => new Promise((r) => setTimeout(r, ms));

describe("DaintreeApp (full mount, offline)", () => {
  test("renders the single-column cockpit: header, status line, composer", async () => {
    const stateDir = fs.mkdtempSync(path.join(os.tmpdir(), "dt-ink-"));
    const app = App.create({
      // splash:false → skip the boot gate (vitest pins NO_SPLASH; bun tests don't,
      // so we disable it explicitly to land straight in the cockpit).
      overrides: { offline: true, stateDir, projectPath: stateDir, tier: "operator", splash: false },
    });
    const exit = mock();

    const t = await testRender(<DaintreeApp app={app} exit={exit} />, {
      width: 80,
      height: 24,
    });
    await t.flush();
    await tick(); // let mount effects (connect + scheduler + first poll) settle
    await t.flush();

    const frame = t.captureCharFrame();
    expect(frame).toContain("Daintree Assistant"); // brand masthead wordmark
    expect(frame).toContain("›"); // the composer prompt glyph
    // Offline → the connection badge degrades and the mount log lands in the
    // transcript (which is why the empty hint is gone by now).
    expect(frame).toContain("DEGRADED");
    expect(frame).toContain("not connected");
    // The operations surface is never inline now — it lives behind ^O / a
    // /panel command, covered by OperationsView.test.

    t.renderer.destroy?.();
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

  test("^O opens the operations view, Esc returns home", async () => {
    const stateDir = fs.mkdtempSync(path.join(os.tmpdir(), "dt-ops-"));
    const app = App.create({
      overrides: { offline: true, stateDir, projectPath: stateDir, tier: "operator", splash: false },
    });
    const exit = mock();

    const t = await testRender(<DaintreeApp app={app} exit={exit} />, {
      width: 80,
      height: 24,
    });
    await t.flush();
    await tick();
    await t.flush();

    // Home owns the screen: the composer prompt is visible.
    expect(t.captureCharFrame()).toContain("›");

    // ^O opens the full operations deck (purposeful view, not an inline dump).
    await act(async () => {
      t.mockInput.pressKey("o", { ctrl: true });
    });
    await t.flush();
    const ops = t.captureCharFrame();
    // The composer is replaced by the operations view: its prompt glyph is gone.
    expect(ops).not.toContain("›");

    // Esc returns home — the composer comes back. A lone ESC byte is ambiguous to
    // the input parser (start of an escape sequence), so it only dispatches after the
    // escape-timeout elapses — wait past it before flushing, unlike the unambiguous
    // ^O CSI sequence which fires immediately.
    await act(async () => {
      t.mockInput.pressEscape();
    });
    await tick(120);
    await t.flush();
    expect(t.captureCharFrame()).toContain("›");

    t.renderer.destroy?.();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });

  test("^C calls the injected exit callback", async () => {
    const stateDir = fs.mkdtempSync(path.join(os.tmpdir(), "dt-exit-"));
    const app = App.create({
      overrides: { offline: true, stateDir, projectPath: stateDir, tier: "operator", splash: false },
    });
    const exit = mock();

    const t = await testRender(<DaintreeApp app={app} exit={exit} />, {
      width: 80,
      height: 24,
    });
    await t.flush();
    await tick();
    await t.flush();

    // There is no useApp().exit anymore — the bootstrap injects an exit callback,
    // and Ctrl-C must drive it (the app owns its own shutdown).
    await act(async () => {
      t.mockInput.pressKey("c", { ctrl: true });
    });
    await t.flush();
    expect(exit).toHaveBeenCalled();

    t.renderer.destroy?.();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });

  test("plays the boot splash, then dissolves into the cockpit", async () => {
    const stateDir = fs.mkdtempSync(path.join(os.tmpdir(), "dt-splash-"));
    const app = App.create({
      // splash:true overrides the test-pinned NO_SPLASH so we exercise the boot gate.
      overrides: {
        offline: true,
        stateDir,
        projectPath: stateDir,
        tier: "operator",
        splash: true,
      },
    });
    const exit = mock();

    const t = await testRender(<DaintreeApp app={app} exit={exit} />, {
      width: 80,
      height: 24,
    });
    await t.flush();
    await tick(60); // a frame or two into the draw
    await t.flush();
    const booting = t.captureCharFrame();
    // The splash owns the screen: the cockpit is not rendered behind it.
    expect(booting).not.toContain("Daintree Assistant");
    expect(booting).not.toContain("›");

    // The draw finishes (~1.1s) AND offline startup settles immediately, so the gate
    // opens and the cockpit takes over. Poll for it (rather than one fixed wait) so a
    // slow CI just takes more iterations instead of flaking.
    let cockpit = "";
    for (let i = 0; i < 100; i++) {
      cockpit = t.captureCharFrame();
      if (cockpit.includes("Daintree Assistant") && cockpit.includes("›")) break;
      await tick(50);
      await t.flush();
    }
    expect(cockpit).toContain("Daintree Assistant");
    expect(cockpit).toContain("›");

    t.renderer.destroy?.();
    await app.shutdown();
    fs.rmSync(stateDir, { recursive: true, force: true });
  });
});
