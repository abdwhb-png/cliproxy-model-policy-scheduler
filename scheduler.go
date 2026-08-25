package main

import (
	"fmt"
	"net/url"
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
	providerGroupByProvider            = "provider"
	providerGroupByBaseURL             = "base-url"
	maxProviderWeight                  = 1_000_000
)

type ruleConfig struct {
	Strategy        string           `yaml:"strategy"`
	ProviderGroupBy string           `yaml:"provider-group-by"`
	ProviderWeights map[string]int64 `yaml:"provider-weights"`
}

type pluginConfig struct {
	Rules map[string]ruleConfig `yaml:"rules"`
}

type schedulerPlugin struct {
	mu                sync.Mutex
	config            pluginConfig
	groupCurrent      map[string]map[string]int64
	credentialCursors map[string]map[string]int
}

func newSchedulerPlugin() *schedulerPlugin {
	return &schedulerPlugin{
		groupCurrent:      make(map[string]map[string]int64),
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

		rule.ProviderGroupBy = strings.ToLower(strings.TrimSpace(rule.ProviderGroupBy))
		switch rule.ProviderGroupBy {
		case "", providerGroupByProvider:
			rule.ProviderGroupBy = providerGroupByProvider
		case providerGroupByBaseURL:
		default:
			return fmt.Errorf("model %q has unsupported provider group %q", normalizedModel, rule.ProviderGroupBy)
		}

		normalizedWeights := make(map[string]int64, len(rule.ProviderWeights))
		for provider, weight := range rule.ProviderWeights {
			normalizedProvider := normalizeGroupKey(rule.ProviderGroupBy, provider)
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
	p.groupCurrent = make(map[string]map[string]int64)
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

func (p *schedulerPlugin) pickProviderWeightedLocked(
	model string,
	rule ruleConfig,
	candidates []pluginapi.SchedulerAuthCandidate,
) pluginapi.SchedulerPickResponse {
	if len(candidates) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}

	bestPriority := 0
	hasEligible := false
	for _, candidate := range candidates {
		group := candidateGroup(rule, candidate)
		if !eligibleCandidate(rule, candidate, group) {
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

	byGroup := make(map[string][]pluginapi.SchedulerAuthCandidate)
	for _, candidate := range candidates {
		group := candidateGroup(rule, candidate)
		if candidate.Priority != bestPriority || !eligibleCandidate(rule, candidate, group) {
			continue
		}
		byGroup[group] = append(byGroup[group], candidate)
	}
	if len(byGroup) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}

	groups := make([]string, 0, len(byGroup))
	for group := range byGroup {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	for _, group := range groups {
		sort.Slice(byGroup[group], func(i, j int) bool {
			return byGroup[group][i].ID < byGroup[group][j].ID
		})
	}

	current := p.groupCurrent[model]
	if current == nil {
		current = make(map[string]int64)
		p.groupCurrent[model] = current
	}
	for group := range current {
		if _, active := byGroup[group]; !active {
			delete(current, group)
		}
	}

	selectedGroup := ""
	var selectedCurrent int64
	var totalWeight int64
	for _, group := range groups {
		weight := groupWeight(rule, group)
		current[group] += weight
		totalWeight += weight
		if selectedGroup == "" || current[group] > selectedCurrent {
			selectedGroup = group
			selectedCurrent = current[group]
		}
	}
	current[selectedGroup] -= totalWeight

	groupCursors := p.credentialCursors[model]
	if groupCursors == nil {
		groupCursors = make(map[string]int)
		p.credentialCursors[model] = groupCursors
	}
	groupCandidates := byGroup[selectedGroup]
	cursor := groupCursors[selectedGroup] % len(groupCandidates)
	selected := groupCandidates[cursor]
	groupCursors[selectedGroup] = cursor + 1

	return pluginapi.SchedulerPickResponse{AuthID: selected.ID, Handled: true}
}

func candidateGroup(rule ruleConfig, candidate pluginapi.SchedulerAuthCandidate) string {
	provider := normalizeGroupKey(providerGroupByProvider, candidate.Provider)
	if rule.ProviderGroupBy != providerGroupByBaseURL {
		return provider
	}
	baseURL := normalizeGroupKey(providerGroupByBaseURL, candidate.Attributes["base_url"])
	if baseURL != "" {
		return baseURL
	}
	return provider
}

func normalizeGroupKey(groupBy, value string) string {
	trimmed := strings.TrimSpace(value)
	if groupBy != providerGroupByBaseURL {
		return strings.ToLower(trimmed)
	}
	return normalizeBaseURL(trimmed)
}

func normalizeBaseURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return value
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	return parsed.String()
}

func eligibleCandidate(
	rule ruleConfig,
	candidate pluginapi.SchedulerAuthCandidate,
	group string,
) bool {
	return group != "" && strings.TrimSpace(candidate.ID) != "" && groupWeight(rule, group) > 0
}

func groupWeight(rule ruleConfig, group string) int64 {
	if weight, exists := rule.ProviderWeights[group]; exists {
		return weight
	}
	return 1
}
