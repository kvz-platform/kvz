# Operations

## Commands

| Command | What it does |
|---|---|
| `kvz server` | Runs the control plane and, unless `--disable-agent` is set, a local node |
| `kvz agent --server URL --token TOKEN` | Joins this Mac to a cluster as a node |
| `kvz install server` / `kvz install agent …` | Installs kvz as a launchd service that starts now and at login |
| `kvz uninstall` | Removes the service. Add `--purge` to also delete `~/.kvz` |
| `kvz etcd-snapshot save` / `ls` / `delete` | Manages snapshots of the cluster datastore |
| `kvz kubectl …` | Runs a kubectl that matches the cluster's version |
| `kvz version` | Prints the version |

## Server flags

| Flag | Default | Meaning |
|---|---|---|
| `--https-listen-port` | `6443` | API server port. The join endpoint uses the next port. |
| `--etcd-port` | `2379` | Embedded etcd client port |
| `--disable-agent` | off | Run only the control plane, with no local node |
| `--disable` | none | Skip bundled add-ons: `traefik`, `helm-controller`, `local-storage`, `metrics-server` |
| `--etcd-sync` | `barrier` | How etcd flushes writes: `full`, `barrier` (crash-consistent) or `none` |
| `--etcd-snapshot-interval` | `12h` | How often to take automatic snapshots. `0` turns them off. |
| `--etcd-snapshot-retention` | `5` | How many automatic snapshots to keep |
| `--cluster-reset-restore-path` | none | Restore the datastore from a snapshot before starting |
| `--cluster-init` | off | Start a cluster that more servers can join (high availability) |
| `--server`, `--token` | none | Join an existing cluster as another server |
| `--secrets-encryption` | off | Encrypt Secrets at rest (AES-CBC). Stays on once enabled. |
| `--kube-apiserver-arg`, `--kube-controller-manager-arg`, `--kube-scheduler-arg` | none | Extra flags for a component, as `name=value`; repeatable |
| `--tls-san` | none | Extra names or IPs for the API server certificate |

## Node flags (server and agent)

| Flag | Default | Meaning |
|---|---|---|
| `--node-name` | the Mac's host name | Name of this node |
| `--node-ip` | the Tailscale IP if present, otherwise the main IP | Address other nodes use to reach this one |
| `--data-dir` | `~/.kvz` | Where kvz keeps its state |
| `--default-runtime` | `linux` | Runtime for pods without `runtimeClassName` |
| `--max-pods` | `1000` | Pod capacity of this node |
| `--kubelet-port` | `10250` | Port for `logs`, `exec` and metrics |
| `--overlay-port` | `6446` | Port of the pod network link between nodes |

## Files

| Path | Contents |
|---|---|
| `~/.kvz/kubeconfig.yaml` | Admin kubeconfig for the cluster |
| `~/.kvz/kernel/Image` | The pod kernel built by `kernel/build.sh`. If it's missing, kvz uses the one Homebrew installs (`$(brew --prefix)/share/kvz/kernel/Image`). Set `KVZ_KERNEL` to use another. |
| `~/.kvz/server/manifests/` | YAML here is applied automatically, like k3s |
| `~/.kvz/registries.yaml` | Registry mirrors and credentials (`KVZ_REGISTRIES` overrides the path) |
| `~/.kvz/server/db/snapshots/` | etcd snapshots |
| `~/.kvz/logs/` | Logs from the launchd service |
| `~/Library/LaunchAgents/dev.kvz.server.plist` | The installed service (or `dev.kvz.agent.plist`) |

## Backups and restores

```sh
kvz etcd-snapshot save --name before-upgrade
kvz etcd-snapshot ls
kvz server --cluster-reset-restore-path ~/.kvz/server/db/snapshots/<file>
```

The server also takes a snapshot every 12 hours and keeps the five newest.

## High availability

Start the first server with `--cluster-init`, then join more servers with the
same token. Each server runs the API server, controller-manager, scheduler
and an etcd member; three servers tolerate the loss of one.

```sh
kvz server --cluster-init                                       # first Mac
kvz server --server https://<first-mac>:6443 --token <token>    # each other server
kvz agent  --server https://<any-server>:6443 --token <token>   # worker Macs
```

New servers join etcd as learners and are promoted once they have caught up.
Agents keep a local load balancer in front of every server they know about, so
they keep working when one server goes down. HA mode is remembered in the data
dir, so restarts need no extra flags.

## Registry mirrors

`~/.kvz/registries.yaml` uses the k3s format. Mirrors are tried in order, then
the registry itself:

```yaml
mirrors:
  docker.io:
    endpoint: ["https://mirror.gcr.io"]
configs:
  registry.example.com:
    auth: {username: me, password: secret}
    tls: {ca_file: /path/to/ca.pem}
```

## Helm charts

The bundled Helm controller installs `HelmChart` objects, as in k3s:

```yaml
apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata: {name: podinfo, namespace: kube-system}
spec:
  repo: https://stefanprodan.github.io/podinfo
  chart: podinfo
  targetNamespace: default
```

## Debugging a node

Every node serves Go profiles and a dump of its pod network (switch ports,
addresses and the gateway's ARP cache) through the API server:

```sh
kvz kubectl get --raw /api/v1/nodes/<node>/proxy/debug/kvz/podnet
kvz kubectl get --raw "/api/v1/nodes/<node>/proxy/debug/pprof/goroutine?debug=2"
```

## Upgrades

Upgrading kvz upgrades the embedded etcd too, and etcd can't move a data
directory back to an older version. Take a snapshot before upgrading
(`kvz etcd-snapshot save`) if you might roll back.

## Joining Macs securely

The join token looks like `K10<hash>::<secret>`, the same format as k3s. The
hash pins the cluster's certificate authority, so a new node can verify the
server before it sends the secret. Each node then gets its own certificate,
limited to what a node is allowed to do.

## Uninstalling

```sh
kvz uninstall            # remove the launchd service
kvz uninstall --purge    # also delete ~/.kvz: cluster state, images, kernel and volumes
```
