#!/usr/bin/env bash
# End-to-end mission check against a real gateway, real PostgreSQL and the
# real agent loop, with the deterministic scripted provider (no LLM spend).
#
#   1. fix-sum with a correct scripted agent        -> must be SUCCEEDED
#   2. same contract with a "false claim" agent     -> must be FAILED
#   3. cancel of a queued/running mission           -> must be CANCELLED
#
# Uses a THROWAWAY database (name must contain "e2e"). Requirements: docker
# PG container (PG_CONTAINER, default pgtest on :5433), go, git, curl, jq.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
EX="$ROOT/examples/missions/fix-sum"
PG_CONTAINER="${PG_CONTAINER:-pgtest}"
DB="${E2E_DB:-goclaw_e2e}"
PORT="${E2E_PORT:-18992}"
TOKEN="e2e-token"
WORK="${E2E_WORK:-$(mktemp -d)}"
case "$DB" in *e2e*) ;; *) echo "refusing: DB name must contain 'e2e'" >&2; exit 2;; esac

fail() { echo "E2E FAIL: $*" >&2; [ -f "$WORK/gateway.log" ] && tail -40 "$WORK/gateway.log" >&2; exit 1; }
step() { echo "== $*"; }
api() { curl -fsS -H "Authorization: Bearer $TOKEN" -H "X-GoClaw-User-Id: operator" -H 'Content-Type: application/json' "$@"; }

step "build"
( cd "$ROOT" && go build -o "$WORK/goclaw" . )

step "fresh database $DB"
docker exec -i "$PG_CONTAINER" psql -U postgres -qc "DROP DATABASE IF EXISTS $DB WITH (FORCE)" -c "CREATE DATABASE $DB" >/dev/null
export GOCLAW_POSTGRES_DSN="postgres://postgres:test@localhost:5433/$DB?sslmode=disable"
GOCLAW_MIGRATIONS_DIR="$ROOT/migrations" "$WORK/goclaw" migrate up >/dev/null 2>&1 || fail "migrate"

step "start gateway (missions + scripted provider enabled)"
mkdir -p "$WORK/home" "$WORK/data"
start_gateway() {
  HOME="$WORK/home" GOCLAW_CONFIG="$WORK/config.json" GOCLAW_DATA_DIR="$WORK/data" \
  GOCLAW_GATEWAY_TOKEN="$TOKEN" GOCLAW_PORT="$PORT" \
  GOCLAW_ENCRYPTION_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  GOCLAW_MISSIONS=1 GOCLAW_MISSIONS_SOURCE_ROOT="$EX/testdata" GOCLAW_ENABLE_SCRIPTED_PROVIDER=1 \
  GOCLAW_MISSIONS_LEASE_SECONDS=4 GOCLAW_OWNER_IDS=operator \
    "$WORK/goclaw" >> "$WORK/gateway.log" 2>&1 &
  GW=$!
  for _ in $(seq 1 60); do curl -fsS "localhost:$PORT/health" >/dev/null 2>&1 && break; sleep 1; done
  curl -fsS "localhost:$PORT/health" >/dev/null || fail "gateway not healthy"
}
start_gateway
trap 'kill $GW 2>/dev/null || true' EXIT
export GOCLAW_SERVER="http://localhost:$PORT" GOCLAW_GATEWAY_TOKEN="$TOKEN"

step "register scripted providers + agents"
for p in fix false-claim slow slow-fix; do
  jq -n --arg name "scripted-$p" --slurpfile s "$EX/scripts/scripted-$p.json" \
    '{name:$name, display_name:$name, provider_type:"scripted", enabled:true, settings:$s[0]}' |
    api -X POST "localhost:$PORT/v1/providers" -d @- >/dev/null || fail "create provider scripted-$p"
done
api -X POST "localhost:$PORT/v1/agents" -d '{"agent_key":"mission-coder","display_name":"Mission Coder","provider":"scripted-fix","model":"scripted-fix-sum","summon":false}' >/dev/null || fail "create agent"
api -X POST "localhost:$PORT/v1/agents" -d '{"agent_key":"mission-liar","display_name":"Mission Liar","provider":"scripted-false-claim","model":"scripted-false-claim","summon":false}' >/dev/null || fail "create liar agent"
api -X POST "localhost:$PORT/v1/agents" -d '{"agent_key":"mission-slow","display_name":"Mission Slow","provider":"scripted-slow","model":"scripted-slow","summon":false}' >/dev/null || fail "create slow agent"
api -X POST "localhost:$PORT/v1/agents" -d '{"agent_key":"mission-slowfix","display_name":"Mission Slow Fix","provider":"scripted-slow-fix","model":"scripted-slow-fix","summon":false}' >/dev/null || fail "create slow-fix agent"

