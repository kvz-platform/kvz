package microvmrt

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
)

// IPAM hands out pod IPs on the Virtualization.framework NAT network. macOS's
// bootpd leases addresses from the bottom of that subnet to DHCP guests (e.g.
// macOS VM pods), so kvz allocates statically from the top down.
type IPAM struct {
	mu   sync.Mutex
	GW   net.IP
	Bits int
	base [4]byte
	used map[byte]bool
	lo   byte
	hi   byte
	next byte
}

func NewIPAM() (*IPAM, error) {
	gw := "192.168.65.1"
	mask := "255.255.255.0"
	if out, err := exec.Command("defaults", "read", "/Library/Preferences/SystemConfiguration/com.apple.vmnet", "Shared_Net_Address").Output(); err == nil {
		gw = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("defaults", "read", "/Library/Preferences/SystemConfiguration/com.apple.vmnet", "Shared_Net_Mask").Output(); err == nil {
		mask = strings.TrimSpace(string(out))
	}
	ip := net.ParseIP(gw).To4()
	m := net.IPMask(net.ParseIP(mask).To4())
	bits, _ := m.Size()
	if ip == nil || bits != 24 {
		// Only /24 supported for now; fall back to the default VZ subnet.
		ip, bits = net.ParseIP("192.168.65.1").To4(), 24
	}
	a := &IPAM{GW: ip, Bits: bits, used: map[byte]bool{}, lo: 64, hi: 254}
	copy(a.base[:], ip)
	a.next = a.hi
	return a, nil
}

func (a *IPAM) Alloc() (net.IP, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := 0; i <= int(a.hi-a.lo); i++ {
		o := a.next
		if a.next <= a.lo {
			a.next = a.hi
		} else {
			a.next--
		}
		if !a.used[o] {
			a.used[o] = true
			return net.IPv4(a.base[0], a.base[1], a.base[2], o).To4(), nil
		}
	}
	return nil, fmt.Errorf("pod IP pool exhausted (%d addresses)", a.hi-a.lo+1)
}

func (a *IPAM) Free(ip net.IP) {
	if ip4 := ip.To4(); ip4 != nil {
		a.mu.Lock()
		delete(a.used, ip4[3])
		a.mu.Unlock()
	}
}
