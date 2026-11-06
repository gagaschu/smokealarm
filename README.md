# smokealarm

> A smoke alarm for your Kubernetes cluster – silent until something's on fire.

**Built with AI.** Most of the code and the docs in this repository were written by Claude Code (Anthropic). I wrote the spec, reviewed every step and test it on my own cluster.

smokealarm runs inside your cluster and tells you what is broken right now. It runs a fixed set of checks that need no configuration, and shows current problems and their history on a web page that also works on a phone. When a problem starts or ends, it tells you by mail, through ntfy, Telegram, Slack or Microsoft Teams, or with a webhook to anything else.

![The overview with three critical problems and two warnings](docs/screenshots/overview.png)

The list of checks is short on purpose, so that each one can be reliable. smokealarm has no metrics database, no graphs and no query language, so it won't replace Prometheus. It doesn't collect logs or traces, and it doesn't do root-cause analysis: it can tell you that a pod is crash-looping, but finding out why is up to you.

It was developed and tested with k3s on a Raspberry Pi (arm64). Nothing in it depends on k3s, so it runs on any standard Kubernetes.

## Quickstart

You need a Kubernetes cluster, `kubectl`, [Helm](https://helm.sh/) 3 or 4 and Docker. The image is published as `ghcr.io/gagaschu/smokealarm` for `linux/amd64` and `linux/arm64`, and the chart uses it by default.

Create the bcrypt hash of the admin password:

```sh
read -s PW; printf '%s' "$PW" | docker run -i --rm ghcr.io/gagaschu/smokealarm:0.1.0 hash-password
```

Install the chart. The hash contains `$` characters, so keep it in single quotes:

```sh
HASH='<the hash>'
helm install smokealarm ./deploy/helm/smokealarm \
  --namespace smokealarm --create-namespace \
  --set-string auth.passwordHash="$HASH" \
  --set config.cookieSecure=false   # only for the port-forward below
```

Open it and log in as `admin`:

```sh
kubectl -n smokealarm port-forward svc/smokealarm 8080:80
```

<http://localhost:8080> now shows what is broken. To get notified, add a target, for example mail:

```yaml
# my-values.yaml
smtp:
  host: smtp.example.com
  from: smokealarm@example.com
  to: [me@example.com]
  username: smokealarm
  password: "..."
```

```sh
helm upgrade smokealarm ./deploy/helm/smokealarm -n smokealarm --reuse-values -f my-values.yaml
```

To try it, break something on purpose: `kubectl create deployment brokendep --image=does-not-exist/nope` is reported as critical after two minutes.

For a permanent setup with ingress and TLS, and without `cookieSecure=false`, see [Installation](docs/installation.md).

## Documentation

- [Installation](docs/installation.md): image, password, chart, your own Secret, ingress
- [How it works](docs/how-it-works.md): when a problem is reported, the web UI, acknowledging, downtimes, failed nodes, a lost API connection
- [The checks](docs/checks.md): what is checked, and how to ignore things
- [Notifications](docs/notifications.md): mail, ntfy, Telegram, Slack, Teams and the webhook
- [Routing](docs/routing.md): which problems go to which target
- [Message templates](docs/message-templates.md): your own wording for the messages
- [Configuration](docs/configuration.md): all environment variables
- [Login with OIDC](docs/oidc.md): log in through your identity provider, next to the password
- [Security](docs/security.md): permissions, the kubelet, login
- [Troubleshooting](docs/troubleshooting.md)
- [Local development](docs/development.md) and [ARCHITECTURE.md](ARCHITECTURE.md)

## License

Apache License 2.0, see [LICENSE](LICENSE).
