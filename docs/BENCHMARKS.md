# kvz benchmarks (final, 2026-11-02)

| host | chip | cores | RAM | macOS | notes |
|---|---|---|---|---|---|
| studio | M5 Max | 18 | 36 GB | 27.0.1 | busy: ~26 unrelated OrbStack containers running throughout |
| amelia, derek, lexie | M4 | 10 | 16 GB | 26.6.2 | wiped beforehand; some background jobs from other projects (QEMU VM on amelia, `powermetrics`/Python on derek/lexie) |

**Platforms**
* **kvz:** this repo, final build. Native control plane; Linux pods are kvz
  microVMs, one cold-booted VM per pod.
* **k3s in Lima:** k3s in a Lima Virtualization.framework VM, containerd
  pods. 12 CPU / 16 GiB on the Studio, 8 CPU / 12 GiB on the minis.
* **Apple container cluster:** Apple `container cluster` 1.6.1 (a kind node
  image in one Apple container VM), same sizing.
* **kvz v1:** kvz before tonight's rebuild (Linux pods via Apple's `container`
  daemon), kept as a baseline.

**Rules.** One platform runs at a time per host, all driven by the same
client-go benchmark binary (`cmd/kvzbench`). Images are pulled before the timed
runs on every platform (a warm-up pod). **No caching for speed:** no VM pools,
snapshots or pre-started pods; every kvz Linux pod boots a fresh VM. "Ready" is
the pod's Ready condition, seen by a watching client through the apiserver.
Memory is macOS `phys_footprint` summed over each platform's processes,
including its VMs. Values are medians across repetitions. "M4" is the median
of the three minis.

Raw data: `results/<host>/*.jsonl` · all tables: `results/SUMMARY.md` ·
charts: `results/report.html` · harness: `bench/run.py`, `bench/net.py`,
`bench/multinode.sh`, `bench/macos_vm.py`, `cmd/vmbench`.

## 1. Lifecycle (M4 median · Studio)

| metric | kvz | k3s in Lima | Apple cluster | kvz v1 |
|---|---|---|---|---|
| fresh start → API ready | **3.9 s · 1.6 s** | 17.8 s · 9.6 s | 58.6 s · 58.9 s | 4.3 s · 2.2 s |
| restart → API ready | **1.7 s · 2.0 s** | 9.3 s · 9.1 s | n/a | 2.1 s · 2.1 s |
| idle CPU (% of a core) | **1.6 % · 1.1 %** | 20.1 % · 9.9 % | 26.1 % · 14.2 % | 1.6 % · 0.9 % |
| idle memory | **257 MB · 276 MB** | 3,192 MB · 2,782 MB | 2,453 MB · 2,598 MB | 250 MB · 279 MB |
| 1 nginx pod → Ready | **99 ms · 94 ms** | 421 ms · 403 ms | 859 ms · 873 ms | 756 ms · 575 ms |
| 20 nginx pods → all Ready | **0.44 s · 0.23 s** | 1.30 s · 0.97 s | 0.88 s · 0.93 s | 12.2 s · 9.8 s |
| 20 pods: ready p50 | **395 ms · 221 ms** | 1,196 ms · 885 ms | 864 ms · 903 ms | 6,174 ms · 5,060 ms |
| host memory with 20 pods | **1,558 MB · 1,578 MB** | 3,773 MB · 3,571 MB | 3,009 MB · 3,196 MB | 6,882 MB · 6,959 MB |
| delete 20 pods → gone | **0.11 s · 0.06 s** | 0.81 s · 1.07 s | 1.18 s · 1.13 s | 2.84 s · 4.38 s |
| 50 nginx pods → all Ready | **1.51 s · 0.62 s** | 4.48 s · 4.46 s | 4.20 s · 4.32 s | 30.9 s · 25.7 s |
| host memory with 50 pods | **3,375 MB · 3,401 MB** | 4,390 MB · 4,390 MB | 3,494 MB · 3,763 MB | 14,512 MB · 16,003 MB |
| delete 50 pods → gone | **0.28 s · 0.15 s** | 4.22 s · 3.93 s | 3.72 s · 3.46 s | 7.1 s · 4.5 s |
| 100 nginx pods → all Ready | 3.7 s · 2.6 s | not run | not run | not run |
| `kubectl run --rm -i` round trip (Studio) | **0.12 s** | 2.0 s | not run | not run |

## 2. API server (2000 × 512 B ConfigMaps, 32 clients; M4 median · Studio)

