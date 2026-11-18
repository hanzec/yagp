package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"

	bizerrors "yagp/internal/errors"
	"yagp/internal/model"
	"yagp/pkg/gost"

	"gorm.io/gorm"
)

var relaySharedNATRange = netip.MustParsePrefix("100.64.0.0/10")

type GostAllowlistService struct {
	db *gorm.DB
}

func NewGostAllowlistService(db *gorm.DB) *GostAllowlistService {
	return &GostAllowlistService{db: db}
}

func (s *GostAllowlistService) validateNodeIDs(ctx context.Context, nodeIDs []uint) ([]uint, error) {
	nodeIDs = append([]uint{}, nodeIDs...)
	slices.Sort(nodeIDs)
	nodeIDs = slices.Compact(nodeIDs)
	nodes, err := s.sourceNodes(ctx, nodeIDs)
	if err != nil {
		return nil, err
	}
	for _, id := range nodeIDs {
		node := nodes[id]
		if node == nil {
			return nil, bizerrors.New(40000, "白名单包含不存在的节点", http.StatusBadRequest)
		}
		if len(node.Addresses) == 0 && len(node.ManualAddresses) == 0 && node.IPv4 == "" && node.IPv6 == "" {
			return nil, bizerrors.New(40000, fmt.Sprintf("节点 %q 暂无来源 IP，请注册节点或等待节点上报", node.Name), http.StatusBadRequest)
		}
	}
	return nodeIDs, nil
}

type sourceNodes map[uint]*model.GostNode

func (s *GostAllowlistService) sourceNodes(ctx context.Context, ids []uint) (sourceNodes, error) {
	result := make(sourceNodes, len(ids))
	for batch := range slices.Chunk(ids, 1000) {
		var nodes []model.GostNode
		if err := s.db.WithContext(ctx).Select("id", "name", "ipv4", "ipv6", "addresses", "manual_addresses", "local_addresses").
			Where("id IN ?", batch).Find(&nodes).Error; err != nil {
			return nil, err
		}
		for i := range nodes {
			result[nodes[i].ID] = &nodes[i]
		}
	}
	return result, nil
}

func (nodes sourceNodes) addresses(ids []uint, includeLocal bool) []string {
	addresses := []string{}
	for _, id := range ids {
		node := nodes[id]
		if node == nil {
			continue
		}
		addresses = append(addresses, node.ManualAddresses...)
		if includeLocal {
			addresses = append(addresses, node.LocalAddresses...)
		}
		if len(node.Addresses) != 0 {
			addresses = append(addresses, node.Addresses...)
			continue
		}
		if node.IPv4 != "" {
			addresses = append(addresses, node.IPv4)
		}
		if node.IPv6 != "" {
			addresses = append(addresses, node.IPv6)
		}
	}
	slices.Sort(addresses)
	return slices.Compact(addresses)
}

func relayServiceName(id uint) string {
	return fmt.Sprintf("relay-tunnel-%d", id)
}

func (s *GostAllowlistService) prepareRelay(client *gost.Client, relay *gost.ServiceConfig, tunnel *model.GostTunnel) error {
	if !tunnel.AutoAllowlist && len(tunnel.AllowlistNodeIDs) == 0 {
		return nil
	}
	desired, err := s.relayAdmission(tunnel, nil)
	if err != nil {
		return err
	}
	if desired == nil {
		// A rebuilt relay may have a different entry; wait for its addresses instead of reusing an old admission.
		return nil
	}
	current, err := client.GetConfig()
	if err != nil {
		return err
	}
	if _, err = client.EnsureAdmission(*desired, current.Admissions); err != nil {
		return err
	}
	relay.Admission = gost.TunnelRelayAllowlistName(relay.Name)
	return nil
}

