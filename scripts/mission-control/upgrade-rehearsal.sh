#!/usr/bin/env bash
# Upgrade rehearsal: old fork schema (v5, Feb 2026) -> current schema, on a
# THROWAWAY database. Never point this at a real database.
#
# Steps: create DB -> migrate to v5 -> seed legacy-shaped data (incl. an
# AES-GCM encrypted API key) -> pg_dump backup -> migrate up -> verify ->
# restore the dump into a second DB -> verify the restore matches pre-upgrade.
#
# Requirements: a PostgreSQL+pgvector server reachable via PG_ADMIN_DSN
# (default: the docker test container on :5433), psql + pg_dump + pg_restore
# either locally or inside the container named by PG_CONTAINER.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
PG_CONTAINER="${PG_CONTAINER:-pgtest}"
PG_HOSTPORT="${PG_HOSTPORT:-localhost:5433}"
PG_USER="${PG_USER:-postgres}"
PG_PASS="${PG_PASS:-test}"
DB="${REHEARSAL_DB:-goclaw_rehearsal}"
RESTORE_DB="${DB}_restore"
WORK="${REHEARSAL_WORK:-$(mktemp -d)}"
export GOCLAW_ENCRYPTION_KEY="${GOCLAW_ENCRYPTION_KEY:-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}"

case "$DB" in *rehearsal*) ;; *) echo "refusing: DB name must contain 'rehearsal'" >&2; exit 2;; esac

psql_c() { docker exec -i "$PG_CONTAINER" psql -v ON_ERROR_STOP=1 -U "$PG_USER" -qAt "$@"; }
dsn() { echo "postgres://$PG_USER:$PG_PASS@$PG_HOSTPORT/$1?sslmode=disable"; }
fail() { echo "REHEARSAL FAIL: $*" >&2; exit 1; }
step() { echo "== $*"; }

step "build binaries"
( cd "$ROOT" && go build -o "$WORK/goclaw" . && go build -o "$WORK/cryptocheck" ./scripts/mission-control/cryptocheck )
export GOCLAW_MIGRATIONS_DIR="$ROOT/migrations"

step "fresh database $DB at legacy schema v5"
psql_c -d postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" -c "DROP DATABASE IF EXISTS $RESTORE_DB WITH (FORCE)" -c "CREATE DATABASE $DB"
GOCLAW_POSTGRES_DSN="$(dsn "$DB")" "$WORK/goclaw" migrate goto 5 >/dev/null 2>&1
[ "$(psql_c -d "$DB" -c 'select version from schema_migrations')" = "5" ] || fail "not at v5"

step "seed legacy data"
SECRET="sk-rehearsal-$(date +%s)"
ENC="$("$WORK/cryptocheck" encrypt "$SECRET")"
psql_c -d "$DB" <<SQL
INSERT INTO llm_providers (name, provider_type, api_base, api_key) VALUES ('legacy-openai','openai_compat','https://example.invalid/v1','$ENC');
INSERT INTO agents (id, agent_key, owner_id, provider, model, agent_type) VALUES
 ('00000000-0000-7000-8000-000000000001','alpha','owner-1','legacy-openai','gpt-x','open'),
 ('00000000-0000-7000-8000-000000000002','beta','owner-1','legacy-openai','gpt-x','predefined');
INSERT INTO agents (id, agent_key, owner_id, provider, model, deleted_at) VALUES
 ('00000000-0000-7000-8000-000000000003','gone','owner-1','legacy-openai','gpt-x', now());
INSERT INTO agent_links (source_agent_id, target_agent_id, created_by) VALUES
 ('00000000-0000-7000-8000-000000000001','00000000-0000-7000-8000-000000000002','owner-1');
INSERT INTO agent_context_files (agent_id, file_name, content) VALUES
 ('00000000-0000-7000-8000-000000000001','SOUL.md','legacy soul');
INSERT INTO sessions (session_key, agent_id, user_id, messages) VALUES
 ('agent:alpha:ws:direct:u1','00000000-0000-7000-8000-000000000001','u1','[{"role":"user","content":"hello from v5"}]');
INSERT INTO cron_jobs (agent_id, user_id, name, schedule_kind, cron_expression, payload) VALUES
 ('00000000-0000-7000-8000-000000000001','u1','legacy-job','cron','0 9 * * *','{"message":"ping"}');
SQL
PRE="$(psql_c -d "$DB" -c "select (select count(*) from agents)||','||(select count(*) from agent_links)||','||(select count(*) from sessions)||','||(select count(*) from cron_jobs)||','||(select count(*) from agent_context_files)")"
echo "pre-upgrade counts agents,links,sessions,cron,ctx = $PRE"

step "backup (pg_dump -Fc)"
docker exec "$PG_CONTAINER" pg_dump -U "$PG_USER" -Fc -f /tmp/rehearsal.dump "$DB"

