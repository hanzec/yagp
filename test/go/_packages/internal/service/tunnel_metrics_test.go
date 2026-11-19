package service

import (
	"testing"
	"time"

	"yagp/internal/errors"
	"yagp/internal/model"
	"yagp/internal/repository"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestTunnelMetricsStreamsOrderedSamplesWithoutLoadingRelations(t *testing.T) {
	db := newServiceTestDB(t)
	requireServiceError(t, db.AutoMigrate(&model.TunnelProbeSample{}), nil)
	entry, exit := model.GostNode{Name: "entry"}, model.GostNode{Name: "exit"}
	saveServiceFixtures(t, db, &entry, &exit)
	tunnel := model.GostTunnel{Name: "monitored", EntryNodeID: entry.ID, ExitNodeID: exit.ID}
	saveServiceFixtures(t, db, &tunnel)
	requireServiceError(t, repository.MigrateTunnelGroups(db), nil)

	now := time.Now().Unix()
	var samples []model.TunnelProbeSample
	for i := 1; i <= 20; i++ {
		samples = append(samples, model.TunnelProbeSample{
			TunnelID: tunnel.ID, SampledAt: now - 600, Status: model.TunnelProbeSuccessful,
			LatencyMS: new(float64(i)), TTL: new(uint32(i)),
		})
	}
	samples = append(samples, model.TunnelProbeSample{
		TunnelID: tunnel.ID, SampledAt: now - 60, Status: model.TunnelProbeFailed, TTL: new(uint32(55)),
	}, model.TunnelProbeSample{
		TunnelID: tunnel.ID, SampledAt: now - 60, Status: model.TunnelProbeUnavailable,
	})
	saveServiceFixtures(t, db, &samples)

	queries := &queryCounter{Interface: gormlogger.Default.LogMode(gormlogger.Silent)}
	svc := NewTunnelService(db.Session(&gorm.Session{Logger: queries}))
	metrics, err := svc.Metrics(t.Context(), tunnel.ID, "1h")
	requireServiceError(t, err, nil)
	if queries.count != 2 {
		t.Fatalf("metrics executed %d queries, want only tunnel existence and probe history", queries.count)
	}
	summary := metrics.Summary
	if summary.Attempts != 22 || summary.Successful != 20 || summary.Failed != 1 || summary.Unavailable != 1 ||
		summary.P95LatencyMS == nil || *summary.P95LatencyMS != 19 ||
		summary.LatestLatencyMS == nil || *summary.LatestLatencyMS != 20 ||
		summary.LatestStatus == nil || *summary.LatestStatus != model.TunnelProbeUnavailable ||
		summary.LatestTTL == nil || *summary.LatestTTL != 55 {
		t.Fatalf("unexpected aggregate: %+v", summary)
	}
	point := metrics.Points[(samples[0].SampledAt-metrics.From.Unix())/metrics.BucketSeconds]
	if point.TTL == nil || *point.TTL != 20 || point.MinLatencyMS == nil || *point.MinLatencyMS != 1 ||
		point.AvgLatencyMS == nil || *point.AvgLatencyMS != 10.5 || point.MaxLatencyMS == nil || *point.MaxLatencyMS != 20 {
		t.Fatalf("earlier bucket values after reading later samples: %+v", point)
	}

	requireServiceError(t, db.Delete(&tunnel).Error, nil)
	_, err = svc.Metrics(t.Context(), tunnel.ID, "1h")
	requireServiceError(t, err, errors.ErrTunnelNotFound)
}
