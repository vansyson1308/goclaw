import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import type { MissionStatus } from "@/types/mission";

const COLORS: Record<MissionStatus, string> = {
  planned: "bg-slate-100 text-slate-700 dark:bg-slate-800 dark:text-slate-300",
  preparing: "bg-blue-100 text-blue-700 dark:bg-blue-900 dark:text-blue-300",
  running: "bg-blue-100 text-blue-700 dark:bg-blue-900 dark:text-blue-300",
  verifying: "bg-purple-100 text-purple-700 dark:bg-purple-900 dark:text-purple-300",
  succeeded: "bg-green-100 text-green-700 dark:bg-green-900 dark:text-green-300",
  partial: "bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-300",
  failed: "bg-red-100 text-red-700 dark:bg-red-900 dark:text-red-300",
  blocked: "bg-orange-100 text-orange-800 dark:bg-orange-900 dark:text-orange-300",
  cancelled: "bg-gray-100 text-gray-600 dark:bg-gray-800 dark:text-gray-400",
};

export function MissionStatusBadge({ status, testId = "mission-status" }: { status: MissionStatus; testId?: string }) {
  const { t } = useTranslation("missions");
  return (
    <Badge variant="outline" className={COLORS[status] ?? ""} data-testid={testId}>
      {t(`status.${status}`)}
    </Badge>
  );
}
