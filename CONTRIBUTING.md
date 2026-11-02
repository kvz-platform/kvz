# Contributing to kvz

Thanks for helping. This guide covers setting up, testing and sending a change.

## Set up

You need an Apple Silicon Mac on macOS 26 or later, Go (the version in
`go.mod`), the Xcode command line tools, and Apple's `container` tool (to build
the pod kernel once).

```sh
git clone https://github.com/kvz-platform/kvz && cd kvz
make build              # bin/kvz, signed for Virtualization.framework
./kernel/build.sh       # the pod kernel, into ~/.kvz/kernel/Image
```

## Test

| Command | What it runs | Needs |
|---|---|---|
| `make lint` | gofmt and `go vet` | nothing special |
| `make test` | unit tests | nothing special |
| `make e2e` | starts a real kvz on spare ports and checks Linux pods, native pods, and Services with DNS | the pod kernel, and no other kvz running |

Set `KVZ_E2E_MACOS_IMAGE=ghcr.io/cirruslabs/macos-tahoe-base:latest` to also
test macOS VM pods (slower, needs `tart`).

If your change touches pod start, the API server or the pod network, run the
benchmarks before and after (`bench/run.py`, see
[docs/BENCHMARKS.md](docs/BENCHMARKS.md)) and put both sets of numbers in the
pull request.

## Send a change

1. Open an issue first for anything larger than a small fix, so we can agree on
   the approach.
2. Keep each pull request to one change. Update `docs/` when behavior or flags
   change.
3. Write commits and the PR title as
   [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/):

   ```
   feat(microvm): restartPolicy with kubelet-style CrashLoopBackOff
   fix(build): add go.sum entries for embedded kubectl
   perf(podnet): default MTU 65520
   ```

   Types: `feat`, `fix`, `perf`, `refactor`, `docs`, `test`, `build`, `ci`,
   `chore`, `style`, `revert`. Scopes name the area, such as `microvm`,
   `podnet`, `macos`, `linux`, `etcd`, `bench`, `netpol`, `storage`, `images`,
   `metrics` or `ops`. Release notes are generated from these subjects, so
   write them for users.
4. Buildkite runs lint, tests, a build and the end-to-end tests on Apple
   Silicon Macs. A maintainer reviews and merges.

## Releases

Maintainers cut releases from `main`. See [docs/releasing.md](docs/releasing.md).

## Conduct and security

Everyone in the project follows the [code of conduct](CODE_OF_CONDUCT.md).
Report security problems privately, as described in [SECURITY.md](SECURITY.md).
