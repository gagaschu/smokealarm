// Package helm tests the Helm chart in deploy/helm/smokealarm by rendering it
// with "helm template" and checking the resulting Kubernetes objects. The
// tests are skipped if the helm binary is not installed.
package helm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// baseValues is the minimum a user must provide: the password hash.
const baseValues = "auth:\n  passwordHash: '$2a$10$abcdefghijklmnopqrstuuvwxyz0123456789ABCDEFGHIJKLMNOPQR'\n"

// release is a rendered chart.
type release struct {
	t       *testing.T
	objects []*unstructured.Unstructured
	raw     string
}

// helmRun runs a helm command on the chart with the given values files
// content, in order, and returns its output (stderr if it failed) and error.
// command is "template", or "install" with --dry-run=client.
func helmRun(t *testing.T, command []string, releaseName string, values ...string) (string, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}

	args := append(slices.Clone(command), releaseName, "smokealarm", "--namespace", "monitoring")
	for i, content := range values {
		file := filepath.Join(t.TempDir(), "values-"+string(rune('a'+i))+".yaml")
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-f", file)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, helm, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stderr.String(), err
	}
	return stdout.String(), nil
}

// helmTemplate renders the chart. It returns the helm output and its error.
func helmTemplate(t *testing.T, releaseName string, values ...string) (string, error) {
	t.Helper()
	return helmRun(t, []string{"template"}, releaseName, values...)
}

// helmNotes returns the text that "helm install" prints after the install
// (NOTES.txt). A dry run on the client side needs no cluster.
func helmNotes(t *testing.T, values ...string) string {
	t.Helper()
	out, err := helmRun(t, []string{"install", "--dry-run=client"}, "smokealarm", append([]string{baseValues}, values...)...)
	if err != nil {
		t.Fatalf("helm install --dry-run failed: %v\n%s", err, out)
	}
	_, notes, found := strings.Cut(out, "NOTES:\n")
	if !found {
		t.Fatalf("helm printed no NOTES:\n%s", out)
	}
	return notes
}

// render renders the chart with the required hash plus extra values and fails
// the test if rendering fails.
func render(t *testing.T, extra string) *release {
	t.Helper()
	out, err := helmTemplate(t, "smokealarm", baseValues, extra)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return parseRelease(t, out)
}

func parseRelease(t *testing.T, out string) *release {
	t.Helper()
	r := &release{t: t, raw: out}
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(out), 4096)
	for {
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(&obj.Object)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("rendered output is not valid YAML: %v", err)
		}
		if len(obj.Object) > 0 { // empty documents between "---" are skipped
			r.objects = append(r.objects, obj)
		}
	}
	return r
}

// find returns the object of a kind, or nil.
func (r *release) find(kind string) *unstructured.Unstructured {
	for _, o := range r.objects {
		if o.GetKind() == kind {
			return o
		}
	}
	return nil
}

func (r *release) kinds() []string {
	var kinds []string
	for _, o := range r.objects {
		kinds = append(kinds, o.GetKind())
	}
	sort.Strings(kinds)
	return kinds
}

func (r *release) has(kind string) bool { return r.find(kind) != nil }

// as converts an object of the given kind into a typed Kubernetes object.
func (r *release) as(kind string, into any) {
	r.t.Helper()
	obj := r.find(kind)
	if obj == nil {
		r.t.Fatalf("the chart rendered no %s; got %v", kind, r.kinds())
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, into); err != nil {
		r.t.Fatalf("convert %s: %v", kind, err)
	}
}

func (r *release) deployment() appsv1.Deployment {
	r.t.Helper()
	var d appsv1.Deployment
	r.as("Deployment", &d)
	return d
}

// env returns the environment of the main container as a map.
func (r *release) env() map[string]string {
	r.t.Helper()
	env := map[string]string{}
	for _, e := range r.deployment().Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

func TestDefaultInstallRendersTheExpectedObjects(t *testing.T) {
	r := render(t, "")
	want := []string{"ClusterRole", "ClusterRoleBinding", "Deployment", "PersistentVolumeClaim", "Secret", "Service", "ServiceAccount"}
	if got := r.kinds(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("kinds = %v, want %v (no Ingress by default)", got, want)
	}
}

func TestDeploymentIsASingleRecreatePod(t *testing.T) {
	d := render(t, "").deployment()
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want 1 (SQLite)", d.Spec.Replicas)
	}
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("strategy = %s, want Recreate: the old pod must release the volume first", d.Spec.Strategy.Type)
	}
	for k, v := range d.Spec.Selector.MatchLabels {
		if d.Spec.Template.Labels[k] != v {
			t.Errorf("selector label %s=%s is not on the pod template", k, v)
		}
	}
}