mission_id() { jq -r .id; }
wait_final() {
  for _ in $(seq 1 240); do
    s=$(api "localhost:$PORT/v1/missions/$1" | jq -r .status)
    case "$s" in planned|preparing|running|verifying) sleep 1;; *) echo "$s"; return;; esac
  done
  echo timeout
}

step "mission 1: correct agent must SUCCEED with evidence"
M1=$(api -X POST "localhost:$PORT/v1/missions" -d @"$EX/contract.json" | mission_id)
S1=$(wait_final "$M1")
api "localhost:$PORT/v1/missions/$M1" > "$WORK/m1.json"
[ "$S1" = succeeded ] || { jq '{status,status_reason,verification}' "$WORK/m1.json"; fail "mission 1 status $S1"; }
jq -e '[.verification[] | select(.status=="pass")] | length == 3' "$WORK/m1.json" >/dev/null || fail "not all criteria passed"
jq -e '.verification[] | select(.id=="behavior") | .baseline_status == "fail"' "$WORK/m1.json" >/dev/null || fail "behavior baseline not recorded as fail"
jq -e '.verification[] | select(.id=="behavior") | .tests.TestAcceptanceSumIncludesNegatives == "pass"' "$WORK/m1.json" >/dev/null || fail "hidden test did not report an explicit pass"
jq -e '(.pins.source | startswith("sha256:")) and (.pins.overlays.behavior | startswith("sha256:"))' "$WORK/m1.json" >/dev/null || fail "inputs not pinned"
jq -e '[.verification[] | select(.id | startswith("_"))] | length == 0' "$WORK/m1.json" >/dev/null || fail "unexpected internal finding"
jq -e '.attempt == 1 and .max_attempts == 2 and (.usage_incomplete | not)' "$WORK/m1.json" >/dev/null || fail "attempt bookkeeping"
api "localhost:$PORT/v1/missions/$M1/receipts" > "$WORK/m1-receipts.json"
jq -e 'length >= 3 and all(.[]; .status == "ok" and .attempt == 1) and ([.[].tool] | index("edit") != null and index("write_file") != null)' "$WORK/m1-receipts.json" >/dev/null || { cat "$WORK/m1-receipts.json"; fail "tool receipts"; }
jq -e '.changed_files | index("sum.go") != null and index("sum_negative_test.go") != null' "$WORK/m1.json" >/dev/null || fail "changed files"
jq -e '.diff | contains("-\t\tif x > 0 {")' "$WORK/m1.json" >/dev/null || fail "diff lacks the fix"
jq -e '(.changed_files | index("zz_acceptance_test.go")) == null' "$WORK/m1.json" >/dev/null || fail "hidden test leaked"
grep -q 'if x > 0' "$EX/testdata/sumrepo/sum.go" || fail "source repository was modified"
"$WORK/goclaw" mission show "$M1" > "$WORK/m1.txt"
grep -q 'SUCCEEDED' "$WORK/m1.txt" || fail "CLI show"
echo "   succeeded: $(jq -r .status_reason "$WORK/m1.json")"

step "mission 2: false claim of success must FAIL"
jq '.agent="mission-liar"' "$EX/contract.json" > "$WORK/liar.json"
M2=$(api -X POST "localhost:$PORT/v1/missions" -d @"$WORK/liar.json" | mission_id)
S2=$(wait_final "$M2")
api "localhost:$PORT/v1/missions/$M2" > "$WORK/m2.json"
[ "$S2" = failed ] || { jq '{status,status_reason,summary}' "$WORK/m2.json"; fail "false claim got status $S2"; }
jq -e '.summary | test("All done")' "$WORK/m2.json" >/dev/null || fail "agent narrative not kept"
echo "   failed as expected: $(jq -r .status_reason "$WORK/m2.json")"

step "mission 3: cancel"
M3=$(api -X POST "localhost:$PORT/v1/missions" -d @"$EX/contract.json" | mission_id)
api -X POST "localhost:$PORT/v1/missions/$M3/cancel" -d '{}' >/dev/null || fail "cancel"
S3=$(wait_final "$M3")
[ "$S3" = cancelled ] || fail "cancel status $S3"
if api -X POST "localhost:$PORT/v1/missions/$M3/cancel" -d '{}' >/dev/null 2>&1; then fail "second cancel should be 409"; fi

