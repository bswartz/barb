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
	"net"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
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

const barbProto = 250

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
	lastCidr4   string
	lastCidr6   string
	routes      map[string]string
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
		routes:      make(map[string]string),
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

	var err error
	err = c.readRoutes(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	err = c.readRoutes(netlink.FAMILY_V6)
	if err != nil {
		return err
	}

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
		barb = new(Barb)
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

func (c *controller) configureBridge(_ context.Context, cidr4, cidr6 string) error {
	if c.lastCidr4 == cidr4 && c.lastCidr6 == cidr6 {
		// No change
		return nil
	}

	// TODO: Configure local CNI plugin

	return nil
}

func isLinkLocak(ip net.IP) bool {
	switch len(ip) {
	case net.IPv4len:
		return 169 == ip[0] && 254 == ip[1]
	case net.IPv6len:
		return 0xfe == ip[0] && 0x80 == (ip[1]&0xc0)
	default:
		return false
	}
}

func findOtherAddress(ip net.IP) (*net.IP, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	for _, link := range links {
		var addrs []netlink.Addr
		addrs, err = netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return nil, err
		}
		found := false
		var otherIp net.IP
		for _, addr := range addrs {
			if ip.Equal(addr.IP) {
				found = true
			} else if len(addr.IP) == len(ip) {
				// Same addr type
				continue
			} else if isLinkLocak(addr.IP) {
				continue
			} else {
				otherIp = addr.IP
			}
		}
		if found {
			return &otherIp, nil
		}
	}
	return nil, fmt.Errorf("no link found")
}

func (c *controller) readRoutes(family int) error {
	routes, err := netlink.RouteList(nil, family)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if nil == route.Dst || nil == route.Gw {
			continue
		}
		c.routes[route.Dst.String()] = route.Gw.String()
	}

	return nil
}

func (c *controller) updateRoute(cidr, gw string) error {
	_, dst, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}

	route := &netlink.Route{
		Dst:      dst,
		Gw:       net.ParseIP(gw),
		Protocol: barbProto,
		Table:    unix.RT_TABLE_MAIN,
		Type:     unix.RTN_UNICAST,
	}

	err = netlink.RouteReplace(route)
	if err != nil {
		return err
	}

	// Remember the updated route
	c.routes[cidr] = gw

	return nil
}

func (c *controller) syncSelf(ctx context.Context, barb *Barb) error {
	var err error
	var node *corev1.Node
	node, err = c.kubeClient.CoreV1().Nodes().Get(ctx, c.nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	var gw4, gw6, cidr4, cidr6 string
	cidrs := node.Spec.PodCIDRs
	if len(cidrs) == 0 {
		cidrs = []string{node.Spec.PodCIDR}
	}
	for _, cidr := range cidrs {
		var n *net.IPNet
		_, n, err = net.ParseCIDR(cidr)
		if err != nil {
			klog.ErrorS(err, "invalid cidr", "cidr", cidr)
			return err
		}
		switch len(n.IP) {
		case net.IPv4len:
			cidr4 = cidr
		case net.IPv6len:
			cidr6 = cidr
		default:
			err = fmt.Errorf("unknown IP address type")
			klog.ErrorS(err, "unknown IP address type", "ip", n.IP)
			return err
		}
	}

	err = c.configureBridge(ctx, cidr4, cidr6)
	if nil != err {
		return err
	}

	var internalIp string
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			internalIp = address.Address
			break
		}
	}
	if "" == internalIp {
		klog.Info("Node doesn't have internal IP yet")
		return nil
	}
	ip := net.ParseIP(internalIp)
	if ip == nil {
		err = fmt.Errorf("invalid IP address")
		klog.ErrorS(err, "Invalid IP address", "ip", internalIp)
		return err
	}
	// The node will only have an IPv4 or an IPv6, so find the other IP
	// on the same link.
	var otherIp *net.IP
	otherIp, err = findOtherAddress(ip)
	if err != nil {
		return err
	}

	switch len(ip) {
	case net.IPv4len:
		gw4 = internalIp
		if otherIp != nil {
			gw6 = otherIp.String()
		}
	case net.IPv6len:
		gw6 = internalIp
		if otherIp != nil {
			gw4 = otherIp.String()
		}
	default:
		err = fmt.Errorf("unknown IP address type")
		klog.ErrorS(err, "unknown IP address type", "ip", ip)
		return err
	}

	needCreate := false
	if nil == barb {
		barb = new(Barb)
		needCreate = true
	}

	// Compare desired/actual barb
	if barb.Gateway4 == gw4 && barb.Gateway6 == gw6 && barb.Cidr4 == cidr4 && barb.Cidr4 == cidr6 {
		// No work to do
		return nil
	}

	// Update the barb
	barb.Gateway4 = gw4
	barb.Gateway6 = gw6
	barb.Cidr4 = cidr4
	barb.Cidr4 = cidr6

	// Convert to unstructured
	var object map[string]any
	object, err = runtime.DefaultUnstructuredConverter.ToUnstructured(barb)
	if err != nil {
		return err
	}
	unst := &unstructured.Unstructured{Object: object}

	// Either create or update the barb, as needed
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

func (c *controller) syncOtherNode(_ context.Context, barb *Barb) error {
	actualGw4 := c.routes[barb.Cidr4]
	actualGw6 := c.routes[barb.Cidr6]
	if actualGw4 == barb.Gateway4 && actualGw6 == barb.Gateway6 {
		// No work to do
		return nil
	}

	if actualGw4 != barb.Gateway4 {
		err := c.updateRoute(barb.Cidr4, barb.Gateway4)
		if err != nil {
			return err
		}
	}
	if actualGw6 != barb.Gateway6 {
		err := c.updateRoute(barb.Cidr6, barb.Gateway6)
		if err != nil {
			return err
		}
	}

	return nil
}
