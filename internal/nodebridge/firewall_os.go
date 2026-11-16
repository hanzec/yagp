package nodebridge

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"slices"
	"strings"
)

const (
	firewallNFT       = "nftables"
	firewallUFW       = "ufw"
	firewallFirewalld = "firewalld"
)

func hostFirewallBackend(config FirewallConfig) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	return selectFirewallBackend(config, os.ReadFile)
}

func selectFirewallBackend(config FirewallConfig, readFile func(string) ([]byte, error)) string {
	backend := readFirewallBackend(readFile)
	// Keep existing nftables delegation on nodes that have already configured it.
	if backend == firewallNFT && config.Table == "" && config.InputChain == "" && hostUFWActive() {
		return firewallUFW
	}
	return backend
}

func readFirewallBackend(readFile func(string) ([]byte, error)) string {
	data, err := readFile("/etc/os-release")
	if errors.Is(err, fs.ErrNotExist) {
		data, err = readFile("/usr/lib/os-release")
	}
	if err != nil {
		return ""
	}
	var distro, related string
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch key {
		case "ID":
			distro = value
		case "ID_LIKE":
			related = value
		}
	}
	families := strings.Fields(related)
	if distro == "ubuntu" || slices.Contains(families, "ubuntu") {
		return firewallUFW
	}
	if isRHELFamily(distro) || slices.ContainsFunc(families, isRHELFamily) {
		return firewallFirewalld
	}
	if distro == "" {
		return ""
	}
	return firewallNFT
}

func isRHELFamily(distro string) bool {
	switch distro {
	case "rhel", "centos", "rocky", "almalinux", "ol", "opencloudos", "opencloudos-stream":
		return true
	default:
		return false
	}
}
