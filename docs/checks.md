# The checks

The checks need no configuration. Severity is either critical (act now) or warning (have a look soon). The status page shows which checks are active and why any are skipped.

| Check | Severity | Reports |
|---|---|---|
| `pod-crashloop` | critical | A container is in `CrashLoopBackOff`. The kubelet only shows this state after several crashes, so it can take a few minutes. |
| `pod-image-pull` | critical | A container cannot pull its image (`ImagePullBackOff` / `ErrImagePull`). |
| `pod-oomkilled` | warning | A container was killed for exceeding its memory limit in the last 15 minutes. |
| `pod-restarts` | warning | More restarts than `SMOKEALARM_RESTART_THRESHOLD`, with the last restart within the past hour. |
| `pod-pending` | warning | A pod has been pending longer than `SMOKEALARM_POD_PENDING_AFTER`; the message says why (for example `Unschedulable`). |
| `workload-replicas` | warning / critical | A Deployment, StatefulSet or DaemonSet has fewer available replicas than desired (warning), or none at all (critical). |
| `node-not-ready` | critical | A node is `NotReady` or stopped reporting. |
| `node-pressure` | warning | A node reports `MemoryPressure`, `DiskPressure` or `PIDPressure`. |
| `job-failed` | warning | A Job failed. For CronJobs only the newest Job counts, so old failures don't alarm forever. |
| `cronjob-missed` | warning | A scheduled CronJob run did not happen. Suspended CronJobs are ignored. |
| `pvc-pending` | warning | A claim is pending longer than `SMOKEALARM_PVC_PENDING_AFTER`. Claims of a `WaitForFirstConsumer` storage class are skipped, because they stay pending until a pod uses them. |
| `pvc-usage` | warning / critical | A volume is more than 85 % (warning) or 95 % (critical) full. Asks the kubelets directly and needs `get` on `nodes/stats`; see [Volume usage and the kubelet](security.md#volume-usage-and-the-kubelet). |
| `cert-not-ready` | warning | A cert-manager Certificate is not `Ready`. Only if cert-manager is installed. |
| `cert-expiry` | warning / critical | A certificate expires in less than 14 days (warning) or 3 days (critical), or has expired. Only if cert-manager is installed. |
| `argocd-health` | critical | An Argo CD Application is `Degraded` or `Missing`. Only if Argo CD is installed. |
| `argocd-sync` | warning | An Argo CD Application is `OutOfSync`. Only if Argo CD is installed. |
| `api-connection` | critical | smokealarm has been unable to keep its copy of the cluster up to date for 3 minutes: the API server doesn't answer, or a watch fails (a missing permission, a deleted CRD). See [When the API cannot be read](how-it-works.md#when-the-api-cannot-be-read). |

## Notes

**cert-manager and Argo CD.** Their CRDs are detected once at startup, and only if an enabled check needs them. If a CRD is missing or the service account may not list it, the matching checks are skipped and the status page says why. Restart smokealarm after installing the CRD or granting the permission. While a permission is missing, open problems from those checks stay open, since they may still exist. If the CRD is not installed, they count as gone.

**Switching a check off closes its problems.** This applies to `SMOKEALARM_DISABLED_CHECKS` and to the chart's `rbac.*` switches, which disable the checks that need the permission. The open problems are reported as resolved after `SMOKEALARM_RESOLVE_AFTER`, even if the cause is still there. A check that is only skipped for a moment, for example because the kubelet doesn't answer, leaves its problems alone.

**Warning to critical.** `pvc-usage`, `cert-expiry` and `workload-replicas` can go from warning to critical. It stays the same problem and isn't reported as resolved. You get a new mail saying it is now critical once that has lasted `SMOKEALARM_ALERT_AFTER`.

**CronJob time zones.** A CronJob without `spec.timeZone` runs in the local time of the Kubernetes controller. On k3s the controller runs inside the host process, so that should be the host's time zone (check with `timedatectl`). smokealarm can't see it from the API and assumes UTC. If the host is not on UTC, set `SMOKEALARM_CRONJOB_TIME_ZONE` to the same zone, or daily and weekly CronJobs are reported as missed most of the day. Short schedules like `*/5 * * * *` are not affected. A schedule that never comes true, like `0 0 31 2 *` for a CronJob that only runs by hand, is never reported as missed.

**What `pvc-usage` measures.** It looks at the filesystem a volume lives on, because that is what the kubelet reports. With k3s `local-path` that is the node's disk, and the requested size isn't enforced, so the check only fires when the node disk is nearly full. It is more useful for volumes with their own filesystem (Longhorn, Ceph, block volumes).

If `pvc-usage` can't read the volume stats (no permission, a node doesn't answer, a certificate can't be verified), it is skipped for that run, open problems stay open, and the status page shows the reason. A node that is down can't be asked, so a `pvc-usage` problem whose volume isn't measured stays open, because the volume may live on that node. It also stays open if its pod is deleted in the meantime, until all nodes are back.

## Ignoring things

There are three ways to keep smokealarm quiet about something:

- Put the annotation `smokealarm.gagaschu.github.io/ignore: "true"` on an object or its namespace to silence all its findings. For pods, setting it in the pod template of the Deployment, StatefulSet, Job and so on is enough.
- List namespaces in `SMOKEALARM_IGNORE_NAMESPACES`.
- Switch off a whole check with `SMOKEALARM_DISABLED_CHECKS`.

To change where the problems of an app go, or how severe they are, without silencing them, see [Routing](routing.md).
