# kvz overnight report (2026-11-02)

**Goal:** a k3s-compatible Kubernetes that runs natively on macOS, much faster
than anything else on a Mac, with no caching tricks. No warm VM pools, no
snapshot/restore, no pre-started pods. Every Linux pod is a cold-booted VM.

**Result:** kvz beats k3s-in-Lima and Apple's `container cluster` on every
lifecycle metric measured on Apple Silicon (startup, idle cost, API, pod start,
burst, teardown, memory), usually by 3–16×. It also runs workloads neither of
them can: native macOS processes and macOS VMs as pods. Where it loses
(pod-to-pod bandwidth) is explained below.

Full tables: [`results/SUMMARY.md`](../results/SUMMARY.md). Charts:
[`results/report.html`](../results/report.html). Methodology:
[`BENCHMARKS.md`](BENCHMARKS.md). Design: [`ARCHITECTURE.md`](ARCHITECTURE.md).

---

## 1. Headline (M4 Mac mini, median of 3 machines; Studio in brackets)

From the final full run (`BENCHMARKS.md` has every metric):

| | kvz | k3s in Lima | Apple container cluster |
|---|---|---|---|
| restart → API ready | **1.7 s** [2.0 s] | 9.3 s | 58 s (create) |
| idle CPU / memory | **1.6 % / 257 MB** | 20 % / 3.2 GB | 26 % / 2.5 GB |
| API creates/s | **9.6k** [14.5k] | 3.2k [4.5k] | 8.0k [8.8k] |
| create → watch p50 | **0.40 ms** | 0.84 ms | 0.52 ms |
| 1 nginx pod → Ready | **99 ms** [94 ms] | 421 ms | 859 ms |
| 20 nginx pods → all Ready | **0.44 s** [0.23 s] | 1.30 s [0.97 s] | 0.88 s [0.93 s] |
| 50 nginx pods → all Ready | **1.51 s** [0.62 s] | 4.48 s | 4.20 s |
| host memory with 20 pods | **1.56 GB** | 3.77 GB | 3.01 GB |
| delete 20 pods | **0.11 s** [0.06 s] | 0.81 s | 1.18 s |
| `kubectl run --rm -i` round trip | **0.12 s** [Studio] | 2.0 s | n/a |
| pod-to-pod TCP, 1 stream | 33–52 Gbps [38.5] | **84 Gbps** [80] | **86 Gbps** |
| native macOS process pod → Ready | **12–46 ms** | n/a | n/a |
| macOS VM pod → SSH-ready | **13–20 s** | n/a | n/a |

## 2. What changed tonight: from 12 s to 0.25 s

The v1 Linux runtime delegated to Apple's `container` daemon, which boots
micro-VMs **serially at ~2/s per host** no matter the client concurrency
(measured: 16 starts take 7.2–7.9 s at concurrency 1, 2, 4, 8 and 16). So
20 pods took ~10–12 s. Throwing away that dependency and rebuilding the
path from the ground up:

| step | what | 20 pods → Ready (Studio) | single pod |
|---|---|---|---|
| v1 | Apple `container` daemon per container | 9.81 s | 575 ms |
| 1 | **kvz's own microVM monitor** (Virtualization.framework, concurrent boots), Go PID 1, virtiofs rootfs + tmpfs overlay, vsock events | 0.74–0.80 s | ~226 ms (raw VM) |
| 2 | **kvz kernel**: Linux 6.18 LTS from allnoconfig + only pod needs, 8.5 MB (Apple's is a 30 MB debug build). Kernel → `/init` 106 → 17 ms | **0.27 s** | 122 ms |
| 3 | network config moved from kernel IP-PNP (waited 25 ms for carrier) into init via netlink; one multi-dir virtiofs device instead of one per share (each virtio device ≈7 ms) | (included above) | |
| 4 | **kvz pod network** (in-process switch + gVisor netstack) replaces VZ NAT: no per-VM kernel interfaces, no leaks, no cap | 0.27 s | |
| 5 | loopback kubeconfig (the Tailscale route cut API throughput 3×), 1 ms queue base delay | **0.25 s** | **~90 ms** |

Teardown went from 4.4 s (v1) and 1.2 s (step 1) to **0.05 s** for 20 pods
once virtual-kubelet stopped sleeping the full grace period after the provider
had already stopped the pod (patched), and once pod exits during deletion
stopped being treated as `CrashLoopBackOff`.

Raw VM path (no Kubernetes), Studio: one cold VM to "container started" in
**84 ms**, 20 in 225 ms, 40 in 407 ms, 80 in 0.88 s.

### Other hyper-optimizations

| area | before | after | how |
|---|---|---|---|
| API create→watch | 4.0 ms | **0.42 ms** | etcd WAL `F_BARRIERFSYNC` instead of `F_FULLFSYNC` (crash-consistent; `--etcd-sync=full` available) |
| API creates/s (Studio) | 4.5k | **15.7k** | loopback kubeconfig, barrier fsync |
| native pods, 50 → Ready | 6.8 s | **0.42 s** | replaced client-go's 10 qps default limiter on virtual-kubelet's queues |
| native pod → Ready | 39 ms | **12–19 ms** | 1 ms queue base delay |
| scheduling 200 pods | 1.3 s p90 | ms-range | scheduler API QPS 50 → 500 |
| pod-to-pod TCP | 4.5 Gbps | **38.5 Gbps** [Studio], 49–54 [M4] | 64 KiB MTU (frame cost dominates), then kvz's own lock-light L2 switch for pod↔pod (gvisor's switch took two global mutexes per frame) |
| macOS VM ready | 13–85 s (erratic) | **13.6–16.9 s** | no unauthenticated SSH probes: OpenSSH PerSourcePenalties on macOS 26 throttled our banner probes |
| macOS VM delete | 5.1 s | grace-bounded | guest shutdown bounded by the pod's grace period (disk is a throwaway clone) |

