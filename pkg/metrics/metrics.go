// Package metrics is kvz's built-in metrics.k8s.io API (the metrics-server
// equivalent): it scrapes every node's kubelet /stats/summary, derives usage
// rates, and serves NodeMetrics/PodMetrics as an aggregated API, so
// `kubectl top` and the HorizontalPodAutoscaler work out of the box.
package metrics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	apiregv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	aggclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	mv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
)

type sample struct {
	t   time.Time
	cpu uint64
	mem uint64
}

type usage struct {
	ts     time.Time
	window time.Duration
	cpu    int64 // nanocores
	mem    int64
}

type Server struct {
	cs      kubernetes.Interface
	client  *http.Client
	mu      sync.RWMutex
	prev    map[string]sample // key -> previous cumulative sample
	nodes   map[string]usage
	pods    map[string]map[string]usage // ns/name -> container -> usage
	podInfo map[string]metav1.ObjectMeta
}

func New(cs kubernetes.Interface, caFile, certFile, keyFile string) (*Server, error) {
	pool := x509.NewCertPool()
	if b, err := os.ReadFile(caFile); err == nil {
		pool.AppendCertsFromPEM(b)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &Server{cs: cs, client: &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true}}},
		prev: map[string]sample{}, nodes: map[string]usage{}, pods: map[string]map[string]usage{}, podInfo: map[string]metav1.ObjectMeta{}}, nil
}

func (s *Server) rate(key string, cur sample) (usage, bool) {
	p, ok := s.prev[key]
	s.prev[key] = cur
	if !ok || !cur.t.After(p.t) || cur.cpu < p.cpu {
		return usage{}, false
	}
	dt := cur.t.Sub(p.t)
	return usage{ts: cur.t, window: dt, cpu: int64(float64(cur.cpu-p.cpu) / dt.Seconds()), mem: int64(cur.mem)}, true
}

// Scrape polls every node once.
func (s *Server) Scrape(ctx context.Context) {
	nodes, err := s.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	pods, _ := s.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	meta := map[string]metav1.ObjectMeta{}
	if pods != nil {
		for _, p := range pods.Items {
			meta[p.Namespace+"/"+p.Name] = p.ObjectMeta
		}
	}
	var wg sync.WaitGroup
	for _, n := range nodes.Items {
		addr := ""
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				addr = a.Address
			}
		}
		port := n.Status.DaemonEndpoints.KubeletEndpoint.Port
		if addr == "" || port == 0 {
			continue
		}
		wg.Add(1)
		go func(name, url string) {
			defer wg.Done()
			resp, err := s.client.Get(url)
			if err != nil {
				klog.V(2).InfoS("metrics: scrape failed", "node", name, "err", err)
				return
			}
			defer resp.Body.Close()
			var sum statsv1alpha1.Summary
			if json.NewDecoder(resp.Body).Decode(&sum) != nil {
				return
			}
			now := time.Now()
			s.mu.Lock()
			defer s.mu.Unlock()
			if c, m := sum.Node.CPU, sum.Node.Memory; c != nil && c.UsageCoreNanoSeconds != nil && m != nil && m.WorkingSetBytes != nil {
				if u, ok := s.rate("node/"+name, sample{now, *c.UsageCoreNanoSeconds, *m.WorkingSetBytes}); ok {
					s.nodes[name] = u
				}
			}
			for _, p := range sum.Pods {
				key := p.PodRef.Namespace + "/" + p.PodRef.Name
				cu := map[string]usage{}
				for _, c := range p.Containers {
					if c.CPU == nil || c.CPU.UsageCoreNanoSeconds == nil || c.Memory == nil || c.Memory.WorkingSetBytes == nil {
						continue
					}
					if u, ok := s.rate("pod/"+key+"/"+c.Name+"/"+p.PodRef.UID, sample{now, *c.CPU.UsageCoreNanoSeconds, *c.Memory.WorkingSetBytes}); ok {
						cu[c.Name] = u
					}
				}
				if len(cu) > 0 {
					s.pods[key] = cu
				}
			}
		}(n.Name, fmt.Sprintf("https://%s/stats/summary", net.JoinHostPort(addr, fmt.Sprint(port))))
	}
	wg.Wait()
	s.mu.Lock()
	for k := range s.pods {
		if _, ok := meta[k]; !ok {
			delete(s.pods, k)
		}
	}
	s.podInfo = meta
	s.mu.Unlock()
}

func q(cpu, mem int64) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewScaledQuantity(cpu, resource.Nano),
		corev1.ResourceMemory: *resource.NewQuantity(mem, resource.BinarySI),
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"}, GroupVersion: "metrics.k8s.io/v1beta1",
			APIResources: []metav1.APIResource{
				{Name: "nodes", Kind: "NodeMetrics", Verbs: []string{"get", "list"}},
				{Name: "pods", Namespaced: true, Kind: "PodMetrics", Verbs: []string{"get", "list"}},
			}})
	})
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/nodes", s.nodesHandler)
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/nodes/", s.nodesHandler)
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/pods", s.podsHandler)
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/namespaces/", s.podsHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	return mux
}

