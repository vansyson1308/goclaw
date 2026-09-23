import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router";
import { Target, Plus, AlertTriangle } from "lucide-react";
import { Button } from "@/components/ui/button";
import { PageHeader } from "@/components/shared/page-header";
import { EmptyState } from "@/components/shared/empty-state";
import { TableSkeleton } from "@/components/shared/loading-skeleton";
import { formatDate } from "@/lib/format";
import { useUiStore } from "@/stores/use-ui-store";
import { useMissions, useMissionActions } from "./hooks/use-missions";
import { MissionStatusBadge } from "./mission-status-badge";
import { CreateMissionDialog } from "./create-mission-dialog";
import { formatCost } from "./mission-format";

export function MissionsPage() {
  const { t } = useTranslation("missions");
  const tz = useUiStore((s) => s.timezone);
  const navigate = useNavigate();
  const { missions, enabled, loading, error } = useMissions();
  const { create } = useMissionActions();
  const [creating, setCreating] = useState(false);

  return (
    <div className="p-4 sm:p-6 space-y-4">
      <PageHeader
        title={t("title")}
        description={t("description")}
        actions={
          <Button size="sm" onClick={() => setCreating(true)} disabled={!enabled} data-testid="mission-new">
            <Plus className="h-4 w-4 mr-1" /> {t("newMission")}
          </Button>
        }
      />

      {!enabled && (
        <div className="flex items-start gap-2 rounded-md border border-amber-300 bg-amber-50 p-3 text-sm dark:border-amber-800 dark:bg-amber-950">
          <AlertTriangle className="h-4 w-4 mt-0.5 text-amber-600 shrink-0" />
          <div>
            <p className="font-medium">{t("disabledTitle")}</p>
            <p className="text-muted-foreground">{t("disabledHint")}</p>
          </div>
        </div>
      )}

      {error ? (
        <p className="text-sm text-destructive" role="alert">{(error as Error).message}</p>
      ) : loading ? (
        <TableSkeleton rows={5} />
      ) : missions.length === 0 ? (
        <EmptyState icon={Target} title={t("emptyTitle")} description={t("emptyDescription")} />
      ) : (
        <div className="rounded-md border">
          <div className="overflow-x-auto">
            <table className="w-full text-sm min-w-[600px]">
              <thead>
                <tr className="border-b bg-muted/50 text-left">
                  <th className="px-3 py-2 font-medium">{t("col.status")}</th>
                  <th className="px-3 py-2 font-medium">{t("col.title")}</th>
                  <th className="px-3 py-2 font-medium">{t("col.agent")}</th>
                  <th className="px-3 py-2 font-medium">{t("col.created")}</th>
                  <th className="px-3 py-2 font-medium text-right">{t("col.cost")}</th>
                </tr>
              </thead>
              <tbody>
                {missions.map((m) => (
                  <tr
                    key={m.id}
                    className="border-b hover:bg-muted/30 cursor-pointer"
                    onClick={() => navigate(`/missions/${m.id}`)}
                    data-testid="mission-row"
                  >
                    <td className="px-3 py-2"><MissionStatusBadge status={m.status} /></td>
                    <td className="px-3 py-2 max-w-[320px]">
                      <p className="truncate font-medium" title={m.title}>{m.title}</p>
                      {m.status_reason && <p className="truncate text-xs text-muted-foreground" title={m.status_reason}>{m.status_reason}</p>}
                    </td>
                    <td className="px-3 py-2 font-mono text-xs">{m.agent_key}</td>
                    <td className="px-3 py-2 text-xs text-muted-foreground whitespace-nowrap">{formatDate(m.created_at, tz)}</td>
                    <td className="px-3 py-2 text-xs text-right whitespace-nowrap">{formatCost(m.cost_usd, t)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      <CreateMissionDialog
        open={creating}
        onOpenChange={setCreating}
        onCreate={create}
        onCreated={(m) => navigate(`/missions/${m.id}`)}
      />
    </div>
  );
}
