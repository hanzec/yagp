package service

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"

	bizerrors "yagp/internal/errors"
	"yagp/internal/model"
	"yagp/internal/repository"
	"yagp/pkg/gost"
	"yagp/pkg/logger"
	"yagp/pkg/nodegrpc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Endpoint membership and its live Gost listeners are updated together.
var endpointTunnelRuntime sync.RWMutex

func (s *TunnelService) runningEndpointTunnels(column string, id, exclude uint) ([]*model.GostTunnel, error) {
	var tunnels []*model.GostTunnel
	err := s.tunnelRepo.DB.Where(column+" = ? AND id <> ? AND status = ?", id, exclude, model.TunnelStatusRunning).
		Order("id").Preload("EntryNode").Preload("ExitNode").Preload("EntryRule").Preload("ExitRule").Find(&tunnels).Error
	for _, tunnel := range tunnels {
		tunnel.EntryRule, tunnel.ExitRule = tunnel.EntryRule.WithAppliedEndpoint(), tunnel.ExitRule.WithAppliedEndpoint()
	}
	return tunnels, err
}

func (s *TunnelService) checkEndpointRestart(t *model.GostTunnel) error {
	var pending []uint
	for _, endpoint := range []*model.GostRule{t.EntryRule, t.ExitRule} {
		if endpoint.AppliedEndpoint != nil {
			pending = append(pending, endpoint.ID)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	bindings, err := repository.NewRuleRepository(s.tunnelRepo.DB).EntranceBindings(pending)
	if err != nil {
		return err
	}
	for _, bound := range bindings {
		if bound.ID != t.ID && bound.Status == model.TunnelStatusRunning {
			return bizerrors.ErrTunnelEndpointRestart
		}
	}
	return nil
}

func (s *TunnelService) configureSharedRelay(client *gost.Client, relay *gost.ServiceConfig, tunnels []*model.GostTunnel) error {
	if err := s.allowlist.writeSharedRelayPolicy(client, relay.Name, tunnels); err != nil {
		return err
	}
	current, err := client.Service(relay.Name)
	if err != nil {
		return err
	}
	if current != nil && current.Handler != nil && current.Handler.Auther == relay.Name &&
		current.Bypass == relay.Bypass && slices.Equal(current.Bypasses, relay.Bypasses) {
		return nil
	}
	address, err := client.ObserverAddress()
	if err != nil {
		return err
	}
	if err := client.EnsureRelayAuther(relay.Name, address, client.ObserverToken()); err != nil {
		return err
	}
	relay.Handler.Auth = nil
	relay.Handler.Auther = relay.Name
	return client.ReplaceService(relay)
}

func (s *GostAllowlistService) writeSharedRelayPolicy(client *gost.Client, name string, tunnels []*model.GostTunnel) error {
	policy := nodegrpc.RelayAuthConfig{Users: make([]nodegrpc.RelayAuthUser, 0, len(tunnels))}
	for _, tunnel := range tunnels {
		addresses, restrict, err := s.relaySources(tunnel)
		if err != nil {
			return err
		}
		user := nodegrpc.RelayAuthUser{
			Username: relayServiceName(tunnel.ID), Password: tunnel.RelaySecret,
			RestrictSources: restrict, AllowedIPs: addresses,
		}
		policy.Users = append(policy.Users, user)
	}
	err := client.ConfigureRelayAuth(name, policy)
	if status.Code(err) == codes.Unimplemented {
		return bizerrors.ErrTunnelSharedDaemon
	}
	return err
}

func (s *TunnelService) removeEndpointRelay(client *gost.Client, tunnel *model.GostTunnel, peers []*model.GostTunnel) error {
	if len(peers) == 0 {
		name := tunnel.ServiceID
		if name == "" {
			name = relayServiceName(tunnel.ID)
		}
		if tunnel.ExitRule.Role != model.EndpointRoleExit {
			return client.DeleteService(name)
		}
		shared, err := client.RelayAuthConfigured(name)
		if err != nil {
			return err
		}
		if err := client.DeleteService(name); err != nil || !shared {
			return err
		}
		return client.DeleteRelayAuther(name)
	}
	name := peers[0].ServiceID
	current, err := client.Service(name)
	if err != nil {
		return err
	}
	if current == nil || current.Handler == nil || current.Handler.Auther != name {
		// Rollback can arrive before the shared auth plugin was applied.
		return nil
	}
	return s.allowlist.writeSharedRelayPolicy(client, name, peers)
}

func (s *TunnelService) applySharedIngress(client *gost.Client, t *model.GostTunnel, tunnels []*model.GostTunnel, redirect *nodegrpc.TransparentRedirectRequest) error {
	if redirect == nil {
		return s.applySharedFixedIngress(client, t.EntryRule, tunnels)
	}
	rules := NewRuleService(s.tunnelRepo.DB)
	for _, tunnel := range tunnels {
		source := endpointTargetRule(tunnel)
		if _, err := rules.prepareTransparentBypass(client, transparentPolicyName(tunnel.ID), &source); err != nil {
			return err
		}
	}
	return s.applySharedTransparentIngress(client, t.EntryRule, tunnels, redirect)
}

func (s *TunnelService) applySharedTransparentIngress(client *gost.Client, endpoint *model.GostRule, tunnels []*model.GostTunnel, redirect *nodegrpc.TransparentRedirectRequest) error {
	group, sniff := sharedTransparentGroup(tunnels)
	for _, tunnel := range tunnels {
		// The group evaluates the policy before selecting the marked route.
		if err := client.EnsureMarkedTransparentChain(tunnel.ChainID, sharedTransparentChainName(tunnel.ID), redirect.GetMark(), ""); err != nil {
			return err
		}
	}
	rules := NewRuleService(s.tunnelRepo.DB)
	listener := gost.BuildMarkedTransparentRedirectService(entranceServiceName(endpoint.ID), redirect.GetMark(), sniff)
	listener.Addr = net.JoinHostPort(endpoint.ListenIP, strconv.Itoa(endpoint.ListenPort))
	listener.Handler.ChainGroup = group
	if err := rules.setupRuleObserver(client, endpoint, listener); err != nil {
		return err
	}
	if err := client.ReplaceService(listener); err != nil {
		if status.Code(err) == codes.InvalidArgument || status.Code(err) == codes.Unimplemented {
			return bizerrors.ErrTunnelSharedDaemon
		}
		return err
	}
	endpoint.ServiceID = listener.Name

	if redirect.GetDockerAddress() == "" {
		return nil
	}
	docker := gost.BuildMarkedTransparentRedirectService(dockerEntranceServiceName(endpoint.ID), redirect.GetMark(), sniff)
	docker.Addr = net.JoinHostPort(redirect.GetDockerAddress(), strconv.Itoa(endpoint.ListenPort))
	docker.Listener.Metadata = map[string]any{"tproxy": true}
	docker.Handler.ChainGroup = group
	return client.ReplaceService(docker)
}

func sharedTransparentGroup(tunnels []*model.GostTunnel) (*gost.ChainGroupConfig, bool) {
	var matches []string
	group := &gost.ChainGroupConfig{Selector: &gost.SelectorConfig{Strategy: "round"}}
	for _, tunnel := range tunnels {
		source := endpointTargetRule(tunnel)
		entry := gost.ChainGroupEntry{Chain: sharedTransparentChainName(tunnel.ID)}
		if transparentFiltering(&source) {
			name := transparentPolicyName(tunnel.ID)
			if source.UseClashRuleset && !hasSelectedRules(&source) {
				name = gost.TransparentBypassName
			}
			rule := fmt.Sprintf("Bypass(%q)", name)
			matches = append(matches, rule)
			entry.Matcher = &gost.ChainMatcher{Rule: rule}
		}
		group.Chains = append(group.Chains, entry)
	}
	if len(matches) != 0 {
		fallback := &gost.ChainMatcher{Rule: "!(" + strings.Join(matches, " || ") + ")"}
		for i, entry := range group.Chains {
			if entry.Matcher == nil {
				group.Chains[i].Matcher = fallback
			}
		}
	}
	return group, len(matches) != 0
}

func sharedTransparentChainName(id uint) string {
	return fmt.Sprintf("tunnel-%d-shared-chain", id)
}

func (s *TunnelService) restoreEndpointIngress(client *gost.Client, peers []*model.GostTunnel) error {
	first := peers[0]
	source := endpointTargetRule(first)
	if len(peers) == 1 {
		current, err := client.Service(entranceServiceName(first.EntryRule.ID))
		if err != nil {
			return err
		}
		if current != nil && current.Handler != nil && current.Handler.Chain != "" {
			// A failed start may leave the original single-tunnel listener untouched.
			if source.UseOriginalTarget || source.UseClashRuleset {
				docker, err := client.Service(dockerEntranceServiceName(first.EntryRule.ID))
				if err != nil {
					return err
				}
				if docker == nil || docker.Handler == nil || docker.Handler.ChainGroup == nil {
					return client.DeleteChain(sharedTransparentChainName(first.ID))
				}
			} else {
				return client.DeleteService(sharedFixedIngressServiceName(first.EntryRule.ID, first.ID))
			}
		}
	}
	if source.UseOriginalTarget || source.UseClashRuleset {
		redirect, err := prepareEndpointRedirect(first, client, relayHost(first.ExitRule, first.ExitNode), peers[1:])
		if err != nil {
			return err
		}
		if err := s.applySharedTransparentIngress(client, first.EntryRule, peers, redirect); err != nil {
			return err
		}
		redirect.RelayHosts = relayHosts(peers)
		redirect.Action = nodegrpc.TransparentRedirectAction_TRANSPARENT_REDIRECT_ACTION_APPLY
		_, err = client.ConfigureTransparentRedirect(context.Background(), redirect)
		return err
	}
	return s.applySharedFixedIngress(client, first.EntryRule, peers)
}

func validateSharedIngress(tunnel *model.GostTunnel, peers []*model.GostTunnel) error {
	source := endpointTargetRule(tunnel)
	transparent := source.UseOriginalTarget || source.UseClashRuleset
	for _, peer := range peers {
		rule := endpointTargetRule(peer)
		if transparent != (rule.UseOriginalTarget || rule.UseClashRuleset) {
			return bizerrors.ErrTunnelSharedIngressPolicy
		}
	}
	return nil
}

func relayHosts(tunnels []*model.GostTunnel) []string {
	hosts := make([]string, 0, len(tunnels))
	for _, tunnel := range tunnels {
		if host := relayHost(tunnel.ExitRule, tunnel.ExitNode); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

func (s *TunnelService) applySharedFixedIngress(client *gost.Client, endpoint *model.GostRule, tunnels []*model.GostTunnel) error {
	targets := make([]string, 0, len(tunnels))
	for _, tunnel := range tunnels {
		address, err := ensureSharedFixedIngressRoute(client, endpoint, tunnel)
		if err != nil {
			logger.Warnf("配置入口 %d 的隧道 %d 本机转发失败: %v", endpoint.ID, tunnel.ID, err)
			return bizerrors.ErrRuleStartFailed
		}
		targets = append(targets, address)
	}

	listener := buildFixedIngressService(endpoint, entranceServiceName(endpoint.ID), endpoint.ListenIP, endpoint.ListenPort, targets, "round")
	rules := NewRuleService(s.tunnelRepo.DB)
	if err := rules.setupRuleObserver(client, endpoint, listener); err != nil {
		return err
	}
	if err := client.ReplaceService(listener); err != nil {
		if code := status.Code(err); code == codes.InvalidArgument || code == codes.Unimplemented {
			return bizerrors.ErrTunnelSharedDaemon
		}
		logger.Warnf("配置入口 %d 的共享监听失败: %v", endpoint.ID, err)
		return bizerrors.ErrRuleStartFailed
	}
	endpoint.ServiceID = listener.Name
	return nil
}

func ensureSharedFixedIngressRoute(client *gost.Client, endpoint *model.GostRule, tunnel *model.GostTunnel) (string, error) {
	const loopback = "127.0.0.1"
	name := sharedFixedIngressServiceName(endpoint.ID, tunnel.ID)
	existing, err := client.Service(name)
	if err != nil {
		return "", err
	}
	if existing != nil {
		host, _, err := net.SplitHostPort(existing.Addr)
		if err != nil || host != loopback || existing.Handler == nil || existing.Handler.Chain != tunnel.ChainID ||
			existing.Listener == nil || existing.Listener.Type != string(endpoint.Protocol) {
			return "", fmt.Errorf("invalid local forwarding service %s", name)
		}
		return existing.Addr, nil
	}

	policy := endpointTargetRule(tunnel)
	for range 3 {
		port := 49152 + rand.IntN(16384)
		route := buildFixedIngressService(endpoint, name, loopback, port, policy.Targets, policy.Strategy)
		route.Handler.Chain = tunnel.ChainID
		if _, err = client.EnsureService(route); err == nil {
			return route.Addr, nil
		}
		if !strings.Contains(strings.ToLower(err.Error()), "address already in use") {
			return "", err
		}
	}
	return "", err
}

func buildFixedIngressService(endpoint *model.GostRule, name, host string, port int, targets []string, strategy string) *gost.ServiceConfig {
	if len(targets) == 1 {
		strategy = "round"
	}
	var service *gost.ServiceConfig
	if endpoint.Protocol == model.RuleProtocolUDP {
		service = gost.BuildUDPForwardService(name, port, targets, strategy)
	} else {
		service = gost.BuildTCPForwardService(name, port, targets, strategy)
	}
	service.Addr = net.JoinHostPort(host, strconv.Itoa(port))
	return service
}

func sharedFixedIngressServiceName(entryID, tunnelID uint) string {
	return fmt.Sprintf("rule-%d-tunnel-%d", entryID, tunnelID)
}
