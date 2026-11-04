package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/store"
)

func downNode(name string) store.Record {
	r := problem("node-"+name, "", checks.Critical, store.StateFiring, testNow.Add(-10*time.Minute), "node is not ready")
	r.Check = checks.CheckNodeNotReady
	r.Object = checks.ObjectRef{Kind: checks.KindNode, Name: name}
	r.Reason = "NotReady"
	return r
}

func heldBack(key, node string) store.Record {
	r := problem(key, "media", checks.Critical, store.StatePending, testNow.Add(-5*time.Minute), "held "+key)
	r.CausedBy = node
	return r
}

func TestOverviewShowsTheNodeFirstAndWhatItHoldsBack(t *testing.T) {
	e := newTestEnv(t)
	e.status.LastRun = testNow
	e.insert(
		problem("crit-old", "media", checks.Critical, store.StateFiring, testNow.Add(-5*time.Hour), "c"),
		downNode("pi-2"),
		heldBack("sonarr", "pi-2"),
		heldBack("radarr", "pi-2"),
		problem("pending-one", "media", checks.Critical, store.StatePending, testNow, "not yet"),
	)

	body := e.do(http.MethodGet, "/", nil, e.login()).Body.String()
	node, other, section := strings.Index(body, "Node pi-2"), strings.Index(body, "crit-old"), strings.Index(body, "Held back: node pi-2 is down")
	if node < 0 || other < 0 || section < 0 || node > other || other > section {
		t.Fatalf("want the node first, then the other problems, then the held-back section:\n%s", body)
	}
	for _, want := range []string{"2 current problems", "Holds back 2 problems while the node is down", "Pod media/sonarr", "Pod media/radarr"} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not contain %q", want)
		}
	}
	if strings.Contains(body, "pending-one") {
		t.Error("an ordinary pending problem must not be listed")
	}
}

func TestOverviewShowsHeldBackProblemsOfAnIgnoredNode(t *testing.T) {
	e := newTestEnv(t)
	e.status.LastRun = testNow
	e.insert(heldBack("sonarr", "pi-test")) // the node itself is ignored: no NotReady

	body := e.do(http.MethodGet, "/", nil, e.login()).Body.String()
	if strings.Contains(body, "All good") {
		t.Errorf("held-back problems are not \"All good\":\n%s", body)
	}
	for _, want := range []string{"Held back: node pi-test is down", "Pod media/sonarr", "0 current problems"} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not contain %q:\n%s", want, body)
		}
	}
}
