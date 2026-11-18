package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yagp/internal/config"
)

func TestInstallPortRendersEffectiveAddress(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"empty", "", ":8080"},
		{"missing", "server: {mode: release}\n", ":8080"},
		{"flow", "server: {port: '[::1]:39100'}\n", "[::1]:8080"},
		{"alias", "defaults: &base {port: '127.0.0.1:39100'}\nserver: *base\n", "127.0.0.1:8080"},
		{"same", "SERVER: {PORT: ':8080'}\n", ":8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			rendered, err := installListenAddress(path, cfg.Server.Port, "08080")
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "same" && rendered != tc.yaml {
				t.Fatalf("rewrote identical port: %s", rendered)
			}
			if tc.name == "alias" && !strings.Contains(rendered, "127.0.0.1:39100") {
				t.Fatalf("lost original anchor: %s", rendered)
			}
			if err := os.WriteFile(path, []byte(rendered), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err = config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			addr, err := installListenAddress(path, cfg.Server.Port, "current")
			if err != nil || addr != tc.want {
				t.Errorf("effective address = %q, %v; want %q", addr, err, tc.want)
			}
		})
	}
}