func (s *Server) nodesHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/apis/metrics.k8s.io/v1beta1/nodes"), "/")
	s.mu.RLock()
	defer s.mu.RUnlock()
	mk := func(n string, u usage) mv1beta1.NodeMetrics {
		return mv1beta1.NodeMetrics{TypeMeta: metav1.TypeMeta{Kind: "NodeMetrics", APIVersion: "metrics.k8s.io/v1beta1"},
			ObjectMeta: metav1.ObjectMeta{Name: n, CreationTimestamp: metav1.Now()},
			Timestamp:  metav1.NewTime(u.ts), Window: metav1.Duration{Duration: u.window}, Usage: q(u.cpu, u.mem)}
	}
	if name != "" {
		u, ok := s.nodes[name]
		if !ok {
			writeErr(w, apierrors.NewNotFound(mv1beta1.Resource("nodes"), name))
			return
		}
		writeJSON(w, mk(name, u))
		return
	}
	list := mv1beta1.NodeMetricsList{TypeMeta: metav1.TypeMeta{Kind: "NodeMetricsList", APIVersion: "metrics.k8s.io/v1beta1"}}
	for n, u := range s.nodes {
		list.Items = append(list.Items, mk(n, u))
	}
	writeJSON(w, list)
}

func (s *Server) podsHandler(w http.ResponseWriter, r *http.Request) {
	ns, name := "", ""
	if rest, ok := strings.CutPrefix(r.URL.Path, "/apis/metrics.k8s.io/v1beta1/namespaces/"); ok {
		parts := strings.Split(rest, "/")
		if len(parts) < 2 || parts[1] != "pods" {
			http.NotFound(w, r)
			return
		}
		ns = parts[0]
		if len(parts) > 2 {
			name = parts[2]
		}
	}
	sel, err := metav1.ParseToLabelSelector(r.URL.Query().Get("labelSelector"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	selector, _ := metav1.LabelSelectorAsSelector(sel)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var items []mv1beta1.PodMetrics
	for key, cu := range s.pods {
		pns, pname, _ := strings.Cut(key, "/")
		if ns != "" && pns != ns || name != "" && pname != name {
			continue
		}
		meta := s.podInfo[key]
		if selector != nil && !selector.Empty() && !selector.Matches(labels.Set(meta.Labels)) {
			continue
		}
		pm := mv1beta1.PodMetrics{TypeMeta: metav1.TypeMeta{Kind: "PodMetrics", APIVersion: "metrics.k8s.io/v1beta1"},
			ObjectMeta: metav1.ObjectMeta{Name: pname, Namespace: pns, Labels: meta.Labels, CreationTimestamp: metav1.Now()}}
		for c, u := range cu {
			pm.Timestamp, pm.Window = metav1.NewTime(u.ts), metav1.Duration{Duration: u.window}
			pm.Containers = append(pm.Containers, mv1beta1.ContainerMetrics{Name: c, Usage: q(u.cpu, u.mem)})
		}
		items = append(items, pm)
	}
	if name != "" {
		if len(items) == 0 {
			writeErr(w, apierrors.NewNotFound(mv1beta1.Resource("pods"), name))
			return
		}
		writeJSON(w, items[0])
		return
	}
	writeJSON(w, mv1beta1.PodMetricsList{TypeMeta: metav1.TypeMeta{Kind: "PodMetricsList", APIVersion: "metrics.k8s.io/v1beta1"}, Items: items})
}

func writeErr(w http.ResponseWriter, err *apierrors.StatusError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(int(err.ErrStatus.Code))
	_ = json.NewEncoder(w).Encode(err.ErrStatus)
}

// Run serves the API on addr (TLS, client certs from the cluster CA, i.e. the
// aggregator's front-proxy identity) and scrapes every 10 s.
func (s *Server) Run(ctx context.Context, addr, caFile, certFile, keyFile string) error {
	go func() {
		for {
			s.Scrape(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
	}()
	pool := x509.NewCertPool()
	if b, err := os.ReadFile(caFile); err == nil {
		pool.AppendCertsFromPEM(b)
	}
	srv := &http.Server{Addr: addr, Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}}
	go func() { <-ctx.Done(); srv.Close() }()
	return srv.ListenAndServeTLS(certFile, keyFile)
}

// Register points the aggregated metrics.k8s.io API at this server.
func Register(ctx context.Context, cs kubernetes.Interface, agg aggclient.Interface, hostIP string, port int) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kvz-metrics", Namespace: "kube-system"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "https", Port: 443, TargetPort: intstr.FromInt32(int32(port))}}}}
	if _, err := cs.CoreV1().Services("kube-system").Create(ctx, svc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	ep := &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "kvz-metrics", Namespace: "kube-system"},
		Subsets: []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: hostIP}}, Ports: []corev1.EndpointPort{{Name: "https", Port: int32(port)}}}}}
	if _, err := cs.CoreV1().Endpoints("kube-system").Create(ctx, ep, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		_, err = cs.CoreV1().Endpoints("kube-system").Update(ctx, ep, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	svcPort := int32(443)
	as := &apiregv1.APIService{ObjectMeta: metav1.ObjectMeta{Name: "v1beta1.metrics.k8s.io"},
		Spec: apiregv1.APIServiceSpec{Service: &apiregv1.ServiceReference{Namespace: "kube-system", Name: "kvz-metrics", Port: &svcPort},
			Group: "metrics.k8s.io", Version: "v1beta1", InsecureSkipTLSVerify: true, GroupPriorityMinimum: 100, VersionPriority: 100}}
	if _, err := agg.ApiregistrationV1().APIServices().Create(ctx, as, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
