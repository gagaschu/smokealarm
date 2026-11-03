package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
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

	namespaces     cache.SharedIndexInformer
	pods           cache.SharedIndexInformer
	events         cache.SharedIndexInformer
	deployments    cache.SharedIndexInformer
	statefulSets   cache.SharedIndexInformer
	daemonSets     cache.SharedIndexInformer
	nodes          cache.SharedIndexInformer
	jobs           cache.SharedIndexInformer
	cronJobs       cache.SharedIndexInformer
	pvcs           cache.SharedIndexInformer
	storageClasses cache.SharedIndexInformer
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
	c.deployments = c.coreFactory.Apps().V1().Deployments().Informer()
	c.statefulSets = c.coreFactory.Apps().V1().StatefulSets().Informer()
	c.daemonSets = c.coreFactory.Apps().V1().DaemonSets().Informer()
	c.nodes = c.coreFactory.Core().V1().Nodes().Informer()
	c.jobs = c.coreFactory.Batch().V1().Jobs().Informer()
	c.cronJobs = c.coreFactory.Batch().V1().CronJobs().Informer()
	c.pvcs = c.coreFactory.Core().V1().PersistentVolumeClaims().Informer()
	c.storageClasses = c.coreFactory.Storage().V1().StorageClasses().Informer()

	// Transforms must be set before the informers start.
	for _, informer := range c.allInformers() {
		if err := informer.SetTransform(trimObject); err != nil {
			return nil, fmt.Errorf("set informer transform: %w", err)
		}
	}
	return c, nil
}

// allInformers lists every informer, for the steps that treat them alike.
func (c *Cluster) allInformers() []cache.SharedIndexInformer {
	return []cache.SharedIndexInformer{
		c.namespaces, c.pods, c.events,
		c.deployments, c.statefulSets, c.daemonSets,
		c.nodes, c.jobs, c.cronJobs, c.pvcs, c.storageClasses,
	}
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
	var synced []cache.InformerSynced
	for _, informer := range c.allInformers() {
		synced = append(synced, informer.HasSynced)
	}
	if !cache.WaitForCacheSync(ctx.Done(), synced...) {
		return errors.New("informer caches did not sync before shutdown")
	}
	c.logger.Info("informer caches synced")
	return nil
}

// Synced reports whether the caches are filled. It backs /readyz.
func (c *Cluster) Synced() bool {
	for _, informer := range c.allInformers() {
		if !informer.HasSynced() {
			return false
		}
	}
	return true
}

// Ready is the readiness probe for the web server.
func (c *Cluster) Ready() error {
	if !c.Synced() {
		return errors.New("informer caches are not synced yet")
	}
	return nil
}