step "migrate up"
GOCLAW_POSTGRES_DSN="$(dsn "$DB")" "$WORK/goclaw" migrate up > "$WORK/migrate.log" 2>&1 || { tail -20 "$WORK/migrate.log"; fail "migrate up"; }
REQ="$(grep -o 'RequiredSchemaVersion uint = [0-9]*' "$ROOT/internal/upgrade/version.go" | grep -o '[0-9]*$')"
GOT="$(psql_c -d "$DB" -c 'select version||'"'"'/'"'"'||dirty from schema_migrations')"
[ "$GOT" = "$REQ/false" ] || fail "schema version $GOT, want $REQ/false"
echo "schema at $GOT"

step "verify upgraded data"
# Known, documented upstream behavior: 000023 hard-deletes soft-deleted agents,
# 000039 truncates agent_links. Everything else must survive.
[ "$(psql_c -d "$DB" -c "select count(*) from agents where agent_key in ('alpha','beta')")" = "2" ] || fail "live agents lost"
[ "$(psql_c -d "$DB" -c "select count(*) from agents where agent_key='gone'")" = "0" ] || fail "expected soft-deleted agent purged by 000023"
[ "$(psql_c -d "$DB" -c "select count(*) from agent_links")" = "0" ] || fail "expected agent_links truncated by 000039"
[ "$(psql_c -d "$DB" -c "select count(*) from sessions where session_key='agent:alpha:ws:direct:u1'")" = "1" ] || fail "session lost"
[ "$(psql_c -d "$DB" -c "select count(*) from cron_jobs where name='legacy-job'")" = "1" ] || fail "cron job lost"
[ "$(psql_c -d "$DB" -c "select content from agent_context_files where file_name='SOUL.md'")" = "legacy soul" ] || fail "context file lost"
[ "$(psql_c -d "$DB" -c "select count(*) from agents where agent_key in ('alpha','beta') and tenant_id is not null")" = "2" ] || fail "agents not assigned to a tenant"
ENC_AFTER="$(psql_c -d "$DB" -c "select api_key from llm_providers where name='legacy-openai'")"
[ "$("$WORK/cryptocheck" decrypt "$ENC_AFTER")" = "$SECRET" ] || fail "API key not decryptable after upgrade"
echo "data verified; encrypted API key still decrypts with the same GOCLAW_ENCRYPTION_KEY"

step "gateway smoke on the upgraded database"
PORT="${REHEARSAL_PORT:-18991}"
mkdir -p "$WORK/home"
HOME="$WORK/home" GOCLAW_CONFIG="$WORK/config.json" GOCLAW_POSTGRES_DSN="$(dsn "$DB")" \
  GOCLAW_GATEWAY_TOKEN=rehearsal-token GOCLAW_PORT="$PORT" "$WORK/goclaw" > "$WORK/gateway.log" 2>&1 &
GW_PID=$!
trap 'kill $GW_PID 2>/dev/null || true' EXIT
for _ in $(seq 1 60); do curl -fsS "localhost:$PORT/health" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "localhost:$PORT/health" >/dev/null || { tail -30 "$WORK/gateway.log"; fail "gateway did not become healthy"; }
AGENTS="$(curl -fsS -H 'Authorization: Bearer rehearsal-token' "localhost:$PORT/v1/agents" | grep -o '"agent_key":"[a-z]*"' | sort | tr '\n' ' ')"
[ "$AGENTS" = '"agent_key":"alpha" "agent_key":"beta" ' ] || fail "gateway agent list after upgrade: [$AGENTS]"
curl -fsS -H 'Authorization: Bearer rehearsal-token' "localhost:$PORT/v1/providers" | grep -q '"name":"legacy-openai"' || fail "legacy provider not loaded"
if grep -qE 'Scan error|agents.scan_row_skipped' "$WORK/gateway.log"; then fail "row scan errors in gateway log"; fi
kill $GW_PID; wait $GW_PID 2>/dev/null || true
echo "gateway healthy; legacy agents and provider visible"

step "restore backup into $RESTORE_DB"
psql_c -d postgres -c "CREATE DATABASE $RESTORE_DB"
docker exec "$PG_CONTAINER" pg_restore -U "$PG_USER" -d "$RESTORE_DB" /tmp/rehearsal.dump
POST_RESTORE="$(psql_c -d "$RESTORE_DB" -c "select (select count(*) from agents)||','||(select count(*) from agent_links)||','||(select count(*) from sessions)||','||(select count(*) from cron_jobs)||','||(select count(*) from agent_context_files)")"
[ "$POST_RESTORE" = "$PRE" ] || fail "restore mismatch: $POST_RESTORE vs $PRE"
[ "$(psql_c -d "$RESTORE_DB" -c 'select version from schema_migrations')" = "5" ] || fail "restore not at v5"
echo "restore matches pre-upgrade snapshot ($POST_RESTORE) at schema v5"

echo "REHEARSAL PASS (work dir: $WORK)"
