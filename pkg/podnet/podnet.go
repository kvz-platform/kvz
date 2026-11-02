// Package podnet is the kvz pod network: an in-process L2 switch + L3
// gateway (gVisor netstack, via gvisor-tap-vsock) that every pod VM plugs
// into through a SOCK_DGRAM socketpair (VZFileHandleNetworkDeviceAttachment).
//
// Why not Virtualization.framework NAT: every NAT-attached VM creates a
// kernel vmenet interface, hard-stopped VMs leak them, and the host stops
// creating new ones at ~250 (found the hard way during benchmarking). Here a
// pod's NIC is just a file descriptor: no host interfaces, no leak, no cap,
// and pod-to-pod traffic never leaves the kvz process.
package podnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
	"golang.org/x/sys/unix"
)

type Net struct {
	vn     *virtualnetwork.VirtualNetwork
	sw     *fastSwitch
	nat    *natTable
	Subnet *net.IPNet
	GW     net.IP
	MTU    int

	mu   sync.Mutex
	used map[uint32]bool
	next uint32
	lo   uint32
	hi   uint32
}

func ip2u(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func u2ip(u uint32) net.IP { return net.IPv4(byte(u>>24), byte(u>>16), byte(u>>8), byte(u)).To4() }

// New creates the pod network for one node.
func New(cidr string, mtu int) (*Net, error) {
	_, sn, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	base := ip2u(sn.IP)
	ones, bits := sn.Mask.Size()
	size := uint32(1) << uint(bits-ones)
	gw := u2ip(base + 1)
	if mtu == 0 {
		// Jumbo frames: the switch's cost is per frame, so 64 KiB frames move
		// pod-to-pod TCP at ~30-35 Gbps vs ~4.5 Gbps at 1500 (Studio).
		mtu = 65520
	}
	vn, err := virtualnetwork.New(&types.Configuration{
		MTU:               mtu,
		Subnet:            cidr,
		GatewayIP:         gw.String(),
		GatewayMacAddress: "5a:94:ef:e4:0c:dd",
		DNS:               []types.Zone{},
		NAT:               map[string]string{},
		Forwards:          map[string]string{},
		DHCPStaticLeases:  map[string]string{},
		GatewayVirtualIPs: []string{gw.String()},
		Protocol:          types.VfkitProtocol,
	})
	if err != nil {
		return nil, err
	}
	// Static pod IPs come from the top half; the built-in DHCP server (used by
	// macOS guests) assigns from the bottom up.
	n := &Net{vn: vn, Subnet: sn, GW: gw, MTU: mtu, used: map[uint32]bool{}, lo: base + size/2 + 2, hi: base + size - 2}
	n.next = n.lo
	if os.Getenv("KVZ_PODNET_SWITCH") != "gvisor" {
		// kvz fast switch in front; the netstack hangs off an uplink port.
		up, err := socketpair()
		if err != nil {
			return nil, err
		}
		gwmac, _ := net.ParseMAC("5a:94:ef:e4:0c:dd")
		svcCIDR := os.Getenv("KVZ_SERVICE_CIDR")
		if svcCIDR == "" {
			svcCIDR = "10.43.0.0/16"
		}
		n.nat = newNATTable(svcCIDR, gwmac)
		n.sw = newFastSwitch(n.nat)
		n.sw.pol = newPolicyTable(gw)
		n.sw.add(up[0], true, [4]byte{}, [6]byte{})
		f := os.NewFile(uintptr(up[1]), "podnet-uplink")
		conn, err := net.FileConn(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		go func() { _ = vn.AcceptVfkit(context.Background(), namedConn{conn}) }()
	}
	return n, nil
}

// Alloc returns a free pod IP.
func (n *Net) Alloc() (net.IP, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := uint32(0); i <= n.hi-n.lo; i++ {
		u := n.next
		n.next++
		if n.next > n.hi {
			n.next = n.lo
		}
		if !n.used[u] {
			n.used[u] = true
			return u2ip(u), nil
		}
	}
	return nil, fmt.Errorf("pod network %s exhausted", n.Subnet)
}

func (n *Net) Free(ip net.IP) {
	if ip.To4() == nil {
		return
	}
	n.mu.Lock()
	delete(n.used, ip2u(ip))
	n.mu.Unlock()
}

func (n *Net) Bits() int { o, _ := n.Subnet.Mask.Size(); return o }

// Attach creates a NIC for one VM. It returns the VM-side file (hand it to
// VZFileHandleNetworkDeviceAttachment) and plugs the other end into the switch
// until ctx is cancelled.
func socketpair() ([2]int, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		return [2]int{}, err
	}
	for _, fd := range fds {
		// room for bursts of 64 KiB frames in both directions
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 1<<20)
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<20)
		unix.CloseOnExec(fd)
	}
	return [2]int{fds[0], fds[1]}, nil
}

// Attach creates a NIC for one VM (see AttachPod).
func (n *Net) Attach(ctx context.Context) (*os.File, error) { return n.AttachPod(ctx, nil, nil) }

