// Package node runs a kvz node: a virtual-kubelet whose pods are dispatched,
// by RuntimeClass handler, to native macOS backends.
package node

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/shirou/gopsutil/v4/mem"
	vknode "github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	"github.com/GrowlyX/kvz/pkg/podnet"
	rt "github.com/GrowlyX/kvz/pkg/runtime"
	"github.com/GrowlyX/kvz/pkg/runtime/linux"
	"github.com/GrowlyX/kvz/pkg/runtime/macos"
	"github.com/GrowlyX/kvz/pkg/runtime/microvmrt"
	"github.com/GrowlyX/kvz/pkg/runtime/process"
	"github.com/GrowlyX/kvz/pkg/runtime/volumes"
)

const Version = "v1.35.6-kvz"

type Options struct {
	Name           string
	Kubeconfig     string
	DataDir        string
	NodeIP         string
	ListenPort     int
	ServingCert    string
	ServingKey     string
	ClientCA       string
	DefaultHandler string // runtime for pods with no runtimeClassName
	MaxPods        int
	Backends       []string // subset of linux, darwin, macos
	Workers        int
}

// Provider multiplexes backends.
type Provider struct {
	t        *rt.Tracker
	backends map[string]rt.Backend
	def      string
	hostIP   string
	handler  map[string]string // pod key -> handler
}

func (p *Provider) backendFor(pod *corev1.Pod) (rt.Backend, error) {
	h := p.def
	if pod.Spec.RuntimeClassName != nil && *pod.Spec.RuntimeClassName != "" {
		h = *pod.Spec.RuntimeClassName
	}
	b, ok := p.backends[h]
	if !ok {
		return nil, fmt.Errorf("runtime %q not enabled on this node", h)
	}
	return b, nil
}

func (p *Provider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	b, err := p.backendFor(pod)
	if err != nil {
		return err
	}
	pod = pod.DeepCopy()
	rt.InitStatus(pod, p.hostIP)
	p.t.Put(pod)
	return b.Start(ctx, pod)
}

func (p *Provider) UpdatePod(ctx context.Context, pod *corev1.Pod) error { return nil }

func (p *Provider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	b, err := p.backendFor(pod)
	if err != nil {
		p.t.Delete(pod.Namespace, pod.Name)
		return nil
	}
	err = b.Stop(ctx, pod)
	// Report terminal status, then forget the pod.
	p.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) {
		for i := range s.ContainerStatuses {
			if s.ContainerStatuses[i].State.Terminated == nil {
				rt.SetTerminated(s, s.ContainerStatuses[i].Name, 0, "Completed")
			}
		}
		s.Phase = corev1.PodSucceeded
	})
	p.t.Delete(pod.Namespace, pod.Name)
	return err
}

func (p *Provider) GetPod(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	if pod := p.t.Get(ns, name); pod != nil {
		return pod, nil
	}
	return nil, apierrors.NewNotFound(corev1.Resource("pods"), name)
}

