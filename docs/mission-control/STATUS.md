# Mission Control — status and handoff

Update this file at every checkpoint. Statuses: `pending` / `in_progress` / `verified` / `blocked`.

## Phases

| Phase | Status | Gate evidence |
|---|---|---|
| A. Upstream integration | verified | EVIDENCE.md §A |
| B. Evolution correctness | verified | EVIDENCE.md §B |
| C. Mission vertical slice | verified | EVIDENCE.md §C (incl. adversarial review fixes) |
| D. Durable recovery | verified | EVIDENCE.md §D (incl. adversarial review fixes) |
| E. Enforced boundaries | verified | EVIDENCE.md §E |
| F. Evaluation suite | in_progress | 27 cases, harness + CLI |
| G. Improvement lifecycle | pending | — |
| H. Product completion | pending | — |

## Reporting dimensions (do not set optimistically)

| Dimension | State |
|---|---|
| SOURCE READY | no (in progress) |
| OFFLINE/INTEGRATION VERIFIED | partial: Phases A–E |
| LIVE PROVIDER VERIFIED | BLOCKED: no provider credentials or budget |
| COMMERCIAL LICENSE READY | BLOCKED: CC BY-NC 4.0 upstream |
| PRODUCTION RELEASE APPROVED | no: not requested |

## Environment recipe (dev container)

```bash
nohup dockerd >/tmp/dockerd.log 2>&1 &          # Docker Hub may rate-limit; use mirror.gcr.io
docker run -d --name pgtest -p 5433:5432 -e POSTGRES_PASSWORD=test -e POSTGRES_DB=goclaw_test mirror.gcr.io/pgvector/pgvector:pg18
export GOTOOLCHAIN=auto TEST_DATABASE_URL="postgres://postgres:test@localhost:5433/goclaw_test?sslmode=disable"
go build ./... && go build -tags sqliteonly ./... && go vet ./...
go test ./...                                             # see EVIDENCE.md for known env-only failures
go test -race -tags integration ./tests/invariants/... ./tests/integration/
./scripts/mission-control/upgrade-rehearsal.sh            # v5 -> current, backup/restore, gateway smoke
(cd ui/web && pnpm install --frozen-lockfile && pnpm lint && pnpm build && pnpm test --run)
```

## Handoff

- Branch: `claude/blissful-pascal-jo32ao`.
- Next action: Phase F (run the full offline suite on host and docker, evidence), then Phase G (candidate lifecycle).