// AttachPod creates a NIC for a VM whose IP and MAC are known up front, so
// the switch can deliver service traffic to it before it has sent a frame.
func (n *Net) AttachPod(ctx context.Context, ip net.IP, mac net.HardwareAddr) (*os.File, error) {
	fds, err := socketpair()
	if err != nil {
		return nil, err
	}
	vmSide := os.NewFile(uintptr(fds[0]), "pod-nic-vm")
	if n.sw != nil {
		var pip [4]byte
		var pmac [6]byte
		if ip != nil {
			copy(pip[:], ip.To4())
		}
		if mac != nil {
			copy(pmac[:], mac)
		}
		port := n.sw.add(fds[1], false, pip, pmac)
		go func() {
			<-ctx.Done()
			n.sw.remove(port)
			_ = unix.Shutdown(fds[1], unix.SHUT_RDWR) // wakes the port's blocked read
			_ = unix.Close(fds[1])
		}()
		return vmSide, nil
	}
	hostFile := os.NewFile(uintptr(fds[1]), "pod-nic-host")
	conn, err := net.FileConn(hostFile)
	hostFile.Close()
	if err != nil {
		vmSide.Close()
		return nil, err
	}
	nc := namedConn{conn}
	go func() {
		_ = n.vn.AcceptVfkit(ctx, nc)
	}()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	return vmSide, nil
}

// Dial connects to a pod IP:port through the virtual network (used for
// port-forward and host access).
func (n *Net) Dial(ctx context.Context, addr string) (net.Conn, error) {
	return n.vn.DialContextTCP(ctx, addr)
}

// Routable reports whether ip is in the pod network reachable from this node
// (the local pod CIDR or a remote node's CIDR over the overlay).
func (n *Net) Routable(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	if n.Subnet.Contains(ip4) {
		return true
	}
	if n.sw != nil && n.sw.ov != nil {
		var k [4]byte
		copy(k[:], ip4)
		_, remote := n.sw.ov.lookup(k)
		return remote
	}
	return false
}

// namedConn gives the unnamed socketpair end stable addresses (the switch
// logs them).
type namedConn struct{ net.Conn }

var podAddr = &net.UnixAddr{Name: "pod", Net: "unixgram"}

func (namedConn) LocalAddr() net.Addr  { return podAddr }
func (namedConn) RemoteAddr() net.Addr { return podAddr }

// Leases returns the raw DHCP lease table (ip -> mac).
func (n *Net) Leases() map[string]string {
	rec := httptest.NewRecorder()
	n.vn.ServicesMux().ServeHTTP(rec, httptest.NewRequest("GET", "/leases", nil))
	var leases map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &leases)
	return leases
}

// LeaseFor returns the DHCP-assigned IP for mac, if any (macOS guests).
func (n *Net) LeaseFor(mac string) (string, bool) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/leases", nil)
	n.vn.ServicesMux().ServeHTTP(rec, req)
	var leases map[string]string // ip -> mac
	if json.Unmarshal(rec.Body.Bytes(), &leases) != nil {
		return "", false
	}
	want := strings.ToLower(mac)
	for ip, m := range leases {
		if strings.ToLower(m) == want {
			return ip, true
		}
	}
	return "", false
}

// SetService programs a ClusterIP:port (proto "TCP"/"UDP") with its backends.
func (n *Net) SetService(vip net.IP, port uint16, proto string, backends []Backend) {
	if n.nat == nil {
		return
	}
	p := uint8(6)
	if proto == "UDP" {
		p = 17
	}
	n.nat.SetService(vip, port, p, backends)
}

// AddDNS publishes name (relative to zone) -> ip on the gateway DNS server.
func (n *Net) AddDNS(zone, name string, ip net.IP) {
	body, _ := json.Marshal(types.Zone{Name: zone, Records: []types.Record{{Name: name, IP: ip}}})
	req := httptest.NewRequest("POST", "/services/dns/add", bytes.NewReader(body))
	n.vn.ServicesMux().ServeHTTP(httptest.NewRecorder(), req)
}

// EnableOverlay starts the cross-node link listener. secret authenticates peers.
func (n *Net) EnableOverlay(node, listenAddr string, secret []byte) error {
	if n.sw == nil {
		return fmt.Errorf("overlay requires the kvz switch")
	}
	o := &overlay{node: node, secret: secret, peers: map[string]*peerConn{}, want: map[string]string{}}
	o.deliver = n.sw.deliverInbound
	n.sw.ov = o
	return o.listen(listenAddr)
}

// SetRoutes programs remote pod CIDRs (cidr -> node) and peer endpoints (node -> host:port).
func (n *Net) SetRoutes(routes, endpoints map[string]string) {
	if n.sw != nil && n.sw.ov != nil {
		n.sw.ov.set(routes, endpoints)
	}
}

// SetPolicies installs compiled NetworkPolicies keyed by local pod IP.
func (n *Net) SetPolicies(p map[string]*PodPolicy) {
	if n.sw == nil || n.sw.pol == nil {
		return
	}
	m := map[[4]byte]*PodPolicy{}
	for ip, pp := range p {
		if v4 := net.ParseIP(ip).To4(); v4 != nil {
			var k [4]byte
			copy(k[:], v4)
			m[k] = pp
		}
	}
	n.sw.pol.set(m)
}
