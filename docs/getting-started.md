# Getting started

This guide takes you from a fresh clone to a cluster running all three kinds of
pods. It takes about ten minutes, most of it building the pod kernel once.

## What you need

- An **Apple Silicon Mac** (M1 or later) running **macOS 26 or later**.
- **Go** and the **Xcode command line tools** (`xcode-select --install`), to
  build kvz.
- **Apple's `container` tool**, used once to build the pod kernel.
- **`kubectl`**, or use the one built into kvz (`kvz kubectl`).
- **`tart`**, only if you want macOS pods from OCI images.

## 1. Build kvz

```sh
git clone https://github.com/GrowlyX/kvz && cd kvz
make build
```

This produces `bin/kvz` and signs it with the virtualization entitlement it
needs to start VMs.

## 2. Build the pod kernel

Linux pods boot a small kernel made for kvz (Linux 6.18, about 8.5 MB). Build it
once:

```sh
./kernel/build.sh
```

The kernel lands in `~/.kvz/kernel/Image`. If it's missing, kvz falls back to
the kernel that ships with Apple's `container`, which works but boots slower.

## 3. Start the cluster

```sh
./bin/kvz server
```

kvz starts etcd, the API server, the scheduler, the controllers and a local
node, all in this one process. When it's ready it prints two things:

```
kvz server ready in 1.7s
  export KUBECONFIG=/Users/you/.kvz/kubeconfig.yaml
  join: kvz agent --server https://192.168.1.20:6443 --token K10…::…
```

Copy the `export` line into another terminal. The `join` line is for adding
more Macs later.

## 4. Run a Linux pod

```sh
export KUBECONFIG=~/.kvz/kubeconfig.yaml
kubectl run web --image=nginx:alpine
kubectl get pods -o wide
```

The pod gets its own cold-booted VM and its own IP address. Pods without a
`runtimeClassName` use the Linux runtime.

## 5. Run a native macOS pod

Native pods are plain macOS processes. They're useful for anything that needs
the host itself, such as Xcode, code signing or the GPU.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: hello-mac
spec:
  runtimeClassName: darwin
  restartPolicy: Never
  containers:
    - name: main
      image: host          # not pulled; native pods run on the host
      command: ["/usr/bin/sw_vers"]
```

```sh
kubectl apply -f hello-mac.yaml
kubectl logs hello-mac
```

Native pods must set `command`. Each pod gets its own working directory, passed
in as `$KVZ_POD_DIR`.

## 6. Run a macOS VM pod

A macOS pod is a full macOS virtual machine. Its disk is a copy-on-write clone,
so starting one is instant on disk, and the VM is ready for `exec` once SSH
answers (about 15 to 20 seconds).

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: mac
spec:
  runtimeClassName: macos
  containers:
    - name: vm
      image: ghcr.io/cirruslabs/macos-tahoe-base:latest
```

```sh
kubectl apply -f mac.yaml
kubectl exec mac -- sw_vers
```

macOS allows two macOS VMs per Mac. Extra macOS pods wait with the reason
`WaitingForVMSlot` until a slot frees up.

## 7. Add more Macs

On each extra Mac, build kvz and run the `join` command the server printed:

```sh
./bin/kvz agent --server https://192.168.1.20:6443 --token K10…::…
```

The new Mac shows up in `kubectl get nodes`, and pods spread across all of
them. Pods on different Macs can reach each other and use the same Services.

## 8. Run kvz in the background

To keep kvz running after you close the terminal, and start it at login:

```sh
./bin/kvz install server        # or: kvz install agent --server … --token …
```

This installs a launchd service and starts it. Logs go to `~/.kvz/logs`. Remove
it with `kvz uninstall`.

## Next

- [Runtimes](runtimes.md): what each kind of pod is and how to pick one
- [Kubernetes features](kubernetes-features.md): what works today
- [Operations](operations.md): flags, snapshots and uninstalling
