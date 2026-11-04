package checks

import (
	"fmt"
	"slices"
	"strings"
)

// TargetsAnnotation on an object or its namespace limits the notification
// targets that hear about its problems, e.g. "slack,mail". The names are the
// kinds of targets (mail, webhook, ntfy, telegram, slack, teams); "telegram"
// means every configured chat.
const TargetsAnnotation = AnnotationPrefix + "/targets"

// SeverityAnnotation on an object or its namespace replaces the severity of
// all its problems with "warning" or "critical". It can raise and lower: a
// test namespace can be made quieter, an important app louder.
const SeverityAnnotation = AnnotationPrefix + "/severity"

// ApplyAnnotations sets the severity and the targets of the findings from the
// annotations of their objects. The annotation on the object wins over the one
// on its namespace; each annotation is looked up on its own. For pods the
// annotations of the pod count, which it gets from the pod template.
//
// It runs after the checks, so the checks themselves stay unaware of it. It
// returns a sentence for every annotation value it could not use; the problem
// keeps the severity of the check then, so a typo never hides an alarm.
func ApplyAnnotations(snap *Snapshot, findings []Finding) ([]Finding, []string) {
	objects := objectAnnotations(snap)
	namespaces := map[string]map[string]string{}
	for _, ns := range snap.Namespaces {
		namespaces[ns.Name] = ns.Annotations
	}

	var problems []string
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		own := objects[objectKey(f.Object)]
		var inherited map[string]string
		if f.Object.Namespace != "" {
			inherited = namespaces[f.Object.Namespace]
		}

		if value, found := lookup(own, inherited, SeverityAnnotation); found {
			severity, ok := parseSeverity(value)
			if ok {
				f.Severity = severity
			} else {
				problems = append(problems, fmt.Sprintf("%s: %s %q is neither %q nor %q",
					f.Object.Label(), SeverityAnnotation, value, Warning, Critical))
			}
		}
		// A list without names (" , ") counts as not set, like an empty value.
		if targets := ParseTargets(own[TargetsAnnotation]); len(targets) > 0 {
			f.Targets = targets
		} else {
			f.Targets = ParseTargets(inherited[TargetsAnnotation])
		}
		out = append(out, f)
	}
	return out, problems
}

// lookup returns the value of the annotation on the object, or else on its
// namespace. An empty value counts as not set.
func lookup(own, inherited map[string]string, name string) (string, bool) {
	if value := strings.TrimSpace(own[name]); value != "" {
		return value, true
	}
	if value := strings.TrimSpace(inherited[name]); value != "" {
		return value, true
	}
	return "", false
}

func parseSeverity(value string) (Severity, bool) {
	switch Severity(strings.ToLower(value)) {
	case Warning:
		return Warning, true
	case Critical:
		return Critical, true
	}
	return "", false
}

// ParseTargets splits a comma-separated list of target kinds into lower-case
// names, sorted and without duplicates or empty entries. The store keeps the
// list in this form.
func ParseTargets(value string) []string {
	var targets []string
	for _, part := range strings.Split(value, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name != "" && !slices.Contains(targets, name) {
			targets = append(targets, name)
		}
	}
	slices.Sort(targets)
	return targets
}

// objectAnnotations collects the annotations of every object a check can
// report on, by objectKey.
func objectAnnotations(snap *Snapshot) map[string]map[string]string {
	objects := map[string]map[string]string{}
	add := func(kind, namespace, name string, annotations map[string]string) {
		if len(annotations) > 0 {
			objects[objectKey(ObjectRef{Kind: kind, Namespace: namespace, Name: name})] = annotations
		}
	}
	for _, o := range snap.Pods {
		add("Pod", o.Namespace, o.Name, o.Annotations)
	}
	for _, o := range snap.Deployments {
		add("Deployment", o.Namespace, o.Name, o.Annotations)
	}
	for _, o := range snap.StatefulSets {
		add("StatefulSet", o.Namespace, o.Name, o.Annotations)
	}
	for _, o := range snap.DaemonSets {
		add("DaemonSet", o.Namespace, o.Name, o.Annotations)
	}
	for _, o := range snap.Nodes {
		add("Node", "", o.Name, o.Annotations)
	}
	for _, o := range snap.Jobs {
		add("Job", o.Namespace, o.Name, o.Annotations)
	}
	for _, o := range snap.CronJobs {
		add("CronJob", o.Namespace, o.Name, o.Annotations)
	}
	for _, o := range snap.PVCs {
		add("PersistentVolumeClaim", o.Namespace, o.Name, o.Annotations)
	}
	for _, o := range snap.Certificates {
		add("Certificate", o.Meta.Namespace, o.Meta.Name, o.Meta.Annotations)
	}
	for _, o := range snap.Applications {
		add("Application", o.Meta.Namespace, o.Meta.Name, o.Meta.Annotations)
	}
	return objects
}

func objectKey(ref ObjectRef) string {
	return ref.Kind + "/" + ref.Namespace + "/" + ref.Name
}
