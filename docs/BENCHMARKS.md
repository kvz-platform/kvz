# kvz benchmarks

Run on 2026-11-02 across four Apple Silicon Macs:

| host | chip | cores | RAM | macOS | notes |
|---|---|---|---|---|---|
| studio | M5 Max | 18 | 36 GB | 27.0.1 | ~26 unrelated OrbStack containers running throughout (noisy) |
| amelia, derek, lexie | M4 | 10 | 16 GB | 26.6.2 | wiped before the runs, otherwise idle (clean) |

Raw data: `results/<host>/*.jsonl`. Every table: `results/SUMMARY.md`
(regenerate with `bench/report.py`). Harness: `bench/run.py` (one platform per
run, nothing else running from the harness), `cmd/kvzbench` (client-go; the
same binary for every platform), `bench/macos_vm.py`, `cmd/cpubench`,
`bench/container_scaling.py`.

**Platforms compared**
* **kvz**: this repo. The control plane runs natively in-process; pods run on
  Apple `container` (linux), Virtualization.framework (macos), or as processes
  (darwin).
* **lima-k3s**: k3s v1.3x in a Lima VZ VM (template `k3s`), containerd pods.
  Sized 12 CPU / 16 GiB on the studio, 8 CPU / 12 GiB on the minis.
* **apple-cluster**: Apple `container cluster` 1.6.1, a kind node image
  (kubeadm, etcd, containerd) in one Apple container VM, same sizing as Lima.

Memory is macOS `phys_footprint` summed over the platform's processes,
including its VMs. For VMs that's the guest memory actually touched.

---

## TL;DR

| | kvz | k3s in Lima | Apple `container cluster` |
|---|---|---|---|
| Restart → API ready (M4 median) | **2.0 s** | 9.3 s | 58 s (create only) |
| Fresh cluster → API ready (M4) | **4.1 s** | 17.8 s (VM boot) | 58 s |
| Idle CPU (M4) | **1.6 %** | 20 % | 26 % |
| Idle memory (M4) | **~250 MB** | 3.2 GB | 2.5 GB |
| API create p99 (M4) | 22 ms | 70 ms | **13 ms** |
| API create→watch p50 (M4) | 6 ms (durable fsync) / 0.4 ms (no fsync, Studio) | 0.8 ms | **0.5 ms** |
| 1 nginx pod → Ready | 740 ms | **410 ms** | 860 ms |
| 20 nginx pods → all Ready (M4) | 12 s | 1.3 s | **0.9 s** |
| Memory with 20 nginx pods (M4) | 6.9 GB | 3.8 GB | **3.0 GB** |
| Native macOS process pod → Ready | **48 ms** | n/a | n/a |
| 500 native pods → all Ready (M4) | **5.6 s** | n/a | n/a |
| macOS VM pod → SSH ready | **16–20 s** | n/a | n/a |
| 8 macOS VMs on 4 Macs | **25 s** | n/a | n/a |

**Where kvz wins:** cold start (4×), idle CPU (12–16×), idle memory (10–13×),
tail latency on API creates, and anything native: macOS process pods,
macOS VM pods, multi-Mac clusters.

**Where kvz loses:** Linux pod density and burst start (8–14× slower, ~2× the
memory). Every kvz Linux container is its own micro-VM, and Apple's container
daemon boots those VMs one at a time. The VM-based platforms pack all pods
into one VM. kvz also loses on durable-write latency (see fsync below).

---

## 1. Control-plane startup

| host | kvz fresh | kvz restart | lima-k3s first boot | lima-k3s restart | apple-cluster create |
|---|---|---|---|---|---|
| studio | 2.53 s | 2.17 s | 9.63 s | 9.13 s | 58.9 s |
| amelia | 4.07 s | 1.95 s | 17.25 s | 9.24 s | 58.8 s |
| derek | 3.84 s | 2.04 s | 18.02 s | 9.36 s | 58.6 s |
| lexie | 4.44 s | 2.07 s | 17.81 s | 9.31 s | 57.1 s |

Times are to API `/readyz`. Node Ready follows within ~30 ms for kvz. A kvz
fresh start includes CA/PKI generation and etcd bootstrap.

## 2. Idle cost (empty cluster, 30 s sample after 20 s settle)

| host | kvz CPU / mem | lima-k3s CPU / mem | apple-cluster CPU / mem |
|---|---|---|---|
| studio | 0.9 % / 279 MB | 9.9 % / 2,782 MB | 14.2 % / 2,598 MB |
| amelia | 1.6 % / 240 MB | 20.8 % / 3,194 MB | 27.5 % / 2,446 MB |
| derek | 1.5 % / 366 MB | 20.1 % / 3,192 MB | 26.1 % / 2,453 MB |
| lexie | 1.7 % / 250 MB | 20.0 % / 3,169 MB | 25.7 % / 2,473 MB |

