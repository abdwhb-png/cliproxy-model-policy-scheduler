package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPluginABI_ReconfigureThenPickCandidate(t *testing.T) {
	configRequest, err := json.Marshal(struct {
		ConfigYAML []byte `json:"config_yaml"`
	}{ConfigYAML: []byte(`
rules:
  ox-alpha-free:
    strategy: provider-weighted-round-robin
`)})
	if err != nil {
		t.Fatalf("marshal config request: %v", err)
	}
	configureRaw, err := handleMethod(pluginabi.MethodPluginReconfigure, configRequest)
	if err != nil {
		t.Fatalf("handle reconfigure: %v", err)
	}
	var configureEnvelope rpcEnvelope
	if err := json.Unmarshal(configureRaw, &configureEnvelope); err != nil {
		t.Fatalf("unmarshal reconfigure response: %v", err)
	}
	if !configureEnvelope.OK {
		t.Fatalf("reconfigure response = %s, want ok envelope", configureRaw)
	}

	pickRequest, err := json.Marshal(pluginapi.SchedulerPickRequest{
		Model: "ox-alpha-free",
		Candidates: []pluginapi.SchedulerAuthCandidate{{
			ID:       "auth-1",
			Provider: "provider-a",
			Status:   "active",
		}},
	})
	if err != nil {
		t.Fatalf("marshal scheduler request: %v", err)
	}
	pickRaw, err := handleMethod(pluginabi.MethodSchedulerPick, pickRequest)
	if err != nil {
		t.Fatalf("handle scheduler pick: %v", err)
	}
	var pickEnvelope rpcEnvelope
	if err := json.Unmarshal(pickRaw, &pickEnvelope); err != nil {
		t.Fatalf("unmarshal scheduler response: %v", err)
	}
	var response pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(pickEnvelope.Result, &response); err != nil {
		t.Fatalf("unmarshal scheduler result: %v", err)
	}
	if !pickEnvelope.OK || !response.Handled || response.AuthID != "auth-1" {
		t.Fatalf("scheduler response = %s, want auth-1 handled", pickRaw)
	}
}
