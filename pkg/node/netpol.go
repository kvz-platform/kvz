package node

import (
	"context"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/GrowlyX/kvz/pkg/podnet"
)

// runNetworkPolicy compiles NetworkPolicy objects into per-pod rule sets for
// the pods on this node and installs them in the pod switch.
func runNetworkPolicy(ctx context.Context, f informers.SharedInformerFactory, nodeName string, pn *podnet.Net) {
	npInf := f.Networking().V1().NetworkPolicies()
	podInf := f.Core().V1().Pods()
	nsInf := f.Core().V1().Namespaces()
	kick := make(chan struct{}, 1)
	h := cache.ResourceEventHandlerFuncs{AddFunc: func(any) { nudge(kick) }, UpdateFunc: func(any, any) { nudge(kick) }, DeleteFunc: func(any) { nudge(kick) }}
	for _, inf := range []cache.SharedIndexInformer{npInf.Informer(), podInf.Informer(), nsInf.Informer()} {
		_, _ = inf.AddEventHandler(h)
		go inf.Run(ctx.Done())
	}
	cache.WaitForCacheSync(ctx.Done(), npInf.Informer().HasSynced, podInf.Informer().HasSynced, nsInf.Informer().HasSynced)
	nudge(kick)
	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
		}
		time.Sleep(20 * time.Millisecond)
		policies, _ := npInf.Lister().List(labels.Everything())
		pods, _ := podInf.Lister().List(labels.Everything())
		nss, _ := nsInf.Lister().List(labels.Everything())
		nsLabels := map[string]labels.Set{}
		for _, ns := range nss {
			nsLabels[ns.Name] = ns.Labels
		}
		compiled := map[string]*podnet.PodPolicy{}
		for _, pod := range pods {
			if pod.Spec.NodeName != nodeName || pod.Status.PodIP == "" || pod.Spec.HostNetwork {
				continue
			}
			var pp *podnet.PodPolicy
			for _, np := range policies {
				if np.Namespace != pod.Namespace {
					continue
				}
				sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
				if err != nil || !sel.Matches(labels.Set(pod.Labels)) {
					continue
				}
				if pp == nil {
					pp = &podnet.PodPolicy{}
				}
				ingress, egress := policyTypes(np)
				if ingress {
					pp.IngressIsolated = true
					for _, r := range np.Spec.Ingress {
						pp.Ingress = append(pp.Ingress, podnet.Rule{
							Peers: compilePeers(np.Namespace, r.From, pods, nsLabels),
							Ports: compilePorts(r.Ports, pod)})
					}
				}
				if egress {
					pp.EgressIsolated = true
					for _, r := range np.Spec.Egress {
						pp.Egress = append(pp.Egress, podnet.Rule{
							Peers: compilePeers(np.Namespace, r.To, pods, nsLabels),
							Ports: compilePorts(r.Ports, nil)})
					}
				}
			}
			if pp != nil {
				compiled[pod.Status.PodIP] = pp
			}
		}
		pn.SetPolicies(compiled)
		klog.V(2).InfoS("network policies programmed", "pods", len(compiled))
	}
}

func policyTypes(np *networkingv1.NetworkPolicy) (ingress, egress bool) {
	if len(np.Spec.PolicyTypes) == 0 {
		return true, len(np.Spec.Egress) > 0
	}
	for _, t := range np.Spec.PolicyTypes {
		switch t {
		case networkingv1.PolicyTypeIngress:
			ingress = true
		case networkingv1.PolicyTypeEgress:
			egress = true
		}
	}
	return
}

func compilePeers(ns string, peers []networkingv1.NetworkPolicyPeer, pods []*corev1.Pod, nsLabels map[string]labels.Set) podnet.PeerRule {
	if len(peers) == 0 {
		return podnet.PeerRule{Any: true}
	}
	var out podnet.PeerRule
	host := func(ip string) {
		if p := net.ParseIP(ip); p != nil && p.To4() != nil {
			out.CIDRs = append(out.CIDRs, &net.IPNet{IP: p.To4(), Mask: net.CIDRMask(32, 32)})
		}
	}
	for _, peer := range peers {
		if peer.IPBlock != nil {
			if _, c, err := net.ParseCIDR(peer.IPBlock.CIDR); err == nil {
				out.CIDRs = append(out.CIDRs, c)
			}
			for _, e := range peer.IPBlock.Except {
				if _, c, err := net.ParseCIDR(e); err == nil {
					out.Except = append(out.Except, c)
				}
			}
			continue
		}
		var podSel, nsSel labels.Selector
		if peer.PodSelector != nil {
			podSel, _ = metav1.LabelSelectorAsSelector(peer.PodSelector)
		}
		if peer.NamespaceSelector != nil {
			nsSel, _ = metav1.LabelSelectorAsSelector(peer.NamespaceSelector)
		}
		for _, p := range pods {
			if p.Status.PodIP == "" {
				continue
			}
			inNS := p.Namespace == ns
			if nsSel != nil {
				inNS = nsSel.Matches(nsLabels[p.Namespace])
			}
			if !inNS {
				continue
			}
			if podSel != nil && !podSel.Matches(labels.Set(p.Labels)) {
				continue
			}
			host(p.Status.PodIP)
		}
	}
	return out
}

func compilePorts(ports []networkingv1.NetworkPolicyPort, dst *corev1.Pod) []podnet.PortRule {
	var out []podnet.PortRule
	for _, p := range ports {
		r := podnet.PortRule{Proto: 6}
		if p.Protocol != nil {
			switch *p.Protocol {
			case corev1.ProtocolUDP:
				r.Proto = 17
			case corev1.ProtocolSCTP:
				r.Proto = 132
			}
		}
		if p.Port == nil {
			r.Any = true
		} else if p.Port.Type == 0 { // int
			r.Port = uint16(p.Port.IntVal)
			if p.EndPort != nil {
				r.EndPort = uint16(*p.EndPort)
			}
		} else if dst != nil { // named port of the destination pod
			for _, c := range dst.Spec.Containers {
				for _, cp := range c.Ports {
					if cp.Name == p.Port.StrVal {
						r.Port = uint16(cp.ContainerPort)
					}
				}
			}
			if r.Port == 0 {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}
