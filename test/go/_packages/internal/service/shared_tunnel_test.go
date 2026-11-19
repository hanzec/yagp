package service

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"reflect"
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
	first.IPv4, second.IPv4 = "192.0.2.11", "192.0.2.12"
	saveServiceFixtures(t, db, &first, &second, &exit)
	firstEntry := model.GostRule{Name: "first source", NodeID: &first.ID, Role: model.EndpointRoleEntry, Protocol: model.RuleProtocolTCP, ListenPort: 18001}
	secondEntry := model.GostRule{Name: "second source", NodeID: &second.ID, Role: model.EndpointRoleEntry, Protocol: model.RuleProtocolTCP, ListenPort: 18002}
	sharedExit := model.GostRule{Name: "shared destination", NodeID: &exit.ID, Role: model.EndpointRoleExit, Protocol: "tcp", ListenPort: 18443, Targets: []string{"app.test:443"}}
	saveServiceFixtures(t, db, &firstEntry, &secondEntry, &sharedExit)
	tunnels := NewTunnelService(db)

	left, err := tunnels.Create(&dto.CreateTunnelReq{Name: "first path", EntryRuleID: firstEntry.ID, ExitRuleID: sharedExit.ID}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	relay := exitRemote.service(left.ServiceID)
	originalChain := firstRemote.chain(left.ChainID)
	if relay.Handler == nil || relay.Handler.Auth == nil || relay.Handler.Auther != "" ||
		!relay.HasAdmission(gost.TunnelRelayAllowlistName(relay.Name)) {
		t.Fatalf("new Relay did not prebind a hot-updatable admission and static credentials: %+v", relay)
	}
	commandsBeforeSharing := exitRemote.commandCount()
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		exitRemote.rejectCommand(method+" /config/services/"+relay.Name, http.StatusServiceUnavailable)
		firstRemote.rejectCommand(method+" /config/chains/"+left.ChainID, http.StatusServiceUnavailable)
		firstRemote.rejectCommand(method+" /config/services/"+left.EntryRule.ServiceID, http.StatusServiceUnavailable)
	}

	// A failed unrestricted addition must restore the existing source restriction.
	secondRemote.rejectCommand("POST /config/chains", http.StatusBadGateway)
	_, err = tunnels.Create(&dto.CreateTunnelReq{Name: "failed path", EntryRuleID: secondEntry.ID, ExitRuleID: sharedExit.ID, AllowlistNodeIDs: []uint{}},
		testAdmin, testIP, testAgent)
	requireServiceError(t, err, bizerrors.ErrTunnelChainCreateFailed)
	requireSharedRelaySources(t, exitRemote, relay.Name, true, first.IPv4)
	secondRemote.rejectCommand("POST /config/chains", 0)

	// Sharing must preserve the live source when a surviving entry temporarily has no snapshot.
	requireServiceError(t, db.Model(&model.GostNode{}).Where("id = ?", first.ID).Update("ipv4", "").Error, nil)
	right, err := tunnels.Create(&dto.CreateTunnelReq{Name: "second path", EntryRuleID: secondEntry.ID, ExitRuleID: sharedExit.ID}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	left = loadSharedTunnel(t, tunnels, left.ID)
	if left.ServiceID == "" || left.ServiceID != right.ServiceID || left.ChainID == "" || right.ChainID == "" || left.ChainID == right.ChainID {
		t.Fatalf("expected one Relay and two independent chains, got %s %s and %s %s", left.ServiceID, left.ChainID, right.ServiceID, right.ChainID)
	}
	for _, link := range []struct {
		tunnel *model.GostTunnel
		remote *lifecycleNode
	}{{left, firstRemote}, {right, secondRemote}} {
		requireRunningTunnelRelay(t, link.tunnel, link.remote, "198.51.100.21:18443")
		chain := link.remote.chain(link.tunnel.ChainID)
		credentials := chain.Hops[0].Nodes[0].Connector.Auth
		if credentials == nil || *credentials != *relay.Handler.Auth {
			t.Fatalf("chain %s did not reuse the live Relay credentials", link.tunnel.ChainID)
		}
	}
	requireSharedRelaySources(t, exitRemote, relay.Name, true, first.IPv4, second.IPv4)
	requireUnchangedSharedRelay(t, exitRemote, relay, commandsBeforeSharing)
	if !reflect.DeepEqual(firstRemote.chain(left.ChainID), originalChain) {
		t.Fatal("adding a shared exit changed the existing entrance chain")
	}

	// Periodic reconciliation must retain all members rather than only the last tunnel.
	first.IPv4, second.IPv4 = "192.0.2.21", "192.0.2.22"
	requireServiceError(t, db.Model(&first).Update("ipv4", first.IPv4).Error, nil)
	requireServiceError(t, db.Model(&second).Update("ipv4", second.IPv4).Error, nil)
	cfg := gost.GostConfig{Services: []gost.ServiceConfig{relay}, Admissions: []gost.AdmissionConfig{exitRemote.admission(gost.TunnelRelayAllowlistName(relay.Name))}}
	_, err = tunnels.allowlist.syncRelays(utils.GetGostClient(&exit), &cfg, exit.ID,
		[]model.GostTunnel{*left, *right}, sourceNodes{first.ID: &first, second.ID: &second})
	requireServiceError(t, err, nil)
	requireSharedRelaySources(t, exitRemote, relay.Name, true, first.IPv4, second.IPv4)

	// Removing the original owner leaves its Relay identity and the surviving chain intact.
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		firstRemote.rejectCommand(method+" /config/chains/"+left.ChainID, 0)
		firstRemote.rejectCommand(method+" /config/services/"+left.EntryRule.ServiceID, 0)
		secondRemote.rejectCommand(method+" /config/chains/"+right.ChainID, http.StatusServiceUnavailable)
		secondRemote.rejectCommand(method+" /config/services/"+right.EntryRule.ServiceID, http.StatusServiceUnavailable)
	}
	requireServiceError(t, db.Model(&model.GostNode{}).Where("id = ?", second.ID).Update("ipv4", "").Error, nil)
	secondChain := secondRemote.chain(right.ChainID)
	requireServiceError(t, tunnels.Delete(left.ID, testAdmin, testIP, testAgent), nil)
	right = loadSharedTunnel(t, tunnels, right.ID)
	requireRunningTunnelRelay(t, right, secondRemote, "198.51.100.21:18443")
	if right.ServiceID != relay.Name || !reflect.DeepEqual(secondRemote.chain(right.ChainID), secondChain) {
		t.Fatal("deleting the Relay owner changed the surviving tunnel")
	}
	// With no per-member snapshot, keep the combined policy until the survivor reports again.
	requireSharedRelaySources(t, exitRemote, relay.Name, true, first.IPv4, second.IPv4)
	requireServiceError(t, db.Model(&second).Update("ipv4", second.IPv4).Error, nil)
	requireServiceError(t, tunnels.allowlist.writeRelaySources(utils.GetGostClient(&exit), &relay, []*model.GostTunnel{right}), nil)
	requireSharedRelaySources(t, exitRemote, relay.Name, true, second.IPv4)
	requireUnchangedSharedRelay(t, exitRemote, relay, commandsBeforeSharing)

	// A repaired sole survivor must keep using credentials owned by the live Relay.
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		secondRemote.rejectCommand(method+" /config/chains/"+right.ChainID, 0)
		secondRemote.rejectCommand(method+" /config/services/"+right.EntryRule.ServiceID, 0)
	}
	requireServiceError(t, tunnels.startEndpointTunnelRuntime(right), nil)
	if credentials := secondRemote.chain(right.ChainID).Hops[0].Nodes[0].Connector.Auth; credentials == nil || *credentials != *relay.Handler.Auth {
		t.Fatalf("sole survivor repair lost the shared Relay credentials: %+v", credentials)
	}
	requireUnchangedSharedRelay(t, exitRemote, relay, commandsBeforeSharing)

	exitRemote.rejectCommand("DELETE /config/services/"+relay.Name, 0)
	requireServiceError(t, tunnels.Stop(right.ID, testAdmin, testIP, testAgent), nil)
	if exitRemote.service(relay.Name).Name != "" {
		t.Fatal("stopping the last tunnel left its Relay running")
	}
}

