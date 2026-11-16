package service

import (
	"context"
	"slices"
	"sync"
	"time"

	"yagp/internal/model"
	"yagp/internal/utils"
	"yagp/pkg/logger"
	"yagp/pkg/nodegrpc"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const tunnelProbeConcurrency = 8

func (s *TunnelService) RunMonitor(ctx context.Context) {
	probe := time.NewTicker(tunnelProbeInterval)
	defer probe.Stop()
	cleanup := time.NewTicker(time.Hour)
	defer cleanup.Stop()
	s.pruneTunnelProbes(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-probe.C:
			s.sampleRunningTunnels(ctx)
		case now := <-cleanup.C:
			s.pruneTunnelProbes(ctx, now)
		}
	}
}

func sampleTunnel(ctx context.Context, tunnel *model.GostTunnel) model.TunnelProbeSample {
	sample := model.TunnelProbeSample{
		TunnelID: tunnel.ID, SampledAt: time.Now().Unix(), Status: model.TunnelProbeUnavailable,
	}
	if tunnel.ChainID == "" || endpointNodesOnline(tunnel) != nil {
		return sample
	}

	ctx, cancel := context.WithTimeout(ctx, tunnelTestTimeout)
	defer cancel()
	endpoint, err := utils.GetGostClient(tunnel.ExitNode).StartTunnelProbe(ctx)
	if err != nil {
		return sample
	}
	reply, err := utils.GetGostClient(tunnel.EntryNode).ProbeTunnel(ctx, &nodegrpc.TunnelProbeRequest{
		ChainName: tunnel.ChainID, Address: endpoint.GetAddress(), Token: endpoint.GetToken(), MeasureReplyIpTtl: true,
	})
	if err != nil {
		recordTunnelProbeFailure(&sample, err)
		return sample
	}
	latency := float64(reply.GetRoundTripMs())
	sample.Status = model.TunnelProbeSuccessful
	sample.LatencyMS = &latency
	sample.TTL = probeReplyTTL(reply)
	return sample
}

func recordTunnelProbeFailure(sample *model.TunnelProbeSample, err error) {
	sample.Status = model.TunnelProbeFailed
	st := status.Convert(err)
	switch st.Code() {
	case codes.Unimplemented, codes.Canceled, codes.InvalidArgument, codes.NotFound, codes.ResourceExhausted, codes.FailedPrecondition, codes.Internal:
		sample.Status = model.TunnelProbeUnavailable
	}
	if tunnelProbeSessionFailure(err) != "" {
		sample.Status = model.TunnelProbeUnavailable
	}
	for _, detail := range st.Details() {
		switch info := detail.(type) {
		case *nodegrpc.TunnelProbeReply:
			sample.TTL = probeReplyTTL(info)
		case *errdetails.ErrorInfo:
			if info.GetDomain() != "yagp.tunnel_probe" {
				continue
			}
			switch info.GetReason() {
			case "entry_gost_api", "entry_chain", "entry_gost_process":
				sample.Status = model.TunnelProbeUnavailable
			}
			if info.GetMetadata()["gost_error"] == "relay_target_forbidden" {
				sample.Status = model.TunnelProbeUnavailable
			}
		}
	}
}

func probeReplyTTL(reply *nodegrpc.TunnelProbeReply) *uint32 {
	ttl := reply.GetReplyIpTtl()
	if ttl == 0 || ttl > 255 {
		return nil
	}
	return &ttl
}

func (s *TunnelService) pruneTunnelProbes(ctx context.Context, now time.Time) {
	if err := s.tunnelRepo.PruneProbeHistory(ctx, now.Add(-tunnelProbeRetention).Unix()); err != nil && ctx.Err() == nil {
		logger.Warnf("清理隧道监测记录失败: %v", err)
	}
}

func (s *TunnelService) sampleRunningTunnels(ctx context.Context) {
	tunnels, err := s.tunnelRepo.RunningTunnelsForProbe(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logger.Warnf("查询运行中的隧道失败: %v", err)
		}
		return
	}
	for batch := range slices.Chunk(tunnels, tunnelProbeConcurrency) {
		if ctx.Err() != nil {
			return
		}
		samples := make([]model.TunnelProbeSample, len(batch))
		var probes sync.WaitGroup
		for i := range batch {
			probes.Go(func() { samples[i] = sampleTunnel(ctx, &batch[i]) })
		}
		probes.Wait()
		if err := s.tunnelRepo.SaveProbeSamples(ctx, samples); err != nil && ctx.Err() == nil {
			logger.Warnf("保存隧道监测记录失败: %v", err)
		}
	}
}
