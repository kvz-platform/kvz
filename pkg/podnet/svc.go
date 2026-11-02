package podnet

import (
	"encoding/binary"
	"hash/fnv"
	"net"
	"sync"
	"time"
)

// Service load balancing in the switch (kube-proxy equivalent): frames to a
// ClusterIP:port are DNATed to a backend and replies are reverse-NATed, with a
// small conntrack table. Works for TCP and UDP over IPv4.

type Backend struct {
	IP   net.IP
	Port uint16
}

type svcKey struct {
	ip    [4]byte
	port  uint16
	proto uint8
}

type ctKey struct {
	proto        uint8
	sip, dip     [4]byte
	sport, dport uint16
}

type ctEntry struct {
	// original destination (service) and chosen backend
	vip   [4]byte
	vport uint16
	bip   [4]byte
	bport uint16
	bmac  [6]byte
	viaUp bool
	last  int64
}

type affKey struct {
	svc    svcKey
	client [4]byte
}

type affEntry struct {
	b    Backend
	last int64
}

type natTable struct {
	mu   sync.RWMutex
	svcs map[svcKey][]Backend
	// sessionAffinity: ClientIP (timeout per service, sticky choices)
	affTimeout map[svcKey]int64 // nanoseconds
	aff        map[affKey]*affEntry
	fwd        map[ctKey]*ctEntry // client -> service (key: original tuple)
	rev        map[ctKey]*ctEntry // backend -> client (key: reply tuple as seen from backend)
	cidr       *net.IPNet
	gwMAC      [6]byte
}

func newNATTable(serviceCIDR string, gwMAC net.HardwareAddr) *natTable {
	_, c, _ := net.ParseCIDR(serviceCIDR)
	t := &natTable{svcs: map[svcKey][]Backend{}, fwd: map[ctKey]*ctEntry{}, rev: map[ctKey]*ctEntry{}, cidr: c,
		affTimeout: map[svcKey]int64{}, aff: map[affKey]*affEntry{}}
	copy(t.gwMAC[:], gwMAC)
	go t.gc()
	return t
}

func ip4(ip net.IP) (a [4]byte) { copy(a[:], ip.To4()); return }

// SetService replaces the backends of vip:port/proto (proto 6=tcp, 17=udp).
func (t *natTable) SetService(vip net.IP, port uint16, proto uint8, backends []Backend) {
	k := svcKey{ip4(vip), port, proto}
	t.mu.Lock()
	if len(backends) == 0 {
		delete(t.svcs, k)
	} else {
		t.svcs[k] = backends
	}
	t.mu.Unlock()
}

// SetAffinity enables ClientIP session affinity with the given timeout (0 disables).
func (t *natTable) SetAffinity(vip net.IP, port uint16, proto uint8, timeout time.Duration) {
	k := svcKey{ip4(vip), port, proto}
	t.mu.Lock()
	if timeout <= 0 {
		delete(t.affTimeout, k)
		for ak := range t.aff {
			if ak.svc == k {
				delete(t.aff, ak)
			}
		}
	} else {
		t.affTimeout[k] = int64(timeout)
	}
	t.mu.Unlock()
}

// pickBackend chooses a backend for a new flow: sticky per client IP when the
// service has session affinity, otherwise spread by client IP and port.
func (t *natTable) pickBackend(sk svcKey, backends []Backend, sip [4]byte, sport uint16) Backend {
	now := time.Now().UnixNano()
	t.mu.Lock()
	defer t.mu.Unlock()
	if to, ok := t.affTimeout[sk]; ok {
		ak := affKey{sk, sip}
		if e, ok := t.aff[ak]; ok && now-e.last < to {
			for _, b := range backends {
				if b.Port == e.b.Port && b.IP.Equal(e.b.IP) {
					e.last = now
					return b
				}
			}
		}
		b := backends[int(now/1000)%len(backends)]
		t.aff[ak] = &affEntry{b: b, last: now}
		return b
	}
	h := fnv.New32a()
	h.Write(sip[:])
	h.Write([]byte{byte(sport >> 8), byte(sport)})
	return backends[int(h.Sum32())%len(backends)]
}

func (t *natTable) gc() {
	for range time.Tick(30 * time.Second) {
		cut := time.Now().Add(-5 * time.Minute).UnixNano()
		t.mu.Lock()
		for k, e := range t.fwd {
			if e.last < cut {
				delete(t.fwd, k)
			}
		}
		for k, e := range t.rev {
			if e.last < cut {
				delete(t.rev, k)
			}
		}
		t.mu.Unlock()
	}
}

// frame layout helpers
const ethHdr = 14

