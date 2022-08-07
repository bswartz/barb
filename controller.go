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
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/dynamic/dynamiclister"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const barbProto = 250

const (
	crdKind     = "Barb"
	crdGroup    = "barb.com"
	crdVersion  = "v1"
	crdResource = "barbs"
)

type controller struct {
	nodeName    string
	dynClient   dynamic.Interface
	nodeGvr     schema.GroupVersionResource
	barbGvr     schema.GroupVersionResource
	nodeLister  dynamiclister.Lister
	nodesSynced cache.InformerSynced
	barbLister  dynamiclister.Lister
	barbsSynced cache.InformerSynced
	workqueue   workqueue.RateLimitingInterface
	lastCidr4   string
	lastCidr6   string
	routes      map[string]string
	cniDir      string
}

func newController(
	dynClient dynamic.Interface,
	nodeName string,
	cniDir string,
	informerFactory dynamicinformer.DynamicSharedInformerFactory,
) *controller {
	c := &controller{
		nodeName:  nodeName,
		dynClient: dynClient,
		nodeGvr:   schema.GroupVersionResource{Group: "", Version: "v1", Resource: "nodes"},
		barbGvr:   schema.GroupVersionResource{Group: crdGroup, Version: crdVersion, Resource: crdResource},
		workqueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Barbs"),
		routes:    make(map[string]string),
		cniDir:    cniDir,
	}

	barbInformer := informerFactory.ForResource(c.barbGvr).Informer()
	c.barbLister = dynamiclister.New(barbInformer.GetIndexer(), c.barbGvr)
	c.barbsSynced = barbInformer.HasSynced
	nodeInformer := informerFactory.ForResource(c.barbGvr).Informer()
	c.nodeLister = dynamiclister.New(nodeInformer.GetIndexer(), c.nodeGvr)
	c.nodesSynced = nodeInformer.HasSynced

	klog.InfoS("Setting up event handlers")
	barbInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
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
	nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handleNode,
		UpdateFunc: func(old, new any) {
			newNode := new.(*unstructured.Unstructured)
			oldNode := old.(*unstructured.Unstructured)
			if newNode.GetResourceVersion() == oldNode.GetResourceVersion() {
				return
			}
			oldCidrs, _, _ := unstructured.NestedStringSlice(oldNode.UnstructuredContent(), "spec", "podCIDRs")
			newCidrs, _, _ := unstructured.NestedStringSlice(newNode.UnstructuredContent(), "spec", "podCIDRs")
			oldAddresses, _, _ := unstructured.NestedSlice(oldNode.UnstructuredContent(), "status", "addresses")
			newAddresses, _, _ := unstructured.NestedSlice(oldNode.UnstructuredContent(), "status", "addresses")
			if reflect.DeepEqual(oldCidrs, newCidrs) && reflect.DeepEqual(oldAddresses, newAddresses) {
				return
			}
			c.handleNode(new)
		},
	})

	return c
}

func (c *controller) enqueueBarb(obj any) {
	barb, ok := obj.(*unstructured.Unstructured)
	if ok {
		c.workqueue.Add(barb.GetName())
	}
}

func (c *controller) handleNode(obj any) {
	node, ok := obj.(*unstructured.Unstructured)
	if ok && c.nodeName == node.GetName() {
		c.workqueue.Add(c.nodeName)
	}
}

