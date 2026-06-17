import { decide, tierAllowsRisk } from "../src/safety/policy.js";
import type { RiskClass, Tier } from "../src/schemas.js";

const TIERS: Tier[] = ["supervisor", "operator", "system"];

describe("decide()", () => {
  it("supervisor denies 'terminal' (allowed:false, no confirmation)", () => {
    const d = decide("terminal", "supervisor");
    expect(d.allowed).toBe(false);
    expect(d.needsConfirmation).toBe(false);
    expect(d.reason).toMatch(/higher tier/);
  });

  it("operator allows 'project' with needsConfirmation:true", () => {
    const d = decide("project", "operator");
    expect(d.allowed).toBe(true);
    expect(d.needsConfirmation).toBe(true);
  });

  it("operator denies 'git' (requires system tier)", () => {
    const d = decide("git", "operator");
    expect(d.allowed).toBe(false);
    expect(d.needsConfirmation).toBe(false);
  });

  it("system allows 'git' with confirmation", () => {
    const d = decide("git", "system");
    expect(d.allowed).toBe(true);
    expect(d.needsConfirmation).toBe(true);
  });

  it("scoped approval suppresses confirmation for a confirm-risk class", () => {
    const d = decide("project", "operator", { hasScopedApproval: true });
    expect(d.allowed).toBe(true);
    expect(d.needsConfirmation).toBe(false);
  });

  it("'read' and 'local' never need confirmation in any tier", () => {
    for (const tier of TIERS) {
      for (const risk of ["read", "local"] as RiskClass[]) {
        const d = decide(risk, tier);
        expect(d.allowed).toBe(true);
        expect(d.needsConfirmation).toBe(false);
      }
    }
  });
});

describe("tierAllowsRisk matrix", () => {
  const allowed: Record<Tier, RiskClass[]> = {
    supervisor: ["read", "local", "ui"],
    operator: ["read", "local", "ui", "terminal", "project", "external"],
    system: [
      "read",
      "local",
      "ui",
      "terminal",
      "project",
      "external",
      "git",
      "system",
    ],
  };
  const all: RiskClass[] = [
    "read",
    "local",
    "ui",
    "terminal",
    "project",
    "external",
    "git",
    "system",
  ];

  for (const tier of TIERS) {
    for (const risk of all) {
      const expected = allowed[tier].includes(risk);
      it(`${tier} ${expected ? "allows" : "denies"} '${risk}'`, () => {
        expect(tierAllowsRisk(tier, risk)).toBe(expected);
      });
    }
  }
});
