#!/usr/bin/env bash
# One-step DeepSeek setup for a running GoClaw gateway.
#
#   scripts/deepseek-setup.sh                      # asks for the API key (hidden)
#   DEEPSEEK_API_KEY=sk-... scripts/deepseek-setup.sh
#
# What it does (idempotent — safe to run again, e.g. to rotate the key):
#   1. creates or updates the provider "deepseek" (type deepseek, key encrypted
#      at rest by the gateway),
#   2. verifies the key with one real call to the model (a few tokens),
#   3. loads prices for deepseek-flash and deepseek-v4-pro so costs and mission
#      cost limits work (DeepSeek's peak rates: an upper bound off-peak),
#   4. creates the agent "deepseek" on deepseek-flash, ready to chat.
#
# Environment:
#   GOCLAW_SERVER         gateway URL (default http://localhost:18790)
#   GOCLAW_GATEWAY_TOKEN  gateway token (read from .env.local if present)
#   DEEPSEEK_API_KEY      the key (asked for when unset)
#   DEEPSEEK_MODEL        default model (default deepseek-flash)
#   DEEPSEEK_BASE_URL     API base (default https://api.deepseek.com)
#   DEEPSEEK_AGENT_KEY    agent to create ("" to skip; default deepseek)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
if [ -z "${GOCLAW_GATEWAY_TOKEN:-}" ] && [ -f "$ROOT/.env.local" ]; then
  # shellcheck disable=SC1091
  set -a; . "$ROOT/.env.local"; set +a
fi
SERVER="${GOCLAW_SERVER:-http://localhost:18790}"
case "$SERVER" in http://*|https://*) ;; *) SERVER="http://$SERVER";; esac
TOKEN="${GOCLAW_GATEWAY_TOKEN:?set GOCLAW_GATEWAY_TOKEN (or run from the repo with .env.local)}"
MODEL="${DEEPSEEK_MODEL:-deepseek-flash}"
BASE="${DEEPSEEK_BASE_URL:-https://api.deepseek.com}"
AGENT="${DEEPSEEK_AGENT_KEY-deepseek}"
NAME="deepseek"

for bin in curl jq; do command -v "$bin" >/dev/null || { echo "missing: $bin" >&2; exit 2; }; done
die() { echo "deepseek-setup: $*" >&2; exit 1; }
api() { curl -fsS -H "Authorization: Bearer $TOKEN" -H "X-GoClaw-User-Id: ${GOCLAW_USER_ID:-operator}" -H 'Content-Type: application/json' "$@"; }

curl -fsS "$SERVER/health" >/dev/null 2>&1 || die "gateway not reachable at $SERVER (start it, or set GOCLAW_SERVER)"

KEY="${DEEPSEEK_API_KEY:-}"
if [ -z "$KEY" ]; then
  [ -t 0 ] || die "DEEPSEEK_API_KEY is not set and there is no terminal to ask for it"
  read -rsp "DeepSeek API key: " KEY; echo
fi
KEY="$(printf '%s' "$KEY" | tr -d '[:space:]')"
[ -n "$KEY" ] || die "empty API key"

echo "== provider $NAME ($BASE)"
PID="$(api "$SERVER/v1/providers" | jq -r --arg n "$NAME" '.providers[] | select(.name == $n) | .id' | head -1)"
# The key goes through stdin only (never on a command line).
BODY() { printf '%s' "$KEY" | jq -Rs --arg n "$NAME" --arg b "$BASE" \
  '{name: $n, display_name: "DeepSeek", provider_type: "deepseek", api_base: $b, api_key: ., enabled: true}'; }
if [ -n "$PID" ]; then
  BODY | api -X PUT "$SERVER/v1/providers/$PID" -d @- >/dev/null || die "update provider failed"
  echo "   updated $PID"
else
  PID="$(BODY | api -X POST "$SERVER/v1/providers" -d @- | jq -r .id)" || die "create provider failed"
  [ -n "$PID" ] && [ "$PID" != null ] || die "create provider returned no id"
  echo "   created $PID"
fi

echo "== verify key with $MODEL"
V="$(jq -n --arg m "$MODEL" '{model: $m}' | api -X POST "$SERVER/v1/providers/$PID/verify" -d @-)" || die "verify request failed"
if [ "$(jq -r .valid <<<"$V")" != true ]; then
  die "DeepSeek rejected the call: $(jq -r '.error // .' <<<"$V")"
fi
echo "   ok"

echo "== prices (USD per token; DeepSeek peak rates, an upper bound off-peak)"
price() { # model input output cache_read
  jq -n --arg p "$PID" --arg m "$1" --arg i "$2" --arg o "$3" --arg c "$4" \
    '{provider_id: $p, provider_type: "deepseek", model_id: $m, enabled: true,
      pricing: {input: $i, output: $o, cache_read: $c}}' |
    api -X PUT "$SERVER/v1/model-pricing/overrides" -d @- >/dev/null || die "pricing for $1 failed"
  echo "   $1: input \$$(awk "BEGIN{print $2*1e6}")/M, output \$$(awk "BEGIN{print $3*1e6}")/M, cache hit \$$(awk "BEGIN{print $4*1e6}")/M"
}
# DeepSeek V4.1 Flash / V4 Pro list prices (September 2026, peak hours).
price deepseek-flash  0.0000003  0.0000012  0.000000006
price deepseek-v4-pro 0.00000132 0.00000396 0.00000132

if [ -n "$AGENT" ]; then
  echo "== agent $AGENT"
  if api "$SERVER/v1/agents" | jq -e --arg a "$AGENT" '[.agents[]? // .[]? | select(.agent_key == $a)] | length > 0' >/dev/null 2>&1; then
    echo "   exists (not changed)"
  else
    jq -n --arg a "$AGENT" --arg p "$NAME" --arg m "$MODEL" \
      '{agent_key: $a, display_name: "DeepSeek", provider: $p, model: $m, context_window: 1000000}' |
      api -X POST "$SERVER/v1/agents" -d @- >/dev/null || die "create agent failed"
    echo "   created on $MODEL"
  fi
fi

echo "DeepSeek is ready: provider '$NAME'${AGENT:+, agent '$AGENT'} on $MODEL."
