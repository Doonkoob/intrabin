#!/usr/bin/env bash
# End-to-end cluster test against three running Intrabin nodes.
#
# Required environment:
#   N1 N2 N3            public API base URLs of the three nodes (e.g. http://127.0.0.1:18081)
#   SETUP_TOKEN         setup token configured on all three (fresh) nodes
#   TARGET_URL          an HTTP URL every node can reach (monitored during the test)
#   stop_node / start_node   shell functions (sourced from $CONTROL) that stop/start node N (1..3)
# Optional:
#   CONTROL             file defining stop_node/start_node
#   WITH_RUNNER=1       all nodes share an Action Runner: also test a script action that is in
#                       flight while the leader is killed (it must be recovered, not re-run)
#
# The test: setup on node 1, found a cluster, join nodes 2 and 3 through the setup/join API,
# promote both to voters, create a monitor, verify multi-node probing and follower behaviour,
# kill the leader, verify failover and continued evaluation, restart the old leader and verify it
# rejoins as a follower, then verify that a single surviving node refuses writes (NO_QUORUM).
set -euo pipefail
: "${N1:?}" "${N2:?}" "${N3:?}" "${SETUP_TOKEN:?}" "${TARGET_URL:?}"
[ -n "${CONTROL:-}" ] && source "$CONTROL"
PW='Cluster-Test-Password-1'
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
URLS=("" "$N1" "$N2" "$N3")

log() { printf '\033[1;34m[cluster-e2e]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[cluster-e2e] FAIL:\033[0m %s\n' "$*"; exit 1; }

