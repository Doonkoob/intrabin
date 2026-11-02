#!/usr/bin/env bash
# Builds the image (unless SKIP_BUILD=1), starts the 3-node Compose cluster from scratch, runs
# the failover test, and tears everything down (unless KEEP=1).
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
COMPOSE=(docker compose -f "$ROOT/tests/cluster/compose.yaml")
export CONTROL_COMPOSE="${COMPOSE[*]}"
if [ "${SKIP_BUILD:-0}" != 1 ]; then "$ROOT/tests/cluster/build_prebuilt.sh"; fi
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
cleanup() {
  if [ "${KEEP:-0}" != 1 ]; then "${COMPOSE[@]}" logs --no-color > "${LOG_OUT:-/tmp/intrabin-cluster-compose.log}" 2>&1 || true; "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT
"${COMPOSE[@]}" up -d
cat > /tmp/intrabin-compose-control.sh <<EOS
stop_node() { $CONTROL_COMPOSE stop node\$1 >/dev/null; }
start_node() { $CONTROL_COMPOSE start node\$1 >/dev/null; }
EOS
N1=http://127.0.0.1:18081 N2=http://127.0.0.1:18082 N3=http://127.0.0.1:18083 SETUP_TOKEN=compose-cluster-setup-token-0123 \
TARGET_URL=http://target:8080/api/v1/health CONTROL=/tmp/intrabin-compose-control.sh "$ROOT/tests/cluster/cluster_e2e.sh"
