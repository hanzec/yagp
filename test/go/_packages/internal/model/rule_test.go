package model

import (
	"encoding/json"
	"testing"
)

func TestRuleExposesAppliedEndpointWithoutChangingStoredSnapshotFormat(t *testing.T) {
	// Snapshots already stored in the database use the original Go field names.
	var applied EndpointConfig
	if err := json.Unmarshal([]byte(`{"Protocol":"tcp","ListenIP":"192.0.2.1","ListenPort":8443,"Targets":["old.test:443"],"UseOriginalTarget":false}`), &applied); err != nil {
		t.Fatal(err)
	}
	rule := GostRule{Protocol: "tls", ListenPort: 9443, Targets: []string{"new.test:443"}, AppliedEndpoint: &applied}
	live := rule.WithAppliedEndpoint()
	if live.Protocol != "tcp" || live.ListenPort != 8443 || live.Targets[0] != "old.test:443" {
		t.Fatalf("stored snapshot was not restored: %+v", live)
	}
	encoded, err := json.Marshal(rule)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(response["applied_endpoint"], &snapshot); err != nil {
		t.Fatal(err)
	}
	if string(response["protocol"]) != `"tls"` || string(snapshot["Protocol"]) != `"tcp"` || string(snapshot["ListenPort"]) != "8443" {
		t.Fatalf("response did not distinguish saved and applied settings: %s", encoded)
	}
	rule.AppliedEndpoint = nil
	encoded, err = json.Marshal(rule)
	if err != nil {
		t.Fatal(err)
	}
	response = nil
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if _, present := response["applied_endpoint"]; present {
		t.Fatalf("an endpoint without pending changes exposed a snapshot: %s", encoded)
	}
}
