package nodebridge

import (
	"context"

	"yagp/pkg/nodegrpc"

	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *controlServer) GetFirewallStatus(context.Context, *emptypb.Empty) (*nodegrpc.FirewallStatus, error) {
	checked := s.firewallRestartChecked.Load()
	manager := s.firewall.Load()
	if manager == nil {
		return &nodegrpc.FirewallStatus{}, nil
	}
	state := manager.Status()
	if checked != s.firewallRestartRequested.Load() {
		state.DisabledReason = "checking_firewall"
		state.Synchronized = false
		state.LastError = nil
	}
	return firewallStatusReply(state), nil
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