func TestServiceSelectsThePods(t *testing.T) {
	r := render(t, "")
	var svc corev1.Service
	r.as("Service", &svc)
	d := r.deployment()
	for k, v := range svc.Spec.Selector {
		if d.Spec.Template.Labels[k] != v {
			t.Errorf("service selector %s=%s does not match the pods", k, v)
		}
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.StrVal != "http" {
		t.Errorf("service ports = %+v, want 80 -> http", svc.Spec.Ports)
	}
}

func TestSecurityContext(t *testing.T) {
	pod := render(t, "").deployment().Spec.Template.Spec
	podSC := pod.SecurityContext
	if podSC == nil || podSC.RunAsNonRoot == nil || !*podSC.RunAsNonRoot ||
		podSC.RunAsUser == nil || *podSC.RunAsUser != 65532 || podSC.FSGroup == nil || *podSC.FSGroup != 65532 ||
		podSC.SeccompProfile == nil || podSC.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context = %+v", podSC)
	}

	sc := pod.Containers[0].SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("container security context = %+v", sc)
	}
	if sc.Privileged != nil && *sc.Privileged {
		t.Error("the container must not be privileged")
	}
}

func TestOnlyTheVolumeIsWritable(t *testing.T) {
	pod := render(t, "").deployment().Spec.Template.Spec
	mounts := pod.Containers[0].VolumeMounts
	if len(mounts) != 1 || mounts[0].MountPath != "/data" || mounts[0].ReadOnly {
		t.Errorf("volume mounts = %+v, want only /data", mounts)
	}
	for _, v := range pod.Volumes {
		if v.HostPath != nil {
			t.Errorf("volume %s is a hostPath", v.Name)
		}
	}
	env := render(t, "").env()
	if env["SMOKEALARM_DB_PATH"] != "/data/smokealarm.db" || env["TMPDIR"] != "/data" {
		t.Errorf("database and temp files must be on /data: %v", env)
	}
}

func TestResourcesAndProbes(t *testing.T) {
	c := render(t, "").deployment().Spec.Template.Spec.Containers[0]
	if c.Resources.Requests.Cpu().String() != "10m" || c.Resources.Requests.Memory().String() != "64Mi" ||
		c.Resources.Limits.Memory().String() != "128Mi" {
		t.Errorf("resources = %+v", c.Resources)
	}
	if c.LivenessProbe == nil || c.LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("liveness probe = %+v, want /healthz", c.LivenessProbe)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Errorf("readiness probe = %+v, want /readyz", c.ReadinessProbe)
	}

	// Start-up (database, migrations, cache sync) can be slow on a Raspberry
	// Pi: a startup probe protects it from the liveness probe, and a timeout
	// longer than the default 1s keeps a busy moment from counting as a failure.
	if c.StartupProbe == nil || c.StartupProbe.HTTPGet.Path != "/healthz" {
		t.Fatalf("startup probe = %+v, want /healthz", c.StartupProbe)
	}
	if window := c.StartupProbe.PeriodSeconds * c.StartupProbe.FailureThreshold; window < 120 {
		t.Errorf("startup probe allows %ds, want at least 120s", window)
	}
	for name, probe := range map[string]*corev1.Probe{"startup": c.StartupProbe, "liveness": c.LivenessProbe, "readiness": c.ReadinessProbe} {
		if probe.TimeoutSeconds < 3 {
			t.Errorf("%s probe timeout = %ds, want at least 3s", name, probe.TimeoutSeconds)
		}
	}
}

func TestPasswordHashIsOnlyInTheSecret(t *testing.T) {
	r := render(t, "")
	var secret corev1.Secret
	r.as("Secret", &secret)
	hash := secret.StringData["SMOKEALARM_ADMIN_PASSWORD_HASH"]
	if !strings.HasPrefix(hash, "$2a$10$") {
		t.Fatalf("secret does not hold the hash: %v", secret.StringData)
	}

	deployment := r.find("Deployment")
	raw, _ := deployment.MarshalJSON()
	if strings.Contains(string(raw), hash) {
		t.Error("the password hash appears in the Deployment; it belongs in the Secret only")
	}
	envFrom := r.deployment().Spec.Template.Spec.Containers[0].EnvFrom
	if len(envFrom) != 1 || envFrom[0].SecretRef == nil || envFrom[0].SecretRef.Name != "smokealarm" {
		t.Errorf("envFrom = %+v, want the chart's Secret", envFrom)
	}
}

