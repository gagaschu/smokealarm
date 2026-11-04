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
// annotations of the pod count, which it gets from the pod template; for
// CronJobs also those of the job template.
//
// It runs after the checks, so the checks themselves stay unaware of it. It
// returns a sentence for every severity value it could not use, naming the
// object or namespace that carries it; the problem keeps the severity of the
// check then, so a typo never hides an alarm. Which target names are
// configured is the engine's business; Finding.TargetsFrom tells it where the
// list came from.
func ApplyAnnotations(snap *Snapshot, findings []Finding) ([]Finding, []string) {
	if len(findings) == 0 {
		return findings, nil // a healthy cluster: no need to collect anything
	}
	objects := objectAnnotations(snap)
	namespaces := map[string]map[string]string{}
	for _, ns := range snap.Namespaces {
		namespaces[ns.Name] = ns.Annotations
	}

	var problems []string
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		sources := []annotationSource{{f.Object.Label(), objects[objectKey(f.Object)]}}
		if f.Object.Namespace != "" {
			sources = append(sources, annotationSource{"Namespace " + f.Object.Namespace, namespaces[f.Object.Namespace]})
		}

		if value, from, found := lookup(sources, SeverityAnnotation); found {
			if severity, ok := parseSeverity(value); ok {
				f.Severity = severity
			} else {
				problems = append(problems, fmt.Sprintf("%s: %s %q is neither %q nor %q",
					from, SeverityAnnotation, value, Warning, Critical))
			}
		}
		if value, from, found := lookup(sources, TargetsAnnotation); found {
			f.Targets, f.TargetsFrom = ParseTargets(value), from
		}
		out = append(out, f)
	}
	return out, problems
}

// annotationSource is where annotations can come from: the object itself or
// its namespace, with the label used in log messages.
type annotationSource struct {
	label       string // "Pod media/jellyfin", "Namespace media"
	annotations map[string]string
}

// lookup returns the value of the annotation from the first source that has
// it, and that source's label. An empty value counts as not set, and so does a
// targets list without names (" , ").
func lookup(sources []annotationSource, name string) (value, from string, found bool) {
	for _, src := range sources {
		value := strings.TrimSpace(src.annotations[name])
		if value == "" || (name == TargetsAnnotation && len(ParseTargets(value)) == 0) {
			continue
		}
		return value, src.label, true
	}
	return "", "", false
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
		add(KindPod, o.Namespace, o.Name, o.Annotations)
	}
	for _, o := range snap.Deployments {
		add(KindDeployment, o.Namespace, o.Name, o.Annotations)
	}
	for _, o := range snap.StatefulSets {
		add(KindStatefulSet, o.Namespace, o.Name, o.Annotations)
	}
	for _, o := range snap.DaemonSets {
		add(KindDaemonSet, o.Namespace, o.Name, o.Annotations)
	}
	for _, o := range snap.Nodes {
		add(KindNode, "", o.Name, o.Annotations)
	}
	for _, o := range snap.Jobs {
		add(KindJob, o.Namespace, o.Name, o.Annotations)
	}
	for _, o := range snap.CronJobs {
		add(KindCronJob, o.Namespace, o.Name, withTemplate(o.Annotations, o.Spec.JobTemplate.Annotations))
	}
	for _, o := range snap.PVCs {
		add(KindPVC, o.Namespace, o.Name, o.Annotations)
	}
	for _, o := range snap.Certificates {
		add(KindCertificate, o.Meta.Namespace, o.Meta.Name, o.Meta.Annotations)
	}
	for _, o := range snap.Applications {
		add(KindApplication, o.Meta.Namespace, o.Meta.Name, o.Meta.Annotations)
	}
	return objects
}

// withTemplate merges the annotations of a CronJob with those of its job
// template, which every Job it creates gets; the CronJob's own win. That is
// what the ignore annotation sees on the newest Job (see JobFailed).
func withTemplate(own, template map[string]string) map[string]string {
	if len(template) == 0 {
		return own
	}
	merged := map[string]string{}
	for name, value := range template {
		merged[name] = value
	}
	for name, value := range own {
		merged[name] = value
	}
	return merged
}

func objectKey(ref ObjectRef) string {
	return ref.Kind + "/" + ref.Namespace + "/" + ref.Name
}