## 3. Kubernetes compatibility added tonight

- ClusterIP **Services** via kube-proxy-style DNAT/conntrack in the pod switch
  (TCP/UDP, verified load balancing across replicas), **cluster DNS**
  (`<svc>.<ns>.svc.cluster.local`, search path, `ndots:5`), **LoadBalancer /
  NodePort** exposed on the host (ServiceLB-style).
- **Projected service-account tokens** (TokenRequest, bound to the pod) and the
  `kubernetes` Service: in-cluster `kubectl get pods` works from a pod.
- `status.podIP` / `status.hostIP` env refs (pod IP pre-assigned before env
  resolution, as the kubelet does), restartPolicy with real `CrashLoopBackOff`
  semantics, `kubectl exec` (pty), `logs -f`, `port-forward`, `attach`
  (output), `run --rm -i`.
- Verified workloads: Deployments (scale, rollout), StatefulSets, Jobs,
  ConfigMap/Secret/emptyDir/hostPath/projected volumes, nginx, redis, alpine,
  bitnami/kubectl.

## 4. Where kvz loses, and why

* **Pod-to-pod bandwidth.** In k3s and Apple's cluster all pods share one Linux
  kernel, so traffic between them is a memory copy through a kernel bridge
  (80–164 Gbps). kvz pods are separate VMs, so frames cross
  Virtualization.framework through a userspace switch: 30–54 Gbps single
  stream, about 1.6–2.8× lower. Latency is 0.2–0.5 ms vs 0.06–0.1 ms. It's the
  price of per-pod VM isolation. Per-frame syscall cost now dominates, and
  64 KiB frames are the ceiling.
* **≈127 concurrent VMs per Mac** (hard Virtualization.framework limit,
  independent of guest RAM). kvz gates at 120 and queues the rest with
  `WaitingForVMSlot`. For comparison, the kubelet's default `max-pods` is 110.
  Native darwin pods are unaffected, and multi-Mac clusters scale it out (200
  Linux pods across 4 Macs: 1.68 s).
* **Big bursts are CPU-bound.** Virtualization.framework spends ~65 ms per VM
  start in its own XPC process. The Studio's 18 cores do 100 pods in ~1.75 s;
  the M4's 10 cores take 2.3–4.7 s.
* **macOS guests** take 13–20 s to boot (macOS itself). Two at a time per host
  is a kernel limit.

## 5. Bugs found (and fixed in vendored copies under `third_party/`)

1. **Code-Hex/vz vsock double-close.** `newVirtioSocketConnection` wrapped the
   fd owned by `VZVirtioSocketConnection` in an `*os.File` and closed it, so the
   same fd number was closed twice. Under concurrency the second close hit
   whatever file had reused the number: random `EBADF`, VMs failing to start.
   Fixed with `dup`. Worth reporting upstream.
2. **Code-Hex/vz** `NewFileHandleNetworkDeviceAttachment` keeps no reference
   to the `*os.File`, and VZ doesn't dup the fd, so Go's finalizer can close a
   live NIC. kvz now owns the file for the VM's lifetime.
3. **virtual-kubelet** sleeps `DeletionGracePeriodSeconds` before finalizing a
   deleted pod even when the provider is done; doesn't support `status.*`
   field refs; resolves env before any pod IP exists; uses `go-cmp`
   reflection on every status notify.
4. **macOS vmnet:** NAT-attached VMs that are hard-stopped leak `vmenet`
   interfaces. On the Studio they reached ~251 and the host stopped creating
   new ones, so no VZ guest, kvz's or anyone else's (tart, Apple
   `container`), gets a network until reboot. kvz no longer uses vmnet at all.
5. **Apple `container cluster`:** writes `~/.kube/config` and switches the
   current context unconditionally; its published API port resets connections
   once the creating CLI exits.

## 6. Reliability

1,000 pod VM lifecycles (20 × 50) on the Studio: zero failures, each round
0.55–0.71 s, no leaked VMs or file descriptors.

## 7. Operational notes

* The **Studio's vmnet is exhausted until it reboots** (item 4 above, caused
  by tonight's VM churn before the pod network existed). kvz is unaffected; tart
  and Apple `container` on the Studio need a reboot to get networking back.
* Your kube context is still `breezy`; the harness snapshots and restores
  `~/.kube/config` around Apple's cluster tool.
* The minis' background jobs (a QEMU VM and Shortcuts automation on amelia;
  `powermetrics`/Python on derek and lexie) add noise. Results are medians
  across the three minis.
* OrbStack's Kubernetes couldn't be benchmarked headlessly (its first start
  needs a GUI TCC permission). Your Studio's OrbStack was left untouched.