func TestMissingPasswordHashFailsTheInstall(t *testing.T) {
	out, err := helmTemplate(t, "smokealarm", "")
	if err == nil || !strings.Contains(out, "auth.passwordHash") {
		t.Errorf("err = %v, output %q; want an error asking for auth.passwordHash", err, out)
	}
}

func TestExistingSecret(t *testing.T) {
	// No hash needed in the values when the Secret comes from the user.
	out, err := helmTemplate(t, "smokealarm", "existingSecret: my-secret\n")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	r := parseRelease(t, out)
	if r.has("Secret") {
		t.Error("the chart must not create a Secret when existingSecret is set")
	}
	envFrom := r.deployment().Spec.Template.Spec.Containers[0].EnvFrom
	if len(envFrom) != 1 || envFrom[0].SecretRef.Name != "my-secret" {
		t.Errorf("envFrom = %+v, want my-secret", envFrom)
	}
}

// ruleSet lists "group/resource:verbs" for every rule of the ClusterRole.
func ruleSet(t *testing.T, r *release) []string {
	t.Helper()
	var role rbacv1.ClusterRole
	r.as("ClusterRole", &role)
	var rules []string
	for _, rule := range role.Rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				rules = append(rules, group+"/"+resource+":"+strings.Join(rule.Verbs, ","))
			}
		}
	}
	sort.Strings(rules)
	return rules
}

const readOnly = ":get,list,watch"

func coreRules() []string {
	return []string{
		"/events" + readOnly, "/namespaces" + readOnly, "/nodes" + readOnly, "/persistentvolumeclaims" + readOnly, "/pods" + readOnly,
		"apps/daemonsets" + readOnly, "apps/deployments" + readOnly, "apps/statefulsets" + readOnly,
		"batch/cronjobs" + readOnly, "batch/jobs" + readOnly,
		"storage.k8s.io/storageclasses" + readOnly,
	}
}

func joined(lists ...[]string) string {
	var all []string
	for _, l := range lists {
		all = append(all, l...)
	}
	sort.Strings(all)
	return strings.Join(all, "\n")
}

func TestRBACDefaultAllowsOnlyReading(t *testing.T) {
	r := render(t, "")
	optional := []string{"/nodes/proxy:get", "argoproj.io/applications" + readOnly, "cert-manager.io/certificates" + readOnly}
	if got := strings.Join(ruleSet(t, r), "\n"); got != joined(coreRules(), optional) {
		t.Errorf("rules =\n%s\nwant\n%s", got, joined(coreRules(), optional))
	}

	var role rbacv1.ClusterRole
	r.as("ClusterRole", &role)
	for _, rule := range role.Rules {
		for _, verb := range rule.Verbs {
			if verb != "get" && verb != "list" && verb != "watch" {
				t.Errorf("rule %+v allows %q; smokealarm must only read", rule, verb)
			}
		}
		for _, resource := range rule.Resources {
			if resource == "secrets" || resource == "*" {
				t.Errorf("rule %+v grants %q", rule, resource)
			}
		}
	}
}

