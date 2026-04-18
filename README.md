# agent-exporter

Prometheus exporter for AI agent observability with agentctl-backed metrics.

## Features

- Exposes a Prometheus `/metrics` endpoint on port `9100` by default
- Reads session, task, and action data from `~/.agentctl/manager.db`
- Reads live cost-per-hour data from `~/.agentctl/ccusage-cache.json`
- Reads Codex thread data from `~/.codex/state_5.sqlite`
- Reads Claude conversation logs from `~/.claude/projects/**/*.jsonl`
- Refreshes metrics every `5m` with configurable paths and interval

## Metrics

- `agent_session_count{status,agent,repository}`
- `agent_task_total{status}`
- `agent_task_duration_seconds`
- `agent_action_token_burn_total{agent,repository}`
- `agent_cost_per_hour`
- `codex_thread_tokens_total{model,repository}`
- `codex_thread_tokens_histogram{model}`
- `claude_input_tokens_total{model}`
- `claude_output_tokens_total{model}`
- `claude_cache_creation_tokens_total{model}`
- `claude_cache_read_tokens_total{model}`

## Configuration

The exporter reads `config.yaml` from the current working directory when present.
If the file is missing, the built-in defaults below are used.

```yaml
agentctl_db: ${HOME}/.agentctl/manager.db
ccusage_cache: ${HOME}/.agentctl/ccusage-cache.json
codex_db: ${HOME}/.codex/state_5.sqlite
claude_projects_dir: ${HOME}/.claude/projects
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

## Installation

### Build and install as a launchd daemon (macOS)

```bash
make install
```

This will:
1. Build the binary to ~/go/bin/agent-exporter
2. Install the launchd plist to ~/Library/LaunchAgents/
3. Start agent-exporter as a background service

### Endpoints

- Prometheus metrics: http://localhost:9100/metrics
- MCP HTTP server: http://localhost:9101/mcp
- Health check: http://localhost:9101/health

### Uninstall

```bash
make uninstall
```

### Logs

```bash
tail -f /tmp/agent-exporter.log
tail -f /tmp/agent-exporter.err
```
