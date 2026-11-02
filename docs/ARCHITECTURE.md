# kvz architecture

kvz is a k3s-shaped Kubernetes distribution for Apple Silicon Macs. One signed
darwin/arm64 binary runs the whole control plane in-process, natively on
macOS, plus a node agent whose pods are native macOS workloads.

```
                    ┌──────────────────────────── kvz server (one darwin process) ────────────────────────────┐
 kubectl ──TLS──▶   │ kube-apiserver ── etcd (embedded, F_BARRIERFSYNC WAL) │ controller-manager │ scheduler   │
                    │       ▲                                                                                  │
                    │       │ client-go (node identity)                                                        │
                    │  kvz node (virtual-kubelet, tuned queues) ── Provider mux (by RuntimeClass) ─┐            │
                    │                                                                               │            │
                    │  podnet: L2 switch + gateway + DHCP/DNS (gVisor netstack), 10.88.0.0/16 ◀───┼──┐         │
                    └───────────────────────────────────────────────────────────────────────────┼──┼─────────┘
                ┌────────────────────────────┬──────────────────────────┬────────────────────┘  │ SOCK_DGRAM socketpair
                ▼                            ▼                          ▼                       │ per VM NIC
   linux (default)                 macos                        darwin                         │
   kvz microVM per pod             macOS VM per pod             native processes               │
   kvz kernel + kvz-init (PID 1)   APFS clone, DHCP on podnet   fork/exec, pgid kill           │
   virtiofs image + tmpfs overlay  SSH exec over podnet                                        │
   vsock events + exec ────────────────────────────────────────────────────────────────────────┘
   apple: Apple `container` daemon (v1 runtime, kept for comparison)
```

## Control plane

* `k8s.io/kubernetes` v1.35.6 is imported as a library (staging modules pinned,
  no fork). kube-apiserver, controller-manager and scheduler run in one darwin
  process. Two shims make that possible:
  * `logsapi.ReapplyHandling = ReapplyHandlingIgnoreUnchanged`
  * scheduler via `app.Setup`/`app.Run`, avoiding a second process-wide signal
    handler
* **etcd WAL sync.** On darwin, etcd fsyncs with `F_FULLFSYNC`, which flushes
  the SSD cache on every commit (≈4 ms per write). kvz vendors etcd's `fileutil`
  and defaults to `F_BARRIERFSYNC`. It's a write-ordering barrier: crash-
  consistent, though the most recent commits can be lost on sudden power loss.
  It's what Apple recommends for databases, and roughly what a Linux guest's
  fsync gets on Virtualization.framework anyway. `--etcd-sync=full` restores
  strict durability. Effect: create→watch 4.0 → 0.5 ms.
* **Local kubeconfig uses loopback.** Talking to the host's own Tailscale
  address went through the VPN's network extension and cut API throughput
  about 3×. `kubeconfig-external.yaml` carries the routable address.
* Scheduler API QPS is raised to 500/1000. Cloud and volume controllers that
  don't apply are disabled.
* PKI: one ECDSA CA, Node+RBAC authorization, and k3s-style join tokens
  (`K10<ca-sha256>::<secret>`).

## Node (virtual-kubelet, vendored)

* The pod controller is built directly, with a 2000 qps work-queue limiter
  instead of client-go's default 10 qps (50 native pods: 6.8 s → 0.42 s).
* **Patch:** finalize deleted pods as soon as the provider has stopped them.
  Upstream always slept `DeletionGracePeriodSeconds` (1 pod: 1.05 s → 54 ms).
* The provider pushes status through `PodNotifier`; there's no polling.

## linux runtime: kvz microVMs

Every pod is a cold-booted Linux microVM, built from scratch to boot fast:

