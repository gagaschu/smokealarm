package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// webhookVersion is the version of the JSON format. It changes only if a field
// is removed or changes its meaning; new fields can be added without it.
const webhookVersion = 1

// Webhook posts the notification as JSON to a URL of the user's choice, for
// example a relay such as Apprise or n8n that passes it on. The format is
// described in the README.
type Webhook struct {
	URL string // may contain a secret (many services put a token in the path)
	// HeaderName and HeaderValue are an optional extra header, usually
	// "Authorization: Bearer ...". The value is a secret.
	HeaderName  string
	HeaderValue string
	Client      *http.Client
}

// Name identifies the target.
func (w *Webhook) Name() string { return "webhook" }

// Description shows nothing of the URL, which may contain a secret.
func (w *Webhook) Description() string { return "webhook" }

// Send posts the notification.
func (w *Webhook) Send(ctx context.Context, n Notification) error {
	body, err := json.Marshal(webhookPayloadFor(n))
	if err != nil {
		return fmt.Errorf("webhook: encode payload: %w", err)
	}
	r := request{
		target:      "webhook",
		url:         w.URL,
		contentType: "application/json",
		body:        body,
		secrets:     headerSecrets(w.HeaderValue),
	}
	if w.HeaderName != "" {
		r.headers = map[string]string{w.HeaderName: w.HeaderValue}
	}
	return post(ctx, w.Client, r)
}

// headerSecrets returns the header value and, for "Bearer <token>", the part
// after the scheme: a server may quote just the token.
func headerSecrets(value string) []string {
	secrets := []string{value}
	if _, credentials, found := strings.Cut(value, " "); found {
		secrets = append(secrets, strings.TrimSpace(credentials))
	}
	return secrets
}

type webhookPayload struct {
	Version   int            `json:"version"`
	Subject   string         `json:"subject"`
	BaseURL   string         `json:"base_url"`
	Counts    webhookCounts  `json:"counts"`
	New       []webhookAlert `json:"new"`
	Reminders []webhookAlert `json:"reminders"`
	Resolved  []webhookAlert `json:"resolved"`
}

type webhookCounts struct {
	New       int `json:"new"`
	Reminders int `json:"reminders"`
	Resolved  int `json:"resolved"`
}

type webhookAlert struct {
	Key        string        `json:"key"`
	Check      string        `json:"check"`
	Severity   string        `json:"severity"`
	Object     webhookObject `json:"object"`
	Reason     string        `json:"reason"`
	Message    string        `json:"message"`
	Since      time.Time     `json:"since"`
	ResolvedAt *time.Time    `json:"resolved_at"` // null unless resolved
	// Affected lists, for a node that is down, the problems it holds back.
	// Added in phase 15; empty for every other alert, never null.
	Affected []webhookAffected `json:"affected"`
}

type webhookAffected struct {
	Object webhookObject `json:"object"`
	Reason string        `json:"reason"`
}

type webhookObject struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"` // empty for cluster-scoped objects
	Name      string `json:"name"`
}

func webhookPayloadFor(n Notification) webhookPayload {
	return webhookPayload{
		Version: webhookVersion,
		Subject: Subject(n),
		BaseURL: n.BaseURL,
		Counts:  webhookCounts{New: len(n.New), Reminders: len(n.Reminders), Resolved: len(n.Resolved)},
		// Always lists, never null, so a receiver can loop without a check.
		New:       webhookAlerts(n.New),
		Reminders: webhookAlerts(n.Reminders),
		Resolved:  webhookAlerts(n.Resolved),
	}
}

func webhookAlerts(alerts []Alert) []webhookAlert {
	list := []webhookAlert{}
	for _, a := range alerts {
		entry := webhookAlert{
			Key:      a.Key,
			Check:    a.Check,
			Severity: string(a.Severity),
			Object:   webhookObject{Kind: a.Object.Kind, Namespace: a.Object.Namespace, Name: a.Object.Name},
			Reason:   a.Reason,
			Message:  a.Message,
			Since:    a.Since.UTC(),
			Affected: []webhookAffected{},
		}
		for _, p := range a.Affected {
			entry.Affected = append(entry.Affected, webhookAffected{
				Object: webhookObject{Kind: p.Object.Kind, Namespace: p.Object.Namespace, Name: p.Object.Name},
				Reason: p.Reason,
			})
		}
		if !a.ResolvedAt.IsZero() {
			resolvedAt := a.ResolvedAt.UTC()
			entry.ResolvedAt = &resolvedAt
		}
		list = append(list, entry)
	}
	return list
}