# Processes running a command whose line contains $1, excluding zombies
# (this container's PID 1 does not reap them) and this shell's own greps.
live_procs() { ps -eo stat=,args= | awk -v pat="$1" '$1 !~ /^Z/ && index($0, pat) && !/awk/' ; }

step "mission 4: cancel stops a running tool process"
jq '.agent="mission-slow"' "$EX/contract.json" > "$WORK/slow.json"
M4=$(api -X POST "localhost:$PORT/v1/missions" -d @"$WORK/slow.json" | mission_id)
for _ in $(seq 1 120); do [ -n "$(live_procs 'sleep 97')" ] && break; sleep 0.5; done
[ -n "$(live_procs 'sleep 97')" ] || fail "slow tool never started"
api -X POST "localhost:$PORT/v1/missions/$M4/cancel" -d '{}' >/dev/null || fail "cancel slow mission"
for _ in $(seq 1 20); do [ -z "$(live_procs 'sleep 97')" ] && break; sleep 0.5; done
[ -z "$(live_procs 'sleep 97')" ] || fail "tool process still running 10s after cancel: $(live_procs 'sleep 97')"
[ "$(wait_final "$M4")" = cancelled ] || fail "slow mission not cancelled"
api "localhost:$PORT/v1/missions/$M4/receipts" | jq -e 'length == 1 and .[0].tool == "exec"' >/dev/null || fail "slow mission receipt"
echo "   running tool process stopped by cancel"

step "mission 5: gateway killed mid-run -> retried after restart"
jq '.agent="mission-slowfix"' "$EX/contract.json" > "$WORK/slowfix.json"
M5=$(api -X POST "localhost:$PORT/v1/missions" -d @"$WORK/slowfix.json" | mission_id)
for _ in $(seq 1 120); do
  api "localhost:$PORT/v1/missions/$M5/receipts" | jq -e 'any(.[]; .tool == "exec")' >/dev/null 2>&1 && break; sleep 0.5
done
kill -9 "$GW"; wait "$GW" 2>/dev/null || true
echo "   gateway killed (SIGKILL) while attempt 1 was running a tool"
start_gateway
S5=$(wait_final "$M5")
api "localhost:$PORT/v1/missions/$M5" > "$WORK/m5.json"
api "localhost:$PORT/v1/missions/$M5/receipts" > "$WORK/m5-receipts.json"
api "localhost:$PORT/v1/missions/$M5/events" > "$WORK/m5-events.json"
[ "$S5" = succeeded ] || { jq '{status,status_reason,attempt}' "$WORK/m5.json"; fail "restarted mission status $S5"; }
jq -e '.attempt == 2 and .usage_incomplete == true' "$WORK/m5.json" >/dev/null || fail "attempt 2 / incomplete usage not recorded"
jq -e 'any(.[]; .attempt == 1 and .tool == "exec" and .status == "started")' "$WORK/m5-receipts.json" >/dev/null || fail "unacknowledged attempt-1 call not visible"
jq -e '[.[] | select(.attempt == 2)] | length >= 4 and all(.[]; .status == "ok")' "$WORK/m5-receipts.json" >/dev/null || fail "attempt 2 receipts"
jq -e 'any(.[]; (.message // "") | test("attempt 1/2 was interrupted"))' "$WORK/m5-events.json" >/dev/null || fail "interruption not in audit trail"
echo "   $(jq -r .status_reason "$WORK/m5.json")"

step "audit trail"
api "localhost:$PORT/v1/missions/$M1/events" | jq -e '[.[] | .to_status] | index("succeeded") != null and index("verifying") != null' >/dev/null || fail "events"

if [ "${UI_CHECK:-0}" = 1 ]; then
  step "UI check (Vite dev server + Playwright)"
  UI_PORT="${UI_PORT:-5199}"
  ( cd "$ROOT/ui/web" && VITE_BACKEND_PORT="$PORT" exec setsid pnpm exec vite --port "$UI_PORT" --strictPort > "$WORK/vite.log" 2>&1 ) &
  VITE=$!
  trap 'kill $GW 2>/dev/null; kill -- -$VITE 2>/dev/null; fuser -k "$UI_PORT/tcp" >/dev/null 2>&1 || true' EXIT
  for _ in $(seq 1 60); do curl -fsS "localhost:$UI_PORT" >/dev/null 2>&1 && break; sleep 1; done
  node "$ROOT/scripts/mission-control/ui-missions.mjs" "http://localhost:$UI_PORT" "$TOKEN" "$WORK" || fail "UI check"
fi

echo "E2E PASS (work dir: $WORK)"
