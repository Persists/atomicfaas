#!/usr/bin/env bash
# Deploy the workloads pointed at the provider mock, with Redis and the controllers where an atomic variant runs.
#   experiments/setup/mock-host.sh <mock-address> [variant...]     default: atomic saga vanilla
# MOCK_PORT, CPU_REQUEST, MIN_SCALE, MAX_SCALE override the manifests.
set -euo pipefail
cd "$(dirname "$0")/../.."

HOST=${1:?mock address}; shift
VARIANTS=("$@")
[ ${#VARIANTS[@]} -gt 0 ] || VARIANTS=(atomic saga vanilla)
URL="http://$HOST:${MOCK_PORT:-8090}"
echo "pointing ${VARIANTS[*]} at $URL${CPU_REQUEST:+, cpu $CPU_REQUEST}${MIN_SCALE:+, min-scale $MIN_SCALE}${MAX_SCALE:+, max-scale $MAX_SCALE}"

FILES=()
for v in "${VARIANTS[@]}"; do FILES+=("deploy/$v-extended.yaml"); done
if [[ " ${VARIANTS[*]} " == *" atomic "* ]]; then
  kubectl apply -f deploy/redis.yaml >/dev/null
  kubectl rollout status deployment/redis --timeout=120s >/dev/null
  FILES+=(deploy/reverter.yaml deploy/crash-detector.yaml)
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
cp "${FILES[@]}" "$TMP"/
python3 - "$TMP" "$URL" "${CPU_REQUEST:-}" "${MIN_SCALE:-}" "${MAX_SCALE:-}" <<'EOF'
import glob, re, sys
d, url, cpu, lo, hi = sys.argv[1:6]
for f in glob.glob(d + "/*.yaml"):
    s = open(f).read()
    s = re.sub(r"http://[^\s]+:8090", url, s)
    if f.endswith("-extended.yaml"):
        if cpu:
            s = re.sub(r"(\bcpu: )\S+", r"\g<1>" + cpu, s)
        if lo:
            s = re.sub(r'(min-scale: ")\d+', r"\g<1>" + lo, s)
        if hi:
            s = re.sub(r'(max-scale: ")\d+', r"\g<1>" + hi, s)
    open(f, "w").write(s)
EOF
kubectl apply -f "$TMP"/ >/dev/null
kubectl wait ksvc --all --for=condition=Ready --timeout=300s >/dev/null
echo "done - ${VARIANTS[*]} now call $URL"
