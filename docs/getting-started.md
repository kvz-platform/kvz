# Getting started

This guide takes you from installing kvz to a cluster running all three kinds
of pods. It takes a few minutes.

## What you need

- An **Apple Silicon Mac** (M1 or later) running **macOS 26 or later**.
- **`kubectl`**, or use the one built into kvz (`kvz kubectl`).
- **`tart`**, only if you want macOS pods from OCI images.

## 1. Install kvz

```sh
brew install kvz-platform/tap/kvz
```

This installs `kvz` and the small Linux kernel that pods boot (about 9 MB).
Update later with `brew upgrade kvz`.

Without Homebrew, download `kvz-<version>-darwin-arm64.tar.gz` from the
[releases page](https://github.com/kvz-platform/kvz/releases), unpack it and run
`bin/kvz` from inside it (it finds the kernel in `share/kvz/kernel` next to it):

```sh
tar -xzf kvz-v1.37.2-2-darwin-arm64.tar.gz
cd kvz-v1.37.2-2-darwin-arm64
xattr -dr com.apple.quarantine .   # only if a browser downloaded it
./bin/kvz server
```

Early releases are ad-hoc signed rather than notarized, so a browser download
needs the `xattr` line before macOS will run it. Homebrew and `curl` downloads
don't.

## 2. Or build from source

Skip this if you used Homebrew. To build from source you also need Go, the
Xcode command line tools, and Apple's `container` tool (used once, to build the
kernel).

```sh
git clone https://github.com/kvz-platform/kvz && cd kvz
make build                 # bin/kvz, signed for Virtualization.framework
./kernel/build.sh          # the pod kernel, into ~/.kvz/kernel/Image
```

The steps below write `kvz`. From a source build, that's `./bin/kvz`.

## 3. Start the cluster

```sh
kvz server
```

kvz starts etcd, the API server, the scheduler, the controllers and a local
node, all in this one process. When it's ready it prints:

```
kvz server ready in 1.7s
  export KUBECONFIG=/Users/you/.kvz/kubeconfig.yaml
  join: kvz agent --server https://192.168.1.20:6443 --token <token in /Users/you/.kvz/join-token>
  logs: /Users/you/.kvz/kvz.log (--verbose to stream them here)
```

Copy the `export` line into another terminal. The `join` line is for adding
more Macs later. It doesn't show the join token itself, so the output is safe
to share; the token is in the file it names. The Kubernetes components log to
`kvz.log` rather than the terminal; run `kvz server --verbose` to see them
live. (When kvz runs as a service, or its output goes to a file or pipe, the
full logs go to that output as usual.)

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

On each extra Mac, install kvz and run the `join` command the server printed,
with the token from the server's `~/.kvz/join-token`:

```sh
kvz agent --server https://192.168.1.20:6443 --token K10…::…
```

The new Mac shows up in `kubectl get nodes`, and pods spread across all of
them. Pods on different Macs can reach each other and use the same Services.

For more than a couple of Macs (headless Mac minis, high availability, ports,
automatic login, verification), follow [Fleet setup](fleet.md).

## 8. Run kvz in the background

To keep kvz running after you close the terminal, and start it at login:

```sh
kvz install server        # or: kvz install agent --server … --token …
```

This installs a launchd service and starts it. Logs go to `~/.kvz/logs`. Remove
it with `kvz uninstall`.

## Next

- [Runtimes](runtimes.md): what each kind of pod is and how to pick one
- [Kubernetes features](kubernetes-features.md): what works today
- [Operations](operations.md): flags, snapshots and uninstalling
