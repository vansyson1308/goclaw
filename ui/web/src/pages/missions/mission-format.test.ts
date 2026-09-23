import { describe, expect, it } from "vitest";
import type { TFunction } from "i18next";
import { formatCost } from "./mission-format";

const t = ((key: string, opts?: { value?: string }) =>
  key === "costAtLeast" ? `at least ${opts?.value}` : key) as unknown as TFunction<"missions">;

describe("formatCost", () => {
  it("never shows unknown cost as $0", () => {
    expect(formatCost(null, t)).toBe("costUnknown");
    expect(formatCost(undefined, t, true)).toBe("costUnknown");
  });
  it("shows a known cost", () => {
    expect(formatCost(0.0125, t)).toBe("$0.0125");
  });
  it("marks a cost as a lower bound when an attempt's usage was lost", () => {
    expect(formatCost(0.0125, t, true)).toBe("at least $0.0125");
  });
});
