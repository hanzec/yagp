package service

import (
	"context"
	"errors"
	"time"

	"yagp/internal/model"
	"yagp/internal/utils"
	"yagp/pkg/gost"
	"yagp/pkg/logger"
)

const gostVersionRefreshInterval = 5 * time.Minute

func (s *RuleSyncService) updateNodeHealth(node model.GostNode, configErr error) {
	status := model.NodeStatusOnline
	if configErr != nil && !errors.Is(configErr, gost.ErrInvalidConfig) {
		status = model.NodeStatusOffline
	}
	if status != node.Status {
		logger.Infof("节点 %s 状态变更: %s -> %s", node.Name, node.Status, status)
		if err := s.nodeRepo.UpdateStatus(node.ID, status); err != nil {
			logger.Errorf("更新节点 %s 状态失败: %v", node.Name, err)
		}
	}
	if status == model.NodeStatusOnline {
		s.refreshGostVersion(node, time.Now())
	} else {
		_ = s.ruleRepo.StopByNodeID(node.ID)
		if tunnels, err := s.tunnelRepo.FindByNodeID(node.ID); err == nil && len(tunnels) > 0 {
			tunnelIDs := make([]uint, 0, len(tunnels))
			for _, tunnel := range tunnels {
				tunnelIDs = append(tunnelIDs, tunnel.ID)
			}
			_ = s.ruleRepo.StopByTunnelIDs(tunnelIDs)
		}
		_ = s.tunnelRepo.StopByNodeID(node.ID)
	}

	_ = s.nodeRepo.UpdateLastCheck(node.ID)
}

func (s *RuleSyncService) refreshGostVersion(node model.GostNode, now time.Time) {
	if node.GostVersionCheckedAt != nil && now.Before(node.GostVersionCheckedAt.Add(gostVersionRefreshInterval)) {
		return
	}
	// This RPC reports the installed binary version from the node.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	state, _ := utils.GetGostClient(&node).GetUpgradeStatus(ctx)
	if err := s.nodeRepo.UpdateGostVersion(node.ID, state.GetGostVersion(), now); err != nil {
		logger.Debugf("更新节点 %d Gost 版本失败: %v", node.ID, err)
	}
}
