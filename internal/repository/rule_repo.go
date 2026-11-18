package repository

import (
	"errors"
	"math"
	"slices"

	"yagp/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrTrafficCounterOverflow = errors.New("traffic counter limit exceeded")

// RuleRepository 规则仓库
type RuleRepository struct {
	*BaseRepository
}

// NewRuleRepository 创建规则仓库
func NewRuleRepository(db *gorm.DB) *RuleRepository {
	return &RuleRepository{
		BaseRepository: NewBaseRepository(db),
	}
}

// Create 创建规则
func (r *RuleRepository) Create(rule *model.GostRule) error {
	return r.DB.Create(rule).Error
}

// Update 更新规则
func (r *RuleRepository) Update(rule *model.GostRule) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		var current model.GostRule
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("node_id", "relay_input_bytes", "relay_output_bytes", "relay_service_created_at").
			First(&current, rule.ID).Error; err != nil {
			return err
		}
		if current.NodeID != nil && (rule.NodeID == nil || *rule.NodeID != *current.NodeID) && current.RelayTraffic.ServiceCreatedAt > 0 {
			history := model.ExitRelayHistory{
				RuleID: rule.ID, NodeID: *current.NodeID,
				InputBytes: current.RelayTraffic.InputBytes, OutputBytes: current.RelayTraffic.OutputBytes,
			}
			if err := tx.Create(&history).Error; err != nil {
				return err
			}
			// A listener on the destination starts with a new counter checkpoint.
			if err := tx.Model(rule).UpdateColumns(map[string]any{
				"relay_counter_id": "", "relay_input_bytes": 0, "relay_output_bytes": 0,
				"relay_last_input_bytes": 0, "relay_last_output_bytes": 0, "relay_service_created_at": 0,
			}).Error; err != nil {
				return err
			}
		}
		return tx.Model(rule).Select("NodeID", "Name", "Role", "Protocol", "ListenIP", "ListenPort", "ProtocolOptions",
			"Targets", "UseClashRuleset", "UseOriginalTarget", "RulesetIDs", "ManualDomains", "Strategy", "EnableTLS",
			"Remark", "AppliedEndpoint", "UpdatedAt").Updates(rule).Error
	})
}

// Delete 删除规则
func (r *RuleRepository) Delete(id uint) error {
	return r.DB.Delete(&model.GostRule{}, id).Error
}

// FindByID 根据 ID 查询规则
func (r *RuleRepository) FindByID(id uint) (*model.GostRule, error) {
	var rule model.GostRule
	err := r.DB.Preload("Node").Preload("Tunnel").Preload("Tunnel.EntryNode").Preload("Tunnel.ExitNode").First(&rule, id).Error
	if err != nil {
		return nil, err
	}
	return &rule, nil
}

// List 查询规则列表
func (r *RuleRepository) List(opt *QueryOption) ([]model.GostRule, int64, error) {
	var rules []model.GostRule
	var total int64

	db := r.DB.Model(&model.GostRule{})

	// 应用条件过滤
	db = ApplyConditions(db, opt)

	// 统计总数（包含过滤条件）
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// 预加载节点和隧道
	db = db.Preload("Node").Preload("Tunnel").Preload("Tunnel.EntryNode").Preload("Tunnel.ExitNode")

	// 默认按创建时间倒序
	if opt == nil || len(opt.Orders) == 0 {
		db = db.Order("created_at DESC")
	}

	// 应用分页
	db = ApplyPagination(db, opt)

	if err := db.Find(&rules).Error; err != nil {
		return nil, 0, err
	}

	return rules, total, nil
}

func (r *RuleRepository) onEntryNode(nodeID uint) *gorm.DB {
	tunnels := r.DB.Model(&model.GostTunnel{}).Select("id").Where("entry_node_id = ?", nodeID)
	return r.DB.Model(&model.GostRule{}).
		Where("((type = ? AND node_id = ?) OR (type = ? AND tunnel_id IN (?)))",
			model.RuleTypeForward, nodeID, model.RuleTypeTunnel, tunnels)
}

func (r *RuleRepository) FindByEntryNodeID(nodeID uint) ([]model.GostRule, error) {
	var rules []model.GostRule
	err := r.onEntryNode(nodeID).Find(&rules).Error
	return rules, err
}

// EntranceBindings finds the tunnels selecting any of the listed rule IDs.
func (r *RuleRepository) EntranceBindings(ruleIDs []uint) ([]model.GostTunnel, error) {
	var tunnels []model.GostTunnel
	err := r.DB.Select("id", "entry_rule_id", "exit_rule_id", "status").
		Where("entry_rule_id IN ? OR exit_rule_id IN ?", ruleIDs, ruleIDs).Order("id").Find(&tunnels).Error
	return tunnels, err
}

