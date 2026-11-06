# Troubleshooting

## The pod restarts about two minutes after it starts

The log says `informer caches did not sync within 2m0s: <resources>` and, before that, `forbidden`. smokealarm can't read the listed resources. The Helm chart grants them, so this happens with `rbac.create=false` and your own RBAC. Compare yours with [clusterrole.yaml](../deploy/helm/smokealarm/templates/clusterrole.yaml). Missing cert-manager or Argo CD permissions don't cause this; those checks are skipped, and the status page shows why.

## Something is broken but nothing is reported

Problems are only reported after `SMOKEALARM_ALERT_AFTER` (2 minutes by default). Look at the overview page, and in the log for `finding pending` and `finding firing`. Without a [notification target](notifications.md), notifications only go to the log.

## A target shows "sending failed" on the status page

The error is next to it, with the HTTP status and the start of the answer (never the token or URL). That target is retried in every run; the others are not affected. A `401` or `403` usually means a wrong token. A `404` means a wrong URL or Telegram chat id ("chat not found": send the bot a message first). A `3xx` means the URL redirects, which smokealarm doesn't follow.

## A check shows as skipped on the status page

The reason is next to it: disabled in the configuration, CRD not installed, or a missing permission. For `pvc-usage`, see [Volume usage and the kubelet](security.md#volume-usage-and-the-kubelet).

## Login works, but I'm logged out again right away

The cookie is `Secure` and you're using plain HTTP. Use HTTPS, or set `SMOKEALARM_COOKIE_SECURE=false` for local use.

## The history is gone after a restart

The database wasn't on a persistent volume (`persistence.enabled=false`, or `SMOKEALARM_DB_PATH` outside the volume).
