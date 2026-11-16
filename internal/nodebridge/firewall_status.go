package nodebridge

import (
	"context"

	"yagp/pkg/nodegrpc"

	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *controlServer) GetFirewallStatus(context.Context, *emptypb.Empty) (*nodegrpc.FirewallStatus, error) {
	manager := s.firewallManager()
	if manager == nil {
		return &nodegrpc.FirewallStatus{}, nil
	}
	return firewallStatusReply(manager.Status()), nil
}

func (s *controlServer) firewallManager() *FirewallManager {
	s.firewallMu.RLock()
	defer s.firewallMu.RUnlock()
	return s.firewall
}

func firewallStatusReply(state FirewallStatus) *nodegrpc.FirewallStatus {
	reply := &nodegrpc.FirewallStatus{
		Supported: state.Supported, Managed: state.Managed, Synchronized: state.Synchronized,
		LastError: state.LastError, Backend: state.Backend, DisabledReason: state.DisabledReason,
	}
	if state.LastSync != nil {
		reply.LastSync = timestamppb.New(*state.LastSync)
	}
	return reply
}
