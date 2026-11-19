package repository

import (
	"context"

	"yagp/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MigrateTunnelGroups enrolls saved connections in ID order, retaining the oldest
// managed ID when joining groups. Listener configuration and probe IDs stay on the connections.
func MigrateTunnelGroups(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var connections []model.GostTunnel
		if err := tx.Where("group_id = 0").Order("id").Find(&connections).Error; err != nil || len(connections) == 0 {
			return err
		}
		repo := NewTunnelRepository(tx)
		for i := range connections {
			connection := &connections[i]
			groups, err := repo.MatchingGroups(connection.EntryNodeID, connection.ExitNodeID, 0)
			if err != nil {
				return err
			}
			if _, err := repo.assignGroup(connection, groups); err != nil {
				return err
			}
		}
		// A preview database can contain group rows from before membership was persisted.
		assigned := tx.Model(&model.GostTunnel{}).Select("group_id")
		return tx.Where("id NOT IN (?)", assigned).Delete(&model.TunnelGroup{}).Error
	})
}

func (r *TunnelRepository) groupQuery() *gorm.DB {
	return r.DB.Model(&model.TunnelGroup{}).
		Select("tunnel_groups.*, state.status").
		Joins("JOIN (?) AS state ON state.group_id = tunnel_groups.id", r.groupStates())
}

func (r *TunnelRepository) groupStates() *gorm.DB {
	const groupKey = "CASE WHEN group_id = 0 THEN id ELSE group_id END"
	return r.DB.Model(&model.GostTunnel{}).Select(groupKey + ` AS group_id, CASE
		WHEN COUNT(*) = SUM(CASE WHEN status = 'running' THEN 1 ELSE 0 END) THEN 'running'
		WHEN COUNT(*) = SUM(CASE WHEN status = 'stopped' THEN 1 ELSE 0 END) THEN 'stopped'
		ELSE 'error' END AS status`).Group(groupKey)
}

// MatchingGroups finds parallel paths and paths that meet head to tail, in ID order.
func (r *TunnelRepository) MatchingGroups(entryID, exitID, excludeGroupID uint) ([]model.TunnelGroup, error) {
	members := r.DB.Model(&model.GostTunnel{}).Select("group_id").
		Where("(entry_node_id = ? AND exit_node_id = ?) OR exit_node_id = ? OR entry_node_id = ?", entryID, exitID, entryID, exitID)
	var groups []model.TunnelGroup
	err := r.groupQuery().Where("tunnel_groups.id IN (?) AND tunnel_groups.id <> ?", members, excludeGroupID).
		Order("tunnel_groups.id").Find(&groups).Error
	return groups, err
}

func (r *TunnelRepository) FindGroup(id uint, acceptConnectionID bool) (*model.TunnelGroup, error) {
	query := r.whereGroupID(r.groupQuery(), id, acceptConnectionID)
	var group model.TunnelGroup
	err := preloadTunnelConnections(query).Take(&group).Error
	return &group, err
}

func (r *TunnelRepository) GroupConnectionIDs(ctx context.Context, id uint) ([]uint, error) {
	group := r.whereGroupID(r.DB.Model(&model.TunnelGroup{}), id, true).Select("tunnel_groups.id")
	var ids []uint
	err := r.DB.WithContext(ctx).Model(&model.GostTunnel{}).Where("group_id IN (?)", group).Pluck("id", &ids).Error
	return ids, err
}

func (r *TunnelRepository) whereGroupID(query *gorm.DB, id uint, acceptConnectionID bool) *gorm.DB {
	if acceptConnectionID {
		// Old detail URLs can still resolve to their group. Write operations use only the managed ID.
		member := r.DB.Model(&model.GostTunnel{}).Select("group_id").Where("id = ?", id)
		return query.Where("tunnel_groups.id = ? OR tunnel_groups.id IN (?)", id, member)
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
	err := preloadTunnelConnections(ApplyPagination(query, opt)).
		Order("tunnel_groups.created_at DESC, tunnel_groups.id DESC").Find(&groups).Error
	return groups, total, err
}

func preloadTunnelConnections(query *gorm.DB) *gorm.DB {
	return query.Preload("Connections", func(db *gorm.DB) *gorm.DB {
		return db.Order("id")
	}).Preload("Connections.EntryNode").Preload("Connections.ExitNode").
		Preload("Connections.EntryRule").Preload("Connections.ExitRule")
}

func (r *TunnelRepository) AddGroupConnection(connection *model.GostTunnel, matches []model.TunnelGroup) (*model.TunnelGroup, error) {
	var group *model.TunnelGroup
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit(clause.Associations).Create(connection).Error; err != nil {
			return err
		}
		var err error
		group, err = NewTunnelRepository(tx).assignGroup(connection, matches)
		return err
	})
	return group, err
}

func (r *TunnelRepository) assignGroup(connection *model.GostTunnel, matches []model.TunnelGroup) (*model.TunnelGroup, error) {
	var group *model.TunnelGroup
	if len(matches) == 0 {
		// Use the connection ID sequence so old detail URLs cannot collide with another tunnel.
		group = &model.TunnelGroup{
			ID: connection.ID, EntryNodeID: connection.EntryNodeID, ExitNodeID: connection.ExitNodeID,
			Name: connection.Name, Remark: connection.Remark, CreatedAt: connection.CreatedAt,
		}
		if err := r.DB.Where("id = ?", group.ID).FirstOrCreate(group).Error; err != nil {
			return nil, err
		}
	} else {
		group = &matches[0]
	}
	if err := r.DB.Model(connection).UpdateColumn("group_id", group.ID).Error; err != nil {
		return nil, err
	}
	connection.GroupID = group.ID
	if len(matches) > 1 {
		absorbed := make([]uint, 0, len(matches)-1)
		for _, other := range matches[1:] {
			absorbed = append(absorbed, other.ID)
		}
		if err := r.DB.Model(&model.GostTunnel{}).Where("group_id IN ?", absorbed).UpdateColumn("group_id", group.ID).Error; err != nil {
			return nil, err
		}
		if err := r.DB.Delete(&model.TunnelGroup{}, absorbed).Error; err != nil {
			return nil, err
		}
	}
	return group, nil
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

func (r *TunnelRepository) MergeGroup(source *model.TunnelGroup, connection *model.GostTunnel, updateAllowlist bool, matches []model.TunnelGroup) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		repo := NewTunnelRepository(tx)
		if err := repo.Update(connection, updateAllowlist); err != nil {
			return err
		}
		if _, err := repo.assignGroup(connection, matches); err != nil {
			return err
		}
		return tx.Delete(source).Error
	})
}

func (r *TunnelRepository) DeleteGroup(group *model.TunnelGroup) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("group_id = ?", group.ID).Delete(&model.GostTunnel{}).Error; err != nil {
			return err
		}
		return tx.Delete(group).Error
	})
}
