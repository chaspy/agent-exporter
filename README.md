# agent-exporter

Prometheus exporter for AI agent observability with agentctl-backed metrics.

## Features

- Exposes a Prometheus `/metrics` endpoint on port `9100` by default
- Reads session, task, and action data from `~/.agentctl/manager.db`
- Reads live cost-per-hour data from `~/.agentctl/ccusage-cache.json`
- Refreshes metrics every `5m` with configurable paths and interval

## Metrics

- `agent_session_count{status,agent,repository}`
- `agent_task_total{status}`
- `agent_task_duration_seconds`
- `agent_action_token_burn_total{agent,repository}`
- `agent_cost_per_hour`

## Configuration

The exporter reads `config.yaml` from the current working directory when present.
If the file is missing, the built-in defaults below are used.

```yaml
agentctl_db: ${HOME}/.agentctl/manager.db
ccusage_cache: ${HOME}/.agentctl/ccusage-cache.json
prometheus_port: 9100
collect_interval: 5m
```

## Run

```bash
go run ./cmd/agent-exporter
```

Override the config file location with:

```bash
go run ./cmd/agent-exporter --config /path/to/config.yaml
```

Then scrape:

```bash
curl http://localhost:9100/metrics
```

## Development

```bash
go build ./...
go vet ./...
```
