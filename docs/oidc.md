# Login with OIDC

smokealarm can log people in through an OpenID Connect (OIDC) identity provider such as Keycloak, Authentik or Google, so you don't have to share the admin password. It is a second way in, next to the password. The local admin stays, so you can still log in when the provider is down.

The login page then shows a button "Login with <name>" below the password form. The name is `SMOKEALARM_OIDC_NAME` (default `OIDC`), for example `MeineID`.

There are no roles. Everybody who is let in can do everything the admin can: acknowledge problems and set downtimes.

## Set it up

**1. Register a client at the identity provider.** It must be a confidential client (one with a client secret) that uses the authorization code flow. smokealarm also uses PKCE; a provider that has a setting for it can leave it on. The redirect URI is the address of the web UI plus `/login/oidc/callback`:

```
https://smokealarm.example.org/login/oidc/callback
```

It is built from `SMOKEALARM_BASE_URL` (Helm: `config.baseUrl`, or the ingress host), and the log line `OIDC login is on` and the notes after `helm install` print it. It has to match what you register exactly.

**2. Decide who may log in and make sure the ID token carries it.** See [Who may log in](#who-may-log-in). For emails the ID token needs the claims `email` and `email_verified`, which the scope `email` gives you. For groups, the provider has to put the groups into the ID token, under the claim `SMOKEALARM_OIDC_GROUPS_CLAIM` (default `groups`). Some providers need an extra scope or a mapper for that.

**3. Give smokealarm the settings.** With Helm (see [examples/oidc.yaml](../deploy/helm/examples/oidc.yaml)):

```yaml
oidc:
  issuerUrl: https://id.example.org/realms/home
  clientId: smokealarm
  name: MeineID
  allowedEmails: [gabriel@example.org]
  allowedGroups: [smokealarm-admins]
  scopes: [openid, email, groups]   # only if your provider needs "groups" for the groups claim
config:
  baseUrl: https://smokealarm.example.org
```

```sh
helm upgrade --install smokealarm ./deploy/helm/smokealarm -n smokealarm -f my-values.yaml \
  --set-string oidc.clientSecret="$CLIENT_SECRET" --set-string auth.passwordHash="$HASH"
```

The client secret goes into the chart's Secret, never into the Deployment. With your own Secret ([installation](installation.md#install-the-chart)), put it there as `SMOKEALARM_OIDC_CLIENT_SECRET` and leave `oidc.clientSecret` empty. The chart refuses incomplete settings when you install, for example a missing client ID or allowlist. All variables are in the [configuration table](configuration.md#oidc-login).

**4. Try it.** Open the login page, click the button, log in at the provider and you land on the overview. If something is wrong, the login page only says "Login with … failed." and the reason is in the log. See [Troubleshooting](troubleshooting.md#an-oidc-login-fails).

### Examples for two providers

These are starting points. Check them against the documentation of your provider and its version.

- **Keycloak:** the issuer URL is the address of the realm, `https://<host>/realms/<realm>`. Create a client with "Client authentication" on and the standard flow enabled, and enter the redirect URI. For groups, add a "Group Membership" mapper to the client with "Add to ID token" on and the claim name `groups`. Switch "Full group path" off if you want to write `ops` and not `/ops`.
- **Authentik:** create an OAuth2/OpenID provider and an application for it. The issuer URL is shown on the provider page and ends with the slug of the application, like `https://<host>/application/o/<slug>/`. Include the trailing slash exactly as shown, because it must match the issuer the provider reports.

## Who may log in

Access is by allowlist, and it fails closed: with OIDC on, `SMOKEALARM_OIDC_ALLOWED_EMAILS` or `SMOKEALARM_OIDC_ALLOWED_GROUPS` (or both) is required, and the start is refused without them. A provider usually knows many more people than should see your cluster, so there is no "everybody the provider lets in".

- **Emails:** the ID token's `email` must be on the list, compared without regard to case, and the provider must say it is verified (`email_verified`). An address that nobody has verified could be typed in by anybody who is allowed to edit their own profile at the provider, so it never counts.
- **Groups:** one of the groups in the claim `SMOKEALARM_OIDC_GROUPS_CLAIM` must be on the list. Groups are compared exactly as written, including case. The claim may be a list or a single text. Only the ID token is read, not the userinfo endpoint.
- One match, by email or by group, is enough.

The user's name in the session is the verified email, otherwise the claim `preferred_username`, otherwise the user ID (`sub`).

## Sessions and logging out

A login by OIDC creates the same kind of session as the password login and lasts `SMOKEALARM_SESSION_TTL` (7 days by default). "Log out" ends the smokealarm session only; the session at the provider stays, so the next click on the button logs you in again without asking for a password.

smokealarm checks the allowlist when somebody logs in, not on every request. So:

- Changing the issuer URL, the client ID or either allowlist ends all OIDC sessions when smokealarm restarts (a Helm upgrade restarts it). Password sessions are not touched. Changing the password ends the password sessions, not the OIDC ones.
- Removing somebody at the provider, or from a group there, does **not** end a session that is already open. It lasts until `SESSION_TTL`. If that is too long for you, set a shorter `SMOKEALARM_SESSION_TTL`, or take the person off the allowlist, which ends all OIDC sessions at the next restart.
- Changing `SMOKEALARM_OIDC_GROUPS_CLAIM` does not end sessions.

## When the provider is down

smokealarm starts and works without reaching the provider. It asks the provider for its configuration at the first login attempt and tries again at most every 30 seconds after a failure. While the provider can't be reached the button leads to a page that says "Login with … is not available right now" and the password form still works. The log has the reason (`OIDC login could not start`; see [Troubleshooting](troubleshooting.md#the-oidc-button-says-the-login-is-not-available)).

## A provider with an internal certificate

If the provider's certificate comes from your own CA, put the CA into a ConfigMap and name it. The key must be `ca.crt`:

```sh
kubectl -n smokealarm create configmap oidc-ca --from-file=ca.crt=/path/to/ca.crt
```

```yaml
oidc:
  caConfigMap: oidc-ca
```

This CA is used instead of the system's trusted CAs for the requests to the provider, so include the whole chain you need. Locally, use `SMOKEALARM_OIDC_CA_FILE`. If you need an HTTP proxy to reach the provider, `HTTPS_PROXY` is used.

## What it does not do

One provider only. No roles or rights per user. No single logout and no refresh tokens. No creating of users: nobody needs an account in smokealarm. If you need one of these, open an issue.

## Security notes

- The authorization code flow with PKCE, a `state` and a `nonce` is used. These are kept in a short-lived cookie (10 minutes, `HttpOnly`, `SameSite=Lax`, and `Secure` unless you turned that off) that is deleted when the browser comes back, so one login can't be used twice.
- The ID token is checked by the [go-oidc](https://github.com/coreos/go-oidc) library: signature, issuer, audience, expiry and nonce.
- The issuer URL must use `https`. Plain `http` is accepted only for `localhost`.
- The client secret, tokens, codes and the login cookie are never logged, never part of an error message and never shown on the status page.
- Requests to the provider don't follow redirects, so a redirect can't send the client secret to another host.
- The login failure page doesn't give the reason (for example that an address is not on the allowlist). The reason is in the log.
