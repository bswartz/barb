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
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

var version = "unknown"

func main() {
	var (
		masterURL  string
		kubeconfig string
		nodeName   string
		cniDir     string
	)
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&nodeName, "node-name", "", "The name of this node.")
	flag.StringVar(&cniDir, "cni-dir", "/etc/cni/net.d", "The directory for CNI config files.")

	klog.InitFlags(nil)
	flag.Parse()

	if nodeName == "" {
		log.Fatalf("node-name is a required argument")
	}

	klog.InfoS("Starting", "version", version)

	// set up signals so we handle the first shutdown signal gracefully
	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		close(stopCh)
		<-sigCh
		os.Exit(1) // second signal. Exit directly.
	}()

	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		klog.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Failed to create dynamic client: %v", err)
	}

	gvr := schema.GroupVersionResource{Group: "barb.com", Version: "v1", Resource: "barbs"}

	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	dynInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 0)

	c := newController(kubeClient, dynClient, gvr, nodeName, cniDir,
		kubeInformerFactory.Core().V1().Nodes(),
		dynInformerFactory.ForResource(gvr).Informer())

	kubeInformerFactory.Start(stopCh)
	dynInformerFactory.Start(stopCh)

	if err = c.Run(2, stopCh); err != nil {
		klog.Fatalf("Error running controller: %s", err.Error())
	}
}
