/** Mission lifecycle statuses (see docs/mission-control/MISSIONS.md). */
export type MissionStatus =
  | "planned" | "preparing" | "running" | "verifying"
  | "succeeded" | "partial" | "failed" | "blocked" | "cancelled";

export const ACTIVE_MISSION_STATUSES: MissionStatus[] = ["planned", "preparing", "running", "verifying"];

/** Evidence for one acceptance criterion. */
export interface CriterionResult {
  id: string;
  kind: string;
  description?: string;
  status: "pass" | "fail" | "error";
  detail?: string;
  command?: string[];
  exit_code?: number;
  duration_ms: number;
  output_tail?: string;
  matched_files?: string[];
  baseline_status?: string;
  /** expect_tests: test name → pass | fail | skip | missing (from go test -json). */
  tests?: Record<string, string>;
  proves_change: boolean;
  executor: string;
  contract_digest: string;
}

export interface Mission {
  id: string;
  owner_id: string;
  agent_key: string;
  title: string;
  contract: Record<string, unknown>;
  contract_digest: string;
  status: MissionStatus;
  status_reason?: string;
  executor?: string;
  base_revision?: string;
  verification?: CriterionResult[] | null;
  diff?: string;
  diff_truncated?: boolean;
  changed_files?: string[] | null;
  summary?: string;
  input_tokens: number;
  output_tokens: number;
  /** null/undefined = unknown (unpriced), never assumed zero. */
  cost_usd?: number | null;
  iterations: number;
  /** An attempt ended without reporting usage: totals are a lower bound. */
  usage_incomplete?: boolean;
  attempt: number;
  max_attempts: number;
  created_at: string;
  started_at?: string | null;
  finished_at?: string | null;
}

/** One tool call of a mission attempt, written before the call ran. */
export interface MissionReceipt {
  attempt: number;
  seq: number;
  tool: string;
  action_class: string;
  /** "started" without a later outcome means the result was never acknowledged. */
  status: "denied" | "started" | "ok" | "error";
  reason?: string;
  args_digest: string;
  duration_ms: number;
  created_at: string;
}

export interface MissionEvent {
  id: string;
  kind: string;
  from_status?: string;
  to_status?: string;
  actor: string;
  message?: string;
  created_at: string;
}