func TestRBACSwitches(t *testing.T) {
	tests := []struct {
		name         string
		values       string
		wantOptional []string
		wantDisabled string // comma separated, exact
	}{
		{"nothing optional", "rbac:\n  nodesProxy: false\n  certManager: false\n  argocd: false\n", nil,
			"pvc-usage,cert-not-ready,cert-expiry,argocd-health,argocd-sync"},
		{"no nodes/proxy", "rbac:\n  nodesProxy: false\n",
			[]string{"argoproj.io/applications" + readOnly, "cert-manager.io/certificates" + readOnly}, "pvc-usage"},
		{"no cert-manager", "rbac:\n  certManager: false\n",
			[]string{"/nodes/proxy:get", "argoproj.io/applications" + readOnly}, "cert-not-ready,cert-expiry"},
		{"no argocd", "rbac:\n  argocd: false\n",
			[]string{"/nodes/proxy:get", "cert-manager.io/certificates" + readOnly}, "argocd-health,argocd-sync"},
		// Switching the check off by name must also drop the permission.
		{"pvc-usage disabled by the user", "disabledChecks: [pvc-usage]\n",
			[]string{"argoproj.io/applications" + readOnly, "cert-manager.io/certificates" + readOnly}, "pvc-usage"},
		{"user list is merged without duplicates", "disabledChecks: [pod-restarts, pvc-usage]\nrbac:\n  nodesProxy: false\n",
			[]string{"argoproj.io/applications" + readOnly, "cert-manager.io/certificates" + readOnly}, "pod-restarts,pvc-usage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := render(t, tt.values)
			if got := strings.Join(ruleSet(t, r), "\n"); got != joined(coreRules(), tt.wantOptional) {
				t.Errorf("rules =\n%s\nwant\n%s", got, joined(coreRules(), tt.wantOptional))
			}
			if got := r.env()["SMOKEALARM_DISABLED_CHECKS"]; got != tt.wantDisabled {
				t.Errorf("SMOKEALARM_DISABLED_CHECKS = %q, want %q", got, tt.wantDisabled)
			}
		})
	}
}

// rbacSwitches maps each optional permission switch of the chart to the checks
// that need that permission.
func rbacSwitches() []struct {
	value  string
	covers func(checks.Check) bool
} {
	return []struct {
		value  string
		covers func(checks.Check) bool
	}{
		{"nodesProxy", func(c checks.Check) bool { return c.Name == checks.PVCUsageCheck }},
		{"certManager", func(c checks.Check) bool { return c.Requires == checks.RequiresCertManager }},
		{"argocd", func(c checks.Check) bool { return c.Requires == checks.RequiresArgoCD }},
	}
}

// The chart lists check names per switch in _helpers.tpl. This test ties that
// list to checks.All(), so a new check cannot run without its permission.
func TestRBACSwitchesCoverEveryCheckThatNeedsThem(t *testing.T) {
	for _, c := range checks.All() {
		if c.Requires == "" && c.Name != checks.PVCUsageCheck {
			continue
		}
		covered := false
		for _, sw := range rbacSwitches() {
			covered = covered || sw.covers(c)
		}
		if !covered {
			t.Errorf("check %s needs an optional permission, but no rbac switch covers it: add one to values.yaml, "+
				"clusterrole.yaml and smokealarm.disabledChecks in _helpers.tpl, and list it in rbacSwitches", c.Name)
		}
	}

	for _, sw := range rbacSwitches() {
		var want []string
		for _, c := range checks.All() {
			if sw.covers(c) {
				want = append(want, c.Name)
			}
		}
		got := strings.Split(render(t, "rbac:\n  "+sw.value+": false\n").env()["SMOKEALARM_DISABLED_CHECKS"], ",")
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("rbac.%s=false disables %v, want exactly the checks that need it: %v", sw.value, got, want)
		}
	}
}

// The ClusterRole is cluster-wide: its name must not clash when the chart is
// installed in two namespaces.
func TestClusterRoleNameIncludesTheNamespace(t *testing.T) {
	r := render(t, "")
	var role rbacv1.ClusterRole
	r.as("ClusterRole", &role)
	var binding rbacv1.ClusterRoleBinding
	r.as("ClusterRoleBinding", &binding)

	if role.Name != "monitoring-smokealarm" || binding.Name != role.Name || binding.RoleRef.Name != role.Name {
		t.Errorf("role %q, binding %q -> %q; want all named monitoring-smokealarm (namespace-release)",
			role.Name, binding.Name, binding.RoleRef.Name)
	}
}

func TestNotes(t *testing.T) {
	tests := []struct {
		name    string
		values  string
		want    []string
		notWant []string
	}{
		{"port-forward", "",
			[]string{"port-forward", "config.cookieSecure is true"}, []string{"ingress.tls", "resolved"}},
		{"port-forward with insecure cookie", "config:\n  cookieSecure: false\n",
			[]string{"port-forward"}, []string{"cookieSecure"}},
		{"ingress without tls warns about the cookie", "ingress:\n  enabled: true\n  host: smoke.example.com\n",
			[]string{"http://smoke.example.com", "ingress.tls.enabled is false", "config.cookieSecure"}, []string{"port-forward"}},
		{"ingress with tls", "ingress:\n  enabled: true\n  host: smoke.example.com\n  tls:\n    enabled: true\n",
			[]string{"https://smoke.example.com"}, []string{"cookieSecure", "port-forward"}},
		{"ingress without tls and insecure cookie", "ingress:\n  enabled: true\n  host: smoke.example.com\nconfig:\n  cookieSecure: false\n",
			[]string{"http://smoke.example.com"}, []string{"cookieSecure"}},
		{"disabled checks warn about resolved mails", "disabledChecks: [pod-restarts]\n",
			[]string{"Disabled checks: pod-restarts", `"resolved"`}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notes := helmNotes(t, tt.values)
			for _, text := range tt.want {
				if !strings.Contains(notes, text) {
					t.Errorf("notes do not contain %q:\n%s", text, notes)
				}
			}
			for _, text := range tt.notWant {
				if strings.Contains(notes, text) {
					t.Errorf("notes contain %q, which does not apply:\n%s", text, notes)
				}
			}
		})
	}
}

