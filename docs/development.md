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
