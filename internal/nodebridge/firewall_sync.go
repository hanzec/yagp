//go:build linux

package nodebridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync/atomic"
	"time"

	"yagp/pkg/gost"
	"yagp/pkg/nodegrpc"

	"google.golang.org/protobuf/types/known/emptypb"
)

const firewallSyncInterval = 15 * time.Second

func wakeFirewallWorker(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *controlServer) queueFirewallRestartCheck() {
	s.firewallRestartRequested.Add(1)
	wakeFirewallWorker(s.firewallChanged)
}

type firewallPolicy struct {
	legacy          *FirewallManager
	ready           bool
	hasExitEndpoint *bool
	pendingCache    *nodegrpc.NodeConfig
	cleanup         *FirewallManager
	cleanupRequired bool
}

func newFirewallPolicy(s *controlServer, legacy *FirewallManager) firewallPolicy {
	policy := firewallPolicy{legacy: legacy}
	if s.config.ConfigFile != "" {
		if config, err := readPanelConfig(s.config.ConfigFile); err == nil {
			policy.update(s, config.HasExitEndpoint)
			return policy
		}
	}
	config := s.config.Firewall
	disabled := false
	config.Enabled = &disabled
	waiting, _ := newFirewallManager(s.config.NodeID, config, legacy.backend)
	waiting.status.DisabledReason = "waiting_panel"
	s.firewall.Store(waiting)
	return policy
}

func (s *controlServer) maintainFirewall(ctx context.Context, current *atomic.Pointer[nodegrpc.Session], policy firewallPolicy) {
	ticker := time.NewTicker(firewallSyncInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		var client nodegrpc.V1Client
		if session := current.Load(); session != nil {
			client = nodegrpc.NewV1Client(session)
		}
		policy.refresh(ctx, s, client)
		policy.check(ctx, s, hostFirewallBackend)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.firewallChanged:
		}
	}
}

func (p *firewallPolicy) check(ctx context.Context, s *controlServer, detect func(FirewallConfig) string) {
	requested := s.firewallRestartRequested.Load()
	checkRestart := requested != s.firewallRestartChecked.Load()
	manager := s.firewall.Load()
	// Some hosts start UFW after the daemon initially falls back to nftables.
	if checkRestart || (manager.Status().Managed &&
		(manager.backend == "" || (manager.backend == firewallNFT && manager.config.Table == "" && manager.config.InputChain == ""))) {
		p.redetect(s, detect(s.config.Firewall))
	}
	p.reconcile(ctx, s)
	if checkRestart && p.ready {
		s.firewallRestartChecked.Store(requested)
	}
}

func (p *firewallPolicy) redetect(s *controlServer, backend string) {
	previous := s.firewall.Load()
	if backend == "" || previous.backend == backend {
		return
	}
	manager, err := newFirewallManager(s.config.NodeID, previous.config, backend)
	if err != nil {
		log.Printf("configure redetected firewall backend: %v", err)
	}
	switch previous.Status().DisabledReason {
	case "waiting_panel":
		manager.status.DisabledReason = "waiting_panel"
		p.legacy, _ = newFirewallManager(s.config.NodeID, s.config.Firewall, backend)
	case "no_exit_endpoint":
		manager.status.DisabledReason = "no_exit_endpoint"
		if p.cleanup == nil {
			p.prepareCleanup(s, manager)
		}
	case "":
		if p.cleanup == nil {
			p.cleanup = previous
			p.cleanupRequired = true
		}
	}
	// A second detection may return to the backend we have not cleared yet.
	if p.cleanup != nil && p.cleanup.backend == backend && manager.status.DisabledReason == "" {
		p.cleanup = nil
		p.cleanupRequired = false
	}
	s.firewall.Store(manager)
}

func (p *firewallPolicy) refresh(ctx context.Context, s *controlServer, client nodegrpc.V1Client) {
	if client != nil {
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		config, err := client.GetNodeConfig(readCtx, &emptypb.Empty{})
		cancel()
		if err == nil && p.update(s, config.HasExitEndpoint) && s.config.ConfigFile != "" {
			p.pendingCache = config
		}
	}
	// Retry a failed disk write even if the panel has since disconnected.
	if p.pendingCache == nil {
		return
	}
	if err := storePanelConfig(s.config.ConfigFile, p.pendingCache); err != nil {
		log.Printf("cache automatic firewall policy: %v", err)
		return
	}
	p.pendingCache = nil
}

