package kube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

var healthStart = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func TestShortWatchFailureIsNoProblem(t *testing.T) {
	h := newAPIHealth()
	h.watchFailed("pods", errors.New("broken"), healthStart)
	if got := h.problem(2, healthStart.Add(apiProblemAfter-time.Second)); got != nil {
		t.Errorf("problem = %+v, want none yet", got)
	}
}

func TestLastingWatchFailureIsAProblem(t *testing.T) {
	h := newAPIHealth()
	var now time.Time
	for minute := 0; minute <= 3; minute++ {
		now = healthStart.Add(time.Duration(minute) * time.Minute)
		h.watchFailed("pods", errors.New("forbidden"), now)
	}
	got := h.problem(2, now)
	if got == nil || got.Blind || !slices.Equal(got.Resources, []string{"pods"}) ||
		got.Error != "forbidden" || !got.Since.Equal(healthStart) {
		t.Errorf("problem = %+v, want pods failing since the start, others fine", got)
	}
}

func TestWatchCountsAsBackAfterSomeTimeWithoutError(t *testing.T) {
	h := newAPIHealth()
	h.watchFailed("pods", errors.New("broken"), healthStart)
	back := healthStart.Add(watchFailureEnds)
	if got := h.problem(2, back.Add(time.Hour)); got != nil {
		t.Errorf("problem = %+v, want none", got)
	}

	// A later error starts a new series.
	h.watchFailed("pods", errors.New("broken"), back.Add(2*time.Hour))
	if got := h.problem(2, back.Add(2*time.Hour)); got != nil {
		t.Errorf("problem = %+v, want none so soon after the new start", got)
	}
}

func TestNormalEndOfAWatchIsNoError(t *testing.T) {
	expired := apierrors.NewResourceExpired("too old")
	gone := apierrors.NewGone("gone")
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, expired, gone} {
		h := newAPIHealth()
		h.watchFailed("pods", err, healthStart)
		if len(h.watches) != 0 {
			t.Errorf("%v counted as a failure", err)
		}
	}
}

func TestAllWatchesFailingMeansBlindOnceTheLastHasLasted(t *testing.T) {
	h := newAPIHealth()
	h.watchFailed("certificates", errors.New("not found"), healthStart)
	later := healthStart.Add(time.Hour)
	for offset := time.Duration(0); offset <= apiProblemAfter; offset += time.Minute {
		h.watchFailed("certificates", errors.New("not found"), later.Add(offset))
		h.watchFailed("pods", errors.New("refused"), later.Add(offset))
	}
	if got := h.problem(3, later.Add(apiProblemAfter)); got == nil || got.Blind {
		t.Errorf("problem with two of three = %+v, want not blind", got)
	}

	got := h.problem(2, later.Add(apiProblemAfter))
	if got == nil || !got.Blind {
		t.Errorf("problem with all = %+v, want blind", got)
	}
}

func TestAPIServerThatDoesNotAnswerIsBlindAndEndsAtOnce(t *testing.T) {
	h := newAPIHealth()
	refused := errors.New("connection refused")
	h.probed(refused, healthStart)
	h.probed(refused, healthStart.Add(apiProblemAfter-time.Second))
	if got := h.problem(2, healthStart.Add(apiProblemAfter-time.Second)); got != nil {
		t.Errorf("problem = %+v, want none yet", got)
	}

	h.probed(refused, healthStart.Add(apiProblemAfter))
	got := h.problem(2, healthStart.Add(apiProblemAfter))
	if got == nil || !got.Blind || got.Error != "connection refused" || !got.Since.Equal(healthStart) {
		t.Fatalf("problem = %+v, want blind since the start", got)
	}

	h.probed(nil, healthStart.Add(apiProblemAfter+time.Second))
	if got := h.problem(2, healthStart.Add(apiProblemAfter+time.Second)); got != nil {
		t.Errorf("problem = %+v, want none once it answers", got)
	}
}

func TestAPIProbeAgainstAServer(t *testing.T) {
	answer := func(code int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{}`))
		}))
	}
	stopped := answer(http.StatusOK)
	stopped.Close()

	tests := []struct {
		name     string
		server   *httptest.Server
		wantFail bool
	}{
		{"it answers", answer(http.StatusOK), false},
		{"not allowed, but it answers", answer(http.StatusForbidden), false},
		{"server error", answer(http.StatusInternalServerError), true},
		{"connection refused", stopped, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer tt.server.Close()
			client, err := kubernetes.NewForConfig(&rest.Config{Host: tt.server.URL})
			if err != nil {
				t.Fatalf("NewForConfig: %v", err)
			}
			err = APIProbe(client)(context.Background())
			if (err != nil) != tt.wantFail {
				t.Errorf("err = %v, want failure = %v", err, tt.wantFail)
			}
		})
	}
}

// From a failing watch in the informer to the snapshot.
func TestFailingWatchOfAnInformerReachesTheSnapshot(t *testing.T) {
	client := fake.NewClientset()
	client.PrependWatchReactor("pods", func(clienttesting.Action) (bool, watch.Interface, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("RBAC: access denied"))
	})
	client.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("RBAC: access denied"))
	})
	cluster, err := New(client, nil, Options{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var clock atomic.Int64
	clock.Store(healthStart.Unix())
	cluster.now = func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster.Start(ctx)

	waitForErrorAt := func(at time.Time) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			cluster.health.mu.Lock()
			failure, found := cluster.health.watches["pods"]
			cluster.health.mu.Unlock()
			if found && failure.last.Equal(at) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the failing watch was not noticed")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	waitForErrorAt(healthStart)
	if snap := cluster.BuildSnapshot(ctx, healthStart); snap.APIProblem != nil {
		t.Errorf("APIProblem = %+v, want none so soon", snap.APIProblem)
	}

	// One step per minute: the errors go on, so it stays one series of errors.
	later := healthStart
	for later.Before(healthStart.Add(apiProblemAfter)) {
		later = later.Add(time.Minute)
		clock.Store(later.Unix())
		waitForErrorAt(later)
	}
	snap := cluster.BuildSnapshot(ctx, later)
	if snap.APIProblem == nil || snap.APIProblem.Blind || !slices.Contains(snap.APIProblem.Resources, "pods") {
		t.Errorf("APIProblem = %+v, want pods, and the other resources still fine", snap.APIProblem)
	}
}