func TestCreatingTunnelKeepsPendingSharedExitChange(t *testing.T) {
	for _, failFirstStart := range []bool{false, true} {
		name := "starts the new tunnel"
		if failFirstStart {
			name = "rolls back a failed new tunnel"
		}
		t.Run(name, func(t *testing.T) {
			testCreatingTunnelKeepsPendingSharedExitChange(t, failFirstStart)
		})
	}
}

func testCreatingTunnelKeepsPendingSharedExitChange(t *testing.T, failFirstStart bool) {
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
		Name: sharedExit.Name, Protocol: "tls", ListenPort: 18444, Targets: []string{"new.test:443"},
	}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	_, err = rules.Update(entries[0].ID, &dto.UpdateRuleReq{
		Name: entries[0].Name, Protocol: "tcp", ListenPort: 18005,
	}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	relay := exitRemote.service(first.ServiceID)
	commandsBeforeSharing := exitRemote.commandCount()
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		exitRemote.rejectCommand(method+" /config/services/"+relay.Name, http.StatusServiceUnavailable)
		for _, tunnel := range []*model.GostTunnel{first, second, other} {
			entryRemote.rejectCommand(method+" /config/chains/"+tunnel.ChainID, http.StatusServiceUnavailable)
		}
	}

	const liveRelayAddress = "198.51.100.21:18443"
	if failFirstStart {
		newEntryRemote.rejectCommand("POST /config/chains", http.StatusBadGateway)
		_, err := tunnels.Create(&dto.CreateTunnelReq{
			Name: "new path", EntryRuleID: addedEntry.ID, ExitRuleID: sharedExit.ID, AllowlistNodeIDs: []uint{},
		}, testAdmin, testIP, testAgent)
		requireServiceError(t, err, bizerrors.ErrTunnelChainCreateFailed)
		newEntryRemote.rejectCommand("POST /config/chains", 0)
	}

	added := startSharedTunnel(t, tunnels, "new path", addedEntry.ID, sharedExit.ID)
	requireRunningTunnelRelay(t, added, newEntryRemote, liveRelayAddress)
	if chain := newEntryRemote.chain(added.ChainID); chain.Hops[0].Nodes[0].Dialer.Type != "tcp" {
		t.Fatalf("new chain used the pending protocol instead of the live Relay: %+v", chain)
	}
	for _, id := range []uint{first.ID, second.ID} {
		tunnel := loadSharedTunnel(t, tunnels, id)
		requireRunningTunnelRelay(t, tunnel, entryRemote, liveRelayAddress)
		if tunnel.ServiceID != added.ServiceID || tunnel.ExitRule.AppliedEndpoint == nil || tunnel.ExitRule.ListenPort != 18443 {
			t.Fatalf("tunnel %d lost the pending exit configuration", id)
		}
	}
	var savedExit model.GostRule
	requireServiceError(t, db.First(&savedExit, sharedExit.ID).Error, nil)
	if savedExit.AppliedEndpoint == nil || savedExit.Protocol != "tls" || savedExit.ListenPort != 18444 {
		t.Fatalf("sharing cleared the saved exit changes: %+v", savedExit)
	}
	requireUnchangedSharedRelay(t, exitRemote, relay, commandsBeforeSharing)
	other = loadSharedTunnel(t, tunnels, other.ID)
	requireRunningTunnelRelay(t, other, entryRemote, "198.51.100.21:18445")
	if listener := entryRemote.service(other.EntryRule.ServiceID); listener.Addr != ":18001" || other.EntryRule.AppliedEndpoint == nil {
		t.Fatalf("sharing an exit applied an unrelated pending entry change: %+v", listener)
	}
	if saved := loadSharedTunnel(t, tunnels, stopped.ID); saved.Status != model.TunnelStatusStopped ||
		entryRemote.chain(stopped.ChainID).Name != "" {
		t.Fatal("adding a tunnel restarted a peer that had already been stopped")
	}
}

