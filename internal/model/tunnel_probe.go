package model

type TunnelProbeStatus string

const (
	TunnelProbeSuccessful  TunnelProbeStatus = "successful"
	TunnelProbeFailed      TunnelProbeStatus = "failed"
	TunnelProbeUnavailable TunnelProbeStatus = "unavailable"
)

type TunnelProbeSample struct {
	ID        uint              `gorm:"primaryKey"`
	TunnelID  uint              `gorm:"not null;index:idx_tunnel_probe_history"`
	SampledAt int64             `gorm:"not null;index:idx_tunnel_probe_history;index"`
	Status    TunnelProbeStatus `gorm:"size:16;not null"`
	LatencyMS *float64
	TTL       *uint32
}
