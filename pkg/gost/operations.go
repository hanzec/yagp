package gost

import (
	"context"
	"time"

	"yagp/pkg/nodegrpc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const nodeMutationTimeout = 20 * time.Second

func (c *Client) CheckListener(ctx context.Context, protocol, ip string, port uint32) (*nodegrpc.ListenerCheckReply, error) {
	return callNode(ctx, c, c.config.Timeout, func(ctx context.Context, client nodegrpc.V1Client) (*nodegrpc.ListenerCheckReply, error) {
		return client.CheckListener(ctx, &nodegrpc.ListenerCheckRequest{Protocol: protocol, Ip: ip, Port: port})
	})
}

func (c *Client) StartTunnelProbe(ctx context.Context) (*nodegrpc.TunnelProbeEndpoint, error) {
	return callNode(ctx, c, c.config.Timeout, func(ctx context.Context, client nodegrpc.V1Client) (*nodegrpc.TunnelProbeEndpoint, error) {
		return client.StartTunnelProbe(ctx, &emptypb.Empty{})
	})
}

func (c *Client) ProbeTunnel(ctx context.Context, req *nodegrpc.TunnelProbeRequest) (*nodegrpc.TunnelProbeReply, error) {
	return callNode(ctx, c, 9*time.Second, func(ctx context.Context, client nodegrpc.V1Client) (*nodegrpc.TunnelProbeReply, error) {
		return client.ProbeTunnel(ctx, req)
	})
}

func (c *Client) GostServiceStatus(ctx context.Context) (*nodegrpc.GostServiceStatus, error) {
	return callNode(ctx, c, c.config.Timeout, func(ctx context.Context, client nodegrpc.V1Client) (*nodegrpc.GostServiceStatus, error) {
		return client.GetGostServiceStatus(ctx, &emptypb.Empty{})
	})
}

func (c *Client) RestartGostService(ctx context.Context) (*nodegrpc.GostServiceStatus, error) {
	return callNode(ctx, c, nodeMutationTimeout, func(ctx context.Context, client nodegrpc.V1Client) (*nodegrpc.GostServiceStatus, error) {
		return client.RestartGostService(ctx, &emptypb.Empty{})
	})
}

func (c *Client) GostLogs(ctx context.Context) (*nodegrpc.GostLogsReply, error) {
	return callNode(ctx, c, c.config.Timeout, func(ctx context.Context, client nodegrpc.V1Client) (*nodegrpc.GostLogsReply, error) {
		return client.GetGostLogs(ctx, &nodegrpc.GostLogsRequest{Lines: 100})
	})
}

func (c *Client) GetNftConfig(ctx context.Context) (*nodegrpc.NftConfig, error) {
	return callNode(ctx, c, c.config.Timeout, func(ctx context.Context, client nodegrpc.V1Client) (*nodegrpc.NftConfig, error) {
		return client.GetNftConfig(ctx, &emptypb.Empty{})
	})
}

func (c *Client) GetFirewallStatus(ctx context.Context) (*nodegrpc.FirewallStatus, error) {
	return callNode(ctx, c, c.config.Timeout, func(ctx context.Context, client nodegrpc.V1Client) (*nodegrpc.FirewallStatus, error) {
		return client.GetFirewallStatus(ctx, &emptypb.Empty{})
	})
}

func (c *Client) ApplyNftConfig(ctx context.Context, config string) (*nodegrpc.NftConfig, error) {
	return callNode(ctx, c, nodeMutationTimeout, func(ctx context.Context, client nodegrpc.V1Client) (*nodegrpc.NftConfig, error) {
		return client.ApplyNftConfig(ctx, &nodegrpc.NftConfig{Config: config})
	})
}

func (c *Client) ConfigureTransparentRedirect(ctx context.Context, request *nodegrpc.TransparentRedirectRequest) (*nodegrpc.TransparentRedirectReply, error) {
	return callNode(ctx, c, nodeMutationTimeout, func(ctx context.Context, client nodegrpc.V1Client) (*nodegrpc.TransparentRedirectReply, error) {
		return client.ConfigureTransparentRedirect(ctx, request)
	})
}

func (c *Client) SyncTrafficQuota(ctx context.Context, limit, used uint64, windowStart int64, tcpPorts, udpPorts []uint32) (uint64, error) {
	reply, err := c.updateTrafficQuota(ctx, &nodegrpc.TrafficQuotaRequest{
		LimitBytes: limit, UsedBytes: used, WindowStart: windowStart, TcpPorts: tcpPorts, UdpPorts: udpPorts,
	})
	return reply.GetUsedBytes(), err
}

func (c *Client) CalibrateTrafficQuota(ctx context.Context, request *nodegrpc.TrafficQuotaRequest) (uint64, error) {
	reply, err := c.updateTrafficQuota(ctx, request)
	if err == nil && !reply.GetCalibrated() {
		return 0, status.Error(codes.Unimplemented, "请升级节点守护程序后再校准配额用量")
	}
	return reply.GetUsedBytes(), err
}

func (c *Client) updateTrafficQuota(ctx context.Context, request *nodegrpc.TrafficQuotaRequest) (*nodegrpc.TrafficQuotaReply, error) {
	return callNode(ctx, c, nodeMutationTimeout, func(ctx context.Context, client nodegrpc.V1Client) (*nodegrpc.TrafficQuotaReply, error) {
		return client.SyncTrafficQuota(ctx, request)
	})
}

func (c *Client) GetUpgradeStatus(ctx context.Context) (*nodegrpc.NodeUpgradeStatus, error) {
	return callNode(ctx, c, c.config.Timeout, func(ctx context.Context, client nodegrpc.V1Client) (*nodegrpc.NodeUpgradeStatus, error) {
		return client.GetUpgradeStatus(ctx, &emptypb.Empty{})
	})
}

func (c *Client) StartUpgrade(ctx context.Context, component, version string) (*nodegrpc.NodeUpgradeStatus, error) {
	return callNode(ctx, c, c.config.Timeout, func(ctx context.Context, client nodegrpc.V1Client) (*nodegrpc.NodeUpgradeStatus, error) {
		return client.StartUpgrade(ctx, &nodegrpc.NodeUpgradeRequest{Component: component, Version: version})
	})
}

func callNode[T any](ctx context.Context, c *Client, timeout time.Duration, call func(context.Context, nodegrpc.V1Client) (*T, error)) (*T, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return call(ctx, client)
}
