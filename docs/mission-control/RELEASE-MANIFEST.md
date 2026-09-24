# Release manifest: Mission Control (candidate, not released)

**Status: release candidate on branch `claude/blissful-pascal-jo32ao`.**
- Nothing has been tagged, published or deployed; the scope did not authorize any of that.
- Draft PR into fork `main`: vansyson1308/goclaw#1.

## Base

| | |
|---|---|
| Upstream | `nextlevelbuilder/goclaw` `dev` @ `4f808241` + cherry-picked `549c81fd` → `ee4adc0e` |
| Fork base | `f3ba434` (fast-forwarded; history preserved, no force-push) |
| Go | 1.26 (`GOTOOLCHAIN=auto`) |
| PostgreSQL | 15+ with pgvector (tested on 18) |

## Changes (branch commits after the upstream import)

| Phase | Commits |
|---|---|
| A: upstream integration | `2915dd55` provider tool-call order · `ead55304` fork-safe CI · `453c1f8b` NULL-tolerant scans · `92221998` control set + upgrade rehearsal |
| B: evolution correctness | `bdd92023` · `a2621031` · `32d75c90` · `9aacedb3` |
| C: missions | `ecf58858` core · `4c951289` API/CLI/E2E · `798e2e34` web UI · `f1acfa6e` example layout + deflake · `f3d7c876` review fixes · `8956bf56` evidence |
| D: durability | `067b3aaf` store (leases, receipts) · `9903cd14` service/guard · `870f9b2f` review fixes |
| E: boundaries | `aac50dab` containers by default |
| F: evaluation | `80ea8fa8` 27-case suite + `goclaw mission eval` |
| G: improvement | `e0ebcb7b` gated lifecycle + `goclaw improve` |
| H: product | journeys, onboarding, this manifest, final report (the commit that adds this file) |

## Schema

| Store | Version | New objects |
|---|---|---|
| PostgreSQL | **100** (`RequiredSchemaVersion`) | 000098 `agent_evolution_events` + apply state · 000099 `missions`, `mission_events` · 000100 attempts/leases/pins on `missions`, `mission_receipts` |
| SQLite (Lite) | **63** | Same tables (v61–v63); idempotent patches |

- All migrations have down migrations.
- New tenant tables are in the backup registry.
- Upgrade path rehearsed from schema v5 (`scripts/mission-control/upgrade-rehearsal.sh`, Phase A).

## Configuration (all opt-in)

| Variable | Default | Meaning |
|---|---|---|
| `GOCLAW_MISSIONS` | off | `1` enables mission create/cancel/execute (reads always work) |
| `GOCLAW_MISSIONS_SOURCE_ROOT` | `<data>/mission-sources` | Only place mission sources and hidden overlays may come from |
| `GOCLAW_MISSIONS_EXECUTOR` | `docker` | `docker` (fails closed) or `host` (trusted repos only) |
| `GOCLAW_MISSIONS_IMAGE` | `golang:1.26-bookworm` | Verifier/agent sandbox image; must be pulled; Debian-based |
| `GOCLAW_MISSIONS_SANDBOX_USER` | gateway uid:gid | Container user |
| `GOCLAW_MISSIONS_MAX_CONCURRENT` | 1 | Missions executing at once per gateway |
| `GOCLAW_MISSIONS_LEASE_SECONDS` | 60 (min 3) | Lease TTL; lost attempts retried after it |
| `GOCLAW_ENABLE_SCRIPTED_PROVIDER` | off | Deterministic offline provider (tests/E2E only) |

## Surfaces

- **HTTP:**
  - `GET/POST /v1/missions`;
  - `GET /v1/missions/{id}`, `/events`, `/receipts`;
  - `POST /v1/missions/{id}/cancel`;
  - evolution endpoints (Phase B).
- **CLI:**
  - `goclaw mission create|list|show|cancel|export-task|eval`;
  - `goclaw improve evaluate|propose|monitor|status`;
  - `goclaw evolution reconcile`.
- **Web:** Missions list, contract editor, and detail page with criteria/per-test evidence, diff, tool calls, attempts, lower-bound cost and events. Strings in en/vi/zh/ko/ru.
- **Desktop (Lite):** no Missions UI (D16).

## Verification summary

Details are in [EVIDENCE.md](EVIDENCE.md).

| Gate | Result |
|---|---|
| Unit tests (`go test ./...`) | Pass, except environment-only failures listed in §A (tiktoken egress blocked; zombie reaping in this VM) |
| Race (`-race`) on changed packages | Pass (same environment exceptions in `internal/tools`) |
| Integration + invariants (PG 18) | Pass; inherited timing test `TestHooksB2_MemoryBombBoundedByTimeout` sometimes exceeds its 2.5s bound on this VM |
| SQLite store incl. schema replay | Pass |
| Web lint/build/vitest | 0 errors / OK / 363 tests pass |
| Missions E2E (real gateway + PG + agent loop + Docker + Playwright) | Pass (7 missions + learning step + UI; §C–§H) |
| Offline evaluation suite | 27/27 on host and on Docker, 0 false successes (§F) |
| Improvement lifecycle | Reject ×3, promote, rollback, promote, as designed (§G) |
| GitHub Actions on PR #1 (`go` incl. race + integration, `web`, `release-versioning`) | Pass at `e0ebcb7b` (§H) |
| Independent adversarial reviews | Phase C: 3 high / 3 medium, all fixed · Phase D: 2 high / 4 medium, all fixed (§C, §D) |

## Known limits

Stated in MISSIONS.md "Known limits", "Threat model" and EVALS.md:
- Command verifiers execute agent-written code. A determined agent could still forge output by discovering hidden test names at runtime.
- The host executor is not a boundary.
- Container escapes are out of scope.
- Some model calls outside the agent loop are not counted against `max_tokens`.
- There is no automatic retention of mission directories.
- Mixed pre-/post-Phase-D gateways on one database are not supported.

## Release dimensions

| Dimension | State |
|---|---|
| SOURCE READY | **Yes** for review: feature-complete for A–H on this branch, documented, tests listed above |
| OFFLINE/INTEGRATION VERIFIED | **Yes**, with the environment-only exceptions listed |
| LIVE PROVIDER VERIFIED | **Ready to run, not yet run**: DeepSeek V4.1 Flash integrated; wire contract verified against a local mock only. A live run needs the owner's key (`scripts/mission-control/live-deepseek.sh`, see DEEPSEEK.md) |
| COMMERCIAL LICENSE READY | **BLOCKED**: upstream is CC BY-NC 4.0 (non-commercial). Not relicensed; commercial use needs independently resolved licensing |
| PRODUCTION RELEASE APPROVED | **No**: not requested; no tag, publish or deploy was performed |
