package service

import (
	"context"
	"time"

	"yagp/internal/model"
	"yagp/internal/utils"
	"yagp/pkg/nodegrpc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *NodeService) CurrentTrafficUsage(ctx context.Context, nodes []model.GostNode, now time.Time) (map[uint]uint64, error) {
	usage := make(map[uint]uint64, len(nodes))
	var uncapped []uint
	for i := range nodes {
		node := &nodes[i]
		if node.TrafficLimitBytes == 0 {
			uncapped = append(uncapped, node.ID)
		}
	}
	relays, err := s.nodeRepo.RelayUsageByNode(ctx, uncapped)
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		node := &nodes[i]
		if node.TrafficLimitBytes == 0 {
			if relayBytes, sampled := relays[node.ID]; sampled || node.TotalBytes > 0 {
				usage[node.ID] = lifetimeTrafficBytes(node.TotalBytes, relayBytes)
			}
			continue
		}
		if (node.TrafficQuotaState == model.TrafficQuotaActive || node.TrafficQuotaState == model.TrafficQuotaBlocked) &&
			node.TrafficQuotaWindowStart == model.TrafficQuotaWindow(node.TrafficResetPeriod, now) {
			usage[node.ID] = node.TrafficQuotaUsedBytes
		}
	}
	return usage, nil
}

func lifetimeTrafficBytes(observerBytes int64, relayBytes uint64) uint64 {
	return uint64(max(observerBytes, 0)) + relayBytes
}

func (s *NodeService) CalibrateNodeTotals(ctx context.Context, id uint, input, output int64) error {
	if _, err := s.GetByID(id); err != nil {
		return err
	}
	return s.nodeRepo.SetStats(ctx, id, input, output)
}

func (s *NodeService) CalibrateNodeQuota(ctx context.Context, id uint, used uint64) error {
	unlock := lockTrafficQuota(id)
	defer unlock()
	node, err := s.GetByID(id)
	if err != nil {
		return err
	}
	if node.TrafficLimitBytes <= 0 || node.Status != model.NodeStatusOnline {
		return status.Error(codes.FailedPrecondition, "校准配额用量需要在线且已设置流量上限的节点")
	}
	client := utils.GetGostClient(node)
	config, err := client.GetConfig()
	if err != nil {
		return err
	}
	window := model.TrafficQuotaWindow(node.TrafficResetPeriod, time.Now())
	var previous uint64
	if node.TrafficQuotaWindowStart == window {
		previous = node.TrafficQuotaUsedBytes
	}
	limit := uint64(node.TrafficLimitBytes)
	tcp, udp := quotaPorts(config.Services)
	reported, err := client.CalibrateTrafficQuota(ctx, &nodegrpc.TrafficQuotaRequest{
		LimitBytes: limit, UsedBytes: previous, WindowStart: window, TcpPorts: tcp, UdpPorts: udp,
		CalibratedUsedBytes: &used,
	})
	if err != nil {
		return err
	}
	state := model.TrafficQuotaActive
	if reported >= limit {
		state = model.TrafficQuotaBlocked
	}
	return s.nodeRepo.UpdateTrafficQuota(*node, window, reported, state)
}
