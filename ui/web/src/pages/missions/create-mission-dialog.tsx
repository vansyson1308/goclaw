import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { toast } from "@/stores/use-toast-store";
import type { Mission } from "@/types/mission";

/** Starter contract shown in the editor (matches examples/missions/fix-sum). */
export const CONTRACT_TEMPLATE = JSON.stringify({
  version: 1,
  title: "Fix Sum for negative numbers",
  objective: "Sum ignores negative numbers. Make Sum add every element, and add a regression test that covers negative inputs.",
  constraints: ["Keep the Sum signature unchanged"],
  agent: "mission-coder",
  workspace: { source_dir: "sumrepo" },
  acceptance: [
    { id: "existing-tests", description: "existing tests still pass", kind: "command", command: ["go", "test", "./..."] },
    { id: "behavior", description: "hidden acceptance tests pass", kind: "command", command: ["go", "test", "-run", "TestAcceptance", "./..."], must_change: true, overlay_dir: "sumrepo-acceptance" },
    { id: "regression-test", description: "a regression test was added", kind: "file_changed", glob: "*_test.go" },
  ],
  limits: { max_iterations: 12, timeout_seconds: 600 },
}, null, 2);

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreate: (contract: unknown) => Promise<Mission>;
  onCreated: (m: Mission) => void;
}

export function CreateMissionDialog({ open, onOpenChange, onCreate, onCreated }: Props) {
  const { t } = useTranslation("missions");
  const [text, setText] = useState(CONTRACT_TEMPLATE);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    let contract: unknown;
    try {
      contract = JSON.parse(text);
    } catch (e) {
      setError(t("create.invalidJson", { error: (e as Error).message }));
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const m = await onCreate(contract);
      toast.success(t("create.created"));
      onCreated(m);
      onOpenChange(false);
    } catch (e) {
      // Server-side validation explains exactly what is wrong with the contract.
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t("create.title")}</DialogTitle>
          <DialogDescription>{t("create.hint")}</DialogDescription>
        </DialogHeader>
        <Textarea
          value={text}
          onChange={(e) => setText(e.target.value)}
          className="font-mono text-base md:text-xs min-h-[320px]"
          spellCheck={false}
          aria-label={t("create.contractLabel")}
          data-testid="mission-contract"
        />
        {error && <p className="text-sm text-destructive whitespace-pre-wrap" role="alert">{error}</p>}
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={busy}>{t("common.cancel")}</Button>
          <Button onClick={submit} disabled={busy} data-testid="mission-submit">
            {busy ? t("create.creating") : t("create.submit")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
