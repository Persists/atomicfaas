# AtomicFaaS

Prototype of platform-side compensation for FaaS on Knative Serving and Redis.
An interceptor in the function's pod logs every outbound call before it is
forwarded. If the invocation fails or its instance crashes, a reverter undoes
the logged effects from outside the instance, using a catalog of inverse calls.
The function code is unmodified.

## Layout

| Directory | Role |
|---|---|
| `interceptor/` | Proxy next to the function: write-ahead effect log, deferral of irreversible calls, commit; `shim.js` patches `fetch` |
| `crash-detector/` | Watches container terminations and hands their invocations to the reverter |
| `reverter/` | Undoes the effect log in reverse order, driven by `catalog.json` |
| `pathmatch/` | Path patterns of the catalog |
| `deploy/` | Kubernetes and Knative manifests |
| `trip-fn/` | Trip-booking workload: `vanilla-*.js` forward-only, `saga-*.js` with hand-written compensation |
| `experiments/loadgen/` | Load generator and mock provider APIs |
| `experiments/setup/` | Bring-up of the GCE machines and the unattended E3 and E4 runs |
| `experiments/infra/` | Terraform for E2, E3 and E4 |
| `experiments/plots/` | Figure scripts |
| `experiments/results/` | Raw records of the published runs and `figures/` |
| `experiments/scc-report.sh` | E1, lines of code |

## Running the experiments

Prerequisites: `gcloud` with a project and zone set, `terraform`, Go, `scc`, and
`python3 -m venv .venv && .venv/bin/pip install matplotlib seaborn` for the figures.

```sh
experiments/scc-report.sh                 # E1: lines of code, then experiments/plots/plot-e1.py
experiments/setup/run-e3.sh               # E3: single-invocation latency, about 3 h
experiments/setup/run-e4.sh 5ms           # E4: latency and throughput under load, one sweep
experiments/setup/run-e4.sh 50ms
```

`run-e3.sh` and `run-e4.sh` create the machines, run the experiment, copy the
records into `experiments/results/`, destroy the machines, and print the command
that draws the figure. Profiles and sizing are at the top of the scripts.

E2 runs one machine per variant with the load generator on it:

```sh
cd experiments/infra/e2 && terraform init && terraform apply && cd ../../..
experiments/setup/bringup.sh e2
gcloud compute ssh afaas-e2-atomic -- 'cd atomic-faas-impl && ./bin/loadgen -e2 -variants atomic -cluster-port 31080'
```

Kourier is a NodePort service, so the ingress is port 31080. Destroy the
machines with `terraform destroy` when done.
