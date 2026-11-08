package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gagaschu/smokealarm/internal/engine"
	"github.com/gagaschu/smokealarm/internal/store"
)

const telegramTarget = "telegram:-100123"

// muteEnv is a test environment with two notification targets.
func muteEnv(t *testing.T) (*testEnv, *http.Cookie) {
	e := newTestEnv(t)
	e.status = engine.Status{
		LastRun: testNow,
		Targets: []engine.TargetStatus{
			{Name: "mail", Description: "mail (smtp.example.com)"},
			{Name: telegramTarget, Description: "telegram (chat -100123)"},
		},
	}
	return e, e.login()
}

func (e *testEnv) mutes() map[string]store.TargetMute {
	e.t.Helper()
	mutes, err := e.store.TargetMutes(context.Background())
	if err != nil {
		e.t.Fatalf("TargetMutes: %v", err)
	}
	return mutes
}

func TestMuteAndUnmuteATarget(t *testing.T) {
	e, cookie := muteEnv(t)

	rec := e.do(http.MethodPost, "/targets/mail/mute", nil, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/status" {
		t.Fatalf("mute: status %d, location %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok := e.mutes()["mail"]; !ok {
		t.Fatal("mail is not muted")
	}

	rec = e.do(http.MethodPost, "/targets/mail/unmute", nil, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/status" {
		t.Fatalf("unmute: status %d, location %q", rec.Code, rec.Header().Get("Location"))
	}
	if e.mutes()["mail"].UnmuteRequestedAt == nil {
		t.Error("the switch-on request was not recorded")
	}
}

func TestMuteRouteFindsTelegramChats(t *testing.T) {
	e, cookie := muteEnv(t)

	rec := e.do(http.MethodPost, "/targets/"+url.PathEscape(telegramTarget)+"/mute", nil, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if _, ok := e.mutes()[telegramTarget]; !ok {
		t.Errorf("no row for %s: %v", telegramTarget, e.mutes())
	}
}

func TestMuteUnknownTargetIs404(t *testing.T) {
	e, cookie := muteEnv(t)
	e.status.Targets = append(e.status.Targets, engine.TargetStatus{Name: "log", Description: "log"})

	for _, path := range []string{"/targets/nope/mute", "/targets/log/mute", "/targets/nope/unmute"} {
		if rec := e.do(http.MethodPost, path, nil, cookie); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
	if len(e.mutes()) != 0 {
		t.Errorf("rows were written: %v", e.mutes())
	}
}

func TestMuteNeedsSessionAndSameOrigin(t *testing.T) {
	e, cookie := muteEnv(t)

	rec := e.do(http.MethodPost, "/targets/mail/mute", nil, nil)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("without session: status %d, location %q", rec.Code, rec.Header().Get("Location"))
	}

	req := httptest.NewRequest(http.MethodPost, "/targets/mail/mute", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site: status = %d, want 403", rec.Code)
	}
	if len(e.mutes()) != 0 {
		t.Errorf("rows were written: %v", e.mutes())
	}
}

func TestMuteAgainWhileSwitchingOn(t *testing.T) {
	e, cookie := muteEnv(t)
	e.do(http.MethodPost, "/targets/mail/mute", nil, cookie)
	e.do(http.MethodPost, "/targets/mail/unmute", nil, cookie)
	e.do(http.MethodPost, "/targets/mail/mute", nil, cookie)

	if e.mutes()["mail"].UnmuteRequestedAt != nil {
		t.Error("muting again did not cancel the request")
	}
	body := e.do(http.MethodGet, "/status", nil, cookie).Body.String()
	if strings.Contains(body, "being switched on") {
		t.Error("status page still says it is being switched on")
	}
	if !strings.Contains(body, `<span class="pill">off</span>`) {
		t.Error("status page does not say the target is off")
	}
}

func TestStatusPageShowsMutedTargets(t *testing.T) {
	e, cookie := muteEnv(t)
	e.do(http.MethodPost, "/targets/mail/mute", nil, cookie)

	body := e.do(http.MethodGet, "/status", nil, cookie).Body.String()
	for _, want := range []string{
		`<span class="pill">off</span>`,
		"since 2026-", // the time in the zone, see timeFormat
		"Switch on",
		`action="/targets/mail/unmute"`,
		"Switch off",
		`action="/targets/telegram:-100123/mute"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("status page lacks %q", want)
		}
	}
	if strings.Count(body, "Switch on") != 1 {
		t.Error("only the muted target offers Switch on")
	}

	e.do(http.MethodPost, "/targets/mail/unmute", nil, cookie)
	body = e.do(http.MethodGet, "/status", nil, cookie).Body.String()
	if !strings.Contains(body, "being switched on") {
		t.Error("status page does not say the target is being switched on")
	}
}

func TestOverviewNamesMutedTargets(t *testing.T) {
	e, cookie := muteEnv(t)
	body := e.do(http.MethodGet, "/", nil, cookie).Body.String()
	if strings.Contains(body, "Notifications are off") {
		t.Error("the note is shown although every target is on")
	}

	e.do(http.MethodPost, "/targets/mail/mute", nil, cookie)
	body = e.do(http.MethodGet, "/", nil, cookie).Body.String()
	for _, want := range []string{"Notifications are off", "mail (smtp.example.com)", `href="/status"`} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	if strings.Contains(body, "telegram (chat") {
		t.Error("overview names a target that is on")
	}
}

func TestOverviewAllGoodWithEveryTargetOff(t *testing.T) {
	e, cookie := muteEnv(t)
	e.do(http.MethodPost, "/targets/mail/mute", nil, cookie)
	e.do(http.MethodPost, "/targets/"+url.PathEscape(telegramTarget)+"/mute", nil, cookie)

	body := e.do(http.MethodGet, "/", nil, cookie).Body.String()
	for _, want := range []string{"All good", "mail (smtp.example.com)", "telegram (chat -100123)"} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
}