func (p *Provider) GetPodStatus(ctx context.Context, ns, name string) (*corev1.PodStatus, error) {
	pod, err := p.GetPod(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	return &pod.Status, nil
}

func (p *Provider) GetPods(ctx context.Context) ([]*corev1.Pod, error) { return p.t.List(), nil }

func (p *Provider) NotifyPods(ctx context.Context, f func(*corev1.Pod)) { p.t.SetNotifier(f) }

func (p *Provider) lookup(ns, name string) (*corev1.Pod, rt.Backend, error) {
	pod := p.t.Get(ns, name)
	if pod == nil {
		return nil, nil, apierrors.NewNotFound(corev1.Resource("pods"), name)
	}
	b, err := p.backendFor(pod)
	return pod, b, err
}

func (p *Provider) GetContainerLogs(ctx context.Context, ns, name, c string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	pod, b, err := p.lookup(ns, name)
	if err != nil {
		return nil, err
	}
	return b.Logs(ctx, pod, c, opts)
}

func (p *Provider) RunInContainer(ctx context.Context, ns, name, c string, cmd []string, attach api.AttachIO) error {
	pod, b, err := p.lookup(ns, name)
	if err != nil {
		return err
	}
	return b.Exec(ctx, pod, c, cmd, attach)
}

func (p *Provider) AttachToContainer(ctx context.Context, ns, name, c string, attach api.AttachIO) error {
	return fmt.Errorf("attach not supported")
}

func (p *Provider) GetStatsSummary(context.Context) (*statsv1alpha1.Summary, error) {
	return &statsv1alpha1.Summary{}, nil
}

func (p *Provider) GetMetricsResource(context.Context) ([]*dto.MetricFamily, error) { return nil, nil }

type portForwarder interface {
	PortForward(ctx context.Context, pod *corev1.Pod, port int32, stream io.ReadWriteCloser) error
}

func (p *Provider) PortForward(ctx context.Context, ns, name string, port int32, stream io.ReadWriteCloser) error {
	pod, b, err := p.lookup(ns, name)
	if err != nil {
		return err
	}
	if pf, ok := b.(portForwarder); ok {
		return pf.PortForward(ctx, pod, port, stream)
	}
	return fmt.Errorf("port-forward not supported by runtime %s", b.Handler())
}

var _ nodeutil.Provider = (*Provider)(nil)
var _ vknode.PodNotifier = (*Provider)(nil)

// EnsureRuntimeClasses creates RuntimeClass objects for every backend.
func EnsureRuntimeClasses(ctx context.Context, cs kubernetes.Interface) error {
	for _, h := range []string{"linux", "apple", "darwin", "macos"} {
		rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: h}, Handler: h,
			Scheduling: &nodev1.Scheduling{NodeSelector: map[string]string{"kvz.dev/runtime-" + h: "true"}}}
		if _, err := cs.NodeV1().RuntimeClasses().Create(ctx, rc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func Run(ctx context.Context, o Options) error {
	cfg, err := clientcmd.BuildConfigFromFlags("", o.Kubeconfig)
	if err != nil {
		return err
	}
	cfg.QPS, cfg.Burst = 100, 200
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	if o.MaxPods == 0 {
		o.MaxPods = 250
	}
	if o.Workers == 0 {
		o.Workers = 32
	}
	enabled := map[string]bool{}
	for _, b := range o.Backends {
		enabled[b] = true
	}
	labels := map[string]string{
		"kubernetes.io/os":       "linux", // default runtime runs Linux images
		"kubernetes.io/arch":     "arm64",
		"kubernetes.io/hostname": o.Name,
		"kvz.dev/host-os":        "darwin",
		"type":                   "kvz",
	}
	for b := range enabled {
		labels["kvz.dev/runtime-"+b] = "true"
	}
	vm, _ := mem.VirtualMemory()
	memQ := resource.NewQuantity(int64(vm.Total), resource.BinarySI)
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(runtime.NumCPU()), resource.DecimalSI),
		corev1.ResourceMemory: *memQ,
		corev1.ResourcePods:   *resource.NewQuantity(int64(o.MaxPods), resource.DecimalSI),
	}
	if enabled["macos"] {
		capacity["kvz.dev/macos-vm"] = *resource.NewQuantity(macos.MaxVMs, resource.DecimalSI)
	}

	podInf := informers.NewSharedInformerFactoryWithOptions(cs, 5*time.Minute, nodeutil.PodInformerFilter(o.Name))
	scmInf := informers.NewSharedInformerFactoryWithOptions(cs, 5*time.Minute)
	pc := nodeutil.ProviderConfig{
		Pods: podInf.Core().V1().Pods().Lister(), ConfigMaps: scmInf.Core().V1().ConfigMaps().Lister(),
		Secrets: scmInf.Core().V1().Secrets().Lister(), Services: scmInf.Core().V1().Services().Lister(),
	}
	t := rt.NewTracker()
	vols := &volumes.Materializer{ConfigMaps: pc.ConfigMaps, Secrets: pc.Secrets}
	p := &Provider{t: t, backends: map[string]rt.Backend{}, def: o.DefaultHandler, hostIP: o.NodeIP}
	cidr := os.Getenv("KVZ_POD_CIDR")
	if cidr == "" {
		cidr = "10.88.0.0/16"
	}
	pn, err := podnet.New(cidr, 0)
	if err != nil {
		return fmt.Errorf("pod network: %w", err)
	}
	if enabled["linux"] {
		mb, err := microvmrt.New(o.DataDir, t, vols, pn)
		if err != nil {
			return fmt.Errorf("linux microvm runtime: %w", err)
		}
		p.backends["linux"] = mb
	}
	if enabled["apple"] {
		p.backends["apple"] = linux.New(o.DataDir, o.NodeIP, t, vols)
	}
	if enabled["darwin"] {
		p.backends["darwin"] = process.New(o.DataDir, o.NodeIP, t)
	}
	if enabled["macos"] {
		p.backends["macos"] = macos.New(o.DataDir, t, pn)
	}
	spec := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: o.Name, Labels: labels},
		Status: corev1.NodeStatus{
			Phase:       corev1.NodePending,
			Capacity:    capacity,
			Allocatable: capacity,
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
				{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse},
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
				{Type: corev1.NodePIDPressure, Status: corev1.ConditionFalse},
				{Type: corev1.NodeNetworkUnavailable, Status: corev1.ConditionFalse},
			},
			Addresses:       []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: o.NodeIP}, {Type: corev1.NodeHostName, Address: o.Name}},
			DaemonEndpoints: corev1.NodeDaemonEndpoints{KubeletEndpoint: corev1.DaemonEndpoint{Port: int32(o.ListenPort)}},
			NodeInfo: corev1.NodeSystemInfo{
				OperatingSystem: "linux", Architecture: "arm64", KubeletVersion: Version,
				OSImage: "macOS (kvz)", ContainerRuntimeVersion: "kvz://apple-container+vz",
			},
		},
	}

	pool := x509.NewCertPool()
	if b, err := os.ReadFile(o.ClientCA); err == nil {
		pool.AppendCertsFromPEM(b)
	}
	cert, err := tls.LoadX509KeyPair(o.ServingCert, o.ServingKey)
	if err != nil {
		return err
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	mux := http.NewServeMux()
	mux.Handle("/", api.PodHandler(api.PodHandlerConfig{
		RunInContainer: p.RunInContainer, AttachToContainer: p.AttachToContainer, GetContainerLogs: p.GetContainerLogs,
		GetPods: p.GetPods, GetStatsSummary: p.GetStatsSummary, GetMetricsResource: p.GetMetricsResource, PortForward: p.PortForward,
		GetPodsFromKubernetes: func(context.Context) ([]*corev1.Pod, error) { return pc.Pods.List(k8slabels.Everything()) },
	}, false))
	klog.InfoS("starting kvz node", "name", o.Name, "ip", o.NodeIP, "backends", o.Backends)
	return runVK(ctx, vkConfig{name: o.Name, client: cs, spec: spec, provider: p, listenAddr: fmt.Sprintf(":%d", o.ListenPort),
		tlsConfig: tlsCfg, handler: mux, workers: o.Workers}, pc, podInf, scmInf)
}
