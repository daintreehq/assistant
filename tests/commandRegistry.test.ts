import {
  COMMAND_REGISTRY,
  paletteEntries,
  overlayEntries,
  helpLines,
} from "../src/commandRegistry.js";

describe("command registry (single source of truth for slash commands)", () => {
  it("has unique command names", () => {
    const names = COMMAND_REGISTRY.map((c) => c.name);
    expect(new Set(names).size).toBe(names.length);
  });

  it("is the exact, ordered set of commands the handlers accept (issue #50)", () => {
    // Pinned so silently dropping a command (e.g. /models, the original bug, or
    // /doctor) fails here rather than only surfacing as a missing palette entry.
    expect(COMMAND_REGISTRY.map((c) => c.name)).toEqual([
      "status",
      "inbox",
      "tools",
      "timers",
      "watchers",
      "audit",
      "explain",
      "models",
      "permissions",
      "recipes",
      "compact",
      "clear",
      "doctor",
      "reconnect",
      "help",
      "quit",
    ]);
  });

  it("every entry carries non-empty palette, syntax, and help text", () => {
    for (const c of COMMAND_REGISTRY) {
      expect(c.palette.length).toBeGreaterThan(0);
      expect(c.syntax.startsWith("/")).toBe(true);
      expect(c.syntax.slice(1).startsWith(c.name)).toBe(true);
      expect(c.help.length).toBeGreaterThan(0);
    }
  });

  it("derives the palette, overlay, and help-text surfaces from one registry", () => {
    // All three surfaces enumerate the same commands, in the same order — so they
    // cannot silently diverge the way the old hand-maintained literals did.
    expect(paletteEntries()).toHaveLength(COMMAND_REGISTRY.length);
    expect(overlayEntries()).toHaveLength(COMMAND_REGISTRY.length);
    expect(helpLines()).toHaveLength(COMMAND_REGISTRY.length);

    const fromPalette = paletteEntries().map(([cmd]) => cmd.slice(1));
    const fromOverlay = overlayEntries().map(([syntax]) => syntax.slice(1).split(/\s/)[0]);
    expect(fromPalette).toEqual(COMMAND_REGISTRY.map((c) => c.name));
    expect(fromOverlay).toEqual(COMMAND_REGISTRY.map((c) => c.name));
  });

  it("formats help lines with the syntax left-padded ahead of the description", () => {
    const modelsLine = helpLines().find((l) => l.startsWith("/models"));
    expect(modelsLine).toBeDefined();
    expect(modelsLine).toContain("model routing");
    // The description starts past the pad column, never butted against the syntax.
    expect(modelsLine).toMatch(/^\/models {2,}model routing/);
  });
});
