package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/gagaschu/smokealarm/internal/auth"
	"github.com/gagaschu/smokealarm/internal/config"
)

func oidcTestConfig() config.Config {
	return config.Config{
		BaseURL:      "https://smokealarm.example.org/",
		CookieSecure: true,
		OIDC: config.OIDCConfig{
			IssuerURL: "https://id.example.org/realms/home", ClientID: "smokealarm", ClientSecret: "secret",
			Scopes: []string{"openid", "email"}, GroupsClaim: "groups",
			AllowedEmails: []string{"gabriel@example.org"}, AllowedGroups: []string{"ops"},
		},
	}
}

func TestNewOIDCBuildsTheRedirectURIFromTheBaseURL(t *testing.T) {
	login, err := newOIDC(oidcTestConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newOIDC: %v", err)
	}
	// The trailing slash of the base URL must not end up twice.
	if want := "https://smokealarm.example.org/login/oidc/callback"; login.RedirectURL != want {
		t.Errorf("RedirectURL = %q, want %q", login.RedirectURL, want)
	}
	if !login.SecureCookie || login.Client == nil || login.Now == nil {
		t.Errorf("OIDC = %+v, want a secure cookie, a client and a clock", login)
	}
}

func TestNewOIDCFailsForAnUnusableCAFile(t *testing.T) {
	cfg := oidcTestConfig()
	cfg.OIDC.CAFile = "/does/not/exist.crt"
	_, err := newOIDC(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "SMOKEALARM_OIDC_CA_FILE") {
		t.Errorf("error = %v, want one that names SMOKEALARM_OIDC_CA_FILE", err)
	}
}

func TestOIDCFingerprintFollowsTheAllowlists(t *testing.T) {
	c := oidcTestConfig().OIDC
	want := auth.OIDCFingerprint(c.IssuerURL, c.ClientID, c.GroupsClaim, c.AllowedEmails, c.AllowedGroups)
	if oidcFingerprint(c) != want {
		t.Error("the fingerprint does not use issuer, client, groups claim and both allowlists")
	}
	c.AllowedGroups = []string{"ops", "dev"}
	if oidcFingerprint(c) == want {
		t.Error("a new group did not change the fingerprint")
	}
}
