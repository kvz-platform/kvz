package node

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/GrowlyX/kvz/pkg/clusterdns"
	"github.com/GrowlyX/kvz/pkg/podnet"
)

// runServiceProxy programs ClusterIP load balancing into the pod switch and
// publishes <svc>.<ns>.svc.cluster.local on the gateway DNS (the kvz
// equivalents of kube-proxy and CoreDNS).
func runServiceProxy(ctx context.Context, f informers.SharedInformerFactory, pn *podnet.Net) {
	svcInf := f.Core().V1().Services()
	epsInf := f.Discovery().V1().EndpointSlices()
	kick := make(chan struct{}, 1)
	h := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { nudge(kick) },
		UpdateFunc: func(any, any) { nudge(kick) },
		DeleteFunc: func(any) { nudge(kick) },
	}
	_, _ = svcInf.Informer().AddEventHandler(h)
	_, _ = epsInf.Informer().AddEventHandler(h)
	go svcInf.Informer().Run(ctx.Done())
	go epsInf.Informer().Run(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), svcInf.Informer().HasSynced, epsInf.Informer().HasSynced)

	type key struct {
		ip    string
		port  int32
		proto string
	}
	programmed := map[key]bool{}
	lb := newHostLB(pn)
	// cluster DNS (CoreDNS equivalent): UDP answered in the switch, TCP via
	// the netstack listener the VIP's TCP port is NATed to
	cdns := clusterdns.New()
	cdns.SetPodCIDRs([]string{"10.88.0.0/13", "10.96.0.0/11"}) // 10.88-10.127: node pod CIDRs
	if err := pn.ServeClusterDNS(ctx, net.ParseIP(ClusterDNS), cdns); err != nil {
		klog.ErrorS(err, "cluster DNS")
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
		}
		time.Sleep(20 * time.Millisecond) // coalesce bursts
		svcs, _ := svcInf.Lister().List(labels.Everything())
		slices, _ := epsInf.Lister().List(labels.Everything())
		bySvc := map[string][]*discoveryv1.EndpointSlice{}
		for _, s := range slices {
			n := s.Labels[discoveryv1.LabelServiceName]
			bySvc[s.Namespace+"/"+n] = append(bySvc[s.Namespace+"/"+n], s)
		}
		seen := map[key]bool{}
		// cluster DNS VIP -> this node's gateway resolver (node-local DNS)
		cdns.Sync(svcs, slices)
		pn.SetService(net.ParseIP(ClusterDNS), 53, "UDP", []podnet.Backend{{IP: pn.GW, Port: 53}}) // fallback only
		pn.SetService(net.ParseIP(ClusterDNS), 53, "TCP", []podnet.Backend{{IP: pn.GW, Port: podnet.DNSTCPPort}})
		seen[key{ClusterDNS, 53, "UDP"}] = true
		seen[key{ClusterDNS, 53, "TCP"}] = true
		for _, svc := range svcs {
			eps := bySvc[svc.Namespace+"/"+svc.Name]
			if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone {
				// headless: per-endpoint hostname records (StatefulSet pod DNS)
				for _, s := range eps {
					for _, e := range s.Endpoints {
						if e.Hostname != nil && *e.Hostname != "" && len(e.Addresses) > 0 {
							pn.AddDNS("svc.cluster.local.", *e.Hostname+"."+svc.Name+"."+svc.Namespace, net.ParseIP(e.Addresses[0]))
						}
					}
				}
				// and the service name to the first ready endpoint
				for _, s := range eps {
					for _, e := range s.Endpoints {
						if ready(e) && len(e.Addresses) > 0 {
							pn.AddDNS("svc.cluster.local.", svc.Name+"."+svc.Namespace, net.ParseIP(e.Addresses[0]))
							goto next
						}
					}
				}
			next:
				continue
			}
			if svc.Spec.ClusterIP == ClusterDNS {
				continue // programmed above (node-local resolver)
			}
			vip := net.ParseIP(svc.Spec.ClusterIP)
			pn.AddDNS("svc.cluster.local.", svc.Name+"."+svc.Namespace, vip)
			for _, sp := range svc.Spec.Ports {
				proto := string(sp.Protocol)
				if proto == "" {
					proto = "TCP"
				}
				var backends []podnet.Backend
				for _, s := range eps {
					var port int32 = -1
					for _, p := range s.Ports {
						if p.Name != nil && *p.Name == sp.Name && p.Port != nil {
							port = *p.Port
						}
					}
					if port < 0 {
						continue
					}
					for _, e := range s.Endpoints {
						if !ready(e) {
							continue
						}
						for _, a := range e.Addresses {
							if ip := net.ParseIP(a); ip != nil {
								backends = append(backends, podnet.Backend{IP: ip, Port: uint16(port)})
							}
						}
					}
				}
				k := key{svc.Spec.ClusterIP, sp.Port, proto}
				seen[k] = true
				pn.SetService(vip, uint16(sp.Port), proto, backends)
				var affinity time.Duration
				if svc.Spec.SessionAffinity == corev1.ServiceAffinityClientIP {
					affinity = 3 * time.Hour
					if c := svc.Spec.SessionAffinityConfig; c != nil && c.ClientIP != nil && c.ClientIP.TimeoutSeconds != nil {
						affinity = time.Duration(*c.ClientIP.TimeoutSeconds) * time.Second
					}
				}
				pn.SetAffinity(vip, uint16(sp.Port), proto, affinity)
				programmed[k] = true
				if proto == "TCP" {
					switch svc.Spec.Type {
					case corev1.ServiceTypeLoadBalancer:
						lb.want(int(sp.Port), backends)
					case corev1.ServiceTypeNodePort:
						lb.want(int(sp.NodePort), backends)
					}
				}
			}
		}
		lb.sync()
		for k := range programmed {
			if !seen[k] {
				pn.SetService(net.ParseIP(k.ip), uint16(k.port), k.proto, nil)
				delete(programmed, k)
			}
		}
		klog.V(2).InfoS("services programmed", "count", len(seen))
	}
}