func requireSharedRelaySources(t *testing.T, remote *sharedRelayTestNode, name string, whitelist bool, sources ...string) {
	t.Helper()
	admission := remote.admission(gost.TunnelRelayAllowlistName(name))
	slices.Sort(sources)
	if admission.Name == "" || admission.Whitelist != whitelist || !slices.Equal(admission.Matchers, sources) {
		t.Fatalf("shared Relay source admission = %+v, want whitelist %t and %v", admission, whitelist, sources)
	}
}

func requireUnchangedSharedRelay(t *testing.T, remote *sharedRelayTestNode, original gost.ServiceConfig, since int) {
	t.Helper()
	if current := remote.service(original.Name); !reflect.DeepEqual(current, original) {
		t.Fatalf("sharing changed the live Relay: before %+v, after %+v", original, current)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	for _, command := range remote.commands[since:] {
		if command == "PUT /config/services/"+original.Name || command == "DELETE /config/services/"+original.Name {
			t.Fatalf("sharing reloaded the live Relay with %s", command)
		}
	}
}

func TestSharingLegacyRelayWithoutAdmissionDoesNotReloadIt(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		name := "unrestricted"
		if restricted {
			name = "requires a source restriction"
		}
		t.Run(name, func(t *testing.T) {
			db := newServiceTestDB(t)
			loadServiceConfig(t, "report:\n  listen: 127.0.0.1:39001\n")
			firstRemote, secondRemote := &lifecycleNode{t: t}, &lifecycleNode{t: t}
			exitRemote := &sharedRelayTestNode{lifecycleNode: lifecycleNode{t: t}, policies: make(map[string]nodegrpc.RelayAuthConfig)}
			first, second, exit := startServiceTestNode(t, firstRemote), startServiceTestNode(t, secondRemote), startServiceTestNode(t, exitRemote)
			first.Name, second.Name, exit.Name, exit.Address = "first", "second", "exit", "198.51.100.21"
			second.IPv4 = "192.0.2.12"
			saveServiceFixtures(t, db, &first, &second, &exit)
			leftEntry := model.GostRule{Name: "first source", NodeID: &first.ID, Role: model.EndpointRoleEntry, Protocol: model.RuleProtocolTCP, ListenPort: 18001}
			rightEntry := model.GostRule{Name: "second source", NodeID: &second.ID, Role: model.EndpointRoleEntry, Protocol: model.RuleProtocolTCP, ListenPort: 18002}
			sharedExit := model.GostRule{Name: "exit", NodeID: &exit.ID, Role: model.EndpointRoleExit, Protocol: "tcp", ListenPort: 18443, Targets: []string{"app.test:443"}}
			saveServiceFixtures(t, db, &leftEntry, &rightEntry, &sharedExit)
			tunnels := NewTunnelService(db)
			leftReq := &dto.CreateTunnelReq{Name: "existing", EntryRuleID: leftEntry.ID, ExitRuleID: sharedExit.ID, AllowlistNodeIDs: []uint{}}
			if restricted {
				leftReq.AllowlistNodeIDs = nil
			}
			left, err := tunnels.Create(leftReq, testAdmin, testIP, testAgent)
			requireServiceError(t, err, nil)
			relay := exitRemote.service(left.ServiceID)
			// Model a listener created before the panel prebound an admission resource.
			relay.Admission, relay.Admissions = "", nil
			data, err := json.Marshal(relay)
			requireServiceError(t, err, nil)
			exitRemote.mu.Lock()
			exitRemote.resources["/config/services/"+relay.Name] = data
			delete(exitRemote.resources, "/config/admissions/"+gost.TunnelRelayAllowlistName(relay.Name))
			exitRemote.mu.Unlock()
			before := exitRemote.commandCount()
			for _, method := range []string{http.MethodPut, http.MethodDelete} {
				exitRemote.rejectCommand(method+" /config/services/"+relay.Name, http.StatusServiceUnavailable)
				firstRemote.rejectCommand(method+" /config/chains/"+left.ChainID, http.StatusServiceUnavailable)
			}
			req := &dto.CreateTunnelReq{Name: "added", EntryRuleID: rightEntry.ID, ExitRuleID: sharedExit.ID, AllowlistNodeIDs: []uint{}}
			if restricted {
				req.AllowlistNodeIDs = nil
			}
			added, err := tunnels.Create(req, testAdmin, testIP, testAgent)
			if restricted {
				if err == nil {
					t.Fatal("source restrictions were silently accepted without a bound admission")
				}
			} else {
				requireServiceError(t, err, nil)
				requireRunningTunnelRelay(t, added, secondRemote, "198.51.100.21:18443")
			}
			requireRunningTunnelRelay(t, loadSharedTunnel(t, tunnels, left.ID), firstRemote, "198.51.100.21:18443")
			requireUnchangedSharedRelay(t, exitRemote, relay, before)
		})
	}
}

