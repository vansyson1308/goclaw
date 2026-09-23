import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate, useParams } from "react-router";
import { ArrowLeft, CheckCircle2, XCircle, AlertCircle, ChevronDown, ChevronRight, Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { TableSkeleton } from "@/components/shared/loading-skeleton";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { formatDate, formatTokens } from "@/lib/format";
import { useUiStore } from "@/stores/use-ui-store";
import { toast } from "@/stores/use-toast-store";
import { ACTIVE_MISSION_STATUSES, type CriterionResult } from "@/types/mission";
import { useMission, useMissionActions } from "./hooks/use-missions";
import { MissionStatusBadge } from "./mission-status-badge";
import { formatCost } from "./mission-format";

const RESULT_ICON = {
  pass: <CheckCircle2 className="h-4 w-4 text-green-600" />,
  fail: <XCircle className="h-4 w-4 text-red-600" />,
  error: <AlertCircle className="h-4 w-4 text-orange-600" />,
} as const;

export function MissionDetailPage() {
  const { t } = useTranslation("missions");
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const tz = useUiStore((s) => s.timezone);
  const { mission: m, events, loading, error } = useMission(id);
  const { cancel } = useMissionActions();
  const [confirmCancel, setConfirmCancel] = useState(false);
  const [showDiff, setShowDiff] = useState(true);

  if (loading) return <div className="p-4 sm:p-6"><TableSkeleton rows={6} /></div>;
  if (error || !m) {
    return (
      <div className="p-4 sm:p-6 space-y-3">
        <Button variant="ghost" size="sm" onClick={() => navigate("/missions")}><ArrowLeft className="h-4 w-4 mr-1" />{t("detail.back")}</Button>
        <p className="text-sm text-destructive" role="alert">{error ? (error as Error).message : t("detail.notFound")}</p>
      </div>
    );
  }
  const active = ACTIVE_MISSION_STATUSES.includes(m.status);
  const criteria = m.verification ?? [];

  const doCancel = async () => {
    try {
      await cancel(m.id);
      toast.success(t("detail.cancelled"));
    } catch (e) {
      toast.error(e instanceof Error ? e.message : String(e));
    } finally {
      setConfirmCancel(false);
    }
  };

  return (
    <div className="p-4 sm:p-6 space-y-6">
      <div className="space-y-2">
        <Button variant="ghost" size="sm" onClick={() => navigate("/missions")}><ArrowLeft className="h-4 w-4 mr-1" />{t("detail.back")}</Button>
        <div className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
          <div className="space-y-1 min-w-0">
            <h1 className="text-xl font-semibold tracking-tight break-words">{m.title}</h1>
            <div className="flex flex-wrap items-center gap-2 text-sm">
              <MissionStatusBadge status={m.status} testId="mission-detail-status" />
              {active && <Loader2 className="h-4 w-4 animate-spin text-muted-foreground" aria-label={t("detail.inProgress")} />}
              <span className="text-muted-foreground font-mono text-xs">{m.agent_key}</span>
            </div>
            {m.status_reason && <p className="text-sm text-muted-foreground" data-testid="mission-reason">{m.status_reason}</p>}
          </div>
          {active && (
            <Button variant="outline" size="sm" onClick={() => setConfirmCancel(true)} data-testid="mission-cancel">{t("detail.cancel")}</Button>
          )}
        </div>
      </div>

      <section className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-3 text-sm">
        <Stat label={t("detail.tokens")} value={`${formatTokens(m.input_tokens)} / ${formatTokens(m.output_tokens)}`} />
        <Stat label={t("detail.cost")} value={formatCost(m.cost_usd, t)} />
        <Stat label={t("detail.executor")} value={m.executor || "—"} />
        <Stat label={t("detail.finished")} value={m.finished_at ? formatDate(m.finished_at, tz) : "—"} />
      </section>

      <section className="space-y-2">
        <h2 className="text-sm font-medium">{t("detail.criteria")}</h2>
        {criteria.length === 0 ? (
          <p className="text-xs text-muted-foreground">{active ? t("detail.criteriaPending") : t("detail.criteriaNone")}</p>
        ) : (
          <div className="rounded-md border divide-y" data-testid="mission-criteria">
            {criteria.map((c) => <CriterionRow key={c.id} c={c} />)}
          </div>
        )}
      </section>

      <section className="space-y-2">
        <h2 className="text-sm font-medium">{t("detail.changedFiles")}</h2>
        {m.changed_files && m.changed_files.length > 0 ? (
          <div className="flex flex-wrap gap-1">
            {m.changed_files.map((f) => <Badge key={f} variant="outline" className="font-mono text-xs">{f}</Badge>)}
          </div>
        ) : <p className="text-xs text-muted-foreground">{t("detail.noChanges")}</p>}
        {m.diff && (
          <div className="space-y-1">
            <button className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground" onClick={() => setShowDiff((v) => !v)}>
              {showDiff ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}{t("detail.diff")}
            </button>
            {showDiff && <DiffView diff={m.diff} />}
            {m.diff_truncated && <p className="text-xs text-amber-600">{t("detail.diffTruncated")}</p>}
          </div>
        )}
      </section>

      {m.summary && (
        <section className="space-y-1">
          <h2 className="text-sm font-medium">{t("detail.summary")}</h2>
          <p className="text-xs text-muted-foreground">{t("detail.summaryHint")}</p>
          <p className="text-sm whitespace-pre-wrap rounded-md bg-muted/40 p-3">{m.summary}</p>
        </section>
      )}

      <section className="space-y-2">
        <h2 className="text-sm font-medium">{t("detail.events")}</h2>
        <ol className="space-y-1 text-xs" data-testid="mission-events">
          {events.map((e) => (
            <li key={e.id} className="flex flex-wrap gap-x-2">
              <span className="text-muted-foreground whitespace-nowrap">{formatDate(e.created_at, tz)}</span>
              {e.kind === "transition"
                ? <span className="font-mono">{e.from_status ? `${e.from_status} → ` : ""}{e.to_status}</span>
                : <span className="font-mono">{e.kind}</span>}
              {e.message && <span className="text-muted-foreground break-all">{e.message}</span>}
              <span className="text-muted-foreground">· {e.actor}</span>
            </li>
          ))}
        </ol>
      </section>

      <Dialog open={confirmCancel} onOpenChange={setConfirmCancel}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t("detail.confirmCancel")}</DialogTitle>
            <DialogDescription>{t("detail.confirmCancelHint")}</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirmCancel(false)}>{t("common.back")}</Button>
            <Button variant="destructive" onClick={doCancel}>{t("detail.cancel")}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-md border p-3">
      <p className="text-xs text-muted-foreground">{label}</p>
      <p className="font-medium break-all">{value}</p>
    </div>
  );
}

