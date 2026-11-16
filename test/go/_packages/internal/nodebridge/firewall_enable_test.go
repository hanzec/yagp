package nodebridge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"gopkg.in/yaml.v3"
)

func TestEnableFirewallYAMLPreservesConfiguration(t *testing.T) {
	for name, test := range map[string]struct{ input, expected string }{
		"missing firewall": {"node_id: 42\nusername: node\n", "firewall:\n  enabled: true"},
		"explicit opt-out": {"node_id: 42\n# firewall comment\nfirewall:\n  enabled: false # existing flag\n  table: filter\n  input_chain: input\n", "# firewall comment\nfirewall:\n  enabled: true # existing flag\n  table: filter\n  input_chain: input"},
		"null firewall":    {"node_id: 42\nfirewall: null\n", "firewall:\n  enabled: true"},
		"firewall alias":   {"node_id: 42\n<<: &defaults\n  firewall: &fw\n    enabled: false\n    policy: yagp-42\n    future_key: preserved\nfirewall: *fw\n", "<<: *fw\n  enabled: true"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, updated, err := enableFirewallYAML([]byte(test.input), 42)
			if err != nil || cfg.Enabled == nil || !*cfg.Enabled || !strings.Contains(string(updated), test.expected) {
				t.Fatalf("enabled YAML = %q (%+v), %v", updated, cfg, err)
			}
			var before, after map[string]any
			if err := yaml.Unmarshal([]byte(test.input), &before); err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal(updated, &after); err != nil {
				t.Fatal(err)
			}
			if got, ok := after["firewall"].(map[string]any); !ok || got["enabled"] != true {
				t.Fatalf("firewall remains disabled: %v", after["firewall"])
			} else if old, ok := before["firewall"].(map[string]any); ok {
				for key, value := range old {
					if key != "enabled" && got[key] != value {
						t.Errorf("firewall.%s = %v, want %v", key, got[key], value)
					}
				}
			}
			for key, value := range before {
				if key != "firewall" && after[key] != value {
					t.Errorf("%s = %v, want %v", key, after[key], value)
				}
			}
			if _, retry, err := enableFirewallYAML(updated, 42); err != nil || retry != nil {
				t.Fatalf("already enabled file would be rewritten: %q, %v", retry, err)
			}
		})
	}
}

func TestEnableFirewallYAMLRejectsAmbiguousChanges(t *testing.T) {
	for name, input := range map[string]string{
		"multiple documents": "node_id: 42\n---\nnode_id: 42\n",
		"wrong node":         "node_id: 43\n",
		"shared anchor":      "node_id: 42\nfirewall:\n  enabled: &flag false\nusername: *flag\n",
		"invalid flag":       "node_id: 42\nfirewall:\n  enabled: [false]\n",
		"inherited firewall": "node_id: 42\n<<: &defaults\n  firewall:\n    enabled: false\n    policy: yagp-42\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, updated, err := enableFirewallYAML([]byte(input), 42); err == nil || updated != nil {
				t.Fatalf("invalid input generated YAML %q, %v", updated, err)
			}
		})
	}
}

func TestRemoteFirewallEnablePersistsAndWakesWorker(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("automatic firewall rules require Linux")
	}
	commands := t.TempDir()
	t.Setenv("PATH", commands)
	for _, command := range []string{"nft", "ufw"} {
		if err := os.WriteFile(filepath.Join(commands, command), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	const data = "node_id: 42\nusername: node\nfirewall:\n  enabled: false\n  table: host\n  input_chain: input\n  policy: yagp-42\n"
	dir := t.TempDir()
	actual := filepath.Join(dir, "custom.yaml")
	if err := WriteNodeFile(actual, []byte(data)); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(dir, "node.yaml")
	if err := os.Symlink(actual, configured); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"error":"configuration unavailable"}`)
	}))
	t.Cleanup(server.Close)
	control := firewallTestControl(server)
	control.config.NodeID, control.config.ConfigFile = 42, configured
	disabled := false
	manager, err := NewFirewallManager(42, FirewallConfig{Enabled: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	control.firewall = manager
	control.firewallChanged = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		control.maintainFirewall(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })

	if initial, err := control.GetFirewallStatus(ctx, &emptypb.Empty{}); err != nil || initial.GetManaged() {
		t.Fatalf("initial status = %v, %v", initial, err)
	}
	if enabled, err := control.EnableFirewall(ctx, &emptypb.Empty{}); err != nil || !enabled.GetManaged() || enabled.GetDisabledReason() != "" {
		t.Fatalf("enable = %v, %v", enabled, err)
	}
	updated, err := os.ReadFile(actual)
	if err != nil || string(updated) != strings.Replace(data, "enabled: false", "enabled: true", 1) {
		t.Fatalf("persisted node config = %q, %v", updated, err)
	}
	if link, err := os.Readlink(configured); err != nil || link != actual {
		t.Fatalf("configuration link = %q, %v", link, err)
	}
	timeout := time.After(2 * time.Second)
	for {
		current, err := control.GetFirewallStatus(ctx, &emptypb.Empty{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(current.GetLastError(), "missing services") {
			break
		}
		select {
		case <-timeout:
			t.Fatalf("worker did not reconcile after enable: %v", current)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if current, err := control.EnableFirewall(ctx, &emptypb.Empty{}); err != nil || !strings.Contains(current.GetLastError(), "missing services") {
		t.Fatalf("idempotent enable lost live status: %v, %v", current, err)
	}
}

func TestRemoteFirewallEnableRejectsUnsupportedConfig(t *testing.T) {
	const data = "node_id: 43\nfirewall:\n  enabled: false\n"
	path := filepath.Join(t.TempDir(), "node.yaml")
	if err := WriteNodeFile(path, []byte(data)); err != nil {
		t.Fatal(err)
	}
	control := controlServer{config: Config{NodeID: 42, ConfigFile: path}}
	if _, err := control.EnableFirewall(t.Context(), &emptypb.Empty{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("enable with unsupported config = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != data {
		t.Fatalf("invalid node config changed: %q, %v", got, err)
	}
}
