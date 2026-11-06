# Installation

You need a Kubernetes cluster, `kubectl` and [Helm](https://helm.sh/) 3 or 4.

## The image

The image contains one static binary and is built for `linux/amd64` and `linux/arm64`. No image is published yet, so you have to build it yourself and push it to a registry your cluster can pull from:

```sh
make image IMAGE=ghcr.io/<you>/smokealarm TAG=0.1.0 PUSH=1
```

The multi-arch build needs a buildx builder that supports several platforms; the comment in the [Makefile](../Makefile) explains how to set one up. `make image-local` builds for your own machine only.

## The admin password

There is one local admin user. smokealarm is given the bcrypt hash of the password, not the password itself, so create the hash first:

```sh
read -s PW; printf '%s' "$PW" | docker run -i --rm ghcr.io/<you>/smokealarm:0.1.0 hash-password
```

Without Docker, run `make hash-password` in a checkout. The hash contains `$` characters. Keep it in single quotes or in a variable and pass it with `--set-string`, as below.

## Install the chart

```sh
HASH='<the hash from above>'
helm install smokealarm ./deploy/helm/smokealarm \
  --namespace smokealarm --create-namespace \
  --set image.repository=ghcr.io/<you>/smokealarm \
  --set-string auth.passwordHash="$HASH"
```

The chart puts the hash into a Kubernetes Secret. If you'd rather manage the Secret yourself:

```sh
kubectl -n smokealarm create secret generic smokealarm-auth \
  --from-literal=SMOKEALARM_ADMIN_PASSWORD_HASH="$HASH"
helm install smokealarm ./deploy/helm/smokealarm -n smokealarm \
  --set image.repository=ghcr.io/<you>/smokealarm --set existingSecret=smokealarm-auth
```

The same Secret can also hold `SMOKEALARM_SMTP_USERNAME` and `SMOKEALARM_SMTP_PASSWORD`.

[`deploy/helm/examples/`](../deploy/helm/examples) has values files for common setups: ingress with TLS, mail, minimal permissions and your own Secret. Pass one with `-f deploy/helm/examples/<file>`. The comment at the top of each file has the full command. All chart values are described in [values.yaml](../deploy/helm/smokealarm/values.yaml), and the variables behind them in [Configuration](configuration.md).

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

## Notifications

Without a notification target, state changes only go to the log. Mail, ntfy, Telegram, Slack, Microsoft Teams and a generic webhook are built in. [Notifications](notifications.md) explains how to set up each one.
