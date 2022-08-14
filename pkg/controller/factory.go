package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/kcp-dev/kcp/pkg/logging"
	objectruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

type Resource interface {
	objectruntime.Object
	logging.Object
}

// instances contain only the logic to reconcile some resource type
type Reconciler[O Resource] interface {
	// main reconciliation logic
	Reconcile(context.Context, O) error

	// update status, etc.
	PostReconcile(context.Context, string, O, O, error) error

	GetLogger() logr.Logger
}

// controller encapsulates the queueing and other logic to drive a single reconciler
type Controller[O Resource] interface {
	Enqueue(O, logr.Logger, string)
	Start(context.Context, int)
}

type Options struct {
	Name         string
	NumRequeues  int
	ResyncPeriod time.Duration
}

type controller[R Reconciler[O], O Resource] struct {
	name         string // some unique name for logging purposes
	focusType    string // the type that is the primary focus of this controller
	queue        workqueue.RateLimitingInterface
	indexer      cache.Indexer
	recon        R // reconciliation logic for the focus type
	numRequeues  int
	resyncPeriod time.Duration
}

// Opinionated creation of plumbing to drive typed reconciliation logic
func New[R Reconciler[O], O Resource](informer cache.SharedIndexInformer, recon R, options *Options) *controller[R, O] {
	name := options.Name
	focusType := fmt.Sprintf("%T", *new(O))
	numRequeues := 5
	resyncPeriod := options.ResyncPeriod

	if name == "" {
		name = fmt.Sprintf("%s-controller", focusType)
	}
	if options.NumRequeues == 0 {
		numRequeues = 5
	}

	queue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), name)

	c := &controller[R, O]{
		name:         name,
		focusType:    focusType,
		queue:        queue,
		indexer:      informer.GetIndexer(),
		recon:        recon,
		numRequeues:  numRequeues,
		resyncPeriod: resyncPeriod,
	}

	logger := recon.GetLogger()

	informer.AddEventHandlerWithResyncPeriod(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.Enqueue(obj.(O), logger, "") },
		UpdateFunc: func(oldObj, newObj interface{}) { c.Enqueue(newObj.(O), logger, "") },
		DeleteFunc: func(obj interface{}) { c.Enqueue(obj.(O), logger, "") },
	}, options.ResyncPeriod)

	return c
}

// type safe enqueue
func (c *controller[R, O]) Enqueue(obj O, logger logr.Logger, suffix string) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	logger = logging.WithQueueKey(logger, key)
	logger.V(2).Info(fmt.Sprintf("queueing %s%s", c.focusType, suffix))
	c.queue.Add(key)
}

func (c *controller[R, O]) Start(ctx context.Context, numWorkers int) {
	defer runtime.HandleCrash()
	defer c.queue.ShutDown()

	logger := logging.WithReconciler(klog.FromContext(ctx), c.name)
	ctx = klog.NewContext(ctx, logger)
	logger.Info("Starting controller")

	defer logger.Info("Shutting down controller")

	done := ctx.Done()
	for i := 0; i < numWorkers; i++ {
		go wait.Until(func() { c.processNextItem(ctx) }, time.Second, done)
	}

	<-done
}

func (c *controller[R, O]) handleError(err error, key interface{}) {
	if c.queue.NumRequeues(key) < c.numRequeues {
		klog.Infof("[%s] Error syncing %s %v: %v", c.name, c.focusType, key, err)
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	runtime.HandleError(err)
	klog.Infof("[%s] Dropping %s %q out of the queue: %v", c.name, c.focusType, key, err)
}

func (c *controller[R, O]) processNextItem(ctx context.Context) bool {
	k, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(k)

	key := k.(string)

	logger := logging.WithQueueKey(c.recon.GetLogger(), key)
	ctx = klog.NewContext(ctx, logger)
	logger.V(1).Info("processing key")

	_, clusterAwareName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logger.Error(err, "invalid key")
		return true
	}

	obj, _, err := c.indexer.GetByKey(key)
	if err != nil {
		klog.Errorf("[%s] Fetching object with key %s from store failed with %v", c.name, k, err)
		c.handleError(err, k)
		return true
	}

	var cur O
	var prev O
	if obj != nil {
		prev = obj.(O)
		cur = prev.DeepCopyObject().(O)
	}
	err = c.recon.Reconcile(ctx, cur)
	pErr := c.recon.PostReconcile(ctx, clusterAwareName, prev, cur, err)

	if pErr != nil {
		err = pErr
	}

	if err != nil {
		c.handleError(err, k)
	}
	return true
}
