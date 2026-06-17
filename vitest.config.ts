import { defineConfig } from "vitest/config";

export default defineConfig({
  // Ink/React components are authored as .tsx — compile JSX with the automatic runtime.
  esbuild: { jsx: "automatic" },
  test: {
    globals: true,
    environment: "node",
    include: ["tests/**/*.test.{ts,tsx}"],
    testTimeout: 15000,
  },
});