// ListenerConflicts checks whether a rule on the same ingress overlaps this bind.
func (r *RuleRepository) ListenerConflicts(nodeID uint, protocol model.RuleProtocol, ip string, port int, excludeID ...uint) (bool, error) {
	db := r.onEntryNode(nodeID).Where("(listen_port = ? OR applied_endpoint IS NOT NULL)", port)
	if len(excludeID) > 0 {
		db = db.Where("id != ?", excludeID[0])
	}
	var rules []model.GostRule
	if err := db.Select("protocol", "listen_ip", "listen_port", "applied_endpoint").Find(&rules).Error; err != nil {
		return false, err
	}
	network := protocol.ListenerNetwork()
	matches := func(listener model.EndpointConfig) bool {
		return listener.ListenPort == port && listener.Protocol.ListenerNetwork() == network &&
			(ip == "" || listener.ListenIP == "" || listener.ListenIP == ip)
	}
	return slices.ContainsFunc(rules, func(rule model.GostRule) bool {
		return matches(rule.EndpointConfig()) || rule.AppliedEndpoint != nil && matches(*rule.AppliedEndpoint)
	}), nil
}

// UpdateStatus 更新规则状态
func (r *RuleRepository) UpdateStatus(id uint, status model.RuleStatus) error {
	return r.UpdateField(&model.GostRule{}, id, "status", status)
}

// UpdateRuntime updates the service ID and status together.
func (r *RuleRepository) UpdateRuntime(id uint, serviceID string, status model.RuleStatus) error {
	return r.UpdateFields(&model.GostRule{}, id, map[string]interface{}{
		"service_id": serviceID,
		"status":     status,
	})
}

// StopEndpoint releases both the active listener and its saved configuration.
func (r *RuleRepository) StopEndpoint(id uint) error {
	return r.UpdateFields(&model.GostRule{}, id, map[string]any{
		"service_id": "", "status": model.RuleStatusStopped, "applied_endpoint": nil,
	})
}

// UpdateObserverID 更新观察器 ID
func (r *RuleRepository) UpdateObserverID(id uint, observerID string) error {
	return r.UpdateField(&model.GostRule{}, id, "observer_id", observerID)
}

// StopByNodeID 停止指定节点的所有规则
func (r *RuleRepository) StopByNodeID(nodeID uint) error {
	return r.DB.Model(&model.GostRule{}).
		Where("node_id = ? AND status = ?", nodeID, model.RuleStatusRunning).
		Update("status", model.RuleStatusStopped).Error
}

// UpdateTrafficSample atomically stores the observer checkpoint and increments rule totals.
func (r *RuleRepository) UpdateTrafficSample(id uint, delta, observed model.ObserverCounters) error {
	// Guard the addition in SQL so an overflowing report cannot corrupt SQLite's integer columns.
	result := r.DB.Model(&model.GostRule{}).Where("id = ?", id).
		Where("input_bytes <= ? AND output_bytes <= ? AND total_bytes <= ? AND total_requests <= ?",
			math.MaxInt64-delta.InputBytes, math.MaxInt64-delta.OutputBytes,
			math.MaxInt64-delta.InputBytes-delta.OutputBytes, math.MaxInt64-delta.Connections).
		Updates(map[string]interface{}{
			"input_bytes":           gorm.Expr("input_bytes + ?", delta.InputBytes),
			"output_bytes":          gorm.Expr("output_bytes + ?", delta.OutputBytes),
			"total_bytes":           gorm.Expr("total_bytes + ?", delta.InputBytes+delta.OutputBytes),
			"total_requests":        gorm.Expr("total_requests + ?", delta.Connections),
			"observer_input_bytes":  observed.InputBytes,
			"observer_output_bytes": observed.OutputBytes,
			"observer_connections":  observed.Connections,
		})
	// MySQL can report zero changed rows for an unchanged snapshot; only a positive delta can overflow.
	if result.Error == nil && result.RowsAffected == 0 && (delta.InputBytes != 0 || delta.OutputBytes != 0 || delta.Connections != 0) {
		return ErrTrafficCounterOverflow
	}
	return result.Error
}

// StopByTunnelIDs 停止指定隧道列表关联的所有规则
func (r *RuleRepository) StopByTunnelIDs(tunnelIDs []uint) error {
	if len(tunnelIDs) == 0 {
		return nil
	}
	return r.DB.Model(&model.GostRule{}).
		Where("tunnel_id IN ? AND status = ?", tunnelIDs, model.RuleStatusRunning).
		Update("status", model.RuleStatusStopped).Error
}
