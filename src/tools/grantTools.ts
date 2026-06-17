/**
 * Automation-grant tools (CLI-local). A grant is a scoped, expiring
 * authorization that lets one specific watcher/timer perform a bounded number of
 * confirm-required follow-up mutations without an interactive prompt — minted by
 * the main actor, consumed atomically at dispatch time (see registry.dispatch).
 *
 * Minting/revoking grants only mutates local daemon state, so these are risk
 * "local" ("read" for the listing). Because "local" is never confirmation-gated,
 * the ONLY guard against a non-interactive actor minting its own grant is the
 * explicit `ctx.actor === "main"` check in the create/revoke handlers below.
 */
import { z } from "zod";
import { ok, fail, type ToolDef } from "./types.js";
import { RiskClass } from "../schemas.js";

/** Tools that must never be self-granted: doing so would let an actor widen or
 * renew its own authorization. They are "local" risk (never confirm-gated) so a
 * grant has no effect on them anyway — this block just removes the foot-gun. */
const UNGRANTABLE_TOOLS: ReadonlySet<string> = new Set([
  "grant.create",
  "grant.revoke",
]);

const CreateArgs = z.object({
  actorId: z
    .string()
    .min(1)
    .describe("Watcher (wch_…) or timer (tmr_…) id this grant authorizes."),
  actorType: z
    .enum(["watcher", "timer"])
    .describe("Kind of non-interactive actor the grant is scoped to."),
  allowedRiskClasses: z
    .array(RiskClass)
    .optional()
    .describe(
      "Risk classes the actor may run unattended (union with allowedToolNames).",
    ),
  allowedToolNames: z
    .array(z.string().min(1))
    .optional()
    .describe(
      "Specific tool names the actor may run unattended (union with allowedRiskClasses).",
    ),
  ttlMs: z
    .number()
    .int()
    .positive()
    .describe("Grant lifetime in milliseconds from now (TTL)."),
  maxUses: z
    .number()
    .int()
    .positive()
    .max(1000)
    .describe("Maximum number of authorized follow-up calls before exhaustion."),
});
type CreateArgs = z.infer<typeof CreateArgs>;

const ListArgs = z.object({
  actorId: z
    .string()
    .optional()
    .describe("Only list live grants for this watcher/timer id."),
});
type ListArgs = z.infer<typeof ListArgs>;

const RevokeArgs = z.object({
  id: z.string().describe("Grant id (grt_…) to revoke."),
});
type RevokeArgs = z.infer<typeof RevokeArgs>;

