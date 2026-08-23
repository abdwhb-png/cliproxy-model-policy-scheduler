package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	strategyFillFirst                  = "fill-first"
	strategyRoundRobin                 = "round-robin"
	strategyProviderWeightedRoundRobin = "provider-weighted-round-robin"
	maxProviderWeight                  = 1_000_000
)

type ruleConfig struct {
	Strategy        string           `yaml:"strategy"`
	ProviderWeights map[string]int64 `yaml:"provider-weights"`
}

type pluginConfig struct {
	Rules map[string]ruleConfig `yaml:"rules"`
}

type schedulerPlugin struct {
	mu                sync.Mutex
	config            pluginConfig
	providerCurrent   map[string]map[string]int64
	credentialCursors map[string]map[string]int
}

func newSchedulerPlugin() *schedulerPlugin {
	return &schedulerPlugin{
		providerCurrent:   make(map[string]map[string]int64),
		credentialCursors: make(map[string]map[string]int),
	}
}

func (p *schedulerPlugin) Reconfigure(raw []byte) error {
	var decoded pluginConfig
	if err := yaml.Unmarshal(raw, &decoded); err != nil {
		return err
	}

	config := pluginConfig{Rules: make(map[string]ruleConfig, len(decoded.Rules))}
	for model, rule := range decoded.Rules {
		normalizedModel := strings.TrimSpace(model)
		if normalizedModel == "" {
			return fmt.Errorf("model rule ID is required")
		}
		if _, duplicate := config.Rules[normalizedModel]; duplicate {
			return fmt.Errorf("duplicate model rule %q", normalizedModel)
		}

		rule.Strategy = strings.ToLower(strings.TrimSpace(rule.Strategy))
		switch rule.Strategy {
		case strategyFillFirst, strategyRoundRobin, strategyProviderWeightedRoundRobin:
		default:
			return fmt.Errorf("model %q has unsupported strategy %q", normalizedModel, rule.Strategy)
		}

		normalizedWeights := make(map[string]int64, len(rule.ProviderWeights))
		for provider, weight := range rule.ProviderWeights {
			normalizedProvider := strings.ToLower(strings.TrimSpace(provider))
			if normalizedProvider == "" {
				return fmt.Errorf("model %q has an empty provider weight key", normalizedModel)
			}
			if weight < 0 {
				return fmt.Errorf("model %q provider %q has negative weight", normalizedModel, normalizedProvider)
			}
			if weight > maxProviderWeight {
				return fmt.Errorf("model %q provider %q weight exceeds %d", normalizedModel, normalizedProvider, maxProviderWeight)
			}
			if _, duplicate := normalizedWeights[normalizedProvider]; duplicate {
				return fmt.Errorf("model %q has duplicate provider weight %q", normalizedModel, normalizedProvider)
			}
			normalizedWeights[normalizedProvider] = weight
		}
		rule.ProviderWeights = normalizedWeights
		config.Rules[normalizedModel] = rule
	}

	p.mu.Lock()
	p.config = config
	p.providerCurrent = make(map[string]map[string]int64)
	p.credentialCursors = make(map[string]map[string]int)
	p.mu.Unlock()
	return nil
}

func (p *schedulerPlugin) Pick(request pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	model := strings.TrimSpace(request.Model)

	p.mu.Lock()
	defer p.mu.Unlock()

	rule, matched := p.config.Rules[model]
	if !matched {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}

	switch rule.Strategy {
	case strategyFillFirst, strategyRoundRobin:
		return pluginapi.SchedulerPickResponse{
			DelegateBuiltin: rule.Strategy,
			Handled:         true,
		}
	case strategyProviderWeightedRoundRobin:
		return p.pickProviderWeightedLocked(model, rule, request.Candidates)
	default:
		return pluginapi.SchedulerPickResponse{Handled: false}
	}
}

func (p *schedulerPlugin) pickProviderWeightedLocked(model string, rule ruleConfig, candidates []pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickResponse {
	if len(candidates) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}

	bestPriority := 0
	hasEligible := false
	for _, candidate := range candidates {
		provider := strings.ToLower(strings.TrimSpace(candidate.Provider))
		if provider == "" || strings.TrimSpace(candidate.ID) == "" || providerWeight(rule, provider) <= 0 {
			continue
		}
		if !hasEligible || candidate.Priority > bestPriority {
			bestPriority = candidate.Priority
			hasEligible = true
		}
	}
	if !hasEligible {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}

	byProvider := make(map[string][]pluginapi.SchedulerAuthCandidate)
	for _, candidate := range candidates {
		provider := strings.ToLower(strings.TrimSpace(candidate.Provider))
		if candidate.Priority != bestPriority || provider == "" || strings.TrimSpace(candidate.ID) == "" || providerWeight(rule, provider) <= 0 {
			continue
		}
		byProvider[provider] = append(byProvider[provider], candidate)
	}
	if len(byProvider) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}

	providers := make([]string, 0, len(byProvider))
	for provider := range byProvider {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	for _, provider := range providers {
		sort.Slice(byProvider[provider], func(i, j int) bool {
			return byProvider[provider][i].ID < byProvider[provider][j].ID
		})
	}

	current := p.providerCurrent[model]
	if current == nil {
		current = make(map[string]int64)
		p.providerCurrent[model] = current
	}
	for provider := range current {
		if _, active := byProvider[provider]; !active {
			delete(current, provider)
		}
	}

	selectedProvider := ""
	var selectedCurrent int64
	var totalWeight int64
	for _, provider := range providers {
		weight := providerWeight(rule, provider)
		current[provider] += weight
		totalWeight += weight
		if selectedProvider == "" || current[provider] > selectedCurrent {
			selectedProvider = provider
			selectedCurrent = current[provider]
		}
	}
	current[selectedProvider] -= totalWeight

	providerCursors := p.credentialCursors[model]
	if providerCursors == nil {
		providerCursors = make(map[string]int)
		p.credentialCursors[model] = providerCursors
	}
	providerCandidates := byProvider[selectedProvider]
	cursor := providerCursors[selectedProvider] % len(providerCandidates)
	selected := providerCandidates[cursor]
	providerCursors[selectedProvider] = cursor + 1

	return pluginapi.SchedulerPickResponse{AuthID: selected.ID, Handled: true}
}

func providerWeight(rule ruleConfig, provider string) int64 {
	if weight, exists := rule.ProviderWeights[provider]; exists {
		return weight
	}
	return 1
}
