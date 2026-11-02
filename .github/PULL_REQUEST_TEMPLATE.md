## What this changes

<!-- One or two sentences. Link the issue it fixes, if any: "Fixes #123". -->

## Why

<!-- The problem, or what a user can do now that they couldn't before. -->

## How it was tested

- [ ] `make lint` and `make test`
- [ ] `make e2e` on an Apple Silicon Mac (or explain why it doesn't apply)
- [ ] Benchmarks, if this touches pod start, the API server or the pod network
      (`bench/run.py`, before and after numbers in the description)

## Checklist

- [ ] The PR title follows [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/),
      for example `fix(podnet): …` (it becomes the release note)
- [ ] Docs in `docs/` are updated if behavior or flags changed
- [ ] No secrets, tokens or personal data in code, logs or screenshots