func (p *firewallPolicy) update(s *controlServer, hasExitEndpoint *bool) bool {
	previous := s.firewall.Load()
	if !p.ready {
		p.ready = true
		previous = p.legacy
		if hasExitEndpoint == nil {
			s.firewall.Store(previous)
		}
	}
	// An old panel or interrupted lookup does not revoke a known decision.
	if hasExitEndpoint == nil || (p.hasExitEndpoint != nil && *p.hasExitEndpoint == *hasExitEndpoint) {
		return false
	}
	config := s.config.Firewall
	config.Enabled = hasExitEndpoint
	manager, err := newFirewallManager(s.config.NodeID, config, previous.backend)
	if err != nil {
		log.Printf("configure firewall for exit endpoints: %v", err)
	}
	p.hasExitEndpoint = hasExitEndpoint
	if *hasExitEndpoint && p.cleanup != nil && (!p.cleanupRequired || p.cleanup.backend == manager.backend) {
		p.cleanup = nil
		p.cleanupRequired = false
	}
	if !*hasExitEndpoint {
		manager.status.DisabledReason = "no_exit_endpoint"
		if p.cleanup == nil {
			p.prepareCleanup(s, previous)
		}
	}
	s.firewall.Store(manager)
	return true
}

func (p *firewallPolicy) prepareCleanup(s *controlServer, previous *FirewallManager) {
	if previous.Status().DisabledReason == "" {
		p.cleanup = previous
		p.cleanupRequired = true
		return
	}
	// An earlier daemon may have left allowances when it stopped. Check a valid
	// backend without treating a host that was never managed as an error.
	config := s.config.Firewall
	enabled := true
	config.Enabled = &enabled
	if manager, err := newFirewallManager(s.config.NodeID, config, previous.backend); err == nil && manager.Status().Supported {
		p.cleanup = manager
	}
}

func (p *firewallPolicy) reconcile(ctx context.Context, s *controlServer) {
	if !p.ready {
		return
	}
	manager := s.firewall.Load()
	if p.cleanup != nil {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		err := p.cleanup.clear(ctx)
		if err != nil {
			if p.cleanupRequired {
				manager.recordSyncError(fmt.Errorf("clear automatic firewall rules: %w", err))
			}
			return
		}
		if manager.configErr == nil {
			manager.statusMu.Lock()
			manager.status.LastError = nil
			manager.statusMu.Unlock()
		}
		p.cleanup = nil
	}
	if manager.Status().DisabledReason == "" {
		s.syncFirewall(ctx, manager)
	}
}

func (s *controlServer) syncFirewall(ctx context.Context, manager *FirewallManager) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ports, err := s.firewallPorts(ctx)
	if err != nil {
		manager.recordSyncError(err)
		return
	}
	_ = manager.Reconcile(ctx, ports)
}

func (s *controlServer) firewallPorts(ctx context.Context) (FirewallPorts, error) {
	// Gost loads its saved listeners on boot and reports their current state over loopback.
	reply, err := s.Execute(ctx, &nodegrpc.CommandRequest{Method: http.MethodGet, Path: "/config"})
	if err != nil {
		return FirewallPorts{}, fmt.Errorf("read Gost listeners: %w", err)
	}
	if reply.GetStatusCode() != http.StatusOK {
		return FirewallPorts{}, fmt.Errorf("read Gost listeners: HTTP %d", reply.GetStatusCode())
	}
	var config struct {
		Services json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(reply.GetBody(), &config); err != nil {
		return FirewallPorts{}, fmt.Errorf("decode Gost configuration: %w", err)
	}
	if config.Services == nil {
		return FirewallPorts{}, fmt.Errorf("Gost configuration is missing services")
	}
	var services []gost.ServiceConfig
	if err := json.Unmarshal(config.Services, &services); err != nil {
		return FirewallPorts{}, fmt.Errorf("decode Gost services: %w", err)
	}
	return managedFirewallPorts(services), nil
}

func managedFirewallPorts(services []gost.ServiceConfig) FirewallPorts {
	var ports FirewallPorts
	for _, service := range services {
		if !service.IsManagedForwarder() || service.Listener == nil || service.Status == nil ||
			(service.Status.State != "running" && service.Status.State != "ready") {
			continue
		}
		host, rawPort, err := net.SplitHostPort(service.Addr)
		if err != nil {
			continue
		}
		port, err := strconv.ParseUint(rawPort, 10, 16)
		if err != nil || port == 0 {
			continue
		}
		var addr netip.Addr
		if host != "" {
			addr, err = netip.ParseAddr(host)
			if err != nil || addr.Zone() != "" {
				continue
			}
			addr = addr.Unmap()
			if addr.IsLoopback() || addr.IsUnspecified() {
				continue
			}
		}
		switch service.Listener.Network() {
		case "tcp":
			addFirewallEndpoint(&ports.TCP, &ports.TCPAddresses, addr, uint16(port))
		case "udp":
			addFirewallEndpoint(&ports.UDP, &ports.UDPAddresses, addr, uint16(port))
		}
	}
	return ports
}

func addFirewallEndpoint(ports *[]uint16, addresses *[]FirewallEndpoint, addr netip.Addr, port uint16) {
	if addr.IsValid() {
		*addresses = append(*addresses, FirewallEndpoint{Address: addr, Port: port})
	} else {
		*ports = append(*ports, port)
	}
}