func TestNothingDisabledByDefault(t *testing.T) {
	if got, found := render(t, "").env()["SMOKEALARM_DISABLED_CHECKS"]; found {
		t.Errorf("SMOKEALARM_DISABLED_CHECKS = %q, want it unset by default", got)
	}
}

func TestRBACCanBeLeftToTheUser(t *testing.T) {
	r := render(t, "rbac:\n  create: false\n")
	if r.has("ClusterRole") || r.has("ClusterRoleBinding") {
		t.Errorf("rbac.create=false must not render RBAC objects, got %v", r.kinds())
	}
}

func TestMail(t *testing.T) {
	off := render(t, "").env()
	for name := range off {
		if strings.HasPrefix(name, "SMOKEALARM_SMTP_") {
			t.Errorf("%s is set although mail is off", name)
		}
	}

	r := render(t, "smtp:\n  host: mail.example.com\n  port: 465\n  tls: tls\n  from: smokealarm@example.com\n  to: [me@example.com, ops@example.com]\n")
	env := r.env()
	want := map[string]string{
		"SMOKEALARM_SMTP_HOST": "mail.example.com", "SMOKEALARM_SMTP_PORT": "465", "SMOKEALARM_SMTP_TLS": "tls",
		"SMOKEALARM_SMTP_FROM": "smokealarm@example.com", "SMOKEALARM_SMTP_TO": "me@example.com,ops@example.com",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if _, found := env["SMOKEALARM_SMTP_PASSWORD"]; found {
		t.Error("the SMTP password must never be a plain env var")
	}
}

func TestMailLoginGoesIntoTheSecret(t *testing.T) {
	r := render(t, "smtp:\n  host: mail.example.com\n  from: a@example.com\n  to: [b@example.com]\n  username: alice\n  password: s3cret\n")
	var secret corev1.Secret
	r.as("Secret", &secret)
	if secret.StringData["SMOKEALARM_SMTP_USERNAME"] != "alice" || secret.StringData["SMOKEALARM_SMTP_PASSWORD"] != "s3cret" {
		t.Errorf("secret = %v, want the SMTP login", secret.StringData)
	}
	raw, _ := r.find("Deployment").MarshalJSON()
	if strings.Contains(string(raw), "s3cret") {
		t.Error("the SMTP password appears in the Deployment")
	}

	out, err := helmTemplate(t, "smokealarm", baseValues, "smtp:\n  username: alice\n")
	if err == nil || !strings.Contains(out, "smtp.password") {
		t.Errorf("err = %v, output %q; want an error: username without password", err, out)
	}
}

func TestIngressAndBaseURL(t *testing.T) {
	tests := []struct {
		name        string
		values      string
		wantIngress bool
		wantBaseURL string
		wantTLS     bool
	}{
		{"no ingress", "", false, "", false},
		{"ingress over http", "ingress:\n  enabled: true\n  host: smoke.example.com\n", true, "http://smoke.example.com", false},
		{"ingress with tls", "ingress:\n  enabled: true\n  host: smoke.example.com\n  tls:\n    enabled: true\n    secretName: smoke-tls\n", true, "https://smoke.example.com", true},
		{"explicit base url wins", "config:\n  baseUrl: https://alarm.example.org\ningress:\n  enabled: true\n  host: smoke.example.com\n", true, "https://alarm.example.org", false},
		{"base url without ingress", "config:\n  baseUrl: https://alarm.example.org\n", false, "https://alarm.example.org", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := render(t, tt.values)
			if r.has("Ingress") != tt.wantIngress {
				t.Fatalf("Ingress rendered = %v, want %v", r.has("Ingress"), tt.wantIngress)
			}
			if got := r.env()["SMOKEALARM_BASE_URL"]; got != tt.wantBaseURL {
				t.Errorf("SMOKEALARM_BASE_URL = %q, want %q", got, tt.wantBaseURL)
			}
			if !tt.wantIngress {
				return
			}
			var ing networkingv1.Ingress
			r.as("Ingress", &ing)
			if ing.Spec.Rules[0].Host != "smoke.example.com" {
				t.Errorf("host = %q", ing.Spec.Rules[0].Host)
			}
			if (len(ing.Spec.TLS) == 1) != tt.wantTLS {
				t.Errorf("tls = %+v, want enabled = %v", ing.Spec.TLS, tt.wantTLS)
			}
			if backend := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service; backend.Name != "smokealarm" || backend.Port.Name != "http" {
				t.Errorf("backend = %+v", backend)
			}
		})
	}
}

