package service

import (
	"context"
	"errors"
	"slices"

	"yagp/internal/model"
	"yagp/internal/repository"

	"gorm.io/gorm"
)

var ErrReportNodeMismatch = errors.New("rule does not belong to the reporting node")

type TrafficSample struct {
	RuleID uint
	model.ObserverCounters
	Reset bool
}

type ObserverService struct {
	db *gorm.DB
}

func NewObserverService(db *gorm.DB) *ObserverService {
	return &ObserverService{db: db}
}

func (s *ObserverService) RecordTraffic(ctx context.Context, nodeID uint, samples []TrafficSample) error {
	if len(samples) == 0 {
		return nil
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Limit SQL bind parameters while keeping the entire report in one transaction.
		for batch := range slices.Chunk(samples, 500) {
			if err := recordTrafficBatch(tx, nodeID, batch); err != nil {
				return err
			}
		}
		return nil
	})
}

func recordTrafficBatch(tx *gorm.DB, nodeID uint, samples []TrafficSample) error {
	ids := make([]uint, len(samples))
	for i, sample := range samples {
		ids[i] = sample.RuleID
	}
	var rules []model.GostRule
	err := tx.Select("id", "type", "node_id", "tunnel_id", "observer_input_bytes", "observer_output_bytes", "observer_connections").
		Preload("Tunnel", func(db *gorm.DB) *gorm.DB { return db.Select("id", "entry_node_id") }).
		Where("id IN ?", ids).Find(&rules).Error
	if err != nil {
		return err
	}
	byID := make(map[uint]*model.GostRule, len(rules))
	for i := range rules {
		byID[rules[i].ID] = &rules[i]
	}

	ruleRepo := repository.NewRuleRepository(tx)
	nodeRepo := repository.NewNodeRepository(tx)
	for _, sample := range samples {
		rule := byID[sample.RuleID]
		if rule == nil {
			continue // The service can still report its last interval after deletion.
		}
		if !reportsForNode(rule, nodeID) {
			return ErrReportNodeMismatch
		}
		observed := sample.ObserverCounters
		if sample.Reset {
			observed = model.ObserverCounters{}
		}
		if observed == rule.ObserverCounters {
			continue
		}
		delta := model.ObserverCounters{
			InputBytes:  counterIncrease(observed.InputBytes, rule.ObserverCounters.InputBytes),
			OutputBytes: counterIncrease(observed.OutputBytes, rule.ObserverCounters.OutputBytes),
			Connections: counterIncrease(observed.Connections, rule.ObserverCounters.Connections),
		}
		if err := ruleRepo.UpdateTrafficSample(rule.ID, delta, observed); err != nil {
			return err
		}
		rule.ObserverCounters = observed
		if delta.InputBytes != 0 || delta.OutputBytes != 0 {
			if err := nodeRepo.UpdateStats(nodeID, delta.InputBytes, delta.OutputBytes); err != nil {
				return err
			}
		}
	}
	return nil
}

func counterIncrease(observed, previous int64) int64 {
	if observed < previous {
		return observed
	}
	return observed - previous
}

func reportsForNode(rule *model.GostRule, nodeID uint) bool {
	if rule.Type == model.RuleTypeTunnel {
		return rule.Tunnel != nil && rule.Tunnel.EntryNodeID == nodeID
	}
	return rule.NodeID != nil && *rule.NodeID == nodeID
}
