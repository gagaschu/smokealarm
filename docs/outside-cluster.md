# Outside the cluster

smokealarm usually runs as a pod in the cluster it watches. It can also run somewhere else and watch the cluster through a kubeconfig:

- on a host with Docker or systemd, for example a small server next to the cluster;
- in another cluster, for example a management cluster that watches the others.

The main reason is a cluster that fails as a whole. If the control plane or all nodes are down, an instance inside the cluster goes down with them, and nobody gets a message. An instance outside keeps running, and `api-connection` reports that the API server doesn't answer (see [When the API cannot be read](how-it-works.md#when-the-api-cannot-be-read)).

One instance watches one cluster. For several clusters, run one instance per cluster, each with its own kubeconfig, database and port.

## 1. The access in the watched cluster

The chart can install only the access, without smokealarm itself: a ServiceAccount, the read-only ClusterRole and a Secret with a token for it.

```sh
helm install smokealarm-access oci://ghcr.io/gagaschu/charts/smokealarm \
  --namespace smokealarm --create-namespace \
  --set externalAccess.enabled=true
```

No password hash is needed for this. The `rbac.*` switches work as usual (see [Security](security.md#permissions-in-the-cluster)). If you turn one off, the notes after the install list the checks you must switch off in the instance outside with `SMOKEALARM_DISABLED_CHECKS`. Otherwise they fail for lack of permission, and `api-connection` reports it.

The token doesn't expire. It stays valid until the Secret `smokealarm-access-token` is deleted. To revoke it, delete the Secret and run `helm upgrade` to get a new token.

## 2. The kubeconfig

The notes after the install print the commands. In an empty directory:

```sh
kubectl -n smokealarm get secret smokealarm-access-token -o jsonpath='{.data.token}' | base64 -d > token
kubectl -n smokealarm get secret smokealarm-access-token -o jsonpath='{.data.ca\.crt}' | base64 -d > ca.crt
chmod 600 token
kubectl config --kubeconfig=kubeconfig set-cluster watched \
  --server="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')" \
  --certificate-authority=ca.crt --embed-certs
kubectl config --kubeconfig=kubeconfig set users.smokealarm.tokenFile /etc/smokealarm/kube/token
kubectl config --kubeconfig=kubeconfig set-context watched --cluster=watched --user=smokealarm
kubectl config --kubeconfig=kubeconfig use-context watched
```

The kubeconfig refers to the token by `tokenFile` and doesn't contain it. smokealarm reads the file again every minute, so you can replace the token without a restart. The path `/etc/smokealarm/kube/token` is where the examples below put it. Change it if yours is elsewhere.

The `--server` address must be reachable from where smokealarm runs. `kubectl config view --minify` takes it from your current context. If that is a local address such as `127.0.0.1`, put in the address of the API server or its load balancer instead.

Treat both files like a password: they give read access to the whole cluster, though not to Secrets.

## 3. Run smokealarm

smokealarm uses the kubeconfig when `KUBECONFIG` is set or `SMOKEALARM_KUBE_CONTEXT` names a context. That holds even inside a pod: the kubeconfig wins over the service account. Without either, it uses the service account in a pod and `~/.kube/config` everywhere else. At startup it logs which one it uses:

```
level=INFO msg="connected to the Kubernetes API" source="kubeconfig context \"watched\"" server=https://k8s.example.org:6443
```

`SMOKEALARM_KUBE_CONTEXT` picks a context from a kubeconfig with several clusters. Without it, smokealarm takes the current context. An unknown context stops the start with an error.

All other settings are the usual [variables](configuration.md).

### With Docker Compose

[`deploy/compose/docker-compose.yaml`](../deploy/compose/docker-compose.yaml) runs the image with the database on a volume:

```sh
mkdir kube && cp kubeconfig token kube/
sudo chown -R 65532 kube && chmod 0700 kube && chmod 0600 kube/*   # the image runs as user 65532
cat > smokealarm.env <<'EOF'
SMOKEALARM_ADMIN_USER=admin
SMOKEALARM_ADMIN_PASSWORD_HASH='<the hash, in single quotes>'
SMOKEALARM_COOKIE_SECURE=false
EOF
docker compose up -d
```

`SMOKEALARM_COOKIE_SECURE=false` is only for a first test over plain HTTP. For permanent use, put a reverse proxy with HTTPS in front and leave it on (see [Security](security.md#login-and-the-web-ui)).

### With systemd

[`deploy/systemd/smokealarm.service`](../deploy/systemd/smokealarm.service) runs the binary (`make build`, or copy it out of the image) as its own user, with the database in `/var/lib/smokealarm`. The comment at the top of the file has the steps. The kubeconfig and the token go to `/etc/smokealarm/kube/`, the variables to `/etc/smokealarm/smokealarm.env`.

### In another cluster

To watch cluster B from cluster A, build the kubeconfig in B as above. Then put it into a Secret in A together with the token, and install smokealarm in A with `kubeconfig.existingSecret`:

```sh
kubectl -n smokealarm create secret generic cluster-b \
  --from-file=config=./kubeconfig --from-file=token=./token
helm install smokealarm-b oci://ghcr.io/gagaschu/charts/smokealarm -n smokealarm \
  --set kubeconfig.existingSecret=cluster-b \
  --set-string auth.passwordHash="$HASH"
```

The Secret is mounted read-only at `/etc/smokealarm/kube`, which matches the `tokenFile` from step 2. `KUBECONFIG` points to the key `config`, or to `kubeconfig.key` if you set it. `kubeconfig.context` sets `SMOKEALARM_KUBE_CONTEXT`. No ClusterRole is created in cluster A, and the service account token of A is not mounted: smokealarm reads nothing there. See [`deploy/helm/examples/watch-other-cluster.yaml`](../deploy/helm/examples/watch-other-cluster.yaml).

## What is different outside the cluster

- **`api-connection` also measures the network.** If the connection between smokealarm and the cluster fails, you get the same critical alarm as for an API server that is down. This is the point of running outside, but it also means a network problem on your side raises an alarm.
- **`pvc-usage` calls every kubelet directly**, at `https://<node InternalIP>:<kubelet port>`. From outside the cluster, these addresses are often unreachable. The kubelet certificates must also be signed by the CA in the kubeconfig (true on k3s) or by the one in `SMOKEALARM_KUBELET_CA_FILE`. If either fails, the check is skipped and the status page says why (see [Volume usage and the kubelet](security.md#volume-usage-and-the-kubelet)). If you don't need it, switch it off with `SMOKEALARM_DISABLED_CHECKS=pvc-usage` and install the access with `--set rbac.nodesStats=false`.
- **Logs**: `SMOKEALARM_LOG_FORMAT=auto` means JSON only in a pod. Set it to `json` if a log collector reads the output. The Compose example does that.
- **Messages don't name the cluster yet.** With several instances, tell them apart by `SMOKEALARM_BASE_URL`: the link in every message leads to the instance that sent it.
