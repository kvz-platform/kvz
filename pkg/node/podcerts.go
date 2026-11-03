package node

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/kubelet/podcertificate"
	"k8s.io/utils/clock"

	rt "github.com/GrowlyX/kvz/pkg/runtime"
)

// startPodCertificates runs the kubelet's pod certificate manager for this
// node: for each podCertificate projected volume source it generates a key,
// creates a PodCertificateRequest, waits for the signer to issue a chain, and
// refreshes it before expiry. Wired the same way as in pkg/kubelet.
func startPodCertificates(ctx context.Context, cs kubernetes.Interface, nodeName string, t *rt.Tracker) podcertificate.Manager {
	byField := func(field, value string) informers.SharedInformerOption {
		return informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector(field, value).String()
		})
	}
	pcrInf := informers.NewSharedInformerFactoryWithOptions(cs, 0, byField("spec.nodeName", nodeName))
	nodeInf := informers.NewSharedInformerFactoryWithOptions(cs, 0, byField("metadata.name", nodeName))
	eb := record.NewBroadcaster()
	eb.StartRecordingToSink(&corev1client.EventSinkImpl{Interface: cs.CoreV1().Events(corev1.NamespaceAll)})
	rec := eb.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "kubelet", Host: nodeName})
	m := podcertificate.NewIssuingManager(cs, trackerPods{t}, rec,
		pcrInf.Certificates().V1().PodCertificateRequests(), nodeInf.Core().V1().Nodes(),
		types.NodeName(nodeName), clock.RealClock{})
	pcrInf.Start(ctx.Done())
	nodeInf.Start(ctx.Done())
	go m.Run(ctx)
	go func() { <-ctx.Done(); eb.Shutdown() }()
	return m
}

// trackerPods is the kubelet pod manager view the pod certificate manager
// needs: the pods this node is running.
type trackerPods struct{ t *rt.Tracker }

func (p trackerPods) GetPods() []*corev1.Pod { return p.t.List() }

func (p trackerPods) GetPodByUID(uid types.UID) (*corev1.Pod, bool) {
	for _, pod := range p.t.List() {
		if pod.UID == uid {
			return pod, true
		}
	}
	return nil, false
}