func parse(frame []byte) (ihl int, proto uint8, ok bool) {
	if len(frame) < ethHdr+20 || binary.BigEndian.Uint16(frame[12:14]) != 0x0800 {
		return 0, 0, false
	}
	ip := frame[ethHdr:]
	if ip[0]>>4 != 4 {
		return 0, 0, false
	}
	ihl = int(ip[0]&0x0f) * 4
	proto = ip[9]
	if (proto != 6 && proto != 17) || len(ip) < ihl+8 {
		return 0, 0, false
	}
	// fragments other than the first carry no ports
	if binary.BigEndian.Uint16(ip[6:8])&0x1fff != 0 {
		return 0, 0, false
	}
	return ihl, proto, true
}

// csum16 incremental update (RFC 1624): HC' = ~(~HC + ~m + m')
func csumUpdate(sum uint16, old, new []byte) uint16 {
	s := uint32(^sum)
	for i := 0; i+1 < len(old); i += 2 {
		s += uint32(^binary.BigEndian.Uint16(old[i:]))
		s += uint32(binary.BigEndian.Uint16(new[i:]))
	}
	for s>>16 != 0 {
		s = (s & 0xffff) + (s >> 16)
	}
	return ^uint16(s)
}

func rewriteAddr(frame []byte, ihl int, proto uint8, dst bool, newIP [4]byte, newPort uint16) {
	ip := frame[ethHdr:]
	l4 := ip[ihl:]
	ipOff, portOff := 12, 0 // src
	if dst {
		ipOff, portOff = 16, 2
	}
	oldIP := append([]byte(nil), ip[ipOff:ipOff+4]...)
	oldPort := append([]byte(nil), l4[portOff:portOff+2]...)
	var np [2]byte
	binary.BigEndian.PutUint16(np[:], newPort)
	// IPv4 header checksum
	binary.BigEndian.PutUint16(ip[10:12], csumUpdate(binary.BigEndian.Uint16(ip[10:12]), oldIP, newIP[:]))
	copy(ip[ipOff:ipOff+4], newIP[:])
	// L4 checksum (pseudo-header IP + port)
	csOff := 16 // tcp
	if proto == 17 {
		csOff = 6
	}
	if len(l4) >= csOff+2 {
		cs := binary.BigEndian.Uint16(l4[csOff : csOff+2])
		if !(proto == 17 && cs == 0) { // UDP checksum 0 = none
			cs = csumUpdate(cs, oldIP, newIP[:])
			cs = csumUpdate(cs, oldPort, np[:])
			binary.BigEndian.PutUint16(l4[csOff:csOff+2], cs)
		}
	}
	copy(l4[portOff:portOff+2], np[:])
}

func tuple(frame []byte, ihl int, proto uint8) ctKey {
	ip := frame[ethHdr:]
	l4 := ip[ihl:]
	var k ctKey
	k.proto = proto
	copy(k.sip[:], ip[12:16])
	copy(k.dip[:], ip[16:20])
	k.sport = binary.BigEndian.Uint16(l4[0:2])
	k.dport = binary.BigEndian.Uint16(l4[2:4])
	return k
}

// outbound handles a frame from a pod. It returns (rewritten, dstMAC, viaUplink).
// rewritten=false means the frame is not service traffic.
func (t *natTable) outbound(frame []byte, macOf func([4]byte) ([6]byte, bool)) (bool, [6]byte, bool) {
	ihl, proto, ok := parse(frame)
	if !ok {
		return false, [6]byte{}, false
	}
	k := tuple(frame, ihl, proto)
	// reply from a backend to a client?
	t.mu.RLock()
	if e, ok := t.rev[k]; ok {
		t.mu.RUnlock()
		e.last = time.Now().UnixNano()
		rewriteAddr(frame, ihl, proto, false, e.vip, e.vport)
		mac, _ := macOf(k.dip)
		return true, mac, false
	}
	if t.cidr == nil || !t.cidr.Contains(net.IP(k.dip[:])) {
		t.mu.RUnlock()
		return false, [6]byte{}, false
	}
	e, ok := t.fwd[k]
	var backends []Backend
	if !ok {
		backends = t.svcs[svcKey{k.dip, k.dport, proto}]
	}
	t.mu.RUnlock()
	if !ok {
		if len(backends) == 0 {
			return false, [6]byte{}, false
		}
		b := t.pickBackend(svcKey{k.dip, k.dport, proto}, backends, k.sip, k.sport)
		e = &ctEntry{vip: k.dip, vport: k.dport, bip: ip4(b.IP), bport: b.Port}
		if mac, ok := macOf(e.bip); ok {
			e.bmac = mac
		} else {
			e.viaUp, e.bmac = true, t.gwMAC // not a local pod: route via the netstack
		}
		rk := ctKey{proto: proto, sip: e.bip, sport: e.bport, dip: k.sip, dport: k.sport}
		t.mu.Lock()
		t.fwd[k] = e
		t.rev[rk] = e
		t.mu.Unlock()
	}
	e.last = time.Now().UnixNano()
	rewriteAddr(frame, ihl, proto, true, e.bip, e.bport)
	return true, e.bmac, e.viaUp
}
