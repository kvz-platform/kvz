// Package node runs a kvz node: a virtual-kubelet whose pods are dispatched,
// by RuntimeClass handler, to native macOS backends.
package node

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/GrowlyX/kvz/pkg/downward"
	"github.com/GrowlyX/kvz/pkg/egress"
	"github.com/GrowlyX/kvz/pkg/storage"
	vknode "github.com/virtual-kubelet/virtual-kubelet/node"
	"io"
	"k8s.io/client-go/tools/cache"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
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

const Version = "v1.35.10+kvz"

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
	PodCIDR        string // this node's pod CIDR (cluster-allocated)
	OverlaySecret  []byte // authenticates cross-node pod network links
	OverlayPort    int
}

// Provider multiplexes backends.
type Provider struct {
	nodeName string
	started  metav1.Time
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

// PreparePodNetwork fills status IPs before env resolution (see vendored VK).
func (p *Provider) PreparePodNetwork(pod *corev1.Pod) {
	pod.Status.HostIP = p.hostIP
	pod.Status.HostIPs = []corev1.HostIP{{IP: p.hostIP}}
	b, err := p.backendFor(pod)
	if err != nil {
		return
	}
	if pod.Spec.HostNetwork {
		pod.Status.PodIP = p.hostIP
		pod.Status.PodIPs = []corev1.PodIP{{IP: p.hostIP}}
		return
	}
	if pr, ok := b.(interface{ PrepareIP(*corev1.Pod) string }); ok {
		if ip := pr.PrepareIP(pod); ip != "" {
			pod.Status.PodIP = ip
			pod.Status.PodIPs = []corev1.PodIP{{IP: ip}}
		}
	} else if b.Handler() == "darwin" {
		pod.Status.PodIP = p.hostIP
		pod.Status.PodIPs = []corev1.PodIP{{IP: p.hostIP}}
	}
}

// UpdatePod forwards spec/metadata updates (env re-resolved by the pod
// controller) to backends that react to them.
func (p *Provider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	p.t.UpdateMeta(pod)
	b, err := p.backendFor(pod)
	if err != nil {
		return nil
	}
	if u, ok := b.(interface{ UpdatePod(*corev1.Pod) }); ok {
		u.UpdatePod(pod)
	}
	return nil
}

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

// AttachToContainer streams the container's output (from the start) until it
// exits; stdin is not wired (containers' stdio is captured to their log).
func (p *Provider) AttachToContainer(ctx context.Context, ns, name, c string, attach api.AttachIO) error {
	pod, b, err := p.lookup(ns, name)
	if err != nil {
		return err
	}
	if c == "" && len(pod.Spec.Containers) > 0 {
		c = pod.Spec.Containers[0].Name
	}
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	rc, err := b.Logs(actx, pod, c, api.ContainerLogOpts{Follow: true})
	if err != nil {
		return err
	}
	defer rc.Close()
	go func() {
		// stop following once the container has terminated
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-actx.Done():
				return
			case <-t.C:
				cur := p.t.Get(ns, name)
				if cur == nil {
					cancel()
					return
				}
				for _, cs := range cur.Status.ContainerStatuses {
					if cs.Name == c && cs.State.Terminated != nil {
						time.Sleep(300 * time.Millisecond) // drain
						cancel()
						return
					}
				}
			}
		}
	}()
	if out := attach.Stdout(); out != nil {
		_, _ = io.Copy(out, rc)
	}
	return nil
}

type statser interface {
	Stats(ctx context.Context, pod *corev1.Pod) (*rt.PodStats, error)
}

