import { defineConfig } from "tsup";

export default defineConfig({
  entry: { index: "src/cli/index.ts" },
  format: ["esm"],
  target: "node22",
  platform: "node",
  clean: true,
  sourcemap: true,
  dts: false,
  banner: { js: "#!/usr/bin/env node" },
  external: ["node:sqlite"],
  esbuildOptions(options) {
    // Compile JSX (the Ink UI under src/ui) with the React 17+ automatic runtime.
    options.jsx = "automatic";
  },
  esbuildPlugins: [
    {
      // esbuild (<= 0.27.x) doesn't recognise `node:sqlite` as a builtin and
      // strips the `node:` prefix, producing an unresolvable `import "sqlite"`.
      // Pin the specifier and keep it external so Node resolves the builtin.
      name: "preserve-node-sqlite",
      setup(build) {
        build.onResolve({ filter: /^node:sqlite$/ }, () => ({
          path: "node:sqlite",
          external: true,
        }));
      },
    },
  ],
});
