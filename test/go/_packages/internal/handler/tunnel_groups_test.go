package handler

import (
	"bytes"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"yagp/internal/model"
	"yagp/internal/service"
	"yagp/pkg/nodegrpc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTunnelHTTPGroupsDirectedPairsAndRetainsProbeHistory(t *testing.T) {
	f := newHandlerHTTPFixture(t)
	f.registerForwarding()
	f.engine.GET("/dashboard/stats", NewStatsHandler(service.NewStatsService(f.db)).GetDashboard)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	nodes := []model.GostNode{{Name: "A", IPv4: "192.0.2.1"}, {Name: "B"}, {Name: "C"}}
	must(f.db.Create(&nodes).Error)
	a, b, c := nodes[0].ID, nodes[1].ID, nodes[2].ID
	legacy := []model.GostTunnel{
		{Name: "A to B", EntryNodeID: a, ExitNodeID: b, RelayPort: 8443, Status: model.TunnelStatusRunning, ChainID: "saved-first"},
		{Name: "another A to B", EntryNodeID: a, ExitNodeID: b, RelayPort: 8444},
		{Name: "same entry", EntryNodeID: a, ExitNodeID: c},
		{Name: "same exit", EntryNodeID: c, ExitNodeID: b},
		{Name: "reversed", EntryNodeID: b, ExitNodeID: a},
		{Name: "onward", EntryNodeID: b, ExitNodeID: c},
	}
	must(f.db.Create(&legacy).Error)
	f.migrateTunnelGroups(t)
	must(f.db.AutoMigrate(&model.TunnelProbeSample{}))
	sampledAt := time.Now().Add(-time.Minute).Unix()
	must(f.db.Create(&[]model.TunnelProbeSample{
		{TunnelID: legacy[0].ID, SampledAt: sampledAt, Status: model.TunnelProbeSuccessful, LatencyMS: new(10.0)},
		{TunnelID: legacy[1].ID, SampledAt: sampledAt, Status: model.TunnelProbeSuccessful, LatencyMS: new(30.0)},
		{TunnelID: legacy[2].ID, SampledAt: sampledAt, Status: model.TunnelProbeFailed},
	}).Error)

	path := fmt.Sprintf("/tunnels/%d", legacy[0].ID)
	alias := fmt.Sprintf("/tunnels/%d", legacy[1].ID)
	group := handlerData[service.TunnelGroupView](t, f.request(t, http.MethodGet, alias, "", http.StatusOK))
	if group.ID != legacy[0].ID || group.Name != "A to B" || group.Status != model.TunnelStatusError ||
		len(group.Connections) != 2 || group.Connections[0].ChainID != "saved-first" || group.Connections[1].RelayPort != 8444 {
		t.Fatalf("migrated tunnel = %+v, connections = %+v", group, group.Connections)
	}
	page := handlerData[handlerPage[service.TunnelGroupView]](t, f.request(t, http.MethodGet, "/tunnels?pageSize=2", "", http.StatusOK))
	if page.Total != 5 || len(page.List) != 2 {
		t.Fatalf("pagination should count node pairs: %+v", page)
	}
	matching := fmt.Sprintf("/tunnels?node_id=%d&status=error", b)
	page = handlerData[handlerPage[service.TunnelGroupView]](t, f.request(t, http.MethodGet, matching, "", http.StatusOK))
	if page.Total != 1 || len(page.List) != 1 || page.List[0].ID != group.ID {
		t.Fatalf("filtered tunnels: %+v", page)
	}
	stats := handlerData[service.DashboardStats](t, f.request(t, http.MethodGet, "/dashboard/stats", "", http.StatusOK))
	if stats.Tunnels.Total != 5 || stats.Tunnels.Running != 0 {
		t.Fatalf("dashboard counted connections as tunnels: %+v", stats.Tunnels)
	}
	metrics := handlerData[service.TunnelMetrics](t, f.request(t, http.MethodGet, alias+"/metrics?range=1h", "", http.StatusOK))
	if metrics.Summary.Attempts != 2 || metrics.Summary.AvgLatencyMS == nil || *metrics.Summary.AvgLatencyMS != 20 {
		t.Fatalf("group probe summary = %+v", metrics.Summary)
	}
	f.request(t, http.MethodPost, alias+"/stop", "", http.StatusNotFound)
	f.request(t, http.MethodDelete, alias, "", http.StatusNotFound)
	updated := handlerData[service.TunnelGroupView](t, f.request(t, http.MethodPut, path, `{"name":"Renamed","remark":"All listeners"}`, http.StatusOK))
	if updated.ID != group.ID || updated.Name != "Renamed" || len(updated.Connections) != 2 {
		t.Fatalf("edited group = %+v", updated)
	}
	allowlist := fmt.Sprintf(`{"allowlist_node_ids":[%d]}`, a)
	updated = handlerData[service.TunnelGroupView](t, f.request(t, http.MethodPut, path+"/allowlist", allowlist, http.StatusOK))
	for _, connection := range updated.Connections {
		if connection.AutoAllowlist || !slices.Equal(connection.AllowlistNodeIDs, []uint{a}) {
			t.Fatalf("connection allowlist was not updated: %+v", connection)
		}
	}
	f.migrateTunnelGroups(t)
	body := fmt.Sprintf(`{"name":"another connection","entry_node_id":%d,"exit_node_id":%d,"protocol":"tcp","relay_port":8455}`, a, b)
	joined := handlerData[service.TunnelGroupView](t, f.request(t, http.MethodPost, "/tunnels", body, http.StatusOK))
	if joined.ID != group.ID || joined.Name != "Renamed" || joined.Remark != "All listeners" || len(joined.Connections) != 3 ||
		joined.Connections[2].Status != model.TunnelStatusStopped {
		t.Fatalf("joining existing tunnel returned %+v, connections %+v", joined, joined.Connections)
	}
}

