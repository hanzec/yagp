package service

import (
	"context"
	"slices"
	"time"

	"yagp/internal/errors"
	"yagp/internal/model"
)

const (
	tunnelProbeInterval  = time.Minute
	tunnelProbeRetention = 30 * 24 * time.Hour
)

var tunnelMetricRanges = map[string]struct {
	duration time.Duration
	bucket   time.Duration
}{
	"1h":  {time.Hour, time.Minute},
	"6h":  {6 * time.Hour, time.Minute},
	"24h": {24 * time.Hour, 5 * time.Minute},
	"7d":  {7 * 24 * time.Hour, 30 * time.Minute},
	"30d": {tunnelProbeRetention, 2 * time.Hour},
}

type TunnelMetrics struct {
	From            time.Time           `json:"from"`
	To              time.Time           `json:"to"`
	IntervalSeconds int64               `json:"interval_seconds"`
	BucketSeconds   int64               `json:"bucket_seconds"`
	Summary         TunnelMetricSummary `json:"summary"`
	Points          []TunnelMetricPoint `json:"points"`
}

type TunnelMetricStats struct {
	Attempts     int      `json:"attempts"`
	Successful   int      `json:"successful"`
	Failed       int      `json:"failed"`
	Unavailable  int      `json:"unavailable"`
	AvgLatencyMS *float64 `json:"avg_latency_ms"`
	MinLatencyMS *float64 `json:"min_latency_ms"`
	MaxLatencyMS *float64 `json:"max_latency_ms"`

	latencySum   float64
	latencyCount int
}

type TunnelMetricSummary struct {
	TunnelMetricStats
	P95LatencyMS    *float64                 `json:"p95_latency_ms"`
	LatestLatencyMS *float64                 `json:"latest_latency_ms"`
	LatestTTL       *uint32                  `json:"latest_ttl"`
	LatestProbeAt   *time.Time               `json:"latest_probe_at"`
	LatestStatus    *model.TunnelProbeStatus `json:"latest_status"`
}

type TunnelMetricPoint struct {
	TunnelMetricStats
	Timestamp time.Time `json:"timestamp"`
	TTL       *uint32   `json:"ttl"`
}

func (s *TunnelService) Metrics(ctx context.Context, id uint, span string) (*TunnelMetrics, error) {
	if span == "" {
		span = "24h"
	}
	window, ok := tunnelMetricRanges[span]
	if !ok {
		return nil, errors.ErrTunnelMetricRange
	}
	connectionIDs, err := s.tunnelRepo.GroupConnectionIDs(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(connectionIDs) == 0 {
		return nil, errors.ErrTunnelNotFound
	}

	to := time.Now().UTC().Truncate(time.Second)
	from := to.Add(-window.duration)
	return s.readTunnelMetrics(ctx, connectionIDs, from, to, window.bucket)
}

func (s *TunnelService) readTunnelMetrics(ctx context.Context, connectionIDs []uint, from, to time.Time, bucket time.Duration) (*TunnelMetrics, error) {
	metrics := &TunnelMetrics{
		From:            from,
		To:              to,
		IntervalSeconds: int64(tunnelProbeInterval / time.Second),
		BucketSeconds:   int64(bucket / time.Second),
		Points:          make([]TunnelMetricPoint, to.Sub(from)/bucket),
	}
	for i := range metrics.Points {
		metrics.Points[i].Timestamp = from.Add(time.Duration(i) * bucket)
	}

	var latencies []float64
	var latest model.TunnelProbeSample
	summary := &metrics.Summary
	err := s.tunnelRepo.WalkProbeHistory(ctx, connectionIDs, from.Unix(), to.Unix(), func(sample model.TunnelProbeSample) {
		latest = sample
		sampledAt := time.Unix(sample.SampledAt, 0).UTC()
		// The right edge is inclusive, so a sample taken exactly at `to` belongs to the final bucket.
		index := min(int(sampledAt.Sub(from)/bucket), len(metrics.Points)-1)
		point := &metrics.Points[index]
		point.record(sample)
		summary.record(sample)
		if sample.TTL != nil {
			point.TTL = sample.TTL
			summary.LatestTTL = sample.TTL
		}
		if sample.Status == model.TunnelProbeSuccessful && sample.LatencyMS != nil {
			latencies = append(latencies, *sample.LatencyMS)
			summary.LatestLatencyMS = sample.LatencyMS
		}
	})
	if err != nil {
		return nil, err
	}
	if summary.Attempts > 0 {
		sampledAt := time.Unix(latest.SampledAt, 0).UTC()
		summary.LatestProbeAt = &sampledAt
		summary.LatestStatus = &latest.Status
	}
	for i := range metrics.Points {
		metrics.Points[i].finish()
	}
	summary.finish()
	if len(latencies) != 0 {
		slices.Sort(latencies)
		index := (95*len(latencies)+99)/100 - 1
		summary.P95LatencyMS = new(latencies[index])
	}
	return metrics, nil
}

func (s *TunnelMetricStats) record(sample model.TunnelProbeSample) {
	s.Attempts++
	switch sample.Status {
	case model.TunnelProbeSuccessful:
		s.Successful++
	case model.TunnelProbeFailed:
		s.Failed++
	case model.TunnelProbeUnavailable:
		s.Unavailable++
	}
	if sample.Status != model.TunnelProbeSuccessful || sample.LatencyMS == nil {
		return
	}
	value := *sample.LatencyMS
	s.latencySum += value
	s.latencyCount++
	if s.MinLatencyMS == nil || value < *s.MinLatencyMS {
		s.MinLatencyMS = sample.LatencyMS
	}
	if s.MaxLatencyMS == nil || value > *s.MaxLatencyMS {
		s.MaxLatencyMS = sample.LatencyMS
	}
}

func (s *TunnelMetricStats) finish() {
	if s.latencyCount != 0 {
		average := s.latencySum / float64(s.latencyCount)
		s.AvgLatencyMS = &average
	}
}
