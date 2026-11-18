package service

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"yagp/internal/dto"
	bizerrors "yagp/internal/errors"
	"yagp/internal/model"
	"yagp/pkg/gost"
)

func TestLegacyTunnelStartRetainsRetryStateOnPersistenceFailure(t *testing.T) {
	for _, failure := range []string{"exit config", "entry config", "runtime tracking", "running status", "rollback removal"} {
		t.Run(failure, func(t *testing.T) {
			service, tunnel, entry, exit := newLegacyTunnelTest(t)
			db := service.tunnelRepo.DB
			switch failure {
			case "exit config":
				exit.rejectCommand("POST /config?format=yaml", http.StatusServiceUnavailable)
			case "entry config":
				entry.rejectCommand("POST /config?format=yaml", http.StatusServiceUnavailable)
			case "runtime tracking", "running status":
				state := "error"
				if failure == "running status" {
					state = "running"
				}
				requireServiceError(t, db.Exec("CREATE TRIGGER reject_tunnel_start BEFORE UPDATE ON tunnels WHEN NEW.status = '"+state+"' BEGIN SELECT RAISE(ABORT, 'test write rejected'); END").Error, nil)
			case "rollback removal":
				entry.rejectCommand("POST /config/chains", http.StatusServiceUnavailable)
				exit.rejectCommand("DELETE /config/services/"+relayServiceName(tunnel.ID), http.StatusServiceUnavailable)
			}
			if err := service.Start(tunnel.ID, testAdmin, testIP, testAgent); err == nil {
				t.Fatal("failed persistence or rollback was reported as successful")
			}
			stored, err := service.GetByID(tunnel.ID)
			requireServiceError(t, err, nil)
			if failure == "runtime tracking" {
				if stored.Status != model.TunnelStatusStopped || stored.ServiceID != "" || stored.ChainID != "" {
					t.Fatalf("failed tracking changed runtime: %+v", stored)
				}
			} else if stored.Status != model.TunnelStatusError || stored.ServiceID != relayServiceName(tunnel.ID) || stored.ChainID != tunnelChainName(tunnel.ID) {
				t.Fatalf("failed start lost its retry identity: %+v", stored)
			}
			if entry.chain(tunnelChainName(tunnel.ID)).Name != "" || (exit.service(relayServiceName(tunnel.ID)).Name != "") != (failure == "rollback removal") {
				t.Fatal("failed start skipped its available cleanup")
			}
			var audits int64
			requireServiceError(t, db.Model(&model.OperationLog{}).Where("action = ?", model.ActionStart).Count(&audits).Error, nil)
			if audits != 0 {
				t.Fatal("failed start emitted a success audit")
			}
			entry.rejectCommand("POST /config?format=yaml", 0)
			entry.rejectCommand("POST /config/chains", 0)
			exit.rejectCommand("POST /config?format=yaml", 0)
			exit.rejectCommand("DELETE /config/services/"+relayServiceName(tunnel.ID), 0)
			if failure == "runtime tracking" || failure == "running status" {
				requireServiceError(t, db.Exec("DROP TRIGGER reject_tunnel_start").Error, nil)
			}
			requireServiceError(t, service.Start(tunnel.ID, testAdmin, testIP, testAgent), nil)
			stored, err = service.GetByID(tunnel.ID)
			requireServiceError(t, err, nil)
			if stored.Status != model.TunnelStatusRunning || entry.chain(stored.ChainID).Name == "" || exit.service(stored.ServiceID).Name == "" {
				t.Fatalf("retry did not start both tunnel endpoints: %+v", stored)
			}
		})
	}
}

