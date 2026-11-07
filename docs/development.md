# Local development

You need Go (the version in [go.mod](../go.mod)), `golangci-lint`, and Docker and Helm for the image and chart tasks.

```sh
make test        # go vet and all tests, including the chart tests (skipped without helm)
make test-race   # the same with the race detector (on Linux this needs a C compiler)
make lint        # golangci-lint
make build       # bin/smokealarm
make chart       # lint and render the Helm chart: the defaults and every file in deploy/helm/examples
make screenshots # take the images in docs/screenshots again (needs Chrome or Chromium, and pngquant)

export SMOKEALARM_ADMIN_PASSWORD_HASH="$(make -s hash-password)"
make run         # runs against the cluster from KUBECONFIG, UI on http://localhost:8080
```

`make run` sets the user to `admin` and uses plain-text logs and a cookie that works over HTTP. The [variables](configuration.md) override all of this.

`pvc-usage` then calls the kubelets from your machine, with the credentials of your kubeconfig, so the node IPs must be reachable from there. Otherwise the check is skipped with the reason on the status page.

Use a throw-away cluster or namespace to try things out. For example, `kubectl create deployment brokendep --image=does-not-exist/nope` produces a critical finding after `SMOKEALARM_ALERT_AFTER`.

`make help` lists all tasks. The code is meant to be readable without knowing Go; [ARCHITECTURE.md](../ARCHITECTURE.md) is the place to start.

## CI and releases

[.github/workflows/ci.yml](../.github/workflows/ci.yml) runs `make test`, `make chart` and golangci-lint on every pull request, on every push to `main` and on every version tag. After that it builds the image for `linux/amd64` and `linux/arm64`. A pull request only builds it. `main` pushes `ghcr.io/gagaschu/smokealarm:sha-<commit>` for every commit, and moves `:main` to it if no newer commit has landed on `main` in the meantime (runs of several quick merges can finish out of order).

[.forgejo/workflows/build.yml](../.forgejo/workflows/build.yml) is for a self-hosted Forgejo that pull-mirrors this repository: every branch that arrives there is built for both architectures and pushed to that Forgejo's registry as `:sha-<commit>` and `:br-<branch>`, so a branch can be tried in a test cluster before it is merged. *Run workflow* in the Forgejo UI builds a branch again without a new commit. It only builds; tests run on GitHub. GitHub ignores the directory, and Forgejo ignores `.github/workflows` because it exists. The comment at the top of the file lists what the Forgejo side has to provide.

Every pull request with a change users notice adds a line under `## [Unreleased]` in [CHANGELOG.md](../CHANGELOG.md), in the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) groups (Added, Changed, Deprecated, Removed, Fixed, Security). Refactoring, tests and CI need no entry.

A release is a Git tag:

1. In a pull request: set `version` and `appVersion` in [Chart.yaml](../deploy/helm/smokealarm/Chart.yaml) to the new version, rename `## [Unreleased]` in CHANGELOG.md to `## [x.y.z] - <date>` with a new empty `## [Unreleased]` above it, and update the compare links at the bottom.
2. After the merge, tag that commit on `main` and push the tag: `git tag v0.1.0 && git push origin v0.1.0`.

The tag `v0.1.0` pushes `:0.1.0` and `:0.1`, `:latest` if it is the highest release tag (a fix `v0.1.1` after `v0.2.0` leaves `latest` on 0.2.0), and after the image the chart as `oci://ghcr.io/gagaschu/charts/smokealarm` version `0.1.0`. Chart versions are only published from release tags, never from `main`. A pre-release such as `v0.2.0-rc.1` pushes only the image `:0.2.0-rc.1`, no chart; try it from a checkout with `--set image.tag=0.2.0-rc.1`. The workflow fails before building if the tag does not match the chart, because the chart uses `appVersion` as its default image tag.

