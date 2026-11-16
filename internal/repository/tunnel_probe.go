package repository

import (
	"context"

	"yagp/internal/model"
)

func (r *TunnelRepository) RunningTunnelsForProbe(ctx context.Context) ([]model.GostTunnel, error) {
	var tunnels []model.GostTunnel
	err := r.DB.WithContext(ctx).Where("status = ?", model.TunnelStatusRunning).
		Preload("EntryNode").Preload("ExitNode").Order("id").Find(&tunnels).Error
	return tunnels, err
}

func (r *TunnelRepository) SaveProbeSamples(ctx context.Context, samples []model.TunnelProbeSample) error {
	return r.DB.WithContext(ctx).Create(&samples).Error
}

func (r *TunnelRepository) ProbeHistory(ctx context.Context, tunnelID uint, from, to int64) ([]model.TunnelProbeSample, error) {
	var samples []model.TunnelProbeSample
	err := r.DB.WithContext(ctx).Where("tunnel_id = ? AND sampled_at BETWEEN ? AND ?", tunnelID, from, to).
		Order("sampled_at, id").Find(&samples).Error
	return samples, err
}

func (r *TunnelRepository) PruneProbeHistory(ctx context.Context, before int64) error {
	return r.DB.WithContext(ctx).Where("sampled_at < ?", before).Delete(&model.TunnelProbeSample{}).Error
}
