# Installation

You need a Kubernetes cluster, `kubectl` and [Helm](https://helm.sh/) 3 or 4.

## The image

The image contains one static binary and is built for `linux/amd64` and `linux/arm64`. It is published as `ghcr.io/gagaschu/smokealarm`, and the chart uses it by default, with the chart's `appVersion` as the tag. The tags:

| Tag | What it is |
| --- | --- |
| `0.1.0`, `0.1` | a release, and the newest release of that minor version |
| `latest` | the highest release |
| `main`, `sha-<commit>` | the newest commit on `main` and every single commit on `main`; not a release |
| `0.2.0-rc.1` | a pre-release; image only, no chart |

To build the image yourself and push it to your own registry:

```sh
make image IMAGE=ghcr.io/<you>/smokealarm TAG=0.1.0 PUSH=1
```

Then add `--set image.repository=ghcr.io/<you>/smokealarm` to the `helm install` commands below. The multi-arch build needs a buildx builder that supports several platforms; the comment in the [Makefile](../Makefile) explains how to set one up. `make image-local` builds for your own machine only.

## The admin password

There is one local admin user. smokealarm is given the bcrypt hash of the password, not the password itself, so create the hash first:

```sh
read -s PW; printf '%s' "$PW" | docker run -i --rm ghcr.io/gagaschu/smokealarm:latest hash-password
```

Without Docker, run `make hash-password` in a checkout. The hash contains `$` characters. Keep it in single quotes or in a variable and pass it with `--set-string`, as below.

## Install the chart

The chart is published as `oci://ghcr.io/gagaschu/charts/smokealarm`, in the same version as the image; Helm 3.8 or newer can install it directly. Without `--version` Helm takes the newest release; add `--version 0.1.0` to pin one. `helm show values oci://ghcr.io/gagaschu/charts/smokealarm` prints all values. From a checkout, `./deploy/helm/smokealarm` works in place of the `oci://` address.

```sh
HASH='<the hash from above>'
helm install smokealarm oci://ghcr.io/gagaschu/charts/smokealarm \
  --namespace smokealarm --create-namespace \
  --set-string auth.passwordHash="$HASH"
```

The chart puts the hash into a Kubernetes Secret. If you'd rather manage the Secret yourself:

```sh
kubectl -n smokealarm create secret generic smokealarm-auth \
  --from-literal=SMOKEALARM_ADMIN_PASSWORD_HASH="$HASH"
helm install smokealarm oci://ghcr.io/gagaschu/charts/smokealarm -n smokealarm \
  --set existingSecret=smokealarm-auth
```

The same Secret can also hold `SMOKEALARM_SMTP_USERNAME` and `SMOKEALARM_SMTP_PASSWORD`.

[`deploy/helm/examples/`](../deploy/helm/examples) has values files for common setups: ingress with TLS, mail, minimal permissions and your own Secret. Pass one with `-f <file>`: download it from the repository, or use `-f deploy/helm/examples/<file>` in a checkout. The comment at the top of each file has the full command. All chart values are described in [values.yaml](../deploy/helm/smokealarm/values.yaml), and the variables behind them in [Configuration](configuration.md).

## Open it

```sh
kubectl -n smokealarm port-forward svc/smokealarm 8080:80
```

Open <http://localhost:8080> and log in as `admin`. The login cookie is only sent over HTTPS by default. If the login doesn't stick over the port-forward, install with `--set config.cookieSecure=false` for local use.

For permanent access, turn on the ingress:

```yaml
# my-values.yaml
ingress:
  enabled: true
  className: nginx
  host: smokealarm.example.com
  tls:
    enabled: true
    secretName: smokealarm-tls
```

Read [Security](security.md) before you make the UI reachable from outside your network.

## Outside the cluster

smokealarm can also run outside the cluster it watches, so it still reports a cluster that fails as a whole: with Docker Compose or systemd on a host, or in another cluster. See [Outside the cluster](outside-cluster.md).

## Notifications

Without a notification target, state changes only go to the log. Mail, ntfy, Telegram, Slack, Microsoft Teams and a generic webhook are built in. [Notifications](notifications.md) explains how to set up each one.
