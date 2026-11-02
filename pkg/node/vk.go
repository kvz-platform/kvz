package node

import (
	"context"
	"crypto/tls"
	"net/http"
	"path"
	"time"

	vknode "github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

// fastLimiter replaces client-go's default controller limiter (10 qps /
// burst 100 overall), which caps a node at ~10 pod status transitions per
// second and was the dominant source of pod-start latency in early runs.
func fastLimiter() workqueue.TypedRateLimiter[any] {
	return workqueue.NewTypedMaxOfRateLimiter[any](
		workqueue.NewTypedItemExponentialFailureRateLimiter[any](time.Millisecond, 30*time.Second),
		&workqueue.TypedBucketRateLimiter[any]{Limiter: rate.NewLimiter(rate.Limit(2000), 5000)},
	)
}

type vkConfig struct {
	name       string
	client     kubernetes.Interface
	spec       corev1.Node
	provider   *Provider
	listenAddr string
	tlsConfig  *tls.Config
	handler    http.Handler
	workers    int
}

// runVK is nodeutil.Node.Run with tunable queues.
func runVK(ctx context.Context, c vkConfig, pc nodeutil.ProviderConfig, podInf informers.SharedInformerFactory, scmInf informers.SharedInformerFactory) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	eb := record.NewBroadcaster()
	rec := eb.NewRecorder(scheme.Scheme, corev1.EventSource{Component: path.Join(c.name, "pod-controller")})
	eb.StartRecordingToSink(&corev1client.EventSinkImpl{Interface: c.client.CoreV1().Events(corev1.NamespaceAll)})
	defer eb.Shutdown()

	np := vknode.NewNaiveNodeProvider()
	nc, err := vknode.NewNodeController(np, &c.spec, c.client.CoreV1().Nodes(),
		vknode.WithNodeEnableLeaseV1(nodeutil.NodeLeaseV1Client(c.client), vknode.DefaultLeaseDuration))
	if err != nil {
		return err
	}
	podCtl, err := vknode.NewPodController(vknode.PodControllerConfig{
		PodClient:                            c.client.CoreV1(),
		EventRecorder:                        rec,
		Provider:                             c.provider,
		PodInformer:                          podInf.Core().V1().Pods(),
		SecretInformer:                       scmInf.Core().V1().Secrets(),
		ConfigMapInformer:                    scmInf.Core().V1().ConfigMaps(),
		ServiceInformer:                      scmInf.Core().V1().Services(),
		SyncPodsFromKubernetesRateLimiter:    fastLimiter(),
		DeletePodsFromKubernetesRateLimiter:  fastLimiter(),
		SyncPodStatusFromProviderRateLimiter: fastLimiter(),
	})
	if err != nil {
		return err
	}

	l, err := tls.Listen("tcp", c.listenAddr, c.tlsConfig)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: c.handler, TLSConfig: c.tlsConfig, ReadHeaderTimeout: 30 * time.Second}
	go srv.Serve(l) //nolint:errcheck
	defer srv.Close()

	go podInf.Start(ctx.Done())
	go scmInf.Start(ctx.Done())
	go podCtl.Run(ctx, c.workers) //nolint:errcheck
	select {
	case <-ctx.Done():
		return nil
	case <-podCtl.Ready():
	case <-podCtl.Done():
		return podCtl.Err()
	}
	go nc.Run(ctx) //nolint:errcheck
	select {
	case <-ctx.Done():
		return nil
	case <-nc.Ready():
	case <-nc.Done():
		return nc.Err()
	}
	// mark ready
	for i, cond := range c.spec.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			c.spec.Status.Conditions[i].Status = corev1.ConditionTrue
			c.spec.Status.Conditions[i].Reason = "KubeletReady"
			c.spec.Status.Conditions[i].Message = "kvz node is ready"
			c.spec.Status.Conditions[i].LastHeartbeatTime = metav1.Now()
			c.spec.Status.Conditions[i].LastTransitionTime = metav1.Now()
		}
	}
	c.spec.Status.Phase = corev1.NodeRunning
	if err := np.UpdateStatus(ctx, &c.spec); err != nil {
		return err
	}
	klog.InfoS("kvz node ready", "name", c.name)
	select {
	case <-nc.Done():
		return nc.Err()
	case <-podCtl.Done():
		return podCtl.Err()
	case <-ctx.Done():
		return nil
	}
}
