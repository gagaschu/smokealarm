# Configuration

smokealarm is configured through environment variables that start with `SMOKEALARM_`. The Helm chart sets them from its [values.yaml](../deploy/helm/smokealarm/values.yaml). Invalid values stop the program at startup with an error message. Durations use Go syntax (`30s`, `5m`, `2h`) plus `d` for days (`30d`).

## General

| Variable | Default | Description |
|---|---|---|
| `SMOKEALARM_ADMIN_USER` | – (required) | Name of the admin user. |
| `SMOKEALARM_ADMIN_PASSWORD_HASH` | – (required) | bcrypt hash of the admin password. Create it with `smokealarm hash-password`. |
| `SMOKEALARM_CHECK_INTERVAL` | `30s` | How often the checks run. |
| `SMOKEALARM_ALERT_AFTER` | `2m` | How long a problem must last before it is reported. `0s` reports at once. |
| `SMOKEALARM_RESOLVE_AFTER` | `5m` | How long a reported problem must be gone before it counts as resolved and that is reported. `0s` resolves it at once. |
| `SMOKEALARM_CRITICAL_REPEAT_INTERVAL` | `0s` | How often a critical problem that is still open is reported again, for example `4h`. `0s` means never. Otherwise it must be at least `SMOKEALARM_CHECK_INTERVAL`. Critical problems that have been open for longer are reported in the next run after you turn this on. |
| `SMOKEALARM_RETENTION` | `30d` | How long resolved problems are kept in the history. |
| `SMOKEALARM_DB_PATH` | `smokealarm.db` | The SQLite database file. The container image uses `/data/smokealarm.db`. |
| `SMOKEALARM_LISTEN_ADDR` | `:8080` | Address of the web server. |
| `SMOKEALARM_BASE_URL` | – | Public URL of the web UI, used for the link in messages. |
| `SMOKEALARM_LOG_FORMAT` | `auto` | `json`, `text` or `auto` (JSON inside a cluster, text otherwise). |
| `SMOKEALARM_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `SMOKEALARM_SESSION_TTL` | `7d` | How long a login stays valid. |
| `SMOKEALARM_COOKIE_SECURE` | `true` | Send the login cookie only over HTTPS. Set `false` only for local development over plain HTTP. |

## OIDC login

An optional second way to log in next to the admin password. It is on when `SMOKEALARM_OIDC_ISSUER_URL` is set, and then needs the client ID and secret, an allowlist and `SMOKEALARM_BASE_URL`. Any other `SMOKEALARM_OIDC_*` variable without the issuer URL stops the start, so a typo cannot switch it off unnoticed.

| Variable | Default | Description |
|---|---|---|
| `SMOKEALARM_OIDC_ISSUER_URL` | – | Issuer URL of the identity provider. Switches the OIDC login on. Must be `https` (plain `http` only for localhost). |
| `SMOKEALARM_OIDC_CLIENT_ID` | – (required with OIDC) | Client ID of the client you created at the provider. |
| `SMOKEALARM_OIDC_CLIENT_SECRET` | – (required with OIDC) | Client secret. A secret: the Helm chart keeps it in the Secret only. |
| `SMOKEALARM_OIDC_NAME` | `OIDC` | Name shown in the login button, for example `MeineID` for "Login with MeineID". At most 40 characters, no control characters. |
| `SMOKEALARM_OIDC_SCOPES` | `openid email` | Scopes to ask for, separated by spaces or commas. Must contain `openid`. Some providers need an extra scope to put the groups into the ID token. |
| `SMOKEALARM_OIDC_GROUPS_CLAIM` | `groups` | The claim of the ID token that holds the groups of the user. |
| `SMOKEALARM_OIDC_ALLOWED_EMAILS` | – | Comma-separated email addresses that may log in. An address counts only if the provider says it is verified. Compared without regard to case. |
| `SMOKEALARM_OIDC_ALLOWED_GROUPS` | – | Comma-separated groups that may log in. Compared exactly as written. At least one of the two lists is required, and one match is enough. |
| `SMOKEALARM_OIDC_CA_FILE` | – | A CA file for a provider whose certificate comes from an internal CA. Empty means the system's trusted CAs. |

## Checks

| Variable | Default | Description |
|---|---|---|
| `SMOKEALARM_DISABLED_CHECKS` | – | Comma-separated check names to switch off, for example `pod-restarts,pvc-usage`. An unknown name is an error. Open problems of a switched-off check are closed and reported as resolved, see [The checks](checks.md#notes). |
| `SMOKEALARM_IGNORE_NAMESPACES` | – | Comma-separated namespaces that never produce findings. |
| `SMOKEALARM_RESTART_THRESHOLD` | `5` | `pod-restarts` reports containers with more restarts than this. |
| `SMOKEALARM_POD_PENDING_AFTER` | `5m` | `pod-pending` reports pods that are pending longer than this. |
| `SMOKEALARM_PVC_PENDING_AFTER` | `5m` | `pvc-pending` reports claims that are pending longer than this. |
| `SMOKEALARM_CRONJOB_GRACE` | `5m` | How late a CronJob run may be before `cronjob-missed` reports it. |
| `SMOKEALARM_CRONJOB_TIME_ZONE` | (UTC) | The time zone the Kubernetes controller reads schedules in, for CronJobs without `spec.timeZone`, for example `Europe/Berlin`. See [CronJob time zones](checks.md#notes). |
| `SMOKEALARM_TIME_ZONE` | `UTC` | The time zone for all times the web UI, the mails and the chat messages show, and for the time in the downtime form, for example `Europe/Zurich`. Stored times and the time fields of the webhook stay in UTC. `Local` is refused. See [time zone](how-it-works.md#time-zone). |
| `SMOKEALARM_KUBELET_CA_FILE` | (the CA of the API client) | A PEM file with the CA that signed the kubelet serving certificates, for `pvc-usage`. Only needed if that is not the CA of the service account (`ca.crt`). The chart sets it from `kubelet.caConfigMap`. See [Volume usage and the kubelet](security.md#volume-usage-and-the-kubelet). |

## Mail

| Variable | Default | Description |
|---|---|---|
| `SMOKEALARM_SMTP_HOST` | – | Mail server. Setting it switches mail on. |
| `SMOKEALARM_SMTP_PORT` | `587` | Mail server port. |
| `SMOKEALARM_SMTP_TLS` | `starttls` | `starttls`, `tls` (encrypted from the start, usually port 465) or `none` (local testing only; a login is then only possible to `localhost`). With `starttls`, a server that doesn't offer it is an error; there is no silent downgrade. |
| `SMOKEALARM_SMTP_USERNAME` | – | Login for the mail server (optional). |
| `SMOKEALARM_SMTP_PASSWORD` | – | Password for the login. Required if a username is set. |
| `SMOKEALARM_SMTP_FROM` | – (required if mail is on) | Sender address. |
| `SMOKEALARM_SMTP_TO` | – (required if mail is on) | Comma-separated recipient addresses. |

## Other notification targets

| Variable | Default | Description |
|---|---|---|
| `SMOKEALARM_WEBHOOK_URL` | – | Secret. Webhook URL; setting it switches the [webhook](notifications.md#webhook) on. |
| `SMOKEALARM_WEBHOOK_HEADER` | – | Secret. One extra header for the webhook, `Name: value`, e.g. `Authorization: Bearer <token>`. |
| `SMOKEALARM_NTFY_TOPIC` | – | Secret. The [ntfy](notifications.md#ntfy) topic; setting it switches ntfy on. Letters, digits, `-` and `_`. |
| `SMOKEALARM_NTFY_URL` | `https://ntfy.sh` | The ntfy server, for a self-hosted one. |
| `SMOKEALARM_NTFY_TOKEN` | – | Secret. Access token for the ntfy server (optional). |
| `SMOKEALARM_TELEGRAM_BOT_TOKEN` | – | Secret. The bot token from @BotFather; setting it switches [Telegram](notifications.md#telegram) on. |
| `SMOKEALARM_TELEGRAM_CHAT_IDS` | – (required if Telegram is on) | Comma-separated chat ids (numbers such as `-1001234567890`) or `@channelname`s. |
| `SMOKEALARM_SLACK_WEBHOOK_URL` | – | Secret. A Slack incoming webhook URL; setting it switches [Slack](notifications.md#slack) on. |
| `SMOKEALARM_TEAMS_WEBHOOK_URL` | – | Secret. A Teams Workflows webhook URL; setting it switches [Teams](notifications.md#microsoft-teams) on. Old `webhook.office.com` connector URLs are refused. |
| `SMOKEALARM_NOTIFY_ALLOW_HTTP` | `false` | Allow plain `http://` for the webhook and the ntfy server, e.g. a relay in the cluster. `localhost` is always allowed. |
| `SMOKEALARM_TEMPLATE_DIR` | – | A directory with your own [message templates](message-templates.md) for the subject and text of mail, ntfy, Telegram, Slack and Teams. Empty means the built-in messages. The chart sets it from `templates`. |

## Routing

See [Routing](routing.md). Each of these is refused if its target is not configured.

| Variable | Default | Description |
|---|---|---|
| `SMOKEALARM_SMTP_MIN_SEVERITY` | `warning` | `critical` sends only critical problems by mail. |
| `SMOKEALARM_WEBHOOK_MIN_SEVERITY` | `warning` | The same for the webhook. |
| `SMOKEALARM_NTFY_MIN_SEVERITY` | `warning` | The same for ntfy. |
| `SMOKEALARM_TELEGRAM_MIN_SEVERITY` | `warning` | The same for Telegram, all chats. |
| `SMOKEALARM_SLACK_MIN_SEVERITY` | `warning` | The same for Slack. |
| `SMOKEALARM_TEAMS_MIN_SEVERITY` | `warning` | The same for Teams. |

## Outside the cluster

For local runs smokealarm also reads the standard `KUBECONFIG` variable. In a pod it uses the service account.
