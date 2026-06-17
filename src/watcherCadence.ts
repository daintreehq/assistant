/**
 * Watcher cadence constants — the timing contract shared by the scheduler and
 * the tools that create watchers.
 *
 * The scheduler ticks every {@link SCHEDULER_TICK_MS}; a watcher's effective
 * check interval is therefore `max(cadenceMs, SCHEDULER_TICK_MS)`. Supervisor
 * watchers (attached when the CLI spawns a worker terminal) need a short cadence
 * so a stalled or blocked worker surfaces in seconds, not minutes — hence the
 * scheduler tick and the supervisor default are kept equal so the stored cadence
 * is honoured exactly. Monitor watchers (user-created background watchers) stay
 * slow by default to keep classification cost low.
 */

/** Scheduler tick interval. Also the floor for any supervisor cadence — a
 * supervisor watcher cannot be checked faster than the scheduler fires. */
export const SCHEDULER_TICK_MS = 3_000;

/** Default cadence for supervisor watchers attached to CLI-spawned workers. */
export const SUPERVISOR_DEFAULT_CADENCE_MS = 3_000;

/** Default cadence for user-created background ("monitor") watchers. */
export const MONITOR_DEFAULT_CADENCE_MS = 120_000;