func (c *controller) Run(workers int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	klog.InfoS("Starting Barb controller")

	var err error
	err = c.readRoutes(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	err = c.readRoutes(netlink.FAMILY_V6)
	if err != nil {
		return err
	}

	klog.InfoS("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.nodesSynced, c.barbsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.InfoS("Starting workers")
	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	klog.InfoS("Started workers")
	<-stopCh
	klog.InfoS("Shutting down workers")

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
		klog.InfoS("Successfully synced", "node", nodeName)
		return nil
	}(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

func (c *controller) syncNode(ctx context.Context, nodeName string) error {
	barb, err := c.barbLister.Get(nodeName)
	if err != nil {
		if !errors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to get barb", "node", nodeName)
			return err
		}
		// Not found, but that's okay
	}
	if c.nodeName == nodeName {
		return c.syncSelf(ctx, barb)
	} else if nil != barb {
		return c.syncOtherNode(ctx, barb)
	}
	return nil
}

func createBridgeConf(cidr4, cidr6 string) any {
	// https://github.com/containernetworking/cni/blob/main/pkg/types/types.go
	type Route struct {
		Dst string `json:"dst"`
		GW  string `json:"gw,omitempty"`
	}

	// https://github.com/containernetworking/plugins/blob/main/plugins/ipam/host-local/backend/allocator/config.go
	type Range struct {
		RangeStart string `json:"rangeStart,omitempty"` // The first ip, inclusive
		RangeEnd   string `json:"rangeEnd,omitempty"`   // The last ip, inclusive
		Subnet     string `json:"subnet"`
		Gateway    string `json:"gateway,omitempty"`
	}

	type RangeSet []Range

	var ranges []RangeSet
	var routes []*Route

	if cidr4 != "" {
		ranges = append(ranges, []Range{{Subnet: cidr4}})
		routes = append(routes, &Route{Dst: "0.0.0.0/0"})
	}
	if cidr6 != "" {
		ranges = append(ranges, []Range{{Subnet: cidr6}})
		routes = append(routes, &Route{Dst: "::/0"})
	}

	type IPAMConfig struct {
		Type   string     `json:"type,omitempty"`
		Routes []*Route   `json:"routes"`
		Ranges []RangeSet `json:"ranges"`
	}

	// https://github.com/containernetworking/plugins/blob/main/plugins/main/bridge/bridge.go
	type NetConf struct {
		CNIVersion string     `json:"cniVersion,omitempty"`
		Name       string     `json:"name,omitempty"`
		Type       string     `json:"type,omitempty"`
		BrName     string     `json:"bridge"`
		IsGW       bool       `json:"isGateway"`
		IPMasq     bool       `json:"ipMasq"`
		IPAM       IPAMConfig `json:"ipam,omitempty"`
	}

	return &NetConf{
		CNIVersion: "0.6.0",
		Name:       "bridge",
		Type:       "bridge",
		BrName:     "cni0",
		IsGW:       true,
		IPMasq:     true,
		IPAM: IPAMConfig{
			Type:   "host-local",
			Ranges: ranges,
			Routes: routes,
		},
	}
}

func (c *controller) configureBridge(cidr4, cidr6 string) error {
	if c.lastCidr4 == cidr4 && c.lastCidr6 == cidr6 {
		// No change
		return nil
	}

	confPath := filepath.Join(c.cniDir, "90-bridge.conf")
	tmpPath := confPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		klog.ErrorS(err, "Failed to create CNI config file")
		return err
	}

	conf := createBridgeConf(cidr4, cidr6)
	err = json.NewEncoder(f).Encode(conf)
	f.Close()
	if err != nil {
		klog.ErrorS(err, "Failed to write CNI config file")
		return err
	}

	err = os.Rename(tmpPath, confPath)
	if err != nil {
		klog.ErrorS(err, "Failed to rename CNI config file")
		return err
	}

	c.lastCidr4 = cidr4
	c.lastCidr6 = cidr6

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

func matchAddrFamily(x, y net.IP) bool {
	return x.To4() != nil && y.To4() != nil || x.To16() != nil && x.To4() == nil && y.To16() != nil && y.To4() == nil
}

func findOtherAddress(ip net.IP) (net.IP, error) {
	links, err := netlink.LinkList()
	if err != nil {
		klog.ErrorS(err, "Failed to list links")
		return nil, err
	}
	for _, link := range links {
		var addrs []netlink.Addr
		addrs, err = netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			klog.ErrorS(err, "Failed to list addresses", "link", link)
			return nil, err
		}
		found := false
		var otherIp net.IP
		for _, addr := range addrs {
			if ip.Equal(addr.IP) {
				found = true
			} else if matchAddrFamily(addr.IP, ip) {
				// Same addr type
				continue
			} else if isLinkLocak(addr.IP) {
				continue
			} else {
				otherIp = addr.IP
			}
		}
		if found {
			return otherIp, nil
		}
	}
	klog.ErrorS(nil, "No link found that matches address", "ip", ip)
	return nil, fmt.Errorf("no link found")
}

func (c *controller) readRoutes(family int) error {
	routes, err := netlink.RouteList(nil, family)
	if err != nil {
		klog.ErrorS(err, "Failed to list routes")
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
		klog.ErrorS(err, "Failed to parse CIDR", "cidr", cidr)
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
		klog.ErrorS(err, "Failed to replace route")
		return err
	}

	// Remember the updated route
	c.routes[cidr] = gw

	return nil
}

func (c *controller) syncSelf(ctx context.Context, barb *unstructured.Unstructured) error {
	node, err := c.nodeLister.Get(c.nodeName)
	if err != nil {
		klog.ErrorS(err, "Failed to get node", "node", c.nodeName)
		return err
	}
	content := node.UnstructuredContent()

	var gw4, gw6, cidr4, cidr6 string
	cidrs, _, _ := unstructured.NestedStringSlice(content, "spec", "podCIDRs")
	if len(cidrs) == 0 {
		cidr, _, _ := unstructured.NestedString(content, "spec", "podCIDR")
		cidrs = []string{cidr}
	}
	for _, cidr := range cidrs {
		var n *net.IPNet
		_, n, err = net.ParseCIDR(cidr)
		if err != nil {
			klog.ErrorS(err, "Invalid cidr", "cidr", cidr)
			return err
		}
		switch len(n.IP) {
		case net.IPv4len:
			cidr4 = cidr
		case net.IPv6len:
			cidr6 = cidr
		default:
			err = fmt.Errorf("unknown IP address type")
			klog.ErrorS(err, "Unknown IP address type", "ip", n.IP)
			return err
		}
	}

	err = c.configureBridge(cidr4, cidr6)
	if err != nil {
		// Logged
		return err
	}

	addresses, _, _ := unstructured.NestedSlice(content, "status", "addresses")
	var internalIp string
	for _, address := range addresses {
		addrContent, ok := address.(map[string]any)
		if !ok {
			continue
		}
		addrVal, _, _ := unstructured.NestedString(addrContent, "address")
		addrType, _, _ := unstructured.NestedString(addrContent, "type")
		if addrType == "InternalIP" {
			internalIp = addrVal
			break
		}
	}
	if "" == internalIp {
		klog.Warning("Node doesn't have internal IP yet")
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
	var otherIp net.IP
	otherIp, err = findOtherAddress(ip)
	if err != nil {
		// Logged
		return err
	}

	if ip.To4() != nil {
		gw4 = internalIp
		if otherIp != nil {
			gw6 = otherIp.String()
		}
	} else {
		gw6 = internalIp
		if otherIp != nil {
			gw4 = otherIp.String()
		}
	}

	needCreate := false
	var oldGw4, oldGw6, oldCidr4, oldCidr6 string
	if nil == barb {
		needCreate = true
	} else {
		content = barb.UnstructuredContent()
		oldGw4, _, _ = unstructured.NestedString(content, "gw4")
		oldGw6, _, _ = unstructured.NestedString(content, "gw6")
		oldCidr4, _, _ = unstructured.NestedString(content, "cidr4")
		oldCidr6, _, _ = unstructured.NestedString(content, "cidr6")
	}

	// Compare desired/actual barb
	if oldGw4 == gw4 && oldGw6 == gw6 && oldCidr4 == cidr4 && oldCidr6 == cidr6 {
		// No work to do
		klog.V(2).InfoS("No change on local node")
		return nil
	}

	// Update the barb
	klog.V(1).InfoS("Changed information on local node", "gw4", gw4, "gw6", gw6,
		"cidr4", cidr4, "cidr6", cidr6)

	barb = new(unstructured.Unstructured)
	barb.SetAPIVersion(fmt.Sprintf("%s/%s", crdGroup, crdVersion))
	barb.SetKind(crdKind)
	barb.SetName(c.nodeName)
	content = barb.UnstructuredContent()
	_ = unstructured.SetNestedField(content, gw4, "gw4")
	_ = unstructured.SetNestedField(content, gw6, "gw6")
	_ = unstructured.SetNestedField(content, cidr4, "cidr4")
	_ = unstructured.SetNestedField(content, cidr6, "cidr6")

	// Either create or update the barb, as needed
	if needCreate {
		_, err = c.dynClient.Resource(c.barbGvr).Create(ctx, barb, metav1.CreateOptions{})
		if err != nil {
			klog.ErrorS(err, "Failed to create barb")
			return err
		}
	} else {
		_, err = c.dynClient.Resource(c.barbGvr).Update(ctx, barb, metav1.UpdateOptions{})
		if err != nil {
			klog.ErrorS(err, "Failed to update barb")
			return err
		}
	}

	return nil
}

func (c *controller) syncOtherNode(_ context.Context, barb *unstructured.Unstructured) error {
	name := barb.GetName()
	content := barb.UnstructuredContent()
	cidr4, _, _ := unstructured.NestedString(content, "cidr4")
	cidr6, _, _ := unstructured.NestedString(content, "cidr6")
	gw4, _, _ := unstructured.NestedString(content, "gw4")
	gw6, _, _ := unstructured.NestedString(content, "gw6")
	changed := false
	if cidr4 != "" && c.routes[cidr4] != gw4 {
		err := c.updateRoute(cidr4, gw4)
		if err != nil {
			// Logged
			return err
		}
		klog.V(1).InfoS("Updated v4 route", "barb", name,
			"gw", gw4, "cidr", cidr4)
		changed = true
	}
	if cidr6 != "" && c.routes[cidr6] != gw6 {
		err := c.updateRoute(cidr6, gw6)
		if err != nil {
			// Logged
			return err
		}
		klog.V(1).InfoS("Updated v6 route", "barb", name,
			"gw", gw6, "cidr", cidr6)
		changed = true
	}
	if !changed {
		// No work to do
		klog.V(2).InfoS("No changed routes on barb", "barb", name)
		return nil
	}

	return nil
}