export const grantTools: ToolDef[] = [
  {
    name: "grant.create",
    description:
      "Mint a scoped, expiring automation grant so a specific watcher/timer can perform a bounded number of confirm-required follow-up actions without prompting. Main actor only.",
    risk: "local",
    schema: CreateArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        actorId: {
          type: "string",
          description: "Watcher (wch_…) or timer (tmr_…) id this grant authorizes.",
        },
        actorType: {
          type: "string",
          enum: ["watcher", "timer"],
          description: "Kind of non-interactive actor the grant is scoped to.",
        },
        allowedRiskClasses: {
          type: "array",
          items: { type: "string" },
          description:
            "Risk classes the actor may run unattended (union with allowedToolNames).",
        },
        allowedToolNames: {
          type: "array",
          items: { type: "string" },
          description:
            "Specific tool names the actor may run unattended (union with allowedRiskClasses).",
        },
        ttlMs: {
          type: "number",
          description: "Grant lifetime in milliseconds from now (TTL).",
        },
        maxUses: {
          type: "number",
          description:
            "Maximum number of authorized follow-up calls before exhaustion.",
        },
      },
      required: ["actorId", "actorType", "ttlMs", "maxUses"],
    },
    async handler(args: CreateArgs, ctx) {
      // "local" risk is never confirmation-gated, so this is the only thing that
      // stops a watcher/timer from minting itself a wider grant.
      if (ctx.actor !== "main") {
        return fail(
          "GRANT_ACTOR_FORBIDDEN",
          `Only the main (interactive) actor can mint automation grants; '${ctx.actor}' may not.`,
          { recoverable: false },
        );
      }

      const risks = args.allowedRiskClasses ?? [];
      const tools = args.allowedToolNames ?? [];
      if (risks.length === 0 && tools.length === 0) {
        return fail(
          "GRANT_EMPTY_SCOPE",
          "A grant must allow at least one risk class or tool name.",
          { recoverable: false },
        );
      }
      const ungrantable = tools.filter((t) => UNGRANTABLE_TOOLS.has(t));
      if (ungrantable.length > 0) {
        return fail(
          "GRANT_UNGRANTABLE_TOOL",
          `These tools cannot be granted: ${ungrantable.join(", ")}.`,
          { recoverable: false },
        );
      }

      try {
        const now = Date.now();
        const rec = ctx.db.insertGrant({
          actorId: args.actorId,
          actorType: args.actorType,
          allowedRiskClassesJson: risks.length ? JSON.stringify(risks) : null,
          allowedToolNamesJson: tools.length ? JSON.stringify(tools) : null,
          expiresAt: now + args.ttlMs,
          maxUses: args.maxUses,
        });
        const scope = [
          risks.length ? `risk: ${risks.join(", ")}` : "",
          tools.length ? `tools: ${tools.join(", ")}` : "",
        ]
          .filter(Boolean)
          .join("; ");
        return ok(
          `Granted ${rec.id} to ${rec.actorType} ${rec.actorId}: ${args.maxUses} use(s), expires ${new Date(rec.expiresAt).toISOString()} (${scope}).`,
          {
            id: rec.id,
            actorId: rec.actorId,
            actorType: rec.actorType,
            expiresAt: rec.expiresAt,
            maxUses: rec.maxUses,
          },
        );
      } catch (e) {
        return fail(
          "GRANT_CREATE",
          `Could not create grant: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "grant.list",
    description:
      "List live automation grants (non-revoked, non-expired, with uses remaining), optionally filtered to one watcher/timer id.",
    risk: "read",
    readOnly: true,
    schema: ListArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        actorId: {
          type: "string",
          description: "Only list live grants for this watcher/timer id.",
        },
      },
    },
    async handler(args: ListArgs, ctx) {
      try {
        const grants = ctx.db.listGrants(args.actorId);
        const items = grants.map((g) => ({
          id: g.id,
          actorId: g.actorId,
          actorType: g.actorType,
          allowedRiskClasses: g.allowedRiskClassesJson
            ? (JSON.parse(g.allowedRiskClassesJson) as string[])
            : [],
          allowedToolNames: g.allowedToolNamesJson
            ? (JSON.parse(g.allowedToolNamesJson) as string[])
            : [],
          expiresAt: new Date(g.expiresAt).toISOString(),
          usesRemaining: g.usesRemaining,
          maxUses: g.maxUses,
        }));
        const summary =
          items.length === 0
            ? "No live automation grants."
            : `${items.length} live grant${items.length === 1 ? "" : "s"}: ${items
                .map(
                  (g) =>
                    `${g.id} → ${g.actorType} ${g.actorId} (${g.usesRemaining}/${g.maxUses} left)`,
                )
                .join("; ")}`;
        return ok(summary, { grants: items });
      } catch (e) {
        return fail(
          "GRANT_LIST",
          `Could not list grants: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "grant.revoke",
    description:
      "Revoke an automation grant by id so it can authorize no further actions. Main actor only.",
    risk: "local",
    schema: RevokeArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        id: { type: "string", description: "Grant id (grt_…) to revoke." },
      },
      required: ["id"],
    },
    async handler(args: RevokeArgs, ctx) {
      if (ctx.actor !== "main") {
        return fail(
          "GRANT_ACTOR_FORBIDDEN",
          `Only the main (interactive) actor can revoke grants; '${ctx.actor}' may not.`,
          { recoverable: false },
        );
      }
      try {
        const revoked = ctx.db.revokeGrant(args.id);
        if (!revoked) {
          return fail(
            "GRANT_NOT_FOUND",
            `No live grant with id ${args.id} (already revoked or never existed).`,
            { recoverable: false },
          );
        }
        return ok(`Revoked grant ${args.id}.`, { id: args.id });
      } catch (e) {
        return fail(
          "GRANT_REVOKE",
          `Could not revoke grant ${args.id}: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
];
