/**
 * Safety policy: tier gating, the confirmation matrix, and the no-file-edit guard.
 *
 * The CLI's own safety layer sits ABOVE Daintree's tiers. Even if Daintree would
 * allow an action, we apply these rules first.
 */
import path from "node:path";
import fs from "node:fs";
import type { RiskClass, Tier } from "../schemas.js";

/** Which risk classes each CLI tier may perform at all. */
const TIER_ALLOWED: Record<Tier, ReadonlySet<RiskClass>> = {
  supervisor: new Set<RiskClass>(["read", "local", "ui"]),
  operator: new Set<RiskClass>([
    "read",
    "local",
    "ui",
    "terminal",
    "project",
    "external",
  ]),
  system: new Set<RiskClass>([
    "read",
    "local",
    "ui",
    "terminal",
    "project",
    "external",
    "git",
    "system",
  ]),
};

/** Risk classes that always require explicit user confirmation before running. */
const ALWAYS_CONFIRM: ReadonlySet<RiskClass> = new Set<RiskClass>([
  "terminal",
  "project",
  "git",
  "external",
  "system",
]);

export interface PolicyDecision {
  allowed: boolean;
  needsConfirmation: boolean;
  reason?: string;
}

export function tierAllowsRisk(tier: Tier, risk: RiskClass): boolean {
  return TIER_ALLOWED[tier].has(risk);
}

export function decide(
  risk: RiskClass,
  tier: Tier,
  opts: { hasScopedApproval?: boolean } = {},
): PolicyDecision {
  if (!tierAllowsRisk(tier, risk)) {
    return {
      allowed: false,
      needsConfirmation: false,
      reason: `'${risk}' actions require a higher tier than '${tier}'. Switch tier with /permissions.`,
    };
  }
  const needsConfirmation = ALWAYS_CONFIRM.has(risk) && !opts.hasScopedApproval;
  return { allowed: true, needsConfirmation };
}

/* -------------------------------------------------------------------------- */
/* No-file-edit guard                                                          */
/* -------------------------------------------------------------------------- */

export class FileEditAttemptError extends Error {
  readonly code = "FILE_EDIT_FORBIDDEN";
  constructor(message: string) {
    super(message);
    this.name = "FileEditAttemptError";
  }
}

/**
 * Tool-name fragments that imply mutating the local filesystem. The registry
 * asserts that NO registered tool name matches these, so a write tool can never
 * be wired in by accident.
 */
const FORBIDDEN_TOOL_FRAGMENTS = [
  "write_file",
  "writefile",
  "apply_patch",
  "applypatch",
  "edit_file",
  "editfile",
  "fs.write",
  "fs.edit",
  "file.write",
  "file.edit",
  "patch.apply",
];

export function isForbiddenToolName(name: string): boolean {
  const n = name.toLowerCase();
  return FORBIDDEN_TOOL_FRAGMENTS.some((frag) => n.includes(frag));
}

/** Throws if any registered local tool name implies file mutation. */
export function assertNoFileEditTools(toolNames: string[]): void {
  const offenders = toolNames.filter(isForbiddenToolName);
  if (offenders.length > 0) {
    throw new FileEditAttemptError(
      `Refusing to register file-mutating tools: ${offenders.join(", ")}. The CLI must delegate edits to a spawned agent.`,
    );
  }
}

/* -------------------------------------------------------------------------- */
/* Secret-file guard                                                           */
/* -------------------------------------------------------------------------- */

/**
 * Basenames and suffixes that commonly hold credentials. A read-only tool can
 * still leak secrets into the durable audit log / conversation history, so the
 * fs tools refuse to read these and the recursive search skips them. Matching is
 * on the path's basename, case-insensitive.
 */
const SECRET_BASENAMES: ReadonlySet<string> = new Set([
  ".env",
  ".npmrc",
  ".netrc",
  ".pgpass",
  ".htpasswd",
  "credentials",
  "id_rsa",
  "id_dsa",
  "id_ecdsa",
  "id_ed25519",
  ".dockercfg",
]);

const SECRET_SUFFIXES: readonly string[] = [
  ".pem",
  ".key",
  ".p12",
  ".pfx",
  ".keystore",
  ".jks",
  ".asc",
  ".gpg",
  ".ppk",
];

/**
 * True if `relOrPath` looks like a secrets-bearing file (env files, private
 * keys, cloud/credential stores, SSH keys). Matches `.env`, `.env.local`, etc.,
 * known credential basenames, and key/cert suffixes — anywhere in the path.
 */
const SECRET_DIR_SEGMENTS: ReadonlySet<string> = new Set([".ssh", ".aws", ".gnupg"]);

/** A single path segment that signals secrets (env file/dir or credential dir). */
function isSensitiveSegment(seg: string): boolean {
  if (SECRET_DIR_SEGMENTS.has(seg)) return true;
  // .env, prod.env, .env.local, an `.env/` secrets dir, etc.
  if (seg === ".env" || seg.endsWith(".env") || seg.startsWith(".env.")) return true;
  return false;
}

export function isSensitivePath(relOrPath: string): boolean {
  const lower = relOrPath.toLowerCase();
  const base = path.basename(lower);
  if (SECRET_BASENAMES.has(base)) return true;
  if (SECRET_SUFFIXES.some((s) => base.endsWith(s))) return true;
  // Check EVERY segment, so a sensitive file or directory anywhere in the path
  // (e.g. `nested/.env/x`, `home/.aws/credentials`, `config/prod.env`) is caught.
  return lower.split(/[\\/]/).some(isSensitiveSegment);
}

/**
 * Resolve a user-supplied path against the project root and ensure it stays
 * inside it. Used by every read-only fs tool. Throws on traversal.
 */
export function resolveInsideProject(projectPath: string, rel: string): string {
  const lexicalRoot = path.resolve(projectPath);
  const target = path.resolve(lexicalRoot, rel);
  // 1) Lexical containment (cheap, catches ../ traversal even for missing paths).
  assertInside(lexicalRoot, target, rel);
  // 2) Symlink-resolved containment: resolve both sides via their nearest existing
  //    ancestor so a repo-local symlink can't point outside the project (e.g. to
  //    /etc/passwd), while benign system symlinks (/tmp -> /private/tmp) on both
  //    sides cancel out.
  assertInside(realpathOfExisting(lexicalRoot), realpathOfExisting(target), rel);
  return target;
}

function assertInside(root: string, p: string, rel: string): void {
  const normalizedRoot = root.endsWith(path.sep) ? root : root + path.sep;
  if (p !== root && !p.startsWith(normalizedRoot)) {
    throw new FileEditAttemptError(`Path escapes the project root: ${rel}`);
  }
}

/** realpath the nearest existing ancestor and re-append the missing remainder. */
function realpathOfExisting(p: string): string {
  let cur = p;
  const tail: string[] = [];
  for (;;) {
    try {
      const real = fs.realpathSync(cur);
      return tail.length ? path.resolve(real, ...tail.reverse()) : real;
    } catch {
      const parent = path.dirname(cur);
      if (parent === cur) return p;
      tail.push(path.basename(cur));
      cur = parent;
    }
  }
}
