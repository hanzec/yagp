package service

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yagp/internal/model"
	"yagp/pkg/nodegrpc"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type monitorTestNode struct {
	nodegrpc.UnimplementedV1Server
	endpoint *nodegrpc.TunnelProbeEndpoint
	probe    func(*nodegrpc.TunnelProbeRequest) (*nodegrpc.TunnelProbeReply, error)
}

func (n monitorTestNode) StartTunnelProbe(context.Context, *emptypb.Empty) (*nodegrpc.TunnelProbeEndpoint, error) {
	return n.endpoint, nil
}

func (n monitorTestNode) ProbeTunnel(_ context.Context, req *nodegrpc.TunnelProbeRequest) (*nodegrpc.TunnelProbeReply, error) {
	return n.probe(req)
}

func TestTunnelMonitorRecordsRelayOutcomeAndIndependentTTL(t *testing.T) {
	db := newServiceTestDB(t)
	requireServiceError(t, db.AutoMigrate(&model.TunnelProbeSample{}), nil)
	endpoint := &nodegrpc.TunnelProbeEndpoint{Address: "127.0.0.1:4001", Token: bytes.Repeat([]byte{0x71}, 32)}
	ttl := uint32(58)
	restricted, err := status.New(codes.Unavailable, "destination rejected").WithDetails(
		&errdetails.ErrorInfo{Domain: "yagp.tunnel_probe", Reason: "relay_read", Metadata: map[string]string{"gost_error": "relay_target_forbidden"}},
		&nodegrpc.TunnelProbeReply{ReplyIpTtl: &ttl},
	)
	requireServiceError(t, err, nil)

	reqs := make(chan *nodegrpc.TunnelProbeRequest, 4)
	entry := startServiceTestNode(t, monitorTestNode{probe: func(req *nodegrpc.TunnelProbeRequest) (*nodegrpc.TunnelProbeReply, error) {
		reqs <- req
		switch req.GetChainName() {
		case "tunnel-1-chain":
			return &nodegrpc.TunnelProbeReply{RoundTripMs: 0, ReplyIpTtl: &ttl}, nil
		case "tunnel-2-chain":
			return nil, restricted.Err()
		case "tunnel-3-chain":
			return nil, status.Error(codes.DeadlineExceeded, "relay timeout")
		default:
			return nil, status.Error(codes.InvalidArgument, "unexpected probe")
		}
	}})
	exit := startServiceTestNode(t, monitorTestNode{endpoint: endpoint})
	offline := model.GostNode{Name: "offline", Status: model.NodeStatusOffline}
	saveServiceFixtures(t, db, &entry, &exit, &offline)
	first := model.GostTunnel{Name: "running", EntryNodeID: entry.ID, ExitNodeID: exit.ID, ChainID: "tunnel-1-chain", Status: model.TunnelStatusRunning}
	second := model.GostTunnel{Name: "restricted", EntryNodeID: entry.ID, ExitNodeID: exit.ID, ChainID: "tunnel-2-chain", Status: model.TunnelStatusRunning}
	third := model.GostTunnel{Name: "timeout", EntryNodeID: entry.ID, ExitNodeID: exit.ID, ChainID: "tunnel-3-chain", Status: model.TunnelStatusRunning}
	fourth := model.GostTunnel{Name: "offline", EntryNodeID: offline.ID, ExitNodeID: exit.ID, ChainID: "tunnel-4-chain", Status: model.TunnelStatusRunning}
	stopped := model.GostTunnel{Name: "stopped", EntryNodeID: entry.ID, ExitNodeID: exit.ID, ChainID: "tunnel-5-chain", Status: model.TunnelStatusStopped}
	saveServiceFixtures(t, db, &first, &second, &third, &fourth, &stopped)

	svc := NewTunnelService(db)
	now := time.Now()
	saveServiceFixtures(t, db, &model.TunnelProbeSample{
		TunnelID: first.ID, SampledAt: now.Add(-tunnelProbeRetention - time.Second).Unix(), Status: model.TunnelProbeSuccessful,
	})
	svc.pruneTunnelProbes(t.Context(), now)
	svc.sampleRunningTunnels(t.Context())

	close(reqs)
	var sent int
	for req := range reqs {
		if !req.GetMeasureReplyIpTtl() || req.GetAddress() != endpoint.GetAddress() || !bytes.Equal(req.GetToken(), endpoint.GetToken()) {
			t.Fatalf("unexpected probe request: %v", req)
		}
		sent++
	}
	if sent != 3 {
		t.Fatalf("sent %d probes, want 3", sent)
	}
	var samples []model.TunnelProbeSample
	requireServiceError(t, db.Order("tunnel_id").Find(&samples).Error, nil)
	if len(samples) != 4 {
		t.Fatalf("saved samples = %+v, want one for each running tunnel", samples)
	}
	for i, want := range []struct {
		tunnelID uint
		status   model.TunnelProbeStatus
		hasRTT   bool
		hasTTL   bool
	}{
		{first.ID, model.TunnelProbeSuccessful, true, true},
		{second.ID, model.TunnelProbeUnavailable, false, true},
		{third.ID, model.TunnelProbeFailed, false, false},
		{fourth.ID, model.TunnelProbeUnavailable, false, false},
	} {
		got := samples[i]
		if got.TunnelID != want.tunnelID || got.Status != want.status || (got.LatencyMS != nil) != want.hasRTT ||
			(got.LatencyMS != nil && *got.LatencyMS != 0) || (got.TTL != nil) != want.hasTTL || (got.TTL != nil && *got.TTL != ttl) {
			t.Errorf("sample %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestMonitorStartsNextTunnelWhenOneProbeFinishes(t *testing.T) {
	db := newServiceTestDB(t)
	requireServiceError(t, db.AutoMigrate(&model.TunnelProbeSample{}), nil)
	started := make(chan string, tunnelProbeConcurrency+1)
	release := make(chan struct{})
	var active atomic.Int32
	entry := startServiceTestNode(t, monitorTestNode{probe: func(req *nodegrpc.TunnelProbeRequest) (*nodegrpc.TunnelProbeReply, error) {
		if n := active.Add(1); n > tunnelProbeConcurrency {
			t.Errorf("started %d concurrent probes, limit %d", n, tunnelProbeConcurrency)
		}
		defer active.Add(-1)
		started <- req.GetChainName()
		<-release
		return &nodegrpc.TunnelProbeReply{RoundTripMs: 1}, nil
	}})
	exit := startServiceTestNode(t, monitorTestNode{endpoint: &nodegrpc.TunnelProbeEndpoint{Address: "127.0.0.1:4001", Token: bytes.Repeat([]byte{1}, 32)}})
	saveServiceFixtures(t, db, &entry, &exit)
	for i := range tunnelProbeConcurrency + 1 {
		name := fmt.Sprintf("tunnel-%d-chain", i+1)
		saveServiceFixtures(t, db, &model.GostTunnel{
			Name: name, ChainID: name, Status: model.TunnelStatusRunning, EntryNodeID: entry.ID, ExitNodeID: exit.ID,
		})
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		NewTunnelService(db).sampleRunningTunnels(t.Context())
	}()
	finish := sync.OnceFunc(func() { close(release); <-done })
	t.Cleanup(finish)

	last := fmt.Sprintf("tunnel-%d-chain", tunnelProbeConcurrency+1)
	for range tunnelProbeConcurrency {
		select {
		case name := <-started:
			if name == last {
				t.Fatal("extra probe started before a slot was available")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("initial probes did not all start")
		}
	}
	release <- struct{}{}
	select {
	case name := <-started:
		if name != last {
			t.Fatalf("started %s, want %s", name, last)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("next tunnel still waiting after a probe slot became available")
	}
	finish()
	var saved int64
	requireServiceError(t, db.Model(&model.TunnelProbeSample{}).Where("status = ?", model.TunnelProbeSuccessful).Count(&saved).Error, nil)
	if saved != tunnelProbeConcurrency+1 {
		t.Fatalf("saved %d successful samples, want %d", saved, tunnelProbeConcurrency+1)
	}
}

func TestTunnelMonitorKeepsRelayTTLWhenDomainRetrySucceeds(t *testing.T) {
	endpoint := &nodegrpc.TunnelProbeEndpoint{
		Address: "127.0.0.1:4001", DomainAddress: nodegrpc.TunnelProbeDomain, Token: bytes.Repeat([]byte{0x71}, 32),
	}
	ttl := uint32(58)
	restricted, err := status.New(codes.Unavailable, "destination rejected").WithDetails(
		&errdetails.ErrorInfo{Domain: "yagp.tunnel_probe", Reason: "relay_read", Metadata: map[string]string{"gost_error": "relay_target_forbidden"}},
		&nodegrpc.TunnelProbeReply{ReplyIpTtl: &ttl},
	)
	requireServiceError(t, err, nil)
	entry := startServiceTestNode(t, monitorTestNode{probe: func(req *nodegrpc.TunnelProbeRequest) (*nodegrpc.TunnelProbeReply, error) {
		switch req.GetAddress() {
		case endpoint.GetAddress():
			if !req.GetMeasureReplyIpTtl() {
				t.Error("first probe did not request a TTL measurement")
			}
			return nil, restricted.Err()
		case endpoint.GetDomainAddress():
			if req.GetMeasureReplyIpTtl() {
				t.Error("domain retry requested another TTL measurement")
			}
			return &nodegrpc.TunnelProbeReply{RoundTripMs: 23}, nil
		default:
			return nil, status.Error(codes.InvalidArgument, "unexpected probe target")
		}
	}})
	exit := startServiceTestNode(t, monitorTestNode{endpoint: endpoint})
	sample := sampleTunnel(t.Context(), &model.GostTunnel{ChainID: "tunnel-1-chain", EntryNode: &entry, ExitNode: &exit})
	if sample.Status != model.TunnelProbeSuccessful || sample.LatencyMS == nil || *sample.LatencyMS != 23 {
		t.Fatalf("sample = %+v, want successful domain probe with 23 ms RTT", sample)
	}
	if sample.TTL == nil || *sample.TTL != ttl {
		t.Fatalf("sample TTL = %v, want %d from the first probe", sample.TTL, ttl)
	}
}
