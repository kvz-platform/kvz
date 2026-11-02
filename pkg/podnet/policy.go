package podnet

import (
	"encoding/binary"
	"net"
	"sync"
	"time"
)

// NetworkPolicy enforcement in the switch. The node's controller compiles
// NetworkPolicy objects into per-pod rule sets (peer CIDRs + ports); the switch
// checks the first packet of every flow and remembers allowed flows (and their
// replies), so policies are stateful like those of CNI plugins.

type PortRule struct {
	Proto   uint8 // 6 tcp, 17 udp, 132 sctp; 0 = any
	Port    uint16
	EndPort uint16 // inclusive range end (0 = single port)
	Any     bool   // rule has no port restriction
}

type PeerRule struct {
	CIDRs  []*net.IPNet
	Except []*net.IPNet
	Any    bool // empty from/to: every peer
}

type Rule struct {
	Peers PeerRule
	Ports []PortRule // empty = all ports
}

// PodPolicy is the compiled policy of one local pod.
type PodPolicy struct {
	IngressIsolated, EgressIsolated bool
	Ingress, Egress                 []Rule
}

type flowKey struct {
	proto        uint8
	sip, dip     [4]byte
	sport, dport uint16
}

type policyTable struct {
	mu     sync.RWMutex
	pods   map[[4]byte]*PodPolicy
	flowMu sync.Mutex
	flows  map[flowKey]int64
	gw     [4]byte
}

func newPolicyTable(gw net.IP) *policyTable {
	t := &policyTable{pods: map[[4]byte]*PodPolicy{}, flows: map[flowKey]int64{}}
	copy(t.gw[:], gw.To4())
	go func() {
		for range time.Tick(30 * time.Second) {
			cut := time.Now().Add(-10 * time.Minute).UnixNano()
			t.flowMu.Lock()
			for k, v := range t.flows {
				if v < cut {
					delete(t.flows, k)
				}
			}
			t.flowMu.Unlock()
		}
	}()
	return t
}

func (t *policyTable) set(p map[[4]byte]*PodPolicy) {
	t.mu.Lock()
	t.pods = p
	t.mu.Unlock()
	t.flowMu.Lock()
	t.flows = map[flowKey]int64{} // re-evaluate existing flows under the new rules
	t.flowMu.Unlock()
}

func (r PeerRule) match(ip net.IP) bool {
	if r.Any {
		return true
	}
	for _, c := range r.CIDRs {
		if c.Contains(ip) {
			for _, e := range r.Except {
				if e.Contains(ip) {
					return false
				}
			}
			return true
		}
	}
	return false
}

func portsMatch(ports []PortRule, proto uint8, port uint16) bool {
	if len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		if p.Proto != 0 && p.Proto != proto {
			continue
		}
		if p.Any || p.Port == 0 {
			return true
		}
		if port == p.Port || (p.EndPort != 0 && port >= p.Port && port <= p.EndPort) {
			return true
		}
	}
	return false
}

func allowedBy(rules []Rule, peer net.IP, proto uint8, port uint16) bool {
	for _, r := range rules {
		if r.Peers.match(peer) && portsMatch(r.Ports, proto, port) {
			return true
		}
	}
	return false
}

// check decides whether an IPv4 frame may pass. egress/ingress select which
// side(s) this node enforces (the source node checks egress, the destination
// node checks ingress).
func (t *policyTable) check(frame []byte, egress, ingress bool) bool {
	if len(frame) < ethHdr+20 || frame[12] != 0x08 || frame[13] != 0x00 {
		return true
	}
	t.mu.RLock()
	empty := len(t.pods) == 0
	t.mu.RUnlock()
	if empty {
		return true
	}
	ip := frame[ethHdr:]
	ihl := int(ip[0]&0x0f) * 4
	var k flowKey
	k.proto = ip[9]
	copy(k.sip[:], ip[12:16])
	copy(k.dip[:], ip[16:20])
	if (k.proto == 6 || k.proto == 17 || k.proto == 132) && len(ip) >= ihl+4 {
		k.sport = binary.BigEndian.Uint16(ip[ihl : ihl+2])
		k.dport = binary.BigEndian.Uint16(ip[ihl+2 : ihl+4])
	}
	now := time.Now().UnixNano()
	t.flowMu.Lock()
	if _, ok := t.flows[k]; ok {
		t.flows[k] = now
		t.flowMu.Unlock()
		return true
	}
	t.flowMu.Unlock()
	t.mu.RLock()
	src, dst := t.pods[k.sip], t.pods[k.dip]
	t.mu.RUnlock()
	ok := true
	if egress && src != nil && src.EgressIsolated {
		toGatewayDNS := k.dip == t.gw && k.dport == 53
		ok = toGatewayDNS || allowedBy(src.Egress, net.IP(k.dip[:]), k.proto, k.dport)
	}
	if ok && ingress && dst != nil && dst.IngressIsolated {
		fromGateway := k.sip == t.gw // host, LoadBalancer, port-forward traffic
		ok = fromGateway || allowedBy(dst.Ingress, net.IP(k.sip[:]), k.proto, k.dport)
	}
	if ok {
		rev := flowKey{proto: k.proto, sip: k.dip, dip: k.sip, sport: k.dport, dport: k.sport}
		t.flowMu.Lock()
		t.flows[k] = now
		t.flows[rev] = now
		t.flowMu.Unlock()
	}
	return ok
}
