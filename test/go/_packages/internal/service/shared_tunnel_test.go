package service

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"

	"yagp/internal/dto"
	bizerrors "yagp/internal/errors"
	"yagp/internal/model"
	"yagp/internal/utils"
	"yagp/pkg/gost"
	"yagp/pkg/nodegrpc"

	"gorm.io/gorm"
)

func TestEndpointTunnelsShareAnExitRelay(t *testing.T) {
	db := newServiceTestDB(t)
	loadServiceConfig(t, "report:\n  listen: 127.0.0.1:39001\n")
	firstRemote, secondRemote := &lifecycleNode{t: t}, &lifecycleNode{t: t}
	exitRemote := &sharedRelayTestNode{lifecycleNode: lifecycleNode{t: t}, policies: make(map[string]nodegrpc.RelayAuthConfig)}
	first := startServiceTestNode(t, firstRemote)
	second := startServiceTestNode(t, secondRemote)
	exit := startServiceTestNode(t, exitRemote)
	first.Name, second.Name, exit.Name = "first entry", "second entry", "shared exit"
	exit.Address = "198.51.100.21"
	saveServiceFixtures(t, db, &first, &second, &exit)
	firstEntry := model.GostRule{Name: "first source", NodeID: &first.ID, Role: model.EndpointRoleEntry, Protocol: model.RuleProtocolTCP, ListenPort: 18001}
	secondEntry := model.GostRule{Name: "second source", NodeID: &second.ID, Role: model.EndpointRoleEntry, Protocol: model.RuleProtocolTCP, ListenPort: 18002}
	sharedExit := model.GostRule{Name: "shared destination", NodeID: &exit.ID, Role: model.EndpointRoleExit, Protocol: "tcp", ListenPort: 18443, Targets: []string{"app.test:443"}}
	saveServiceFixtures(t, db, &firstEntry, &secondEntry, &sharedExit)
	tunnels := NewTunnelService(db)

	left := startSharedTunnel(t, tunnels, "first path", firstEntry.ID, sharedExit.ID)
	secondRemote.mu.Lock()
	secondRemote.reject = map[string]int{"POST /config/chains": http.StatusBadGateway}
	secondRemote.mu.Unlock()
	_, err := tunnels.Create(&dto.CreateTunnelReq{Name: "failed path", EntryRuleID: secondEntry.ID, ExitRuleID: sharedExit.ID, AllowlistNodeIDs: []uint{}},
		testAdmin, testIP, testAgent)
	requireServiceError(t, err, bizerrors.ErrTunnelChainCreateFailed)
	if relay := exitRemote.service(left.ServiceID); relay.Name == "" || firstRemote.chain(left.ChainID).Name == "" {
		t.Fatal("rolling back a failed start removed the existing tunnel's Relay or chain")
	}
	if policy := exitRemote.policy(left.ServiceID); len(policy.Users) != 1 ||
		policy.Users[0].Username != firstRemote.chain(left.ChainID).Hops[0].Nodes[0].Connector.Auth.Username {
		t.Fatal("rolling back a failed start did not keep only the existing tunnel's Relay credentials")
	}
	secondRemote.mu.Lock()
	secondRemote.reject = nil
	secondRemote.mu.Unlock()
	right := startSharedTunnel(t, tunnels, "second path", secondEntry.ID, sharedExit.ID)
	left = loadSharedTunnel(t, tunnels, left.ID)
	if left.ServiceID == "" || left.ServiceID != right.ServiceID || left.ChainID == "" || right.ChainID == "" || left.ChainID == right.ChainID {
		t.Fatalf("expected one Relay and two independent chains, got %s %s and %s %s", left.ServiceID, left.ChainID, right.ServiceID, right.ChainID)
	}
	if relay := exitRemote.service(left.ServiceID); relay.Addr != ":18443" || relay.Handler == nil || relay.Handler.Type != "relay" {
		t.Fatalf("shared Relay = %+v", relay)
	}
	var users []string
	for _, link := range []struct {
		tunnel *model.GostTunnel
		remote *lifecycleNode
	}{{left, firstRemote}, {right, secondRemote}} {
		chain := link.remote.chain(link.tunnel.ChainID)
		addr, ok := chain.TunnelRelayAddress()
		if !ok || addr != "198.51.100.21:18443" {
			t.Fatalf("chain %s connects to %q", link.tunnel.ChainID, addr)
		}
		credentials := chain.Hops[0].Nodes[0].Connector.Auth
		policy := exitRemote.policy(left.ServiceID)
		if credentials == nil || !slices.ContainsFunc(policy.Users, func(user nodegrpc.RelayAuthUser) bool {
			return user.Username == credentials.Username && user.Password == credentials.Password
		}) {
			t.Fatalf("chain %s is missing its Relay authentication policy", link.tunnel.ChainID)
		}
		users = append(users, credentials.Username)
	}
	if users[0] == users[1] {
		t.Fatal("independent tunnels use the same Relay credentials")
	}

	requireServiceError(t, tunnels.Stop(left.ID, testAdmin, testIP, testAgent), nil)
	left, right = loadSharedTunnel(t, tunnels, left.ID), loadSharedTunnel(t, tunnels, right.ID)
	if left.Status != model.TunnelStatusStopped || left.EntryRule.Status != model.RuleStatusStopped ||
		right.Status != model.TunnelStatusRunning || right.EntryRule.Status != model.RuleStatusRunning ||
		right.ExitRule.Status != model.RuleStatusRunning {
		t.Fatalf("stopping one tunnel changed shared endpoint state: left %s/%s, right %s/%s/%s",
			left.Status, left.EntryRule.Status, right.Status, right.EntryRule.Status, right.ExitRule.Status)
	}
	if firstRemote.chain(left.ChainID).Name != "" || secondRemote.chain(right.ChainID).Name == "" ||
		exitRemote.service(right.ServiceID).Name == "" {
		t.Fatal("stopping one tunnel removed another tunnel's resources or retained its own chain")
	}
	if policy := exitRemote.policy(right.ServiceID); len(policy.Users) != 1 || policy.Users[0].Username != users[1] {
		t.Fatal("stopping one tunnel did not revoke only its Relay credentials")
	}

	requireServiceError(t, tunnels.Start(left.ID, testAdmin, testIP, testAgent), nil)
	left, right = loadSharedTunnel(t, tunnels, left.ID), loadSharedTunnel(t, tunnels, right.ID)
	if left.Status != model.TunnelStatusRunning || left.ServiceID != right.ServiceID ||
		right.Status != model.TunnelStatusRunning || secondRemote.chain(right.ChainID).Name == "" ||
		len(exitRemote.policy(right.ServiceID).Users) != 2 {
		t.Fatal("restarting the original tunnel failed to reuse the existing shared Relay")
	}
	_, err = NewRuleService(db).Update(sharedExit.ID, &dto.UpdateRuleReq{
		Name: sharedExit.Name, Protocol: "tcp", ListenPort: 18444, Targets: []string{"new.test:443"},
	}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	requireServiceError(t, tunnels.Stop(left.ID, testAdmin, testIP, testAgent), nil)
	if relay := exitRemote.service(right.ServiceID); relay.Addr != ":18443" {
		t.Fatalf("saving the endpoint changed the live Relay before tunnel startup: %+v", relay)
	}
	stopChain := "DELETE /config/chains/" + right.ChainID
	rightRelay := right.ServiceID
	secondRemote.rejectCommand(stopChain, http.StatusServiceUnavailable)
	requireServiceError(t, tunnels.Start(left.ID, testAdmin, testIP, testAgent), bizerrors.ErrTunnelEndpointRestart)
	right = loadSharedTunnel(t, tunnels, right.ID)
	requireRunningTunnelRelay(t, right, secondRemote, "198.51.100.21:18443")
	if listener := secondRemote.service(right.EntryRule.ServiceID); listener.Addr != ":18002" || exitRemote.service(rightRelay).Addr != ":18443" {
		t.Fatalf("an incomplete stop did not restore the peer's old listener and Relay: %+v", listener)
	}
	if saved := loadSharedTunnel(t, tunnels, left.ID); saved.Status != model.TunnelStatusStopped {
		t.Fatalf("startup continued after a peer failed to stop: %s", saved.Status)
	}
	secondRemote.rejectCommand(stopChain, 0)

	requireServiceError(t, tunnels.Start(left.ID, testAdmin, testIP, testAgent), nil)
	left, right = loadSharedTunnel(t, tunnels, left.ID), loadSharedTunnel(t, tunnels, right.ID)
	if left.ServiceID != right.ServiceID || left.ExitRule.AppliedEndpoint != nil || right.ExitRule.AppliedEndpoint != nil {
		t.Fatal("starting a stopped tunnel did not apply the pending endpoint change to its running peer")
	}
	for _, link := range []struct {
		tunnel *model.GostTunnel
		remote *lifecycleNode
	}{{left, firstRemote}, {right, secondRemote}} {
		requireRunningTunnelRelay(t, link.tunnel, link.remote, "198.51.100.21:18444")
	}
	if relay := exitRemote.service(right.ServiceID); relay.Addr != ":18444" || len(exitRemote.policy(right.ServiceID).Users) != 2 {
		t.Fatalf("restarted Relay did not use its edited listener and both credentials: %+v", relay)
	}

	requireServiceError(t, tunnels.Stop(left.ID, testAdmin, testIP, testAgent), nil)
	requireServiceError(t, tunnels.Stop(right.ID, testAdmin, testIP, testAgent), nil)
	right = loadSharedTunnel(t, tunnels, right.ID)
	if right.ExitRule.Status != model.RuleStatusStopped || exitRemote.service(left.ServiceID).Name != "" ||
		len(exitRemote.policy(left.ServiceID).Users) != 0 {
		t.Fatal("stopping the last tunnel did not remove its shared Relay")
	}
}

func TestCreatingTunnelAppliesPendingSharedEndpointChange(t *testing.T) {
	t.Run("starts the new tunnel", func(t *testing.T) {
		testCreatingTunnelAppliesPendingSharedEndpointChange(t, false)
	})
	t.Run("recovers existing tunnels if the new tunnel fails", func(t *testing.T) {
		testCreatingTunnelAppliesPendingSharedEndpointChange(t, true)
	})
}

func testCreatingTunnelAppliesPendingSharedEndpointChange(t *testing.T, failFirstStart bool) {
	t.Helper()
	db := newServiceTestDB(t)
	loadServiceConfig(t, "report:\n  listen: 127.0.0.1:39001\n")
	entryRemote, newEntryRemote := &lifecycleNode{t: t}, &lifecycleNode{t: t}
	exitRemote := &sharedRelayTestNode{lifecycleNode: lifecycleNode{t: t}, policies: make(map[string]nodegrpc.RelayAuthConfig)}
	entry, newEntry, exit := startServiceTestNode(t, entryRemote), startServiceTestNode(t, newEntryRemote), startServiceTestNode(t, exitRemote)
	entry.Name, newEntry.Name, exit.Name, exit.Address = "entry node", "new entry node", "exit node", "198.51.100.21"
	saveServiceFixtures(t, db, &entry, &newEntry, &exit)
	sharedExit := model.GostRule{Name: "shared exit", NodeID: &exit.ID, Role: model.EndpointRoleExit,
		Protocol: model.RuleProtocolTCP, ListenPort: 18443, Targets: []string{"app.test:443"}}
	otherExit := model.GostRule{Name: "other exit", NodeID: &exit.ID, Role: model.EndpointRoleExit,
		Protocol: model.RuleProtocolTCP, ListenPort: 18445, Targets: []string{"other.test:443"}}
	saveServiceFixtures(t, db, &sharedExit, &otherExit)
	entries := [3]model.GostRule{
		{Name: "first entry", ListenPort: 18001},
		{Name: "second entry", ListenPort: 18002},
		{Name: "stopped entry", ListenPort: 18003},
	}
	for i := range entries {
		entries[i].NodeID, entries[i].Role, entries[i].Protocol = &entry.ID, model.EndpointRoleEntry, model.RuleProtocolTCP
		saveServiceFixtures(t, db, &entries[i])
	}
	addedEntry := model.GostRule{Name: "new entry", NodeID: &newEntry.ID, Role: model.EndpointRoleEntry,
		Protocol: model.RuleProtocolTCP, ListenPort: 18004}
	saveServiceFixtures(t, db, &addedEntry)
	tunnels := NewTunnelService(db)
	first := startSharedTunnel(t, tunnels, "first path", entries[0].ID, sharedExit.ID)
	second := startSharedTunnel(t, tunnels, "second path", entries[1].ID, sharedExit.ID)
	other := startSharedTunnel(t, tunnels, "other path", entries[0].ID, otherExit.ID)
	stopped := startSharedTunnel(t, tunnels, "stopped path", entries[2].ID, sharedExit.ID)
	requireServiceError(t, tunnels.Stop(stopped.ID, testAdmin, testIP, testAgent), nil)

	rules := NewRuleService(db)
	_, err := rules.Update(sharedExit.ID, &dto.UpdateRuleReq{
		Name: sharedExit.Name, Protocol: "tcp", ListenPort: 18444, Targets: []string{"new.test:443"},
	}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	_, err = rules.Update(entries[0].ID, &dto.UpdateRuleReq{
		Name: entries[0].Name, Protocol: "tcp", ListenPort: 18005,
	}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)

	const newRelayAddress = "198.51.100.21:18444"
	if failFirstStart {
		const chainCommand = "POST /config/chains"
		newEntryRemote.rejectCommand(chainCommand, http.StatusBadGateway)
		_, err := tunnels.Create(&dto.CreateTunnelReq{
			Name: "new path", EntryRuleID: addedEntry.ID, ExitRuleID: sharedExit.ID, AllowlistNodeIDs: []uint{},
		}, testAdmin, testIP, testAgent)
		requireServiceError(t, err, bizerrors.ErrTunnelChainCreateFailed)
		for _, id := range []uint{first.ID, second.ID} {
			requireRunningTunnelRelay(t, loadSharedTunnel(t, tunnels, id), entryRemote, newRelayAddress)
		}
		relay := loadSharedTunnel(t, tunnels, first.ID).ServiceID
		if exitRemote.service(relay).Addr != ":18444" || len(exitRemote.policy(relay).Users) != 2 {
			t.Fatal("a failed new tunnel changed the restarted peers' shared Relay")
		}
		newEntryRemote.rejectCommand(chainCommand, 0)
	}

	added := startSharedTunnel(t, tunnels, "new path", addedEntry.ID, sharedExit.ID)
	requireRunningTunnelRelay(t, added, newEntryRemote, newRelayAddress)
	for _, id := range []uint{first.ID, second.ID} {
		tunnel := loadSharedTunnel(t, tunnels, id)
		requireRunningTunnelRelay(t, tunnel, entryRemote, newRelayAddress)
		if tunnel.ServiceID != added.ServiceID || tunnel.ExitRule.AppliedEndpoint != nil {
			t.Fatalf("tunnel %d did not share the updated Relay", id)
		}
	}
	relay := exitRemote.service(added.ServiceID)
	if relay.Addr != ":18444" || len(exitRemote.policy(added.ServiceID).Users) != 3 {
		t.Fatalf("updated Relay did not bind the new listener or retain its active users: %+v", relay)
	}
	other = loadSharedTunnel(t, tunnels, other.ID)
	requireRunningTunnelRelay(t, other, entryRemote, "198.51.100.21:18445")
	if listener := entryRemote.service(other.EntryRule.ServiceID); listener.Addr != ":18005" || other.EntryRule.AppliedEndpoint != nil {
		t.Fatalf("creating through the shared exit did not also apply its peer's edited shared entry: %+v", listener)
	}
	if saved := loadSharedTunnel(t, tunnels, stopped.ID); saved.Status != model.TunnelStatusStopped ||
		entryRemote.chain(stopped.ChainID).Name != "" {
		t.Fatal("adding a tunnel restarted a peer that had already been stopped")
	}
}

func requireRunningTunnelRelay(t *testing.T, tunnel *model.GostTunnel, remote *lifecycleNode, address string) {
	t.Helper()
	chain := remote.chain(tunnel.ChainID)
	if actual, ok := chain.TunnelRelayAddress(); tunnel.Status != model.TunnelStatusRunning || !ok || actual != address {
		t.Fatalf("tunnel %d has status %q and Relay address %q, want running at %q", tunnel.ID, tunnel.Status, actual, address)
	}
}

func TestSavingUnchangedSharedEntryDoesNotStageRestart(t *testing.T) {
	db := newServiceTestDB(t)
	loadServiceConfig(t, "report:\n  listen: 127.0.0.1:39001\n")
	entryRemote := &lifecycleNode{t: t}
	exitRemote := &lifecycleNode{t: t}
	entry, exit := startServiceTestNode(t, entryRemote), startServiceTestNode(t, exitRemote)
	entry.Name, exit.Name, exit.Address = "entry node", "exit node", "198.51.100.21"
	saveServiceFixtures(t, db, &entry, &exit)
	rules := NewRuleService(db)
	sharedEntry, err := rules.Create(&dto.CreateRuleReq{
		Name: "shared entry", NodeID: &entry.ID, Role: "entry", Protocol: "tcp", ListenPort: 18001, Targets: []string{},
	}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	firstExit := model.GostRule{Name: "first exit", NodeID: &exit.ID, Role: model.EndpointRoleExit,
		Protocol: model.RuleProtocolTCP, ListenPort: 18443, Targets: []string{"app.test:443"}}
	secondExit := model.GostRule{Name: "second exit", NodeID: &exit.ID, Role: model.EndpointRoleExit,
		Protocol: model.RuleProtocolTCP, ListenPort: 18444, Targets: []string{"new.test:443"}}
	saveServiceFixtures(t, db, &firstExit, &secondExit)
	tunnels := NewTunnelService(db)
	first := startSharedTunnel(t, tunnels, "first path", sharedEntry.ID, firstExit.ID)
	entryRemote.rejectCommand("DELETE /config/chains/"+first.ChainID, http.StatusServiceUnavailable)

	saved, err := rules.Update(sharedEntry.ID, &dto.UpdateRuleReq{
		Name: "renamed entry", Protocol: "tcp", ListenPort: sharedEntry.ListenPort, Remark: "new remark",
	}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	if saved.AppliedEndpoint != nil || saved.Name != "renamed entry" || saved.Remark != "new remark" {
		t.Fatalf("saving name and remark changed runtime configuration: %+v", saved)
	}
	startSharedTunnel(t, tunnels, "second path", sharedEntry.ID, secondExit.ID)
	first = loadSharedTunnel(t, tunnels, first.ID)
	requireRunningTunnelRelay(t, first, entryRemote, "198.51.100.21:18443")
}

func TestSharedExitRetainsItsDomainBypassUntilLastTunnelStops(t *testing.T) {
	db := newServiceTestDB(t)
	loadServiceConfig(t, "report:\n  listen: 127.0.0.1:39001\n")
	firstRemote := &redirectLifecycleNode{lifecycleNode: lifecycleNode{t: t}, mark: 1234}
	secondRemote := &redirectLifecycleNode{lifecycleNode: lifecycleNode{t: t}, mark: 1234}
	exitRemote := &sharedRelayTestNode{lifecycleNode: lifecycleNode{t: t}, policies: make(map[string]nodegrpc.RelayAuthConfig)}
	first, second, exit := startServiceTestNode(t, firstRemote), startServiceTestNode(t, secondRemote), startServiceTestNode(t, exitRemote)
	first.Name, second.Name, exit.Name = "first entry", "second entry", "shared exit"
	exit.Address = "192.0.2.20"
	saveServiceFixtures(t, db, &first, &second, &exit)
	firstEntry := model.GostRule{Name: "first source", NodeID: &first.ID, Role: model.EndpointRoleEntry,
		ListenIP: "127.0.0.1", ListenPort: 18001, Protocol: model.RuleProtocolTransparent}
	secondEntry := model.GostRule{Name: "second source", NodeID: &second.ID, Role: model.EndpointRoleEntry,
		ListenIP: "127.0.0.1", ListenPort: 18002, Protocol: model.RuleProtocolTransparent}
	sharedExit := model.GostRule{Name: "filtered exit", NodeID: &exit.ID, Role: model.EndpointRoleExit,
		ListenPort: 18443, Protocol: model.RuleProtocolTCP, UseOriginalTarget: true, ManualDomains: []string{"match.test"}}
	saveServiceFixtures(t, db, &firstEntry, &secondEntry, &sharedExit)
	tunnels := NewTunnelService(db)

	firstPath := startSharedTunnel(t, tunnels, "first path", firstEntry.ID, sharedExit.ID)
	secondPath := startSharedTunnel(t, tunnels, "second path", secondEntry.ID, sharedExit.ID)
	relay := firstPath.ServiceID
	policy := relayPolicyName(relay)
	if relay == "" || secondPath.ServiceID != relay {
		t.Fatalf("shared exit used different Relay services: %q and %q", relay, secondPath.ServiceID)
	}
	assertExitResources := func(running bool) {
		t.Helper()
		exitRemote.mu.Lock()
		defer exitRemote.mu.Unlock()
		var services, bypasses []string
		for path := range exitRemote.resources {
			if name, ok := strings.CutPrefix(path, "/config/services/"); ok {
				services = append(services, name)
			}
			if name, ok := strings.CutPrefix(path, "/config/bypasses/"); ok {
				bypasses = append(bypasses, name)
			}
		}
		slices.Sort(bypasses)
		if !running {
			if len(services) != 0 || !slices.Equal(bypasses, []string{gost.TunnelProbeBypassName}) {
				t.Fatalf("stopping the last tunnel left services %v and bypasses %v", services, bypasses)
			}
			return
		}
		if !slices.Equal(services, []string{relay}) || !slices.Equal(bypasses, []string{policy, gost.TunnelProbeBypassName}) {
			t.Fatalf("expected one shared Relay with a business and a probe bypass, got services %v and bypasses %v", services, bypasses)
		}
		var service gost.ServiceConfig
		requireServiceError(t, json.Unmarshal(exitRemote.resources["/config/services/"+relay], &service), nil)
		var bypass struct {
			Whitelist bool
			Matchers  []string
		}
		requireServiceError(t, json.Unmarshal(exitRemote.resources["/config/bypasses/"+policy], &bypass), nil)
		if service.Bypass != policy || !bypass.Whitelist || !slices.Equal(bypass.Matchers, []string{".match.test:443"}) {
			t.Fatalf("shared Relay bypass = %q, policy = %+v", service.Bypass, bypass)
		}
		var probeBypass struct {
			Whitelist bool
			Network   string
			Matchers  []string
		}
		requireServiceError(t, json.Unmarshal(exitRemote.resources["/config/bypasses/"+gost.TunnelProbeBypassName], &probeBypass), nil)
		if !slices.Equal(service.Bypasses, []string{gost.TunnelProbeBypassName}) || !probeBypass.Whitelist ||
			probeBypass.Network != "unix" || !slices.Equal(probeBypass.Matchers, []string{nodegrpc.TunnelProbeDomain}) {
			t.Fatalf("shared Relay probe bypasses = %v, probe policy = %+v", service.Bypasses, probeBypass)
		}
	}
	assertExitResources(true)
	requireServiceError(t, tunnels.Stop(firstPath.ID, testAdmin, testIP, testAgent), nil)
	assertExitResources(true)
	requireServiceError(t, tunnels.Stop(secondPath.ID, testAdmin, testIP, testAgent), nil)
	assertExitResources(false)
}

type sharedRelayTestNode struct {
	lifecycleNode
	policies map[string]nodegrpc.RelayAuthConfig
}

func (n *sharedRelayTestNode) policy(service string) nodegrpc.RelayAuthConfig {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.policies[service]
}

func (n *sharedRelayTestNode) Execute(ctx context.Context, req *nodegrpc.CommandRequest) (*nodegrpc.CommandReply, error) {
	service, ok := strings.CutPrefix(req.GetPath(), nodegrpc.RelayAuthPathPrefix)
	if !ok {
		return n.lifecycleNode.Execute(ctx, req)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	switch req.GetMethod() {
	case http.MethodPut:
		var policy nodegrpc.RelayAuthConfig
		if err := json.Unmarshal(req.GetBody(), &policy); err != nil {
			return &nodegrpc.CommandReply{StatusCode: http.StatusBadRequest}, nil
		}
		n.policies[service] = policy
	case http.MethodDelete:
		delete(n.policies, service)
	case http.MethodGet:
		_, configured := n.policies[service]
		data, err := json.Marshal(nodegrpc.RelayAuthStatus{Configured: configured})
		return &nodegrpc.CommandReply{StatusCode: http.StatusOK, Body: data}, err
	default:
		return &nodegrpc.CommandReply{StatusCode: http.StatusMethodNotAllowed}, nil
	}
	return &nodegrpc.CommandReply{StatusCode: http.StatusOK}, nil
}

func TestEndpointTunnelsShareAnEntryListener(t *testing.T) {
	db := newServiceTestDB(t)
	loadServiceConfig(t, "report:\n  listen: 127.0.0.1:39001\n")
	entryRemote, firstRemote, secondRemote := &lifecycleNode{t: t}, &lifecycleNode{t: t}, &lifecycleNode{t: t}
	entry := startServiceTestNode(t, entryRemote)
	first := startServiceTestNode(t, firstRemote)
	second := startServiceTestNode(t, secondRemote)
	entry.Name, first.Name, second.Name = "shared entry", "first exit", "second exit"
	first.Address, second.Address = "198.51.100.21", "198.51.100.22"
	saveServiceFixtures(t, db, &entry, &first, &second)
	sharedEntry := model.GostRule{Name: "shared source", NodeID: &entry.ID, Role: model.EndpointRoleEntry, Protocol: model.RuleProtocolTCP, ListenPort: 18001}
	firstExit := model.GostRule{Name: "first destination", NodeID: &first.ID, Role: model.EndpointRoleExit, Protocol: "tcp", ListenPort: 18443, Targets: []string{"one.test:443"}}
	secondExit := model.GostRule{Name: "second destination", NodeID: &second.ID, Role: model.EndpointRoleExit, Protocol: "tls", ListenPort: 18444, Targets: []string{"two.test:443"}}
	saveServiceFixtures(t, db, &sharedEntry, &firstExit, &secondExit)
	tunnels := NewTunnelService(db)

	left := startSharedTunnel(t, tunnels, "first path", sharedEntry.ID, firstExit.ID)
	right := startSharedTunnel(t, tunnels, "second path", sharedEntry.ID, secondExit.ID)
	left = loadSharedTunnel(t, tunnels, left.ID)
	listenerName := left.EntryRule.ServiceID
	leftRoute := sharedEntryRoute(t, entryRemote, left.ChainID)
	rightRoute := sharedEntryRoute(t, entryRemote, right.ChainID)
	if listenerName == "" || listenerName != right.EntryRule.ServiceID || leftRoute.Addr == rightRoute.Addr {
		t.Fatalf("expected one entry listener and independent route addresses, got %s %s %s", listenerName, leftRoute.Addr, rightRoute.Addr)
	}
	assertSharedEntryTargets(t, entryRemote, listenerName, leftRoute.Addr, rightRoute.Addr)
	if leftRoute.Forwarder == nil || len(leftRoute.Forwarder.Nodes) != 1 || leftRoute.Forwarder.Nodes[0].Addr != "one.test:443" ||
		rightRoute.Forwarder == nil || len(rightRoute.Forwarder.Nodes) != 1 || rightRoute.Forwarder.Nodes[0].Addr != "two.test:443" {
		t.Fatalf("entry route policy was not preserved: left=%+v right=%+v", leftRoute.Forwarder, rightRoute.Forwarder)
	}

	requireServiceError(t, tunnels.Stop(left.ID, testAdmin, testIP, testAgent), nil)
	right = loadSharedTunnel(t, tunnels, right.ID)
	if right.EntryRule.Status != model.RuleStatusRunning || right.Status != model.TunnelStatusRunning ||
		entryRemote.service(leftRoute.Name).Name != "" {
		t.Fatal("stopping one tunnel removed another tunnel's route or the shared listener")
	}
	listener := entryRemote.service(listenerName)
	if listener.Handler == nil {
		t.Fatal("stopping one tunnel left the entry listener without a handler")
	}
	if listener.Handler.Chain == right.ChainID {
		if listener.Forwarder == nil || len(listener.Forwarder.Nodes) != 1 || listener.Forwarder.Nodes[0].Addr != "two.test:443" {
			t.Fatal("entry listener lost the remaining tunnel's target after reverting to one route")
		}
	} else {
		if entryRemote.service(rightRoute.Name).Addr != rightRoute.Addr {
			t.Fatal("entry listener lost the remaining private forwarding service")
		}
		assertSharedEntryTargets(t, entryRemote, listenerName, rightRoute.Addr)
	}
	requireServiceError(t, tunnels.Stop(right.ID, testAdmin, testIP, testAgent), nil)
	if entryRemote.service(listenerName).Name != "" || entryRemote.service(rightRoute.Name).Name != "" {
		t.Fatal("stopping the last tunnel left the entry listener or route")
	}
}

func startSharedTunnel(t *testing.T, svc *TunnelService, name string, entry, exit uint) *model.GostTunnel {
	t.Helper()
	result, err := svc.Create(&dto.CreateTunnelReq{Name: name, EntryRuleID: entry, ExitRuleID: exit, AllowlistNodeIDs: []uint{}},
		testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	return result
}

func loadSharedTunnel(t *testing.T, svc *TunnelService, id uint) *model.GostTunnel {
	t.Helper()
	result, err := svc.GetByID(id)
	requireServiceError(t, err, nil)
	return result
}

func sharedEntryRoute(t *testing.T, node *lifecycleNode, chain string) gost.ServiceConfig {
	t.Helper()
	var names []string
	node.mu.Lock()
	for path := range node.resources {
		if name, ok := strings.CutPrefix(path, "/config/services/"); ok {
			names = append(names, name)
		}
	}
	node.mu.Unlock()
	for _, name := range names {
		svc := node.service(name)
		if svc.Handler != nil && svc.Handler.Chain == chain {
			host, _, err := net.SplitHostPort(svc.Addr)
			if err != nil || host != "127.0.0.1" {
				t.Fatalf("route for chain %s is not on loopback: %+v", chain, svc)
			}
			return svc
		}
	}
	t.Fatalf("route for chain %s not found", chain)
	return gost.ServiceConfig{}
}

func assertSharedEntryTargets(t *testing.T, node *lifecycleNode, name string, targets ...string) {
	t.Helper()
	listener := node.service(name)
	if listener.Addr != ":18001" || listener.Handler == nil || listener.Handler.Chain != "" ||
		listener.Forwarder == nil || len(listener.Forwarder.Nodes) != len(targets) {
		t.Fatalf("shared listener does not forward to its routes: %+v", listener)
	}
	addresses := make([]string, 0, len(targets))
	for _, route := range listener.Forwarder.Nodes {
		addresses = append(addresses, route.Addr)
	}
	slices.Sort(addresses)
	slices.Sort(targets)
	if !slices.Equal(addresses, targets) {
		t.Fatalf("local targets %v, want %v", addresses, targets)
	}
}

func TestSharedRelayBatchesSourceQueries(t *testing.T) {
	db := newServiceTestDB(t)
	remote := &sharedRelayTestNode{lifecycleNode: lifecycleNode{t: t}, policies: make(map[string]nodegrpc.RelayAuthConfig)}
	exit := startServiceTestNode(t, remote)
	source := model.GostNode{Name: "source", IPv4: "192.0.2.1", LocalAddresses: []string{"10.0.0.1"}}
	saveServiceFixtures(t, db, &exit, &source)
	reads := 0
	requireServiceError(t, db.Callback().Query().After("gorm:query").Register("count_source_queries", func(tx *gorm.DB) {
		if tx.Statement.Table == "nodes" {
			reads++
		}
	}), nil)
	var tunnels []model.GostTunnel
	const name = "relay-tunnel-1"
	exitRuleID := uint(1)
	for i := range 100 {
		tunnels = append(tunnels, model.GostTunnel{
			ID: uint(i + 1), EntryNodeID: source.ID, ExitNodeID: exit.ID, ExitRuleID: &exitRuleID,
			AutoAllowlist: i%2 == 0, AllowlistNodeIDs: []uint{source.ID},
			ServiceID: name, RelaySecret: "test-secret", Status: model.TunnelStatusRunning,
		})
	}
	svc := NewGostAllowlistService(db)
	cfg := gost.GostConfig{Services: []gost.ServiceConfig{{Name: name, Handler: &gost.HandlerConfig{Type: "relay", Auther: name}}}}
	for _, snapshot := range []sourceNodes{nil, {source.ID: &source}} {
		reads = 0
		_, err := svc.syncRelays(utils.GetGostClient(&exit), &cfg, exit.ID, tunnels, snapshot)
		requireServiceError(t, err, nil)
		wantReads := 0
		if snapshot == nil {
			wantReads = 1
		}
		users := remote.policy(name).Users
		if reads != wantReads || len(users) != len(tunnels) {
			t.Fatalf("source queries = %d, want %d for %d users, got %d users", reads, wantReads, len(tunnels), len(users))
		}
		for i, user := range users {
			want := []string{source.IPv4}
			if tunnels[i].AutoAllowlist {
				want = []string{"10.0.0.1", source.IPv4}
			}
			if !user.RestrictSources || !slices.Equal(user.AllowedIPs, want) {
				t.Fatalf("%s sources = %v, restricted = %t", user.Username, user.AllowedIPs, user.RestrictSources)
			}
		}
	}
}
