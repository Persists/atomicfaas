#!/usr/bin/env bash
# The E3 run, unattended: apply the pair, bring it up, pre-flight, drive all three variants from the driver, collect, destroy.
#   experiments/setup/run-e3.sh [--keep]          --keep leaves the pair up; SKIP_BRINGUP=1 reuses a pair that is up
# DELAY_MS, N, WARMUP, CPU_REQUEST, MIN_SCALE, MAX_SCALE and RSSH_TRIES can be overridden in the environment.
set -euo pipefail
cd "$(dirname "$0")/../.."

main() {
KEEP=${1:-}
DELAY_MS=${DELAY_MS:-5}; N=${N:-1000}; WARMUP=${WARMUP:-10}
export CPU_REQUEST=${CPU_REQUEST:-150m} MIN_SCALE=${MIN_SCALE:-1} MAX_SCALE=${MAX_SCALE:-50}
C=afaas-e3-cluster; D=afaas-e3-driver; NODE_PORT=31080
VARIANTS=(atomic saga vanilla)
DEST=experiments/results/campaigns/e3-${DELAY_MS}ms-$(date -u +%Y%m%dT%H%M%SZ)
ip() { gcloud compute instances describe "$1" --format='value(networkInterfaces[0].networkIP)'; }
rssh() { local h=$1; shift; for i in $(seq 1 "${RSSH_TRIES:-6}"); do gcloud compute ssh "$h" --command "$*" && return 0; sleep 15; done; return 1; }
say() { echo "$(date -u +%H:%M:%S) $*"; }

say "== E3: delay ${DELAY_MS}ms, n=$N per cell (+$WARMUP warm-up rounds), cpu $CPU_REQUEST, scale $MIN_SCALE..$MAX_SCALE"
if [ "${SKIP_BRINGUP:-}" = 1 ]; then
  say "== reusing the pair that is up"
else
  say "== pair"
  (cd experiments/infra/e3 && terraform init -input=false >/dev/null && terraform apply -auto-approve -input=false)
  experiments/setup/bringup.sh e3
fi
CIP=$(ip "$C")

say "== pre-flight"
for v in "${VARIANTS[@]}"; do
  echo "-- $v"
  rssh "$C" "cd atomic-faas-impl && experiments/setup/preflight.sh $v $MIN_SCALE"
done

say "== driving (one loadgen for all variants)"
rssh "$D" "cd atomic-faas-impl && rm -rf experiments/results/campaigns e3.log && (nohup ./bin/loadgen -e3 -n $N -warmup $WARMUP \
  -delay-ms $DELAY_MS -cluster $CIP -cluster-port $NODE_PORT -out experiments/results/campaigns >e3.log 2>&1 </dev/null &) ; \
  sleep 1; pgrep -x loadgen >/dev/null && echo started"

while :; do
  sleep 300
  if out=$(gcloud compute ssh "$D" --command 'cd atomic-faas-impl; pgrep -cx loadgen || true; grep -E "===|per variant|error|panic" e3.log | tail -2' 2>/dev/null); then
    running=$(head -1 <<<"$out")
    say "loadgen running: $running"
    tail -n +2 <<<"$out" | sed 's/^/    /'
    [ "$running" = 0 ] && break
  else
    say "driver unreachable; retrying"
  fi
done

say "== collecting to $DEST"
mkdir -p "$DEST"
rssh "$D" "cd atomic-faas-impl && tar czf e3.tgz -C experiments/results campaigns -C ../.. e3.log"
gcloud compute scp "$D:~/atomic-faas-impl/e3.tgz" "$DEST/e3.tgz"
tar xzf "$DEST/e3.tgz" -C "$DEST" && rm "$DEST/e3.tgz"
for v in "${VARIANTS[@]}"; do
  echo "-- $v after the run"
  RSSH_TRIES=1 rssh "$C" "cd atomic-faas-impl && experiments/setup/preflight.sh $v $MIN_SCALE" | tee "$DEST/cluster-$v-after.txt" || true
done
tail -5 "$DEST/e3.log"

if [ "$KEEP" != "--keep" ]; then
  say "== destroying the pair"
  (cd experiments/infra/e3 && terraform destroy -auto-approve -input=false)
fi
cat <<NEXT

done: $DEST
  .venv/bin/python experiments/plots/plot-e3.py $DEST/campaigns/*
NEXT
}
main "$@"
