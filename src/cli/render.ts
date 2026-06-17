/**
 * Terminal rendering helpers for the legacy console surface (one-shot, doctor,
 * --classic REPL). The interactive default is the Ink UI under src/ui. Keep this
 * dependency-free (just ANSI codes). NO_COLOR is respected.
 */
import { truncate, compactArgs } from "../utils/text.js";

// Re-export so existing importers keep working from a single place.
export { truncate, compactArgs };

const useColor = !process.env.NO_COLOR && process.stdout.isTTY;

function wrap(code: string, s: string): string {
  return useColor ? `\x1b[${code}m${s}\x1b[0m` : s;
}

export const c = {
  dim: (s: string) => wrap("2", s),
  bold: (s: string) => wrap("1", s),
  red: (s: string) => wrap("31", s),
  green: (s: string) => wrap("32", s),
  yellow: (s: string) => wrap("33", s),
  blue: (s: string) => wrap("34", s),
  magenta: (s: string) => wrap("35", s),
  cyan: (s: string) => wrap("36", s),
  gray: (s: string) => wrap("90", s),
};

export const render = {
  out(s: string): void {
    process.stdout.write(s);
  },
  line(s = ""): void {
    process.stdout.write(s + "\n");
  },
  streamToken(s: string): void {
    process.stdout.write(s);
  },
  info(s: string): void {
    this.line(c.cyan("ℹ ") + s);
  },
  warn(s: string): void {
    this.line(c.yellow("⚠ ") + s);
  },
  error(s: string): void {
    this.line(c.red("✗ ") + s);
  },
  success(s: string): void {
    this.line(c.green("✓ ") + s);
  },
  toolCall(name: string, args: unknown): void {
    const a = compactArgs(args);
    this.line(c.gray(`  ⚙ ${name}(${a})`));
  },
  toolResult(ok: boolean, summary: string): void {
    const mark = ok ? c.green("  ↳") : c.red("  ↳");
    this.line(`${mark} ${c.dim(truncate(summary, 200))}`);
  },
  banner(lines: string[]): void {
    this.line();
    for (const l of lines) this.line(l);
    this.line();
  },
  assistantStart(): void {
    this.line();
  },
  assistantEnd(): void {
    this.line("\n");
  },
};
