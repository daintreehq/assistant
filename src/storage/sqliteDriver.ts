/**
 * Runtime-adaptive synchronous SQLite driver.
 *
 * The cockpit's native renderer (OpenTUI) runs under **Bun**, where `node:sqlite`
 * does not exist; the non-UI test suite and any future Electron host run under
 * **Node**, where `bun:sqlite` does not exist. The durable store (`db.ts`) is the
 * same code in both worlds, so it talks to this driver instead of a hard-coded
 * builtin: we pick `bun:sqlite` when running under Bun and Node's built-in
 * `node:sqlite` otherwise, and expose the small synchronous surface `db.ts` needs
 * (`exec` / `prepare` → `run|get|all` / `close`) with identical semantics.
 *
 * Both modules are loaded via `createRequire` so the bundler leaves the `node:` /
 * `bun:` specifiers intact (esbuild otherwise strips the prefix off builtins it
 * doesn't recognise, producing an unresolvable `import "sqlite"`), and so a plain
 * `tsc` type-check never has to resolve the runtime-only `bun:sqlite` types.
 */
import { createRequire } from "node:module";

const require_ = createRequire(import.meta.url);

/** Result of a mutating statement — mirrors `node:sqlite`'s `StatementResultingChanges`. */
export interface RunResult {
  changes: number | bigint;
  lastInsertRowid: number | bigint;
}

/** The subset of a prepared statement `db.ts` uses. Positional params only. */
export interface SqliteStatement {
  run(...params: unknown[]): RunResult;
  get(...params: unknown[]): unknown;
  all(...params: unknown[]): unknown[];
}

/** The subset of a database handle `db.ts` uses. */
export interface SqliteDatabase {
  exec(sql: string): void;
  prepare(sql: string): SqliteStatement;
  close(): void;
}

export type SqliteDatabaseConstructor = new (path: string) => SqliteDatabase;

const isBun = typeof (globalThis as { Bun?: unknown }).Bun !== "undefined";

/** Node's built-in driver already matches {@link SqliteDatabase} exactly. */
function loadNodeDriver(): SqliteDatabaseConstructor {
  const { DatabaseSync } = require_("node:sqlite") as {
    DatabaseSync: SqliteDatabaseConstructor;
  };
  return DatabaseSync;
}

/**
 * `bun:sqlite`'s `Database` is API-compatible enough that a thin wrapper suffices.
 * The one normalization that matters: `node:sqlite`'s `.get()` returns `undefined`
 * for a miss while `bun:sqlite` returns `null` — `db.ts` checks `=== undefined`, so
 * we coerce. Everything else (positional binding, `{ changes, lastInsertRowid }`,
 * `.all()` arrays, `PRAGMA` rows) is already shaped the same.
 */
function loadBunDriver(): SqliteDatabaseConstructor {
  const { Database } = require_("bun:sqlite") as {
    Database: new (path: string) => {
      exec(sql: string): void;
      prepare(sql: string): {
        run(...params: unknown[]): RunResult;
        get(...params: unknown[]): unknown;
        all(...params: unknown[]): unknown[];
      };
      close(): void;
    };
  };
  return class BunDatabaseSync implements SqliteDatabase {
    private readonly db: InstanceType<typeof Database>;
    constructor(path: string) {
      this.db = new Database(path);
    }
    exec(sql: string): void {
      this.db.exec(sql);
    }
    prepare(sql: string): SqliteStatement {
      const stmt = this.db.prepare(sql);
      return {
        run: (...params) => stmt.run(...params),
        get: (...params) => stmt.get(...params) ?? undefined,
        all: (...params) => stmt.all(...params),
      };
    }
    close(): void {
      this.db.close();
    }
  };
}

/** The active synchronous SQLite constructor for this runtime. */
export const DatabaseSync: SqliteDatabaseConstructor = isBun
  ? loadBunDriver()
  : loadNodeDriver();