func TestSyncPreservesFailedLegacyTunnelUntilRestart(t *testing.T) {
	service, tunnel, entry, exit := newLegacyTunnelTest(t)
	chainName := tunnelChainName(tunnel.ID)
	entry.rejectCommand("POST /config?format=yaml", http.StatusServiceUnavailable)
	entry.rejectCommand("DELETE /config/chains/"+chainName, http.StatusServiceUnavailable)
	if err := service.Start(tunnel.ID, testAdmin, testIP, testAgent); err == nil {
		t.Fatal("expected failed entry save and rollback")
	}
	stored, err := service.GetByID(tunnel.ID)
	requireServiceError(t, err, nil)
	chain := entry.chain(chainName)
	if stored.Status != model.TunnelStatusError || chain.Name == "" || exit.service(stored.ServiceID).Name != "" {
		t.Fatalf("expected only the entry chain to survive rollback, got %+v", stored)
	}

	cfg, err := json.Marshal(gost.GostConfig{Chains: []gost.ChainConfig{chain}})
	requireServiceError(t, err, nil)
	entry.mu.Lock()
	entry.gostConfig = string(cfg)
	entry.mu.Unlock()
	entry.rejectCommand("DELETE /config/chains/"+chainName, 0)
	entry.rejectCommand("POST /config?format=yaml", 0)
	NewRuleSyncService(service.tunnelRepo.DB).syncNodeRules(*stored.EntryNode, map[uint]*model.GostNode{stored.ExitNodeID: stored.ExitNode})
	stored, err = service.GetByID(tunnel.ID)
	requireServiceError(t, err, nil)
	if stored.Status != model.TunnelStatusError {
		t.Fatalf("sync changed partial startup to %s", stored.Status)
	}
	requireServiceError(t, service.Start(tunnel.ID, testAdmin, testIP, testAgent), nil)
	if exit.service(stored.ServiceID).Name == "" {
		t.Fatal("retry did not rebuild the missing relay")
	}
}

func TestLegacyTunnelRemovalRetainsUnconfirmedRuntime(t *testing.T) {
	for _, operation := range []string{"stop", "delete"} {
		for _, failure := range []string{"chain removal", "relay removal", "entry config", "exit config", "runtime persistence", "offline entry", "offline exit"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				service, tunnel, entry, exit := newLegacyTunnelTest(t)
				db := service.tunnelRepo.DB
				requireServiceError(t, service.Start(tunnel.ID, testAdmin, testIP, testAgent), nil)
				tunnel, err := service.GetByID(tunnel.ID)
				requireServiceError(t, err, nil)
				remove := service.Stop
				if operation == "delete" {
					remove = service.Delete
				}
				switch failure {
				case "chain removal":
					entry.rejectCommand("DELETE /config/chains/"+tunnel.ChainID, http.StatusServiceUnavailable)
				case "relay removal":
					exit.rejectCommand("DELETE /config/services/"+tunnel.ServiceID, http.StatusServiceUnavailable)
				case "entry config":
					entry.rejectCommand("POST /config?format=yaml", http.StatusServiceUnavailable)
				case "exit config":
					exit.rejectCommand("POST /config?format=yaml", http.StatusServiceUnavailable)
				case "runtime persistence":
					requireServiceError(t, db.Exec("CREATE TRIGGER reject_tunnel_stop BEFORE UPDATE ON tunnels WHEN NEW.status = 'stopped' BEGIN SELECT RAISE(ABORT, 'test write rejected'); END").Error, nil)
				case "offline entry":
					// Health checks can mark a legacy tunnel stopped while its runtime is still live.
					updateServiceFields(t, db, tunnel, map[string]any{"status": model.TunnelStatusStopped})
					updateServiceFields(t, db, tunnel.EntryNode, map[string]any{"status": model.NodeStatusOffline})
				case "offline exit":
					updateServiceFields(t, db, tunnel, map[string]any{"status": model.TunnelStatusError})
					updateServiceFields(t, db, tunnel.ExitNode, map[string]any{"status": model.NodeStatusOffline})
				}
				if err := remove(tunnel.ID, testAdmin, testIP, testAgent); err == nil {
					t.Fatal("failed cleanup was reported as successful")
				}
				stored, err := service.GetByID(tunnel.ID)
				requireServiceError(t, err, nil)
				if stored.Status != tunnel.Status || stored.ServiceID != tunnel.ServiceID || stored.ChainID != tunnel.ChainID {
					t.Fatalf("failed removal lost its retry state: %+v", stored)
				}
				if failure == "relay removal" && exit.service(tunnel.ServiceID).Name == "" {
					t.Fatal("test failed to retain the rejected Relay")
				}
				var audits int64
				requireServiceError(t, db.Model(&model.OperationLog{}).Where("action IN ?", []string{model.ActionStop, model.ActionDelete}).Count(&audits).Error, nil)
				if audits != 0 {
					t.Fatal("failed removal emitted a success audit")
				}
				entry.rejectCommand("DELETE /config/chains/"+tunnel.ChainID, 0)
				exit.rejectCommand("DELETE /config/services/"+tunnel.ServiceID, 0)
				entry.rejectCommand("POST /config?format=yaml", 0)
				exit.rejectCommand("POST /config?format=yaml", 0)
				if failure == "runtime persistence" {
					requireServiceError(t, db.Exec("DROP TRIGGER reject_tunnel_stop").Error, nil)
				}
				updateServiceFields(t, db, tunnel.EntryNode, map[string]any{"status": model.NodeStatusOnline})
				updateServiceFields(t, db, tunnel.ExitNode, map[string]any{"status": model.NodeStatusOnline})
				requireServiceError(t, remove(tunnel.ID, testAdmin, testIP, testAgent), nil)
				if entry.chain(tunnel.ChainID).Name != "" || exit.service(tunnel.ServiceID).Name != "" {
					t.Fatal("successful retry left live tunnel resources")
				}
				stored, err = service.GetByID(tunnel.ID)
				if operation == "delete" {
					requireServiceError(t, err, bizerrors.ErrTunnelNotFound)
					return
				}
				requireServiceError(t, err, nil)
				if stored.Status != model.TunnelStatusStopped || stored.ServiceID != "" || stored.ChainID != "" {
					t.Fatalf("confirmed stop retained runtime: %+v", stored)
				}
			})
		}
	}
}

