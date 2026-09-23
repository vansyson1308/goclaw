import type { TFunction } from "i18next";

/** Unknown (unpriced) cost is shown as unknown, never as $0. */
export function formatCost(cost: number | null | undefined, t: TFunction<"missions">): string {
  if (cost == null) return t("costUnknown");
  return `$${cost.toFixed(4)}`;
}
