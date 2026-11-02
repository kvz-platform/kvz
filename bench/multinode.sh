#!/bin/sh
# Multi-Mac kvz cluster benchmark: Studio = server (+node), minis = agents.
# Agents reach the API through SSH reverse tunnels (mini->Studio is blocked by
# the tailnet ACL here); apiserver->kubelet goes Studio->mini over Tailscale.
set -u
cd "$(dirname "$0")/.."
DATA=$HOME/.kvz-mn
OUT=results/cluster/multinode.jsonl
MINIS="amelia derek lexie"
pkill -INT -f "kvz server" 2>/dev/null; sleep 2
rm -rf "$DATA"
./bin/kvz server --data-dir "$DATA" > /tmp/kvz-mn.log 2>&1 &
until [ -f "$DATA/join-token" ]; do sleep 0.5; done
TOKEN=$(cat "$DATA/join-token")
for h in $MINIS; do
  ssh -o ServerAliveInterval=30 -o ExitOnForwardFailure=yes -R 6443:127.0.0.1:6443 -R 6444:127.0.0.1:6444 $h \
    "pkill -f 'kvz agent'; rm -rf \$HOME/.kvz-agent; IP=\$(ifconfig | awk '/inet 100\\./{print \$2; exit}'); \
     ~/kvz-tools/gui.sh ~/kvz-tools/kvz agent --server https://127.0.0.1:6443 --token $TOKEN --node-name $h --node-ip \$IP --data-dir \$HOME/.kvz-agent > /tmp/kvz-agent.log 2>&1" &
done
export KUBECONFIG=$DATA/kubeconfig.yaml
for i in $(seq 1 120); do [ "$(kubectl get nodes --no-headers 2>/dev/null | grep -c ' Ready')" = 4 ] && break; sleep 1; done
kubectl get nodes
mkdir -p results/cluster
: > $OUT
B="./bin/kvzbench pods --label cluster-4node --timeout 8m"
$B --n 1000 --c 100 --runtime-class darwin --command "sleep 3600" | tee -a $OUT
$B --n 100 --c 100 --cpu 100m | tee -a $OUT
$B --n 200 --c 200 --cpu 100m | tee -a $OUT
$B --n 8 --c 8 --runtime-class macos --image tahoe-base --cpu 2 --memory 4Gi --extended kvz.dev/macos-vm=1 | tee -a $OUT
for h in $MINIS; do ssh $h "sudo pkill -INT -f 'kvz agent'"; done
pkill -INT -f "kvz server"
wait 2>/dev/null
