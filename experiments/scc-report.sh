#!/usr/bin/env bash
# E1: scc over the four trip workload sources (base/extended x forward-only/saga); they carry no test
# scaffolding, so scc measures them directly. Writes experiments/results/campaigns/e1-scc.json for plot-e1.py.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=experiments/results/campaigns/e1-scc.json
scc --no-cocomo --no-size --by-file -f json \
  trip-fn/vanilla-simple.js trip-fn/saga-simple.js trip-fn/vanilla-extended.js trip-fn/saga-extended.js \
  | python3 -c '
import json, sys
rows = {f["Filename"].split("/")[-1][:-3]: {"lines": f["Lines"], "code": f["Code"], "complexity": f["Complexity"]}
        for lang in json.load(sys.stdin) for f in lang["Files"]}
json.dump(rows, open(sys.argv[1], "w"), indent=1)
for k, v in rows.items():
    code, cx = v["code"], v["complexity"]
    print(f"{k:18s} code={code:3d}  complexity={cx}")
' "$OUT"
echo "wrote $OUT"
