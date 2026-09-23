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
