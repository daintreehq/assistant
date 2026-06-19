import { readProjectName } from "../../src/ui/hooks/useDaintreeController.js";

// readProjectName is the parse behind the masthead's project line. It runs against
// whatever `actions.getContext` returns — either the validated structuredContent
// object OR (when the MCP SDK doesn't surface it) the same object parsed back out of
// the result's JSON text. Both reach this function as a plain object.
describe("readProjectName", () => {
  it("reads the top-level projectName getContext returns", () => {
    expect(readProjectName({ projectId: "ad92568c", projectName: "Daintree" })).toBe(
      "Daintree",
    );
  });

  it("falls back to a nested project.name", () => {
    expect(readProjectName({ project: { name: "My Project" } })).toBe("My Project");
  });

  it("trims surrounding whitespace", () => {
    expect(readProjectName({ projectName: "  spaced  " })).toBe("spaced");
  });

  it("ignores an empty or whitespace-only name (project not yet bound)", () => {
    expect(readProjectName({ projectName: "" })).toBeUndefined();
    expect(readProjectName({ projectName: "   " })).toBeUndefined();
    // getContext omits projectName entirely when nothing is active.
    expect(readProjectName({ portalOpen: true, terminalCount: 0 })).toBeUndefined();
  });

  it("ignores non-string names and non-object payloads", () => {
    expect(readProjectName({ projectName: 42 })).toBeUndefined();
    expect(readProjectName(null)).toBeUndefined();
    expect(readProjectName("Daintree")).toBeUndefined();
    expect(readProjectName(undefined)).toBeUndefined();
  });
});
