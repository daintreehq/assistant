import { describe, it, expect } from "vitest";
import {
  BASE_SYSTEM_PROMPT,
  BASE_SYSTEM_PROMPT_VERSION,
} from "../src/models/prompts/base.js";
import { buildRuntimeContextMessage } from "../src/models/prompts/runtimeContext.js";
import { buildLoadedRecipesMessage } from "../src/models/prompts/recipes.js";
import { renderRecipeBundle } from "../src/recipes/render.js";
import {
  BUILTIN_RECIPES,
  SPAWN_AGENT_FOR_EDITS_RECIPE,
} from "../src/recipes/builtin.js";
import type { MainPromptContext } from "../src/models/prompts/runtimeContext.js";

const CTX: MainPromptContext = {
  tier: "operator",
  projectPath: "/home/dev/widget",
  projectId: "prj_42",
  mcpConnected: true,
  mcpStatusLine: "connected (http, 11 tools)",
  largeModel: "large-model-x",
  smallModel: "small-model-y",
  activeWorktree: "wt_main",
  schedulerActive: true,
};

describe("base system prompt", () => {
  it("contains stable identity but no dynamic state or recipe bodies", () => {
    expect(BASE_SYSTEM_PROMPT).toContain("Daintree Assistant");
    expect(BASE_SYSTEM_PROMPT).toContain("never write, patch, sed");
    // No runtime/dynamic content.
    expect(BASE_SYSTEM_PROMPT).not.toContain(CTX.projectPath);
    expect(BASE_SYSTEM_PROMPT).not.toContain(CTX.mcpStatusLine);
    expect(BASE_SYSTEM_PROMPT).not.toContain(CTX.largeModel);
    expect(BASE_SYSTEM_PROMPT).not.toContain("# Runtime context");
    // No loaded-recipe section or recipe bodies.
    expect(BASE_SYSTEM_PROMPT).not.toContain("# Loaded recipes");
    expect(BASE_SYSTEM_PROMPT).not.toContain("Recipe id:");
    expect(BASE_SYSTEM_PROMPT).not.toContain(SPAWN_AGENT_FOR_EDITS_RECIPE.body);
  });

  it("pins the version used as the cache key", () => {
    expect(BASE_SYSTEM_PROMPT_VERSION).toBe("daintree-main-system-v6");
  });

  it("states the foreground-only scheduler lifecycle", () => {
    expect(BASE_SYSTEM_PROMPT).toContain("Scheduler lifecycle");
    expect(BASE_SYSTEM_PROMPT).toContain("ONLY while this assistant is open");
    expect(BASE_SYSTEM_PROMPT).toContain("watchers");
    expect(BASE_SYSTEM_PROMPT).toContain("timers");
    expect(BASE_SYSTEM_PROMPT).toContain("resume on the next launch");
  });
});

describe("runtime context message", () => {
  it("includes project path, MCP status, tier, and model ids", () => {
    const msg = buildRuntimeContextMessage(CTX);
    expect(msg).toContain("# Runtime context");
    expect(msg).toContain(CTX.projectPath);
    expect(msg).toContain(CTX.mcpStatusLine);
    expect(msg).toContain("operator");
    expect(msg).toContain(CTX.largeModel);
    expect(msg).toContain(CTX.smallModel);
  });

  it("warns clearly when MCP is not connected", () => {
    const msg = buildRuntimeContextMessage({ ...CTX, mcpConnected: false });
    expect(msg).toContain("degraded local mode");
  });

  it("warns the scheduler is dormant only when it is not running", () => {
    expect(buildRuntimeContextMessage(CTX)).not.toContain(
      "the scheduler is NOT running",
    );
    const dormant = buildRuntimeContextMessage({
      ...CTX,
      schedulerActive: false,
    });
    expect(dormant).toContain("the scheduler is NOT running");
    expect(dormant).toContain("dormant");
  });
});

describe("loaded recipes message", () => {
  it("includes selected recipe bodies and ids", () => {
    const bundle = renderRecipeBundle([SPAWN_AGENT_FOR_EDITS_RECIPE]);
    const msg = buildLoadedRecipesMessage(bundle);
    expect(msg).toContain("# Loaded recipes");
    expect(msg).toContain(`Recipe id: ${SPAWN_AGENT_FOR_EDITS_RECIPE.id}`);
    expect(msg).toContain(SPAWN_AGENT_FOR_EDITS_RECIPE.body);
  });

  it("renders a safe fallback for an empty bundle", () => {
    const msg = buildLoadedRecipesMessage(renderRecipeBundle([]));
    expect(msg).toContain("# Loaded recipes");
    expect(msg).toContain("No task-specific recipes");
  });

  it("orders recipes deterministically by id (stable cache hash)", () => {
    const a = renderRecipeBundle([...BUILTIN_RECIPES]);
    const b = renderRecipeBundle([...BUILTIN_RECIPES].reverse());
    expect(a.hash).toBe(b.hash);
    expect(a.ids).toEqual(b.ids);
  });
});
