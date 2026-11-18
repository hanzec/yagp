package gost

import (
	"fmt"
	"strconv"
	"strings"
)

// Network identifies the socket transport used by a Gost listener.
func (l *ListenerConfig) Network() string {
	switch l.Type {
	case "tcp", "mtcp", "tls", "mtls", "ws", "mws", "wss", "mwss", "h2", "h2c", "grpc", "pht", "phts", "ohttp", "otls", "red":
		return "tcp"
	case "udp", "quic", "h3":
		return "udp"
	case "kcp":
		for _, key := range []string{"kcp.tcp", "tcp"} {
			if value, ok := l.Metadata[key]; ok {
				if enabled, _ := strconv.ParseBool(strings.TrimSpace(fmt.Sprint(value))); enabled {
					return "tcp"
				}
				break
			}
		}
		return "udp"
	default:
		return ""
	}
}
