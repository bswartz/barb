/*
Copyright 2017 The Kubernetes Authors.
Copyright 2022 Ben Swartzlander

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"time"

	"golang.org/x/net/context"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamiclister"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

type controller struct {
	nodeName    string
	kubeClient  kubernetes.Interface
	dynClient   dynamic.Interface
	gvr         schema.GroupVersionResource
	nodesLister corelisters.NodeLister
	nodesSynced cache.InformerSynced
	dynLister   dynamiclister.Lister
	dynSynced   cache.InformerSynced
	workqueue   workqueue.RateLimitingInterface
}

func newController(
	kubeClient kubernetes.Interface,
	dynClient dynamic.Interface,
	gvr schema.GroupVersionResource,
	nodeName string,
	nodeInformer coreinformers.NodeInformer,
	dynInformer cache.SharedIndexInformer,
) *controller {
	c := &controller{
		nodeName:    nodeName,
		kubeClient:  kubeClient,
		dynClient:   dynClient,
		gvr:         gvr,
		nodesLister: nodeInformer.Lister(),
		nodesSynced: nodeInformer.Informer().HasSynced,
		dynLister:   dynamiclister.New(dynInformer.GetIndexer(), gvr),
		dynSynced:   dynInformer.HasSynced,
		workqueue:   workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Barbs"),
	}

	klog.Info("Setting up event handlers")
	dynInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.enqueueBarb,
		UpdateFunc: func(old, new any) {
			newBarb := new.(*unstructured.Unstructured)
			oldBarb := old.(*unstructured.Unstructured)
			if newBarb.GetResourceVersion() == oldBarb.GetResourceVersion() {
				return
			}
			c.enqueueBarb(new)
		},
	})
	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handleNode,
		UpdateFunc: func(old, new any) {
			newNode := new.(*corev1.Node)
			oldNode := old.(*corev1.Node)
			if newNode.ResourceVersion == oldNode.ResourceVersion {
				return
			}
			c.handleNode(new)
		},
	})

	return c
}

func (c *controller) enqueueBarb(obj any) {
	barb, ok := obj.(unstructured.Unstructured)
	if ok {
		c.workqueue.Add(barb.GetName())
	}
}

func (c *controller) handleNode(obj any) {
	node, ok := obj.(corev1.Node)
	if ok && c.nodeName == node.Name {
		c.workqueue.Add(node.Name)
	}
}

func (c *controller) Run(workers int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	klog.Info("Starting Barb controller")

	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.nodesSynced, c.dynSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("Starting workers")
	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	klog.Info("Started workers")
	<-stopCh
	klog.Info("Shutting down workers")

	return nil
}

func (c *controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	err := func(obj any) error {
		defer c.workqueue.Done(obj)
		var nodeName string
		var ok bool
		// We expect strings to come off the workqueue. These are the names
		// the node (possibly this node). We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if nodeName, ok = obj.(string); !ok {
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		if err := c.syncNode(context.TODO(), nodeName); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(nodeName)
			return fmt.Errorf("error syncing '%s': %s, requeuing", nodeName, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		klog.Infof("Successfully synced '%s'", nodeName)
		return nil
	}(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

func (c *controller) syncNode(ctx context.Context, nodeName string) error {
	var barb *Barb
	unst, err := c.dynLister.Get(c.nodeName)
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	} else {
		// Convert the barb object
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(unst.UnstructuredContent(), barb)
		if err != nil {
			return err
		}
	}
	if c.nodeName == nodeName {
		return c.syncSelf(ctx, barb)
	} else if nil != barb {
		return c.syncOtherNode(ctx, barb)
	}
	return nil
}

func (c *controller) syncSelf(ctx context.Context, barb *Barb) error {
	needCreate := false
	if nil == barb {
		barb = &Barb{}
		needCreate = true
	}
	var err error

	// TODO: Compute the desired barb and compare it to the actual barb

	var object map[string]any
	object, err = runtime.DefaultUnstructuredConverter.ToUnstructured(barb)
	if err != nil {
		return err
	}
	unst := &unstructured.Unstructured{Object: object}

	if needCreate {
		_, err = c.dynClient.Resource(c.gvr).Create(ctx, unst, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	} else {
		_, err = c.dynClient.Resource(c.gvr).Update(ctx, unst, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *controller) syncOtherNode(_ context.Context, _ *Barb) error {

	// TODO: Update our local routing table with information from the other node

	return nil
}
