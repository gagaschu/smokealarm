package checks

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func certMeta() metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: "web", Name: "site-tls"}
}

func expiresIn(d time.Duration) *time.Time {
	t := testNow.Add(d)
	return &t
}

func TestCertNotReady(t *testing.T) {
	tests := []struct {
		name        string
		cert        Certificate
		wantMessage string // empty: no finding expected
	}{
		{"ready", Certificate{Meta: certMeta(), Ready: true}, ""},
		{"not ready with details", Certificate{Meta: certMeta(), ReadyReason: "Failed", ReadyMessage: "issuer not found"},
			"Failed: issuer not found"},
		{"no condition yet", Certificate{Meta: certMeta()}, "no Ready condition reported yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &Snapshot{Now: testNow, Certificates: []Certificate{tt.cert}}
			got := CertNotReady(snap, testOptions())

			if tt.wantMessage == "" {
				if len(got) != 0 {
					t.Fatalf("got %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Message != tt.wantMessage ||
				got[0].Key != "cert-not-ready/Certificate/web/site-tls/CertificateNotReady" || got[0].Severity != Warning {
				t.Errorf("got %+v, want one warning with message %q", got, tt.wantMessage)
			}
		})
	}
}

func TestCertExpiry(t *testing.T) {
	tests := []struct {
		name       string
		notAfter   *time.Time
		wantReason string
		wantSev    Severity
	}{
		{"not issued yet", nil, "", ""},
		{"far in the future", expiresIn(60 * 24 * time.Hour), "", ""},
		{"exactly 14 days", expiresIn(14 * 24 * time.Hour), "", ""},
		{"just below 14 days", expiresIn(14*24*time.Hour - time.Minute), "ExpiresSoon", Warning},
		{"5 days", expiresIn(5 * 24 * time.Hour), "ExpiresSoon", Warning},
		{"exactly 3 days", expiresIn(3 * 24 * time.Hour), "ExpiresSoon", Warning},
		{"just below 3 days", expiresIn(3*24*time.Hour - time.Minute), "ExpiresVerySoon", Critical},
		{"1 hour", expiresIn(time.Hour), "ExpiresVerySoon", Critical},
		{"expired a minute ago", expiresIn(-time.Minute), "Expired", Critical},
		{"expired exactly now", expiresIn(0), "Expired", Critical},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &Snapshot{Now: testNow, Certificates: []Certificate{{Meta: certMeta(), Ready: true, NotAfter: tt.notAfter}}}
			got := CertExpiry(snap, testOptions())

			if tt.wantReason == "" {
				if len(got) != 0 {
					t.Fatalf("got %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Reason != tt.wantReason || got[0].Severity != tt.wantSev {
				t.Errorf("got %+v, want %s %s", got, tt.wantSev, tt.wantReason)
			}
		})
	}
}

func TestCertExpiryMessages(t *testing.T) {
	snap := &Snapshot{Now: testNow, Certificates: []Certificate{
		{Meta: certMeta(), NotAfter: expiresIn(9*24*time.Hour + 3*time.Hour)},
	}}
	got := CertExpiry(snap, testOptions())
	want := "expires in 9d 3h (2026-11-12T15:00:00Z)"
	if len(got) != 1 || got[0].Message != want {
		t.Errorf("got %+v, want message %q", got, want)
	}

	snap.Certificates[0].NotAfter = expiresIn(-90 * time.Minute)
	got = CertExpiry(snap, testOptions())
	want = "expired 1h 30m ago (2026-11-03T10:30:00Z)"
	if len(got) != 1 || got[0].Message != want {
		t.Errorf("got %+v, want message %q", got, want)
	}
}

func TestCertChecksRespectIgnore(t *testing.T) {
	annotated := certMeta()
	annotated.Annotations = map[string]string{IgnoreAnnotation: "true"}
	ignoredNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "web", Annotations: map[string]string{IgnoreAnnotation: "true"},
	}}

	broken := func(meta metav1.ObjectMeta) []Certificate {
		return []Certificate{{Meta: meta, NotAfter: expiresIn(time.Hour)}}
	}
	tests := []struct {
		name string
		snap *Snapshot
		opts Options
		want int
	}{
		{"not ignored", &Snapshot{Certificates: broken(certMeta())}, testOptions(), 1},
		{"object annotation", &Snapshot{Certificates: broken(annotated)}, testOptions(), 0},
		{"namespace annotation", &Snapshot{Certificates: broken(certMeta()), Namespaces: []*corev1.Namespace{ignoredNamespace}}, testOptions(), 0},
		{"namespace in env list", &Snapshot{Certificates: broken(certMeta())}, Options{IgnoredNamespaces: []string{"web"}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.snap.Now = testNow
			if got := len(CertNotReady(tt.snap, tt.opts)); got != tt.want {
				t.Errorf("CertNotReady: %d findings, want %d", got, tt.want)
			}
			if got := len(CertExpiry(tt.snap, tt.opts)); got != tt.want {
				t.Errorf("CertExpiry: %d findings, want %d", got, tt.want)
			}
		})
	}
}

