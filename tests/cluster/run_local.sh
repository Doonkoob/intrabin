#!/usr/bin/env bash
# Runs the cluster e2e test against three local processes (no Docker). Requires bin/intrabin.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
export CL_DIR=${CL_DIR:-$(mktemp -d)}
BIN=${BIN:-$ROOT/bin/intrabin}
cat > "$CL_DIR/start.sh" <<EOS
#!/usr/bin/env bash
i=\$1
mkdir -p $CL_DIR/n\$i
INTRABIN_DATA_DIR=$CL_DIR/n\$i INTRABIN_LISTEN=127.0.0.1:1808\$i INTRABIN_CLUSTER_LISTEN=127.0.0.1:1744\$i \
INTRABIN_CLUSTER_ADVERTISE=127.0.0.1:1744\$i INTRABIN_PUBLIC_URL=http://127.0.0.1:1808\$i INTRABIN_NODE_NAME=node\$i \
INTRABIN_LOG_FORMAT=text INTRABIN_SETUP_TOKEN=local-cluster-setup-token-0123 INTRABIN_MIN_INTERVAL=5s \
nohup $BIN serve >> $CL_DIR/n\$i.log 2>&1 &
echo \$! > $CL_DIR/n\$i.pid
EOS
chmod +x "$CL_DIR/start.sh"
python3 -m http.server 18099 --bind 127.0.0.1 --directory "$CL_DIR" >/dev/null 2>&1 &
TPID=$!
cleanup() { for i in 1 2 3; do [ -f "$CL_DIR/n$i.pid" ] && kill "$(cat "$CL_DIR/n$i.pid")" 2>/dev/null || true; done; kill $TPID 2>/dev/null || true; }
trap cleanup EXIT
for i in 1 2 3; do "$CL_DIR/start.sh" $i; done
N1=http://127.0.0.1:18081 N2=http://127.0.0.1:18082 N3=http://127.0.0.1:18083 SETUP_TOKEN=local-cluster-setup-token-0123 \
TARGET_URL=http://127.0.0.1:18099/ CONTROL=$ROOT/tests/cluster/local_control.sh "$ROOT/tests/cluster/cluster_e2e.sh"
echo "logs: $CL_DIR"
