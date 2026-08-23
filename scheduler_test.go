package main

import (
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestSchedulerPick_UnmatchedModelFallsBackToHost(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  ox-alpha-free:
    strategy: provider-weighted-round-robin
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	response := plugin.Pick(pluginapi.SchedulerPickRequest{
		Model: "another-model",
		Candidates: []pluginapi.SchedulerAuthCandidate{{
			ID:       "auth-1",
			Provider: "provider-a",
			Status:   "available",
		}},
	})

	if response.Handled {
		t.Fatalf("Pick() Handled = true, want false for model without a rule")
	}
	if response.AuthID != "" || response.DelegateBuiltin != "" {
		t.Fatalf("Pick() response = %+v, want empty host fallback response", response)
	}
}

func TestSchedulerPick_DelegatesBuiltInStrategies(t *testing.T) {
	tests := []struct {
		name     string
		strategy string
		want     string
	}{
		{name: "fill first", strategy: strategyFillFirst, want: pluginapi.SchedulerBuiltinFillFirst},
		{name: "round robin", strategy: strategyRoundRobin, want: pluginapi.SchedulerBuiltinRoundRobin},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plugin := newSchedulerPlugin()
			config := []byte("rules:\n  target-model:\n    strategy: " + test.strategy + "\n")
			if err := plugin.Reconfigure(config); err != nil {
				t.Fatalf("Reconfigure() error = %v", err)
			}

			response := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "target-model"})
			if !response.Handled || response.DelegateBuiltin != test.want {
				t.Fatalf("Pick() response = %+v, want handled delegation %q", response, test.want)
			}
			if response.AuthID != "" {
				t.Fatalf("Pick() AuthID = %q, want empty for built-in delegation", response.AuthID)
			}
		})
	}
}

func TestSchedulerPick_ProviderRoundRobinIsIndependentOfCredentialCount(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  ox-alpha-free:
    strategy: provider-weighted-round-robin
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "openrouter-1", Provider: "openrouter", Status: "available"},
		{ID: "openrouter-2", Provider: "openrouter", Status: "available"},
		{ID: "go-1", Provider: "opencode-go", Status: "available"},
		{ID: "zen-1", Provider: "opencode-zen", Status: "available"},
	}
	providerByAuth := map[string]string{
		"openrouter-1": "openrouter",
		"openrouter-2": "openrouter",
		"go-1":         "opencode-go",
		"zen-1":        "opencode-zen",
	}
	providerCounts := map[string]int{}
	authCounts := map[string]int{}

	for range 12 {
		response := plugin.Pick(pluginapi.SchedulerPickRequest{
			Model:      "ox-alpha-free",
			Candidates: candidates,
		})
		provider, exists := providerByAuth[response.AuthID]
		if !response.Handled || !exists {
			t.Fatalf("Pick() response = %+v, want a handled candidate selection", response)
		}
		providerCounts[provider]++
		authCounts[response.AuthID]++
	}

	for _, provider := range []string{"openrouter", "opencode-go", "opencode-zen"} {
		if providerCounts[provider] != 4 {
			t.Fatalf("provider %q picks = %d, want 4; all counts = %#v", provider, providerCounts[provider], providerCounts)
		}
	}
	if authCounts["openrouter-1"] != 2 || authCounts["openrouter-2"] != 2 {
		t.Fatalf("OpenRouter credential picks = %#v, want 2 each", authCounts)
	}
}

func TestSchedulerPick_NormalizesConfiguredProviderWeights(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  weighted-model:
    strategy: provider-weighted-round-robin
    provider-weights:
      OpenRouter: 3
      OPENCODE-GO: 1
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "openrouter-1", Provider: "openrouter", Status: "active"},
		{ID: "go-1", Provider: "opencode-go", Status: "active"},
	}
	counts := map[string]int{}
	for range 4 {
		response := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "weighted-model", Candidates: candidates})
		counts[response.AuthID]++
	}

	if counts["openrouter-1"] != 3 || counts["go-1"] != 1 {
		t.Fatalf("weighted picks = %#v, want OpenRouter 3 and Go 1", counts)
	}
}

