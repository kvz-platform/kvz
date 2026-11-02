package podnet

import (
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"sort"
	"unsafe"

	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// netstack returns gvisor-tap-vsock's (unexported) gvisor stack.
func netstack(vn *virtualnetwork.VirtualNetwork) (*stack.Stack, error) {
	v := reflect.ValueOf(vn).Elem().FieldByName("stack")
	if !v.IsValid() || v.Type() != reflect.TypeOf((*stack.Stack)(nil)) {
		return nil, errors.New("gvisor-tap-vsock: netstack field not found")
	}
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface().(*stack.Stack), nil
}

// DebugDump writes the pod network's state: switch ports and tables, dropped
// frames, the netstack's routes and neighbor (ARP) cache.
func (n *Net) DebugDump(w io.Writer) {
	fmt.Fprintf(w, "subnet %s gateway %s mtu %d\n", n.Subnet, n.GW, n.MTU)
	if s := n.sw; s != nil {
		s.mu.RLock()
		fmt.Fprintf(w, "switch: %d ports, %d cam entries, %d ip->mac\n", len(s.ports), len(s.cam), len(s.ipMAC))
		var ips []string
		for ip, mac := range s.ipMAC {
			port := "MISSING"
			if p := s.cam[mac]; p != nil {
				port = fmt.Sprintf("fd%d", p.fd)
			}
			ips = append(ips, fmt.Sprintf("  %s %s %s", net.IP(ip[:]), net.HardwareAddr(mac[:]), port))
		}
		s.mu.RUnlock()
		sort.Strings(ips)
		for _, l := range ips {
			fmt.Fprintln(w, l)
		}
		fmt.Fprintf(w, "dropped frames (full NIC queues): %d\n", DroppedFrames())
	}
	st, err := netstack(n.vn)
	if err != nil {
		fmt.Fprintln(w, "netstack:", err)
		return
	}
	fmt.Fprintln(w, "netstack routes:")
	for _, r := range st.GetRouteTable() {
		fmt.Fprintf(w, "  %s nic %d gw %s\n", r.Destination, r.NIC, r.Gateway)
	}
	neigh, terr := st.Neighbors(1, ipv4.ProtocolNumber)
	if terr != nil {
		fmt.Fprintln(w, "neighbors:", terr)
		return
	}
	counts := map[string]int{}
	fmt.Fprintf(w, "netstack neighbors (%d):\n", len(neigh))
	for _, e := range neigh {
		counts[e.State.String()]++
		fmt.Fprintf(w, "  %s %s %s\n", e.Addr, e.LinkAddr, e.State)
	}
	fmt.Fprintf(w, "neighbor states: %v\n", counts)
}
