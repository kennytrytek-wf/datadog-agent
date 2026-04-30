// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver

package instrumentation

import (
	"context"
	"fmt"
	"time"

	datadoghq "github.com/DataDog/datadog-operator/api/datadoghq/v1alpha1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/DataDog/datadog-agent/pkg/util/log"
)

const (
	maxRetries = 3
)

var gvrDatadogInstrumentation = datadoghq.GroupVersion.WithResource("datadoginstrumentations")

// Controller watches DatadogInstrumentation CRs and dispatches section events to product handlers.
type Controller struct {
	statusClient dynamic.Interface
	synced       cache.InformerSynced
	workqueue    workqueue.TypedRateLimitingInterface[queueItem]
	handlers     []Handler
	isLeader     func() bool
}

// NewController creates a DatadogInstrumentation controller backed by a dynamic informer.
func NewController(statusClient dynamic.Interface, informer dynamicinformer.DynamicSharedInformerFactory, handlers []Handler, isLeader func() bool) (*Controller, error) {
	datadogInstrumentationInformer := informer.ForResource(gvrDatadogInstrumentation)
	c := &Controller{
		statusClient: statusClient,
		synced:       datadogInstrumentationInformer.Informer().HasSynced,
		workqueue:    workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedItemBasedRateLimiter[queueItem](), workqueue.TypedRateLimitingQueueConfig[queueItem]{Name: "datadoginstrumentations"}),
		handlers:     handlers,
		isLeader:     isLeader,
	}

	if _, err := datadogInstrumentationInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.handleAdd,
		UpdateFunc: c.handleUpdate,
		DeleteFunc: c.handleDelete,
	}); err != nil {
		return nil, fmt.Errorf("cannot add event handler to DatadogInstrumentation informer: %v", err)
	}

	return c, nil
}

// Run starts the controller with a single worker.
func (c *Controller) Run(ctx context.Context) {
	log.Infof("Starting DatadogInstrumentation Controller (waiting for cache sync)")
	if !cache.WaitForCacheSync(ctx.Done(), c.synced) {
		log.Errorf("Failed to wait for DatadogInstrumentation caches to sync")
		return
	}

	go c.worker(ctx)

	log.Infof("Started DatadogInstrumentation Controller (cache sync finished)")
	<-ctx.Done()
	log.Infof("Stopping DatadogInstrumentation Controller")
	c.workqueue.ShutDown()
}

func (c *Controller) worker(ctx context.Context) {
	for c.process(ctx) {
	}
}

func (c *Controller) process(ctx context.Context) bool {
	item, shutdown := c.workqueue.Get()
	if shutdown {
		log.Infof("DatadogInstrumentation Controller: caught stop signal in workqueue")
		return false
	}
	defer c.workqueue.Done(item)

	if err := c.processDatadogInstrumentation(ctx, item); err == nil {
		c.workqueue.Forget(item)
	} else {
		numRequeues := c.workqueue.NumRequeues(item)
		if numRequeues >= maxRetries {
			c.workqueue.Forget(item)
		} else {
			c.workqueue.AddAfter(item, time.Second*time.Duration(numRequeues+1))
		}
		log.Errorf("Impossible to synchronize DatadogInstrumentation (attempt #%d): %s, err: %v", numRequeues, item.key, err)
	}
	return true
}

func (c *Controller) processDatadogInstrumentation(ctx context.Context, item queueItem) error {
	return c.reconcile(ctx, eventSnapshot{old: item.old, new: item.new})
}

func (c *Controller) reconcile(ctx context.Context, snapshot eventSnapshot) error {
	statuses := make([]HandlerStatus, 0)
	handled := false

	if snapshot.old == nil && snapshot.new == nil {
		return nil
	}

	for _, handler := range c.handlers {
		eventType, ok := classifySectionEvent(handler, snapshot.old, snapshot.new)
		if !ok {
			continue
		}
		handled = true

		eventCR := snapshot.new
		if eventType == EventDelete {
			eventCR = snapshot.old
		}
		status, err := handler.Handle(ctx, eventType, eventCR)
		if err != nil {
			return err
		}
		statuses = append(statuses, status)
	}

	if !handled || len(statuses) == 0 || snapshot.new == nil || !c.isLeader() {
		return nil
	}
	return updateStatusConditions(ctx, c.statusClient, snapshot.new, statuses)
}

func (c *Controller) handleAdd(obj interface{}) {
	cr, err := datadogInstrumentationFromObject(obj)
	if err != nil {
		log.Debugf("Couldn't convert DatadogInstrumentation add event: %v", err)
		return
	}
	c.enqueueChangeEvent(nil, cr)
}

func (c *Controller) handleUpdate(oldObj, newObj interface{}) {
	oldCR, err := datadogInstrumentationFromObject(oldObj)
	if err != nil {
		log.Debugf("Couldn't convert old DatadogInstrumentation update event: %v", err)
		return
	}
	newCR, err := datadogInstrumentationFromObject(newObj)
	if err != nil {
		log.Debugf("Couldn't convert new DatadogInstrumentation update event: %v", err)
		return
	}
	c.enqueueChangeEvent(oldCR, newCR)
}

func (c *Controller) handleDelete(obj interface{}) {
	cr, err := datadogInstrumentationFromObject(obj)
	if err != nil {
		log.Debugf("Couldn't convert DatadogInstrumentation delete event: %v", err)
		return
	}
	c.enqueueChangeEvent(cr, nil)
}

func (c *Controller) enqueueChangeEvent(oldObj, newObj *datadoghq.DatadogInstrumentation) {
	obj := newObj
	if obj == nil {
		obj = oldObj
	}
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Debugf("Couldn't get key for DatadogInstrumentation object %v: %v", obj, err)
		return
	}
	c.workqueue.AddRateLimited(newQueueItem(key, oldObj, newObj))
}
