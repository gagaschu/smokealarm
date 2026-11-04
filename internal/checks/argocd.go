package checks

import "fmt"

const (
	checkArgoCDHealth = "argocd-health"
	checkArgoCDSync   = "argocd-sync"
)

// ArgoCDHealth reports Argo CD Applications whose health is Degraded or
// Missing: what Git describes is not running properly, or not running at all.
// Severity is critical. Other states (Healthy, Progressing, Suspended, ...)
// are not problems; a new application is briefly Missing, which ALERT_AFTER
// absorbs.
func ArgoCDHealth(snap *Snapshot, opts Options) []Finding {
	var findings []Finding
	for _, app := range snap.Applications {
		if app.HealthStatus != "Degraded" && app.HealthStatus != "Missing" {
			continue
		}
		if isIgnored(snap, opts, app.Meta) {
			continue
		}
		msg := fmt.Sprintf("application health is %s", app.HealthStatus)
		findings = append(findings, newFinding(checkArgoCDHealth, Critical, appRef(app), app.HealthStatus, msg))
	}
	return findings
}

// ArgoCDSync reports Applications that are OutOfSync: the cluster differs
// from Git. Severity is warning. Short differences while a sync is running
// are absorbed by ALERT_AFTER.
func ArgoCDSync(snap *Snapshot, opts Options) []Finding {
	var findings []Finding
	for _, app := range snap.Applications {
		if app.SyncStatus != "OutOfSync" || isIgnored(snap, opts, app.Meta) {
			continue
		}
		findings = append(findings, newFinding(checkArgoCDSync, Warning, appRef(app), "OutOfSync",
			"application is out of sync with its Git source"))
	}
	return findings
}

func appRef(app Application) ObjectRef {
	return ObjectRef{Kind: KindApplication, Namespace: app.Meta.Namespace, Name: app.Meta.Name}
}
