#!/usr/bin/env bash
# Bring up the machines of one experiment after terraform apply and leave them ready to drive.
#   experiments/setup/bringup.sh e2 [variant...]   one machine per variant, mock and load generator on it
#   experiments/setup/bringup.sh e3                one cluster with every variant and one driver
#   experiments/setup/bringup.sh e4 [variant...]   one cluster per variant and one shared driver
# CPU_REQUEST, MIN_SCALE, MAX_SCALE pass through to mock-host.sh; e2 defaults to min-scale 1.
set -euo pipefail
cd "$(dirname "$0")/../.."

EXP=${1:?e2 | e3 | e4}; shift
VARIANTS=("$@")
NODE_PORT=31080
SIZING="CPU_REQUEST=${CPU_REQUEST:-} MIN_SCALE=${MIN_SCALE:-} MAX_SCALE=${MAX_SCALE:-}"

ip() { gcloud compute instances describe "$1" --format='value(networkInterfaces[0].networkIP)'; }
run() { gcloud compute ssh "$1" --command "$2"; }
mock_port() { case "$1" in atomic) echo 8090;; saga) echo 8091;; vanilla) echo 8092;; *) echo "unknown variant $1" >&2; exit 1;; esac; }

prepare() {
  for i in $(seq 1 40); do gcloud compute ssh "$1" --command true 2>/dev/null && break; sleep 10; done
  run "$1" 'mkdir -p ~/atomic-faas-impl/experiments'
  gcloud compute scp --recurse deploy interceptor pathmatch reverter crash-detector trip-fn go.mod go.sum README.md controllers.Dockerfile "$1":~/atomic-faas-impl/
  gcloud compute scp --recurse experiments/loadgen experiments/plots experiments/setup experiments/scc-report.sh "$1":~/atomic-faas-impl/experiments/
  run "$1" 'chmod +x ~/atomic-faas-impl/experiments/*.sh ~/atomic-faas-impl/experiments/setup/*.sh'
}

driver() {
  prepare "$1"
  run "$1" 'cd atomic-faas-impl && experiments/setup/bootstrap.sh driver' >/dev/null
}

cluster() {
  local m=$1 d=$2 mp=$3; shift 3
  prepare "$m"
  run "$m" "cd atomic-faas-impl && MOCK_PORT=$mp $SIZING experiments/setup/bootstrap.sh cluster ${d:+--driver $d} $*"
}

verify() {
  local code
  code=$(run "$1" "curl -s -m 10 -o /dev/null -w '%{http_code}' http://$3.default.$2.sslip.io:$NODE_PORT/healthz")
  [ "$code" = 200 ] || { echo "ingress check failed for $3 at $2:$NODE_PORT (got $code)" >&2; exit 1; }
  echo "   $3: 200 from $2:$NODE_PORT"
}

LOGS=$(mktemp -d)
PIDS=(); NAMES=()
spawn() { local name=$1; shift; ("$@") >"$LOGS/$name.log" 2>&1 & PIDS+=($!); NAMES+=("$name"); }
join() {
  local failed=0 i
  echo "== bringing up ${#NAMES[@]} machine(s) in parallel (20-30 min); logs in $LOGS"
  for i in "${!PIDS[@]}"; do
    if wait "${PIDS[$i]}"; then echo "   ${NAMES[$i]}: ok"; else echo "   ${NAMES[$i]}: FAILED" >&2; tail -30 "$LOGS/${NAMES[$i]}.log" >&2; failed=1; fi
  done
  [ "$failed" = 0 ] || exit 1
}

e2_machine() {
  cluster "afaas-e2-$1" "" 8090 "$1"
  run "afaas-e2-$1" "cd atomic-faas-impl && ./bin/loadgen -e2 -n 1 -variants $1 -cluster-port $NODE_PORT"
}

case $EXP in
  e2)
    [ ${#VARIANTS[@]} -gt 0 ] || VARIANTS=(atomic saga vanilla)
    SIZING="CPU_REQUEST=${CPU_REQUEST:-} MIN_SCALE=${MIN_SCALE:-1} MAX_SCALE=${MAX_SCALE:-}"
    for v in "${VARIANTS[@]}"; do spawn "$v" e2_machine "$v"; done
    join
    echo "== ready. On each machine: tmux new -s e2 './bin/loadgen -e2 -variants <variant> -cluster-port $NODE_PORT'"
    ;;
  e3)
    driver afaas-e3-driver
    DIP=$(ip afaas-e3-driver)
    spawn cluster cluster afaas-e3-cluster "$DIP" 8090
    join
    CIP=$(ip afaas-e3-cluster)
    for v in atomic saga vanilla; do verify afaas-e3-driver "$CIP" "$v-extended"; done
    echo "== pair ready: on afaas-e3-driver pass -cluster $CIP -cluster-port $NODE_PORT to loadgen"
    ;;
  e4)
    [ ${#VARIANTS[@]} -gt 0 ] || VARIANTS=(atomic vanilla)
    driver afaas-e4-driver
    DIP=$(ip afaas-e4-driver)
    for v in "${VARIANTS[@]}"; do spawn "$v" cluster "afaas-e4-$v-cluster" "$DIP" "$(mock_port "$v")" "$v"; done
    join
    echo "== fleet ready. On afaas-e4-driver, one process per variant:"
    for v in "${VARIANTS[@]}"; do
      CIP=$(ip "afaas-e4-$v-cluster")
      verify afaas-e4-driver "$CIP" "$v-extended"
      echo "   tmux new -d -s e4-$v './bin/loadgen -e4 -variants $v -cluster $CIP -cluster-port $NODE_PORT -port $(mock_port "$v") -out experiments/results/campaigns/$v'"
    done
    ;;
  *) echo "usage: $0 e2|e3|e4 [variant...]" >&2; exit 1;;
esac
