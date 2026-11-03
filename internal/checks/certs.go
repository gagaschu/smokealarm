package checks

import (
	"fmt"
	"time"
)

const (
	checkCertNotReady = "cert-not-ready"
	checkCertExpiry   = "cert-expiry"
)

// Expiry thresholds from the specification. cert-manager renews a certificate
// long before it expires, so little remaining time means renewal is failing.
const (
	certWarnBefore     = 14 * 24 * time.Hour
	certCriticalBefore = 3 * 24 * time.Hour
)

// CertNotReady reports cert-manager Certificates whose Ready condition is not
// true: the certificate was not issued or could not be renewed. Severity is
// warning; CertExpiry raises the alarm once it gets close to expiring.
func CertNotReady(snap *Snapshot, opts Options) []Finding {
	var findings []Finding
	for _, cert := range snap.Certificates {
		if cert.Ready || isIgnored(snap, opts, cert.Meta) {
			continue
		}
		msg := "no Ready condition reported yet"
		if cert.ReadyReason != "" {
			msg = fmt.Sprintf("%s: %s", cert.ReadyReason, cert.ReadyMessage)
		}
		findings = append(findings, newFinding(checkCertNotReady, Warning, certRef(cert), "CertificateNotReady", msg))
	}
	return findings
}

// CertExpiry reports certificates that expire soon: warning below 14 days,
// critical below 3 days, and critical (reason Expired) once expired. It is
// one problem that gets worse, so the finding key does not change with the
// reason. Certificates without notAfter (not issued yet) are left to
// CertNotReady.
func CertExpiry(snap *Snapshot, opts Options) []Finding {
	var findings []Finding
	for _, cert := range snap.Certificates {
		if cert.NotAfter == nil || isIgnored(snap, opts, cert.Meta) {
			continue
		}
		remaining := cert.NotAfter.Sub(snap.Now)
		ref := certRef(cert)
		when := cert.NotAfter.UTC().Format(time.RFC3339)

		switch {
		case remaining <= 0:
			msg := fmt.Sprintf("expired %s ago (%s)", humanDuration(-remaining), when)
			findings = append(findings, newEscalatingFinding(checkCertExpiry, Critical, ref, "expiry", "Expired", msg))
		case remaining < certCriticalBefore:
			msg := fmt.Sprintf("expires in %s (%s)", humanDuration(remaining), when)
			findings = append(findings, newEscalatingFinding(checkCertExpiry, Critical, ref, "expiry", "ExpiresVerySoon", msg))
		case remaining < certWarnBefore:
			msg := fmt.Sprintf("expires in %s (%s)", humanDuration(remaining), when)
			findings = append(findings, newEscalatingFinding(checkCertExpiry, Warning, ref, "expiry", "ExpiresSoon", msg))
		}
	}
	return findings
}

func certRef(cert Certificate) ObjectRef {
	return ObjectRef{Kind: "Certificate", Namespace: cert.Meta.Namespace, Name: cert.Meta.Name}
}

// humanDuration writes a duration as "9d 3h", "5h 20m" or "12m".
func humanDuration(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}
