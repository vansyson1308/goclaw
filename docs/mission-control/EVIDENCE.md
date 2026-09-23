# Evidence index

## §A Upstream integration (2026-09-23)

**Import**
- Fast-forward f3ba434 → 4f808241 (upstream dev).
- Cherry-pick 549c81fd → ee4adc0e (vault dedup; `internal/vault` tests pass).
- Fork `main` untouched. Offline bundle: `fork-main-f3ba434.bundle` (session scratchpad).

**Build and static checks**
- `go build ./...` OK.
- `go build -tags sqliteonly ./...` OK.
- `go vet ./...` OK.
- Upstream `main` 549c81fd fails vet (`recordingBuiltinToolStore` missing `Upsert`). This does not apply to dev.

**Unit tests (`go test ./...`), baseline on the dev import**

| Failure | Classification | Evidence / action |
|---|---|---|
| providers `TestChatStream_MultipleToolCalls_OneTruncated` | **Real bug**, flaky ~1/20 | Tool calls iterated from a map, so order was random. Fixed in 2915dd55 (OpenAI-compat and Codex). New order tests fail without the fix |
| tokencount `TestCountMessages_Cache*`, `TestResetCache` | Environment | Container egress blocks the tiktoken BPE download (`openaipublic.blob.core.windows.net` → 403); fallback encoding is used |
| tools `TestShellAbort_ProcessGroupKilled`, `TestLocalExtractParser_TimeoutReapsProcessGroup` | Environment | The remaining PIDs are `Z <defunct>` with PPID 1. The processes were killed; this VM's PID 1 (`process_api`) does not reap zombies |

**Integration and invariants (PG18 + pgvector, `-race`)**
- `tests/invariants` pass.
- `tests/integration`: all pass except `TestHooksB2_MemoryBombBoundedByTimeout`.
  - That test asserts wall time < 2.5s, but a 1 GiB string doubling runs 3–7s on this 4-core VM.
  - The decision is still `timeout`, so the verdict is correct and only the timing bound is missed.
  - Classified as inherited and timing-sensitive; not changed.

**Web (`ui/web`)**
- `pnpm install --frozen-lockfile` OK.
- `pnpm lint`: 0 errors, 4 warnings.
- `pnpm build` OK.
- vitest pass.

**Upgrade rehearsal (`scripts/mission-control/upgrade-rehearsal.sh`)**
- Path: legacy schema v5 → 97, with a `pg_dump` backup and a restore check.
- Encrypted API key still decrypts with the same `GOCLAW_ENCRYPTION_KEY`.
- Documented destructive steps were observed as expected:
  - 000023 purges soft-deleted agents;
  - 000039 truncates `agent_links`.
- Gateway smoke on the upgraded DB is healthy.
- **Found two real upgrade bugs; both fixed in 453c1f8b with PG and SQLite regression tests:**
  - A NULL `display_name` on `llm_providers` failed the whole provider list, so the gateway ran with zero providers.
  - A NULL `display_name`/`status` on `agents` made the row be skipped silently, so upgraded agents vanished from `/v1/agents`.
- **Also found:** the old fork commit f3ba434 does not build from a clean clone. `.gitignore` has `sandbox`, so `internal/sandbox` was never committed. Git history cannot reproduce the legacy binary.

## §B Evolution correctness (2026-09-23)

### Gate B checks

Normal execution, retry, concurrency and injected failures were each checked for agreement between the API status, the stored state and the visible result.

**PG integration (`-race`, PG18)**

`tests/integration/evolution_apply_rollback_test.go`:
- `tool_order` apply is agent-scoped: another agent in the tenant and `builtin_tool_tenant_configs` are untouched.
- Rollback restores absence exactly and restores a pre-existing list.
- 8 concurrent applies: exactly one wins, the other seven get a conflict, one audit event is written.
- Repeat apply or repeat rollback returns a conflict.
- Rollback refuses to overwrite a newer edit, and the status stays `applied`.
- An injected failure inside the transaction writes nothing: no status change, no config change, no event. A non-allowlisted column is rejected.
- Cross-tenant apply, read and events are denied.
- Legacy `_baseline` rollback: an absent key is deleted; an explicit zero is restored as zero.
- Threshold approval is advisory and changes no config.
- `ListEvolutionAgents` finds agents while a bare-context `List` fails closed. This is a regression test for the cron bug.

`tests/integration/evolution_http_test.go`:
- Per-agent guardrail: 3 calls get 400; 6 calls with `min_data_points=5` succeed.
- A client-supplied `reviewed_by:"mallory"` is ignored; the actor is the authenticated user.
- REST `rolled_back` really restores config; the audit trail is alice → bob.
- A repeat approve gets 409. A rollback on a pending suggestion gets 409.
- `skill_add` with a missing draft releases the claim to `pending` (events: claim, apply_failed). A valid draft reaches `applied` with exactly one skill row. Repeat approve gets 409 with no duplicate skill. Rollback gets 409.

`tests/integration/evolution_reconcile_test.go`: reports a legacy threshold apply, a legacy tenant-wide disable and a stuck claim; ignores a healthy row; modifies no data.

**SQLite (`-tags sqliteonly`, `-race`)**
- Concurrent apply is exactly-once. Rollback conflict works, then rollback succeeds once the edit is removed.
- A non-vacuous cross-tenant test.
- A failed mutate writes nothing.
- In-place upgrade v60 → v61.
- The whole `sqlitestore` package passes. That includes the schema-replay tests, which caught a non-idempotent first version of the v60 patch; it is now idempotent.

**Unit tests**
- `internal/store`: config path presence, explicit zero, pruning, and refusal of non-object intermediates.
- `internal/agent`: dedup across statuses. The test fails with the old "pending-only" logic.
- `internal/http`, skill patch:
  - a version-record failure keeps v1 with no orphan directory;
  - a mark-applied failure is resumed without minting a new version;
  - tampered files are refused;
  - resume never downgrades a newer active version.

  Each of these fails on the pre-change code.
- Web: vitest for `canRollback` and `changeSummary`; `tsc` clean; lint has 0 errors.

**Independent review** (a fresh reviewer agent)
- No high-severity findings.
- Two medium findings were fixed and each now has a test:
  - resume could downgrade the active version;
  - `applying` could get stuck on client disconnect (release now uses a detached context, and reject from `applying` is allowed).
- Low findings fixed:
  - `FOR NO KEY UPDATE` on the agent row;
  - a vacuous SQLite tenant test;
  - `ConfigSetPath` scalar intermediates;
  - audit table added to the tenant backup registry;
  - UI change summary shown only while applied;
  - cooldown for acknowledged advisories.
- Accepted and documented:
  - the default `min_data_points=100` can hold `tool_order` suggestions that have 20–99 calls;
  - for gateway-token callers the actor comes from the `X-GoClaw-User-Id` header, which is the gateway's existing trust model.

**Pre-existing bugs found and fixed along the way**
- The evolution cron never analyzed any agent on PG (bare-context `List`).
- Suggestions would have been re-proposed every 6 hours after review.
- The desktop suggestions list was always empty (it read `.suggestions` from a bare-array response).
- The desktop showed the requested status instead of the server's resulting status.
- The applied config was never cache-invalidated.
