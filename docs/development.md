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

[.github/workflows/ci.yml](../.github/workflows/ci.yml) runs `make test`, `make chart` and golangci-lint on every pull request, on every push to `main` and on every version tag. After that it builds the image for `linux/amd64` and `linux/arm64`. A pull request only builds it; `main` pushes `ghcr.io/gagaschu/smokealarm:main` and `:sha-<commit>`.

A release is a Git tag:

1. Set `version` and `appVersion` in [Chart.yaml](../deploy/helm/smokealarm/Chart.yaml) to the new version and write the release notes in [CHANGELOG.md](../CHANGELOG.md), in a pull request.
2. After the merge, tag that commit on `main` and push the tag: `git tag v0.1.0 && git push origin v0.1.0`.

The tag `v0.1.0` pushes `:0.1.0`, `:0.1` and `:latest`. A pre-release such as `v0.2.0-rc.1` pushes only `:0.2.0-rc.1`. The workflow fails before building if the tag does not match the chart, because the chart uses `appVersion` as its default image tag.

