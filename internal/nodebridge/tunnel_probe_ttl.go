package nodebridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const relayICMPTimeout = 750 * time.Millisecond

// A Relay forwards application data and opens a new connection to the probe listener.
// Use the IP headers of an independent ICMP echo reply from the saved Relay host.
func probeRelayReplyTTL(ctx context.Context, relayAddress string) *uint32 {
	ctx, cancel := context.WithTimeout(ctx, relayICMPTimeout)
	defer cancel()
	host, _, err := net.SplitHostPort(relayAddress)
	if err != nil {
		return nil
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil
	}
	for _, address := range addresses {
		if address.IP.IsUnspecified() || address.IP.IsMulticast() || address.IP.Equal(net.IPv4bcast) {
			continue
		}
		return echoRelayHost(ctx, &address)
	}
	return nil
}

func echoRelayHost(ctx context.Context, address *net.IPAddr) *uint32 {
	const sequence = 1
	var token [16]byte
	rand.Read(token[:])
	isIPv4 := address.IP.To4() != nil
	network, rawNetwork, bind, protocol := "udp6", "ip6:ipv6-icmp", "::", 58
	var requestType, replyType icmp.Type = ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply
	if isIPv4 {
		network, rawNetwork, bind, protocol = "udp4", "ip4:icmp", "0.0.0.0", 1
		requestType, replyType = ipv4.ICMPTypeEcho, ipv4.ICMPTypeEchoReply
	}
	conn, err := icmp.ListenPacket(network, bind)
	var destination net.Addr = &net.UDPAddr{IP: address.IP, Zone: address.Zone}
	if err != nil {
		// A Linux host may restrict datagram ping sockets even for a privileged daemon.
		conn, err = icmp.ListenPacket(rawNetwork, bind)
		if err != nil {
			return nil
		}
		destination = address
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	v4, v6 := conn.IPv4PacketConn(), conn.IPv6PacketConn()
	if v4 != nil {
		err = v4.SetControlMessage(ipv4.FlagTTL, true)
	} else {
		err = v6.SetControlMessage(ipv6.FlagHopLimit, true)
	}
	if err != nil {
		return nil
	}
	request := icmp.Message{
		Type: requestType,
		Body: &icmp.Echo{ID: int(binary.BigEndian.Uint16(token[:2])), Seq: sequence, Data: token[:]},
	}
	data, err := request.Marshal(nil)
	if err != nil {
		return nil
	}
	if _, err := conn.WriteTo(data, destination); err != nil {
		return nil
	}
	var buffer [128]byte
	for {
		var received, ttl int
		var source net.Addr
		if v4 != nil {
			var control *ipv4.ControlMessage
			received, control, source, err = v4.ReadFrom(buffer[:])
			if control != nil {
				ttl = control.TTL
			}
		} else {
			var control *ipv6.ControlMessage
			received, control, source, err = v6.ReadFrom(buffer[:])
			if control != nil {
				ttl = control.HopLimit
			}
		}
		if err != nil {
			return nil
		}
		if ttl <= 0 || ttl > 255 || !icmpReplyFrom(source, address.IP) {
			continue
		}
		reply, err := icmp.ParseMessage(protocol, buffer[:received])
		if err != nil || reply.Type != replyType || reply.Code != 0 {
			continue
		}
		if echo, ok := reply.Body.(*icmp.Echo); ok && echo.Seq == sequence && bytes.Equal(echo.Data, token[:]) {
			// The kernel can replace the ICMP identifier on a datagram ping socket.
			value := uint32(ttl)
			return &value
		}
	}
}

func icmpReplyFrom(source net.Addr, address net.IP) bool {
	switch value := source.(type) {
	case *net.UDPAddr:
		return value.IP.Equal(address)
	case *net.IPAddr:
		return value.IP.Equal(address)
	default:
		return false
	}
}
