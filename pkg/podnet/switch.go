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
	if !p.uplink {
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
	var src, dst [6]byte
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
		copy(dst[:], buf[0:6])
		copy(src[:], buf[6:12])
		// learn (fast path: already known)
		s.mu.RLock()
		known := s.cam[src]
		s.mu.RUnlock()
		if known != p {
			s.mu.Lock()
			s.cam[src] = p
			s.mu.Unlock()
		}
		frame := buf[:n]
		if s.nat != nil && dst[0]&1 == 0 {
			if hit, dmac, viaUp := s.nat.outbound(frame, s.macOf); hit {
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
				if q != nil && q != p {
					send(q.fd, frame)
				} else if q == p {
					send(p.fd, frame) // hairpin: pod reaching its own service
				}
				continue
			}
		}
		if dst[0]&1 == 1 { // broadcast / multicast: flood
			s.flood(p, frame)
			continue
		}
		s.mu.RLock()
		q := s.cam[dst]
		up := s.uplink
		s.mu.RUnlock()
		switch {
		case q != nil && q != p:
			send(q.fd, frame)
		case q == nil && !p.uplink && up != nil:
			send(up.fd, frame) // gateway / not yet learned: let the netstack handle it
		case q == nil && p.uplink:
			s.flood(p, frame)
		}
	}
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
		send(q.fd, frame)
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
