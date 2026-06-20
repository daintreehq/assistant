import { createRequire } from "node:module";
import path from "node:path";
import { pathToFileURL } from "node:url";
import stripAnsi from "strip-ansi";

type InkLog = {
  clear: () => void;
  sync: (output: string) => void;
};

type InkInstance = {
  log: InkLog;
  lastOutput: string;
  lastOutputToRender: string;
  lastTerminalWidth: number;
  __daintreeResizeReflowGuard?: boolean;
};

type StdoutLike = {
  columns?: number;
};

type InkInstancesModule = {
  default?: WeakMap<object, InkInstance>;
};

const require = createRequire(import.meta.url);

function columnsOf(stdout: StdoutLike): number | undefined {
  const columns = stdout.columns;
  return typeof columns === "number" && Number.isFinite(columns) && columns > 0
    ? Math.floor(columns)
    : undefined;
}

function outputWithLineCount(lineCount: number): string {
  return lineCount <= 0 ? "" : "\n".repeat(lineCount - 1);
}

function printableWidth(value: string): number {
  // The cockpit's glyphs are all single-cell in the supported terminal fonts
  // (box rules, arrows, braille spinner). Strip ANSI before counting so SGR state
  // never inflates the predicted terminal rows.
  return Array.from(stripAnsi(value)).length;
}

export function estimateReflowedFrameLineCount(
  output: string,
  terminalColumns: number,
): number {
  if (output.length === 0) return 0;

  // Reserve the edge column. A line that lands exactly in the final terminal
  // column can set DECAWM's pending-wrap state and become physically taller than
  // Ink's logical line count before the next erase.
  const safeColumns = Math.max(1, terminalColumns - 1);
  return output
    .split("\n")
    .reduce(
      (rows, line) =>
        rows + Math.max(1, Math.ceil(printableWidth(line) / safeColumns)),
      0,
    );
}

export function installResizeReflowGuardOnInkInstance(
  instance: InkInstance | undefined,
  stdout: StdoutLike,
): boolean {
  if (!instance || instance.__daintreeResizeReflowGuard) return false;
  if (
    !instance.log ||
    typeof instance.log.clear !== "function" ||
    typeof instance.log.sync !== "function"
  ) {
    return false;
  }

  const originalClear = instance.log.clear.bind(instance.log);
  const originalSync = instance.log.sync.bind(instance.log);

  instance.log.clear = () => {
    const currentColumns = columnsOf(stdout);
    if (currentColumns !== undefined && currentColumns < instance.lastTerminalWidth) {
      const output =
        instance.lastOutputToRender ||
        (instance.lastOutput ? `${instance.lastOutput}\n` : "");
      const logicalLineCount = output.length === 0 ? 0 : output.split("\n").length;
      const reflowedLineCount = Math.max(
        logicalLineCount,
        estimateReflowedFrameLineCount(output, currentColumns),
      );

      if (reflowedLineCount > logicalLineCount) {
        originalSync(outputWithLineCount(reflowedLineCount));
      }
    }

    originalClear();
  };
  instance.__daintreeResizeReflowGuard = true;
  return true;
}

async function inkInstances(): Promise<WeakMap<object, InkInstance> | undefined> {
  const inkIndexPath = require.resolve("ink");
  const moduleUrl = pathToFileURL(
    path.join(path.dirname(inkIndexPath), "instances.js"),
  ).href;
  const mod = (await import(moduleUrl)) as InkInstancesModule;
  return mod.default;
}

export async function installInkResizeReflowGuard(
  stdout: NodeJS.WriteStream,
): Promise<boolean> {
  try {
    const instances = await inkInstances();
    const instance = instances?.get(stdout);
    return installResizeReflowGuardOnInkInstance(instance, stdout);
  } catch {
    // Ink does not expose this hook publicly. If its internal file layout changes,
    // leave rendering intact rather than failing the assistant during startup.
    return false;
  }
}
