package nodebridge

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestFirewallBackendUsesOSRelease(t *testing.T) {
	for _, test := range []struct {
		name, etc, fallback, want string
		paths                     []string
	}{
		{"Ubuntu", "NAME=Ubuntu\nID=ubuntu\nID_LIKE=debian", "", firewallUFW, []string{"/etc/os-release"}},
		{"Ubuntu derivative", "ID=linuxmint\nID_LIKE=\"ubuntu debian\"", "", firewallUFW, []string{"/etc/os-release"}},
		{"Debian", "ID=debian", "ID=ubuntu", firewallNFT, []string{"/etc/os-release"}},
		{"RHEL", "ID=rhel\nID_LIKE=fedora", "", firewallFirewalld, []string{"/etc/os-release"}},
		{"CentOS", "ID=centos\nID_LIKE=\"rhel fedora\"", "", firewallFirewalld, []string{"/etc/os-release"}},
		{"Rocky Linux", "ID=rocky", "", firewallFirewalld, []string{"/etc/os-release"}},
		{"AlmaLinux", "ID=almalinux", "", firewallFirewalld, []string{"/etc/os-release"}},
		{"Oracle Linux", "ID=ol", "", firewallFirewalld, []string{"/etc/os-release"}},
		{"OpenCloudOS", "ID=opencloudos", "", firewallFirewalld, []string{"/etc/os-release"}},
		{"OpenCloudOS Stream", "ID=opencloudos-stream", "", firewallFirewalld, []string{"/etc/os-release"}},
		{"OpenCloudOS derivative", "ID=custom\nID_LIKE='opencloudos rhel'", "", firewallFirewalld, []string{"/etc/os-release"}},
		{"Fedora", "ID=fedora", "", firewallNFT, []string{"/etc/os-release"}},
		{"Alpine fallback", "", "ID='alpine'", firewallNFT, []string{"/etc/os-release", "/usr/lib/os-release"}},
		{"no OS release", "", "", "", []string{"/etc/os-release", "/usr/lib/os-release"}},
		{"no OS identifier", "NAME=Unknown", "", "", []string{"/etc/os-release"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var paths []string
			got := readFirewallBackend(func(path string) ([]byte, error) {
				paths = append(paths, path)
				data := map[string]string{"/etc/os-release": test.etc, "/usr/lib/os-release": test.fallback}[path]
				if data == "" {
					return nil, fs.ErrNotExist
				}
				return []byte(data), nil
			})
			if got != test.want || !reflect.DeepEqual(paths, test.paths) {
				t.Fatalf("backend = %q via %v, want %q via %v", got, paths, test.want, test.paths)
			}
		})
	}
}

func TestFirewallBackendUsesActiveUFWOutsideUbuntu(t *testing.T) {
	const (
		active   = "printf 'Status: active\n\nTo Action From\n'"
		inactive = "printf 'Status: inactive\n'"
		failed   = "printf 'Status: active\n'; exit 1"
	)
	for _, test := range []struct {
		name, distro, command string
		config                FirewallConfig
		want                  string
	}{
		{"Debian with active UFW", "debian", active, FirewallConfig{}, firewallUFW},
		{"Debian with inactive UFW", "debian", inactive, FirewallConfig{}, firewallNFT},
		{"Debian with unreadable UFW status", "debian", failed, FirewallConfig{}, firewallNFT},
		{"Debian without UFW", "debian", "", FirewallConfig{}, firewallNFT},
		{"Debian with nftables delegation", "debian", active, FirewallConfig{Table: "filter", InputChain: "input"}, firewallNFT},
		{"Ubuntu with inactive UFW", "ubuntu", inactive, FirewallConfig{}, firewallUFW},
		{"CentOS with active UFW", "centos", active, FirewallConfig{}, firewallFirewalld},
	} {
		t.Run(test.name, func(t *testing.T) {
			commands := t.TempDir()
			t.Setenv("PATH", commands)
			if test.command != "" {
				const statusOnly = "#!/bin/sh\n[ \"$#\" -eq 1 ] && [ \"$1\" = status ] || exit 2\n"
				if err := os.WriteFile(filepath.Join(commands, "ufw"), []byte(statusOnly+test.command+"\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			readFile := func(string) ([]byte, error) { return []byte("ID=" + test.distro + "\n"), nil }
			if got := selectFirewallBackend(test.config, readFile); got != test.want {
				t.Fatalf("backend = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFirewallOptInUsesSelectedBackend(t *testing.T) {
	enabled, disabled := true, false
	enabledReason := ""
	if runtime.GOOS != "linux" {
		enabledReason = "unsupported_environment"
	}
	for _, test := range []struct {
		name, backend  string
		config         FirewallConfig
		managed, valid bool
		disabledReason string
	}{
		{"disabled by default", firewallUFW, FirewallConfig{}, false, true, "not_configured"},
		{"Ubuntu requires no delegation", firewallUFW, FirewallConfig{Enabled: &enabled}, true, true, enabledReason},
		{"Ubuntu never uses old nft table", firewallUFW, FirewallConfig{Table: "filter", InputChain: "input"}, true, true, enabledReason},
		{"nft delegation still opts in", firewallNFT, FirewallConfig{Table: "filter", InputChain: "input"}, true, true, enabledReason},
		{"explicit opt-out", firewallNFT, FirewallConfig{Enabled: &disabled, Table: "filter", InputChain: "input"}, false, true, "explicitly_disabled"},
		{"nft requires delegation", firewallNFT, FirewallConfig{Enabled: &enabled}, true, false, "invalid_configuration"},
		{"RHEL requires a delegated policy", firewallFirewalld, FirewallConfig{Enabled: &enabled}, true, false, "invalid_configuration"},
		{"RHEL delegated policy", firewallFirewalld, FirewallConfig{Enabled: &enabled, Policy: "yagp-42"}, true, true, enabledReason},
		{"reject invalid policy name", firewallFirewalld, FirewallConfig{Enabled: &enabled, Policy: "public\n"}, true, false, "invalid_configuration"},
		{"no nft fallback on RHEL", firewallFirewalld, FirewallConfig{Table: "filter", InputChain: "input"}, true, false, "invalid_configuration"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, err := newFirewallManager(42, test.config, test.backend)
			status := manager.Status()
			if status.Backend != test.backend || status.Managed != test.managed || (err == nil) != test.valid ||
				status.DisabledReason != test.disabledReason {
				t.Fatalf("unexpected firewall status: %+v, %v", status, err)
			}
		})
	}
}
