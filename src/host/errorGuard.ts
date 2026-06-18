/**
 * Bootstrap error guard for the host entry. Installed synchronously before any
 * dynamic `import()` so a failed module load (or an early throw) is reported to
 * Daintree and the process exits, rather than hanging the readiness wait
 * silently — Electron 42 only *warns* on unhandled utility-process rejections,
 * it does not crash the child (mirrors Daintree invariant #5 / lesson #8833).
 *
 * Returns a disposer the caller invokes once it has installed its own,
 * longer-lived handlers, so we don't double-report.
 */
export function installBootstrapErrorGuard(report: (code: string, message: string) => void): () => void {
  const onError = (err: unknown) => {
    const message = err instanceof Error ? (err.stack ?? err.message) : String(err);
    report("bootstrap-error", message);
    // Give the message a tick to flush over parentPort before exiting.
    setImmediate(() => process.exit(1));
  };
  const onRejection = (reason: unknown) => onError(reason);

  process.on("uncaughtException", onError);
  process.on("unhandledRejection", onRejection);

  return () => {
    process.off("uncaughtException", onError);
    process.off("unhandledRejection", onRejection);
  };
}
