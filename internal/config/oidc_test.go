package config

import (
	"reflect"
	"strings"
	"testing"
)

// oidcEnv is a complete OIDC setup; tests change single values of it.
func oidcEnv(changes map[string]string) map[string]string {
	values := map[string]string{
		"SMOKEALARM_OIDC_ISSUER_URL":     "https://id.example.org/realms/home",
		"SMOKEALARM_OIDC_CLIENT_ID":      "smokealarm",
		"SMOKEALARM_OIDC_CLIENT_SECRET":  "s3cr3t-value-123",
		"SMOKEALARM_OIDC_ALLOWED_EMAILS": "gabriel@example.org",
		"SMOKEALARM_BASE_URL":            "https://smokealarm.example.org",
	}
	for name, value := range changes {
		if value == "-" {
			delete(values, name)
		} else {
			values[name] = value
		}
	}
	return values
}

func TestOIDCIsOffByDefault(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OIDC.Enabled() {
		t.Error("OIDC is on without an issuer URL")
	}
}

func TestOIDCIsRead(t *testing.T) {
	cfg, err := Load(env(oidcEnv(map[string]string{
		"SMOKEALARM_OIDC_NAME":           "MeineID",
		"SMOKEALARM_OIDC_SCOPES":         "openid, email groups",
		"SMOKEALARM_OIDC_GROUPS_CLAIM":   "roles",
		"SMOKEALARM_OIDC_ALLOWED_EMAILS": "Gabriel@Example.org, other@example.org",
		"SMOKEALARM_OIDC_ALLOWED_GROUPS": "ops, Admins",
		"SMOKEALARM_OIDC_CA_FILE":        "/etc/ca.crt",
	})))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := OIDCConfig{
		IssuerURL:     "https://id.example.org/realms/home",
		ClientID:      "smokealarm",
		ClientSecret:  "s3cr3t-value-123",
		Name:          "MeineID",
		Scopes:        []string{"openid", "email", "groups"},
		GroupsClaim:   "roles",
		AllowedEmails: []string{"gabriel@example.org", "other@example.org"}, // lower case
		AllowedGroups: []string{"ops", "Admins"},                            // as written
		CAFile:        "/etc/ca.crt",
	}
	if !reflect.DeepEqual(cfg.OIDC, want) {
		t.Errorf("OIDC =\n%+v\nwant\n%+v", cfg.OIDC, want)
	}
}

func TestOIDCDefaults(t *testing.T) {
	cfg, err := Load(env(oidcEnv(nil)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfg.OIDC
	if c.Name != "OIDC" || c.GroupsClaim != "groups" || !reflect.DeepEqual(c.Scopes, []string{"openid", "email"}) {
		t.Errorf("defaults = name %q, claim %q, scopes %v", c.Name, c.GroupsClaim, c.Scopes)
	}
}

func TestOIDCValidation(t *testing.T) {
	tests := []struct {
		name    string
		changes map[string]string
		wantErr string // empty: valid
	}{
		{"complete", nil, ""},
		{"groups instead of emails", map[string]string{"SMOKEALARM_OIDC_ALLOWED_EMAILS": "-", "SMOKEALARM_OIDC_ALLOWED_GROUPS": "ops"}, ""},
		{"http on localhost", map[string]string{"SMOKEALARM_OIDC_ISSUER_URL": "http://localhost:8081/realms/home"}, ""},
		{"http on 127.0.0.1", map[string]string{"SMOKEALARM_OIDC_ISSUER_URL": "http://127.0.0.1:8081"}, ""},
		{"http on another host", map[string]string{"SMOKEALARM_OIDC_ISSUER_URL": "http://id.example.org"},
			"SMOKEALARM_OIDC_ISSUER_URL: must use https (plain http only for localhost)"},
		{"issuer is no URL", map[string]string{"SMOKEALARM_OIDC_ISSUER_URL": "id.example.org"},
			"SMOKEALARM_OIDC_ISSUER_URL: \"id.example.org\" is not an http(s) URL"},
		{"issuer with a query", map[string]string{"SMOKEALARM_OIDC_ISSUER_URL": "https://id.example.org/?a=b"},
			"SMOKEALARM_OIDC_ISSUER_URL: must not contain a query or a fragment"},
		{"no client id", map[string]string{"SMOKEALARM_OIDC_CLIENT_ID": "-"},
			"SMOKEALARM_OIDC_CLIENT_ID is required"},
		{"no client secret", map[string]string{"SMOKEALARM_OIDC_CLIENT_SECRET": "-"},
			"SMOKEALARM_OIDC_CLIENT_SECRET is required"},
		{"no allowlist", map[string]string{"SMOKEALARM_OIDC_ALLOWED_EMAILS": "-"},
			"SMOKEALARM_OIDC_ALLOWED_EMAILS or SMOKEALARM_OIDC_ALLOWED_GROUPS is required"},
		{"email without @", map[string]string{"SMOKEALARM_OIDC_ALLOWED_EMAILS": "gabriel"},
			"SMOKEALARM_OIDC_ALLOWED_EMAILS: \"gabriel\" is not an email address"},
		{"no base url", map[string]string{"SMOKEALARM_BASE_URL": "-"},
			"SMOKEALARM_BASE_URL is required when SMOKEALARM_OIDC_ISSUER_URL is set"},
		{"scopes without openid", map[string]string{"SMOKEALARM_OIDC_SCOPES": "email groups"},
			"SMOKEALARM_OIDC_SCOPES: must contain \"openid\""},
		{"name too long", map[string]string{"SMOKEALARM_OIDC_NAME": strings.Repeat("x", 41)},
			"SMOKEALARM_OIDC_NAME: at most 40 characters"},
		{"name with a control character", map[string]string{"SMOKEALARM_OIDC_NAME": "My\nID"},
			"SMOKEALARM_OIDC_NAME: must not contain control characters"},
		{"name of 40 characters", map[string]string{"SMOKEALARM_OIDC_NAME": strings.Repeat("ä", 40)}, ""},
		{"setting without the issuer", map[string]string{"SMOKEALARM_OIDC_ISSUER_URL": "-"},
			"SMOKEALARM_OIDC_CLIENT_ID is set, but SMOKEALARM_OIDC_ISSUER_URL is not"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(oidcEnv(tt.changes)))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestOIDCErrorsNeverShowTheSecret(t *testing.T) {
	// Several things wrong at once, with a secret that must not appear.
	_, err := Load(env(oidcEnv(map[string]string{
		"SMOKEALARM_OIDC_ISSUER_URL":     "http://id.example.org",
		"SMOKEALARM_OIDC_ALLOWED_EMAILS": "-",
		"SMOKEALARM_OIDC_SCOPES":         "email",
	})))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "s3cr3t-value-123") {
		t.Errorf("the error shows the client secret: %v", err)
	}
}
