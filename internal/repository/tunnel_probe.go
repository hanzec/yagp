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

func (r *TunnelRepository) WalkProbeHistory(ctx context.Context, tunnelIDs []uint, from, to int64, visit func(model.TunnelProbeSample)) error {
	rows, err := r.DB.WithContext(ctx).Model(&model.TunnelProbeSample{}).
		Select("sampled_at, status, latency_ms, ttl").
		Where("tunnel_id IN ? AND sampled_at BETWEEN ? AND ?", tunnelIDs, from, to).
		Order("sampled_at, id").Rows()
	if err != nil {
		return err
	}
	defer rows.Close()

	var sample model.TunnelProbeSample
	for rows.Next() {
		if err := rows.Scan(&sample.SampledAt, &sample.Status, &sample.LatencyMS, &sample.TTL); err != nil {
			return err
		}
		visit(sample)
	}
	return rows.Err()
}

func (r *TunnelRepository) PruneProbeHistory(ctx context.Context, before int64) error {
	return r.DB.WithContext(ctx).Where("sampled_at < ?", before).Delete(&model.TunnelProbeSample{}).Error
}
