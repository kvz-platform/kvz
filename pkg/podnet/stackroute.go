package podnet

import (
	"errors"
	"net"
	"reflect"
	"unsafe"

	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// PodSupernet covers every node's pod CIDR (10.88.0.0/16, 10.89.0.0/16, ...).
const PodSupernet = "10.64.0.0/10"

// addSupernetRoute makes the netstack treat other nodes' pod CIDRs as on-link,
// so connections it originates (apiserver egress proxy, port-forward) reach
// remote pods: it ARPs for them, the switch answers (proxy-ARP), and the
// frames take the overlay like pod traffic does. gvisor-tap-vsock keeps its
// stack unexported, hence the reflection (pinned dependency version).
func addSupernetRoute(vn *virtualnetwork.VirtualNetwork) error {
	v := reflect.ValueOf(vn).Elem().FieldByName("stack")
	if !v.IsValid() || v.Type() != reflect.TypeOf((*stack.Stack)(nil)) {
		return errors.New("gvisor-tap-vsock: netstack field not found")
	}
	st := reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface().(*stack.Stack)
	_, super, _ := net.ParseCIDR(PodSupernet)
	subnet, err := tcpip.NewSubnet(tcpip.AddrFrom4Slice(super.IP.To4()), tcpip.MaskFromBytes(super.Mask))
	if err != nil {
		return err
	}
	// appended after the local subnet route, which stays the first match
	st.AddRoute(tcpip.Route{Destination: subnet, NIC: 1})
	return nil
}