func TestSchedulerPick_UsesHighestPriorityEligibleCandidate(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  priority-model:
    strategy: provider-weighted-round-robin
    provider-weights:
      disabled-provider: 0
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	response := plugin.Pick(pluginapi.SchedulerPickRequest{
		Model: "priority-model",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "disabled-high", Provider: "disabled-provider", Priority: 10, Status: "active"},
			{ID: "ready-low", Provider: "ready-provider", Priority: 5, Status: "active"},
		},
	})

	if !response.Handled || response.AuthID != "ready-low" {
		t.Fatalf("Pick() response = %+v, want eligible lower-priority credential", response)
	}
}

func TestSchedulerReconfigure_RejectsInvalidRules(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{
			name: "unknown strategy",
			config: `rules:
  model-a:
    strategy: random
`,
		},
		{
			name: "empty model",
			config: `rules:
  "  ":
    strategy: round-robin
`,
		},
		{
			name: "negative provider weight",
			config: `rules:
  model-a:
    strategy: provider-weighted-round-robin
    provider-weights:
      provider-a: -1
`,
		},
		{
			name: "provider weight above limit",
			config: `rules:
  model-a:
    strategy: provider-weighted-round-robin
    provider-weights:
      provider-a: 1000001
`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plugin := newSchedulerPlugin()
			if err := plugin.Reconfigure([]byte(test.config)); err == nil {
				t.Fatalf("Reconfigure() error = nil, want invalid rule rejection")
			}
		})
	}
}

func TestSchedulerPick_ModelCursorsAreIndependent(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  model-a:
    strategy: provider-weighted-round-robin
  model-b:
    strategy: provider-weighted-round-robin
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "alpha-1", Provider: "alpha"},
		{ID: "beta-1", Provider: "beta"},
	}

	firstA := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "model-a", Candidates: candidates})
	firstB := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "model-b", Candidates: candidates})
	if firstA.AuthID != "alpha-1" || firstB.AuthID != "alpha-1" {
		t.Fatalf("first model picks = %q and %q, want independent alpha-1 starts", firstA.AuthID, firstB.AuthID)
	}
}

func TestSchedulerPick_ConcurrentCallsReturnOnlyCandidates(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  concurrent-model:
    strategy: provider-weighted-round-robin
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "alpha-1", Provider: "alpha"},
		{ID: "beta-1", Provider: "beta"},
		{ID: "gamma-1", Provider: "gamma"},
	}

	const calls = 300
	responses := make(chan pluginapi.SchedulerPickResponse, calls)
	var wait sync.WaitGroup
	for range calls {
		wait.Add(1)
		go func() {
			defer wait.Done()
			responses <- plugin.Pick(pluginapi.SchedulerPickRequest{Model: "concurrent-model", Candidates: candidates})
		}()
	}
	wait.Wait()
	close(responses)

	counts := map[string]int{}
	for response := range responses {
		if !response.Handled {
			t.Fatalf("concurrent Pick() response = %+v, want handled", response)
		}
		counts[response.AuthID]++
	}
	for _, authID := range []string{"alpha-1", "beta-1", "gamma-1"} {
		if counts[authID] != calls/3 {
			t.Fatalf("concurrent picks = %#v, want %d for %s", counts, calls/3, authID)
		}
	}
}

func TestSchedulerReconfigure_PreservesValidConfigAfterRejectedUpdate(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  target-model:
    strategy: round-robin
`)); err != nil {
		t.Fatalf("initial Reconfigure() error = %v", err)
	}
	if err := plugin.Reconfigure([]byte(`
rules:
  target-model:
    strategy: unsupported
`)); err == nil {
		t.Fatal("invalid Reconfigure() error = nil, want rejection")
	}

	response := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "target-model"})
	if !response.Handled || response.DelegateBuiltin != pluginapi.SchedulerBuiltinRoundRobin {
		t.Fatalf("Pick() response after rejected config = %+v, want prior round-robin rule", response)
	}
}
