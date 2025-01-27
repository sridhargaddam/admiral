package controller

import (
	"flag"
	"fmt"
	"sync"

	"github.com/submariner-io/admiral/pkg/federate"
	submarinerClientset "github.com/submariner-io/submariner/pkg/client/clientset/versioned"
	submarinerInformers "github.com/submariner-io/submariner/pkg/client/informers/externalversions"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

type clusterWatcherMap map[string]*clusterWatcher

type clusterWatcher struct {
	clusterID         string
	stopChan          chan struct{}
	clusterWorkQueue  workqueue.RateLimitingInterface
	endpointWorkQueue workqueue.RateLimitingInterface
}

type controller struct {
	sync.Mutex
	federator       federate.Federator
	clusterWatchers clusterWatcherMap

	// Indirection hook for unit tests to supply fake client sets
	newSubmClientset func(kubeConfig *rest.Config) (submarinerClientset.Interface, error)
}

// clusterIDLabelKey is the key for a label that is added to the federated submariner Cluster and Endpoint resources to
// hold the ID of the cluster from which the resource originated, allowing us to only process watch notifications
// emanating from the originating cluster.
const clusterIDLabelKey = "submariner-io/clusterID"

var (
	submarinerNamespace string
)

func init() {
	flag.StringVar(&submarinerNamespace, "submariner-namespace", "submariner", "The namespace in which the submariner components are deployed.")
}

func New(federator federate.Federator) *controller {
	return &controller{
		federator:       federator,
		clusterWatchers: make(clusterWatcherMap),

		newSubmClientset: func(c *rest.Config) (submarinerClientset.Interface, error) {
			return submarinerClientset.NewForConfig(c)
		},
	}
}

func (c *controller) Start() error {
	err := c.federator.WatchClusters(c)
	if err != nil {
		return fmt.Errorf("Could not register cluster watch: %v", err)
	}

	return nil
}

func (c *controller) Stop() {
	c.Lock()
	defer c.Unlock()

	for _, clusterWatcher := range c.clusterWatchers {
		clusterWatcher.close()
	}
}

func (c *controller) removeExistingClusterWatcher(clusterID string) {
	watcher := c.clusterWatchers[clusterID]
	if watcher != nil {
		klog.V(2).Infof("A watcher for cluster %s already exists - stopping watch", clusterID)
		watcher.close()
		delete(c.clusterWatchers, clusterID)
	}
}

func (c *controller) startNewWatcherForCluster(kubeConfig *rest.Config, clusterID string) {
	submarinerClient, err := c.newSubmClientset(kubeConfig)
	if err != nil {
		klog.Errorf("error building submariner clientset for cluster %s: %v", clusterID, err)
		return
	}

	c.Lock()
	defer c.Unlock()

	c.removeExistingClusterWatcher(clusterID)

	watcher := &clusterWatcher{
		clusterID: clusterID,
		stopChan:  make(chan struct{}),
		clusterWorkQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(),
			fmt.Sprintf("Cluster watcher for %s", clusterID)),
		endpointWorkQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(),
			fmt.Sprintf("Endpoint watcher for %s", clusterID)),
	}
	c.clusterWatchers[clusterID] = watcher

	submarinerInformerFactory := submarinerInformers.NewSharedInformerFactoryWithOptions(submarinerClient, 0,
		submarinerInformers.WithNamespace(submarinerNamespace))

	submV1 := submarinerInformerFactory.Submariner().V1()
	clusterInformer := submV1.Clusters().Informer()
	clusterInformer.AddEventHandler(newResourceEventHandler(clusterID, watcher.clusterWorkQueue))

	endpointInformer := submV1.Endpoints().Informer()
	endpointInformer.AddEventHandler(newResourceEventHandler(clusterID, watcher.endpointWorkQueue))

	submarinerInformerFactory.Start(watcher.stopChan)

	go runQueueWorker(clusterID, c.federator, clusterInformer, watcher.clusterWorkQueue)
	go runQueueWorker(clusterID, c.federator, endpointInformer, watcher.endpointWorkQueue)
}

