//go:build linux

package main

import (
	"net"

	"github.com/vishvananda/netlink"
)

// setupNetwork brings up lo and eth0 with the host-assigned address. It does
// not wait for carrier: the virtio link comes up asynchronously and nothing
// on the start path needs it.
func setupNetwork(ip string, bits int, gw string, mtu int) error {
	if lo, err := netlink.LinkByName("lo"); err == nil {
		_ = netlink.LinkSetUp(lo)
	}
	if ip == "" {
		return nil
	}
	eth, err := netlink.LinkByName("eth0")
	if err != nil {
		return err
	}
	if mtu > 1500 {
		_ = netlink.LinkSetMTU(eth, mtu)
	}
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(bits, 32)}}
	if err := netlink.AddrAdd(eth, addr); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(eth); err != nil {
		return err
	}
	if gw != "" {
		return netlink.RouteAdd(&netlink.Route{LinkIndex: eth.Attrs().Index, Gw: net.ParseIP(gw)})
	}
	return nil
}
