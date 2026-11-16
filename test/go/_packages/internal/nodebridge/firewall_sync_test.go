package nodebridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"yagp/pkg/nodegrpc"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestFirewallPolicyFollowsExitEndpointAndCachesChanges(t *testing.T) {
	enabled := true
	disabled := false
	control := &controlServer{config: Config{NodeID: 42, ConfigFile: filepath.Join(t.TempDir(), "node.yaml"),
		Firewall: FirewallConfig{Enabled: &disabled, Table: "host-filter", InputChain: "host-input"}}}
	legacy, err := newFirewallManager(42, control.config.Firewall, firewallNFT)
	if err != nil {
		t.Fatal(err)
	}
	policy := newFirewallPolicy(control, legacy)
	status := func() *nodegrpc.FirewallStatus {
		t.Helper()
		reply, err := control.GetFirewallStatus(t.Context(), &emptypb.Empty{})
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	if got := status(); got.GetManaged() || got.GetDisabledReason() != "waiting_panel" {
		t.Fatalf("initial status = %v", got)
	}
	client := &firewallPolicyClient{config: &nodegrpc.NodeConfig{}}
	policy.refresh(t.Context(), control, client)
	if got := status(); got.GetManaged() || got.GetDisabledReason() != "explicitly_disabled" {
		t.Fatalf("legacy status = %v", got)
	}
	client.config = &nodegrpc.NodeConfig{HasExitEndpoint: &enabled, NftTemplate: "initial"}
	policy.refresh(t.Context(), control, client)
	if got := status(); !got.GetManaged() {
		t.Fatalf("exit did not enable automatic rules: %v", got)
	}
	if cached, err := readPanelConfig(control.config.ConfigFile); err != nil || !cached.GetHasExitEndpoint() {
		t.Fatalf("enabled policy was not cached: %v, %v", cached, err)
	}
	client.config = &nodegrpc.NodeConfig{HasExitEndpoint: &enabled, NftTemplate: "unchanged policy"}
	policy.refresh(t.Context(), control, client)
	if cached, err := readPanelConfig(control.config.ConfigFile); err != nil || cached.GetNftTemplate() != "initial" {
		t.Fatalf("same policy overwrote cache: %v, %v", cached, err)
	}
	client.err = errors.New("panel not available")
	policy.refresh(t.Context(), control, client)
	client.err = nil
	client.config = &nodegrpc.NodeConfig{}
	policy.refresh(t.Context(), control, client)
	if got := status(); !got.GetManaged() {
		t.Fatalf("absent policy cleared a known exit: %v", got)
	}
	client.config = &nodegrpc.NodeConfig{HasExitEndpoint: &disabled}
	policy.refresh(t.Context(), control, client)
	if got := status(); got.GetManaged() || got.GetDisabledReason() != "no_exit_endpoint" {
		t.Fatalf("last exit deletion did not disable automatic rules: %v", got)
	}
	newFirewallPolicy(control, legacy)
	if got := status(); got.GetManaged() || got.GetDisabledReason() != "no_exit_endpoint" {
		t.Fatalf("cached policy was not restored: %v", got)
	}
}

type firewallPolicyClient struct {
	nodegrpc.V1Client
	config *nodegrpc.NodeConfig
	err    error
}

func (c *firewallPolicyClient) GetNodeConfig(context.Context, *emptypb.Empty, ...grpc.CallOption) (*nodegrpc.NodeConfig, error) {
	return c.config, c.err
}

func TestFirewallRestartRechecksBackendAndLiveRules(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		_, _ = fmt.Fprint(w, `{"error":"configuration unavailable"}`)
	}))
	t.Cleanup(server.Close)
	control := firewallTestControl(server)
	control.config.NodeID = 42
	control.config.Firewall = FirewallConfig{Table: "host-filter", InputChain: "host-input"}
	control.firewallChanged = make(chan struct{}, 1)
	manager, err := newFirewallManager(42, control.config.Firewall, firewallNFT)
	if err != nil {
		t.Fatal(err)
	}
	lastCheck := time.Now().UTC()
	manager.status.Synchronized = true
	manager.status.DisabledReason = ""
	manager.status.LastError = nil
	manager.status.LastSync = &lastCheck
	control.firewall.Store(manager)
	policy := firewallPolicy{ready: true}

	control.queueFirewallRestartCheck()
	select {
	case <-control.firewallChanged:
	default:
		t.Fatal("restart did not wake firewall checking")
	}
	got, err := control.GetFirewallStatus(t.Context(), &emptypb.Empty{})
	if err != nil || got.GetSynchronized() || got.GetDisabledReason() != "checking_firewall" || got.LastError != nil ||
		!got.GetLastSync().AsTime().Equal(lastCheck) {
		t.Fatalf("queued restart status = %v, %v", got, err)
	}
	detections := 0
	policy.check(t.Context(), control, func(FirewallConfig) string {
		detections++
		return firewallNFT
	})
	got, err = control.GetFirewallStatus(t.Context(), &emptypb.Empty{})
	if err != nil || detections != 1 || reads.Load() != 1 || got.GetDisabledReason() != "" || !strings.Contains(got.GetLastError(), "missing services") {
		t.Fatalf("restart status = %v, %v, detections=%d, Gost reads=%d", got, err, detections, reads.Load())
	}
}