| metric | kvz | k3s in Lima | Apple cluster | kvz v1 |
|---|---|---|---|---|
| creates/s | **9,614 · 14,531** | 3,200 · 4,501 | 7,974 · 8,826 | 3,273 · 4,527 |
| create p99 | 13.1 ms · **9.5 ms** | 70.8 ms · 68.4 ms | **12.8 ms** · 10.0 ms | 22.0 ms · 17.4 ms |
| gets/s | **18,950 · 24,490** | 9,939 · 15,558 | 15,045 · 14,988 | 14,420 · 9,371 |
| get+update/s | **5,821 · 5,251** | 2,341 · 3,234 | 4,737 · 4,923 | 1,822 · 2,273 |
| deletes/s | **8,077 · 8,212** | 2,989 · 3,799 | 7,305 · 7,318 | 1,715 · 2,522 |
| LIST 2000 p50 | **4.4 ms · 4.3 ms** | 8.5 ms · 7.0 ms | 7.2 ms · 6.4 ms | 5.6 ms · 5.0 ms |
| create → watch event p50 | **0.40 ms · 0.40 ms** | 0.84 ms · 0.82 ms | 0.52 ms · 0.65 ms | 6.0 ms · 4.0 ms |

kvz runs etcd with `F_BARRIERFSYNC` (crash-consistent write ordering) by
default. With `--etcd-sync=full` (`F_FULLFSYNC`, a full drive-cache flush on
every commit) create→watch is ~4 ms. The VM-based platforms' guest fsync never
reaches the drive cache that way.

## 3. Native macOS workloads (kvz only; nothing else can run these)

| metric | M4 median | Studio |
|---|---|---|
| darwin process pod → Ready | 46 ms in-suite (12–19 ms standalone) | 43 ms |
| 200 darwin pods → all Ready | 2.7 s | 3.3 s |
| 500 darwin pods → all Ready | 7.7 s | 8.8 s |
| macOS VM pod → SSH-ready | 18.2 s | 18.1 s (13.6–16.9 s in isolated runs) |
| 2 macOS VMs at once → both ready | 37 s (noisy minis) | 22.4 s |

macOS guests run at 97 % of host single-thread CPU and 95 % of host memcpy
(`cmd/cpubench`). Boot reaches sshd as fast as `tart run` on the same image.

## 4. Pod-to-pod network (iperf3, same node)

| host | kvz 1 / 4 streams | k3s 1 / 4 | Apple cluster 1 / 4 | ping kvz / k3s / Apple |
|---|---|---|---|---|
| lexie (M4) | 52.5 / 28.3 Gbps | 83.9 / 110.3 Gbps | 86.4 / 163.6 Gbps | 0.39 / 0.10 / 0.06 ms |
| derek (M4) | 48.6 / 30.9 Gbps | n/a | n/a | 0.40 ms |
| amelia (M4, noisy) | 32.6 / 25.0 Gbps | n/a | n/a | 0.40 ms |
| Studio | 38.5 / 28.4 Gbps | 79.9 / 117.8 Gbps | n/a (vmnet exhausted) | 0.23 / 0.08 ms |

kvz loses here by design: each pod is its own VM, so frames cross
Virtualization.framework and kvz's userspace switch. The competitors' pods
share one kernel. The 4-stream number is bound by the guest's single iperf3
thread (alpine 3.18 ships iperf3 3.14).

## 5. Multi-Mac cluster (Studio server + 3 mini agents)

| workload | all Ready | delete |
|---|---|---|
| 200 Linux pods (40–79 per Mac, each its own VM) | **1.68 s** | 1.69 s |
| 100 Linux pods | 1.66 s | 0.91 s |
| 1000 darwin pods | 10.3 s | 14.2 s |
| 8 macOS VMs (2 per Mac) | 50.3 s | 1.5 s |

## 6. Reliability

20 rounds × 50 pods (1,000 pod VM lifecycles) on the Studio: **zero failures**,
every round 0.55–0.71 s, no leaked VMs, file descriptors flat (195 → 196). kvz
RSS grew from 301 to 736 MB (Go heap and apiserver watch caches).

## 7. Raw microVM path (`cmd/vmbench`, no Kubernetes, Studio)

| VMs booted concurrently | 1 | 20 | 40 | 60 | 80 |
|---|---|---|---|---|---|
| all containers started | 84 ms | 225 ms | 407 ms | ~640 ms | 880 ms |

Guest-side: kernel → `/init` in 17 ms, container exec'd 3 ms later. The rest
is Virtualization.framework's per-VM start (~65 ms; CPU-bound under bursts).
Hard limit: ~127 concurrent VMs per Mac (kvz queues beyond 120).
