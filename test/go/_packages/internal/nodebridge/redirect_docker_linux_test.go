package nodebridge

import (
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"yagp/pkg/nodegrpc"
)

func dockerBridgeNetworks(t *testing.T, names ...string) (string, func(...string)) {
	t.Helper()
	directory, err := os.MkdirTemp("", "yagp-bridge-networks-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	type network struct {
		Driver  string
		Options map[string]string
	}
	var snapshot atomic.Value
	report := func(names ...string) {
		networks := make([]network, 0, len(names))
		for _, name := range names {
			networks = append(networks, network{Driver: "bridge", Options: map[string]string{"com.docker.network.bridge.name": name}})
		}
		snapshot.Store(networks)
	}
	report(names...)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/networks" {
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(snapshot.Load()); err != nil {
			t.Errorf("write Docker network snapshot: %v", err)
		}
	})}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()
	return socket, report
}

func TestDiscoverDockerBridgesUsesLiveIPv4Address(t *testing.T) {
	if os.Getenv("YAGP_TEST_NFT") != "1" {
		t.Skip("set YAGP_TEST_NFT=1 in a disposable Linux network namespace")
	}
	ip, err := exec.LookPath("ip")
	if err != nil {
		t.Skip("ip link is unavailable")
	}
	const bridge = "br-594147501234"
	const empty = "br-594147501235"
	for _, name := range []string{bridge, empty} {
		if output, err := exec.CommandContext(t.Context(), ip, "link", "add", name, "type", "bridge").CombinedOutput(); err != nil {
			t.Skipf("cannot create a bridge in this test namespace: %v (%s)", err, output)
		}
		t.Cleanup(func() {
			if output, err := exec.Command(ip, "link", "delete", name).CombinedOutput(); err != nil {
				t.Errorf("delete test bridge %s: %v (%s)", name, err, output)
			}
		})
	}
	if output, err := exec.CommandContext(t.Context(), ip, "addr", "add", "172.30.47.1/24", "dev", bridge).CombinedOutput(); err != nil {
		t.Fatalf("configure test bridge: %v (%s)", err, output)
	}
	socket, _ := dockerBridgeNetworks(t, bridge, empty)
	bridges, err := routeDockerBridges(t.Context(), map[string]struct{}{bridge: {}, empty: {}})
	if err != nil {
		t.Fatal(err)
	}
	address := netip.MustParseAddr("172.30.47.1")
	if !slices.Contains(bridges, dockerBridge{Name: bridge, Address: address}) ||
		slices.ContainsFunc(bridges, func(bridge dockerBridge) bool { return bridge.Name == empty }) {
		t.Fatalf("live Docker bridge discovery = %v", bridges)
	}
	t.Run("reported candidates", func(t *testing.T) {
		const notBridge = "br-594147501236"
		if output, err := exec.CommandContext(t.Context(), ip, "link", "add", notBridge, "type", "veth").CombinedOutput(); err != nil {
			t.Skipf("cannot create a veth in this test namespace: %v (%s)", err, output)
		}
		t.Cleanup(func() { _ = exec.Command(ip, "link", "delete", notBridge).Run() })
		if output, err := exec.CommandContext(t.Context(), ip, "addr", "add", "172.30.48.1/24", "dev", notBridge).CombinedOutput(); err != nil {
			t.Fatalf("configure test veth: %v (%s)", err, output)
		}
		reported := map[string]struct{}{bridge: {}, empty: {}, notBridge: {}, "br-594147501239": {}}
		got, err := routeDockerBridges(t.Context(), reported)
		want := []dockerBridge{{Name: bridge, Address: address}}
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("reported bridge discovery = %v, %v; want %v", got, err, want)
		}
	})
	var selected redirectState
	if err := selected.detectDocker(t.Context(), true, socket); err != nil || selected.DockerAddress != bridges[0].Address ||
		!slices.Contains(selected.Bridges, bridge) || slices.Contains(selected.Bridges, empty) {
		t.Fatalf("redirect did not select the live Docker listener: %+v, %v", selected, err)
	}
	if output, err := exec.CommandContext(t.Context(), ip, "addr", "del", "172.30.47.1/24", "dev", bridge).CombinedOutput(); err != nil {
		t.Fatalf("remove Docker gateway address: %v (%s)", err, output)
	}
	persisted := redirectState{DockerAddress: address, Bridges: []string{bridge}}
	if err := persisted.detectDocker(t.Context(), false, socket); err != nil || persisted.DockerAddress != address || len(persisted.Bridges) != 0 {
		t.Fatalf("missing bridge should keep the saved listener and suspend Docker DNAT: %+v, %v", persisted, err)
	}
}

