package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"yagp/internal/model"
	"yagp/pkg/gost"
)

func TestRuleReconciliationRepairsStatusAndExitAllowlist(t *testing.T) {
	db := newServiceTestDB(t)
	remote := &lifecycleNode{t: t}
	node := startServiceTestNode(t, remote)
	const originalIPv4 = "192.0.2.8"
	entry := model.GostNode{
		Name: "allowed source", Port: 39000, Address: originalIPv4, IPv4: originalIPv4, Status: model.NodeStatusOffline,
		ManualAddresses: []string{"203.0.113.8", originalIPv4},
	}
	otherSource := model.GostNode{
		Name: "other source", Port: 39000, Status: model.NodeStatusOffline,
		Addresses: []string{"192.0.2.10", "2001:db8::7"},
	}
	saveServiceFixtures(t, db, &node, &entry, &otherSource)
	known := model.GostRule{Name: "tracked", NodeID: &node.ID, ListenPort: 8080, Status: model.RuleStatusRunning, ServiceID: "failed-service"}
	legacy := model.GostRule{Name: "configured outside database", NodeID: &node.ID, ListenPort: 8081, Status: model.RuleStatusStopped}
	saveServiceFixtures(t, db, &known, &legacy)
	tunnel := model.GostTunnel{Name: "exit relay", EntryNodeID: entry.ID, ExitNodeID: node.ID, AllowlistNodeIDs: []uint{entry.ID}}
	other := model.GostTunnel{Name: "other relay", EntryNodeID: otherSource.ID, ExitNodeID: node.ID, AllowlistNodeIDs: []uint{otherSource.ID}}
	saveServiceFixtures(t, db, &tunnel, &other)
	remote.resources = make(map[string]json.RawMessage)
	for _, item := range []*model.GostTunnel{&tunnel, &other} {
		item.ServiceID = relayServiceName(item.ID)
		requireServiceError(t, db.Save(item).Error, nil)
		remote.resources["/config/services/"+item.ServiceID] = fmt.Appendf(nil,
			`{"name":%q,"handler":{"type":"relay"},"admission":%q}`, item.ServiceID, gost.LegacyRelayAllowlistName)
	}
	const unmanaged = "relay-tunnel-999"
	remote.resources["/config/services/"+unmanaged] = fmt.Appendf(nil,
		`{"name":%q,"handler":{"type":"relay"},"admission":%q}`, unmanaged, gost.LegacyRelayAllowlistName)
	refreshConfig := func() {
		remote.mu.Lock()
		defer remote.mu.Unlock()
		services := []json.RawMessage{[]byte(`{"name":"failed-service","status":{"state":"failed"}}`),
			fmt.Appendf(nil, `{"name":"rule-%d","status":{"state":"running"}}`, legacy.ID), []byte(`{"name":"untracked"}`)}
		admissions := []json.RawMessage{}
		for path, resource := range remote.resources {
			switch {
			case strings.HasPrefix(path, "/config/services/"):
				services = append(services, resource)
			case strings.HasPrefix(path, "/config/admissions/"):
				admissions = append(admissions, resource)
			}
		}
		cfg, err := json.Marshal(map[string][]json.RawMessage{"services": services, "admissions": admissions})
		requireServiceError(t, err, nil)
		remote.gostConfig = string(cfg)
	}
	refreshConfig()
	remote.reject = map[string]int{"POST /config?format=yaml": http.StatusServiceUnavailable}
	firstName := gost.TunnelRelayAllowlistName(tunnel.ServiceID)
	otherName := gost.TunnelRelayAllowlistName(other.ServiceID)
	syncService := NewRuleSyncService(db)
	syncService.Start()
	defer syncService.Stop()
	waitServiceCondition(t, func() bool {
		return remote.service(tunnel.ServiceID).HasAdmission(firstName) && remote.service(other.ServiceID).HasAdmission(otherName)
	})
	waitServiceCondition(t, func() bool {
		_, running := syncService.syncing.Load(node.ID)
		return !running
	})
	requireServiceError(t, db.First(&known, known.ID).Error, nil)
	requireServiceError(t, db.First(&legacy, legacy.ID).Error, nil)
	if known.Status != model.RuleStatusError || legacy.Status != model.RuleStatusRunning || legacy.ServiceID != entranceServiceName(legacy.ID) {
		t.Fatalf("reconciled rules: tracked status %q, restored status %q, restored service %q", known.Status, legacy.Status, legacy.ServiceID)
	}
	assertAdmission := func(name string, whitelist bool, matchers []string) {
		t.Helper()
		if admission := remote.admission(name); admission.Name != name || admission.Whitelist != whitelist || !slices.Equal(admission.Matchers, matchers) {
			t.Fatalf("admission = %+v, want %s (whitelist %t) matching %v", admission, name, whitelist, matchers)
		}
	}
	assertAdmission(firstName, true, []string{originalIPv4, "203.0.113.8"})
	assertAdmission(otherName, true, otherSource.Addresses)
	if remote.service(tunnel.ServiceID).HasAdmission(gost.LegacyRelayAllowlistName) ||
		remote.service(other.ServiceID).HasAdmission(gost.LegacyRelayAllowlistName) ||
		!remote.service(unmanaged).HasAdmission(gost.LegacyRelayAllowlistName) {
		t.Fatal("legacy admission was not replaced for exactly the managed relays")
	}
	if _, pending := syncService.unsavedTunnelConfig.Load(node.ID); !pending {
		t.Fatal("failed allowlist save was not marked for retry")
	}
	refreshConfig()
	remote.mu.Lock()
	remote.reject = nil
	remote.mu.Unlock()
	syncService.syncNodeRules(node, nil)
	if _, pending := syncService.unsavedTunnelConfig.Load(node.ID); pending {
		t.Fatal("allowlist save retry did not succeed")
	}
	refreshConfig()
	requireServiceError(t, db.Model(&entry).Update("ipv4", "192.0.2.9").Error, nil)
	syncService.syncNodeRules(node, nil)
	assertAdmission(firstName, true, []string{originalIPv4, "192.0.2.9", "203.0.113.8"})
	assertAdmission(otherName, true, otherSource.Addresses)
	refreshConfig()
	requireServiceError(t, db.Model(&entry).Update("ipv4", "").Error, nil)
	syncService.syncNodeRules(node, nil)
	assertAdmission(firstName, true, []string{originalIPv4, "203.0.113.8"})
	refreshConfig()
	requireServiceError(t, db.Model(&entry).Update("manual_addresses", "[\"192.168.1.10\",\"fd00::10\"]").Error, nil)
	syncService.syncNodeRules(node, nil)
	assertAdmission(firstName, true, nil)
	refreshConfig()
	tunnel.AllowlistNodeIDs = []uint{}
	requireServiceError(t, db.Save(&tunnel).Error, nil)
	syncService.syncNodeRules(node, nil)
	assertAdmission(firstName, false, nil)
	assertAdmission(otherName, true, otherSource.Addresses)
	if !remote.service(tunnel.ServiceID).HasAdmission(firstName) {
		t.Fatal("clearing the selection detached the hot-updatable admission")
	}
}

