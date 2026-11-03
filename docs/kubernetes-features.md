# Kubernetes features

kvz runs upstream Kubernetes 1.37 (kube-apiserver, controller-manager and
scheduler, unmodified) with embedded etcd 3.7. It is tested with the full
Sonobuoy certified-conformance suite on a two-node cluster; see
[Conformance](#conformance). This page lists what works today and the known
limits.

## Workloads

- Deployments (scaling, rollouts), StatefulSets (with stable pod DNS names),
  Jobs and plain Pods
- Init containers, native sidecars, and `restartPolicy` with real
  `CrashLoopBackOff` back-off
- Startup, readiness and liveness probes (exec, HTTP, TCP; gRPC is checked as
  TCP), `readinessGates`
- `postStart` and `preStop` hooks, termination messages (including
  `FallbackToLogsOnError`), `activeDeadlineSeconds`
- Ephemeral containers (`kubectl debug`), which run inside the pod's VM and
  share its network and processes
- Kubelet-style pod events and condition reasons, `observedGeneration`

## Resources and security

- **CPU and memory limits** are enforced per container with cgroup v2
  (`cpu.max`, `cpu.weight`, `memory.max`), exactly as on a Linux node. A
  container that exceeds its memory limit is reported as `OOMKilled`.
- **In-place resize** (`kubectl patch --subresource resize`) updates the limits
  of running containers, or restarts them where `resizePolicy` asks for it.
- Pods without a CPU limit can use every core of the Mac; pods without a
  memory limit get 2 GiB (`KVZ_DEFAULT_MEMORY_MB`). Guest memory is only
  backed by the Mac when it is used.
- **Security context:** `runAsUser`, `runAsGroup`, `supplementalGroups`,
  `fsGroup`, `runAsNonRoot`, `readOnlyRootFilesystem`, `privileged`,
  capabilities (containerd's default set, plus `add` and `drop`) and
  `allowPrivilegeEscalation`.
- **Sysctls:** every pod has its own kernel, so all sysctls are pod-scoped
  and allowed.
- Each pod is its own VM: containers in different pods never share a kernel.

## kubectl

`exec` (with a terminal), `logs` (including `-f`, `--previous`, `--since` and
`--tail`), `port-forward`, `attach` (interactive for containers with
`stdin`/`tty`), `debug`, `cp`, `run --rm -i` and `top`.
kvz also bundles a version-matched kubectl as
`kvz kubectl`.

## Networking

- **Pod network:** every Linux pod has its own IP address on kvz's pod network,
  an in-process switch. Pods on different Macs reach each other over an
  authenticated overlay between nodes.
- **Services:** `ClusterIP` load balancing across replicas, plus `NodePort` and
  `LoadBalancer`, which are exposed on the Mac. `sessionAffinity: ClientIP`
  works for all three.
- **Cluster DNS:** a built-in server at `10.43.0.10` that answers
  `cluster.local` like CoreDNS: service and headless records, StatefulSet pod
  names, SRV, ExternalName, pod (`a-b-c-d.<ns>.pod`) and reverse records.
  Other names go to the Mac's resolvers, so VPN and Tailscale names resolve
  too. `dnsPolicy` and `dnsConfig` work.
- **The API server reaches pods** (on every node), so admission and conversion
  webhooks, aggregated API servers running in pods, and `kubectl proxy` to
  pods and services work.
- **NetworkPolicy:** enforced in the pod switch, for ingress and egress, pod and
  namespace selectors, IP blocks and ports.
- **Ingress:** Traefik is installed by default.
- **Host networking:** `hostPort` (TCP and UDP, `hostIP`, conflict checks) and
  `hostNetwork`. A `hostNetwork` pod shares the Mac's network as on a Linux
  node: `127.0.0.1` and the node IP inside it are the Mac's, and every socket it
  listens on appears on the Mac at the same address and port.

## Storage and configuration

- ConfigMaps and Secrets, including live updates to mounted volumes (within
  about a second), `defaultMode` and item modes, optional sources
- `emptyDir` (including `medium: Memory`), `hostPath` (directories and files),
  projected and downward API volumes, `subPath` and `subPathExpr`
- PersistentVolumeClaims through the built-in local-path provisioner, which is
  the default StorageClass
- Service account tokens bound to the pod, so in-cluster clients work
- Image volumes (`volumes[].image`): an OCI image's files mounted read-only,
  straight from the image store with nothing copied
- `clusterTrustBundle` projections (CA bundles by name, or by signer and
  label selector) and `podCertificate` projections (a per-pod key and
  certificate from a signer, refreshed before expiry), using the kubelet's own
  managers. Their files keep the volume's default mode: kvz can't yet set the
  per-file owner (`user`, `fsGroup`) that makes the kubelet use 0600.

## Images

- Pull policies `Always`, `IfNotPresent` and `Never`
- `imagePullSecrets` and Docker config credentials
- `ErrImagePull` and `ImagePullBackOff` with retries
- One shared store, keyed by image digest
- Registry mirrors and credentials in `~/.kvz/registries.yaml`, in the k3s
  format (see [Operations](operations.md#registry-mirrors))

## Metrics

kvz reports per-container CPU and memory from inside each pod VM and serves the
`metrics.k8s.io` API, so `kubectl top` and HorizontalPodAutoscalers work. Each
node also serves the kubelet's `/metrics`, `/metrics/cadvisor` and
`/metrics/resource`, so Prometheus and an upstream metrics-server can scrape
it.

## Add-ons installed by default

Traefik, the local-path provisioner, the metrics server and the k3s Helm
controller (so `HelmChart` objects install charts). Skip any of them with
`--disable traefik`, `--disable local-storage`, `--disable metrics-server` or
`--disable helm-controller`. Put your own manifests in
`~/.kvz/server/manifests` and kvz applies them automatically.

## Known limits

| Limit | Why |
|---|---|
| About 120 Linux pods per Mac | Virtualization.framework caps a Mac at about 127 VMs. Add Macs to scale out. |
| Two macOS pods per Mac | macOS itself allows only two macOS guests per host |
| Pod-to-pod bandwidth around 30 to 50 Gbps | Each pod is its own VM, so traffic can't stay inside one kernel |
| Large bursts are CPU-bound | Starting a VM costs about 65 ms of CPU in Virtualization.framework |
| No user namespaces, AppArmor or seccomp profiles | The VM boundary isolates pods; inside a pod, containers rely on capabilities |

## Conformance

kvz passes the full Kubernetes conformance suite:
`sonobuoy run --mode certified-conformance` on a two-node cluster (two kvz nodes
on an M5 Max) reports **428 of 428 tests passed** (Kubernetes 1.35.10).
