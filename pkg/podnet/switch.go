package podnet

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// fastSwitch is kvz's L2 learning switch for pod NICs. gvisor-tap-vsock's
// switch serializes every frame on the node behind two global mutexes (plus a
// write lock on the MAC table per received frame) and converts each frame to a
// netstack PacketBuffer. Here pod-to-pod unicast is one read(2) + one write(2)
// with a read-locked MAC table and no allocation; only traffic for the
// gateway (egress/DNS/DHCP) and broadcasts go to the netstack via the uplink.
type fastSwitch struct {
	mu     sync.RWMutex
	cam    map[[6]byte]*swPort
	ports  map[*swPort]struct{}
	uplink *swPort
	ipMAC  map[[4]byte][6]byte // pod IP -> MAC (registered at attach)
	nat    *natTable
	ov     *overlay
	dns    DNSAnswerer // cluster DNS, answered in the switch for UDP
	dnsVIP [4]byte
	pol    *policyTable
}

// policyOK enforces NetworkPolicy: egress on the source node, ingress on the
// destination node (frames from the overlay were egress-checked at their source).
func (s *fastSwitch) policyOK(p *swPort, frame []byte) bool {
	return s.pol == nil || s.pol.check(frame, p != overlayPort, true)
}

type swPort struct {
	fd     int
	uplink bool
	ip     [4]byte
	mac    [6]byte
}

func newFastSwitch(nat *natTable) *fastSwitch {
	return &fastSwitch{cam: map[[6]byte]*swPort{}, ports: map[*swPort]struct{}{}, ipMAC: map[[4]byte][6]byte{}, nat: nat}
}

func (s *fastSwitch) macOf(ip [4]byte) ([6]byte, bool) {
	s.mu.RLock()
	m, ok := s.ipMAC[ip]
	s.mu.RUnlock()
	return m, ok
}

const frameMax = 65536 + 64

func (s *fastSwitch) add(fd int, uplink bool, ip [4]byte, mac [6]byte) *swPort {
	p := &swPort{fd: fd, uplink: uplink, ip: ip, mac: mac}
	s.mu.Lock()
	s.ports[p] = struct{}{}
	if uplink {
		s.uplink = p
	} else if ip != ([4]byte{}) {
		s.ipMAC[ip] = mac
		s.cam[mac] = p
	}
	s.mu.Unlock()
	go s.rx(p)
	return p
}

func (s *fastSwitch) remove(p *swPort) {
	s.mu.Lock()
	delete(s.ports, p)
	// Only drop mappings this port still owns: the IP may already belong to a
	// new pod that attached before this one finished tearing down.
	if mac, ok := s.ipMAC[p.ip]; ok && !p.uplink && s.cam[mac] == p {
		delete(s.ipMAC, p.ip)
	}
	for mac, q := range s.cam {
		if q == p {
			delete(s.cam, mac)
		}
	}
	s.mu.Unlock()
}

func (s *fastSwitch) rx(p *swPort) {
	buf := make([]byte, frameMax)
	for {
		n, err := unix.Read(p.fd, buf)
		if err != nil {
			if err == unix.EINTR || err == unix.EAGAIN {
				continue
			}
			s.remove(p)
			return
		}
		if n == 0 { // shut down
			s.remove(p)
			return
		}
		if n < 14 {
			continue
		}
		s.handle(p, buf[:n])
	}
}

// droppedFrames counts frames dropped because a receiver stayed full.
var droppedFrames atomic.Uint64

// DroppedFrames reports frames dropped on full pod NIC queues.
func DroppedFrames() uint64 { return droppedFrames.Load() }

// overlayPort is the pseudo ingress port for packets arriving from peers.
var overlayPort = &swPort{fd: -1}

