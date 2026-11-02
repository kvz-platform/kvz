package microvmrt

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/GrowlyX/kvz/pkg/microvm"
	rt "github.com/GrowlyX/kvz/pkg/runtime"
)

// hostNetwork pods: kvz-init reports every listening socket in the VM and the
// host exposes it on the Mac at the same address and port (see kvz-init's
// hostnet.go), so the pod's sockets behave like the node's.

type hostListener struct {
	closer io.Closer
}

func listenerKey(l microvm.Listener) string { return fmt.Sprintf("%s/%s/%d", l.Proto, l.Addr, l.Port) }

// syncHostListeners reconciles the Mac-side listeners of a hostNetwork pod.
func (b *Backend) syncHostListeners(p *corev1.Pod, ps *pod, ls []microvm.Listener) {
	ps.hnMu.Lock()
	defer ps.hnMu.Unlock()
	if ps.hostListeners == nil {
		ps.hostListeners = map[string]hostListener{}
	}
	want := map[string]microvm.Listener{}
	for _, l := range ls {
		want[listenerKey(l)] = l
	}
	for k, hl := range ps.hostListeners {
		if _, ok := want[k]; !ok {
			hl.closer.Close()
			delete(ps.hostListeners, k)
		}
	}
	for k, l := range want {
		if _, ok := ps.hostListeners[k]; ok {
			continue
		}
		bind := net.JoinHostPort(l.Addr, strconv.Itoa(l.Port))
		target := net.JoinHostPort(ps.ip.String(), strconv.Itoa(l.Port))
		c, err := b.exposeOnHost(l.Proto, bind, target)
		if err != nil {
			klog.InfoS("hostNetwork: cannot expose listener on the Mac", "pod", rt.Key(p.Namespace, p.Name), "addr", bind, "proto", l.Proto, "err", err)
			b.t.Event(p.Namespace, p.Name, corev1.EventTypeWarning, "HostPortInUse", "cannot listen on %s/%s on the node: %v", bind, l.Proto, err)
			continue
		}
		ps.hostListeners[k] = hostListener{closer: c}
	}
}

func (b *Backend) closeHostListeners(ps *pod) {
	ps.hnMu.Lock()
	defer ps.hnMu.Unlock()
	for k, hl := range ps.hostListeners {
		hl.closer.Close()
		delete(ps.hostListeners, k)
	}
}

// exposeOnHost listens on the Mac (tcp or udp) and proxies into the pod.
func (b *Backend) exposeOnHost(proto, bind, target string) (io.Closer, error) {
	if proto == "udp" {
		pc, err := net.ListenPacket("udp", bind)
		if err != nil {
			return nil, err
		}
		go b.serveUDP(pc, target)
		return pc, nil
	}
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, err
	}
	go b.serveTCP(ln, target)
	return ln, nil
}

func (b *Backend) serveTCP(ln net.Listener, target string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			up, err := b.pnet.Dial(ctx, target)
			cancel()
			if err != nil {
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(up, conn); done <- struct{}{} }()
			go func() { io.Copy(conn, up); done <- struct{}{} }()
			<-done
		}()
	}
}

// serveUDP relays datagrams: one netstack UDP conn per client address,
// dropped after a minute of silence.
func (b *Backend) serveUDP(pc net.PacketConn, target string) {
	type sess struct {
		conn net.Conn
		last time.Time
	}
	var mu sync.Mutex
	sessions := map[string]*sess{}
	go func() {
		for range time.Tick(30 * time.Second) {
			mu.Lock()
			for k, s := range sessions {
				if time.Since(s.last) > time.Minute {
					s.conn.Close()
					delete(sessions, k)
				}
			}
			mu.Unlock()
		}
	}()
	buf := make([]byte, 65535)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			mu.Lock()
			for _, s := range sessions {
				s.conn.Close()
			}
			mu.Unlock()
			return
		}
		mu.Lock()
		s := sessions[addr.String()]
		if s == nil {
			c, err := b.pnet.DialUDP(target)
			if err != nil {
				mu.Unlock()
				continue
			}
			s = &sess{conn: c}
			sessions[addr.String()] = s
			go func(c net.Conn, addr net.Addr) {
				rb := make([]byte, 65535)
				for {
					m, err := c.Read(rb)
					if err != nil {
						return
					}
					_, _ = pc.WriteTo(rb[:m], addr)
				}
			}(c, addr)
		}
		s.last = time.Now()
		mu.Unlock()
		_, _ = s.conn.Write(buf[:n])
	}
}
