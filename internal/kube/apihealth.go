package kube

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// Informer caches stay "synced" forever after their first fill, so nothing else
// tells when they stop being updated. Two sources tell it here, because neither
// sees everything:
//
//   - The watch error handler of every informer sees problems with one resource
//     (a missing permission, a deleted CRD). But client-go retries "connection
//     refused" on its own without calling the handler, and that is what a
//     stopped API server looks like.
//   - A small request in every check run sees that the API server does not
//     answer. It cannot see the problems with one resource.

const (
	// apiProblemAfter is how long a failure must last before it counts: a
	// restart of the API server is normal and the old data does no harm for a
	// moment. With ALERT_AFTER on top, the alarm comes after about 5 minutes.
	apiProblemAfter = 3 * time.Minute
	// watchFailureEnds is how long after its last error a watch counts as working
	// again, because client-go does not report that it recovered. It is longer
	// than client-go's longest wait between two attempts (about 40 seconds).
	watchFailureEnds = 2 * time.Minute
	// probeTimeout limits how long the API server may take to answer.
	probeTimeout = 5 * time.Second
)

// APIProbe returns the small request that says whether the API server answers.
// Every authenticated client may read /version. An answer with an error like 403
// still means that the server is there; what is wrong with a resource is for the
// watch errors to tell.
func APIProbe(client kubernetes.Interface) func(context.Context) error {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		_, err := client.CoreV1().RESTClient().Get().AbsPath("/version").DoRaw(ctx)
		var status apierrors.APIStatus
		if errors.As(err, &status) && status.Status().Code < 500 {
			return nil
		}
		return err
	}
}

// watchFailure is a series of errors of one watch.
type watchFailure struct {
	since   time.Time
	last    time.Time
	message string
}

// apiHealth remembers what failed. The watch errors arrive on the goroutines of
// the informers and BuildSnapshot reads them in the main loop, hence the mutex.
type apiHealth struct {
	mu           sync.Mutex
	watches      map[string]watchFailure // by resource
	probeSince   time.Time               // zero while the API server answers
	probeMessage string
}

func newAPIHealth() *apiHealth {
	return &apiHealth{watches: map[string]watchFailure{}}
}

// watchFailed records an error of the watch of a resource. A watch that merely
// ended is not an error: the API server closes them regularly.
func (h *apiHealth) watchFailed(resource string, err error, now time.Time) {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	failure, found := h.watches[resource]
	if !found || now.Sub(failure.last) >= watchFailureEnds {
		failure.since = now
	}
	failure.last = now
	failure.message = err.Error()
	h.watches[resource] = failure
}

// probed records the result of the request to the API server. Unlike a watch it
// says at once when the server is back.
func (h *apiHealth) probed(err error, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil {
		h.probeSince, h.probeMessage = time.Time{}, ""
		return
	}
	if h.probeSince.IsZero() {
		h.probeSince = now
	}
	h.probeMessage = err.Error()
}

// problem says what is wrong now, if it has lasted apiProblemAfter. total is the
// number of informers.
func (h *apiHealth) problem(total int, now time.Time) *checks.APIProblem {
	h.mu.Lock()
	defer h.mu.Unlock()
	resources := h.failingResources(now)
	if len(resources) == 0 && h.probeSince.IsZero() {
		return nil
	}

	since, lastToFail := h.probeSince, time.Time{}
	for _, resource := range resources {
		start := h.watches[resource].since
		if since.IsZero() || start.Before(since) {
			since = start
		}
		if start.After(lastToFail) {
			lastToFail = start
		}
	}
	if now.Sub(since) < apiProblemAfter {
		return nil
	}

	probeBlind := !h.probeSince.IsZero() && now.Sub(h.probeSince) >= apiProblemAfter
	watchesBlind := len(resources) == total && now.Sub(lastToFail) >= apiProblemAfter
	problem := &checks.APIProblem{Since: since, Resources: resources, Blind: probeBlind || watchesBlind}
	problem.Error = h.probeMessage
	if problem.Error == "" {
		problem.Error = h.watches[resources[0]].message
	}
	return problem
}

// failingResources forgets the watches that have stopped failing and returns the
// others, sorted. The caller holds the lock.
func (h *apiHealth) failingResources(now time.Time) []string {
	var resources []string
	for resource, failure := range h.watches {
		if now.Sub(failure.last) >= watchFailureEnds {
			delete(h.watches, resource)
			continue
		}
		resources = append(resources, resource)
	}
	sort.Strings(resources)
	return resources
}

// reportWatchErrors makes an informer tell about its errors. The default handler
// is kept, because it writes them to the log. It must be set before the informer
// starts.
func (c *Cluster) reportWatchErrors(n namedInformer) error {
	return n.informer.SetWatchErrorHandlerWithContext(func(ctx context.Context, r *cache.Reflector, err error) {
		cache.DefaultWatchErrorHandler(ctx, r, err)
		c.health.watchFailed(n.name, err, c.now())
	})
}

// addAPIProblem asks the API server whether it answers and puts what is known
// about the connection into the snapshot.
func (c *Cluster) addAPIProblem(ctx context.Context, snap *checks.Snapshot) {
	if c.probe != nil {
		c.health.probed(c.probe(ctx), snap.Now)
	}
	snap.APIProblem = c.health.problem(len(c.syncedInformers()), snap.Now)
}
