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

## An OIDC login fails

The login page only says "Login with … failed." (the name is `SMOKEALARM_OIDC_NAME`). The reason is in the log, in the line `OIDC login failed` under `reason`. The common ones:

- `no login is under way: the cookie is missing or expired`, or `the state does not match`: the browser came back without the cookie that it got when the login started. It took longer than 10 minutes, the callback was opened in another browser, or the browser dropped the cookie because it is `Secure` and you use plain HTTP (see "Login works, but I'm logged out again right away" below).
- `exchange the code for tokens: … invalid_client`, or `invalid_grant`: the client ID or secret is wrong, or the redirect URI that is registered at the provider differs from the one smokealarm sends. The log line `OIDC login is on` at startup shows that one. It has to match exactly, including `https` and a trailing slash.
- `check the ID token: oidc: expected audience …`: `SMOKEALARM_OIDC_CLIENT_ID` is not the client the token was issued for.
- `oidc: id token issued by a different provider`, or at the first login `issuer URL provided to client … did not match the issuer URL returned by provider`: `SMOKEALARM_OIDC_ISSUER_URL` must be the same text as the `issuer` in `<issuer URL>/.well-known/openid-configuration`, down to a trailing slash.
- `oidc: token is expired` or `before the nbf`: the clocks of the node and the provider differ by more than a few seconds. Fix the time on the node.
- `… is not allowed: no allowed email or group`: the person is not on the allowlist, or the ID token has no `groups` claim (or one with another name, `SMOKEALARM_OIDC_GROUPS_CLAIM`). The groups have to be in the ID token itself. Some providers need an extra scope in `SMOKEALARM_OIDC_SCOPES` or a mapper for that.
- `the email is on the allowlist but the provider does not say it is verified`: the provider does not send `email_verified: true`. Verify the address at the provider.

## The OIDC button says the login is not available

smokealarm could not reach the provider to read its configuration. The log line `OIDC login could not start` has the reason: a name that does not resolve, a connection that is refused or blocked by a NetworkPolicy, `x509: certificate signed by unknown authority` (the provider's certificate comes from your own CA, see [A provider with an internal certificate](oidc.md#a-provider-with-an-internal-certificate)), or an HTTP error. smokealarm does not follow redirects when it talks to the provider, so a `301` or `302` there means the issuer URL is not written the way the provider wants it (often a missing or extra trailing slash). smokealarm tries again 30 seconds after a failure. Meanwhile the password login works.

## The history is gone after a restart

The database wasn't on a persistent volume (`persistence.enabled=false`, or `SMOKEALARM_DB_PATH` outside the volume).
