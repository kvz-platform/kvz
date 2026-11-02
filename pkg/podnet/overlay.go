package podnet

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// overlay carries IPv4 packets between nodes' pod networks over TCP links
// (length-framed). TCP instead of UDP because it works through NAT, VPN ACLs
// and SSH tunnels; a link is bidirectional, so only one side needs to be able
// to dial. Peers authenticate with an HMAC of the cluster secret.
type overlay struct {
	node    string
	secret  []byte
	mu      sync.RWMutex
	routes  []route              // remote CIDR -> peer node
	peers   map[string]*peerConn // node -> link
	want    map[string]string    // node -> endpoint to dial
	deliver func([]byte)         // inbound packet handler
}

type route struct {
	net  *net.IPNet
	node string
}

type peerConn struct {
	c  net.Conn
	mu sync.Mutex
	w  *bufio.Writer
}

type hello struct {
	Node string `json:"node"`
	MAC  string `json:"mac"`
}

func (o *overlay) sign(node string) string {
	m := hmac.New(sha256.New, o.secret)
	m.Write([]byte(node))
	return hex.EncodeToString(m.Sum(nil))
}

func (o *overlay) listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go o.handshake(c, "")
		}
	}()
	go o.dialLoop()
	return nil
}

// handshake exchanges hellos; expect is the peer we dialed ("" when accepting).
func (o *overlay) handshake(c net.Conn, expect string) {
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	enc := json.NewEncoder(c)
	if err := enc.Encode(hello{Node: o.node, MAC: o.sign(o.node)}); err != nil {
		c.Close()
		return
	}
	br := bufio.NewReaderSize(c, 256<<10)
	line, err := br.ReadBytes('\n')
	if err != nil {
		c.Close()
		return
	}
	var h hello
	if json.Unmarshal(line, &h) != nil || !hmac.Equal([]byte(h.MAC), []byte(o.sign(h.Node))) || (expect != "" && h.Node != expect) {
		klog.InfoS("overlay: rejected peer", "addr", c.RemoteAddr())
		c.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	pc := &peerConn{c: c, w: bufio.NewWriterSize(c, 256<<10)}
	o.mu.Lock()
	if old := o.peers[h.Node]; old != nil {
		old.c.Close()
	}
	o.peers[h.Node] = pc
	o.mu.Unlock()
	klog.InfoS("overlay: link up", "peer", h.Node, "addr", c.RemoteAddr())
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			break
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n > frameMax {
			break
		}
		pkt := make([]byte, n)
		if _, err := io.ReadFull(br, pkt); err != nil {
			break
		}
		o.deliver(pkt)
	}
	o.mu.Lock()
	if o.peers[h.Node] == pc {
		delete(o.peers, h.Node)
	}
	o.mu.Unlock()
	c.Close()
	klog.InfoS("overlay: link down", "peer", h.Node)
}

// dialLoop keeps a link to every wanted peer we are not already linked with.
func (o *overlay) dialLoop() {
	for {
		o.mu.RLock()
		var todo [][2]string
		for node, ep := range o.want {
			if o.peers[node] == nil {
				todo = append(todo, [2]string{node, ep})
			}
		}
		o.mu.RUnlock()
		for _, t := range todo {
			go func(node, ep string) {
				c, err := net.DialTimeout("tcp", ep, 3*time.Second)
				if err != nil {
					return
				}
				o.handshake(c, node)
			}(t[0], t[1])
		}
		time.Sleep(3 * time.Second)
	}
}

// set replaces the routing table and dial targets.
func (o *overlay) set(routes map[string]string, endpoints map[string]string) {
	var rs []route
	for cidr, node := range routes {
		if _, n, err := net.ParseCIDR(cidr); err == nil && node != o.node {
			rs = append(rs, route{n, node})
		}
	}
	o.mu.Lock()
	o.routes = rs
	o.want = map[string]string{}
	for node, ep := range endpoints {
		if node != o.node && ep != "" {
			o.want[node] = ep
		}
	}
	o.mu.Unlock()
}

// lookup returns the peer link for a destination IP in a remote pod CIDR.
// remote reports whether the IP belongs to another node at all.
func (o *overlay) lookup(ip [4]byte) (pc *peerConn, remote bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	for _, r := range o.routes {
		if r.net.Contains(net.IP(ip[:])) {
			return o.peers[r.node], true
		}
	}
	return nil, false
}

func (pc *peerConn) send(pkt []byte) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(pkt)))
	pc.mu.Lock()
	_, err := pc.w.Write(hdr[:])
	if err == nil {
		_, err = pc.w.Write(pkt)
	}
	if err == nil {
		err = pc.w.Flush()
	}
	pc.mu.Unlock()
	if err != nil {
		pc.c.Close()
	}
}