CPU is % of one core. kvz's figure includes Apple's idle container daemons.

## 3. API server (2000 × 512 B ConfigMaps, 32 concurrent clients)

| host | platform | create/s | create p99 | get/s | get+update/s | create→watch p50 | delete/s |
|---|---|---|---|---|---|---|---|
| studio | kvz | 4,526 | 17 ms | 9,371 | 2,273 | 4.0 ms | 2,522 |
| studio | kvz `--etcd-unsafe-no-fsync` | 5,868 | 15 ms | 7,993 | 3,206 | **0.37 ms** | 6,015 |
| studio | lima-k3s | 4,500 | 68 ms | 15,558 | 3,234 | 0.82 ms | 3,799 |
| studio | apple-cluster | 8,826 | 10 ms | 14,988 | 4,923 | 0.65 ms | 7,318 |
| M4 ×3 (median) | kvz | 3,272 | 22 ms | 14,420 | 1,822 | 6.0 ms | 1,715 |
| M4 ×3 (median) | lima-k3s | 3,200 | 71 ms | 9,939 | 2,341 | 0.84 ms | 2,989 |
| M4 ×3 (median) | apple-cluster | 7,974 | 13 ms | 15,044 | 4,736 | 0.52 ms | 7,304 |

**The fsync story.** On darwin, Go's `File.Sync` (and so etcd's WAL fsync)
issues `F_FULLFSYNC`, a real flush of the SSD's write cache. Inside a Linux
VM, guest fsync never reaches the drive that way, so the VM-based platforms
look faster while being less durable on power loss. With
`--etcd-unsafe-no-fsync`, kvz matches or beats both on watch latency and
deletes. kvz defaults to durable; a future `F_BARRIERFSYNC` mode (ordering
without a full flush, as SQLite on macOS uses) is the likely middle ground.

## 4. Linux pods (nginx:alpine, image pre-pulled)

| host | platform | 1 pod p50 | 20 pods all Ready | 20 pods p50 | mem w/ 20 | 50 pods all Ready | mem w/ 50 |
|---|---|---|---|---|---|---|---|
| studio | kvz | 575 ms | 9.8 s | 5.1 s | 6.96 GB | 25.7 s | 16.0 GB |
| studio | lima-k3s | 403 ms | 0.97 s | 0.89 s | 3.57 GB | 4.5 s | 4.39 GB |
| studio | apple-cluster | 873 ms | 0.93 s | 0.90 s | 3.20 GB | 4.3 s | 3.76 GB |
| M4 (median) | kvz | 756 ms | 12.2 s | 6.2 s | 6.89 GB | 30.9 s | 14.5 GB |
| M4 (median) | lima-k3s | 421 ms | 1.30 s | 1.20 s | 3.77 GB | 4.5 s | 4.39 GB |
| M4 (median) | apple-cluster | 859 ms | 0.88 s | 0.86 s | 3.01 GB | 4.2 s | 3.49 GB |

**Why kvz is slow here.** `bench/container_scaling.py` starts 16 containers
directly with `container run -d`, no kvz involved:

| client concurrency | 1 | 2 | 4 | 8 | 16 |
|---|---|---|---|---|---|
| starts/s | 2.03 | 2.16 | 2.22 | 2.18 | 2.16 |
| per-start p50 | 0.49 s | 0.89 s | 1.75 s | 3.41 s | 7.01 s |

Apple's daemon serializes VM boots at ~2.1/s per host. `container create` is
45 ms and fully parallel; `container start` (0.45 s) is the serialized part.
kvz runs at that ceiling. Its start gate (2 in flight) keeps throughput the
same but makes pods Ready FIFO: 20-pod p50 drops from 9.5 s to 5.1 s.
Getting past this requires driving Containerization directly (see Next steps).

## 5. Native darwin pods (`runtimeClassName: darwin`, `sleep 3600`)

| host | 1 pod p50 | 200 pods all Ready | 500 pods all Ready | 500 pods/s | mem w/ 500 |
|---|---|---|---|---|---|
| studio | 39 ms | 2.1 s | 8.0 s | 63 | 559 MB |
| amelia | 53 ms | 1.6 s | 5.5 s | 90 | 480 MB |
| derek | 48 ms | 1.6 s | 5.6 s | 89 | 520 MB |
| lexie | 56 ms | 1.5 s | 5.6 s | 90 | 467 MB |

