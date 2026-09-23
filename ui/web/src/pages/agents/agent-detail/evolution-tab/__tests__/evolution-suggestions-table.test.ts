import { describe, expect, it } from "vitest";
import { canRollback, changeSummary } from "../evolution-suggestions-table";
import type { EvolutionSuggestion } from "@/types/evolution";

const base: EvolutionSuggestion = {
  id: "s1",
  agent_id: "a1",
  suggestion_type: "tool_order",
  suggestion: "Disable web_fetch",
  rationale: "fails",
  parameters: { tool: "web_fetch" },
  status: "applied",
  reviewed_by: null,
  reviewed_at: null,
  created_at: "2026-09-23T00:00:00Z",
};

describe("canRollback", () => {
  it("allows rollback of an applied config change", () => {
    expect(
      canRollback({
        ...base,
        applied_change: { column: "tools_config", path: ["deny"], before: { present: false }, after: { present: true, value: ["web_fetch"] } },
      }),
    ).toBe(true);
  });

  it("hides rollback when nothing reversible was applied", () => {
    expect(canRollback({ ...base, suggestion_type: "skill_add" })).toBe(false);
    expect(canRollback({ ...base, status: "approved", suggestion_type: "threshold" })).toBe(false);
    expect(canRollback({ ...base, status: "pending" })).toBe(false);
  });

  it("allows rollback of legacy threshold rows that kept a baseline", () => {
    expect(canRollback({ ...base, suggestion_type: "threshold", parameters: { _baseline: {} } })).toBe(true);
  });
});

describe("changeSummary", () => {
  it("names the added list entries", () => {
    expect(
      changeSummary({
        ...base,
        applied_change: {
          column: "tools_config",
          path: ["deny"],
          before: { present: true, value: ["exec"] },
          after: { present: true, value: ["exec", "web_fetch"] },
        },
      }),
    ).toBe("tools_config.deny: + web_fetch");
  });

  it("returns null without a recorded change", () => {
    expect(changeSummary(base)).toBeNull();
  });
});