// GetStatsSummary serves the kubelet /stats/summary API (consumed by kvz's
// metrics API / kubectl top / HPA).
func (p *Provider) GetStatsSummary(ctx context.Context) (*statsv1alpha1.Summary, error) {
	now := metav1.Now()
	sum := &statsv1alpha1.Summary{Node: statsv1alpha1.NodeStats{NodeName: p.nodeName, StartTime: p.started}}
	if t, err := cpu.TimesWithContext(ctx, false); err == nil && len(t) > 0 {
		ns := uint64((t[0].User + t[0].System + t[0].Nice) * 1e9)
		sum.Node.CPU = &statsv1alpha1.CPUStats{Time: now, UsageCoreNanoSeconds: &ns}
	}
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		used := vm.Used
		avail := vm.Available
		sum.Node.Memory = &statsv1alpha1.MemoryStats{Time: now, WorkingSetBytes: &used, AvailableBytes: &avail, UsageBytes: &used}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, pod := range p.t.List() {
		b, err := p.backendFor(pod)
		if err != nil {
			continue
		}
		st, ok := b.(statser)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(pod *corev1.Pod) {
			defer wg.Done()
			sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			ps, err := st.Stats(sctx, pod)
			if err != nil || ps == nil {
				return
			}
			pst := statsv1alpha1.PodStats{PodRef: statsv1alpha1.PodReference{Name: pod.Name, Namespace: pod.Namespace, UID: string(pod.UID)}, StartTime: now}
			var podCPU, podMem uint64
			for n, c := range ps.Containers {
				cpuNs, memB := c.CPUNs, c.MemBytes
				podCPU += cpuNs
				podMem += memB
				pst.Containers = append(pst.Containers, statsv1alpha1.ContainerStats{Name: n, StartTime: now,
					CPU:    &statsv1alpha1.CPUStats{Time: now, UsageCoreNanoSeconds: &cpuNs},
					Memory: &statsv1alpha1.MemoryStats{Time: now, WorkingSetBytes: &memB, UsageBytes: &memB}})
			}
			pst.CPU = &statsv1alpha1.CPUStats{Time: now, UsageCoreNanoSeconds: &podCPU}
			pst.Memory = &statsv1alpha1.MemoryStats{Time: now, WorkingSetBytes: &podMem, UsageBytes: &podMem}
			mu.Lock()
			sum.Pods = append(sum.Pods, pst)
			mu.Unlock()
		}(pod)
	}
	wg.Wait()
	return sum, nil
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
	vols := &volumes.Materializer{ConfigMaps: pc.ConfigMaps, Secrets: pc.Secrets, Client: cs}
	p := &Provider{nodeName: o.Name, started: metav1.Now(), t: t, backends: map[string]rt.Backend{}, def: o.DefaultHandler, hostIP: o.NodeIP}
	cidr := o.PodCIDR
	if cidr == "" {
		cidr = os.Getenv("KVZ_POD_CIDR")
	}
	if cidr == "" {
		cidr = "10.88.0.0/16"
	}
	mtu, _ := strconv.Atoi(os.Getenv("KVZ_POD_MTU"))
	pn, err := podnet.New(cidr, mtu)
	if err != nil {
		return fmt.Errorf("pod network: %w", err)
	}
	egress.Register(pn)
	go storage.GCOrphans(ctx, cs)
	go p.enforceDeadlines(ctx)
	if enabled["linux"] {
		mb, err := microvmrt.New(o.DataDir, o.NodeIP, t, vols, pn)
		if err != nil {
			return fmt.Errorf("linux microvm runtime: %w", err)
		}
		p.backends["linux"] = mb
	}
	if enabled["apple"] {
		p.backends["apple"] = linux.New(o.DataDir, o.NodeIP, t, vols)
	}
	// configMap/secret changes re-render the volumes of affected pods now
	// rather than at the next periodic sync.
	nudge := func(obj any) {
		ns := ""
		switch o := obj.(type) {
		case *corev1.ConfigMap:
			ns = o.Namespace
		case *corev1.Secret:
			ns = o.Namespace
		case cache.DeletedFinalStateUnknown:
			if m, ok := o.Obj.(metav1.Object); ok {
				ns = m.GetNamespace()
			}
		}
		for _, b := range p.backends {
			if r, ok := b.(interface{ RefreshVolumes(ns string) }); ok {
				r.RefreshVolumes(ns)
			}
		}
	}
	vh := cache.ResourceEventHandlerFuncs{AddFunc: nudge, UpdateFunc: func(_, n any) { nudge(n) }, DeleteFunc: nudge}
	_, _ = scmInf.Core().V1().ConfigMaps().Informer().AddEventHandler(vh)
	_, _ = scmInf.Core().V1().Secrets().Informer().AddEventHandler(vh)
	if enabled["darwin"] {
		p.backends["darwin"] = process.New(o.DataDir, o.NodeIP, t)
	}
	if enabled["macos"] {
		p.backends["macos"] = macos.New(o.DataDir, t, pn)
	}
	downward.SetNodeAllocatable(capacity)
	vknode.SetResourceFieldRefResolver(func(fs *corev1.ResourceFieldSelector, pod *corev1.Pod, c *corev1.Container) (string, error) {
		return downward.Resource(fs, pod, c.Name)
	})
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
	mux.Handle("/metrics", p.metricsHandler())
	mux.Handle("/metrics/cadvisor", p.cadvisorHandler())
	mux.Handle("/metrics/resource", p.resourceHandler())
	// debugging (client-cert authenticated like the rest of the kubelet API;
	// reach via kubectl get --raw /api/v1/nodes/<node>/proxy/debug/...)
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/debug/kvz/podnet", func(w http.ResponseWriter, r *http.Request) { pn.DebugDump(w) })
	mux.Handle("/", api.PodHandler(api.PodHandlerConfig{
		RunInContainer: p.RunInContainer, AttachToContainer: p.AttachToContainer, GetContainerLogs: p.GetContainerLogs,
		GetPods: p.GetPods, GetStatsSummary: p.GetStatsSummary, GetMetricsResource: p.GetMetricsResource, PortForward: p.PortForward,
		GetPodsFromKubernetes: func(context.Context) ([]*corev1.Pod, error) { return pc.Pods.List(k8slabels.Everything()) },
	}, false))
	overlayEP := ""
	if len(o.OverlaySecret) > 0 {
		if o.OverlayPort == 0 {
			o.OverlayPort = 6446
		}
		if err := pn.EnableOverlay(o.Name, fmt.Sprintf(":%d", o.OverlayPort), o.OverlaySecret); err != nil {
			klog.ErrorS(err, "pod network overlay disabled")
		} else {
			overlayEP = net.JoinHostPort(o.NodeIP, strconv.Itoa(o.OverlayPort))
			if ep := os.Getenv("KVZ_OVERLAY_ENDPOINT"); ep != "" {
				overlayEP = ep
			}
			go runRoutes(ctx, informers.NewSharedInformerFactory(cs, 10*time.Minute), pn)
		}
	}
	spec.Spec.PodCIDR = cidr
	spec.Spec.PodCIDRs = []string{cidr}
	spec.Annotations = map[string]string{"kvz.dev/pod-cidr": cidr, "kvz.dev/overlay-endpoint": overlayEP}
	go runServiceProxy(ctx, informers.NewSharedInformerFactory(cs, 10*time.Minute), pn)
	go runNetworkPolicy(ctx, informers.NewSharedInformerFactory(cs, 10*time.Minute), o.Name, pn)
	klog.InfoS("starting kvz node", "name", o.Name, "ip", o.NodeIP, "backends", o.Backends)
	return runVK(ctx, vkConfig{name: o.Name, client: cs, spec: spec, provider: p, listenAddr: fmt.Sprintf(":%d", o.ListenPort),
		tlsConfig: tlsCfg, handler: mux, workers: o.Workers}, pc, podInf, scmInf)
}