Two tuning fixes found along the way:
1. virtual-kubelet's default work-queue limiter (10 qps) took 50 pods from
   0.42 s to 6.8 s; kvz replaces it.
2. kube-scheduler's default API QPS (50) made scheduling the long pole at 200
   pods; kvz raises it to 500.

## 6. macOS VM pods (`runtimeClassName: macos`, cirruslabs tahoe-base, 26.6.2 guest)

| host | 1 VM → Ready p50 (min–max) | 2 VMs → both Ready | host mem w/ 2 VMs |
|---|---|---|---|
| studio (8 GiB guests) | 18.8 s (16.1–22.3) | 21.0 s | 16.6 GB |
| amelia (4 GiB) | 15.9 s (11.5–23.0) | 25.0 s | 8.8 GB |
| derek (4 GiB) | 19.7 s (17.0–22.3) | 23.3 s | 8.8 GB |
| lexie (4 GiB) | 17.8 s (15.3–18.2) | 23.0 s | 8.8 GB |

Boot phases on the studio, from the kvz log. The 50 GB disk clone uses
`clonefile` and takes under 1 ms.

| phase | kvz | `tart run` (same image) |
|---|---|---|
| clone | 0.3–0.5 ms | 43–577 ms (`tart clone`) |
| VM start | 0.16 s | n/a |
| DHCP lease | 5.9–6.0 s | 6.1–6.2 s |
| sshd banner | 10.0–11.1 s | 8.7–11.0 s |
| SSH login works (kvz Ready) | 13.2–14.8 s | n/a |

Boot is at parity with tart. The remaining 2.5–5 s is kvz waiting until a
password login succeeds, so `kubectl exec` works the moment the pod is Ready.

**Guest performance** (`cmd/cpubench`, the same binary in the guest and on
the host):

| | guest, 4 vCPU | host M5 Max | ratio |
|---|---|---|---|
| sha256, 1 thread | 3,065 MB/s | 3,149 MB/s | 97 % |
| sha256, all threads | 12,619 MB/s (3,155 per vCPU) | 44,022 MB/s | per-vCPU = native P-core |
| memcpy | 55.7 GB/s | 58.7 GB/s | 95 % |
| 2 GB write, buffered | 0.36 s | 0.35 s | ~same |
| 2 GB write + sync | 1.40 s | 0.35 s | ~4× slower |

## 7. Multi-Mac cluster (studio server, plus amelia, derek and lexie as agents)

Agents joined with `kvz agent --token …`. Pods are scheduled from one API
server onto 4 nodes.

| workload | all Ready | rate | placement |
|---|---|---|---|
| 1000 native pods | **5.8 s** | 172 pods/s | studio 181, amelia 102, derek 604, lexie 113 |
| 80 Linux nginx pods | **17.3 s** | 4.6 pods/s (2.3× one node) | studio 32, others 16 each (CPU-weighted) |
| 8 macOS VMs (2 per Mac) | **25.4 s** | n/a | 2 / 2 / 2 / 2 via `kvz.dev/macos-vm` |
| teardown 1000 / 80 / 8 | 14.3 s / 4.9 s / 6.5 s | | |

`kubectl logs` and `kubectl exec` work against pods on every node.

---

## Operational findings (worth knowing if you run any of this)

* **Local Network privacy.** A process started over SSH that outlives the
  session can't reach local subnets, including the VM networks
  `192.168.64/65.x`. It gets EHOSTUNREACH or hangs. Run agents and benchmarks
  from a live session, a LaunchAgent, or the console user's context.
* **Apple `container` over SSH** needs the console user's launchd bootstrap:
  `sudo launchctl asuser <uid> sudo -u <user> container …`.
* **`container cluster create` rewrites `~/.kube/config`** and switches the
  current context, with no opt-out. Its published API port (127.0.0.1:6445)
  resets connections once the creating CLI exits; use the node's VM IP.
* **Lima k3s template**, `container cluster`, and kvz all need ~nothing extra
  on macOS 26+. kvz needs the binary ad-hoc signed with
  `com.apple.security.virtualization`.

## Next steps

1. **Bypass the serialized VM boot.** A small Swift helper on
   Containerization's `LinuxContainer` / VZ APIs could boot VMs in parallel;
   VZ itself has no such limit, and macOS VM pods boot two at once fine.
   Target: 10+ Linux pods/s per Mac.
2. **Shared-VM Linux runtime class** for density: one Apple container VM
   running containerd, behind a CRI shim. That gives k3s-like packing while
   the control plane stays native.
3. **`F_BARRIERFSYNC` etcd mode**, to close most of the write-latency gap
   without giving up crash consistency.
4. Services/kube-proxy and CoreDNS, cross-node pod routing, port-forward.
