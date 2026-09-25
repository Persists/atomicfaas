#!/usr/bin/env bash
# On a cluster node before and after a run: instances warm, nothing restarted, nothing logging per request.
#   experiments/setup/preflight.sh <variant> <min-scale>
set -uo pipefail
V=$1; MIN=$2; SVC=$V-extended; FAIL=0
ok()   { echo "ok    $*"; }
bad()  { echo "FAIL  $*"; FAIL=1; }
warn() { echo "WARN  $*"; }

ready=$(kubectl get pods -l serving.knative.dev/service="$SVC" --no-headers 2>/dev/null | awk '$2=="2/2" && $3=="Running"' | wc -l)
[ "$ready" -ge "$MIN" ] && ok "$ready $SVC pods ready (min-scale $MIN)" || bad "$ready of $MIN $SVC pods ready"

pending=$(kubectl get pods -A --field-selector=status.phase=Pending --no-headers 2>/dev/null | wc -l)
[ "$pending" -eq 0 ] && ok "no pending pods" || warn "$pending pending pods (over the node's CPU budget)"

envoy=$(pgrep -x envoy | head -1)
soft=$(awk '/open files/{print $4}' /proc/"$envoy"/limits 2>/dev/null)
[ "${soft:-0}" -ge 65536 ] && ok "kourier: $soft open files allowed" || bad "kourier: envoy limited to ${soft:-?} open files"

restarts=$(kubectl -n kourier-system get pods -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.restartCount}{"\n"}{end}{end}' | awk '{s+=$1} END {print s+0}')
[ "$restarts" -eq 0 ] && ok "kourier: 0 restarts" || bad "kourier: $restarts container restarts"

active=$(systemctl is-active apt-daily.timer apt-daily-upgrade.timer unattended-upgrades 2>/dev/null | grep -c '^active')
[ "$active" -eq 0 ] && ok "update timers off" || bad "$active update timers still active"

if [ "$V" = atomic ]; then
  args=$(kubectl get deploy redis -o jsonpath='{.spec.template.spec.containers[0].args}')
  case "$args" in *--save*) ok "redis: snapshotting off";; *) bad "redis: default snapshotting ($args)";; esac
  pod=$(kubectl get pods -l app=redis -o jsonpath='{.items[0].metadata.name}')
  echo "info  redis: $(kubectl exec "$pod" -- redis-cli info memory | tr -d '\r' | grep -E '^used_memory_(human|peak_human)' | paste -sd' ')  keys: $(kubectl exec "$pod" -- redis-cli dbsize)"
fi

# The gateway's access log surfaces with a delay of seconds, so count /healthz lines before and after a burst.
IP=$(hostname -I | awk '{print $1}')
gwlog() { kubectl -n kourier-system logs deploy/3scale-kourier-gateway 2>/dev/null | grep -c healthz; }
before=$(gwlog)
code=$(curl -s -m 10 -o /dev/null -w '%{http_code}' -H "Host: $SVC.default.$IP.sslip.io" http://127.0.0.1:31080/healthz)
[ "$code" = 200 ] && ok "ingress -> $SVC: 200" || bad "ingress -> $SVC: $code"
for i in $(seq 1 50); do curl -s -m 10 -o /dev/null -H "Host: $SVC.default.$IP.sslip.io" http://127.0.0.1:31080/healthz; done
sleep 12
[ "$(gwlog)" -gt "$before" ] && bad "kourier: access logging is on (per-request log lines)" || ok "kourier: no per-request access log"

echo "info  node cpu requests: $(kubectl describe node | grep -A6 'Allocated resources' | awk '$1=="cpu"{print $2, $3}')"
exit $FAIL
