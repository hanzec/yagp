package service

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"yagp/internal/dto"
	bizerrors "yagp/internal/errors"
	"yagp/internal/model"

	"gorm.io/gorm"
)

func TestNodeEnrollmentAndDeletion(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	fingerprint := strings.Repeat("f1", 32)
	s := NewNodeService(db, fingerprint)
	credentials, err := NewNodeCredentialService(db, bytes.Repeat([]byte{1}, 32))
	requireServiceError(t, err, nil)
	limit, reset := int64(12345), "daily"
	request := &dto.CreateNodeReq{Name: "edge", Remark: "registered in test", TrafficLimitBytes: &limit, TrafficResetPeriod: &reset}
	node, err := s.Create(request, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	if node.Status != model.NodeStatusOffline || node.TrafficLimitBytes != limit || node.TrafficResetPeriod != reset || node.TrafficQuotaState != model.TrafficQuotaPending {
		t.Fatalf("initial node = %+v", node)
	}
	_, err = s.Create(request, testAdmin, testIP, testAgent)
	requireServiceError(t, err, bizerrors.ErrNodeNameExists)
	other, err := s.Create(&dto.CreateNodeReq{Name: "other"}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	_, err = s.Update(other.ID, &dto.UpdateNodeReq{Name: node.Name}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, bizerrors.ErrNodeNameExists)
	_, err = s.Update(99, &dto.UpdateNodeReq{Name: "absent"}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, bizerrors.ErrNodeNotFound)
	_, err = s.IssueRegistrationToken(ctx, 99, testAdmin, testIP, testAgent)
	requireServiceError(t, err, bizerrors.ErrNodeNotFound)

	token, err := s.IssueRegistrationToken(ctx, node.ID, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	if !strings.HasPrefix(token.Token, fmt.Sprintf("%d.", node.ID)) || !strings.HasSuffix(token.Token, "."+fingerprint) || time.Until(token.ExpiresAt) < 14*time.Minute {
		t.Fatalf("issued token ID, fingerprint or expiration was invalid: %v", token.ExpiresAt)
	}
	_, err = credentials.Enroll(ctx, node.ID, strings.TrimSuffix(token.Token, "."+fingerprint), "", "2001:db8::8")
	requireServiceError(t, err, gorm.ErrRecordNotFound)
	enrollment, err := credentials.Enroll(ctx, node.ID, token.Token, "", "2001:db8::8")
	requireServiceError(t, err, nil)
	registered, err := credentials.AuthenticateAccess(ctx, node.ID, enrollment.GetCredentials().GetAccessToken())
	requireServiceError(t, err, nil)
	if registered.ID != node.ID || registered.Address != "2001:db8::8" ||
		registered.Username != fmt.Sprintf("node-%d", node.ID) || registered.Password == "" {
		t.Fatal("registration returned unexpected identity, certificate or address")
	}
	_, err = credentials.Enroll(ctx, node.ID, token.Token, "192.0.2.99", "")
	requireServiceError(t, err, gorm.ErrRecordNotFound)
	requireServiceError(t, s.UpdateIPs(ctx, registered, "192.0.2.8", "", nil, nil, false), nil)
	results, count, err := s.List(&dto.NodeListReq{Status: "offline", Keyword: "192.0.2.8"})
	requireServiceError(t, err, nil)
	if count != 1 || len(results) != 1 || results[0].ID != node.ID ||
		results[0].IPv4 != "192.0.2.8" || results[0].IPv6 != "2001:db8::8" ||
		results[0].Address != "192.0.2.8" || results[0].RegistrationTokenHash != "" {
		t.Fatal("enrolled node not found by refreshed IPv4, or stored registration state is invalid")
	}
	_, err = s.CreateGostClient(node.ID)
	requireServiceError(t, err, nil)
	_, err = s.CreateGostClient(99)
	requireServiceError(t, err, bizerrors.ErrNodeNotFound)
	s.RecordOperation(node.ID, "test node audit event", testAdmin, testIP, testAgent)

	rule := model.GostRule{Name: "forward", NodeID: &node.ID, ListenPort: 8080}
	tunnel := model.GostTunnel{Name: "relay", EntryNodeID: node.ID, ExitNodeID: other.ID}
	saveServiceFixtures(t, db, &rule, &tunnel)
	requireServiceError(t, s.Delete(node.ID, testAdmin, testIP, testAgent), bizerrors.ErrNodeHasRules)
	requireServiceError(t, db.Delete(&rule).Error, nil)
	requireServiceError(t, s.Delete(node.ID, testAdmin, testIP, testAgent), bizerrors.ErrNodeHasTunnels)
	requireServiceError(t, db.Delete(&tunnel).Error, nil)
	requireServiceError(t, s.Delete(node.ID, testAdmin, testIP, testAgent), nil)
	requireServiceError(t, s.Delete(node.ID, testAdmin, testIP, testAgent), bizerrors.ErrNodeNotFound)
	_, err = s.GetConfig(node.ID)
	requireServiceError(t, err, gorm.ErrRecordNotFound)
}

func TestUnreachableNodeStopsItsRulesAndRelatedTunnels(t *testing.T) {
	db := newServiceTestDB(t)
	remote := &lifecycleNode{t: t}
	node := startServiceTestNode(t, remote)
	exit := model.GostNode{Name: "other", Port: 39000, Status: model.NodeStatusOffline}
	saveServiceFixtures(t, db, &node, &exit)
	tunnel := model.GostTunnel{Name: "relay", EntryNodeID: node.ID, ExitNodeID: exit.ID, Status: model.TunnelStatusRunning}
	saveServiceFixtures(t, db, &tunnel)
	direct := model.GostRule{Name: "forward", NodeID: &node.ID, ListenPort: 8080, Status: model.RuleStatusRunning}
	relayed := model.GostRule{Name: "through tunnel", Type: model.RuleTypeTunnel, TunnelID: &tunnel.ID, ListenPort: 8081, Status: model.RuleStatusRunning}
	saveServiceFixtures(t, db, &direct, &relayed)

	nodeService := NewNodeService(db, "")
	got, err := nodeService.GetConfig(node.ID)
	requireServiceError(t, err, nil)
	if len(got.Services) != 0 {
		t.Fatalf("unexpected configured services: %+v", got.Services)
	}
	health := NewRuleSyncService(db)

	remote.mu.Lock()
	remote.reject = map[string]int{"GET /config": http.StatusServiceUnavailable}
	remote.mu.Unlock()
	if _, err := nodeService.GetConfig(node.ID); err == nil {
		t.Fatal("GetConfig hid a node failure")
	}
	health.Start()
	defer health.Stop()
	waitServiceCondition(t, func() bool {
		var saved model.GostNode
		requireServiceError(t, db.First(&saved, node.ID).Error, nil)
		return saved.LastCheckAt != nil && saved.Status == model.NodeStatusOffline
	})
	for _, id := range []uint{direct.ID, relayed.ID} {
		var rule model.GostRule
		requireServiceError(t, db.First(&rule, id).Error, nil)
		if rule.Status != model.RuleStatusStopped {
			t.Fatalf("rule %d status after node loss = %q", id, rule.Status)
		}
	}
	var savedTunnel model.GostTunnel
	requireServiceError(t, db.First(&savedTunnel, tunnel.ID).Error, nil)
	if savedTunnel.Status != model.TunnelStatusStopped {
		t.Fatalf("tunnel status after node loss = %q", savedTunnel.Status)
	}
	waitServiceCondition(t, func() bool {
		var saved model.GostNode
		requireServiceError(t, db.First(&saved, exit.ID).Error, nil)
		return saved.LastCheckAt != nil
	})
}

func TestObserverCountersAndTransactionRollback(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	entry, exit := model.GostNode{Name: "edge", Port: 39000}, model.GostNode{Name: "other", Port: 39000}
	saveServiceFixtures(t, db, &entry, &exit)
	tunnel := model.GostTunnel{Name: "relay", EntryNodeID: entry.ID, ExitNodeID: exit.ID}
	saveServiceFixtures(t, db, &tunnel)
	forward := model.GostRule{Name: "forward", NodeID: &entry.ID, ListenPort: 8080}
	relayed := model.GostRule{Name: "relay", Type: model.RuleTypeTunnel, TunnelID: &tunnel.ID, ListenPort: 8081}
	foreign := model.GostRule{Name: "foreign", NodeID: &exit.ID, ListenPort: 8082}
	saveServiceFixtures(t, db, &forward, &relayed, &foreign)
	observer := NewObserverService(db)
	sample := func(id uint, input, output, connections int64) TrafficSample {
		return TrafficSample{RuleID: id, ObserverCounters: model.ObserverCounters{InputBytes: input, OutputBytes: output, Connections: connections}}
	}
	requireServiceError(t, observer.RecordTraffic(ctx, entry.ID, nil), nil)
	requireServiceError(t, observer.RecordTraffic(ctx, entry.ID, []TrafficSample{
		sample(forward.ID, 50, 25, 3), sample(relayed.ID, 100, 75, 4), sample(99, 90, 90, 1),
	}), nil)
	requireServiceError(t, observer.RecordTraffic(ctx, entry.ID, []TrafficSample{
		sample(forward.ID, 60, 30, 4), sample(foreign.ID, 100, 100, 1),
	}), ErrReportNodeMismatch)
	requireServiceError(t, observer.RecordTraffic(ctx, exit.ID, []TrafficSample{sample(relayed.ID, 120, 80, 5)}), ErrReportNodeMismatch)
	requireServiceError(t, observer.RecordTraffic(ctx, entry.ID, []TrafficSample{
		sample(forward.ID, 60, 30, 4), sample(relayed.ID, 10, 15, 2),
	}), nil)
	requireServiceError(t, observer.RecordTraffic(ctx, entry.ID, []TrafficSample{
		{RuleID: forward.ID, Reset: true}, sample(forward.ID, 2, 3, 1),
	}), nil)
	var saved model.GostRule
	requireServiceError(t, db.First(&saved, forward.ID).Error, nil)
	if saved.InputBytes != 62 || saved.OutputBytes != 33 || saved.TotalBytes != 95 || saved.TotalRequests != 5 {
		t.Fatalf("forward rule counters = %+v", saved)
	}
	saved = model.GostRule{}
	requireServiceError(t, db.First(&saved, relayed.ID).Error, nil)
	if saved.TotalBytes != 200 || saved.TotalRequests != 6 {
		t.Fatalf("relay rule counters after counter rollover = %+v", saved)
	}
	requireServiceError(t, db.First(&entry, entry.ID).Error, nil)
	if entry.TotalBytes != 295 {
		t.Fatalf("node traffic = %d, want 295", entry.TotalBytes)
	}
}

func TestObserverReplayReadsCheckpointsOnceAndSkipsWrites(t *testing.T) {
	db := newServiceTestDB(t)
	node := model.GostNode{Name: "reporting node"}
	saveServiceFixtures(t, db, &node)
	rules := make([]model.GostRule, 100)
	for i := range rules {
		rules[i] = model.GostRule{Name: fmt.Sprintf("listener-%d", i), NodeID: &node.ID, ListenPort: 12000 + i}
	}
	requireServiceError(t, db.Create(&rules).Error, nil)
	reports := make([]TrafficSample, len(rules))
	for i, rule := range rules {
		reports[i] = TrafficSample{RuleID: rule.ID, ObserverCounters: model.ObserverCounters{InputBytes: 40, OutputBytes: 60, Connections: 5}}
	}
	observer := NewObserverService(db)
	requireServiceError(t, observer.RecordTraffic(t.Context(), node.ID, reports), nil)

	var reads, writes int
	requireServiceError(t, db.Callback().Query().After("gorm:query").Register("test:count_observer_reads", func(*gorm.DB) { reads++ }), nil)
	requireServiceError(t, db.Callback().Update().After("gorm:update").Register("test:count_observer_writes", func(*gorm.DB) { writes++ }), nil)
	requireServiceError(t, observer.RecordTraffic(t.Context(), node.ID, reports), nil)
	if reads != 1 || writes != 0 {
		t.Fatalf("unchanged batch issued %d reads and %d writes, want one read and zero writes", reads, writes)
	}

	// Each subsequent event for the same rule must use the preceding event's checkpoint.
	changed := reports[0]
	changed.InputBytes += 10
	requireServiceError(t, observer.RecordTraffic(t.Context(), node.ID, []TrafficSample{
		changed, changed, {RuleID: changed.RuleID, Reset: true}, changed,
	}), nil)
	var rule model.GostRule
	requireServiceError(t, db.First(&rule, changed.RuleID).Error, nil)
	if rule.InputBytes != 100 || rule.OutputBytes != 120 || rule.TotalRequests != 10 || rule.ObserverCounters != changed.ObserverCounters {
		t.Fatalf("unexpected counters after replay and reset: %+v", rule)
	}
}

func TestObserverAuthorizationRollsBackAcrossBatches(t *testing.T) {
	db := newServiceTestDB(t)
	entry, other := model.GostNode{Name: "entry"}, model.GostNode{Name: "other"}
	saveServiceFixtures(t, db, &entry, &other)
	local := model.GostRule{Name: "local", NodeID: &entry.ID}
	foreign := model.GostRule{Name: "foreign", NodeID: &other.ID}
	saveServiceFixtures(t, db, &local, &foreign)

	reports := make([]TrafficSample, 501)
	for i := range reports {
		reports[i] = TrafficSample{RuleID: local.ID, ObserverCounters: model.ObserverCounters{InputBytes: 1}}
	}
	// Even an unchanged checkpoint must belong to the reporting node.
	reports[len(reports)-1] = TrafficSample{RuleID: foreign.ID}
	err := NewObserverService(db).RecordTraffic(t.Context(), entry.ID, reports)
	requireServiceError(t, err, ErrReportNodeMismatch)
	requireServiceError(t, db.First(&local, local.ID).Error, nil)
	requireServiceError(t, db.First(&entry, entry.ID).Error, nil)
	if local.InputBytes != 0 || local.ObserverCounters.InputBytes != 0 || entry.TotalBytes != 0 {
		t.Fatalf("rejected report changed counters: rule %d checkpoint %d node %d", local.InputBytes, local.ObserverCounters.InputBytes, entry.TotalBytes)
	}
}

func waitServiceCondition(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for service state")
}
