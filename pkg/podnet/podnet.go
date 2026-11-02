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
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
	"golang.org/x/sys/unix"
)

type Net struct {
	vn     *virtualnetwork.VirtualNetwork
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
		mtu = 1500
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
	n := &Net{vn: vn, Subnet: sn, GW: gw, MTU: mtu, used: map[uint32]bool{}, lo: base + 2, hi: base + size - 2}
	n.next = n.lo
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
func (n *Net) Attach(ctx context.Context) (*os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, err
	}
	for _, fd := range fds {
		// room for bursts of frames in both directions
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 256<<10)
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 1<<20)
	}
	vmSide := os.NewFile(uintptr(fds[0]), "pod-nic-vm")
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

// namedConn gives the unnamed socketpair end stable addresses (the switch
// logs them).
type namedConn struct{ net.Conn }

var podAddr = &net.UnixAddr{Name: "pod", Net: "unixgram"}

func (namedConn) LocalAddr() net.Addr  { return podAddr }
func (namedConn) RemoteAddr() net.Addr { return podAddr }
