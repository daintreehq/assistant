import { defineConfig } from "vitest/config";

export default defineConfig({
  // Ink/React components are authored as .tsx — compile JSX with the automatic runtime.
  esbuild: { jsx: "automatic" },
  test: {
    globals: true,
    environment: "node",
    include: ["tests/**/*.test.{ts,tsx}"],
    testTimeout: 15000,
    // Neutralize the developer's project `.env` debug flag so App-based tests
    // (real config, projectPath = repo root) never write a logs/debug.log into the
    // tree. dotenv won't override an already-set var; debugLog unit tests pass an
    // explicit config object, so they are unaffected by this.
    // Also skip the boot splash so the cockpit renders synchronously under test —
    // the splash is timer-driven and would otherwise hide the UI for ~1s.
    env: { DAINTREE_ASSISTANT_DEBUG_LOG: "0", DAINTREE_ASSISTANT_NO_SPLASH: "1" },
  },
});
