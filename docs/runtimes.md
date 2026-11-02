# Runtimes

Every kvz node runs pods in one of four runtimes. You pick one with the pod's
`runtimeClassName`. kvz creates the matching RuntimeClass objects for you when
the server starts.

| `runtimeClassName` | A pod is | Ready in | Use it for |
|---|---|---|---|
| `linux` (default) | its own Linux microVM | about 100 ms | ordinary container images |
| `macos` | a full macOS virtual machine | 15 to 20 s | macOS CI, testing on a clean macOS |
| `darwin` | native macOS processes on the host | about 15 to 50 ms | Xcode, code signing, GPU work |
| `apple` | a container run by Apple's `container` tool | about 0.6 to 0.8 s | the original v1 runtime, kept for comparison |

## linux

This is the default, so pods without a `runtimeClassName` land here.

Each pod boots its own small VM through Apple's Virtualization.framework:

- **Kernel and init:** the kvz kernel (8.5 MB) and a tiny init program that
  starts your container.
- **Filesystem:** the image is shared into the VM read-only, with a writable
  layer on top in memory.
- **Network:** each pod gets its own IP address on kvz's pod network.

Nothing is pre-started or cached: every pod is a cold boot, and it still
reaches Ready in about 100 ms.

**Limits.**

- **VMs per Mac:** Virtualization.framework allows about 127 VMs per Mac. kvz
  runs up to 120 pod VMs per node and queues the rest with the reason
  `WaitingForVMSlot`. Add more Macs to go past that.
- **Pod-to-pod bandwidth:** because pods don't share a kernel, traffic between
  them is slower than in a single-VM cluster. It measures 30 to 50 Gbps per
  stream, against about 85 Gbps elsewhere.

## macos

A macOS pod is a whole macOS virtual machine. The image can be:

- a local VM bundle in tart's layout (`disk.img`, `nvram.bin`, `config.json`),
- a path to such a bundle, or
- an OCI image reference, which kvz pulls with `tart pull`.

When the pod starts, kvz clones the disk with APFS `clonefile`. That's instant
even for a 50 GB disk, and the clone is thrown away when the pod is deleted. The
pod becomes Ready once the guest answers SSH. `kubectl exec` and `kubectl logs`
run over that SSH connection.

If the first container sets a `command`, kvz runs it inside the guest. Its exit
code ends the pod, which makes macOS pods work well as Jobs.

**Limits.** macOS allows two macOS VMs per Mac. Extra macOS pods wait with
`WaitingForVMSlot`.

## darwin

A native pod runs its containers as ordinary processes on the Mac itself, with:

- **A working directory:** each pod gets its own, passed in as `$KVZ_POD_DIR`.
- **Restarts:** handled with the pod's `restartPolicy`, backing off like the
  kubelet does.
- **Clean shutdown:** deleting the pod stops its whole process group, honoring
  the grace period.

Native pods must set `command`. The `image` field is required by Kubernetes, but
this runtime doesn't use it.

Native pods share the host's network, so they use the Mac's own IP address.

## apple

This was kvz's first Linux runtime. It runs each container through Apple's
`container` daemon. That daemon starts VMs one at a time, at about two per
second, so it's much slower under load than `linux`. It's kept mainly as a
benchmark baseline.

## Choosing a default

The node flag `--default-runtime` sets the runtime for pods without a
`runtimeClassName`. It defaults to `linux`.