func (s *GostAllowlistService) relayAdmission(tunnel *model.GostTunnel, nodes sourceNodes) (*gost.AdmissionConfig, error) {
	nodeIDs := relaySourceIDs(tunnel)
	if nodes == nil {
		var err error
		nodes, err = s.sourceNodes(context.Background(), nodeIDs)
		if err != nil {
			return nil, err
		}
	}
	addresses := nodes.addresses(nodeIDs, tunnel.AutoAllowlist)
	if tunnel.AutoAllowlist && len(addresses) == 0 {
		return nil, nil
	}
	// A recovered sole survivor may still own a Relay named after a former peer.
	name := tunnel.ServiceID
	if name == "" {
		name = relayServiceName(tunnel.ID)
	}
	return &gost.AdmissionConfig{
		Name:      gost.TunnelRelayAllowlistName(name),
		Whitelist: len(nodeIDs) > 0,
		Matchers:  publicRelayMatchers(addresses),
	}, nil
}

func publicRelayMatchers(addresses []string) []string {
	matchers := make([]string, 0, len(addresses))
	for _, address := range addresses {
		ip, err := netip.ParseAddr(address)
		ip = ip.Unmap()
		if err == nil && ip.Zone() == "" && ip.IsGlobalUnicast() && !ip.IsPrivate() && !relaySharedNATRange.Contains(ip) {
			matchers = append(matchers, ip.String())
		}
	}
	slices.Sort(matchers)
	return slices.Compact(matchers)
}

func relaySourceIDs(tunnel *model.GostTunnel) []uint {
	if tunnel.AutoAllowlist {
		return []uint{tunnel.EntryNodeID}
	}
	return tunnel.AllowlistNodeIDs
}

func (s *GostAllowlistService) exitAdmission(name string, tunnels []*model.GostTunnel, nodes sourceNodes, previous *gost.AdmissionConfig) (*gost.AdmissionConfig, error) {
	if nodes == nil {
		var ids []uint
		for _, tunnel := range tunnels {
			ids = append(ids, relaySourceIDs(tunnel)...)
		}
		slices.Sort(ids)
		var err error
		nodes, err = s.sourceNodes(context.Background(), slices.Compact(ids))
		if err != nil {
			return nil, err
		}
	}
	admission := &gost.AdmissionConfig{Name: gost.TunnelRelayAllowlistName(name), Whitelist: true, Matchers: []string{}}
	known, missing := false, false
	for _, tunnel := range tunnels {
		ids := relaySourceIDs(tunnel)
		if len(ids) == 0 {
			admission.Whitelist = false
			admission.Matchers = []string{}
			return admission, nil
		}
		addresses := nodes.addresses(ids, tunnel.AutoAllowlist)
		if tunnel.AutoAllowlist && len(addresses) == 0 {
			missing = true
			continue
		}
		known = true
		admission.Matchers = append(admission.Matchers, publicRelayMatchers(addresses)...)
	}
	if missing && previous != nil {
		if !known {
			admission.Whitelist = previous.Whitelist
		}
		admission.Matchers = append(admission.Matchers, publicRelayMatchers(previous.Matchers)...)
	} else if !known {
		// Credentials protect a newly created exit while its entry has not reported an address.
		admission.Whitelist = false
	}
	slices.Sort(admission.Matchers)
	admission.Matchers = slices.Compact(admission.Matchers)
	return admission, nil
}

func (s *GostAllowlistService) writeRelaySources(client *gost.Client, relay *gost.ServiceConfig, tunnels []*model.GostTunnel) error {
	desired, err := s.exitAdmission(relay.Name, tunnels, nil, nil)
	if err != nil {
		return err
	}
	if !relay.HasAdmission(desired.Name) {
		if desired.Whitelist {
			return bizerrors.New(10242, "出口 Relay 尚未绑定 IP 白名单，无法在不重载服务的情况下启用来源限制", http.StatusConflict)
		}
		return nil
	}
	_, err = client.SetAdmission(*desired)
	return err
}

