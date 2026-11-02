# Kubernetes features

kvz runs upstream Kubernetes 1.35 (kube-apiserver, controller-manager and
scheduler, unmodified) with embedded etcd 3.6. It passes the Sonobuoy quick
conformance check. This page lists what works today and the known limits.

## Workloads

- Deployments (scaling, rollouts), StatefulSets (with stable pod DNS names),
  Jobs and plain Pods
- Init containers, native sidecars, and `restartPolicy` with real
  `CrashLoopBackOff` back-off
- Startup, readiness and liveness probes (exec, HTTP, TCP; gRPC is checked as
  TCP)
- `postStart` and `preStop` hooks
- Kubelet-style pod events

## kubectl

`exec` (with a terminal), `logs` (including `-f`, `--previous`, `--since` and
`--tail`), `port-forward`, `attach` for output, `run --rm -i` and `top`.
kvz also bundles a version-matched kubectl as
`kvz kubectl`.

## Networking

- **Pod network:** every Linux pod has its own IP address on kvz's pod network,
  an in-process switch. Pods on different Macs reach each other over an
  authenticated overlay between nodes.
- **Services:** `ClusterIP` load balancing across replicas, plus `NodePort` and
  `LoadBalancer`, which are exposed on the Mac.
- **Cluster DNS:** at `10.43.0.10`, with names like
  `<service>.<namespace>.svc.cluster.local` and the normal search path.
- **NetworkPolicy:** enforced in the pod switch, for ingress and egress, pod and
  namespace selectors, IP blocks and ports.
- **Ingress:** Traefik is installed by default.
- **Host networking:** `hostPort` and `hostNetwork`.

## Storage and configuration

- ConfigMaps and Secrets, including live updates to mounted volumes
- `emptyDir`, `hostPath`, projected and downward API volumes
- PersistentVolumeClaims through the built-in local-path provisioner, which is
  the default StorageClass
- Service account tokens bound to the pod, so in-cluster clients work

## Images

- Pull policies `Always`, `IfNotPresent` and `Never`
- `imagePullSecrets` and Docker config credentials
- `ErrImagePull` and `ImagePullBackOff` with retries
- One shared store, keyed by image digest

## Metrics

kvz reports per-container CPU and memory from inside each pod VM and serves the
`metrics.k8s.io` API, so `kubectl top` and HorizontalPodAutoscalers work.

## Add-ons installed by default

Traefik, the local-path provisioner and the metrics server. Skip any of them
with `--disable traefik`, `--disable local-storage` or
`--disable metrics-server`. Put your own manifests in
`~/.kvz/server/manifests` and kvz applies them automatically.

## Known limits

| Limit | Why |
|---|---|
| About 120 Linux pods per Mac | Virtualization.framework caps a Mac at about 127 VMs. Add Macs to scale out. |
| Two macOS pods per Mac | macOS itself allows only two macOS guests per host |
| Pod-to-pod bandwidth around 30 to 50 Gbps | Each pod is its own VM, so traffic can't stay inside one kernel |
| Large bursts are CPU-bound | Starting a VM costs about 65 ms of CPU in Virtualization.framework |
| `attach` doesn't send input yet | Output works; input is not implemented |
