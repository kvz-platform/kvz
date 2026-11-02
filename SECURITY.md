# Security policy

kvz runs a Kubernetes control plane and starts virtual machines on your Mac,
so we take security reports seriously.

## Reporting a vulnerability

**Please don't open a public issue.** Report it privately through GitHub:

**[Report a vulnerability](https://github.com/kvz-platform/kvz/security/advisories/new)**

Include what you found, how to reproduce it, the kvz version (`kvz version`)
and your macOS version. If you have a fix in mind, describe it, but don't open
a public pull request for it.

## What to expect

- We aim to acknowledge your report within **3 business days**.
- We'll keep you updated while we investigate, and agree on a disclosure date
  with you. We aim to release a fix within **90 days**, sooner for severe issues.
- Once a fix is released, we publish a GitHub security advisory and credit you,
  unless you'd rather stay anonymous.

## Supported versions

Only the **latest release** gets security fixes. kvz releases are cumulative,
so upgrading (`brew upgrade kvz`) is how you pick up a fix.

## Scope

In scope: the `kvz` binary and everything it runs, including the control plane,
the node agent, the pod network, the join protocol and tokens, the guest init
inside pod VMs, and the release artifacts and Homebrew formula.

Out of scope: vulnerabilities in upstream Kubernetes, etcd or macOS itself.
Report those to their projects. If kvz makes one of them worse or easier to
reach, that part is in scope.
