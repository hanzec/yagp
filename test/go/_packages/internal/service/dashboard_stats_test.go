package service

import (
	"context"
	"testing"
	"time"

	"yagp/internal/model"
	"yagp/internal/repository"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestDashboardSummariesPreserveCountsWithFiveQueries(t *testing.T) {
	db := newServiceTestDB(t)
	check := func(nodes NodeStats, rules RuleStats, tunnels TunnelStats) {
		t.Helper()
		queries := &queryCounter{Interface: gormlogger.Default.LogMode(gormlogger.Silent)}
		stats, err := NewStatsService(db.Session(&gorm.Session{Logger: queries})).GetDashboardStats()
		requireServiceError(t, err, nil)
		if stats.Nodes != nodes || stats.Rules != rules || stats.Tunnels != tunnels {
			t.Fatalf("dashboard counts = %+v, %+v, %+v; want %+v, %+v, %+v", stats.Nodes, stats.Rules, stats.Tunnels, nodes, rules, tunnels)
		}
		if queries.count != 5 {
			t.Fatalf("dashboard executed %d queries, want 3 summaries and 2 traffic queries", queries.count)
		}
	}
	check(NodeStats{}, RuleStats{}, TunnelStats{})

	nodes := []model.GostNode{
		{Name: "online", Status: model.NodeStatusOnline},
		{Name: "offline", Status: model.NodeStatusOffline},
		{Name: "error", Status: model.NodeStatusError},
		{Name: "deleted", Status: model.NodeStatusOnline},
		{Name: "another offline", Status: model.NodeStatusOffline},
	}
	saveServiceFixtures(t, db, &nodes)
	rules := []model.GostRule{
		{Name: "running forward", Type: model.RuleTypeForward, Status: model.RuleStatusRunning},
		{Name: "stopped forward", Type: model.RuleTypeForward, Status: model.RuleStatusStopped},
		{Name: "failed tunnel", Type: model.RuleTypeTunnel, Status: model.RuleStatusError},
		{Name: "running tunnel", Type: model.RuleTypeTunnel, Status: model.RuleStatusRunning},
		{Name: "deleted", Type: model.RuleTypeForward, Status: model.RuleStatusRunning},
	}
	tunnels := []model.GostTunnel{
		{Name: "running", EntryNodeID: nodes[0].ID, ExitNodeID: nodes[1].ID, Status: model.TunnelStatusRunning},
		{Name: "parallel", EntryNodeID: nodes[0].ID, ExitNodeID: nodes[1].ID, Status: model.TunnelStatusRunning},
		{Name: "stopped", EntryNodeID: nodes[0].ID, ExitNodeID: nodes[2].ID, Status: model.TunnelStatusStopped},
		{Name: "error", EntryNodeID: nodes[0].ID, ExitNodeID: nodes[4].ID, Status: model.TunnelStatusError},
		{Name: "deleted", EntryNodeID: nodes[1].ID, ExitNodeID: nodes[2].ID, Status: model.TunnelStatusRunning},
	}
	saveServiceFixtures(t, db, &rules, &tunnels)
	for _, row := range []any{&nodes[3], &rules[4], &tunnels[4]} {
		requireServiceError(t, db.Delete(row).Error, nil)
	}
	requireServiceError(t, repository.MigrateTunnelGroups(db), nil)
	// Preserve the existing API's non-running/non-online buckets, including errors.
	check(NodeStats{Total: 4, Online: 1, Offline: 3},
		RuleStats{Total: 4, Running: 2, Stopped: 2, ForwardType: 2, TunnelType: 2},
		TunnelStats{Total: 3, Running: 1, Stopped: 2})
}

type queryCounter struct {
	gormlogger.Interface
	count int
}

func (c *queryCounter) Trace(context.Context, time.Time, func() (string, int64), error) {
	c.count++
}