func TestTunnelHTTPTestsEveryConnectionThroughTheManagedID(t *testing.T) {
	f := newHandlerHTTPFixture(t)
	f.registerForwarding()
	entry := model.GostNode{Name: "entry", Status: model.NodeStatusOnline, Username: "entry", Password: "test-entry-password"}
	exit := model.GostNode{Name: "exit", Status: model.NodeStatusOnline, Username: "exit", Password: "test-exit-password"}
	for _, node := range []*model.GostNode{&entry, &exit} {
		if err := f.db.Create(node).Error; err != nil {
			t.Fatal(err)
		}
	}
	connections := []model.GostTunnel{
		{Name: "first", EntryNodeID: entry.ID, ExitNodeID: exit.ID, RelayPort: 8443, Status: model.TunnelStatusRunning, ChainID: "tunnel-1-chain"},
		{Name: "second", EntryNodeID: entry.ID, ExitNodeID: exit.ID, RelayPort: 8444, Status: model.TunnelStatusRunning, ChainID: "tunnel-2-chain"},
	}
	if err := f.db.Create(&connections).Error; err != nil {
		t.Fatal(err)
	}
	f.migrateTunnelGroups(t)
	panel := startTestReportServer(t, f.db)
	startTestReportStream(t, t.Context(), panel, exit, nodegrpc.NewDispatcher(tunnelProbeNode{
		start: func() (*nodegrpc.TunnelProbeEndpoint, error) {
			return &nodegrpc.TunnelProbeEndpoint{Address: "127.0.0.1:43210", Token: bytes.Repeat([]byte{1}, 32)}, nil
		},
	}, nodegrpc.V1_StartTunnelProbe_FullMethodName))
	var failSecond atomic.Bool
	startTestReportStream(t, t.Context(), panel, entry, nodegrpc.NewDispatcher(tunnelProbeNode{
		probe: func(req *nodegrpc.TunnelProbeRequest) (*nodegrpc.TunnelProbeReply, error) {
			switch req.GetChainName() {
			case connections[0].ChainID:
				return &nodegrpc.TunnelProbeReply{RoundTripMs: 10}, nil
			case connections[1].ChainID:
				if failSecond.Load() {
					return nil, status.Error(codes.NotFound, "test chain unavailable")
				}
				return &nodegrpc.TunnelProbeReply{RoundTripMs: 30}, nil
			default:
				return nil, status.Error(codes.InvalidArgument, "unexpected chain")
			}
		},
	}, nodegrpc.V1_ProbeTunnel_FullMethodName))
	path := fmt.Sprintf("/tunnels/%d/test", connections[0].ID)
	result := handlerData[service.TunnelTestResult](t, f.request(t, http.MethodPost, path, "", http.StatusOK))
	if !result.Success || result.LatencyMS != 20 || len(result.Diagnostics) != 2 {
		t.Fatalf("test combined result = %+v", result)
	}
	failSecond.Store(true)
	result = handlerData[service.TunnelTestResult](t, f.request(t, http.MethodPost, path, "", http.StatusBadRequest))
	if result.Success || result.Partial || result.FailureStage != "entry_chain" ||
		result.Diagnostics[0].Status != "passed" || result.Diagnostics[1].Status != "failed" {
		t.Fatalf("test ignored the second connection failure: %+v", result)
	}
}
