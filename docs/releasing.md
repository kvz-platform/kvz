# Releasing and CI

## Version numbers

A kvz release is named after the Kubernetes version it embeds, plus a release
number:

```
v1.35.6-12
 └──┬──┘ └┬┘
    │     └─ release number: one higher than the last release, never resets
    └─────── the upstream Kubernetes version inside (K8S_VERSION in the Makefile)
```

- **Upgrading Kubernetes** changes the first part: `v1.35.6-12` is followed by
  `v1.35.7-13`.
- **A kvz-only fix** changes only the number: `v1.35.6-12` is followed by
  `v1.35.6-13`.
- **Sorting:** the release number alone orders every release, across Kubernetes
  versions.
- **The binary:** `kvz version` prints both parts:
  `kvz v1.35.6-12 (Kubernetes v1.35.6)`. Builds that aren't from a release tag
  report `v1.35.6-dev+<commit>`.

In strict semver, a hyphen marks a pre-release, so semver tools would sort
`v1.35.6-12` before `v1.35.6`. Homebrew, Go and GitHub don't apply that rule,
and kvz never publishes a bare `v1.35.6`, so this doesn't come up in practice.

## Cutting a release

1. Make sure the `main` build is green in Buildkite.
2. On that build, press **Release**. The next step runs
   `hack/release/tag.sh`, which works out the next version
   (`hack/release/next-version.sh`) and pushes the tag.
3. The tag starts a release build that:
   1. builds the pod kernel (`kernel/build.sh`),
   2. builds `kvz` with the version stamped in,
   3. signs it with the Developer ID certificate (hardened runtime plus the
      virtualization entitlement) and notarizes it with Apple,
   4. packages `kvz-<version>-darwin-arm64.tar.gz` (the binary and the kernel),
   5. creates the GitHub release, with notes generated from Conventional Commit
      subjects (`hack/release/notes.sh`),
   6. updates `Formula/kvz.rb` in
      [kvz-platform/homebrew-tap](https://github.com/kvz-platform/homebrew-tap).

Users then get it with `brew upgrade kvz`.

To preview the next version and its notes locally:

```sh
hack/release/next-version.sh
hack/release/notes.sh "$(hack/release/next-version.sh)"
```

## Buildkite setup

### Pipeline

The pipeline is [buildkite.com/kvz/kvz](https://buildkite.com/kvz/kvz), in the
org's Default cluster. Its only built-in step is
`buildkite-agent pipeline upload`. Everything else lives in
[`.buildkite/pipeline.yml`](../.buildkite/pipeline.yml). It builds branches,
pull requests and tags, and reports commit statuses to GitHub.

GitHub triggers builds through the **Buildkite GitHub App**, installed on the
`kvz-platform` org. The app also gives hosted agents access to clone the repo
while it's private.

### Agents

| Queue | Machines | Runs |
|---|---|---|
| `macos-medium` | Buildkite-hosted Macs (macOS 26, Xcode 26) | lint, unit tests, build, tagging a release |
| `macos` | our own Apple Silicon Macs | end-to-end tests and every release step |

The end-to-end tests and releases can't run on hosted Macs. They boot real VMs
with Virtualization.framework, and hosted Macs are VMs themselves, which can't
nest another macOS-level VM host. They also need Apple's `container` (to build
the kernel) and the Developer ID keychain.

Each self-hosted Mac needs macOS 26 or later, and:

- **The Buildkite agent**, started with `tags="queue=macos"` and run as a
  LaunchAgent in the logged-in user's session, not a LaunchDaemon, because
  Virtualization.framework needs a GUI session. The agent token is in the
  maintainer's keychain:

  ```sh
  brew install buildkite/buildkite/buildkite-agent
  TOKEN=$(security find-generic-password -s buildkite-agent-token-kvz -w)
  sed -i '' "s/^token=.*/token=\"$TOKEN\"/; s/^#* *tags=.*/tags=\"queue=macos\"/" \
    "$(brew --prefix)/etc/buildkite-agent/buildkite-agent.cfg"
  brew services start buildkite/buildkite/buildkite-agent
  ```

- **Tools:** Go (the version in `go.mod`), the Xcode command line tools, `git`
  and `gh`.
- **Apple's `container` tool**, for building the pod kernel.
- **The pod kernel** at `~/.kvz/kernel/Image`, for end-to-end tests.
- **Optionally, macOS VM tests:** `tart`, plus `KVZ_E2E_MACOS_IMAGE` in the
  agent environment.
- **Git access** to `kvz-platform/kvz`, such as an SSH key added as a read-only
  deploy key, while the repo is private.

The end-to-end step runs one cluster per Mac at a time
(`concurrency_group: kvz/e2e/<agent>`). Don't run a personal kvz on CI Macs,
because the pod network uses fixed subnets.

### Secrets

Store these as [Buildkite secrets](https://buildkite.com/docs/pipelines/security/secrets/buildkite-secrets).
The steps read them with `buildkite-agent secret get`.

| Secret | Used for | What it is |
|---|---|---|
| `GITHUB_TOKEN` | pushing the release tag, creating the GitHub release | fine-grained token for `kvz-platform/kvz` with contents: read and write |
| `HOMEBREW_TAP_TOKEN` | updating the formula | fine-grained token for `kvz-platform/homebrew-tap` with contents: read and write |
| `APPLE_SIGNING_IDENTITY` | code signing | the certificate name, such as `Developer ID Application: Example Inc (TEAMID1234)` |
| `APPLE_API_KEY` | notarization | contents of an App Store Connect API key (`.p8`) with the Developer role |
| `APPLE_API_KEY_ID` | notarization | that key's ID |
| `APPLE_API_ISSUER` | notarization | your App Store Connect issuer ID |

The **Developer ID Application certificate and its private key** must also be
in a keychain the agent user can read, because `codesign` uses the keychain,
not a file. Import it once on each release agent:

```sh
security import developer-id.p12 -k ~/Library/Keychains/login.keychain-db -T /usr/bin/codesign
```

Only the release steps (tag builds and the **Tag release** step on `main`) read
secrets. Pull request builds never do.

## Homebrew tap

[kvz-platform/homebrew-tap](https://github.com/kvz-platform/homebrew-tap) holds
`Formula/kvz.rb`. The release build writes it from
[`packaging/homebrew/kvz.rb.tmpl`](../packaging/homebrew/kvz.rb.tmpl), so
change the template in this repo, not the formula in the tap.

The formula installs a prebuilt, signed and notarized binary, plus the pod
kernel at `share/kvz/kernel/Image`. kvz finds the kernel there on its own.
Release assets must be publicly downloadable for `brew install` to work, which
means the `kvz` repository has to be public.