# api <node> <method> <path> [json] → prints body, sets $CODE
api() {
  local n=$1 m=$2 p=$3 d=${4:-}
  local jar="$TMP/jar$n" csrf=""
  [ -f "$TMP/csrf$n" ] && csrf=$(cat "$TMP/csrf$n")
  local args=(-s -o "$TMP/body" -w '%{http_code}' -X "$m" -b "$jar" -c "$jar" -H "Origin: ${URLS[$n]}" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json')
  [ -n "$d" ] && args+=(--data "$d")
  CODE=$(curl "${args[@]}" "${URLS[$n]}$p" || echo 000)
  cat "$TMP/body" 2>/dev/null || true
}

login() {
  local n=$1
  rm -f "$TMP/jar$n" "$TMP/csrf$n"
  api "$n" POST /api/v1/auth/login "{\"username\":\"owner\",\"password\":\"$PW\"}" >/dev/null
  [ "$CODE" = 200 ] || return 1
  api "$n" GET /api/v1/auth/me | jq -r .csrf_token > "$TMP/csrf$n"
}

wait_until() { # wait_until <seconds> <description> <command...>
  local t=$1 what=$2; shift 2
  for _ in $(seq 1 "$t"); do if "$@" >/dev/null 2>&1; then return 0; fi; sleep 1; done
  fail "timed out after ${t}s waiting for: $what"
}

role() { curl -s "${URLS[$1]}/api/v1/health" | jq -r .role; }
is_role() { [ "$(role "$1")" = "$2" ]; }
healthy() { curl -sf "${URLS[$1]}/api/v1/health" >/dev/null; }

for n in 1 2 3; do wait_until 60 "node $n healthy" healthy $n; done

log "setup owner on node 1"
api 1 POST /api/v1/setup "{\"setup_token\":\"$SETUP_TOKEN\",\"username\":\"owner\",\"password\":\"$PW\",\"language\":\"en\"}" >/dev/null
[ "$CODE" = 201 ] || [ "$CODE" = 200 ] || fail "setup returned $CODE: $(cat "$TMP/body")"
login 1 || fail "login on node 1"

log "found cluster on node 1"
api 1 POST /api/v1/cluster/init '{"advertise":""}' >/dev/null
[ "$CODE" = 200 ] || fail "cluster init returned $CODE: $(cat "$TMP/body")"
wait_until 30 "node 1 LEADER" is_role 1 LEADER

for n in 2 3; do
  log "join node $n (fresh node, via setup token)"
  api 1 POST /api/v1/cluster/tokens "{\"description\":\"node $n\",\"ttl_minutes\":15}" >/dev/null
  [ "$CODE" = 201 ] || fail "token create returned $CODE"
  tok=$(jq -r .token "$TMP/body")
  api $n POST /api/v1/setup/join "{\"setup_token\":\"$SETUP_TOKEN\",\"token\":\"$tok\",\"advertise\":\"\"}" >/dev/null
  [ "$CODE" = 200 ] || fail "join node $n returned $CODE: $(cat "$TMP/body")"
  log "reusing the same token must fail"
  api $n POST /api/v1/setup/join "{\"setup_token\":\"$SETUP_TOKEN\",\"token\":\"$tok\"}" >/dev/null
  [ "$CODE" != 200 ] || fail "token reuse accepted"
done

node_id() { curl -s "${URLS[$1]}/api/v1/health" | jq -r .node_id; }
for n in 2 3; do
  id=$(node_id $n)
  log "promote node $n ($id) to voter"
  promote() { api 1 POST "/api/v1/cluster/nodes/$1/promote" >/dev/null; [ "$CODE" = 200 ]; }
  wait_until 60 "promotion of node $n" promote "$id"
done
gate_open() { api 1 GET /api/v1/cluster | jq -e '.cluster.gate.open == true and .cluster.gate.voters == 3' ; }
wait_until 30 "3 voters, gate open" gate_open
for n in 2 3; do wait_until 30 "node $n FOLLOWER" is_role $n FOLLOWER; done

log "create monitor on the leader"
api 1 POST /api/v1/monitors "{\"name\":\"cluster target\",\"type\":\"http\",\"target\":\"$TARGET_URL\",\"interval_seconds\":10,\"timeout_seconds\":3,\"policy\":{\"confirmations_required\":2,\"min_locations\":1,\"min_valid_probes\":2,\"fail_mode\":\"consecutive\",\"failure_rounds\":2,\"recovery_rounds\":1,\"cooldown_seconds\":60}}" >/dev/null
[ "$CODE" = 201 ] || fail "create monitor returned $CODE: $(cat "$TMP/body")"
MID=$(jq -r .monitor.id "$TMP/body")
probing_nodes() { api 1 GET "/api/v1/monitors/$MID/results?limit=200" | jq '[.results[].node_id] | unique | length'; }
three_nodes_probe() { [ "$(probing_nodes)" -ge 3 ]; }
wait_until 60 "results from all 3 nodes on the leader" three_nodes_probe
is_up() { api "$1" GET "/api/v1/monitors/$MID" | jq -e '.monitor.state.status == "UP"'; }
wait_until 60 "monitor UP" is_up 1

log "follower: login works (forwarded session), management is read-only with a leader link"
login 2 || fail "login on follower"
api 2 POST /api/v1/monitor-groups '{"name":"from-follower"}' >/dev/null
[ "$CODE" = 409 ] || fail "follower accepted a write ($CODE)"
jq -e '.error.code == "node_read_only" and (.error.leader_url | length > 0)' "$TMP/body" >/dev/null || fail "read-only error lacks leader_url: $(cat "$TMP/body")"

if [ "${WITH_RUNNER:-0}" = 1 ]; then
  log "automation: start a slow script on the leader"
  # The script appends to a file in the runner's work directory ($HOME is the per-action job dir).
  api 1 POST /api/v1/workflows '{"name":"slow script","trigger":{"type":"manual"},"steps":[{"name":"slow","type":"script","language":"bash","timeout_seconds":60,"script":"sleep 10\necho x >> \"$HOME/../../e2e-executions\"\necho finished"}]}' >/dev/null
  [ "$CODE" = 201 ] || fail "create workflow returned $CODE: $(cat "$TMP/body")"
  WF=$(jq -r .workflow.id "$TMP/body")
  api 1 POST "/api/v1/workflows/$WF/run" '{}' >/dev/null
  [ "$CODE" = 202 ] || fail "run workflow returned $CODE: $(cat "$TMP/body")"
  RUN=$(jq -r .run.id "$TMP/body")
  dispatching() { api 1 GET "/api/v1/workflow-runs/$RUN" | jq -e '.actions[0].status == "dispatching"'; }
  wait_until 20 "script action dispatched" dispatching
fi

log "kill the leader (node 1)"
stop_node 1
NEW=""
find_leader() { for n in 2 3; do if is_role $n LEADER; then NEW=$n; return 0; fi; done; return 1; }
wait_until 30 "a new leader among nodes 2 and 3" find_leader
log "new leader: node $NEW"
login $NEW || fail "login on new leader"
api $NEW POST /api/v1/monitor-groups '{"name":"after-failover"}' >/dev/null
[ "$CODE" = 201 ] || fail "write on new leader returned $CODE: $(cat "$TMP/body")"
last_round() { api $NEW GET "/api/v1/monitors/$MID" | jq -r .monitor.state.last_round; }
R1=$(last_round)
advanced() { [ "$(last_round)" -gt "$R1" ]; }
wait_until 40 "evaluation continues on the new leader" advanced
is_up $NEW >/dev/null || fail "monitor not UP on new leader"

if [ "${WITH_RUNNER:-0}" = 1 ]; then
  log "automation: the new leader recovers the in-flight script's outcome from the runner"
  recovered() { api $NEW GET "/api/v1/workflow-runs/$RUN" | jq -e '.run.status == "succeeded" and .actions[0].status == "reconciled_succeeded" and .actions[0].reconciled_by == "runner"'; }
  wait_until 60 "run reconciled from the runner record" recovered
  api $NEW GET "/api/v1/workflow-runs/$RUN" | jq -e '(.actions | length) == 1 and (.actions[0].output | contains("finished"))' >/dev/null || fail "unexpected actions: $(cat "$TMP/body")"
  api $NEW POST /api/v1/workflows '{"name":"count","trigger":{"type":"manual"},"steps":[{"name":"count","type":"script","language":"bash","script":"wc -l < \"$HOME/../../e2e-executions\""}]}' >/dev/null
  WF2=$(jq -r .workflow.id "$TMP/body")
  api $NEW POST "/api/v1/workflows/$WF2/run" '{}' >/dev/null
  RUN2=$(jq -r .run.id "$TMP/body")
  counted() { api $NEW GET "/api/v1/workflow-runs/$RUN2" | jq -e '.run.status == "succeeded"'; }
  wait_until 30 "count run" counted
  n=$(api $NEW GET "/api/v1/workflow-runs/$RUN2" | jq -r '.actions[0].output' | tr -d '[:space:]')
  [ "$n" = 1 ] || fail "the script ran $n times across the failover, want exactly 1"
  log "script executed exactly once across the failover"
fi

log "restart the old leader: it must rejoin as a follower"
start_node 1
wait_until 60 "node 1 healthy" healthy 1
wait_until 60 "node 1 FOLLOWER" is_role 1 FOLLOWER
login 1 || fail "login on restarted node 1"
caught_up() {
  local a b
  a=$(api $NEW GET /api/v1/cluster | jq -r .cluster.applied_index)
  b=$(api 1 GET /api/v1/cluster | jq -r .cluster.applied_index)
  [ $((a - b)) -le 5 ]
}
wait_until 60 "node 1 caught up" caught_up

log "quorum loss: stop the two other nodes; the survivor must refuse writes"
for n in 2 3; do stop_node $n; done
wait_until 30 "node 1 NO_QUORUM" is_role 1 NO_QUORUM
api 1 POST /api/v1/monitor-groups '{"name":"no-quorum"}' >/dev/null || true
[ "$CODE" != 201 ] || fail "write accepted without quorum"
log "restore nodes 2 and 3"
for n in 2 3; do start_node $n; done
for n in 2 3; do wait_until 60 "node $n healthy" healthy $n; done
any_leader() { for n in 1 2 3; do is_role $n LEADER && return 0; done; return 1; }
wait_until 60 "leader re-elected" any_leader

log "PASS"