func TestRulesetSyncRetriesSaveWithoutRecreatingBypass(t *testing.T) {
	db := newServiceTestDB(t)
	remote, offlineRemote := &lifecycleNode{t: t}, &lifecycleNode{t: t}
	node := startServiceTestNode(t, remote)
	offline := startServiceTestNode(t, offlineRemote)
	offline.Name, offline.Status = "offline", model.NodeStatusOffline
	saveServiceFixtures(t, db, &node, &offline)
	ctx := context.Background()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rules":
			_, _ = fmt.Fprint(w, "payload:\n  - DOMAIN-SUFFIX,example.com\n")
		case "/large":
			_, _ = fmt.Fprint(w, strings.Repeat("x", maxRulesetBytes+1))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	service := NewGostRulesetService(db)
	service.client = upstream.Client()
	for _, tc := range []struct {
		source string
		ids    []uint
	}{
		{"", []uint{node.ID}},
		{"http://rules.test", nil},
		{upstream.URL + "/rules", []uint{offline.ID + 1}},
		{upstream.URL + "/not-found", nil},
		{upstream.URL + "/large", nil},
	} {
		if _, err := service.Update(ctx, tc.source, tc.ids); err == nil {
			t.Fatalf("invalid ruleset accepted: %+v", tc)
		}
	}
	settings, err := service.Update(ctx, upstream.URL+"/rules", []uint{offline.ID, node.ID, node.ID})
	requireServiceError(t, err, nil)
	if !reflect.DeepEqual(settings.NodeIDs, []uint{node.ID, offline.ID}) {
		t.Fatalf("saved node IDs = %v", settings.NodeIDs)
	}

	remote.mu.Lock()
	remote.reject = map[string]int{"POST /config?format=yaml": http.StatusServiceUnavailable}
	remote.mu.Unlock()
	pending := make(map[uint]bool)
	service.sync(ctx, pending)
	if !pending[node.ID] || pending[offline.ID] {
		t.Fatalf("unsaved nodes = %v", pending)
	}
	const path = "/config/bypasses/" + gost.TransparentBypassName
	var active struct {
		Name      string
		Whitelist bool
		Matchers  []string
	}
	remote.read(path, &active)
	if active.Name != gost.TransparentBypassName || !active.Whitelist || !reflect.DeepEqual(active.Matchers, settings.Matchers) {
		t.Fatalf("node bypass = %+v", active)
	}
	var skipped struct{ Name string }
	offlineRemote.read(path, &skipped)
	if skipped.Name != "" {
		t.Fatalf("offline node changed = %+v", skipped)
	}
	remote.mu.Lock()
	remote.reject = nil
	remote.mu.Unlock()
	service.sync(ctx, pending)
	if len(pending) != 0 {
		t.Fatalf("pending saves remained after retry: %v", pending)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	service.Run(canceled)
}

func TestRestoredRunningRuleRemovesRemoteService(t *testing.T) {
	for _, poll := range []bool{false, true} {
		t.Run(fmt.Sprintf("sync_before_delete_%t", poll), func(t *testing.T) {
			db := newServiceTestDB(t)
			remote := &lifecycleNode{t: t}
			entry := startServiceTestNode(t, remote)
			saveServiceFixtures(t, db, &entry)
			rule := model.GostRule{Name: "restored", NodeID: &entry.ID, ListenPort: 8087, Status: model.RuleStatusRunning}
			saveServiceFixtures(t, db, &rule)
			name := entranceServiceName(rule.ID)
			service := fmt.Appendf(nil, `{"name":%q,"status":{"state":"running"}}`, name)
			remote.resources = map[string]json.RawMessage{"/config/services/" + name: service}
			remote.gostConfig = `{"services":[` + string(service) + `]}`
			rules := NewRuleService(db)
			if poll {
				NewRuleSyncService(db).syncNodeRules(entry, nil)
				stored, err := rules.GetByID(rule.ID)
				requireServiceError(t, err, nil)
				if stored.ServiceID != name {
					t.Fatalf("restored service ID = %q, want %q", stored.ServiceID, name)
				}
			}
			remote.rejectCommand("POST /config?format=yaml", http.StatusServiceUnavailable)
			if err := rules.Delete(rule.ID, testAdmin, testIP, testAgent); err == nil {
				t.Fatal("delete succeeded despite a failed node config save")
			}
			remote.gostConfig = "{}"
			NewRuleSyncService(db).syncNodeRules(entry, nil)
			stored, err := rules.GetByID(rule.ID)
			requireServiceError(t, err, nil)
			if stored.ServiceID != name || stored.Status != model.RuleStatusStopped {
				t.Fatalf("lost pending cleanup after failed save: %+v", stored)
			}
			remote.rejectCommand("POST /config?format=yaml", 0)
			requireServiceError(t, rules.Delete(rule.ID, testAdmin, testIP, testAgent), nil)
			if remote.service(name).Name != "" {
				t.Fatalf("service %q still exists on the node", name)
			}
		})
	}
}

func TestLegacyTunnelRuleRecoversOnEntryReconnect(t *testing.T) {
	db := newServiceTestDB(t)
	remote := &lifecycleNode{t: t}
	entry := startServiceTestNode(t, remote)
	exit := model.GostNode{Name: "exit"}
	saveServiceFixtures(t, db, &entry, &exit)
	tunnel := model.GostTunnel{Name: "legacy", EntryNodeID: entry.ID, ExitNodeID: exit.ID}
	saveServiceFixtures(t, db, &tunnel)
	rule := model.GostRule{Name: "legacy listener", Type: model.RuleTypeTunnel, TunnelID: &tunnel.ID, ServiceID: "legacy-service", Status: model.RuleStatusRunning}
	saveServiceFixtures(t, db, &rule)
	syncService := NewRuleSyncService(db)
	assertStatus := func(want model.RuleStatus) {
		t.Helper()
		stored, err := syncService.ruleRepo.FindByID(rule.ID)
		requireServiceError(t, err, nil)
		if stored.Status != want {
			t.Fatalf("rule status = %q, want %q", stored.Status, want)
		}
	}
	remote.rejectCommand("GET /config", http.StatusServiceUnavailable)
	syncService.syncNodeRules(entry, nil)
	assertStatus(model.RuleStatusStopped)
	remote.rejectCommand("GET /config", 0)
	remote.gostConfig = `{"services":[{"name":"legacy-service","status":{"state":"running"}}]}`
	syncService.syncNodeRules(entry, nil)
	assertStatus(model.RuleStatusRunning)
}
