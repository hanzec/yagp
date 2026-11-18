package service

import (
	"yagp/internal/errors"
	"yagp/internal/model"
	"yagp/internal/repository"
	"yagp/pkg/logger"
)

func (s *TunnelService) startEndpointTunnel(t *model.GostTunnel) error {
	peers, err := s.endpointRestartPeers(t)
	if err != nil {
		return err
	}
	if len(peers) != 0 {
		if err := s.restartEndpointPeers(t, peers); err != nil {
			return err
		}
		// Stopping the last user clears the shared endpoint's runtime service and snapshot.
		fresh, err := s.tunnelRepo.FindByID(t.ID)
		if err != nil {
			return err
		}
		*t = *fresh
	}
	return s.startEndpointTunnelRuntime(t)
}

func (s *TunnelService) endpointRestartPeers(t *model.GostTunnel) ([]*model.GostTunnel, error) {
	rules := repository.NewRuleRepository(s.tunnelRepo.DB)
	group := []*model.GostTunnel{t}
	seenTunnels := map[uint]bool{t.ID: true}
	seenEndpoints := make(map[uint]bool)
	// A restarted peer may also use another edited endpoint with running users.
	for i := 0; i < len(group); i++ {
		var pending []uint
		for _, endpoint := range []*model.GostRule{group[i].EntryRule, group[i].ExitRule} {
			if endpoint == nil || endpoint.Role == model.EndpointRoleExit || seenEndpoints[endpoint.ID] {
				continue
			}
			seenEndpoints[endpoint.ID] = true
			if endpoint.AppliedEndpoint != nil && !endpoint.AppliedEndpoint.Equal(endpoint.EndpointConfig()) {
				pending = append(pending, endpoint.ID)
			}
		}
		if len(pending) == 0 {
			continue
		}
		bindings, err := rules.EntranceBindings(pending)
		if err != nil {
			return nil, err
		}
		for _, binding := range bindings {
			if binding.Status != model.TunnelStatusRunning || seenTunnels[binding.ID] {
				continue
			}
			peer, err := s.tunnelRepo.FindByID(binding.ID)
			if err != nil {
				return nil, err
			}
			group = append(group, peer)
			seenTunnels[binding.ID] = true
		}
	}
	return group[1:], nil
}

func (s *TunnelService) restartEndpointPeers(t *model.GostTunnel, peers []*model.GostTunnel) error {
	if err := s.validateEndpointPeerRestarts(append([]*model.GostTunnel{t}, peers...)); err != nil {
		return err
	}
	if t.Status == model.TunnelStatusError {
		if err := s.stopEndpointTunnel(t); err != nil {
			return err
		}
	}
	for i, peer := range peers {
		if err := s.stopEndpointTunnel(peer); err != nil {
			logger.Warnf("自动停止隧道 %d 失败: %v", peer.ID, err)
			// A failed stop can already have removed part of this peer's runtime.
			_ = s.resumeEndpointPeers(peers[:i+1], true)
			return errors.ErrTunnelEndpointRestart
		}
	}
	// Restore existing traffic before attempting to bring up the requested tunnel.
	return s.resumeEndpointPeers(peers, false)
}

func (s *TunnelService) validateEndpointPeerRestarts(group []*model.GostTunnel) error {
	byEntry := make(map[uint][]*model.GostTunnel)
	restarting := make(map[uint]bool)
	for _, tunnel := range group {
		if err := validateEndpointTunnelStart(tunnel); err != nil {
			return err
		}
		byEntry[tunnel.EntryRule.ID] = append(byEntry[tunnel.EntryRule.ID], tunnel)
		restarting[tunnel.ID] = true
	}
	for id, candidates := range byEntry {
		live, err := s.runningEndpointTunnels("entry_rule_id", id, 0)
		if err != nil {
			return err
		}
		for _, peer := range live {
			if !restarting[peer.ID] {
				candidates = append(candidates, peer)
			}
		}
		if err := validateSharedIngress(candidates[0], candidates[1:]); err != nil {
			return err
		}
	}
	return nil
}

func (s *TunnelService) resumeEndpointPeers(peers []*model.GostTunnel, useApplied bool) error {
	var failure error
	for _, peer := range peers {
		fresh, err := s.tunnelRepo.FindByID(peer.ID)
		if err == nil {
			if useApplied {
				// After a stop failure, any remaining live peers still pin their old settings.
				fresh.EntryRule, fresh.ExitRule = fresh.EntryRule.WithAppliedEndpoint(), fresh.ExitRule.WithAppliedEndpoint()
			}
			err = s.startEndpointTunnelRuntime(fresh)
		}
		if err != nil {
			logger.Warnf("自动启动隧道 %d 失败: %v", peer.ID, err)
			if fresh != nil && fresh.Status == model.TunnelStatusRunning {
				if statusErr := s.tunnelRepo.UpdateStatus(peer.ID, model.TunnelStatusError); statusErr != nil {
					logger.Warnf("更新隧道 %d 的错误状态失败: %v", peer.ID, statusErr)
				}
			}
			failure = errors.ErrTunnelEndpointRestart
		}
	}
	return failure
}