func TestOptionalChecksWithoutData(t *testing.T) {
	// When the CRDs are not installed the snapshot has no objects of that
	// kind, and the checks must simply find nothing.
	snap := &Snapshot{Now: testNow}
	for _, check := range []Check{
		{Name: "cert-not-ready", Run: CertNotReady}, {Name: "cert-expiry", Run: CertExpiry},
		{Name: "argocd-health", Run: ArgoCDHealth}, {Name: "argocd-sync", Run: ArgoCDSync},
		{Name: "pvc-usage", Run: PVCUsage},
	} {
		if got := check.Run(snap, testOptions()); len(got) != 0 {
			t.Errorf("%s: got %+v, want none", check.Name, got)
		}
	}
}

func TestOptionalChecksRequireTheirFeature(t *testing.T) {
	want := map[string]string{
		"cert-not-ready": RequiresCertManager, "cert-expiry": RequiresCertManager,
		"argocd-health": RequiresArgoCD, "argocd-sync": RequiresArgoCD,
		"pvc-usage": "", "pod-crashloop": "",
	}
	for _, c := range All() {
		if expected, ok := want[c.Name]; ok && c.Requires != expected {
			t.Errorf("%s: Requires = %q, want %q", c.Name, c.Requires, expected)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Minute, "5m"},
		{time.Minute, "1m"},
		{59*time.Minute + 59*time.Second, "59m"},
		{time.Hour, "1h 0m"},
		{90 * time.Minute, "1h 30m"},
		{24 * time.Hour, "1d 0h"},
		{26 * time.Hour, "1d 2h"},
		{3*24*time.Hour + 4*time.Hour + 30*time.Minute, "3d 4h"},
		{20 * time.Second, "less than a minute"},
		{-time.Hour, "less than a minute"},
	}
	for _, tt := range tests {
		if got := HumanDuration(tt.d); got != tt.want {
			t.Errorf("HumanDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestCertExpiryKeyIsStableAcrossSeverities(t *testing.T) {
	keyFor := func(remaining time.Duration) (string, string) {
		snap := &Snapshot{Now: testNow, Certificates: []Certificate{{Meta: certMeta(), Ready: true, NotAfter: expiresIn(remaining)}}}
		got := CertExpiry(snap, testOptions())
		if len(got) != 1 {
			t.Fatalf("remaining %v: got %+v, want one finding", remaining, got)
		}
		return got[0].Key, got[0].Reason
	}

	soonKey, soonReason := keyFor(10 * 24 * time.Hour)
	verySoonKey, verySoonReason := keyFor(2 * 24 * time.Hour)
	expiredKey, expiredReason := keyFor(-time.Hour)

	if soonReason != "ExpiresSoon" || verySoonReason != "ExpiresVerySoon" || expiredReason != "Expired" {
		t.Fatalf("reasons = %s, %s, %s", soonReason, verySoonReason, expiredReason)
	}
	if soonKey != verySoonKey || soonKey != expiredKey {
		t.Errorf("key changed with the severity: %q, %q, %q", soonKey, verySoonKey, expiredKey)
	}
}

func TestCertExpiryMessageUsesTheConfiguredZone(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatal(err)
	}
	snap := &Snapshot{Now: testNow, Certificates: []Certificate{
		{Meta: certMeta(), NotAfter: expiresIn(9*24*time.Hour + 3*time.Hour)},
	}}
	opts := testOptions()
	opts.Location = loc
	got := CertExpiry(snap, opts)
	want := "expires in 9d 3h (2026-11-12T16:00:00+01:00)"
	if len(got) != 1 || got[0].Message != want {
		t.Errorf("got %+v, want message %q", got, want)
	}
}
