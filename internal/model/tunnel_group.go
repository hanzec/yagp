package model

import "time"

// TunnelGroup is the managed tunnel for one directed pair of physical nodes.
// GostTunnel rows retain their own runtime identities for listeners, chains and probe history.
type TunnelGroup struct {
	ID          uint   `gorm:"primaryKey;autoIncrement:false"`
	EntryNodeID uint   `gorm:"not null;uniqueIndex:idx_tunnel_node_pair"`
	ExitNodeID  uint   `gorm:"not null;uniqueIndex:idx_tunnel_node_pair"`
	Name        string `gorm:"size:100;not null"`
	Remark      string `gorm:"type:text"`
	CreatedAt   time.Time
	UpdatedAt   time.Time

	Status      TunnelStatus `gorm:"->;-:migration"`
	Connections []GostTunnel `gorm:"foreignKey:EntryNodeID,ExitNodeID;references:EntryNodeID,ExitNodeID;-:migration"`
}
