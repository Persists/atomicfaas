#!/usr/bin/env bash
# One E4 sweep, unattended: apply the fleet, bring it up, pre-flight, drive both variants, collect, destroy.
#   experiments/setup/run-e4.sh <5ms | 50ms | smoke> [--keep]      --keep leaves the fleet up
# SKIP_BRINGUP=1 reuses a fleet that is already up. Every profile value can be overridden in the environment;
# RSSH_TRIES sets how often a remote step is retried.
set -euo pipefail
cd "$(dirname "$0")/../.."

profile() {
  case "$1" in
    5ms)   DELAY_MS=${DELAY_MS:-5};  RATES=${RATES:-50,100,150,200,250,300,350,400,450,500,550,600}; CPU_REQUEST=${CPU_REQUEST:-150m}; MIN_SCALE=${MIN_SCALE:-20}; MAX_SCALE=${MAX_SCALE:-50};;
    50ms)  DELAY_MS=${DELAY_MS:-50}; RATES=${RATES:-50,75,100,125,150,175,200,225,250}; CPU_REQUEST=${CPU_REQUEST:-75m}; MIN_SCALE=${MIN_SCALE:-20}; MAX_SCALE=${MAX_SCALE:-75};;
    smoke) DELAY_MS=${DELAY_MS:-5};  RATES=${RATES:-50}; CPU_REQUEST=${CPU_REQUEST:-150m}; MIN_SCALE=${MIN_SCALE:-20}; MAX_SCALE=${MAX_SCALE:-50}
           DURATION=${DURATION:-60s}; CUT=${CUT:-10s}; CUT_TAIL=${CUT_TAIL:-5s}; PAUSE=${PAUSE:-5s}; STOP_BELOW=${STOP_BELOW:-0.95};;
    *) echo "unknown profile '$1' (5ms | 50ms | smoke)" >&2; return 1;;
  esac
  DURATION=${DURATION:-13m30s}; CUT=${CUT:-3m}; CUT_TAIL=${CUT_TAIL:-30s}; PAUSE=${PAUSE:-10m}; STOP_BELOW=${STOP_BELOW:-0}
}

# The whole run is one function, so an edit to this file while a sweep is in progress cannot derail it.
main() {
PROFILE=${1:?profile: 5ms | 50ms | smoke}
KEEP=${2:-}
profile "$PROFILE"
export CPU_REQUEST MIN_SCALE MAX_SCALE

VARIANTS=(atomic vanilla)
DRIVER=afaas-e4-driver
NODE_PORT=31080
DEST=experiments/results/campaigns/e4-$PROFILE-$(date -u +%Y%m%dT%H%M%SZ)
ip() { gcloud compute instances describe "$1" --format='value(networkInterfaces[0].networkIP)'; }
mock_port() { case "$1" in atomic) echo 8090;; saga) echo 8091;; vanilla) echo 8092;; esac; }
rssh() { local h=$1; shift; for i in $(seq 1 "${RSSH_TRIES:-6}"); do gcloud compute ssh "$h" --command "$*" && return 0; sleep 15; done; return 1; }
say() { echo "$(date -u +%H:%M:%S) $*"; }

say "== profile $PROFILE: delay ${DELAY_MS}ms, rates $RATES, cpu $CPU_REQUEST, scale $MIN_SCALE..$MAX_SCALE, cell $DURATION (cut $CUT + $CUT_TAIL), pause $PAUSE, stop-below $STOP_BELOW"
if [ "${SKIP_BRINGUP:-}" = 1 ]; then
  say "== reusing the fleet that is up"
else
  say "== fleet"
  (cd experiments/infra/e4 && terraform init -input=false >/dev/null && terraform apply -auto-approve -input=false)
  experiments/setup/bringup.sh e4 "${VARIANTS[@]}"
fi

say "== pre-flight"
for v in "${VARIANTS[@]}"; do
  echo "-- $v"
  rssh "afaas-e4-$v-cluster" "cd atomic-faas-impl && experiments/setup/preflight.sh $v $MIN_SCALE"
done

say "== driving (one loadgen per variant, each with its own campaign root)"
rssh "$DRIVER" "cd atomic-faas-impl && rm -rf experiments/results/campaigns e4-*.log"
for v in "${VARIANTS[@]}"; do
  CIP=$(ip "afaas-e4-$v-cluster")
  rssh "$DRIVER" "cd atomic-faas-impl && (nohup ./bin/loadgen -e4 -variants $v \
    -cluster $CIP -cluster-port $NODE_PORT -port $(mock_port "$v") -delay-ms $DELAY_MS -rates $RATES \
    -duration $DURATION -cut $CUT -cut-tail $CUT_TAIL -pause $PAUSE -stop-below $STOP_BELOW \
    -stop-group $(IFS=,; echo "${VARIANTS[*]}") -stop-dir experiments/results/campaigns/.stop \
    -out experiments/results/campaigns/$v >e4-$v.log 2>&1 </dev/null &) ; sleep 1; pgrep -f 'variants $v' >/dev/null && echo started $v"
done

while :; do
  sleep 120
  if out=$(gcloud compute ssh "$DRIVER" --command 'cd atomic-faas-impl; pgrep -cx loadgen || true; for f in e4-*.log; do echo "[$f] $(grep -E "achieved|ending|WARNING|error|panic" "$f" | tail -1)"; done' 2>/dev/null); then
    running=$(head -1 <<<"$out")
    say "loadgens running: $running"
    tail -n +2 <<<"$out" | sed 's/^/    /'
    [ "$running" = 0 ] && break
  else
    say "driver unreachable (under load); retrying"
  fi
done

say "== collecting to $DEST"
mkdir -p "$DEST"
rssh "$DRIVER" "cd atomic-faas-impl && tar czf e4.tgz -C experiments/results campaigns -C ../.. $(printf 'e4-%s.log ' "${VARIANTS[@]}")"
gcloud compute scp "$DRIVER:~/atomic-faas-impl/e4.tgz" "$DEST/e4.tgz"
tar xzf "$DEST/e4.tgz" -C "$DEST" && rm "$DEST/e4.tgz"
find "$DEST" -name 'e4-r*.jsonl' -size +1M -exec gzip -6 {} +
for v in "${VARIANTS[@]}"; do
  echo "-- $v after the sweep"
  RSSH_TRIES=1 rssh "afaas-e4-$v-cluster" "cd atomic-faas-impl && experiments/setup/preflight.sh $v $MIN_SCALE" | tee "$DEST/cluster-$v-after.txt" || true
done
grep -hE "===|achieved|ending|WARNING" "$DEST"/e4-*.log

if [ "$KEEP" != "--keep" ]; then
  say "== destroying the fleet"
  (cd experiments/infra/e4 && terraform destroy -auto-approve -input=false)
fi
cat <<NEXT

done: $DEST
  .venv/bin/python experiments/plots/plot-e4-boxes.py experiments/results/campaigns/e4-*/campaigns/*/*
NEXT
}
main "$@"
