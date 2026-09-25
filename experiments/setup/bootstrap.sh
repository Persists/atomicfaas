#!/usr/bin/env bash
# Bootstrap a fresh Ubuntu machine.
#   experiments/setup/bootstrap.sh driver
#   experiments/setup/bootstrap.sh cluster [--driver <address>] [variant...]
# cluster: a single kubeadm/containerd node with Knative and Kourier, the images built and imported, the
# workloads deployed against the mock on the driver (or on this machine without --driver).
# MOCK_PORT, CPU_REQUEST, MIN_SCALE, MAX_SCALE pass through to mock-host.sh; GO_VERSION, K8S_MINOR,
# KNATIVE_VERSION and MAX_RESTART_PERIOD pin the versions.
set -euo pipefail
cd "$(dirname "$0")/../.."

MODE=${1:?driver | cluster}; shift
DRIVER_IP=
if [ "${1:-}" = --driver ]; then DRIVER_IP=$2; shift 2; fi
VARIANTS=("$@")

GO_VERSION=${GO_VERSION:-1.26.3}
K8S_MINOR=${K8S_MINOR:-v1.32}
KNATIVE_VERSION=${KNATIVE_VERSION:-knative-v1.20.0}
MAX_RESTART_PERIOD=${MAX_RESTART_PERIOD:-1s}

step() { echo; echo "=== $*"; }
have() { command -v "$1" >/dev/null 2>&1; }

step "apt packages"
sudo apt-get update -qq
sudo apt-get install -y -qq curl git tmux ca-certificates apt-transport-https gpg python3-venv

step "background updaters off"
for u in apt-daily.timer apt-daily-upgrade.timer unattended-upgrades fwupd-refresh.timer motd-news.timer man-db.timer snapd.socket snapd.service; do
  sudo systemctl disable --now "$u" 2>/dev/null || true
done

step "go $GO_VERSION"
if ! (have go && go version | grep -q "go$GO_VERSION"); then
  curl -fsSLo /tmp/go.tgz "https://go.dev/dl/go$GO_VERSION.linux-amd64.tar.gz"
  sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
  grep -qs '/usr/local/go/bin' "$HOME/.profile" || echo 'export PATH=$PATH:/usr/local/go/bin' >>"$HOME/.profile"
fi
export PATH=$PATH:/usr/local/go/bin
go build -o bin/loadgen ./experiments/loadgen

if [ "$MODE" = driver ]; then
  [ -d .venv ] || python3 -m venv .venv
  echo "driver ready"
  exit 0
fi
[ "$MODE" = cluster ] || { echo "usage: $0 driver | cluster [--driver <address>] [variant...]" >&2; exit 1; }

step "docker (builder) + containerd (runtime)"
have docker || curl -fsSL https://get.docker.com | sudo sh
sudo mkdir -p /etc/containerd /etc/systemd/system/containerd.service.d
containerd config default | sudo tee /etc/containerd/config.toml >/dev/null
sudo sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
# Envoy inherits containerd's soft limit of 1024 open files and dies under load if it stays.
printf '[Service]\nLimitNOFILE=1048576\n' | sudo tee /etc/systemd/system/containerd.service.d/nofile.conf >/dev/null
sudo systemctl daemon-reload
sudo systemctl restart containerd

step "kernel prerequisites"
sudo modprobe br_netfilter
echo br_netfilter | sudo tee /etc/modules-load.d/k8s.conf >/dev/null
printf 'net.bridge.bridge-nf-call-iptables=1\nnet.ipv4.ip_forward=1\n' | sudo tee /etc/sysctl.d/k8s.conf >/dev/null
sudo sysctl --system >/dev/null

step "kubeadm / kubelet / kubectl ($K8S_MINOR)"
if ! have kubeadm; then
  curl -fsSL "https://pkgs.k8s.io/core:/stable:/$K8S_MINOR/deb/Release.key" | sudo gpg --dearmor -o /etc/apt/keyrings/kubernetes.gpg
  echo "deb [signed-by=/etc/apt/keyrings/kubernetes.gpg] https://pkgs.k8s.io/core:/stable:/$K8S_MINOR/deb/ /" |
    sudo tee /etc/apt/sources.list.d/kubernetes.list >/dev/null
  sudo apt-get update -qq
  sudo apt-get install -y -qq kubelet kubeadm kubectl
  sudo apt-mark hold kubelet kubeadm kubectl >/dev/null
fi
printf 'runtime-endpoint: unix:///run/containerd/containerd.sock\n' | sudo tee /etc/crictl.yaml >/dev/null