func TestSharingExistingAutherRelayKeepsItsDynamicPolicy(t *testing.T) {
	db := newServiceTestDB(t)
	loadServiceConfig(t, "report:\n  listen: 127.0.0.1:39001\n")
	firstRemote, secondRemote := &lifecycleNode{t: t}, &lifecycleNode{t: t}
	exitRemote := &sharedRelayTestNode{lifecycleNode: lifecycleNode{t: t}, policies: make(map[string]nodegrpc.RelayAuthConfig)}
	first, second, exit := startServiceTestNode(t, firstRemote), startServiceTestNode(t, secondRemote), startServiceTestNode(t, exitRemote)
	first.Name, second.Name, exit.Name, exit.Address = "first", "second", "exit", "198.51.100.21"
	first.IPv4, second.IPv4 = "192.0.2.11", "192.0.2.12"
	saveServiceFixtures(t, db, &first, &second, &exit)
	leftEntry := model.GostRule{Name: "first source", NodeID: &first.ID, Role: model.EndpointRoleEntry, Protocol: model.RuleProtocolTCP, ListenPort: 18001}
	rightEntry := model.GostRule{Name: "second source", NodeID: &second.ID, Role: model.EndpointRoleEntry, Protocol: model.RuleProtocolTCP, ListenPort: 18002}
	sharedExit := model.GostRule{Name: "exit", NodeID: &exit.ID, Role: model.EndpointRoleExit, Protocol: "tcp", ListenPort: 18443, Targets: []string{"app.test:443"}}
	saveServiceFixtures(t, db, &leftEntry, &rightEntry, &sharedExit)
	tunnels := NewTunnelService(db)
	left, err := tunnels.Create(&dto.CreateTunnelReq{Name: "existing", EntryRuleID: leftEntry.ID, ExitRuleID: sharedExit.ID}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	relay := exitRemote.service(left.ServiceID)
	relay.Handler.Auth, relay.Handler.Auther = nil, relay.Name
	relay.Admission, relay.Admissions = "", nil
	data, err := json.Marshal(relay)
	requireServiceError(t, err, nil)
	exitRemote.mu.Lock()
	exitRemote.resources["/config/services/"+relay.Name] = data
	exitRemote.mu.Unlock()
	before := exitRemote.commandCount()
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		exitRemote.rejectCommand(method+" /config/services/"+relay.Name, http.StatusServiceUnavailable)
	}
	right, err := tunnels.Create(&dto.CreateTunnelReq{Name: "added", EntryRuleID: rightEntry.ID, ExitRuleID: sharedExit.ID}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	for _, link := range []struct {
		tunnel *model.GostTunnel
		remote *lifecycleNode
	}{{left, firstRemote}, {right, secondRemote}} {
		credentials := link.remote.chain(link.tunnel.ChainID).Hops[0].Nodes[0].Connector.Auth
		if credentials == nil || !slices.ContainsFunc(exitRemote.policy(relay.Name).Users, func(user nodegrpc.RelayAuthUser) bool {
			return user.Username == credentials.Username && user.Password == credentials.Password && user.RestrictSources
		}) {
			t.Fatalf("legacy Auther lost credentials for tunnel %d", link.tunnel.ID)
		}
	}
	requireUnchangedSharedRelay(t, exitRemote, relay, before)
	firstChain, secondChain := firstRemote.chain(left.ChainID), secondRemote.chain(right.ChainID)
	firstAuth, secondAuth := firstChain.Hops[0].Nodes[0].Connector.Auth, secondChain.Hops[0].Nodes[0].Connector.Auth
	if firstAuth == nil || secondAuth == nil || *firstAuth == *secondAuth {
		t.Fatal("legacy Auther fixture did not retain independent credentials")
	}
	// Gost may lose the listener while the daemon still owns the original Auther policy.
	exitRemote.mu.Lock()
	delete(exitRemote.resources, "/config/services/"+relay.Name)
	exitRemote.resources["/config/authers/"+relay.Name] = json.RawMessage(`{"name":"` + relay.Name + `","plugin":{"type":"grpc","addr":"127.0.0.1:39001"}}`)
	exitRemote.mu.Unlock()
	requireServiceError(t, tunnels.startEndpointTunnelRuntime(right), nil)
	if recovered := exitRemote.service(relay.Name); recovered.Handler == nil || recovered.Handler.Auther != relay.Name || recovered.Handler.Auth != nil {
		t.Fatalf("recovering the missing legacy Relay replaced its Auther with static credentials: %+v", recovered)
	}
	if !reflect.DeepEqual(firstRemote.chain(left.ChainID), firstChain) || !reflect.DeepEqual(secondRemote.chain(right.ChainID), secondChain) {
		t.Fatal("recovering the missing Auther Relay changed an existing tunnel's credentials")
	}
	if users := exitRemote.policy(relay.Name).Users; len(users) != 2 || !slices.ContainsFunc(users, func(user nodegrpc.RelayAuthUser) bool {
		return user.Username == firstAuth.Username && user.Password == firstAuth.Password
	}) || !slices.ContainsFunc(users, func(user nodegrpc.RelayAuthUser) bool {
		return user.Username == secondAuth.Username && user.Password == secondAuth.Password
	}) {
		t.Fatalf("recovering the missing Auther Relay lost independent credentials: %+v", users)
	}
	requireServiceError(t, tunnels.Stop(left.ID, testAdmin, testIP, testAgent), nil)
	if users := exitRemote.policy(relay.Name).Users; len(users) != 1 || users[0].Username != relayServiceName(right.ID) {
		t.Fatalf("legacy Auther did not revoke the removed tunnel: %+v", users)
	}
	requireUnchangedSharedRelay(t, exitRemote, relay, before)
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
	originalTarget := false
	_, err := NewRuleService(db).Update(sharedExit.ID, &dto.UpdateRuleReq{
		Name: sharedExit.Name, Protocol: "tcp", ListenPort: sharedExit.ListenPort, UseOriginalTarget: &originalTarget,
		Targets: []string{"fixed.test:443"}, ManualDomains: []string{},
	}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	policyName := relayPolicyName(firstPath.ServiceID)
	exitRemote.rejectCommand("PUT /config/bypasses/"+policyName, http.StatusServiceUnavailable)
	beforeSharing := exitRemote.commandCount()
	originalRelay := exitRemote.service(firstPath.ServiceID)
	secondPath := startSharedTunnel(t, tunnels, "second path", secondEntry.ID, sharedExit.ID)
	requireUnchangedSharedRelay(t, exitRemote, originalRelay, beforeSharing)
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
	commands []string
}

func (n *sharedRelayTestNode) commandCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.commands)
}

func (n *sharedRelayTestNode) policy(service string) nodegrpc.RelayAuthConfig {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.policies[service]
}

func (n *sharedRelayTestNode) Execute(ctx context.Context, req *nodegrpc.CommandRequest) (*nodegrpc.CommandReply, error) {
	n.mu.Lock()
	n.commands = append(n.commands, req.GetMethod()+" "+req.GetPath())
	n.mu.Unlock()
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
