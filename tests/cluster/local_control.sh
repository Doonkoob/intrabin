# stop_node/start_node for three local processes started by tests/cluster/run_local.sh
stop_node() { kill "$(cat "$CL_DIR/n$1.pid")"; sleep 1; }
start_node() { "$CL_DIR/start.sh" "$1"; }
