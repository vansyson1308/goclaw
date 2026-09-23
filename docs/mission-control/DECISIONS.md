# Architecture and process decisions

- **D1 Integration base.** Upstream dev @4f808241 plus the main-only vault fix. v3.14.0 and upstream main are comparison/recovery references. dev passes vet; main does not.
- **D2 Fork safety.** Publishing workflows run only in upstream, or when repo variables `GOCLAW_RELEASES_ENABLED=true` / `GOCLAW_CLAUDE_REVIEW_ENABLED=true` are set. `ci.yaml` is unchanged.
- **D3 Control set.** `docs/mission-control/` holds SCOPE, STATUS, DECISIONS, EVIDENCE, PREFLIGHT and RUNBOOK. It lives in `docs/` because upstream gitignores `plans/`.
- **D4 Known behavior changes inherited from upstream (not reverted):**
  - context pruning is on by default (`cache-ttl`);
  - standalone file mode was removed;
  - `GOCLAW_FEISHU_*` was renamed to `GOCLAW_LARK_*`;
  - migrations 000023 and 000039 delete data.

  These are documented in RUNBOOK.md.
- **D5 Threshold suggestions become advisory.**
  - The only auto-applied change wrote `other_config.retrieval_threshold`, which no runtime code reads.
  - The triggering metric (`used_in_reply = resultCount > 0`) cannot justify raising a threshold: raising it lowers that rate, and evaluation then rolls it back.
  - Rather than retarget an incoherent loop, `threshold` is now review-only and the UI/API say so.
  - Real, measured config improvements come through the eval-gated candidate lifecycle (Phase G).
- **D6 `tool_order` is agent-scoped.** Approval adds the tool to the originating agent's `tools_config.deny` instead of disabling it tenant-wide. Rollback restores the exact prior presence/absence.
- **D7 Apply and rollback are one DB transaction.**
  - The transaction covers the suggestion row lock and status check (CAS), the agent config update, and the baseline/applied/audit records.
  - The baseline records presence as well as value.
  - Rollback refuses (conflict) when the current value no longer equals what was applied, so newer unrelated edits are never overwritten.
- **D8 Audit actor.** The audit actor comes only from authenticated context; client-supplied `reviewed_by` is ignored.
- **D9 Background evolution jobs iterate tenants explicitly.** A bare-context `Agents.List` fails closed and returned nothing, so the cron never analyzed any agent on PG.
- **D10 Skill patch apply order.** Version history is recorded before activation. If recording fails, the skill stays on its current version and the staged directory is removed. If activation or marking the suggestion applied fails, a retry resumes from the recorded version: the immutable files are verified against the recorded hash, then activated, then marked applied. No second version is minted. This replaces upstream's "activate first, keep files" behavior, which could leave an active version with no history row.
- **D11 No automatic metric-driven rollback.** The weekly evaluation job is removed. It never ran on PG because of the D9 bug, and it compared a metric unrelated to the change. Rollback is explicit via the API. Measured, eval-gated observation arrives with the Phase G candidate lifecycle.
- **D12 Reconciliation is report-only.** `goclaw evolution reconcile` lists legacy or inconsistent rows and never repairs data. Legacy threshold applies can be rolled back through the API; tenant-wide tool disables are left for an operator to decide.
