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

/**
 * Fixed cadence for "pr_state" watchers. Not user-configurable: a PR's state
 * changes on the order of minutes, and every check is a `forge.getPR` API call,
 * so 60s keeps the watcher responsive without hammering the forge's rate limit
 * (60 req/hr per watcher, far inside GitHub/GitLab authenticated limits).
 */
export const PR_WATCHER_CADENCE_MS = 60_000;

/**
 * Grace window after a watcher is created during which a target terminal that is
 * absent from terminal.getStatus is treated as "still registering", NOT exited.
 * Right after agent.launch returns a terminalId, Daintree may not yet list the
 * terminal; without this grace the first (near-immediate) check misreads that gap
 * as `terminal_exited` and stops the watcher before it ever sees the agent. Once
 * the terminal has been observed at least once, absence is a real exit regardless
 * of this window. A terminal that never appears within the grace is treated as a
 * genuinely failed launch.
 */
export const WATCHER_SPAWN_GRACE_MS = 20_000;
