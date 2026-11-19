package service

import (
	"fmt"
	"net/http"
	"testing"

	"yagp/internal/dto"
	"yagp/internal/errors"
	"yagp/internal/model"
	"yagp/internal/repository"
)

func TestTunnelGroupControlsEveryConnection(t *testing.T) {
	db := newServiceTestDB(t)
	loadServiceConfig(t, "report:\n  listen: 127.0.0.1:39001\n")
	entryRemote, exitRemote := &lifecycleNode{t: t}, &lifecycleNode{t: t}
	entry, exit := startServiceTestNode(t, entryRemote), startServiceTestNode(t, exitRemote)
	entry.Name, exit.Name = "A", "B"
	saveServiceFixtures(t, db, &entry, &exit)
	svc := NewTunnelService(db)
	newConnection := func(index int) dto.CreateTunnelReq {
		t.Helper()
		source := model.GostRule{Name: fmt.Sprintf("entry %d", index), NodeID: &entry.ID, Role: model.EndpointRoleEntry, ListenPort: 12000 + index}
		destination := model.GostRule{Name: fmt.Sprintf("exit %d", index), NodeID: &exit.ID, Role: model.EndpointRoleExit, ListenPort: 13000 + index, Targets: []string{"origin.test:443"}}
		saveServiceFixtures(t, db, &source, &destination)
		return dto.CreateTunnelReq{Name: "A to B", EntryRuleID: source.ID, ExitRuleID: destination.ID, AllowlistNodeIDs: []uint{}}
	}
	firstRequest, secondRequest := newConnection(1), newConnection(2)
	first, err := svc.CreateGroup(&firstRequest, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	secondRequest.Name = "discarded connection name"
	second, err := svc.CreateGroup(&secondRequest, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	if second.ID != first.ID || second.Name != first.Name || second.Status != model.TunnelStatusRunning || len(second.Connections) != 2 {
		t.Fatalf("adding a running connection returned %+v", second)
	}
	_, err = svc.CreateGroup(&secondRequest, testAdmin, testIP, testAgent)
	requireServiceError(t, err, errors.ErrTunnelDuplicateEndpoints)

	checkConnections := func(want model.TunnelStatus, count int) *TunnelGroupView {
		t.Helper()
		group, err := svc.GetGroup(first.ID)
		requireServiceError(t, err, nil)
		if group.Status != want || len(group.Connections) != count {
			t.Fatalf("group is %s with %d connections, want %s with %d", group.Status, len(group.Connections), want, count)
		}
		for _, connection := range group.Connections {
			hasChain := connection.ChainID != "" && entryRemote.chain(connection.ChainID).Name != ""
			hasRelay := connection.ServiceID != "" && exitRemote.service(connection.ServiceID).Name != ""
			if connection.Status != want || hasChain != (want == model.TunnelStatusRunning) || hasRelay != hasChain {
				t.Fatalf("connection %d: status %s, chain exists %t, relay exists %t", connection.ID, connection.Status, hasChain, hasRelay)
			}
		}
		return group
	}
	checkConnections(model.TunnelStatusRunning, 2)
	requireServiceError(t, svc.StopGroup(first.ID, testAdmin, testIP, testAgent), nil)
	checkConnections(model.TunnelStatusStopped, 2)
	thirdRequest := newConnection(3)
	_, err = svc.CreateGroup(&thirdRequest, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	checkConnections(model.TunnelStatusStopped, 3)
	requireServiceError(t, svc.StartGroup(first.ID, testAdmin, testIP, testAgent), nil)
	running := checkConnections(model.TunnelStatusRunning, 3)

	chainPath := "DELETE /config/chains/" + running.Connections[1].ChainID
	entryRemote.mu.Lock()
	entryRemote.reject = map[string]int{chainPath: http.StatusBadGateway}
	entryRemote.mu.Unlock()
	if err := svc.StopGroup(first.ID, testAdmin, testIP, testAgent); err == nil {
		t.Fatal("failed cleanup was reported as a successful stop")
	}
	partial, err := svc.GetGroup(first.ID)
	requireServiceError(t, err, nil)
	if partial.Status != model.TunnelStatusError || partial.Connections[0].Status != model.TunnelStatusStopped {
		t.Fatalf("partially stopped group has incorrect status: %+v", partial)
	}
	entryRemote.mu.Lock()
	delete(entryRemote.reject, chainPath)
	entryRemote.mu.Unlock()
	requireServiceError(t, svc.StopGroup(first.ID, testAdmin, testIP, testAgent), nil)
	checkConnections(model.TunnelStatusStopped, 3)

	dependent := model.GostRule{Name: "legacy dependent rule", Type: model.RuleTypeTunnel, TunnelID: &second.Connections[1].ID}
	saveServiceFixtures(t, db, &dependent)
	requireServiceError(t, svc.DeleteGroup(first.ID, testAdmin, testIP, testAgent), errors.ErrTunnelHasRules)
	requireServiceError(t, db.Delete(&dependent).Error, nil)
	requireServiceError(t, svc.DeleteGroup(first.ID, testAdmin, testIP, testAgent), nil)
	_, err = svc.GetGroup(first.ID)
	requireServiceError(t, err, errors.ErrTunnelNotFound)
	for _, connection := range running.Connections {
		_, err := svc.GetByID(connection.ID)
		requireServiceError(t, err, errors.ErrTunnelNotFound)
	}
}

func TestReassigningAStoppedTunnelMergesIntoTheExistingPair(t *testing.T) {
	db := newServiceTestDB(t)
	nodes := []model.GostNode{{Name: "A"}, {Name: "B"}, {Name: "C"}}
	saveServiceFixtures(t, db, &nodes)
	a, b, c := nodes[0].ID, nodes[1].ID, nodes[2].ID
	endpoints := []model.GostRule{
		{Name: "first entry", NodeID: &a, Role: model.EndpointRoleEntry, ListenPort: 12001},
		{Name: "first exit", NodeID: &b, Role: model.EndpointRoleExit, ListenPort: 13001, Targets: []string{"first.test:443"}},
		{Name: "second entry", NodeID: &a, Role: model.EndpointRoleEntry, ListenPort: 12002},
		{Name: "old exit", NodeID: &c, Role: model.EndpointRoleExit, ListenPort: 13002, Targets: []string{"second.test:443"}},
		{Name: "new exit", NodeID: &b, Role: model.EndpointRoleExit, ListenPort: 13003, Targets: []string{"second.test:443"}},
	}
	saveServiceFixtures(t, db, &endpoints)
	rows := []model.GostTunnel{
		{Name: "A to B", EntryNodeID: a, ExitNodeID: b, EntryRuleID: &endpoints[0].ID, ExitRuleID: &endpoints[1].ID},
		{Name: "A to C", EntryNodeID: a, ExitNodeID: c, EntryRuleID: &endpoints[2].ID, ExitRuleID: &endpoints[3].ID},
	}
	saveServiceFixtures(t, db, &rows)
	requireServiceError(t, repository.MigrateTunnelGroups(db), nil)
	service := NewTunnelService(db)
	request := dto.UpdateTunnelReq{Name: "A to C", EntryRuleID: endpoints[2].ID, ExitRuleID: endpoints[4].ID}
	merged, err := service.UpdateGroup(rows[1].ID, &request, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	if merged.ID != rows[0].ID || merged.Name != rows[0].Name || len(merged.Connections) != 2 ||
		merged.Connections[1].ID != rows[1].ID || *merged.Connections[1].ExitRuleID != endpoints[4].ID || merged.Status != model.TunnelStatusStopped {
		t.Fatalf("edited connection did not join the existing pair: group %+v, connections %+v", merged, merged.Connections)
	}
	alias, err := service.GetGroup(rows[1].ID)
	requireServiceError(t, err, nil)
	if alias.ID != merged.ID {
		t.Fatalf("old URL resolves to %d, want %d", alias.ID, merged.ID)
	}
	_, total, err := service.ListGroups(&dto.TunnelListReq{})
	requireServiceError(t, err, nil)
	if total != 1 {
		t.Fatalf("there are %d managed tunnels after merging, want one", total)
	}
}
