package podnet

import (
	"context"
	"encoding/binary"
	"net"
	"time"

	"github.com/miekg/dns"
	"k8s.io/klog/v2"
)

// DNSAnswerer resolves DNS queries (pkg/clusterdns).
type DNSAnswerer interface {
	Answer(ctx context.Context, req *dns.Msg) *dns.Msg
}

// DNSTCPPort is where the netstack serves DNS over TCP (the kube-dns VIP's
// TCP port 53 is NATed to gateway:DNSTCPPort).
const DNSTCPPort = 1053

// ServeClusterDNS answers queries to vip:53. UDP is intercepted in the switch
// and answered in place (no TCP/IP stack on the path); TCP goes to a listener
// in the netstack.
func (n *Net) ServeClusterDNS(ctx context.Context, vip net.IP, h DNSAnswerer) error {
	if n.sw == nil {
		return nil
	}
	copy(n.sw.dnsVIP[:], vip.To4())
	n.sw.dns = h
	l, err := n.vn.Listen("tcp", net.JoinHostPort(n.GW.String(), "1053"))
	if err != nil {
		return err
	}
	srv := &dns.Server{Listener: l, Net: "tcp", Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		_ = w.WriteMsg(h.Answer(ctx, r))
	})}
	go func() { <-ctx.Done(); _ = srv.Shutdown() }()
	go func() {
		if err := srv.ActivateAndServe(); err != nil && ctx.Err() == nil {
			klog.ErrorS(err, "cluster DNS (tcp)")
		}
	}()
	return nil
}

// isDNSQuery reports whether frame is an IPv4 UDP packet to the DNS VIP:53.
func (s *fastSwitch) isDNSQuery(frame []byte) bool {
	if s.dns == nil || len(frame) < ethHdr+28 || frame[12] != 0x08 || frame[13] != 0x00 {
		return false
	}
	ip := frame[ethHdr:]
	ihl := int(ip[0]&0x0f) * 4
	if ip[9] != 17 || len(ip) < ihl+8 || [4]byte(ip[16:20]) != s.dnsVIP {
		return false
	}
	return binary.BigEndian.Uint16(ip[ihl+2:ihl+4]) == 53
}

// answerDNS resolves an intercepted query and writes the reply frame to the
// asking port.
func (s *fastSwitch) answerDNS(p *swPort, frame []byte) {
	ip := frame[ethHdr:]
	ihl := int(ip[0]&0x0f) * 4
	udp := ip[ihl:]
	ulen := int(binary.BigEndian.Uint16(udp[4:6]))
	if ulen < 8 || ulen > len(udp) {
		return
	}
	req := new(dns.Msg)
	if err := req.Unpack(udp[8:ulen]); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	resp := s.dns.Answer(ctx, req)
	cancel()
	max := dns.MinMsgSize
	if o := req.IsEdns0(); o != nil && int(o.UDPSize()) > max {
		max = int(o.UDPSize())
	}
	if max > 1400 {
		max = 1400
	}
	resp.Truncate(max)
	payload, err := resp.Pack()
	if err != nil {
		return
	}
	out := make([]byte, ethHdr+20+8+len(payload))
	copy(out[0:6], frame[6:12]) // back to the asker
	copy(out[6:12], frame[0:6]) // from the gateway MAC it addressed
	out[12], out[13] = 0x08, 0x00
	oip := out[ethHdr:]
	oip[0], oip[1] = 0x45, 0
	binary.BigEndian.PutUint16(oip[2:4], uint16(20+8+len(payload)))
	binary.BigEndian.PutUint16(oip[4:6], binary.BigEndian.Uint16(ip[4:6]))
	oip[6], oip[7] = 0x40, 0 // DF
	oip[8], oip[9] = 64, 17
	copy(oip[12:16], ip[16:20]) // from the VIP
	copy(oip[16:20], ip[12:16]) // to the client
	binary.BigEndian.PutUint16(oip[10:12], ipChecksum(oip[:20]))
	ou := oip[20:]
	copy(ou[0:2], udp[2:4]) // sport 53
	copy(ou[2:4], udp[0:2]) // client port
	binary.BigEndian.PutUint16(ou[4:6], uint16(8+len(payload)))
	copy(ou[8:], payload)
	binary.BigEndian.PutUint16(ou[6:8], udpChecksum(oip[12:16], oip[16:20], ou))
	if p.fd >= 0 {
		sendShared(p.fd, out)
	}
}

func ipChecksum(h []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(h); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(h[i:]))
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func udpChecksum(src, dst, seg []byte) uint16 {
	var sum uint32
	add := func(b []byte) {
		for i := 0; i+1 < len(b); i += 2 {
			sum += uint32(binary.BigEndian.Uint16(b[i:]))
		}
		if len(b)%2 == 1 {
			sum += uint32(b[len(b)-1]) << 8
		}
	}
	add(src)
	add(dst)
	sum += 17 + uint32(len(seg))
	add(seg)
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	c := ^uint16(sum)
	if c == 0 {
		c = 0xffff
	}
	return c
}
