<p align="center">
  <picture>
    <source media="(prefers-color-scheme: light)" srcset="docs/assets/brand-v1/readme-banner-light-1280x320.svg">
    <img alt="kvz — native macOS kubernetes" src="docs/assets/brand-v1/readme-banner-1280x320.svg" width="100%">
  </picture>
</p>

# kvz: native-macOS Kubernetes

A k3s-style, single-binary Kubernetes distribution that runs **natively on
macOS**. The control plane has no Linux VM: etcd, kube-apiserver,
controller-manager and scheduler run in one darwin process. Pods run on three
native runtimes:

| `runtimeClassName` | What a pod is | Built on |
|---|---|---|
| `linux` (default) | one Linux micro-VM per container, own routable IP | Apple `container` / Containerization (macOS 26+) |
| `macos` | a full macOS VM (container 0), exec over SSH | Virtualization.framework; APFS `clonefile` disk (50 GB in under 1 ms) |
| `darwin` | native macOS processes | fork/exec, per-pod dir, process-group kill |

Extra Macs join as nodes with a k3s-style token.

## Quickstart

```sh
make build                       # builds + ad-hoc signs bin/kvz (needs the virtualization entitlement)
./bin/kvz server                 # prints KUBECONFIG and a join command
export KUBECONFIG=~/.kvz/kubeconfig.yaml
kubectl run web --image=nginx:alpine                       # Linux micro-VM
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata: {name: mac}
spec:
  runtimeClassName: macos
  containers: [{name: vm, image: ghcr.io/cirruslabs/macos-tahoe-base:latest}]
EOF
kubectl exec mac -- sw_vers                                # runs inside the macOS guest

# on another Mac
kvz agent --server https://<server-ip>:6443 --token K10…::…
```

Requirements: Apple Silicon, macOS 26+. The `linux` runtime needs Apple
`container` installed (`container system start`). The `macos` runtime needs a
tart-format VM bundle; OCI refs are pulled with `tart`.

## Headline numbers (M4 Mac mini; see [docs/BENCHMARKS.md](docs/BENCHMARKS.md))

| | kvz | k3s in Lima | Apple `container cluster` |
|---|---|---|---|
| restart → API ready | **2.0 s** | 9.3 s | 58 s |
| idle CPU / memory | **1.6 % / 250 MB** | 20 % / 3.2 GB | 26 % / 2.5 GB |
| 1 nginx pod → Ready | 740 ms | **410 ms** | 860 ms |
| 20 nginx pods → Ready | 12 s | 1.3 s | **0.9 s** |
| native process pod → Ready | **48 ms** | n/a | n/a |
| macOS VM pod → SSH ready | **~17 s** | n/a | n/a |

kvz is far cheaper to run, and it's the only option of the three for macOS
workloads. Apple's runtime boots Linux micro-VMs serially (~2/s), so kvz is
much slower than VM-packed k3s or kind at bursts of Linux pods.

## Docs
* [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): how it works and what was needed to embed k8s on darwin
* [docs/BENCHMARKS.md](docs/BENCHMARKS.md): full methodology and results across 4 Macs
* `results/`: raw JSONL and the generated `SUMMARY.md`

## Status
Prototype. Not implemented yet: Services/kube-proxy, cluster DNS,
cross-node pod networking, port-forward, attach, projected SA tokens.