func TestRefreshDockerRedirectReplacesRecreatedBridge(t *testing.T) {
	if os.Getenv("YAGP_TEST_NFT") != "1" {
		t.Skip("set YAGP_TEST_NFT=1 in a disposable Linux network namespace")
	}
	ip, err := exec.LookPath("ip")
	if err != nil {
		t.Skip("ip link is unavailable")
	}
	nft, err := exec.LookPath("nft")
	if err != nil {
		t.Skip("nftables is unavailable")
	}
	const listenerBridge = "br-594147501100"
	const oldBridge = "br-594147501101"
	const newBridge = "br-594147501102"
	const subnet = "10.251.1.1/24"
	ctx := t.Context()
	run := func(program string, args ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, program, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v (%s)", program, args, err, output)
		}
		return string(output)
	}
	addBridge := func(name, address string) {
		t.Helper()
		run(ip, "link", "add", name, "type", "bridge")
		t.Cleanup(func() { _ = exec.Command(ip, "link", "delete", name).Run() })
		run(ip, "addr", "add", address, "dev", name)
	}
	addBridge(listenerBridge, "10.250.0.1/24")
	addBridge(oldBridge, subnet)
	socket, report := dockerBridgeNetworks(t, listenerBridge, oldBridge)

	control := &controlServer{config: Config{NodeID: 594147501100, ConfigFile: filepath.Join(t.TempDir(), "node.yaml")}}
	table := nodegrpc.TransparentRedirectTableName(control.config.NodeID)
	path := redirectStatePath(control.config)
	initial := redirectState{
		Version:       redirectRulesVersion,
		Revision:      1,
		Owner:         "rule-5",
		Port:          12345,
		Mark:          0x59414750,
		RelayIPs:      []netip.Addr{netip.MustParseAddr("203.0.113.7")},
		DockerAddress: netip.MustParseAddr("10.250.0.1"),
		Bridges:       []string{listenerBridge, oldBridge},
	}
	if err := writeRedirectState(ctx, nft, path, table, redirectState{}, initial); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command(nft, "delete", "table", "inet", table).Run() })
	tableDeclaration, _, _ := strings.Cut(run(nft, "-a", "list", "table", "inet", table), "\n")
	outputChain := run(nft, "-a", "list", "chain", "inet", table, "output")

	run(ip, "link", "delete", oldBridge)
	addBridge(newBridge, subnet)
	report(listenerBridge, newBridge)
	t.Setenv("PATH", t.TempDir())
	if err := control.refreshDockerRedirect(ctx, socket); err != nil {
		t.Fatal(err)
	}
	members := run(nft, "list", "set", "inet", table, "docker_ifaces")
	if !strings.Contains(members, newBridge) || !strings.Contains(members, listenerBridge) || strings.Contains(members, oldBridge) {
		t.Fatalf("unexpected bridge set members:\n%s", members)
	}
	for _, chain := range []string{"prerouting", "docker_input"} {
		rules := run(nft, "list", "chain", "inet", table, chain)
		if !strings.Contains(rules, "iifname @docker_ifaces") {
			t.Fatalf("chain %s does not use the bridge set:\n%s", chain, rules)
		}
	}
	if got, _, _ := strings.Cut(run(nft, "-a", "list", "table", "inet", table), "\n"); got != tableDeclaration {
		t.Fatalf("transparent redirect table changed from %q to %q", tableDeclaration, got)
	}
	if got := run(nft, "-a", "list", "chain", "inet", table, "output"); got != outputChain {
		t.Fatalf("bridge refresh changed unrelated output rules:\n%s", got)
	}
	want := initial
	want.Bridges = []string{listenerBridge, newBridge}
	state, err := readRedirectState(path)
	if err != nil || !state.sameRules(want) || state.Revision != want.Revision {
		t.Fatalf("refreshed state = %+v, %v; want %+v", state, err, want)
	}

	rules := run(nft, "-a", "list", "table", "inet", table)
	if err := control.refreshDockerRedirect(ctx, socket); err != nil {
		t.Fatal(err)
	}
	if err := control.refreshDockerRedirect(ctx, filepath.Join(t.TempDir(), "unavailable.sock")); err != nil {
		t.Fatal(err)
	}
	if got := run(nft, "-a", "list", "table", "inet", table); got != rules {
		t.Fatalf("unchanged or unavailable Docker snapshot caused an nftables update:\n%s", got)
	}
	state, err = readRedirectState(path)
	if err != nil || !state.sameRules(want) || state.Revision != want.Revision {
		t.Fatalf("unchanged bridges changed saved state to %+v, %v", state, err)
	}

	run(ip, "link", "delete", listenerBridge)
	report(newBridge)
	if err := control.refreshDockerRedirect(ctx, socket); err != nil {
		t.Fatal(err)
	}
	empty := run(nft, "list", "set", "inet", table, "docker_ifaces")
	if strings.Contains(empty, "elements =") {
		t.Fatalf("bridge set is populated while its listener address is absent:\n%s", empty)
	}
	addBridge(listenerBridge, "10.250.0.1/24")
	report(listenerBridge, newBridge)
	if err := control.refreshDockerRedirect(ctx, socket); err != nil {
		t.Fatal(err)
	}
	if got := run(nft, "list", "set", "inet", table, "docker_ifaces"); !strings.Contains(got, listenerBridge) || !strings.Contains(got, newBridge) {
		t.Fatalf("listener restored with unexpected bridge set:\n%s", got)
	}
}
