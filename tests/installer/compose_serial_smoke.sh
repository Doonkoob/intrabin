#!/usr/bin/env bash
# Real Compose integration test; runs only against its own temporary project (no host mounts).
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
TASK_DIR=$(mktemp -d)
export COMPOSE_FILE="$TASK_DIR/compose.yaml"
export COMPOSE_PROJECT_NAME="intrabin-serial-test-$$"
export COMPOSE_PROFILES=automation,proxy
source "$ROOT/scripts/compose-serial.sh"
cleanup() { dc --profile '*' down --remove-orphans >/dev/null 2>&1 || true; rm -rf "$TASK_DIR"; }
trap cleanup EXIT
printf 'FROM busybox:1.37\nCOPY token /token\nCMD ["sleep", "120"]\n' >"$TASK_DIR/Dockerfile"
printf 'serial-smoke\n' >"$TASK_DIR/token"
cat >"$COMPOSE_FILE" <<EOF_CONFIG
services:
  intrabin:
    image: ${COMPOSE_PROJECT_NAME}-app
    build: .
  runner:
    profiles: [automation]
    image: ${COMPOSE_PROJECT_NAME}-runner
    build: .
  caddy:
    profiles: [proxy]
    image: busybox:1.37
    command: ["sleep", "120"]
    depends_on: [intrabin]
EOF_CONFIG
compose_serial_images build
compose_serial_up
count=$(dc ps -q --status running | wc -l | tr -d ' ')
[[ "$count" == 3 ]] || { echo "Expected 3 running containers, got $count" >&2; exit 1; }
export COMPOSE_PROFILES=""
[[ "$(dc config --services)" == intrabin ]] || { echo "Disabled profile was enabled" >&2; exit 1; }
echo "Real Compose serial build/start and profile checks passed"