| piece | what it does | why |
|---|---|---|
| **kvz microVM monitor** (`pkg/microvm`) | Virtualization.framework through Code-Hex/vz, one VM per pod, booted concurrently | Apple's `container` daemon serializes VM starts at ~2/s per host |
| **kvz kernel** (`kernel/`) | Linux 6.18 LTS from `allnoconfig` plus only what pods need, everything built in, 8.5 MB | Apple's 30 MB debug kernel takes 106 ms to reach init; this one takes 17 ms |
| **kvz-init** (`cmd/kvz-init`, Go, static) | PID 1 from an uncompressed initramfs. Mounts proc/dev, the virtiofs share and the overlay root; configures the NIC over netlink; forks/chroots/execs the containers; reaps; restart policy; exec server | No init system, no image-format decoding, no DHCP; it reads `spec.json` and execs |
| **rootfs** | Image unpacked once into the image store; every pod VM mounts it read-only over virtiofs, with a tmpfs upper dir in the guest (overlayfs) | Zero per-pod host disk work, and pods share the host page cache |
| **one virtiofs device** | Multi-directory share: pod dir, image(s), hostPath volumes | Each virtio-pci device costs ~7 ms of guest boot |
| **events over vsock** | `init` / `hello` / `started` / `exit` pushed to the host | Status goes straight to the API |
| **exec over vsock** | Framed stdin/stdout/stderr/resize/exit protocol; pty support | `kubectl exec` without sshd in the guest |
| **IP on spec** | The host allocates the pod IP before boot; the guest sets it over netlink without waiting for carrier | Kernel IP autoconfig waited ~25 ms for link carrier |

Containers of a pod share the VM, so they share the pod's network namespace
and IP, as in Kubernetes. Restart policy is implemented in kvz-init with
kubelet-style `CrashLoopBackOff` status.

### Cold boot timeline (single pod, Studio)

```
 0 ms   CreatePod → spec.json written, NIC socketpair created
 1 ms   VZVirtualMachine created
65 ms   VZ start returns (VZ XPC process up, memory mapped, kernel loaded)
82 ms   kernel → /init (17 ms guest time)
84 ms   overlay mounted, NIC configured, container exec'd → "started"
~105–140 ms   pod Ready observed by a watching client (via apiserver)
```

## Pod network (`pkg/podnet`)

Each node runs an in-process L2 switch plus L3 gateway (gVisor netstack via
gvisor-tap-vsock). A pod VM's NIC is one end of a `SOCK_DGRAM` socketpair
handed to `VZFileHandleNetworkDeviceAttachment`.

* **Why not VZ NAT:** every NAT-attached VM creates a kernel `vmenet`
  interface. Hard-stopped VMs leaked them on the Studio until the host
  refused to create more (~251). From then on no VZ guest on that machine,
  kvz's or anyone else's, had networking until reboot. With podnet a NIC is
  just an fd.
* Pod-to-pod traffic is switched in-process. Egress is NATed through host
  sockets by the netstack, and DNS is served by the gateway.
* `kubectl port-forward` dials the pod through the netstack.
* macOS guests use the same network and get DHCP from the in-process server.
* Static pod IPs come from the top half of the CIDR, DHCP from the bottom.

## Bugs found and fixed upstream-style (vendored under `third_party/`)

* **Code-Hex/vz:** vsock connections double-closed the fd owned by
  `VZVirtioSocketConnection`. Under concurrency the second close hit whatever
  file had reused the number (random EBADF, VMs failing to start). Fixed with
  dup.
* **Code-Hex/vz:** `NewFileHandleNetworkDeviceAttachment` keeps no reference
  to the `*os.File`, so Go's finalizer could close a live NIC. kvz owns the
  file for the VM's lifetime.
* **virtual-kubelet:** the grace-period sleep before finalizing deletion
  (above).

## macos runtime
APFS `clonefile` of the VM bundle (a 50 GB disk in under 1 ms), a fresh
machine identifier, a NIC on podnet, DHCP from the in-process server, and
readiness gated on a working SSH login. If container 0 has a command, it runs
over SSH and its exit completes the pod. The kernel limit is 2 concurrent macOS
guests per host (`kvz.dev/macos-vm: 2`).

## darwin runtime
Native processes with a per-pod working dir, process-group kill honoring the
grace period, and restart policy with backoff.

## Limits
* **~127 concurrent VMs per Mac:** a hard Virtualization.framework /
  hypervisor limit, independent of guest RAM, and so a cap on concurrent
  linux pods per node. Native darwin pods aren't affected. Multi-Mac clusters
  scale it out.
* Not implemented yet: Services/kube-proxy, cluster DNS, cross-node pod
  routing, projected SA tokens in pod volumes, stats summary.
