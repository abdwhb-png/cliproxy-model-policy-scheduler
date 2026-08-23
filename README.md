# CLIProxyAPI Model Policy Scheduler

Model-aware scheduler plugin for CLIProxyAPI. It keeps the host scheduler as the default and overrides only exact model IDs declared in plugin configuration.

## Why

CLIProxyAPI routing strategy is global. This plugin allows models with different cost or fairness requirements to use different credential policies without modifying the CLIProxyAPI fork.

## Strategies

- `fill-first`: delegate the matching model to CLIProxyAPI's built-in fill-first scheduler.
- `round-robin`: delegate the matching model to CLIProxyAPI's built-in credential round-robin scheduler.
- `provider-weighted-round-robin`: select providers proportionally, then rotate credentials inside the selected provider.

Models without a rule return `Handled: false`, so CLIProxyAPI continues with later plugins or its global scheduler.

## Configuration

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cliproxy-model-policy-scheduler:
      enabled: true
      priority: 100
      rules:
        ox-alpha-free:
          strategy: provider-weighted-round-robin
        another-model:
          strategy: provider-weighted-round-robin
          provider-weights:
            openai-compatible-provider-a: 2
            openai-compatible-provider-b: 1
        sequential-model:
          strategy: fill-first
```

Provider names are normalized to lowercase. Omitted provider weights default to `1`; `0` excludes a provider from a custom rule. Valid weights range from `0` to `1,000,000`. Negative or larger weights and unknown strategies are rejected during plugin registration or reconfiguration.

Rule matching is exact after trimming surrounding whitespace. Add another model by adding another entry under `rules`; no plugin rebuild is required.

## Commands

```bash
make verify
make build
```

`make build` produces `dist/cliproxy-model-policy-scheduler.so` for Linux AMD64.

## Local CLIProxyAPI integration

Mount the built library at `/CLIProxyAPI/plugins/cliproxy-model-policy-scheduler.so`, enable plugins globally, and configure `plugins.configs.cliproxy-model-policy-scheduler`.

After startup, request `GET /v0/management/plugins` with the existing management authentication and confirm this plugin reports `registered: true` and `effective_enabled: true`.

## Selection behavior

For `provider-weighted-round-robin`, the plugin:

1. Uses only the highest-priority eligible candidates supplied by the host.
2. Groups candidates by normalized provider ID.
3. Applies smooth weighted round-robin between providers.
4. Applies round-robin between credentials within the selected provider.
5. Returns only an auth ID present in the request's `Candidates` list.

Cursor state is process-local and resets on valid plugin reconfiguration or process restart. Multiple CLIProxyAPI replicas do not share cursor state.

## Compatibility

The plugin implements CLIProxyAPI plugin ABI schema version 1 and depends on the public `github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi` contract.