func (s *GostAllowlistService) syncRelays(client *gost.Client, current *gost.GostConfig, nodeID uint, tunnels []model.GostTunnel, nodes sourceNodes) (bool, error) {
	managed := make(map[string]model.GostTunnel)
	shared := make(map[string][]*model.GostTunnel)
	for i := range tunnels {
		tunnel := &tunnels[i]
		if tunnel.ExitNodeID == nodeID && tunnel.ServiceID != "" &&
			((tunnel.ExitRuleID == nil && tunnel.ServiceID == relayServiceName(tunnel.ID)) ||
				(tunnel.ExitRuleID != nil && tunnel.Status == model.TunnelStatusRunning)) {
			managed[tunnel.ServiceID] = *tunnel
		}
		if tunnel.ExitNodeID == nodeID && tunnel.ExitRuleID != nil && tunnel.Status == model.TunnelStatusRunning {
			shared[tunnel.ServiceID] = append(shared[tunnel.ServiceID], tunnel)
		}
	}
	var changed bool
	var syncError error
	for _, relay := range current.Services {
		if relay.Handler != nil && relay.Handler.Auther == relay.Name {
			if users := shared[relay.Name]; len(users) != 0 {
				syncError = errors.Join(syncError, s.writeSharedRelayPolicy(client, relay.Name, users, nodes))
			}
			continue
		}
		if users := shared[relay.Name]; len(users) != 0 && users[0].ExitRule != nil && users[0].ExitRule.Role == model.EndpointRoleExit {
			name := gost.TunnelRelayAllowlistName(relay.Name)
			if !relay.HasAdmission(name) {
				// Older unbound listeners are left intact until their normal stop/start.
				continue
			}
			var previous *gost.AdmissionConfig
			for i := range current.Admissions {
				if current.Admissions[i].Name == name {
					previous = &current.Admissions[i]
					break
				}
			}
			desired, err := s.exitAdmission(relay.Name, users, nodes, previous)
			if err != nil {
				syncError = errors.Join(syncError, err)
				continue
			}
			updated, err := client.EnsureAdmission(*desired, current.Admissions)
			changed = changed || updated
			syncError = errors.Join(syncError, err)
			continue
		}
		tunnel, managedRelay := managed[relay.Name]
		if !managedRelay || relay.Handler == nil || relay.Handler.Type != "relay" {
			continue
		}
		admissionName := gost.TunnelRelayAllowlistName(relay.Name)
		hasAdmission := relay.HasAdmission(admissionName)
		// Keep an existing per-tunnel binding when clearing the selection to avoid reloading the relay.
		enabled := tunnel.AutoAllowlist || len(tunnel.AllowlistNodeIDs) > 0 || hasAdmission
		if enabled {
			desired, err := s.relayAdmission(&tunnel, nodes)
			if err != nil {
				syncError = errors.Join(syncError, fmt.Errorf("%s: %w", relay.Name, err))
				continue
			}
			if desired == nil {
				// Keep the last known public addresses during a temporary loss of the entry snapshot.
				i := slices.IndexFunc(current.Admissions, func(admission gost.AdmissionConfig) bool {
					return admission.Name == admissionName
				})
				if !hasAdmission || i < 0 {
					continue
				}
				applied := current.Admissions[i]
				applied.Matchers = publicRelayMatchers(applied.Matchers)
				desired = &applied
			}
			updated, err := client.EnsureAdmission(*desired, current.Admissions)
			changed = changed || updated
			if err != nil {
				syncError = errors.Join(syncError, fmt.Errorf("%s: %w", relay.Name, err))
				continue
			}
		}
		if relay.HasAdmission(gost.LegacyRelayAllowlistName) || enabled != hasAdmission {
			updated, err := client.SetRelayAllowlist(relay.Name, enabled)
			changed = changed || updated
			if err != nil {
				syncError = errors.Join(syncError, fmt.Errorf("%s: %w", relay.Name, err))
			}
		}
	}
	return changed, syncError
}