func TestFirewallRedetectsUFWAfterEarlyNftablesFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"error":"configuration unavailable"}`)
	}))
	t.Cleanup(server.Close)
	enabled := true
	control := firewallTestControl(server)
	control.config.NodeID = 42
	control.config.Firewall.Enabled = &enabled
	manager, err := newFirewallManager(42, control.config.Firewall, firewallNFT)
	if err == nil || manager.Status().DisabledReason != "invalid_configuration" {
		t.Fatalf("expected invalid initial nftables backend: %v, %v", manager.Status(), err)
	}
	control.firewall.Store(manager)
	policy := firewallPolicy{ready: true}
	detections := 0
	policy.check(t.Context(), control, func(FirewallConfig) string {
		detections++
		return firewallUFW
	})
	got := control.firewall.Load().Status()
	if detections != 1 || got.Backend != firewallUFW || !got.Managed || got.DisabledReason == "invalid_configuration" || policy.cleanup != nil {
		t.Fatalf("late backend was not selected: %+v, detections=%d, cleanup=%v", got, detections, policy.cleanup)
	}
}

func TestFirewallRestartRecheckRetainsNewerRequest(t *testing.T) {
	disabled := false
	control := &controlServer{config: Config{NodeID: 42, Firewall: FirewallConfig{Enabled: &disabled}}}
	manager, err := newFirewallManager(42, control.config.Firewall, firewallNFT)
	if err != nil {
		t.Fatal(err)
	}
	manager.status.DisabledReason = "no_exit_endpoint"
	control.firewall.Store(manager)
	policy := firewallPolicy{ready: true, hasExitEndpoint: &disabled}
	control.queueFirewallRestartCheck()
	policy.check(t.Context(), control, func(FirewallConfig) string {
		control.queueFirewallRestartCheck()
		return firewallNFT
	})
	got, err := control.GetFirewallStatus(t.Context(), &emptypb.Empty{})
	if err != nil || got.GetDisabledReason() != "checking_firewall" {
		t.Fatalf("newer restart cleared by previous check: %v, %v", got, err)
	}
	policy.check(t.Context(), control, func(FirewallConfig) string { return firewallNFT })
	got, err = control.GetFirewallStatus(t.Context(), &emptypb.Empty{})
	if err != nil || got.GetManaged() || got.GetDisabledReason() != "no_exit_endpoint" {
		t.Fatalf("no-exit mode changed after restart checks: %v, %v", got, err)
	}
}

func TestFirewallReadsManagedGostListeners(t *testing.T) {
	const services = `{"services":[
		{"name":"rule-1","addr":":20001","handler":{"type":"forward"},"listener":{"type":"tcp"},"status":{"state":"running"}},
		{"name":"rule-2","addr":"192.0.2.10:20002","handler":{"type":"forward"},"listener":{"type":"udp"},"status":{"state":"ready"}},
		{"name":"relay-tunnel-1","addr":"[2001:db8::10]:20003","handler":{"type":"relay"},"listener":{"type":"grpc"},"status":{"state":"running"}},
		{"name":"relay-tunnel-2","addr":":20004","handler":{"type":"relay"},"listener":{"type":"h3"},"status":{"state":"running"}},
		{"name":"relay-tunnel-3","addr":":20005","handler":{"type":"relay"},"listener":{"type":"kcp","metadata":{"kcp.tcp":"true"}},"status":{"state":"running"}},
		{"name":"relay-tunnel-4","addr":":20006","handler":{"type":"relay"},"listener":{"type":"kcp"},"status":{"state":"running"}},
		{"name":"relay-tunnel-5","addr":"[::ffff:192.0.2.11]:20007","handler":{"type":"relay"},"listener":{"type":"quic"},"status":{"state":"running"}},
		{"name":"custom","addr":":30001","handler":{"type":"forward"},"listener":{"type":"tcp"},"status":{"state":"running"}},
		{"name":"rule-3","addr":"127.0.0.1:30002","handler":{"type":"red"},"listener":{"type":"red"},"status":{"state":"running"}},
		{"name":"rule-4","addr":"[::1]:30003","handler":{"type":"forward"},"listener":{"type":"tcp"},"status":{"state":"running"}},
		{"name":"rule-5","addr":":30004","handler":{"type":"forward"},"listener":{"type":"tcp"},"status":{"state":"failed"}},
		{"name":"rule-6","addr":":30005","handler":{"type":"forward"},"listener":{"type":"tcp"}},
		{"name":"rule-7","addr":":70000","handler":{"type":"forward"},"listener":{"type":"tcp"},"status":{"state":"running"}},
		{"name":"rule-8","addr":"not-an-ip:30008","handler":{"type":"forward"},"listener":{"type":"tcp"},"status":{"state":"running"}},
		{"name":"relay-tunnel-6","addr":":30009","handler":{"type":"relay"},"listener":{"type":"unknown"},"status":{"state":"running"}}
	]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "daemon" || password != "secret" || r.URL.Path != "/api/config" || r.Method != http.MethodGet {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = fmt.Fprint(w, services)
	}))
	defer server.Close()
	control := firewallTestControl(server)
	got, err := control.firewallPorts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := FirewallPorts{
		TCP: []uint16{20001, 20005}, UDP: []uint16{20004, 20006},
		TCPAddresses: []FirewallEndpoint{{Address: netip.MustParseAddr("2001:db8::10"), Port: 20003}},
		UDPAddresses: []FirewallEndpoint{
			{Address: netip.MustParseAddr("192.0.2.10"), Port: 20002},
			{Address: netip.MustParseAddr("192.0.2.11"), Port: 20007},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("firewall ports = %+v, want %+v", got, want)
	}
}

func TestFirewallSyncRecordsGostReadFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"error":"configuration unavailable"}`)
	}))
	defer server.Close()
	control := firewallTestControl(server)
	manager, err := NewFirewallManager(42, FirewallConfig{Table: "host", InputChain: "input"})
	if err != nil {
		t.Fatal(err)
	}
	control.syncFirewall(context.Background(), manager)
	state := manager.Status()
	if state.Synchronized || state.LastError == nil || !strings.Contains(*state.LastError, "missing services") {
		t.Fatalf("firewall status = %+v", state)
	}
}

func TestFirewallReconcileTriggeredBySuccessfulGostServiceChange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/config/services/rule-2" {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	control := firewallTestControl(server)
	control.firewallChanged = make(chan struct{}, 1)
	for _, change := range []struct {
		method, path string
		trigger      bool
	}{
		{http.MethodPost, "/config/services", true},
		{http.MethodDelete, "/config/services/rule-1", true},
		{http.MethodDelete, "/config/services/rule-2", false},
		{http.MethodGet, "/config/services/rule-1", false},
		{http.MethodPost, "/config/chains", false},
	} {
		if _, err := control.Execute(context.Background(), &nodegrpc.CommandRequest{Method: change.method, Path: change.path}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-control.firewallChanged:
			if !change.trigger {
				t.Errorf("unexpected reconcile trigger for %s %s", change.method, change.path)
			}
		default:
			if change.trigger {
				t.Errorf("missing reconcile trigger for %s %s", change.method, change.path)
			}
		}
	}
}

func firewallTestControl(server *httptest.Server) *controlServer {
	return &controlServer{
		config: Config{AdminAddress: server.Listener.Addr().String(), Username: "daemon", Password: "secret"},
		client: server.Client(),
	}
}
