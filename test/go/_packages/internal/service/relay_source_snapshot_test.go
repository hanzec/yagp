package service

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"yagp/internal/model"
	"yagp/internal/utils"
	"yagp/pkg/gost"
)

func TestLiveRelaySourceUpdatesPreserveMissingSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name         string
		reported     []string
		unrestricted bool
		want         []string
		whitelist    bool
	}{
		{name: "all snapshots missing", want: []string{"192.0.2.1"}, whitelist: true},
		{name: "another member reports", reported: []string{"192.0.2.2"}, want: []string{"192.0.2.1", "192.0.2.2"}, whitelist: true},
		{name: "invalid report cannot open access", reported: []string{"invalid", "10.0.0.1", "::1", "100.64.0.1"}, want: []string{"192.0.2.1"}, whitelist: true},
		{name: "explicit any remains unrestricted", unrestricted: true, whitelist: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newServiceTestDB(t)
			remote := &lifecycleNode{t: t}
			exit := startServiceTestNode(t, remote)
			missing := model.GostNode{Name: "missing"}
			known := model.GostNode{Name: "known", Addresses: tc.reported}
			saveServiceFixtures(t, db, &exit, &missing, &known)
			relay := gost.ServiceConfig{Name: "relay-tunnel-1", Admission: gost.TunnelRelayAllowlistName("relay-tunnel-1")}
			client := utils.GetGostClient(&exit)
			_, err := client.SetAdmission(gost.AdmissionConfig{Name: relay.Admission, Whitelist: true, Matchers: []string{"192.0.2.1", "invalid", "10.0.0.2"}})
			requireServiceError(t, err, nil)
			members := []*model.GostTunnel{{EntryNodeID: missing.ID, AutoAllowlist: true}, {EntryNodeID: known.ID, AutoAllowlist: !tc.unrestricted}}
			svc := NewGostAllowlistService(db)
			requireServiceError(t, svc.writeRelaySources(client, &relay, members), nil)
			got := remote.admission(relay.Admission)
			if got.Whitelist != tc.whitelist || !slices.Equal(got.Matchers, tc.want) {
				t.Fatalf("updated admission = %+v, want whitelist %t, %v", got, tc.whitelist, tc.want)
			}
			if tc.unrestricted {
				return
			}
			// Removing a member while the survivor is missing retains the live snapshot.
			requireServiceError(t, svc.writeRelaySources(client, &relay, members[:1]), nil)
			if got := remote.admission(relay.Admission); !got.Whitelist || !slices.Equal(got.Matchers, tc.want) {
				t.Fatalf("removal lost the survivor's snapshot: %+v", got)
			}
			requireServiceError(t, db.Model(&missing).Update("ipv4", "192.0.2.3").Error, nil)
			requireServiceError(t, svc.writeRelaySources(client, &relay, members[:1]), nil)
			if got := remote.admission(relay.Admission); !got.Whitelist || !slices.Equal(got.Matchers, []string{"192.0.2.3"}) {
				t.Fatalf("complete report did not replace old matchers: %+v", got)
			}
		})
	}
}

func TestLiveRelaySourceReadFailuresDoNotOverwriteAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    json.RawMessage
		reject int
	}{
		{name: "missing resource"},
		{name: "read failure", raw: json.RawMessage(`{"name":"yagp-relay-tunnel-1-allowlist","whitelist":true,"matchers":["192.0.2.1"]}`), reject: http.StatusServiceUnavailable},
		{name: "malformed policy", raw: json.RawMessage(`{"name":"yagp-relay-tunnel-1-allowlist","whitelist":"invalid"}`)},
		{name: "wrong resource", raw: json.RawMessage(`{"name":"other","whitelist":false}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newServiceTestDB(t)
			remote := &lifecycleNode{t: t, resources: map[string]json.RawMessage{}, reject: map[string]int{}}
			exit := startServiceTestNode(t, remote)
			source := model.GostNode{Name: "missing"}
			saveServiceFixtures(t, db, &exit, &source)
			relay := gost.ServiceConfig{Name: "relay-tunnel-1", Admission: gost.TunnelRelayAllowlistName("relay-tunnel-1")}
			path := "/config/admissions/" + relay.Admission
			remote.resources[path] = tc.raw
			remote.reject["GET "+path] = tc.reject
			err := NewGostAllowlistService(db).writeRelaySources(utils.GetGostClient(&exit), &relay, []*model.GostTunnel{{EntryNodeID: source.ID, AutoAllowlist: true}})
			if err == nil {
				t.Fatal("invalid live policy was accepted")
			}
			remote.mu.Lock()
			defer remote.mu.Unlock()
			if string(remote.resources[path]) != string(tc.raw) {
				t.Fatal("failed read overwrote admission")
			}
		})
	}
}
