//go:build linux

package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// hostNetwork pods share the node's network namespace on a real node. A kvz
// pod is a VM, so the guest emulates it:
//   - the node IP is a local address (binding to it works)
//   - traffic to 127.0.0.0/8 and to the node IP leaves for the Mac (nftables
//     DNAT to virtual IPs the netstack forwards to the Mac's 127.0.0.1 and node
//     address, then masquerade)
//   - every listening socket is reported to the host, which exposes it on the
//     Mac at the same address and port; connections it forwards in are DNATed
//     back to loopback- or node-IP-bound listeners
// So 127.0.0.1 in the pod and on the Mac behave as one loopback.

// Listener is a listening socket in the pod VM.
type Listener struct {
	Proto string `json:"proto"` // tcp | udp
	Addr  string `json:"addr"`  // 0.0.0.0, 127.0.0.1 or the node IP
	Port  int    `json:"port"`
}

const (
	hostLoopIP = "192.0.2.1" // netstack -> Mac 127.0.0.1 (podnet.HostLoopIP)
	hostNodeIP = "192.0.2.2" // netstack -> Mac node IP (podnet.HostNodeIP)
)

var (
	hostNetMode *hostNet
	hostNetErr  error
)

type hostNet struct {
	nodeIP net.IP
	conn   *nftables.Conn
	table  *nftables.Table
	sets   map[string]*nftables.Set // lo_tcp lo_udp node_tcp node_udp
	cur    map[string]bool
}

func setupHostNetwork(nodeIP string) (*hostNet, error) {
	ip := net.ParseIP(nodeIP).To4()
	if ip == nil {
		return nil, fmt.Errorf("bad node IP %q", nodeIP)
	}
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return nil, err
	}
	_ = netlink.AddrAdd(lo, &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}})
	for _, k := range []string{"all", "eth0", "lo"} {
		_ = os.WriteFile("/proc/sys/net/ipv4/conf/"+k+"/route_localnet", []byte("1"), 0o644)
	}
	h := &hostNet{nodeIP: ip, conn: &nftables.Conn{}, sets: map[string]*nftables.Set{}, cur: map[string]bool{}}
	c := h.conn
	h.table = c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: "kvzhost"})
	for _, n := range []string{"lo_tcp", "lo_udp", "node_tcp", "node_udp"} {
		s := &nftables.Set{Table: h.table, Name: n, KeyType: nftables.TypeInetService}
		if err := c.AddSet(s, nil); err != nil {
			return nil, err
		}
		h.sets[n] = s
	}
	pol := nftables.ChainPolicyAccept
	out := c.AddChain(&nftables.Chain{Name: "out", Table: h.table, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookOutput, Priority: nftables.ChainPriorityNATDest, Policy: &pol})
	post := c.AddChain(&nftables.Chain{Name: "post", Table: h.table, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource, Policy: &pol})
	pre := c.AddChain(&nftables.Chain{Name: "pre", Table: h.table, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityNATDest, Policy: &pol})

	// out: 127.0.0.0/8 -> Mac loopback, node IP -> Mac node address
	c.AddRule(&nftables.Rule{Table: h.table, Chain: out, Exprs: append(matchDaddr(net.IPv4(127, 0, 0, 0), 8), dnat(net.ParseIP(hostLoopIP))...)})
	c.AddRule(&nftables.Rule{Table: h.table, Chain: out, Exprs: append(matchDaddr(ip, 32), dnat(net.ParseIP(hostNodeIP))...)})
	// post: leave with the pod IP
	c.AddRule(&nftables.Rule{Table: h.table, Chain: post, Exprs: append(append(matchOif("eth0"), matchSaddr(net.IPv4(127, 0, 0, 0), 8)...), &expr.Masq{})})
	c.AddRule(&nftables.Rule{Table: h.table, Chain: post, Exprs: append(append(matchOif("eth0"), matchSaddr(ip, 32)...), &expr.Masq{})})
	// pre: connections the host forwards in reach loopback/node-IP listeners
	for _, r := range []struct {
		set   string
		proto byte
		to    net.IP
	}{{"lo_tcp", unix.IPPROTO_TCP, net.IPv4(127, 0, 0, 1)}, {"lo_udp", unix.IPPROTO_UDP, net.IPv4(127, 0, 0, 1)},
		{"node_tcp", unix.IPPROTO_TCP, ip}, {"node_udp", unix.IPPROTO_UDP, ip}} {
		ex := matchIif("eth0")
		ex = append(ex, &expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{r.proto}})
		ex = append(ex, &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Lookup{SourceRegister: 1, SetName: r.set, SetID: h.sets[r.set].ID})
		c.AddRule(&nftables.Rule{Table: h.table, Chain: pre, Exprs: append(ex, dnat(r.to)...)})
	}
	if err := c.Flush(); err != nil {
		return nil, fmt.Errorf("nftables: %w", err)
	}
	return h, nil
}

