package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// resyncPeriod is zero: we only read the caches, so periodic resyncs of
// unchanged objects would just waste CPU.
const resyncPeriod = 0

// Cluster owns the informers. Informers keep a local copy of the objects
// up to date through a watch, so each check run reads memory instead of
// calling the API server.
type Cluster struct {
	logger *slog.Logger

	// Two factories because only the event factory needs a field selector.
	coreFactory  informers.SharedInformerFactory
	eventFactory informers.SharedInformerFactory

	namespaces cache.SharedIndexInformer
	pods       cache.SharedIndexInformer
	events     cache.SharedIndexInformer
}

// New prepares the informers but does not start them; call Start for that.
// The client is passed in so tests can use a fake clientset.
func New(client kubernetes.Interface, logger *slog.Logger) (*Cluster, error) {
	c := &Cluster{
		logger:      logger,
		coreFactory: informers.NewSharedInformerFactory(client, resyncPeriod),
		eventFactory: informers.NewSharedInformerFactoryWithOptions(client, resyncPeriod,
			informers.WithTweakListOptions(onlyWarnings)),
	}
	c.namespaces = c.coreFactory.Core().V1().Namespaces().Informer()
	c.pods = c.coreFactory.Core().V1().Pods().Informer()
	c.events = c.eventFactory.Core().V1().Events().Informer()

	// Transforms must be set before the informers start.
	for _, informer := range []cache.SharedIndexInformer{c.namespaces, c.pods, c.events} {
		if err := informer.SetTransform(trimObject); err != nil {
			return nil, fmt.Errorf("set informer transform: %w", err)
		}
	}
	return c, nil
}

// onlyWarnings keeps Normal events out of the cache; they are the vast
// majority and we only use warnings as context for findings.
func onlyWarnings(opts *metav1.ListOptions) {
	opts.FieldSelector = "type=" + corev1.EventTypeWarning
}

// Start begins watching. It returns immediately; use WaitForSync to block
// until the caches are filled.
func (c *Cluster) Start(ctx context.Context) {
	c.coreFactory.Start(ctx.Done())
	c.eventFactory.Start(ctx.Done())
}

// WaitForSync blocks until all caches hold the initial list of objects.
// Running checks before that would report objects as missing.
func (c *Cluster) WaitForSync(ctx context.Context) error {
	if !cache.WaitForCacheSync(ctx.Done(), c.namespaces.HasSynced, c.pods.HasSynced, c.events.HasSynced) {
		return errors.New("informer caches did not sync before shutdown")
	}
	c.logger.Info("informer caches synced")
	return nil
}

// Synced reports whether the caches are filled. It backs /readyz.
func (c *Cluster) Synced() bool {
	return c.namespaces.HasSynced() && c.pods.HasSynced() && c.events.HasSynced()
}

// Ready is the readiness probe for the web server.
func (c *Cluster) Ready() error {
	if !c.Synced() {
		return errors.New("informer caches are not synced yet")
	}
	return nil
}

// BuildSnapshot copies the current cache contents into a snapshot. The
// objects are shared with the cache, so checks must treat them as read-only.
func (c *Cluster) BuildSnapshot(now time.Time) *checks.Snapshot {
	snap := &checks.Snapshot{Now: now}
	for _, obj := range c.namespaces.GetStore().List() {
		if ns, ok := obj.(*corev1.Namespace); ok {
			snap.Namespaces = append(snap.Namespaces, ns)
		}
	}
	for _, obj := range c.pods.GetStore().List() {
		if pod, ok := obj.(*corev1.Pod); ok {
			snap.Pods = append(snap.Pods, pod)
		}
	}
	for _, obj := range c.events.GetStore().List() {
		if event, ok := obj.(*corev1.Event); ok {
			snap.Events = append(snap.Events, event)
		}
	}
	return snap
}