func TestPersistence(t *testing.T) {
	tests := []struct {
		name      string
		values    string
		wantPVC   bool
		wantClaim string // "" means an emptyDir is expected
	}{
		{"default", "", true, "smokealarm"},
		{"disabled", "persistence:\n  enabled: false\n", false, ""},
		{"existing claim", "persistence:\n  existingClaim: my-data\n", false, "my-data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := render(t, tt.values)
			if r.has("PersistentVolumeClaim") != tt.wantPVC {
				t.Errorf("PVC rendered = %v, want %v", r.has("PersistentVolumeClaim"), tt.wantPVC)
			}
			volume := r.deployment().Spec.Template.Spec.Volumes[0]
			switch {
			case tt.wantClaim == "" && volume.EmptyDir == nil:
				t.Errorf("volume = %+v, want an emptyDir", volume)
			case tt.wantClaim != "" && (volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName != tt.wantClaim):
				t.Errorf("volume = %+v, want claim %s", volume, tt.wantClaim)
			}
		})
	}

	var pvc corev1.PersistentVolumeClaim
	render(t, "persistence:\n  storageClass: fast\n  size: 5Gi\n").as("PersistentVolumeClaim", &pvc)
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast" ||
		pvc.Spec.Resources.Requests.Storage().String() != "5Gi" ||
		len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("pvc spec = %+v", pvc.Spec)
	}
}

func TestSettingsBecomeEnvironmentVariables(t *testing.T) {
	env := render(t, `auth:
  adminUser: ops
config:
  alertAfter: 5m
  retention: 14d
  cookieSecure: false
  restartThreshold: 10
  ignoreNamespaces: [kube-system, monitoring]
`).env()
	want := map[string]string{
		"SMOKEALARM_ADMIN_USER": "ops", "SMOKEALARM_ALERT_AFTER": "5m", "SMOKEALARM_RETENTION": "14d",
		"SMOKEALARM_COOKIE_SECURE": "false", "SMOKEALARM_RESTART_THRESHOLD": "10",
		"SMOKEALARM_IGNORE_NAMESPACES": "kube-system,monitoring",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
}

func TestNamingFollowsTheReleaseName(t *testing.T) {
	out, err := helmTemplate(t, "alarm", baseValues)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if got := parseRelease(t, out).find("Deployment").GetName(); got != "alarm-smokealarm" {
		t.Errorf("deployment name = %q, want alarm-smokealarm", got)
	}

	out, err = helmTemplate(t, "smokealarm", baseValues)
	if err != nil {
		t.Fatal(err)
	}
	if got := parseRelease(t, out).find("Deployment").GetName(); got != "smokealarm" {
		t.Errorf("deployment name = %q, want smokealarm (no doubled name)", got)
	}
}

func TestServiceAccountTokenIsMounted(t *testing.T) {
	r := render(t, "")
	var sa corev1.ServiceAccount
	r.as("ServiceAccount", &sa)
	if sa.AutomountServiceAccountToken == nil || !*sa.AutomountServiceAccountToken {
		t.Error("smokealarm needs the service account token to reach the API")
	}
	var binding rbacv1.ClusterRoleBinding
	r.as("ClusterRoleBinding", &binding)
	if len(binding.Subjects) != 1 || binding.Subjects[0].Name != "smokealarm" || binding.Subjects[0].Namespace != "monitoring" {
		t.Errorf("binding subjects = %+v, want the service account in the release namespace", binding.Subjects)
	}
}
