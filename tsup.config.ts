import { defineConfig } from "tsup";

export default defineConfig({
  // `index` is the CLI bin; `host` is the Electron utility-process entry Daintree
  // forks for the native assistant surface (#10649).
  entry: { index: "src/cli/index.ts", host: "src/host/index.ts" },
  format: ["esm"],
  target: "node22",
  platform: "node",
  clean: true,
  sourcemap: true,
  dts: false,
  banner: { js: "#!/usr/bin/env node" },
  // OpenTUI ships a native (FFI) core — a per-platform `.dylib`/`.so` package plus
  // tree-sitter `.wasm` assets it resolves from node_modules at runtime. Bundling it
  // would break those path lookups, so keep the renderer packages external and let
  // the runtime resolve them. `node:sqlite`/`bun:sqlite` are loaded via createRequire
  // in sqliteDriver.ts (not static imports), but stay listed for safety.
  external: ["node:sqlite", "bun:sqlite", "@opentui/core", "@opentui/react"],
  esbuildOptions(options) {
    // Compile the cockpit's JSX (src/ui) with the automatic runtime, sourced from
    // OpenTUI's reconciler so the lowercase intrinsics (<box>/<text>) resolve.
    options.jsx = "automatic";
    options.jsxImportSource = "@opentui/react";
  },
  esbuildPlugins: [
    {
      // esbuild (<= 0.27.x) doesn't recognise `node:sqlite` as a builtin and
      // strips the `node:` prefix, producing an unresolvable `import "sqlite"`.
      // Pin the specifier and keep it external so the runtime resolves the builtin.
      name: "preserve-runtime-sqlite",
      setup(build) {
        build.onResolve({ filter: /^(node:sqlite|bun:sqlite)$/ }, (args) => ({
          path: args.path,
          external: true,
        }));
      },
    },
  ],
});