// handle switches one frame from port p (frame may be modified in place).
func (s *fastSwitch) handle(p *swPort, frame []byte) {
	var src, dst [6]byte
	copy(dst[:], frame[0:6])
	copy(src[:], frame[6:12])
	if p != overlayPort {
		// learn (fast path: already known)
		s.mu.RLock()
		known := s.cam[src]
		s.mu.RUnlock()
		if known != p {
			s.mu.Lock()
			s.cam[src] = p
			s.mu.Unlock()
		}
	}
	if p != overlayPort && s.isDNSQuery(frame) {
		go s.answerDNS(p, append([]byte(nil), frame...))
		return
	}
	if s.nat != nil && dst[0]&1 == 0 {
		if hit, dmac, viaUp := s.nat.outbound(frame, s.macOf); hit {
			if !s.policyOK(p, frame) {
				return
			}
			if s.routeRemote(frame) {
				return
			}
			copy(frame[0:6], dmac[:])
			var q *swPort
			s.mu.RLock()
			if viaUp {
				q = s.uplink
			} else {
				q = s.cam[dmac]
				if q == nil {
					q = s.uplink
				}
			}
			s.mu.RUnlock()
			if q != nil && q.fd >= 0 {
				s.guardUplink(q, frame)
				sendFrom(p, q.fd, frame) // includes hairpin (q == p)
			}
			return
		}
	}
	if dst[0]&1 == 1 { // broadcast / multicast: flood
		if p.uplink && s.proxyARP(p, frame) {
			return
		}
		if p != overlayPort {
			s.flood(p, frame)
		}
		return
	}
	if !s.policyOK(p, frame) {
		return
	}
	if p != overlayPort && s.routeRemote(frame) {
		return
	}
	s.mu.RLock()
	q := s.cam[dst]
	up := s.uplink
	s.mu.RUnlock()
	switch {
	case q != nil && q != p && q.fd >= 0:
		s.guardUplink(q, frame)
		sendFrom(p, q.fd, frame)
	case q == nil && !p.uplink && up != nil:
		s.guardUplink(up, frame)
		send(up.fd, frame) // gateway / not yet learned: let the netstack handle it
	case q == nil && p.uplink:
		s.flood(p, frame)
	}
}

// routeRemote sends IPv4 frames destined to another node's pod CIDR over the
// overlay (Ethernet header stripped). Reports whether it took the frame.
func (s *fastSwitch) routeRemote(frame []byte) bool {
	if s.ov == nil || len(frame) < ethHdr+20 || frame[12] != 0x08 || frame[13] != 0x00 {
		return false
	}
	var dip [4]byte
	copy(dip[:], frame[ethHdr+16:ethHdr+20])
	pc, remote := s.ov.lookup(dip)
	if !remote {
		return false
	}
	if pc != nil {
		pc.send(frame[ethHdr:])
	}
	return true // remote but no link yet: drop (TCP retransmits)
}

// deliverInbound turns an IPv4 packet from a peer into a frame for the
// destination pod and switches it (reverse service NAT applies).
func (s *fastSwitch) deliverInbound(pkt []byte) {
	if len(pkt) < 20 {
		return
	}
	var dip [4]byte
	copy(dip[:], pkt[16:20])
	frame := make([]byte, ethHdr+len(pkt))
	mac, ok := s.macOf(dip)
	if !ok {
		// not a local pod: could be a reverse-NAT reply for a service VIP client
		// on this node; frame it to the gateway MAC and let handle() decide.
		mac = s.nat.gwMAC
	}
	copy(frame[0:6], mac[:])
	// Source MAC: the one proxy-ARP advertises for the remote IP. Never the
	// gateway's: gvisor-tap-vsock's switch learns source MACs, and once it
	// believed the gateway MAC lived on the uplink it stopped forwarding the
	// netstack's own broadcasts (ARP) to us, breaking every connection the
	// node originates to pods (egress proxy, port-forward, LoadBalancer).
	frame[6], frame[7] = 0x02, 0x6b
	copy(frame[8:12], pkt[12:16])
	frame[12], frame[13] = 0x08, 0x00
	copy(frame[ethHdr:], pkt)
	s.handle(overlayPort, frame)
}

