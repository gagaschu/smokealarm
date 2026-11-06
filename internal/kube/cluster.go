package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// resyncPeriod is zero: we only read the caches, so periodic resyncs of
// unchanged objects would just waste CPU.
const resyncPeriod = 0

// syncTimeout is how long the caches get to fill at startup. A cache that does
// not fill in this time is almost always one the service account may not read:
// the informer then retries forever and logs "forbidden". Failing with a clear
// message is better than a pod that never becomes ready.
const syncTimeout = 2 * time.Minute

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

	// Only set if the CRDs exist (see Features).
	dynamicFactory dynamicinformer.DynamicSharedInformerFactory
	certificates   cache.SharedIndexInformer
	applications   cache.SharedIndexInformer

	// fetchStats reads the kubelet stats of one node; nil means the volume
	// usage is not collected (pvc-usage is off). Tests replace it.
	fetchStats statsFetcher

	// syncTimeout is a field so tests can make it short.
	syncTimeout time.Duration

	// health remembers whether the caches are still being updated. probe asks
	// the API server; without it only the watch errors are looked at.
	health *apiHealth
	probe  func(context.Context) error
	now    func() time.Time
}

// namedInformer pairs an informer with the name of its resource, for messages.
type namedInformer struct {
	name     string
	informer cache.SharedIndexInformer
}

// Options tells the cluster which optional parts to use.
type Options struct {
	Features Features
	// KubeletClient calls the kubelets for the volume usage, see
	// NewKubeletClient (needs the RBAC permission get on nodes/stats). Leave it
	// nil if pvc-usage is disabled: then no kubelet is asked.
	KubeletClient *http.Client
	// APIProbe asks whether the API server answers, see APIProbe. Without it a
	// stopped API server is not noticed, so only tests leave it out.
	APIProbe func(context.Context) error
}

// New prepares the informers but does not start them; call Start for that.
// The clients are passed in so tests can use fakes. The dynamic client may be
// nil if no optional feature is used.
func New(client kubernetes.Interface, dynamicClient dynamic.Interface, options Options, logger *slog.Logger) (*Cluster, error) {
	c := &Cluster{
		logger:      logger,
		syncTimeout: syncTimeout,
		health:      newAPIHealth(),
		probe:       options.APIProbe,
		now:         time.Now,
		coreFactory: informers.NewSharedInformerFactory(client, resyncPeriod),
		eventFactory: informers.NewSharedInformerFactoryWithOptions(client, resyncPeriod,
			informers.WithTweakListOptions(onlyWarnings)),
	}
	if options.KubeletClient != nil {
		c.fetchStats = kubeletStatsFetcher(options.KubeletClient)
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
	for _, n := range c.allInformers() {
		if err := n.informer.SetTransform(trimObject); err != nil {
			return nil, fmt.Errorf("set informer transform: %w", err)
		}
	}

	if err := c.addCustomResources(dynamicClient, options.Features); err != nil {
		return nil, err
	}
	for _, n := range c.syncedInformers() {
		if err := c.reportWatchErrors(n); err != nil {
			return nil, fmt.Errorf("set watch error handler for %s: %w", n.name, err)
		}
	}
	return c, nil
}

// addCustomResources creates informers for the optional CRDs that exist.
func (c *Cluster) addCustomResources(dynamicClient dynamic.Interface, features Features) error {
	if !features.CertManager && !features.ArgoCD {
		return nil
	}
	if dynamicClient == nil {
		return errors.New("a dynamic client is needed for cert-manager or Argo CD")
	}
	c.dynamicFactory = dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, resyncPeriod)

	if features.CertManager {
		c.certificates = c.dynamicFactory.ForResource(certificateGVR()).Informer()
		if err := c.certificates.SetTransform(trimCertificate); err != nil {
			return fmt.Errorf("set certificate transform: %w", err)
		}
	}
	if features.ArgoCD {
		c.applications = c.dynamicFactory.ForResource(applicationGVR()).Informer()
		if err := c.applications.SetTransform(trimApplication); err != nil {
			return fmt.Errorf("set application transform: %w", err)
		}
	}
	return nil
}

// allInformers lists the typed informers, which share one transform.
func (c *Cluster) allInformers() []namedInformer {
	return []namedInformer{
		{"namespaces", c.namespaces}, {"pods", c.pods}, {"events", c.events},
		{"deployments", c.deployments}, {"statefulsets", c.statefulSets}, {"daemonsets", c.daemonSets},
		{"nodes", c.nodes}, {"jobs", c.jobs}, {"cronjobs", c.cronJobs},
		{"persistentvolumeclaims", c.pvcs}, {"storageclasses", c.storageClasses},
	}
}

// syncedInformers lists every informer that must be synced, including the
// optional custom resources that are in use.
func (c *Cluster) syncedInformers() []namedInformer {
	informers := c.allInformers()
	if c.certificates != nil {
		informers = append(informers, namedInformer{"certificates", c.certificates})
	}
	if c.applications != nil {
		informers = append(informers, namedInformer{"applications", c.applications})
	}
	return informers
}

// unsyncedNames lists the resources whose caches are not filled yet.
func (c *Cluster) unsyncedNames() []string {
	var names []string
	for _, n := range c.syncedInformers() {
		if !n.informer.HasSynced() {
			names = append(names, n.name)
		}
	}
	return names
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
	if c.dynamicFactory != nil {
		c.dynamicFactory.Start(ctx.Done())
	}
}

// WaitForSync blocks until all caches hold the initial list of objects.
// Running checks before that would report objects as missing. It gives up with
// an error that names the missing caches after the sync timeout, or when ctx
// ends first (shutdown).
func (c *Cluster) WaitForSync(ctx context.Context) error {
	syncCtx, cancel := context.WithTimeout(ctx, c.syncTimeout)
	defer cancel()

	var synced []cache.InformerSynced
	for _, n := range c.syncedInformers() {
		synced = append(synced, n.informer.HasSynced)
	}
	if cache.WaitForCacheSync(syncCtx.Done(), synced...) {
		c.logger.Info("informer caches synced")
		return nil
	}
	if ctx.Err() != nil {
		return errors.New("informer caches did not sync before shutdown")
	}
	return fmt.Errorf("informer caches did not sync within %s: %s; usually the service account may not list them (look for \"forbidden\" in the log)",
		c.syncTimeout, strings.Join(c.unsyncedNames(), ", "))
}

// Synced reports whether the caches are filled. It backs /readyz.
func (c *Cluster) Synced() bool {
	return len(c.unsyncedNames()) == 0
}

// Ready is the readiness probe for the web server.
func (c *Cluster) Ready() error {
	if !c.Synced() {
		return errors.New("informer caches are not synced yet")
	}
	return nil
}
