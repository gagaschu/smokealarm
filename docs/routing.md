# Routing

By default every [notification target](notifications.md) hears about every problem. Two settings change that, and you can combine them.

## Critical problems only, per target

`SMOKEALARM_<TARGET>_MIN_SEVERITY=critical` sends only critical problems to that target. `<TARGET>` is `SMTP`, `WEBHOOK`, `NTFY`, `TELEGRAM`, `SLACK` or `TEAMS`; for Telegram it applies to all chats. In the chart it is `minSeverity` under `smtp` and under each target in `notifications`.

For example, to get only critical problems on the phone through ntfy and everything in Slack:

```
SMOKEALARM_NTFY_MIN_SEVERITY=critical
```

## Per app or namespace, with annotations

Put these annotations on an object or on its namespace. The one on the object wins, and each annotation is looked up on its own. For pods, setting them in the pod template is enough.

| Annotation | Value | Effect |
|---|---|---|
| `smokealarm.gagaschu.github.io/targets` | Comma-separated: `mail`, `webhook`, `ntfy`, `telegram`, `slack`, `teams` | Its problems go only to these targets. `telegram` means all chats. |
| `smokealarm.gagaschu.github.io/severity` | `warning` or `critical` | Replaces the severity of all its problems. |

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: media
  annotations:
    smokealarm.gagaschu.github.io/targets: "slack"      # the media team's channel only
    smokealarm.gagaschu.github.io/severity: "warning"   # never wake anybody up for it
```

A problem reads the annotations of its own object: a pod problem those of the pod, a `workload-replicas` problem those of the Deployment (its `metadata`, not the pod template). For a CronJob, those of its job template count too. Annotating the namespace is the easy way to cover all of them.

## How the two work together

A target hears about a problem if the problem's severity is high enough for that target **and** the targets annotation, if there is one, names it.

A typo doesn't silence an alarm. If the targets annotation names no target that is configured (`slak`), the problem goes to every target. Names that aren't configured are logged as a warning, once, together with the object or namespace that carries the annotation. The same goes for a severity that is neither `warning` nor `critical`; it is ignored and the check's own severity counts.

A warning can reach nobody, and that is allowed. With `targets: ntfy` and `SMOKEALARM_NTFY_MIN_SEVERITY=critical`, the warnings of that object go nowhere. smokealarm logs this once as a warning, and the overview says "Notifies nobody".

The filter applies to new problems and reminders. The end of a problem always goes to whoever heard its start: a target that was told about a problem is also told when it is resolved, even if the problem has dropped to a warning or the annotation has changed in the meantime. The same holds when a problem gets worse or a downtime ends. Only the targets it is meant for now hear it again as new; the others still hear the end.

Changing an annotation doesn't create a new problem. The change counts from the next run: a target that now gets the problem and hasn't heard of it yet hears about it as new, and one that no longer gets it gets no more reminders.

When a warning becomes critical (`pvc-usage`, `cert-expiry`, `workload-replicas`, or a `severity: critical` set later), it is reported as new to the critical-only targets.

The severity annotation goes through all the usual rules. `critical` brings reminders and the acknowledge button. `warning` keeps a `pvc-usage` or `cert-expiry` problem a warning, however bad it gets.

Acknowledgements and downtimes work as before. The overview and the history say who is notified when it isn't every target ("Notifies only: slack" or "Notifies nobody"), and the status page shows which targets get critical problems only.

To silence an object completely instead, see [Ignoring things](checks.md#ignoring-things).
