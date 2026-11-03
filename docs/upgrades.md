# Automated upgrades

kvz upgrades itself across a cluster from `upgrade.cattle.io/v1` **Plan**
objects, the same API as the k3s
[system-upgrade-controller](https://docs.k3s.io/upgrades/automated). Plans
written for k3s apply unchanged; kvz runs the controller itself, so there is
nothing to install.

```yaml
apiVersion: upgrade.cattle.io/v1
kind: Plan
metadata:
  name: kvz
  namespace: system-upgrade
spec:
  concurrency: 1
  cordon: true
  drain: {force: true, timeout: 5m}
  version: v1.35.10-2          # or: channel: https://github.com/kvz-platform/kvz/releases/latest
```

```sh
kvz kubectl create namespace system-upgrade
kvz kubectl apply -f plan.yaml
kvz kubectl -n system-upgrade get plans -o wide
```

## What happens

1. The server resolves the Plan's `version`. A `channel` resolves to the last
   path element of the URL it redirects to, so GitHub's `releases/latest`
   works. Channels are checked every 15 minutes.
2. It picks the nodes matching `nodeSelector` (all nodes if unset).
   Control-plane nodes go first, as with k3s: workers don't start until every
   control-plane node in the Plan is done. Then up to `concurrency` nodes
   (default 1) run at a time.
3. With `cordon` or `drain`, it cordons the node, and with `drain` evicts its
   pods (`kubectl drain` semantics: `force`, `timeout`, `gracePeriod`,
   `deleteEmptydirData`, `ignoreDaemonSets` (default true), `disableEviction`,
   `podSelector`).
4. The node's kvz downloads
   `kvz-<version>-darwin-arm64.tar.gz` and checks it against its `.sha256`.
   It then checks the new binary itself: a valid code signature, the
   virtualization entitlement, the same signing team as the running binary
   (when that one is Developer ID signed), and that `kvz version` reports the
   expected release.
5. It swaps `bin/kvz` and, for Homebrew and tarball installs, the pod kernel
   in `share/kvz/kernel`. Each rename is atomic, and the old files are kept as
   `*.prev`. Then kvz restarts.
6. When the node is Ready on the new release, the server uncordons it (unless
   it was cordoned before), labels it `plan.upgrade.cattle.io/<plan>=<hash>`
   and moves on.

A node already on the Plan's version is just labeled. Upgrading the server
restarts the API server; the controller picks up where it left off, because
its progress is kept on the Node objects.

## Restarts and rollback

Run kvz as a service (`kvz install server|agent`). Under launchd, kvz exits
after installing the new binary and launchd starts it again. That also makes
rollback possible: a newly installed binary that starts three times without its
node becoming Ready puts the previous files back and restarts into them. The
node then reports
`Failed: <version> started 3 times without its node becoming Ready; rolled
back to <previous>`, and the Plan stays incomplete until you change its
version.

In a terminal, kvz starts the new binary itself and exits, so the new kvz runs
in the background. There is nothing to restart a binary that keeps crashing,
so there is no automatic rollback in that mode.

## Status

```sh
kvz kubectl -n system-upgrade get plans -o wide           # Complete and a per-node message
kvz kubectl get nodes -L plan.upgrade.cattle.io/kvz
kvz kubectl get node <node> -o jsonpath='{.metadata.annotations}'
```

| Annotation on the node | Set by | Meaning |
|---|---|---|
| `kvz.dev/release` | node | The kvz release the node runs (set once it's Ready on it) |
| `upgrade.kvz.dev/plan` | server | The Plan upgrading this node |
| `upgrade.kvz.dev/apply`, `upgrade.kvz.dev/url` | server | Install this release from this URL (set after the drain) |
| `upgrade.kvz.dev/status` | node | `Downloading`, `Verifying`, `Restarting into …`, `Failed: …` |

A failed download or check is retried every 2 minutes and leaves the node
cordoned, as upstream does. Deleting a Plan releases its nodes and uncordons
the ones it had cordoned.

## kvz-specific settings

| Plan annotation | Meaning |
|---|---|
| `upgrade.kvz.dev/release-url` | Tarball URL template, e.g. `https://mirror.example.com/kvz-{version}-darwin-arm64.tar.gz`. The checksum must be at the same URL plus `.sha256`. |
| `upgrade.kvz.dev/allow-downgrade: "true"` | Allow moving to a lower release number. Downgrades are refused by default, because etcd can't open a data directory written by a newer version. Take a snapshot first. |

Fields that only make sense for upstream's Job-based upgrades (`upgrade.image`,
`prepare`, `serviceAccountName`, `tolerations`, `secrets`) are accepted and
ignored.

## Notes

- Nodes running a kvz without this feature can't upgrade themselves. Upgrade
  them once by hand (`brew upgrade kvz`, or the release tarball).
- A server started with `--disable-agent` has no node, so Plans can't select
  it. Upgrade it by hand.
- Homebrew installs are upgraded in place inside the Cellar. Homebrew still
  lists the old version until the next `brew upgrade kvz`, which installs the
  same files.
- If you run the upstream system-upgrade-controller instead, start servers
  with `--disable upgrade-controller`.
