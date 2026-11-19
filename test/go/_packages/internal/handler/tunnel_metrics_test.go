package handler

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"yagp/internal/model"
	"yagp/internal/service"
)

func TestTunnelMetricsHTTPUsesSelectedRange(t *testing.T) {
	f := newHandlerHTTPFixture(t)
	f.registerForwarding()
	if err := f.db.AutoMigrate(&model.TunnelProbeSample{}); err != nil {
		t.Fatal(err)
	}
	nodes := []model.GostNode{{Name: "entry"}, {Name: "exit"}, {Name: "other"}}
	if err := f.db.Create(&nodes).Error; err != nil {
		t.Fatal(err)
	}
	tunnel := model.GostTunnel{Name: "monitored tunnel", EntryNodeID: nodes[0].ID, ExitNodeID: nodes[1].ID}
	other := model.GostTunnel{Name: "other monitored tunnel", EntryNodeID: nodes[0].ID, ExitNodeID: nodes[2].ID}
	empty := model.GostTunnel{Name: "no samples", EntryNodeID: nodes[1].ID, ExitNodeID: nodes[0].ID}
	for _, row := range []*model.GostTunnel{&tunnel, &other, &empty} {
		if err := f.db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	f.migrateTunnelGroups(t)

	now := time.Now().Truncate(time.Second)
	lastSample := now.Add(-time.Minute)
	zero, fast, older, unrelated := 0.0, 32.0, 90.0, 1000.0
	ttl, lastTTL := uint32(53), uint32(51)
	samples := []model.TunnelProbeSample{
		{TunnelID: tunnel.ID, SampledAt: now.Add(-90 * time.Minute).Unix(), Status: model.TunnelProbeSuccessful, LatencyMS: &older},
		{TunnelID: tunnel.ID, SampledAt: now.Add(-4 * time.Minute).Unix(), Status: model.TunnelProbeSuccessful, LatencyMS: &zero, TTL: &ttl},
		{TunnelID: tunnel.ID, SampledAt: now.Add(-3 * time.Minute).Unix(), Status: model.TunnelProbeSuccessful, LatencyMS: &fast},
		{TunnelID: tunnel.ID, SampledAt: now.Add(-2 * time.Minute).Unix(), Status: model.TunnelProbeFailed},
		{TunnelID: tunnel.ID, SampledAt: lastSample.Unix(), Status: model.TunnelProbeUnavailable, TTL: &lastTTL},
		{TunnelID: other.ID, SampledAt: lastSample.Unix(), Status: model.TunnelProbeSuccessful, LatencyMS: &unrelated},
	}
	if err := f.db.Create(&samples).Error; err != nil {
		t.Fatal(err)
	}

	path := fmt.Sprintf("/tunnels/%d/metrics", tunnel.ID)
	reply := f.request(t, http.MethodGet, path+"?range=1h", "", http.StatusOK)
	metrics := handlerData[service.TunnelMetrics](t, reply)
	summary := metrics.Summary
	if metrics.To.Sub(metrics.From) != time.Hour || metrics.IntervalSeconds != 60 || metrics.BucketSeconds != 60 || len(metrics.Points) != 60 {
		t.Fatalf("unexpected hourly range: %s to %s, %d buckets of %d seconds", metrics.From, metrics.To, len(metrics.Points), metrics.BucketSeconds)
	}
	if summary.Attempts != 4 || summary.Successful != 2 || summary.Failed != 1 || summary.Unavailable != 1 ||
		summary.LatestProbeAt == nil || !summary.LatestProbeAt.Equal(lastSample) ||
		summary.LatestStatus == nil || *summary.LatestStatus != model.TunnelProbeUnavailable ||
		summary.LatestTTL == nil || *summary.LatestTTL != lastTTL {
		t.Fatalf("unexpected hourly summary: %+v", summary)
	}
	for _, metric := range []struct {
		name string
		got  *float64
		want float64
	}{
		{"min", summary.MinLatencyMS, zero},
		{"avg", summary.AvgLatencyMS, 16},
		{"max", summary.MaxLatencyMS, fast},
		{"latest", summary.LatestLatencyMS, fast},
		{"p95", summary.P95LatencyMS, fast},
	} {
		if metric.got == nil || *metric.got != metric.want {
			t.Errorf("%s latency = %v, want %.2f", metric.name, metric.got, metric.want)
		}
	}
	start := metrics.Points[0]
	if !start.Timestamp.Equal(metrics.From) || start.Attempts != 0 || start.AvgLatencyMS != nil || start.TTL != nil {
		t.Errorf("unexpected empty bucket: %+v", start)
	}
	index := int(lastSample.Sub(metrics.From) / time.Minute)
	point := metrics.Points[index]
	if point.Unavailable != 1 || point.AvgLatencyMS != nil || point.TTL == nil || *point.TTL != lastTTL {
		t.Errorf("restricted sample bucket = %+v", point)
	}

	reply = f.request(t, http.MethodGet, path, "", http.StatusOK)
	daily := handlerData[service.TunnelMetrics](t, reply)
	if daily.To.Sub(daily.From) != 24*time.Hour || daily.BucketSeconds != 300 || len(daily.Points) != 288 ||
		daily.Summary.Attempts != 5 || daily.Summary.P95LatencyMS == nil || *daily.Summary.P95LatencyMS != older {
		t.Fatalf("unexpected daily range or summary: %+v", daily.Summary)
	}

	reply = f.request(t, http.MethodGet, fmt.Sprintf("/tunnels/%d/metrics?range=30d", empty.ID), "", http.StatusOK)
	monthly := handlerData[service.TunnelMetrics](t, reply)
	if monthly.To.Sub(monthly.From) != 30*24*time.Hour || len(monthly.Points) != 360 || monthly.Summary.Attempts != 0 ||
		monthly.Summary.LatestProbeAt != nil || monthly.Summary.P95LatencyMS != nil {
		t.Fatalf("unexpected empty monthly range: %+v", monthly.Summary)
	}

	f.request(t, http.MethodGet, path+"?range=90d", "", http.StatusBadRequest)
	f.request(t, http.MethodGet, "/tunnels/0/metrics", "", http.StatusBadRequest)
	f.request(t, http.MethodGet, "/tunnels/999/metrics", "", http.StatusNotFound)
}
