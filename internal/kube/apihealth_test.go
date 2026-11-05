package kube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	gone := &apierrors.StatusError{ErrStatus: metav1.Status{Reason: metav1.StatusReasonGone}}
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

// fakeAPIServer answers like an API server with no objects. While failing is set
// it answers every request with a 500, and it ends the watches that are open, as
// an API server does that is in trouble but still takes connections.
type fakeAPIServer struct {
	*httptest.Server
	failing atomic.Bool
	failed  chan struct{} // closed to end the open watches
	once    sync.Once
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	api := &fakeAPIServer{failed: make(chan struct{})}
	api.Server = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(func() {
		api.fail()
		api.Close()
	})
	return api
}

func (a *fakeAPIServer) fail() {
	a.failing.Store(true)
	a.once.Do(func() { close(a.failed) })
}

func (a *fakeAPIServer) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case a.failing.Load():
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"InternalError","code":500}`))
	case r.URL.Query().Get("sendInitialEvents") == "true":
		// Not supported here: client-go falls back to a plain list and watch.
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Invalid","code":422}`))
	case r.URL.Query().Get("watch") == "true":
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-a.failed:
		case <-r.Context().Done():
		}
	case r.URL.Path == "/version":
		_, _ = w.Write([]byte(`{}`))
	default:
		_, _ = w.Write([]byte(`{"kind":"List","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[]}`))
	}
}

// An API server that answers with errors is a case the watch error handler does
// see, unlike "connection refused". When it answers again the errors of that time
// are over at once, although client-go does not say so.
func TestOutageWithErrorAnswersEndsWhenTheServerAnswersAgain(t *testing.T) {
	api := newFakeAPIServer(t)
	client, err := kubernetes.NewForConfig(&rest.Config{Host: api.URL})
	if err != nil {
		t.Fatalf("NewForConfig: %v", err)
	}
	cluster, err := New(client, nil, Options{APIProbe: APIProbe(client)}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var clock atomic.Int64
	clock.Store(healthStart.Unix())
	cluster.now = func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster.Start(ctx)
	if err := cluster.WaitForSync(ctx); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}
	if snap := cluster.BuildSnapshot(ctx, healthStart); snap.APIProblem != nil {
		t.Fatalf("APIProblem = %+v, want none before the outage", snap.APIProblem)
	}

	api.fail()
	deadline := time.Now().Add(30 * time.Second)
	for {
		cluster.health.mu.Lock()
		failing := len(cluster.health.watches)
		cluster.health.mu.Unlock()
		if failing == len(cluster.syncedInformers()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d watches noticed the outage", failing)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cluster.BuildSnapshot(ctx, healthStart.Add(30*time.Second))

	// The errors go on. Small steps, so it stays one series of errors, up to
	// shortly before the return.
	back := healthStart.Add(150 * time.Second)
	for _, step := range []time.Time{healthStart.Add(70 * time.Second), back.Add(-10 * time.Second)} {
		clock.Store(step.Unix())
		deadline = time.Now().Add(30 * time.Second)
		for {
			cluster.health.mu.Lock()
			failure := cluster.health.watches["pods"]
			cluster.health.mu.Unlock()
			if failure.last.Equal(step) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no new watch error")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Back after 2:30. The last errors are not 2 minutes old for a while yet,
	// but the outage is over and must not turn into a problem now.
	api.failing.Store(false)
	clock.Store(back.Unix())
	if snap := cluster.BuildSnapshot(ctx, back); snap.APIProblem != nil {
		t.Errorf("APIProblem = %+v right after the return", snap.APIProblem)
	}
	soon := back.Add(time.Minute)
	clock.Store(soon.Unix())
	if snap := cluster.BuildSnapshot(ctx, soon); snap.APIProblem != nil {
		t.Errorf("APIProblem = %+v a minute after the return", snap.APIProblem)
	}
}

func TestProbeRecoveryForgetsOnlyTheFailuresOfThatOutage(t *testing.T) {
	refused := errors.New("connection refused")
	broken := errors.New("500")
	forbidden := errors.New("forbidden")

	t.Run("first watch error comes before the first failed probe", func(t *testing.T) {
		h := newAPIHealth()
		h.probed(nil, healthStart)
		h.watchFailed("pods", broken, healthStart.Add(5*time.Second))
		h.probed(broken, healthStart.Add(30*time.Second))
		h.probed(nil, healthStart.Add(time.Minute))
		if len(h.watches) != 0 {
			t.Errorf("watches = %v, want none", h.watches)
		}
	})

	t.Run("one missed probe keeps a failure that was there before", func(t *testing.T) {
		h := newAPIHealth()
		h.probed(nil, healthStart)
		h.watchFailed("certificates", forbidden, healthStart.Add(time.Second))
		now := healthStart.Add(30 * time.Second)
		for ; now.Before(healthStart.Add(apiProblemAfter + time.Minute)); now = now.Add(30 * time.Second) {
			h.probed(nil, now)
			h.watchFailed("certificates", forbidden, now)
		}
		h.probed(refused, now)
		now = now.Add(30 * time.Second)
		h.probed(nil, now)

		got := h.problem(2, now)
		if got == nil || got.Blind || !slices.Equal(got.Resources, []string{"certificates"}) || !got.Since.Equal(healthStart.Add(time.Second)) {
			t.Errorf("problem = %+v, want certificates still reported, since the start", got)
		}
	})

	t.Run("a lasting failure that began in the outage comes back and starts over", func(t *testing.T) {
		h := newAPIHealth()
		h.probed(nil, healthStart)
		h.probed(refused, healthStart.Add(30*time.Second))
		h.watchFailed("pods", forbidden, healthStart.Add(40*time.Second))
		back := healthStart.Add(time.Minute)
		h.probed(nil, back)

		again := back.Add(45 * time.Second)
		h.watchFailed("pods", forbidden, again)
		now := again
		for now.Before(again.Add(apiProblemAfter)) {
			now = now.Add(time.Minute)
			h.watchFailed("pods", forbidden, now)
		}
		got := h.problem(2, now)
		if got == nil || !got.Since.Equal(again) {
			t.Errorf("problem = %+v, want it reported, since %v", got, again)
		}
	})

	t.Run("without a failed probe the watches keep their 2 minutes", func(t *testing.T) {
		h := newAPIHealth()
		h.probed(nil, healthStart)
		h.watchFailed("pods", broken, healthStart.Add(time.Second))
		h.probed(nil, healthStart.Add(30*time.Second))
		if len(h.watches) != 1 {
			t.Errorf("watches = %v, want pods still failing", h.watches)
		}
	})
}