func (s *fastSwitch) flood(from *swPort, frame []byte) {
	s.mu.RLock()
	targets := make([]*swPort, 0, len(s.ports))
	for q := range s.ports {
		if q != from {
			targets = append(targets, q)
		}
	}
	s.mu.RUnlock()
	for _, q := range targets {
		if q.fd >= 0 {
			sendShared(q.fd, frame) // one wedged port must not block a broadcast
		}
	}
}

// send writes one frame. Datagram writes are atomic, so concurrent senders to
// the same port need no lock; a full receiver applies backpressure.
func send(fd int, frame []byte) {
	for {
		_, err := unix.Write(fd, frame)
		if err == unix.EINTR {
			continue
		}
		return
	}
}

// sendShared writes a frame on behalf of a goroutine that serves many pods
// (the uplink/netstack, overlay peers, floods). A pod whose NIC stopped
// draining (wedged or stopping VM) must not stall everyone else: retry a
// full queue briefly, then drop (TCP retransmits; ARP retries).
func sendShared(fd int, frame []byte) {
	for i := 0; i < 200; i++ { // ~2ms worst case
		err := unix.Sendto(fd, frame, unix.MSG_DONTWAIT, nil)
		switch err {
		case nil:
			return
		case unix.EINTR:
			continue
		case unix.EAGAIN, unix.ENOBUFS:
			if i > 20 {
				time.Sleep(10 * time.Microsecond)
			} else {
				runtime.Gosched()
			}
		default:
			return
		}
	}
	droppedFrames.Add(1)
}

// sendFrom picks blocking backpressure for pod-originated frames (it only
// stalls that pod's own port goroutine) and bounded sends otherwise.
func sendFrom(p *swPort, fd int, frame []byte) {
	if p == overlayPort || p.uplink {
		sendShared(fd, frame)
		return
	}
	send(fd, frame)
}

// proxyARP answers the netstack's ARP requests for pods on other nodes with a
// MAC derived from the IP; frames sent there are routed over the overlay.
func (s *fastSwitch) proxyARP(p *swPort, frame []byte) bool {
	if s.ov == nil || len(frame) < 42 || frame[12] != 0x08 || frame[13] != 0x06 || frame[20] != 0 || frame[21] != 1 {
		return false
	}
	var tip [4]byte
	copy(tip[:], frame[38:42])
	if _, remote := s.ov.lookup(tip); !remote {
		return false
	}
	mac := [6]byte{0x02, 0x6b, tip[0], tip[1], tip[2], tip[3]}
	r := make([]byte, 42)
	copy(r[0:6], frame[6:12])
	copy(r[6:12], mac[:])
	r[12], r[13] = 0x08, 0x06
	copy(r[14:20], []byte{0, 1, 8, 0, 6, 4})
	r[20], r[21] = 0, 2 // reply
	copy(r[22:28], mac[:])
	copy(r[28:32], tip[:])
	copy(r[32:38], frame[22:28])
	copy(r[38:42], frame[28:32])
	sendShared(p.fd, r)
	return true
}

// routerMAC replaces the gateway's MAC as source on frames sent to the
// netstack.
var routerMAC = [6]byte{0x02, 0x6b, 0xff, 0xff, 0xff, 0xfe}

// guardUplink keeps an invariant gvisor-tap-vsock depends on: no frame
// reaches the netstack claiming the gateway's own MAC as source. Its switch
// learns source MACs and floods broadcasts everywhere except where a frame's
// source was learned; once it "learned" the gateway on kvz's uplink, the
// netstack's ARP requests stopped reaching pods and every connection the node
// originates (egress proxy, port-forward, hostPort, NodePort) broke. Frames
// the netstack sent that kvz hairpins back (service traffic to an endpoint
// outside the pod network, e.g. the node's own address) would teach it that.
func (s *fastSwitch) guardUplink(q *swPort, frame []byte) {
	if q != nil && q.uplink && s.nat != nil && len(frame) >= 12 && [6]byte(frame[6:12]) == s.nat.gwMAC {
		copy(frame[6:12], routerMAC[:])
	}
}
