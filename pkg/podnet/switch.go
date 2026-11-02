package podnet

import (
	"sync"

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
				send(q.fd, frame) // includes hairpin (q == p)
			}
			return
		}
	}
	if dst[0]&1 == 1 { // broadcast / multicast: flood
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
		send(q.fd, frame)
	case q == nil && !p.uplink && up != nil:
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
	copy(frame[6:12], s.nat.gwMAC[:])
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
			send(q.fd, frame)
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
