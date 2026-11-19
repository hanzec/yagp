package repository

import "yagp/internal/model"

type NodeSummary struct {
	Total  int64
	Online int64
}

func (r *NodeRepository) Summary() (NodeSummary, error) {
	var summary NodeSummary
	err := r.DB.Model(&model.GostNode{}).Select(`COUNT(*) AS total,
		COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS online`, model.NodeStatusOnline).
		Scan(&summary).Error
	return summary, err
}

type RuleSummary struct {
	Total       int64
	Running     int64
	ForwardType int64
	TunnelType  int64
}

func (r *RuleRepository) Summary() (RuleSummary, error) {
	var summary RuleSummary
	err := r.DB.Model(&model.GostRule{}).Select(`COUNT(*) AS total,
		COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS running,
		COALESCE(SUM(CASE WHEN type = ? THEN 1 ELSE 0 END), 0) AS forward_type,
		COALESCE(SUM(CASE WHEN type = ? THEN 1 ELSE 0 END), 0) AS tunnel_type`,
		model.RuleStatusRunning, model.RuleTypeForward, model.RuleTypeTunnel).
		Scan(&summary).Error
	return summary, err
}

type TunnelSummary struct {
	Total   int64
	Running int64
}

func (r *TunnelRepository) Summary() (TunnelSummary, error) {
	var summary TunnelSummary
	err := r.DB.Table("(?) AS state", r.groupStates()).Select(`COUNT(*) AS total,
		COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS running`, model.TunnelStatusRunning).
		Scan(&summary).Error
	return summary, err
}
