//go:build linux

package nodebridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"syscall"
	"time"

	"yagp/pkg/gost"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/status"
)

func diagnoseTunnelProbeFailure(ctx context.Context, err error, relay *gost.NodeConfig, gostFailure string) error {
	st := status.Convert(err)
	for _, detail := range st.Details() {
		info, ok := detail.(*errdetails.ErrorInfo)
		if !ok || info.GetDomain() != "yagp.tunnel_probe" {
			continue
		}
		evidence := make(map[string]string)
		if gostFailure != "" {
			evidence["gost_error"] = gostFailure
		}
		switch info.GetReason() {
		case "relay_write", "relay_read":
			if result := checkTunnelRelayTCP(ctx, relay); result != "" {
				evidence["relay_tcp"] = result
			}
		}
		return tunnelProbeStatus(st.Code(), st.Message(), info.GetReason(), evidence)
	}
	return err
}

func checkTunnelRelayTCP(ctx context.Context, relay *gost.NodeConfig) string {
	switch relay.Dialer.Type {
	case "tcp", "mtcp", "tls", "mtls", "ws", "wss", "mws", "mwss", "h2", "h2c", "grpc", "pht", "phts", "ohttp", "otls":
	case "udp", "kcp", "quic", "h3":
		return "not_applicable"
	default:
		return ""
	}
	if ctx.Err() != nil {
		return ""
	}
	// Use the saved relay address and reserve a separate, short budget after the data exchange.
	dialer := net.Dialer{Timeout: 600 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp", relay.Addr)
	var dns *net.DNSError
	var networkError net.Error
	switch {
	case err == nil:
		_ = conn.Close()
		return "reachable"
	case ctx.Err() != nil:
		return ""
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "unreachable"
	case errors.As(err, &networkError) && networkError.Timeout():
		return "timeout"
	case errors.As(err, &dns):
		return "dns_error"
	default:
		return "unavailable"
	}
}

// Only report recognized failure categories from the private Gost process.
type tunnelProbeLog struct {
	line       [4096]byte
	used       int
	discard    bool
	failure    string
	recognized chan struct{}
}

func (l *tunnelProbeLog) Write(data []byte) (int, error) {
	size := len(data)
	for len(data) > 0 && l.failure == "" {
		line, rest, complete := bytes.Cut(data, []byte{'\n'})
		if len(line) > len(l.line)-l.used {
			l.discard = true
		}
		if !l.discard {
			l.used += copy(l.line[l.used:], line)
		}
		if complete {
			if !l.discard {
				l.failure = tunnelProbeLogFailure(l.line[:l.used])
			}
			if l.failure != "" && l.recognized != nil {
				close(l.recognized)
			}
			l.used, l.discard = 0, false
		}
		data = rest
	}
	return size, nil
}

func tunnelProbeLogFailure(line []byte) string {
	var log struct {
		Message string `json:"msg"`
		Level   string `json:"level"`
		Service string `json:"service"`
	}
	if err := json.Unmarshal(line, &log); err != nil {
		return ""
	}
	if log.Level == "fatal" && strings.HasPrefix(log.Message, "unregistered dialer: ") {
		return "dialer_unsupported"
	}
	if log.Service != "yagp-tunnel-probe" {
		return ""
	}
	// Gost logs the Relay protocol status when the local forwarder pipes the connection.
	switch log.Message {
	case "pipe: 2 Unauthorized":
		return "relay_authentication"
	case "pipe: 3 Forbidden":
		return "relay_target_forbidden"
	case "pipe: 6 Host Unreachable", "pipe: 7 Network Unreachable":
		return "relay_target_unreachable"
	case "websocket: bad handshake":
		return "relay_handshake"
	}
	if log.Level == "error" &&
		(strings.HasPrefix(log.Message, "tls: ") || strings.HasPrefix(log.Message, "remote error: tls: ")) {
		return "relay_tls"
	}
	return ""
}

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
