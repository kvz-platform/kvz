// Package lb is the agent's apiserver load balancer (the k3s agent LB): a
// local TCP proxy that forwards to the first reachable server and fails over
// to the others. TLS stays end to end; servers' certificates carry 127.0.0.1.
package lb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

type LB struct {
	mu        sync.Mutex
	servers   []string // host:port, preferred first
	statePath string
	ln        net.Listener
}

// Start listens on 127.0.0.1 and seeds the server list with initial plus any
// servers remembered in statePath.
func Start(ctx context.Context, initial, statePath string) (*LB, error) {
	l := &LB{statePath: statePath, servers: []string{initial}}
	if b, err := os.ReadFile(statePath); err == nil {
		var saved []string
		if json.Unmarshal(b, &saved) == nil {
			l.merge(saved)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	l.ln = ln
	go func() { <-ctx.Done(); ln.Close() }()
	go l.serve()
	return l, nil
}

// URL is the https URL clients should use.
func (l *LB) URL() string { return "https://" + l.ln.Addr().String() }

func (l *LB) merge(more []string) {
	seen := map[string]bool{}
	for _, s := range l.servers {
		seen[s] = true
	}
	for _, s := range more {
		if !seen[s] {
			l.servers = append(l.servers, s)
			seen[s] = true
		}
	}
}

// Set replaces the server list (keeping the current preferred one first if
// it is still a member) and persists it.
func (l *LB) Set(servers []string) {
	if len(servers) == 0 {
		return
	}
	sort.Strings(servers)
	l.mu.Lock()
	cur := l.servers[0]
	next := []string{}
	for _, s := range servers {
		if s == cur {
			next = append([]string{s}, next...)
		} else {
			next = append(next, s)
		}
	}
	changed := fmt.Sprint(next) != fmt.Sprint(l.servers)
	l.servers = next
	l.mu.Unlock()
	if changed {
		klog.InfoS("apiserver load balancer servers", "servers", next)
		b, _ := json.Marshal(next)
		_ = os.WriteFile(l.statePath, b, 0o600)
	}
}

func (l *LB) dial() (net.Conn, error) {
	l.mu.Lock()
	servers := append([]string(nil), l.servers...)
	l.mu.Unlock()
	var last error
	for i, s := range servers {
		c, err := net.DialTimeout("tcp", s, 2*time.Second)
		if err == nil {
			if i > 0 { // fail over: make the working server preferred
				l.mu.Lock()
				for j, x := range l.servers {
					if x == s {
						l.servers[0], l.servers[j] = l.servers[j], l.servers[0]
					}
				}
				l.mu.Unlock()
				klog.InfoS("apiserver load balancer failed over", "server", s)
			}
			return c, nil
		}
		last = err
	}
	return nil, last
}

func (l *LB) serve() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			up, err := l.dial()
			if err != nil {
				c.Close()
				return
			}
			go func() { _, _ = io.Copy(up, c); up.Close() }()
			_, _ = io.Copy(c, up)
			c.Close()
		}()
	}
}

// Watch keeps the server list in sync with the kubernetes Service's
// EndpointSlices (every apiserver registers itself there).
func (l *LB) Watch(ctx context.Context, cs kubernetes.Interface) {
	for {
		slices, err := cs.DiscoveryV1().EndpointSlices("default").List(ctx, metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=kubernetes"})
		if err == nil {
			var servers []string
			for _, sl := range slices.Items {
				port := int32(6443)
				for _, p := range sl.Ports {
					if p.Port != nil {
						port = *p.Port
					}
				}
				for _, ep := range sl.Endpoints {
					for _, a := range ep.Addresses {
						servers = append(servers, net.JoinHostPort(a, fmt.Sprint(port)))
					}
				}
			}
			l.Set(servers)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
}