function CriterionRow({ c }: { c: CriterionResult }) {
  const { t } = useTranslation("missions");
  const [open, setOpen] = useState(c.status !== "pass");
  return (
    <div className="p-3 space-y-1" data-testid={`criterion-${c.id}`} data-status={c.status}>
      <button className="flex w-full items-start gap-2 text-left" onClick={() => setOpen((v) => !v)}>
        <span className="mt-0.5">{RESULT_ICON[c.status] ?? RESULT_ICON.error}</span>
        <span className="flex-1 min-w-0">
          <span className="font-medium">{c.id}</span>
          <span className="ml-2 text-xs text-muted-foreground">{c.kind}</span>
          {c.proves_change && <Badge variant="outline" className="ml-2 text-[10px]">{t("detail.provesChange")}</Badge>}
          {c.baseline_status && <span className="ml-2 text-xs text-muted-foreground">{t("detail.baseline", { status: c.baseline_status })}</span>}
          {c.description && <span className="block text-xs text-muted-foreground">{c.description}</span>}
        </span>
        <span className="text-xs font-medium uppercase">{t(`result.${c.status}`)}</span>
      </button>
      {open && (
        <div className="pl-6 space-y-1 text-xs">
          {c.detail && <p className="text-muted-foreground">{c.detail}</p>}
          {c.command && <p className="font-mono break-all">$ {c.command.join(" ")}{c.exit_code != null ? `  → exit ${c.exit_code}` : ""}</p>}
          {c.matched_files && c.matched_files.length > 0 && <p className="font-mono">{c.matched_files.join(", ")}</p>}
          {c.tests && Object.keys(c.tests).length > 0 && (
            <ul className="font-mono" data-testid={`criterion-tests-${c.id}`}>
              {Object.entries(c.tests).map(([name, st]) => (
                <li key={name}>{t("detail.expectedTest", { name, status: st })}</li>
              ))}
            </ul>
          )}
          {c.output_tail && <pre className="max-h-48 overflow-auto rounded bg-muted/50 p-2 whitespace-pre-wrap break-all">{c.output_tail}</pre>}
        </div>
      )}
    </div>
  );
}

function DiffView({ diff }: { diff: string }) {
  return (
    <pre className="max-h-[480px] overflow-auto rounded-md border bg-muted/30 p-3 text-xs leading-5" data-testid="mission-diff">
      {diff.split("\n").map((line, i) => {
        const cls = line.startsWith("+") && !line.startsWith("+++") ? "text-green-700 dark:text-green-400"
          : line.startsWith("-") && !line.startsWith("---") ? "text-red-700 dark:text-red-400"
          : line.startsWith("@@") ? "text-blue-700 dark:text-blue-400" : "";
        return <div key={i} className={cls}>{line || " "}</div>;
      })}
    </pre>
  );
}