// ClusterDNS is the kube-dns Service VIP pods use as their nameserver.
const ClusterDNS = "10.43.0.10"

func ready(e discoveryv1.Endpoint) bool { return e.Conditions.Ready == nil || *e.Conditions.Ready }

func nudge(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// hostLB is the ServiceLB equivalent: LoadBalancer (service port) and NodePort
// services listen on the host and proxy to ready backends through the pod
// network (pod IPs are not routable from macOS).
type hostLB struct {
	pn        *podnet.Net
	listeners map[int]*lbListener
	pending   map[int][]podnet.Backend
}

type lbListener struct {
	ln       net.Listener
	mu       sync.Mutex
	backends []podnet.Backend
	rr       int
}

func newHostLB(pn *podnet.Net) *hostLB {
	return &hostLB{pn: pn, listeners: map[int]*lbListener{}, pending: map[int][]podnet.Backend{}}
}

func (h *hostLB) want(port int, b []podnet.Backend) {
	if port > 0 {
		h.pending[port] = append(h.pending[port], b...)
	}
}

func (h *hostLB) sync() {
	for port, l := range h.listeners {
		if _, ok := h.pending[port]; !ok {
			l.ln.Close()
			delete(h.listeners, port)
		}
	}
	for port, b := range h.pending {
		l := h.listeners[port]
		if l == nil {
			ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
			if err != nil {
				klog.ErrorS(err, "service load balancer: listen", "port", port)
				continue
			}
			l = &lbListener{ln: ln}
			h.listeners[port] = l
			go h.serve(l)
		}
		l.mu.Lock()
		l.backends = b
		l.mu.Unlock()
	}
	h.pending = map[int][]podnet.Backend{}
}

func (h *hostLB) serve(l *lbListener) {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		l.mu.Lock()
		var b podnet.Backend
		ok := len(l.backends) > 0
		if ok {
			b = l.backends[l.rr%len(l.backends)]
			l.rr++
		}
		l.mu.Unlock()
		if !ok {
			c.Close()
			continue
		}
		go func() {
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			up, err := h.pn.Dial(ctx, net.JoinHostPort(b.IP.String(), strconv.Itoa(int(b.Port))))
			cancel()
			if err != nil {
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(up, c); done <- struct{}{} }()
			go func() { io.Copy(c, up); done <- struct{}{} }()
			<-done
		}()
	}
}
