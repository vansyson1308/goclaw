import type { TFunction } from "i18next";

/**
 * Unknown (unpriced) cost is shown as unknown, never as $0. When an attempt's
 * usage was lost the known cost is only a lower bound.
 */
export function formatCost(cost: number | null | undefined, t: TFunction<"missions">, incomplete = false): string {
  if (cost == null) return t("costUnknown");
  const v = `$${cost.toFixed(4)}`;
  return incomplete ? t("costAtLeast", { value: v }) : v;
}
