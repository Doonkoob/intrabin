#!/usr/bin/env bash
# Smoke test of the Docker Compose deployment: setup, login, monitor creation,
# real probe results, and persistence across container recreation.
# Usage: tests/smoke/compose_smoke.sh [base_url]   (compose stack must be running)
set -euo pipefail
BASE=${1:-http://127.0.0.1:8081}
JAR=$(mktemp)
trap 'rm -f "$JAR"' EXIT
api() { # method path [json]
  local csrf=""; [[ -f "$JAR.csrf" ]] && csrf=$(cat "$JAR.csrf")
  curl -sf -b "$JAR" -c "$JAR" -H 'Content-Type: application/json' -H "X-CSRF-Token: $csrf" -X "$1" "$BASE/api/v1$2" ${3:+-d "$3"}
}
TOKEN=$(docker compose logs intrabin 2>&1 | grep -o '"setup_token":"[^"]*"' | tail -1 | cut -d'"' -f4)
echo "1. initial setup"
if [[ $(curl -sf "$BASE/api/v1/setup/status" | grep -c '"needs_setup":true') == 1 ]]; then
  api POST /setup "{\"setup_token\":\"$TOKEN\",\"username\":\"admin\",\"password\":\"smoke-test-password\"}" >/dev/null
else
  api POST /auth/login '{"username":"admin","password":"smoke-test-password"}' >/dev/null
fi
api GET /auth/me | sed -E 's/.*"csrf_token":"([^"]+)".*/\1/' > "$JAR.csrf"
NODE1=$(curl -sf "$BASE/api/v1/health" | sed -E 's/.*"node_id":"([^"]+)".*/\1/')
echo "2. create HTTP monitor (30 s interval)"
MID=$(api POST /monitors '{"name":"Self health","type":"http","target":"http://127.0.0.1:8080/api/v1/health","interval_seconds":30,"timeout_seconds":5}' | sed -E 's/.*"monitor":\{"id":"([^"]+)".*/\1/')
echo "   monitor $MID"
echo "3. wait for a real probe result"
for i in $(seq 1 40); do
  N=$(api GET "/monitors/$MID/results" | grep -o '"success":true' | wc -l || true)
  [[ $N -ge 1 ]] && break; sleep 1
done
[[ $N -ge 1 ]] || { echo "no successful probe recorded"; exit 1; }
echo "   ok: $N successful probe(s)"
echo "4. recreate container (volume must persist)"
docker compose down >/dev/null 2>&1
docker compose up -d --no-build >/dev/null 2>&1
for i in $(seq 1 30); do curl -sf "$BASE/api/v1/health" >/dev/null && break; sleep 1; done
NODE2=$(curl -sf "$BASE/api/v1/health" | sed -E 's/.*"node_id":"([^"]+)".*/\1/')
[[ "$NODE1" == "$NODE2" ]] || { echo "node identity changed: $NODE1 -> $NODE2"; exit 1; }
api GET "/monitors/$MID" | grep -q '"Self health"' || { echo "monitor lost after recreation"; exit 1; }
echo "   ok: node identity and monitor persisted; session cookie still valid"
echo "SMOKE TEST PASSED"
