# Security

## Permissions in the cluster

smokealarm only needs `get`, `list` and `watch` on a short list of resources. The `ClusterRole` from the Helm chart grants exactly that, plus `get` on `nodes/stats` for the volume usage. It never reads Secrets.

The optional permissions each have a switch: `rbac.nodesStats` for `pvc-usage`, `rbac.certManager` for cert-manager Certificates and `rbac.argocd` for Argo CD Applications. Turning a switch off also disables the checks that need it. The ClusterRole is named `<namespace>-<release>`, so the chart can be installed in several namespaces.

`nodes/stats` grants resource statistics only: no logs, no command execution, no list of pods. Earlier versions read the stats through the API server, which needs `get` on `nodes/proxy`, and the Kubernetes documentation ([RBAC good practices](https://kubernetes.io/docs/concepts/security/rbac-good-practices/#access-to-proxy-subresource-of-nodes)) warns that this reaches the whole kubelet API. smokealarm now asks the kubelets itself and never needs `nodes/proxy`. The rule only exists while `pvc-usage` is active. If you don't need the check, set `rbac.nodesStats=false` or add `pvc-usage` to `disabledChecks`, and the permission goes away. Values files that still set the old `rbac.nodesProxy` are refused with a hint.

## Volume usage and the kubelet

`pvc-usage` calls `https://<node InternalIP>:<kubelet port>/stats/summary` on every Ready node (the port the node publishes, usually 10250) with the service account token. The kubelet checks the token and the permission itself. This needs three things from the cluster. If one is missing, the check is skipped and the status page says which.

The kubelet port must be reachable from the smokealarm pod. A NetworkPolicy or a host firewall can block it. Through the API server this was never a question. Status page: "could not reach the kubelet of node …".

The kubelet's certificate must be verifiable. smokealarm always verifies it, against the CA of the service account (`ca.crt`) unless `SMOKEALARM_KUBELET_CA_FILE` is set. On k3s the kubelet serving certificates are signed by the cluster's server CA. On kubeadm clusters they are self-signed unless `serverTLSBootstrap: true` is set in the kubelet configuration (and the certificate requests are approved). With self-signed certificates `pvc-usage` can't run. smokealarm deliberately doesn't skip the verification, because the token would go to anybody who answers at the node IP. If your kubelet certificates come from another CA, put it into a ConfigMap and set `kubelet.caConfigMap` (see [values.yaml](../deploy/helm/smokealarm/values.yaml)). Status page: "could not verify the TLS certificate of the kubelet on node …". The certificate must also contain the node's InternalIP; otherwise the status page says it "does not name the address smokealarm calls".

The kubelet must use webhook authentication and authorization (`authentication-token-webhook` and `authorization-mode=Webhook`, the default on k3s and kubeadm). Otherwise it answers 401 or 403, the same as when `nodes/stats` is missing. Status page: "the kubelet of node … refused the request", followed by the kubelet's own answer, which names the user and subresource it checked.

The kubelets are always called directly; `HTTPS_PROXY` only applies to the notification targets. A kubelet that takes longer than 10 seconds is reported as "did not answer in time". If you run `helm upgrade --reuse-values` from a release before this change, the chart stops with a hint about `rbac.nodesProxy`; use `--reset-then-reuse-values` instead.

## The container

The container runs as non-root with a read-only root filesystem, all capabilities dropped, no privilege escalation and the default seccomp profile. It can only write to the data volume.

## Login and the web UI

There is one local admin, with the password stored as a bcrypt hash. Sessions are kept in the database, which only stores a hash of the session id. The cookie is `HttpOnly` and `SameSite=Lax`, and `Secure` unless you turn that off. Failed logins are limited to 5 per 15 minutes per client address. A session ends when the password or the admin user changes.

Behind an Ingress or another proxy, all clients have the same address and share one limit. Five wrong logins by anybody then block the login for everybody for up to 15 minutes, and somebody who keeps doing that can keep you locked out. Existing sessions keep working. So don't expose the UI to the internet without restricting access first, for example with an allowlist or authentication at the Ingress, or a VPN.

Actions that change state, such as acknowledging a problem or setting a downtime, need a valid session. They refuse requests that come from another site or another host of the same domain (Go's `http.CrossOriginProtection`). Over plain HTTP your proxy has to pass on the original `Host` header, otherwise these actions fail with 403. Over HTTPS browsers send a `Sec-Fetch-Site` header and the `Host` doesn't matter.

Passwords, hashes, session ids, tokens and webhook URLs are never logged.

`/healthz` and `/readyz` need no login, so Kubernetes can probe them.