func matchDaddr(ip net.IP, bits int) []expr.Any { return matchAddr(16, ip, bits) }
func matchSaddr(ip net.IP, bits int) []expr.Any { return matchAddr(12, ip, bits) }

func matchAddr(off uint32, ip net.IP, bits int) []expr.Any {
	mask := net.CIDRMask(bits, 32)
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: off, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ip.To4().Mask(mask)},
	}
}

func ifname(n string) []byte { b := make([]byte, 16); copy(b, n); return b }

func matchOif(n string) []expr.Any {
	return []expr.Any{&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(n)}}
}

func matchIif(n string) []expr.Any {
	return []expr.Any{&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(n)}}
}

func dnat(to net.IP) []expr.Any {
	return []expr.Any{
		&expr.Immediate{Register: 1, Data: to.To4()},
		&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1},
	}
}

// watchListeners reports the VM's listening sockets whenever they change and
// keeps the DNAT port sets current.
func (h *hostNet) watchListeners() {
	for {
		ls := scanListeners(h.nodeIP)
		key := map[string]bool{}
		for _, l := range ls {
			key[fmt.Sprintf("%s/%s/%d", l.Proto, l.Addr, l.Port)] = true
		}
		if !sameKeys(key, h.cur) {
			h.cur = key
			h.updateSets(ls)
			emit(Event{Type: "listeners", Listeners: ls})
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func sameKeys(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func (h *hostNet) updateSets(ls []Listener) {
	want := map[string][]nftables.SetElement{}
	for _, l := range ls {
		var set string
		switch l.Addr {
		case "127.0.0.1":
			set = "lo_" + l.Proto
		case h.nodeIP.String():
			set = "node_" + l.Proto
		default:
			continue // wildcard listeners accept on the pod IP directly
		}
		k := make([]byte, 2)
		binary.BigEndian.PutUint16(k, uint16(l.Port))
		want[set] = append(want[set], nftables.SetElement{Key: k})
	}
	for name, s := range h.sets {
		h.conn.FlushSet(s)
		if els := want[name]; len(els) > 0 {
			_ = h.conn.SetAddElements(s, els)
		}
	}
	_ = h.conn.Flush()
}

// scanListeners parses /proc/net: TCP sockets in LISTEN, unconnected UDP
// sockets outside the ephemeral range (clients' sockets live there).
func scanListeners(nodeIP net.IP) []Listener {
	seen := map[string]bool{}
	var out []Listener
	for _, f := range []struct {
		path, proto string
		v6          bool
	}{{"/proc/net/tcp", "tcp", false}, {"/proc/net/tcp6", "tcp", true}, {"/proc/net/udp", "udp", false}, {"/proc/net/udp6", "udp", true}} {
		fh, err := os.Open(f.path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Scan() // header
		for sc.Scan() {
			fs := strings.Fields(sc.Text())
			if len(fs) < 4 {
				continue
			}
			st := fs[3]
			if (f.proto == "tcp" && st != "0A") || (f.proto == "udp" && st != "07") {
				continue
			}
			local := strings.Split(fs[1], ":")
			remote := strings.Split(fs[2], ":")
			if len(local) != 2 || len(remote) != 2 {
				continue
			}
			port64, _ := strconv.ParseUint(local[1], 16, 16)
			port := int(port64)
			if f.proto == "udp" && (strings.Trim(remote[1], "0") != "" || (port >= 32768 && port <= 60999)) {
				continue
			}
			addr := procAddr(local[0], f.v6)
			switch {
			case addr == nil || addr.IsUnspecified():
				addr = net.IPv4zero
			case addr.IsLoopback():
				addr = net.IPv4(127, 0, 0, 1)
			case addr.Equal(nodeIP):
			default:
				continue // pod-IP-bound and other sockets are not host listeners
			}
			l := Listener{Proto: f.proto, Addr: addr.String(), Port: port}
			k := fmt.Sprintf("%s/%s/%d", l.Proto, l.Addr, l.Port)
			if !seen[k] {
				seen[k] = true
				out = append(out, l)
			}
		}
		fh.Close()
	}
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprint(out[i]) < fmt.Sprint(out[j])
	})
	return out
}

// procAddr decodes /proc/net's little-endian hex addresses.
func procAddr(h string, v6 bool) net.IP {
	b, err := hex.DecodeString(h)
	if err != nil {
		return nil
	}
	for i := 0; i+4 <= len(b); i += 4 { // each 32-bit word is host (little) endian
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	ip := net.IP(b)
	if v6 {
		if v4 := ip.To4(); v4 != nil { // ::ffff:a.b.c.d
			return v4
		}
	}
	return ip
}
