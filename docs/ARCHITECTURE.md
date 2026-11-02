# kvz architecture

kvz is a k3s-shaped Kubernetes distribution for Apple Silicon Macs. One signed
darwin/arm64 binary runs the whole control plane in-process, natively on
macOS, plus a node agent whose pods are native macOS workloads.

```
                    ┌──────────────────────── kvz server (one darwin process) ───────────────────────┐
 kubectl ──TLS──▶   │ kube-apiserver ── etcd (embedded) │ kube-controller-manager │ kube-scheduler    │
                    │       ▲                                                                        │
                    │       │ client-go (node identity, system:nodes)                                │
                    │  kvz node (virtual-kubelet) ── Provider mux ── by RuntimeClass handler ───┐     │
                    └───────────────────────────────────────────────────────────────────────────┼─────┘
                                                                                                │
           ┌─────────────────────────────┬──────────────────────────────┬──────────────────────┘
           ▼                             ▼                              ▼
   linux (default)               macos                           darwin
   Apple `container`             Virtualization.framework        fork/exec on host
   1 micro-VM per container      1 macOS VM per pod              process group per container
   own vmnet IP = pod IP         APFS clonefile disk, NAT IP     pod IP = host IP
                                 exec/logs over SSH              logs = files, exec = host exec

 other Macs:  kvz agent --server https://<server>:6443 --token K10<ca-sha256>::<secret>
```

## Control plane

* `k8s.io/kubernetes` v1.35.6 is imported as a library (all staging modules
  pinned to v0.35.6, no fork). kube-apiserver, kube-controller-manager and
  kube-scheduler all compile for darwin unmodified.
* Embedding three components in one process needs two workarounds:
  * `logsapi.ReapplyHandling = ReapplyHandlingIgnoreUnchanged`, because each
    component "applies" the global logging config.
  * The scheduler is started through `app.Setup` + `app.Run`, not its cobra
    command. `runCommand` installs a second process-wide signal handler, and
    `SetupSignalHandler` panics when it's called twice.
* Storage: embedded etcd 3.6 (`go.etcd.io/etcd/server/v3/embed`), localhost only.
  `--etcd-unsafe-no-fsync` exists as a benchmark knob.
* Cloud and volume controllers that make no sense here are disabled: nodeipam,
  route, service LB, PV binder, attach/detach.
* Scheduler API QPS is raised to 500/1000. At the defaults (50/100), binding
  200 pods took about 1.3 s, and that dominated time-to-ready for native pods.
* PKI: one ECDSA P-256 CA. It issues the apiserver serving cert, the
  apiserver→kubelet client cert, admin/controller/scheduler clients, and one
  serving+client cert per node (`system:node:<name>`, group `system:nodes`).
  Authorization is Node+RBAC with NodeRestriction. A `kvz:node` ClusterRole
  lets nodes list the secrets, configmaps and services that virtual-kubelet
  informers need.

## Node (virtual-kubelet)

kvz builds the virtual-kubelet `PodController` and `NodeController` itself
instead of using `nodeutil.NewNode`, for one reason. nodeutil hard-codes
client-go's default controller rate limiter (10 qps, burst 100) on all three
pod queues, which caps a node at about 10 pod status transitions per second.
With that limiter, 50 native processes took 6.8 s to become Ready. With a
2000 qps bucket it takes 0.42 s.

The provider implements `PodNotifier`, so status changes go straight to the
API server and nothing waits on virtual-kubelet's 5 s polling loop.

### linux runtime (Apple `container`)
* Each container is supervised as a foreground `container run --rm` child.
  That gives kvz the exit code, owned log files, and stop-on-cancel.
* One batched `container ls --format json` poller (100 ms) feeds IPs to every
  starting container. kvz doesn't run `inspect` per container.
* Volumes: emptyDir, hostPath, configMap and secret are materialized under the
  pod dir and bind-mounted with `-v`.
* **Runtime ceiling:** Apple's container daemon serializes VM starts at about
  2.1/s per host whatever the client concurrency (`bench/container_scaling.py`).
  kvz gates starts at 2 in flight (`KVZ_LINUX_START_CONCURRENCY`). Throughput
  stays the same, but pods become Ready FIFO: 20-pod burst p50 drops from
  9.5 s to 5.3 s.
* Limitation: no shared network namespace between containers of one pod. Each
  container is its own VM and the pod IP is container 0's.

### macos runtime (Virtualization.framework)
* Image = local VM bundle (tart layout: `disk.img`, `nvram.bin`,
  `config.json`), a bundle path, or an OCI ref pulled with `tart pull`.
* Pod start: `clonefile(2)` the disk and NVRAM (O(1); a 50 GB disk clones in
  under 1 ms), build a `VZMacPlatformConfiguration` with a fresh machine
  identifier, NAT NIC with a random MAC, then boot. The IP comes from
  `/var/db/dhcpd_leases` (100 ms polling), and readiness is gated on SSH.
* If container 0 has a command, kvz runs it over SSH in the guest, streams it
  to the container log, and its exit code completes the pod.
* macOS allows 2 concurrent macOS guests per host. The node advertises
  `kvz.dev/macos-vm: 2`, and extra pods wait with reason `WaitingForVMSlot`.
* The binary must be signed with `com.apple.security.virtualization`
  (`make build` does an ad-hoc sign).

### darwin runtime
Plain processes, for workloads that need the host itself: Xcode toolchains,
signing, GPU/ANE. Each pod gets a working directory (`$KVZ_POD_DIR`). Kill
goes to the process group, honoring the grace period. The restart policy is
implemented with exponential backoff.

## Joining nodes
The token is `K10<sha256(ca.crt)>::<secret>`, the same format as k3s. The
agent fetches the CA from `:6444/v1/ca` without TLS verification and checks it
against the hash. It then posts its name and IP to `/v1/join` over a connection
pinned to that CA, using the secret as a bearer token, and gets back a node
certificate.

## Not implemented (yet)
Services/kube-proxy and cluster DNS, cross-node pod networking, port-forward,
attach, projected service-account tokens in pod volumes, metrics/stats
summary, kine/SQLite storage.
