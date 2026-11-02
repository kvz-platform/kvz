// Package egress is the apiserver's path into the pod network. kube-apiserver
// is configured with an EgressSelectorConfiguration whose "cluster" selection
// is an HTTP CONNECT proxy on a unix socket served here. Destinations inside
// the pod network (the local pod CIDR and every remote node's CIDR, reached
// through the overlay) are dialed through the node's userspace netstack;
// everything else (kubelets, external webhooks) is dialed directly. This is
// what makes admission/conversion webhooks, aggregated API servers and
// pods/services proxy work against pods.
package egress

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// Network is a pod network the proxy can dial into.
type Network interface {
	Routable(ip net.IP) bool
	Dial(ctx context.Context, addr string) (net.Conn, error)
}

var (
	mu       sync.RWMutex
	networks []Network
)

// Register adds a pod network (the in-process node's podnet).
func Register(n Network) {
	mu.Lock()
	networks = append(networks, n)
	mu.Unlock()
}

func dial(ctx context.Context, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip != nil {
		mu.RLock()
		nets := append([]Network(nil), networks...)
		mu.RUnlock()
		for _, n := range nets {
			if n.Routable(ip) {
				return n.Dial(ctx, addr)
			}
		}
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// ConfigYAML returns the EgressSelectorConfiguration for a proxy socket.
func ConfigYAML(sock string) string {
	return `apiVersion: apiserver.k8s.io/v1beta1
kind: EgressSelectorConfiguration
egressSelections:
- name: cluster
  connection:
    proxyProtocol: HTTPConnect
    transport:
      uds:
        udsName: ` + sock + "\n"
}

// Serve runs the CONNECT proxy on a unix socket until ctx ends.
func Serve(ctx context.Context, sock string) error {
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	_ = os.Chmod(sock, 0o600)
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go handle(ctx, c)
	}
}

func handle(ctx context.Context, c net.Conn) {
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	req, err := http.ReadRequest(br)
	if err != nil {
		c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	if req.Method != http.MethodConnect {
		_, _ = io.WriteString(c, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
		c.Close()
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	up, err := dial(dctx, req.Host)
	cancel()
	if err != nil {
		klog.V(2).InfoS("egress dial failed", "dest", req.Host, "err", err)
		_, _ = io.WriteString(c, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n")
		c.Close()
		return
	}
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		c.Close()
		up.Close()
		return
	}
	go func() {
		if n := br.Buffered(); n > 0 { // bytes the client sent after the request
			b, _ := br.Peek(n)
			_, _ = up.Write(b)
		}
		_, _ = io.Copy(up, c)
		closeWrite(up)
	}()
	_, _ = io.Copy(c, up)
	closeWrite(c)
	time.AfterFunc(5*time.Second, func() { c.Close(); up.Close() })
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		_ = c.Close()
	}
}