// enforceDeadlines implements spec.activeDeadlineSeconds: pods active longer
// than their deadline are stopped and fail with reason DeadlineExceeded.
func (p *Provider) enforceDeadlines(ctx context.Context) {
	done := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
		live := map[string]bool{}
		for _, pod := range p.t.List() {
			key := string(pod.UID)
			live[key] = true
			if done[key] || pod.Spec.ActiveDeadlineSeconds == nil || pod.Status.StartTime == nil ||
				pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed || pod.DeletionTimestamp != nil {
				continue
			}
			if time.Since(pod.Status.StartTime.Time) < time.Duration(*pod.Spec.ActiveDeadlineSeconds)*time.Second {
				continue
			}
			done[key] = true
			b, err := p.backendFor(pod)
			if err != nil {
				continue
			}
			go func(pod *corev1.Pod) {
				p.t.Event(pod.Namespace, pod.Name, corev1.EventTypeWarning, "DeadlineExceeded", "Pod was active on the node longer than the specified deadline")
				// set the reason first: virtual-kubelet stops reporting once the
				// pod is terminal, which the container exits below make it
				p.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) {
					s.Reason = "DeadlineExceeded"
					s.Message = "Pod was active on the node longer than the specified deadline"
				})
				_ = b.Stop(ctx, pod)
				p.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) {
					for _, cs := range s.ContainerStatuses {
						if cs.State.Terminated == nil {
							rt.SetTerminated(s, cs.Name, 137, "Error")
						}
					}
					s.Phase = corev1.PodFailed
					s.Reason = "DeadlineExceeded"
					s.Message = "Pod was active on the node longer than the specified deadline"
				})
			}(pod)
		}
		for k := range done {
			if !live[k] {
				delete(done, k)
			}
		}
	}
}
