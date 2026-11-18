package service

import (
	"net/http"
	"testing"

	"yagp/internal/dto"
	bizerrors "yagp/internal/errors"
	"yagp/internal/model"
)

func TestRuleStartRetainsServiceUntilRollbackIsSaved(t *testing.T) {
	for _, removeFails := range []bool{false, true} {
		name := "rollback save fails"
		if removeFails {
			name = "rollback removal fails"
		}
		t.Run(name, func(t *testing.T) {
			db := newServiceTestDB(t)
			loadServiceConfig(t, "report:\n  listen: 127.0.0.1:39001\n")
			remote := &lifecycleNode{t: t}
			node := startServiceTestNode(t, remote)
			saveServiceFixtures(t, db, &node)
			rules := NewRuleService(db)
			rule, err := rules.Create(&dto.CreateRuleReq{
				NodeID: &node.ID, Name: "save failure", Protocol: "tcp", ListenPort: 8443, Targets: []string{"target.test:443"},
			}, testAdmin, testIP, testAgent)
			requireServiceError(t, err, nil)
			serviceID := entranceServiceName(rule.ID)
			remote.rejectCommand("POST /config?format=yaml", http.StatusServiceUnavailable)
			if removeFails {
				remote.rejectCommand("DELETE /config/services/"+serviceID, http.StatusServiceUnavailable)
			}
			requireServiceError(t, rules.Start(rule.ID, testAdmin, testIP, testAgent), bizerrors.ErrRuleStartFailed)
			stored, err := rules.GetByID(rule.ID)
			requireServiceError(t, err, nil)
			if stored.Status != model.RuleStatusError || stored.ServiceID != serviceID || (remote.service(serviceID).Name != "") != removeFails {
				t.Fatalf("failed start lost its retry identity or skipped rollback: %+v", stored)
			}
			var audits int64
			requireServiceError(t, db.Model(&model.OperationLog{}).Where("action = ?", model.ActionStart).Count(&audits).Error, nil)
			if audits != 0 {
				t.Fatal("failed start emitted a success audit")
			}
			remote.rejectCommand("POST /config?format=yaml", 0)
			remote.rejectCommand("DELETE /config/services/"+serviceID, 0)
			requireServiceError(t, rules.Stop(rule.ID, testAdmin, testIP, testAgent), nil)
			stored, err = rules.GetByID(rule.ID)
			requireServiceError(t, err, nil)
			if stored.Status != model.RuleStatusStopped || stored.ServiceID != "" || remote.service(serviceID).Name != "" {
				t.Fatalf("retry did not finish cleanup: %+v", stored)
			}
			requireServiceError(t, rules.Start(rule.ID, testAdmin, testIP, testAgent), nil)
		})
	}
}