func TestLegacyTunnelDeletePropagatesDatabaseFailureAfterCleanup(t *testing.T) {
	service, tunnel, _, _ := newLegacyTunnelTest(t)
	db := service.tunnelRepo.DB
	requireServiceError(t, service.Start(tunnel.ID, testAdmin, testIP, testAgent), nil)
	requireServiceError(t, db.Exec("CREATE TRIGGER reject_tunnel_delete BEFORE UPDATE OF deleted_at ON tunnels BEGIN SELECT RAISE(ABORT, 'delete persistence unavailable'); END").Error, nil)
	if err := service.Delete(tunnel.ID, testAdmin, testIP, testAgent); err == nil || !strings.Contains(err.Error(), "delete persistence unavailable") {
		t.Fatalf("delete write failure = %v", err)
	}
	stored, err := service.GetByID(tunnel.ID)
	requireServiceError(t, err, nil)
	if stored.Status != model.TunnelStatusStopped || stored.ServiceID != "" || stored.ChainID != "" {
		t.Fatalf("completed cleanup was not persisted: %+v", stored)
	}
	requireServiceError(t, db.Exec("DROP TRIGGER reject_tunnel_delete").Error, nil)
	requireServiceError(t, service.Delete(tunnel.ID, testAdmin, testIP, testAgent), nil)
}

func newLegacyTunnelTest(t *testing.T) (*TunnelService, *model.GostTunnel, *lifecycleNode, *lifecycleNode) {
	t.Helper()
	db := newServiceTestDB(t)
	entry, exit := &lifecycleNode{t: t}, &lifecycleNode{t: t}
	entryNode, exitNode := startServiceTestNode(t, entry), startServiceTestNode(t, exit)
	exitNode.Name = "exit"
	saveServiceFixtures(t, db, &entryNode, &exitNode)
	service := NewTunnelService(db)
	tunnel, err := service.Create(&dto.CreateTunnelReq{
		Name: "legacy relay", EntryNodeID: entryNode.ID, ExitNodeID: exitNode.ID, Protocol: "tcp", RelayPort: 8443,
	}, testAdmin, testIP, testAgent)
	requireServiceError(t, err, nil)
	return service, tunnel, entry, exit
}
