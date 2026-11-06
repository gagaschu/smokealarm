package main

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gagaschu/smokealarm/internal/auth"
	"github.com/gagaschu/smokealarm/internal/config"
)

// oidcCallbackPath is where the identity provider sends the browser back. It
// is also what the user registers at the provider, so the docs name it.
const oidcCallbackPath = "/login/oidc/callback"

// newOIDC creates the OIDC login from the configuration. It does not contact
// the provider: that happens at the first login, so smokealarm starts even
// when the provider is down. The configuration was checked by config.Load, so
// the base URL is there.
func newOIDC(cfg config.Config, logger *slog.Logger) (*auth.OIDC, error) {
	client, err := auth.NewOIDCHTTPClient(cfg.OIDC.CAFile)
	if err != nil {
		return nil, fmt.Errorf("create OIDC client: %w", err)
	}
	redirectURL := strings.TrimSuffix(cfg.BaseURL, "/") + oidcCallbackPath
	logger.Info("OIDC login is on", "issuer", cfg.OIDC.IssuerURL, "redirect_uri", redirectURL)

	return &auth.OIDC{
		IssuerURL:     cfg.OIDC.IssuerURL,
		ClientID:      cfg.OIDC.ClientID,
		ClientSecret:  cfg.OIDC.ClientSecret,
		RedirectURL:   redirectURL,
		Scopes:        cfg.OIDC.Scopes,
		GroupsClaim:   cfg.OIDC.GroupsClaim,
		AllowedEmails: cfg.OIDC.AllowedEmails,
		AllowedGroups: cfg.OIDC.AllowedGroups,
		SecureCookie:  cfg.CookieSecure,
		Client:        client,
		Now:           time.Now,
	}, nil
}

// oidcFingerprint is the value that OIDC sessions carry: it changes when the
// provider, the client, the groups claim or an allowlist changes, which ends
// those sessions.
func oidcFingerprint(c config.OIDCConfig) string {
	return auth.OIDCFingerprint(c.IssuerURL, c.ClientID, c.GroupsClaim, c.AllowedEmails, c.AllowedGroups)
}
