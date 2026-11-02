package node

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"

	"github.com/GrowlyX/kvz/pkg/podnet"
)

// runRoutes keeps the pod network's cross-node routes in sync with the Node
// objects' kvz.dev/pod-cidr and kvz.dev/overlay-endpoint annotations.
func runRoutes(ctx context.Context, f informers.SharedInformerFactory, pn *podnet.Net) {
	inf := f.Core().V1().Nodes()
	kick := make(chan struct{}, 1)
	_, _ = inf.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(any) { nudge(kick) }, UpdateFunc: func(any, any) { nudge(kick) }, DeleteFunc: func(any) { nudge(kick) },
	})
	go inf.Informer().Run(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), inf.Informer().HasSynced)
	nudge(kick)
	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
		}
		nodes, _ := inf.Lister().List(labels.Everything())
		routes, eps := map[string]string{}, map[string]string{}
		for _, n := range nodes {
			cidr := n.Annotations["kvz.dev/pod-cidr"]
			if cidr == "" {
				cidr = n.Spec.PodCIDR
			}
			if cidr != "" {
				routes[cidr] = n.Name
			}
			eps[n.Name] = n.Annotations["kvz.dev/overlay-endpoint"]
		}
		pn.SetRoutes(routes, eps)
	}
}

var _ = corev1.NodeReady
