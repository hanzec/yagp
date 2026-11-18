package gost

import "testing"

func TestListenerNetworkHonorsKCPTransport(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		want     string
	}{
		{"default", nil, "udp"},
		{"enabled", map[string]any{"kcp.tcp": true}, "tcp"},
		{"string", map[string]any{"kcp.tcp": " true "}, "tcp"},
		{"legacy", map[string]any{"tcp": "true"}, "tcp"},
		{"disabled takes precedence", map[string]any{"kcp.tcp": false, "tcp": true}, "udp"},
		{"invalid", map[string]any{"kcp.tcp": "invalid"}, "udp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := &ListenerConfig{Type: "kcp", Metadata: tc.metadata}
			if got := listener.Network(); got != tc.want {
				t.Fatalf("KCP network = %q, want %q", got, tc.want)
			}
		})
	}
	if got := (&ListenerConfig{Type: "unknown"}).Network(); got != "" {
		t.Fatalf("unknown listener network = %q", got)
	}
}
