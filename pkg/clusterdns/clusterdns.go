// Package clusterdns is kvz's cluster DNS server (the CoreDNS "kubernetes"
// plugin, built in). It answers cluster.local authoritatively from Services,
// EndpointSlices and pod IPs, and forwards everything else to macOS.
//
// UDP queries never touch a TCP/IP stack: the pod switch intercepts packets to
// the kube-dns VIP and calls Answer. TCP queries reach a listener in the node's
// netstack.
package clusterdns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
)

const (
	Zone = "cluster.local."
	ttl  = 5
)

type srvTarget struct {
	target string
	port   uint16
}

// table is an immutable snapshot of cluster records.
type table struct {
	a     map[string][]net.IP // fqdn -> IPv4s
	cname map[string]string   // ExternalName services
	srv   map[string][]srvTarget
	ptr   map[string]string // reverse name -> fqdn
	names map[string]bool   // every existing name and empty non-terminal
}

type Server struct {
	mu        sync.RWMutex
	t         *table
	podCIDRs  []netip.Prefix
	upstreams []string
	resolver  *net.Resolver
}

func New() *Server {
	s := &Server{t: &table{}, resolver: net.DefaultResolver}
	s.upstreams = hostNameservers()
	return s
}

// hostNameservers reads macOS's primary resolvers (used for non-A/AAAA types;
// A/AAAA go through the system resolver so split DNS keeps working).
func hostNameservers() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return []string{"1.1.1.1:53"}
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && f[0] == "nameserver" {
			out = append(out, net.JoinHostPort(f[1], "53"))
		}
	}
	if len(out) == 0 {
		out = []string{"1.1.1.1:53"}
	}
	return out
}

// SetPodCIDRs marks reverse lookups for pod networks as cluster-local.
func (s *Server) SetPodCIDRs(cidrs []string) {
	var ps []netip.Prefix
	for _, c := range cidrs {
		if p, err := netip.ParsePrefix(c); err == nil {
			ps = append(ps, p)
		}
	}
	s.mu.Lock()
	s.podCIDRs = ps
	s.mu.Unlock()
}

func dashed(ip string) string { return strings.ReplaceAll(ip, ".", "-") }

// Sync rebuilds the record table.
func (s *Server) Sync(svcs []*corev1.Service, slices []*discoveryv1.EndpointSlice) {
	t := &table{a: map[string][]net.IP{}, cname: map[string]string{}, srv: map[string][]srvTarget{}, ptr: map[string]string{}, names: map[string]bool{}}
	bySvc := map[string][]*discoveryv1.EndpointSlice{}
	for _, sl := range slices {
		if n := sl.Labels[discoveryv1.LabelServiceName]; n != "" {
			bySvc[sl.Namespace+"/"+n] = append(bySvc[sl.Namespace+"/"+n], sl)
		}
	}
	addA := func(name string, ip net.IP) {
		if ip = ip.To4(); ip == nil {
			return
		}
		for _, e := range t.a[name] {
			if e.Equal(ip) {
				return
			}
		}
		t.a[name] = append(t.a[name], ip)
	}
	for _, svc := range svcs {
		base := svc.Name + "." + svc.Namespace + ".svc." + Zone
		switch {
		case svc.Spec.Type == corev1.ServiceTypeExternalName:
			t.cname[base] = dns.Fqdn(svc.Spec.ExternalName)
			continue
		case svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != corev1.ClusterIPNone:
			for _, ip := range svc.Spec.ClusterIPs {
				addA(base, net.ParseIP(ip))
				if r, err := dns.ReverseAddr(ip); err == nil {
					t.ptr[r] = base
				}
			}
			t.names[base] = true
			for _, p := range svc.Spec.Ports {
				if p.Name == "" {
					continue
				}
				n := fmt.Sprintf("_%s._%s.%s", p.Name, strings.ToLower(string(p.Protocol)), base)
				t.srv[n] = append(t.srv[n], srvTarget{base, uint16(p.Port)})
			}
		default: // headless: ready endpoints
			t.names[base] = true
			for _, sl := range bySvc[svc.Namespace+"/"+svc.Name] {
				for _, ep := range sl.Endpoints {
					ready := ep.Conditions.Ready == nil || *ep.Conditions.Ready
					if !ready && !svc.Spec.PublishNotReadyAddresses {
						continue
					}
					for _, a := range ep.Addresses {
						ip := net.ParseIP(a)
						addA(base, ip)
						host := dashed(a)
						if ep.Hostname != nil && *ep.Hostname != "" {
							host = *ep.Hostname
						}
						fq := host + "." + base
						addA(fq, ip)
						if r, err := dns.ReverseAddr(a); err == nil {
							t.ptr[r] = fq
						}
						for _, p := range sl.Ports {
							if p.Name == nil || *p.Name == "" || p.Port == nil {
								continue
							}
							proto := "tcp"
							if p.Protocol != nil {
								proto = strings.ToLower(string(*p.Protocol))
							}
							n := fmt.Sprintf("_%s._%s.%s", *p.Name, proto, base)
							t.srv[n] = append(t.srv[n], srvTarget{fq, uint16(*p.Port)})
						}
					}
				}
			}
		}
	}
	// every name and its ancestors (empty non-terminals) exist
	mark := func(n string) {
		for n != Zone && strings.HasSuffix(n, Zone) {
			t.names[n] = true
			i := strings.IndexByte(n, '.')
			if i < 0 {
				return
			}
			n = n[i+1:]
		}
	}
	for n := range t.a {
		mark(n)
	}
	for n := range t.cname {
		mark(n)
	}
	for n := range t.srv {
		mark(n)
	}
	for n := range t.names {
		mark(n)
	}
	for _, v := range t.srv {
		sort.Slice(v, func(i, j int) bool { return v[i].target < v[j].target })
	}
	s.mu.Lock()
	s.t = t
	s.mu.Unlock()
}

