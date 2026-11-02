# kvz — native-macOS Kubernetes

A k3s-style, single-binary Kubernetes distribution that runs **natively on macOS** — no Linux VM for the
control plane — and schedules pods onto three native runtimes:

| RuntimeClass | What a pod is | How |
|---|---|---|
| `linux` (default) | a Linux micro-VM per container, own routable IP | Apple `container` / Containerization framework |
| `macos` | a full macOS VM (container 0) | Virtualization.framework, APFS `clonefile` disk, SSH exec |
| `darwin` | native macOS processes | `fork/exec`, per-pod dir, pgid kill |

Benchmarks and design notes: see [`docs/`](docs/) and [`results/`](results/). (WIP)
