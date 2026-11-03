# Setting up a fleet of Macs

This guide turns several Macs (for example a shelf of Mac minis) into one kvz
cluster that starts on boot. It's written so that a person or an AI agent with
SSH access to the Macs can follow it step by step. Every step says how to check
that it worked before moving on.

If you only have one Mac, [Getting started](getting-started.md) is shorter.

## Contents

1. [Plan the cluster](#1-plan-the-cluster)
2. [Prepare every Mac](#2-prepare-every-mac)
3. [Network](#3-network)
4. [Install kvz on every Mac](#4-install-kvz-on-every-mac)
5. [Start the first server](#5-start-the-first-server)
6. [Add more servers (high availability only)](#6-add-more-servers-high-availability-only)
7. [Join the other Macs as agents](#7-join-the-other-macs-as-agents)
8. [Verify the cluster](#8-verify-the-cluster)
9. [Use the cluster from another computer](#9-use-the-cluster-from-another-computer)
10. [Day-2 operations](#10-day-2-operations)
11. [Troubleshooting](#11-troubleshooting)
12. [Getting help and reporting problems](#12-getting-help-and-reporting-problems)

## 1. Plan the cluster

Every Mac is either a **server** (runs the control plane: etcd, the API server,
the scheduler and controllers, plus a node) or an **agent** (only runs pods).

| Fleet size | Layout | Survives |
|---|---|---|
| 1 to 3 Macs | 1 server, the rest agents | nothing if the server Mac goes down (pods on agents keep running, but nothing can be changed) |
| 3 or more Macs, uptime matters | 3 servers with `--cluster-init`, the rest agents | the loss of any one server |

Use an odd number of servers (1, 3 or 5). Two servers are worse than one: etcd
needs a majority, so losing either of two stops the cluster.

Write down, before you start:

- each Mac's role, SSH address and host name;
- the address the Macs will use to reach each other (see [Network](#3-network));
- which Mac is the **first server**. Every other Mac joins through it.

## 2. Prepare every Mac

Do this on every Mac. All commands run over SSH as the user who will run kvz.
That user must be an administrator, and kvz runs in that user's login session.

### Requirements

```sh
uname -m                 # must print arm64 (Apple Silicon)
sw_vers -productVersion  # must be 26 or later
df -h ~                  # leave at least 20 GB free for images and volumes
```

### A unique name

kvz uses the host name, lowercased and without `.local`, as the node name, so
every Mac needs a different one. Two Macs called `Mac-mini` would fight over
the same node.

```sh
scutil --get LocalHostName
# if it isn't unique:
sudo scutil --set ComputerName  mini-01
sudo scutil --set HostName      mini-01
sudo scutil --set LocalHostName mini-01
```

You can also pass `--node-name` to kvz instead.

### Remote login, sleep and power

```sh
sudo systemsetup -setremotelogin on      # SSH (skip if you're already using it)
sudo pmset -a sleep 0 disksleep 0 displaysleep 0   # never sleep
sudo pmset -a autorestart 1              # power back on after a power cut
sudo pmset -a womp 1                     # wake for network access
```

Check with `pmset -g`: `sleep` must be `0`.

### Automatic login (required)

kvz runs as a launchd service in the user's **GUI login session**, because
Apple's Virtualization.framework, which every pod uses, needs one. A Mac that
reboots and sits at the login window runs no kvz. Turn on automatic login for
the kvz user:

- **System Settings → Users & Groups → Automatically log in as**, or
- over SSH: `sudo sysadminctl -autologin set -userName "$USER" -password '<that user’s password>'`

Automatic login is only possible with **FileVault turned off**
(`fdesetup status`). If FileVault must stay on, someone has to log in at the
console (or over Screen Sharing) after each reboot.

To check that a GUI session exists right now:

```sh
launchctl print gui/$(id -u) >/dev/null && echo "GUI session: yes"
```

## 3. Network

### Which address

Every Mac must reach every other Mac directly. kvz picks the address it
advertises (`--node-ip`) like this: a **Tailscale** address (100.64.0.0/10) if
the Mac has one, otherwise the first IPv4 address on an `en*` interface.

- **Tailscale (recommended for fleets).** Install it on every Mac and log in to
  the same tailnet. kvz uses the Tailscale addresses on its own, they don't
  change when the Macs move, and they avoid macOS's Local Network permission
  (see below). Make sure your tailnet ACL lets the Macs reach each other on the
  ports below.
- **A plain LAN.** Give each Mac a fixed address (a DHCP reservation). If a Mac
  has several interfaces (Wi-Fi and Ethernet, or a VPN), pass `--node-ip` with
  the address the others should use, to the server and to every agent.

### Ports

| Port | Protocol | Open on | Used by |
|---|---|---|---|
| 6443 | TCP | servers | Kubernetes API (`kubectl`, agents) |
| 6444 | TCP | servers | join endpoint (always the API port + 1) |
| 10250 | TCP | every Mac | API server to node: `kubectl logs`, `exec`, metrics |
| 6446 | TCP | every Mac | the pod network between Macs |
| 2380 | TCP | servers (HA only) | etcd between servers (mutual TLS) |
| 30000 to 32767 | TCP/UDP | every Mac | NodePort Services, if you use them |

### macOS firewall and Local Network permission

- If the macOS Application Firewall is on
  (`/usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate`), allow
  kvz after installing it:

  ```sh
  sudo /usr/libexec/ApplicationFirewall/socketfilterfw --add "$(readlink -f "$(which kvz)")"
  sudo /usr/libexec/ApplicationFirewall/socketfilterfw --unblockapp "$(readlink -f "$(which kvz)")"
  ```

- macOS asks for **Local Network** permission the first time a program
  connects to another device on the LAN. On a headless Mac nobody sees that
  prompt, and the connections fail with `no route to host`. If an agent can't
  reach a server's LAN address even though `ping` works, open **System Settings
  → Privacy & Security → Local Network** (over Screen Sharing) and allow kvz,
  or use Tailscale addresses instead.

## 4. Install kvz on every Mac

```sh
brew install kvz-platform/tap/kvz
kvz version
```

Every Mac must run the **same** kvz version; compare the `kvz version` output
across the fleet. Homebrew installs the binary and the small Linux kernel that
pods boot; nothing else is needed. (Without Homebrew, unpack the release tarball
from the [releases page](https://github.com/kvz-platform/kvz/releases) and use
its `bin/kvz`; it finds the kernel in `share/kvz/kernel` next to it.)

Install `tart` too (`brew install cirruslabs/cli/tart`) only if you plan to run
macOS VM pods from OCI images.

## 5. Start the first server

On the first server Mac:

```sh
kvz install server                  # one server
# or, for high availability:
kvz install server --cluster-init
```

Add `--node-ip <address>` if the Mac has more than one possible address, and
`--tls-san <name>` for any extra DNS name you'll use to reach the API server.
`kvz install` writes a launchd service
(`~/Library/LaunchAgents/dev.kvz.server.plist`) that starts now, at every login,
and again if kvz exits. Its log is `~/.kvz/logs/kvz-server.log`.

Wait until the node is Ready (usually a few seconds):

```sh
until kvz kubectl get nodes 2>/dev/null | grep -q ' Ready'; do sleep 1; done
kvz kubectl get nodes -o wide
```

Then collect the two things every other Mac needs:

```sh
cat ~/.kvz/join-token                                    # TOKEN, looks like K10…::…
awk '/server:/{print $2}' ~/.kvz/kubeconfig-external.yaml   # SERVER_URL, https://<address>:6443
```

`SERVER_URL` uses the address this server advertises. Check it's one the other
Macs can reach; if not, reinstall the server with `--node-ip`. Treat the token
like a password: it lets a machine join the cluster.

## 6. Add more servers (high availability only)

Skip this with a single server. On each additional server Mac, **one at a
time**:

```sh
kvz install server --server SERVER_URL --token TOKEN
```

Wait for it to be Ready before adding the next. Check from the first server:

```sh
kvz kubectl get nodes
```

New servers join etcd as learners and are promoted once they have caught up. HA
mode is remembered in each server's data directory, so the service needs no
extra flags after a restart.

## 7. Join the other Macs as agents

On every agent Mac (these can join in parallel):

```sh
kvz install agent --server SERVER_URL --token TOKEN
```

With several servers, any server's URL works: agents learn about every server
and keep a local load balancer in front of them, so they carry on when one goes
down. The agent's log is `~/.kvz/logs/kvz-agent.log`.

## 8. Verify the cluster

Run these on a server Mac (`kvz kubectl` uses `~/.kvz/kubeconfig.yaml` there).

**Every Mac is a Ready node, on the same version:**

```sh
kvz kubectl get nodes -o wide
```

**Pods start on every Mac, and reach each other across Macs:**

```sh
N=$(kvz kubectl get nodes --no-headers | wc -l | tr -d ' ')
kvz kubectl create deployment fleet-check --image=nginx:alpine --replicas=$((N*2))
kvz kubectl expose deployment fleet-check --port=80
kvz kubectl rollout status deployment/fleet-check --timeout=180s
kvz kubectl get pods -l app=fleet-check -o wide      # NODE column: every Mac should appear
for node in $(kvz kubectl get nodes -o name | cut -d/ -f2); do
  kvz kubectl run "net-$node" --rm -i --restart=Never --image=alpine:3.22 \
    --overrides="{\"spec\":{\"nodeName\":\"$node\"}}" -- \
    wget -qO- -T5 http://fleet-check.default.svc.cluster.local | grep -q 'Welcome to nginx' \
    && echo "$node: ok" || echo "$node: FAILED"
done
kvz kubectl delete deployment,service fleet-check
```

Every node must print `ok`. That one check covers pod start-up on each Mac,
cluster DNS, Services and the pod network between Macs.

## 9. Use the cluster from another computer

Each server writes `~/.kvz/kubeconfig-external.yaml`, which points at its own
address. Copy it to your laptop:

```sh
scp mini-01:.kvz/kubeconfig-external.yaml ~/.kube/kvz.yaml
KUBECONFIG=~/.kube/kvz.yaml kubectl get nodes
```

It holds cluster-admin credentials, so keep it private. The API server's
certificate covers the server's address and host name; add more names with
`--tls-san`.

## 10. Day-2 operations

| Task | How |
|---|---|
| Upgrade every Mac | Apply an `upgrade.cattle.io/v1` Plan; kvz drains and upgrades each Mac in turn, servers first, and rolls back a Mac whose new release doesn't start. See [upgrades.md](upgrades.md). This needs a release newer than v1.37.2-2; check with `kvz kubectl api-resources \| grep plans`. On older releases, `brew upgrade kvz` on each Mac, servers first. |
| Back up the datastore | `kvz etcd-snapshot save --name <name>` on a server; automatic snapshots run every 12 hours. See [operations.md](operations.md#backups-and-restores). |
| Add a Mac | Steps 2 to 4, then step 6 or 7. |
| Remove a Mac | `kvz kubectl drain <node> --ignore-daemonsets --delete-emptydir-data`, then on that Mac `kvz uninstall --purge`, then `kvz kubectl delete node <node>`. |
| Reboot a Mac | Just reboot. With automatic login, kvz comes back on its own. |
| Watch logs | `tail -f ~/.kvz/logs/kvz-server.log` (or `kvz-agent.log`) on that Mac. |
| Inspect a node's pod network | `kvz kubectl get --raw /api/v1/nodes/<node>/proxy/debug/kvz/podnet` |
| Registry mirrors and credentials | `~/.kvz/registries.yaml` on every Mac. See [operations.md](operations.md#registry-mirrors). |

All flags and files are listed in [operations.md](operations.md).

## 11. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `kvz install` fails with `launchctl bootstrap: … Bootstrap failed` | The user has no GUI login session | Turn on automatic login (step 2), reboot or log in once, run `kvz install` again |
| kvz stops after a reboot | The Mac is at the login window | Automatic login; FileVault must be off |
| An agent never appears in `get nodes` | It can't reach the server on 6443/6444, or the token is wrong | `nc -vz <server address> 6443` and `nc -vz <server address> 6444` from the agent; read `~/.kvz/logs/kvz-agent.log`; re-copy the token |
| `no route to host` to a LAN address that answers `ping` | macOS Local Network permission | Allow kvz under Privacy & Security → Local Network, or use Tailscale addresses |
| Two Macs show up as one node, or flip back and forth | Duplicate host names | Give each Mac a unique name (step 2) or `--node-name` |
| A node is `NotReady` | That Mac is asleep, offline or kvz isn't running | `pmset -g`, then `launchctl print gui/$(id -u)/dev.kvz.agent` (or `dev.kvz.server`) and the log |
| `kubectl logs` / `exec` fail for pods on one Mac | Port 10250 on that Mac isn't reachable from the servers | Open it in the firewall or ACL |
| Pods on different Macs can't reach each other | Port 6446 is blocked, or `--node-ip` is an address the others can't reach | Open 6446; set `--node-ip` on that Mac and reinstall |
| Pods stay `Pending` on a full Mac | About 120 Linux pods per Mac is the limit (Virtualization.framework caps VMs per host) | Add Macs |
| Pods stuck in `ContainerCreating` | A volume, image or certificate isn't ready | `kvz kubectl describe pod <pod>`: the events say which |
| Everything else | | Run `kvz server --verbose` (or the agent) in a terminal after `kvz uninstall` to watch every component's logs live |

To change a service's flags, run `kvz install server|agent …` again with the
new flags; it replaces the old service.

## 12. Getting help and reporting problems

- **Source code and docs:** <https://github.com/kvz-platform/kvz>
- **Bugs:** <https://github.com/kvz-platform/kvz/issues/new/choose>. Include
  `kvz version`, `sw_vers -productVersion`, the number of Macs and their roles,
  `kvz kubectl get nodes -o wide`, and the relevant part of
  `~/.kvz/logs/kvz-*.log`. **Remove join tokens and kubeconfig contents first.**
- **Questions and ideas:** <https://github.com/kvz-platform/kvz/discussions>
- **Security vulnerabilities:** report privately, never in a public issue:
  <https://github.com/kvz-platform/kvz/security/advisories/new> (see
  [SECURITY.md](../SECURITY.md)).
- **Releases and changelog:** <https://github.com/kvz-platform/kvz/releases>