func hdr(name string, typ uint16) dns.RR_Header {
	return dns.RR_Header{Name: name, Rrtype: typ, Class: dns.ClassINET, Ttl: ttl}
}

func soa() dns.RR {
	return &dns.SOA{Hdr: hdr(Zone, dns.TypeSOA), Ns: "ns.dns." + Zone, Mbox: "hostmaster." + Zone,
		Serial: uint32(time.Now().Unix()), Refresh: 7200, Retry: 1800, Expire: 86400, Minttl: ttl}
}

// podName answers a-b-c-d.<ns>.pod.cluster.local (CoreDNS "pods insecure").
func podName(name string) (net.IP, bool) {
	rest, ok := strings.CutSuffix(name, ".pod."+Zone)
	if !ok {
		return nil, false
	}
	i := strings.IndexByte(rest, '.')
	if i < 0 {
		return nil, false
	}
	ip := net.ParseIP(strings.ReplaceAll(rest[:i], "-", ".")).To4()
	return ip, ip != nil
}

func (s *Server) inPodCIDR(rev string) bool {
	ip := ptrIP(rev)
	if !ip.IsValid() {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.podCIDRs {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func ptrIP(rev string) netip.Addr {
	r, ok := strings.CutSuffix(rev, ".in-addr.arpa.")
	if !ok {
		return netip.Addr{}
	}
	f := strings.Split(r, ".")
	if len(f) != 4 {
		return netip.Addr{}
	}
	a, err := netip.ParseAddr(f[3] + "." + f[2] + "." + f[1] + "." + f[0])
	if err != nil {
		return netip.Addr{}
	}
	return a
}

// Answer resolves one query (blocking for forwarded names).
func (s *Server) Answer(ctx context.Context, req *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(req)
	m.RecursionAvailable = true
	if len(req.Question) != 1 {
		m.Rcode = dns.RcodeFormatError
		return m
	}
	q := req.Question[0]
	name := strings.ToLower(q.Name)
	s.mu.RLock()
	t := s.t
	s.mu.RUnlock()

	if strings.HasSuffix(name, "."+Zone) || name == Zone {
		m.Authoritative = true
		if ip, ok := podName(name); ok {
			if q.Qtype == dns.TypeA || q.Qtype == dns.TypeANY {
				m.Answer = append(m.Answer, &dns.A{Hdr: hdr(q.Name, dns.TypeA), A: ip})
			} else {
				m.Ns = append(m.Ns, soa())
			}
			return m
		}
		if c, ok := t.cname[name]; ok {
			m.Answer = append(m.Answer, &dns.CNAME{Hdr: hdr(q.Name, dns.TypeCNAME), Target: c})
			// stub resolvers don't chase CNAMEs: include the target's records
			if q.Qtype == dns.TypeA || q.Qtype == dns.TypeAAAA {
				sub := new(dns.Msg)
				sub.SetQuestion(c, q.Qtype)
				if r := s.Answer(ctx, sub); r != nil && r.Rcode == dns.RcodeSuccess {
					m.Answer = append(m.Answer, r.Answer...)
				}
			}
			return m
		}
		switch q.Qtype {
		case dns.TypeA, dns.TypeANY:
			for _, ip := range t.a[name] {
				m.Answer = append(m.Answer, &dns.A{Hdr: hdr(q.Name, dns.TypeA), A: ip})
			}
		case dns.TypeSRV:
			for _, st := range t.srv[name] {
				m.Answer = append(m.Answer, &dns.SRV{Hdr: hdr(q.Name, dns.TypeSRV), Priority: 0, Weight: 100, Port: st.port, Target: st.target})
				for _, ip := range t.a[st.target] {
					m.Extra = append(m.Extra, &dns.A{Hdr: hdr(st.target, dns.TypeA), A: ip})
				}
			}
		}
		if len(m.Answer) == 0 {
			if !t.names[name] {
				m.Rcode = dns.RcodeNameError
			}
			m.Ns = append(m.Ns, soa())
		}
		return m
	}
	if q.Qtype == dns.TypePTR && strings.HasSuffix(name, ".in-addr.arpa.") {
		if fq, ok := t.ptr[name]; ok {
			m.Authoritative = true
			m.Answer = append(m.Answer, &dns.PTR{Hdr: hdr(q.Name, dns.TypePTR), Ptr: fq})
			return m
		}
		if ip := ptrIP(name); ip.IsValid() && s.inPodCIDR(name) {
			m.Authoritative = true
			m.Answer = append(m.Answer, &dns.PTR{Hdr: hdr(q.Name, dns.TypePTR), Ptr: dashed(ip.String()) + ".pod." + Zone})
			return m
		}
	}
	return s.forward(ctx, req, q, m)
}

// forward resolves names outside the cluster: A/AAAA through the macOS system
// resolver (split DNS, VPNs, /etc/hosts), other types raw to its nameservers.
func (s *Server) forward(ctx context.Context, req *dns.Msg, q dns.Question, m *dns.Msg) *dns.Msg {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if q.Qtype == dns.TypeA || q.Qtype == dns.TypeAAAA {
		network := "ip4"
		if q.Qtype == dns.TypeAAAA {
			network = "ip6"
		}
		ips, err := s.resolver.LookupNetIP(ctx, network, strings.TrimSuffix(q.Name, "."))
		var de *net.DNSError
		switch {
		case err == nil:
			for _, ip := range ips {
				if q.Qtype == dns.TypeA && ip.Is4() {
					m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: ip.AsSlice()})
				} else if q.Qtype == dns.TypeAAAA && ip.Is6() && !ip.Is4In6() {
					m.Answer = append(m.Answer, &dns.AAAA{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 30}, AAAA: ip.AsSlice()})
				}
			}
			return m
		case errors.As(err, &de) && de.IsNotFound:
			// The system resolver reports "no such host" both for NXDOMAIN and
			// for names without this record type; ask raw to tell them apart.
		default:
			m.Rcode = dns.RcodeServerFailure
			return m
		}
	}
	c := &dns.Client{Timeout: 2 * time.Second}
	for _, up := range s.upstreams {
		r, _, err := c.ExchangeContext(ctx, req, up)
		if err == nil && r != nil && r.Truncated {
			tc := &dns.Client{Net: "tcp", Timeout: 3 * time.Second}
			if r2, _, err2 := tc.ExchangeContext(ctx, req, up); err2 == nil {
				r = r2
			}
		}
		if err == nil && r != nil {
			r.Id = req.Id
			return r
		}
	}
	m.Rcode = dns.RcodeServerFailure
	return m
}