step "cluster"
if ! kubectl get nodes >/dev/null 2>&1; then
  CONF=$(mktemp)
  trap 'rm -f "$CONF"' EXIT
  cat >"$CONF" <<EOF
apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
networking:
  podSubnet: 10.244.0.0/16
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
maxPods: 250
featureGates:
  KubeletCrashLoopBackOffMax: true
crashLoopBackOff:
  maxContainerRestartPeriod: $MAX_RESTART_PERIOD
EOF
  sudo kubeadm init --config "$CONF"
  mkdir -p "$HOME/.kube"
  sudo cp /etc/kubernetes/admin.conf "$HOME/.kube/config"
  sudo chown "$(id -u):$(id -g)" "$HOME/.kube/config"
  kubectl apply -f https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml
  kubectl taint nodes --all node-role.kubernetes.io/control-plane- || true
fi
sudo grep -q "^crashLoopBackOff:" /var/lib/kubelet/config.yaml || { echo "crashLoopBackOff cap missing from the kubelet config" >&2; exit 1; }
kubectl wait node --all --for=condition=Ready --timeout=300s

step "Knative Serving $KNATIVE_VERSION with Kourier"
BASE=https://github.com/knative/serving/releases/download/$KNATIVE_VERSION
kubectl apply -f "$BASE/serving-crds.yaml"
kubectl wait --for=condition=Established crd --all --timeout=120s >/dev/null
kubectl apply -f "$BASE/serving-core.yaml"
kubectl apply -f "https://github.com/knative-extensions/net-kourier/releases/download/$KNATIVE_VERSION/kourier.yaml"
kubectl patch configmap/config-network -n knative-serving --type merge -p '{"data":{"ingress-class":"kourier.ingress.networking.knative.dev"}}'
kubectl patch service/kourier -n kourier-system --type merge -p '{
  "spec": {"type": "NodePort", "ports": [
    {"name": "http2", "port": 80,  "nodePort": 31080, "targetPort": 8080},
    {"name": "https", "port": 443, "nodePort": 31443, "targetPort": 8443}]}}'
kubectl patch configmap/config-domain -n knative-serving --type merge -p '{"data":{"127.0.0.1.sslip.io":""}}'
kubectl wait --for=condition=Available deploy --all -n knative-serving --timeout=300s
kubectl wait --for=condition=Available deploy --all -n kourier-system --timeout=300s
# One guaranteed core and slow probes for the gateway, and no per-request access log: the stock settings restart it under load.
kubectl -n knative-serving patch cm config-kourier --type merge -p '{"data":{"enable-service-access-logging":"false"}}' >/dev/null
kubectl -n kourier-system patch deploy 3scale-kourier-gateway --type=json -p='[
  {"op":"replace","path":"/spec/template/spec/containers/0/resources","value":{"requests":{"cpu":"1","memory":"200Mi"},"limits":{"cpu":"1","memory":"800Mi"}}},
  {"op":"replace","path":"/spec/template/spec/containers/0/livenessProbe/timeoutSeconds","value":5},
  {"op":"replace","path":"/spec/template/spec/containers/0/livenessProbe/failureThreshold","value":12},
  {"op":"replace","path":"/spec/template/spec/containers/0/readinessProbe/timeoutSeconds","value":5}]'
kubectl -n kourier-system rollout status deploy/3scale-kourier-gateway --timeout=120s

step "images"
sudo docker build -q -t dev.local/atomic-faas/trip-fn:dev -f trip-fn/Dockerfile .
sudo docker build -q -t dev.local/atomic-faas/controllers:dev -f controllers.Dockerfile .
sudo docker save dev.local/atomic-faas/trip-fn:dev dev.local/atomic-faas/controllers:dev | sudo ctr -n k8s.io images import - >/dev/null

step "workloads"
experiments/setup/mock-host.sh "${DRIVER_IP:-$(hostname -I | awk '{print $1}')}" "${VARIANTS[@]}"

if [ -n "$DRIVER_IP" ]; then
  step "route domain"
  IP=$(hostname -I | awk '{print $1}')
  OLD=$(kubectl get cm config-domain -n knative-serving -o go-template='{{range $k, $v := .data}}{{if ne $k "_example"}}{{$k}}{{"\n"}}{{end}}{{end}}' | head -1)
  [ "$OLD" = "$IP.sslip.io" ] ||
    kubectl patch cm config-domain -n knative-serving --type merge -p "{\"data\":{\"$OLD\":null,\"$IP.sslip.io\":\"\"}}" >/dev/null
  kubectl wait ksvc --all --for=condition=Ready --timeout=180s >/dev/null
fi
kubectl get ksvc -o custom-columns=NAME:.metadata.name,URL:.status.url --no-headers