func getLabel(obj interface{}, key string) (string, bool) {
	if deleted, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = deleted.Obj
	}

	metadata, err := meta.Accessor(obj)
	if err != nil {
		return "", false
	}

	labels := metadata.GetLabels()
	if labels == nil {
		return "", false
	}

	label, exists := labels[key]
	return label, exists
}

func setLabel(obj runtime.Object, key, value string) runtime.Object {
	copy := obj.DeepCopyObject()

	// We can safely ignore the error as all the resources implement meta.Object
	metadata, _ := meta.Accessor(copy)

	labels := metadata.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}

	labels[key] = value
	metadata.SetLabels(labels)
	return copy
}

func newResourceEventHandler(clusterID string, workQueue workqueue.RateLimitingInterface) cache.ResourceEventHandler {
	enqueue := func(obj interface{}, keyFunc func(obj interface{}) (string, error)) {
		key, err := keyFunc(obj)
		if err != nil {
			klog.Errorf("Error creating MetaNamespaceKey: %v", err)
			return
		}

		label, exists := getLabel(obj, clusterIDLabelKey)
		if exists && label != clusterID {
			klog.V(2).Infof("Label \"%s=%s\" for \"%s\" does not match \"%s\" - skipping", clusterIDLabelKey,
				label, key, clusterID)
			return
		}

		workQueue.Add(key)
	}

	return &cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			enqueue(obj, cache.MetaNamespaceKeyFunc)
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			// TODO(tpantelis) - handle updates or are the resources immutable?
		},
		DeleteFunc: func(obj interface{}) {
			enqueue(obj, cache.DeletionHandlingMetaNamespaceKeyFunc)
		},
	}
}

func (c *controller) OnAdd(clusterID string, kubeConfig *rest.Config) {
	klog.V(2).Infof("In controller OnAdd for cluster %s", clusterID)

	// startNewWatcherForCluster locks the controller so there is no need to lock here.
	c.startNewWatcherForCluster(kubeConfig, clusterID)

	klog.Infof("Cluster \"%s\" has been added - started submariner informers", clusterID)
}

func (c *controller) OnUpdate(clusterID string, kubeConfig *rest.Config) {
	klog.V(2).Infof("In controller OnUpdate for cluster %s", clusterID)

	// startNewWatcherForCluster locks the controller so there is no need to lock here.
	c.startNewWatcherForCluster(kubeConfig, clusterID)

	klog.Infof("Cluster \"%s\" has been updated - restarted submariner informers", clusterID)
}

func (c *controller) OnRemove(clusterID string) {
	klog.V(2).Infof("In controller OnRemove for cluster %s", clusterID)

	c.Lock()
	defer c.Unlock()

	c.removeExistingClusterWatcher(clusterID)

	klog.Infof("Cluster %s has been removed - stopped submariner informers", clusterID)
}

func runQueueWorker(clusterID string, federator federate.Federator, informer cache.SharedIndexInformer,
	workQueue workqueue.RateLimitingInterface) {

	for {
		key, shutdown := workQueue.Get()
		if shutdown {
			klog.V(2).Infof("Submariner watcher for cluster \"%s\" stopped", clusterID)
			return
		}

		klog.V(2).Infof("Processing key \"%s\" for cluster \"%s\"", key, clusterID)

		func() {
			defer workQueue.Done(key)

			item, exists, err := informer.GetIndexer().GetByKey(key.(string))
			if err != nil {
				klog.Errorf("Error fetching object with key %s from store: %v", key, err)
				workQueue.AddRateLimited(key)
			} else if !exists {
				klog.V(2).Infof("Key \"%s\" does not exist - deleting distributed resource", key)
				// TODO - handle delete
				workQueue.Forget(key)
			} else {
				obj := setLabel(item.(runtime.Object), clusterIDLabelKey, clusterID)

				klog.V(2).Infof("Distributing: %#v", obj)

				if err := federator.Distribute(obj); err != nil {
					klog.Errorf("Error distributing %#v: %v", item, err)
					workQueue.AddRateLimited(key)
				} else {
					workQueue.Forget(key)
				}
			}
		}()
	}
}

func (c *clusterWatcher) close() {
	close(c.stopChan)
	c.clusterWorkQueue.ShutDown()
	c.endpointWorkQueue.ShutDown()
}
