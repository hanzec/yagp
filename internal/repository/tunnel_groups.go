package repository

import (
	"context"

	"yagp/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MigrateTunnelGroups keeps the oldest connection ID as the managed ID for each existing pair.
func MigrateTunnelGroups(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		firstConnections := tx.Model(&model.GostTunnel{}).Select("MIN(id)").Group("entry_node_id, exit_node_id")
		var groups []model.TunnelGroup
		if err := tx.Model(&model.GostTunnel{}).
			Select("id, entry_node_id, exit_node_id, name, remark, created_at, updated_at").
			Where("id IN (?)", firstConnections).
			Where(`NOT EXISTS (SELECT 1 FROM tunnel_groups WHERE
				tunnel_groups.entry_node_id = tunnels.entry_node_id AND tunnel_groups.exit_node_id = tunnels.exit_node_id)`).
			Find(&groups).Error; err != nil || len(groups) == 0 {
			return err
		}
		return tx.CreateInBatches(&groups, 100).Error
	})
}

func (r *TunnelRepository) groupQuery() *gorm.DB {
	return r.DB.Model(&model.TunnelGroup{}).
		Joins(`JOIN (?) AS state ON state.entry_node_id = tunnel_groups.entry_node_id
			AND state.exit_node_id = tunnel_groups.exit_node_id`, r.groupStates())
}

func (r *TunnelRepository) groupStates() *gorm.DB {
	return r.DB.Model(&model.GostTunnel{}).Select(`entry_node_id, exit_node_id, CASE
		WHEN COUNT(*) = SUM(CASE WHEN status = 'running' THEN 1 ELSE 0 END) THEN 'running'
		WHEN COUNT(*) = SUM(CASE WHEN status = 'stopped' THEN 1 ELSE 0 END) THEN 'stopped'
		ELSE 'error' END AS status`).Group("entry_node_id, exit_node_id")
}

func (r *TunnelRepository) FindGroupByPair(entryID, exitID uint) (*model.TunnelGroup, error) {
	var group model.TunnelGroup
	err := r.groupQuery().Select("tunnel_groups.*, state.status").
		Where("tunnel_groups.entry_node_id = ? AND tunnel_groups.exit_node_id = ?", entryID, exitID).Take(&group).Error
	return &group, err
}

func (r *TunnelRepository) FindGroup(id uint, acceptConnectionID bool) (*model.TunnelGroup, error) {
	query := r.whereGroupID(r.groupQuery(), id, acceptConnectionID).Select("tunnel_groups.*, state.status")
	var group model.TunnelGroup
	err := preloadTunnelConnections(query).Take(&group).Error
	return &group, err
}

func (r *TunnelRepository) GroupConnectionIDs(ctx context.Context, id uint) ([]uint, error) {
	pair := r.whereGroupID(r.DB.Model(&model.TunnelGroup{}), id, true).
		Select("tunnel_groups.entry_node_id, tunnel_groups.exit_node_id")
	var ids []uint
	err := r.DB.WithContext(ctx).Model(&model.GostTunnel{}).
		Where("(entry_node_id, exit_node_id) IN (?)", pair).Pluck("id", &ids).Error
	return ids, err
}

func (r *TunnelRepository) whereGroupID(query *gorm.DB, id uint, acceptConnectionID bool) *gorm.DB {
	if acceptConnectionID {
		// Old detail URLs can still resolve to their group. Write operations use only the managed ID.
		connection := r.DB.Model(&model.GostTunnel{}).Select("1").Where("id = ?", id).
			Where("entry_node_id = tunnel_groups.entry_node_id AND exit_node_id = tunnel_groups.exit_node_id")
		return query.Where("tunnel_groups.id = ? OR EXISTS (?)", id, connection)
	}
	return query.Where("tunnel_groups.id = ?", id)
}

func (r *TunnelRepository) ListGroups(opt *QueryOption) ([]model.TunnelGroup, int64, error) {
	query := ApplyConditions(r.groupQuery(), opt)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var groups []model.TunnelGroup
	err := preloadTunnelConnections(ApplyPagination(query, opt)).Select("tunnel_groups.*, state.status").
		Order("tunnel_groups.created_at DESC, tunnel_groups.id DESC").Find(&groups).Error
	return groups, total, err
}

func preloadTunnelConnections(query *gorm.DB) *gorm.DB {
	return query.Preload("Connections", func(db *gorm.DB) *gorm.DB {
		return db.Order("id")
	}).Preload("Connections.EntryNode").Preload("Connections.ExitNode").
		Preload("Connections.EntryRule").Preload("Connections.ExitRule")
}

func (r *TunnelRepository) AddGroupConnection(group *model.TunnelGroup, connection *model.GostTunnel) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit(clause.Associations).Create(connection).Error; err != nil {
			return err
		}
		if group.ID != 0 {
			return nil
		}
		// Allocate from the connection ID sequence so old URLs cannot collide with other groups.
		*group = model.TunnelGroup{
			ID: connection.ID, EntryNodeID: connection.EntryNodeID, ExitNodeID: connection.ExitNodeID,
			Name: connection.Name, Remark: connection.Remark, CreatedAt: connection.CreatedAt,
		}
		return tx.Create(group).Error
	})
}

func (r *TunnelRepository) SaveGroup(group *model.TunnelGroup, connection *model.GostTunnel, updateAllowlist bool) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		if connection != nil {
			if err := NewTunnelRepository(tx).Update(connection, updateAllowlist); err != nil {
				return err
			}
		}
		return tx.Model(group).Select("name", "remark", "entry_node_id", "exit_node_id", "updated_at").Updates(group).Error
	})
}

func (r *TunnelRepository) MergeGroup(source *model.TunnelGroup, connection *model.GostTunnel, updateAllowlist bool) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		if err := NewTunnelRepository(tx).Update(connection, updateAllowlist); err != nil {
			return err
		}
		return tx.Delete(source).Error
	})
}

func (r *TunnelRepository) DeleteGroup(group *model.TunnelGroup) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("entry_node_id = ? AND exit_node_id = ?", group.EntryNodeID, group.ExitNodeID).
			Delete(&model.GostTunnel{}).Error; err != nil {
			return err
		}
		return tx.Delete(group).Error
	})
}
