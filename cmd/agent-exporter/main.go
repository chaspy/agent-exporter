package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chaspy/agent-exporter/internal/collector"
	"github.com/chaspy/agent-exporter/internal/exporter"
	"github.com/chaspy/agent-exporter/internal/mcp"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const collectionTimeout = 30 * time.Second

type fileConfig struct {
	AgentctlDB        string `yaml:"agentctl_db"`
	CCUsageCache      string `yaml:"ccusage_cache"`
	CodexDB           string `yaml:"codex_db"`
	ClaudeProjectsDir string `yaml:"claude_projects_dir"`
	PrometheusPort    int    `yaml:"prometheus_port"`
	MCPPort           int    `yaml:"mcp_port"`
	CollectInterval   string `yaml:"collect_interval"`
}

type config struct {
	AgentctlDB        string
	CCUsageCache      string
	CodexDB           string
	ClaudeProjectsDir string
	PrometheusPort    int
	MCPPort           int
	CollectInterval   time.Duration
}

type collectorRunner interface {
	Collect(ctx context.Context) error
}

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:           "agent-exporter",
		Short:         "Expose Prometheus metrics for agentctl activity",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), configPath)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "config.yaml", "Path to the configuration file")

	return cmd
}

func run(parent context.Context, configPath string) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	registry := exporter.NewRegistry()
	agentctlCollector, err := collector.NewAgentctlCollector(
		registry,
		cfg.AgentctlDB,
		cfg.CCUsageCache,
	)
	if err != nil {
		return fmt.Errorf("initialize agentctl collector: %w", err)
	}

	codexCollector, err := collector.NewCodexCollector(registry, cfg.CodexDB)
	if err != nil {
		return fmt.Errorf("initialize codex collector: %w", err)
	}

	claudeCollector, err := collector.NewClaudeCollector(registry, cfg.ClaudeProjectsDir)
	if err != nil {
		return fmt.Errorf("initialize claude collector: %w", err)
	}

	collectors := []collectorRunner{
		agentctlCollector,
		codexCollector,
		claudeCollector,
	}

	if err := collectOnce(ctx, collectors...); err != nil {
		logger.Warn("initial collection failed", "error", err)
	}

	metricsServer := exporter.NewServer(cfg.PrometheusPort, registry)
	mcpServer := mcp.NewServer(cfg.MCPPort, cfg.AgentctlDB, cfg.CodexDB)
	serverErrCh := make(chan error, 2)

	go func() {
		logger.Info(
			"starting exporter",
			"metrics_addr", metricsServer.Addr,
			"mcp_addr", mcpServer.Addr,
			"agentctl_db", cfg.AgentctlDB,
			"ccusage_cache", cfg.CCUsageCache,
			"codex_db", cfg.CodexDB,
			"claude_projects_dir", cfg.ClaudeProjectsDir,
			"collect_interval", cfg.CollectInterval.String(),
		)

		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- fmt.Errorf("metrics server failed: %w", err)
		}
	}()

	go func() {
		if err := mcpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- fmt.Errorf("mcp server failed: %w", err)
		}
	}()

	ticker := time.NewTicker(cfg.CollectInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			var shutdownErrors []error
			if err := metricsServer.Shutdown(shutdownCtx); err != nil {
				shutdownErrors = append(shutdownErrors, fmt.Errorf("shutdown metrics server: %w", err))
			}
			if err := mcpServer.Shutdown(shutdownCtx); err != nil {
				shutdownErrors = append(shutdownErrors, fmt.Errorf("shutdown mcp server: %w", err))
			}
			if err := errors.Join(shutdownErrors...); err != nil {
				return err
			}

			logger.Info("exporter stopped")
			return nil
		case <-ticker.C:
			if err := collectOnce(ctx, collectors...); err != nil {
				logger.Warn("periodic collection failed", "error", err)
			}
		case err := <-serverErrCh:
			stop()

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			var shutdownErrors []error
			if shutdownErr := metricsServer.Shutdown(shutdownCtx); shutdownErr != nil {
				shutdownErrors = append(shutdownErrors, fmt.Errorf("shutdown metrics server: %w", shutdownErr))
			}
			if shutdownErr := mcpServer.Shutdown(shutdownCtx); shutdownErr != nil {
				shutdownErrors = append(shutdownErrors, fmt.Errorf("shutdown mcp server: %w", shutdownErr))
			}

			return errors.Join(append([]error{err}, shutdownErrors...)...)
		}
	}
}

func collectOnce(parent context.Context, collectors ...collectorRunner) error {
	ctx, cancel := context.WithTimeout(parent, collectionTimeout)
	defer cancel()

	var collectErrors []error
	for _, collector := range collectors {
		if err := collector.Collect(ctx); err != nil {
			collectErrors = append(collectErrors, err)
		}
	}

	return errors.Join(collectErrors...)
}

func loadConfig(path string) (config, error) {
	raw := fileConfig{
		AgentctlDB:        "${HOME}/.agentctl/manager.db",
		CCUsageCache:      "${HOME}/.agentctl/ccusage-cache.json",
		CodexDB:           "${HOME}/.codex/state_5.sqlite",
		ClaudeProjectsDir: "${HOME}/.claude/projects",
		PrometheusPort:    9100,
		MCPPort:           9101,
		CollectInterval:   "5m",
	}

	content, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return config{}, fmt.Errorf("read config %q: %w", path, err)
		}
	} else {
		if err := yaml.Unmarshal(content, &raw); err != nil {
			return config{}, fmt.Errorf("parse config %q: %w", path, err)
		}
	}

	agentctlDB, err := expandPath(raw.AgentctlDB)
	if err != nil {
		return config{}, fmt.Errorf("resolve agentctl_db: %w", err)
	}

	ccusageCache, err := expandPath(raw.CCUsageCache)
	if err != nil {
		return config{}, fmt.Errorf("resolve ccusage_cache: %w", err)
	}

	codexDB, err := expandPath(raw.CodexDB)
	if err != nil {
		return config{}, fmt.Errorf("resolve codex_db: %w", err)
	}

	claudeProjectsDir, err := expandPath(raw.ClaudeProjectsDir)
	if err != nil {
		return config{}, fmt.Errorf("resolve claude_projects_dir: %w", err)
	}

	interval, err := time.ParseDuration(raw.CollectInterval)
	if err != nil {
		return config{}, fmt.Errorf("parse collect_interval: %w", err)
	}

	if interval <= 0 {
		return config{}, fmt.Errorf("collect_interval must be positive")
	}

	if raw.PrometheusPort <= 0 || raw.PrometheusPort > 65535 {
		return config{}, fmt.Errorf("prometheus_port must be between 1 and 65535")
	}

	if raw.MCPPort <= 0 || raw.MCPPort > 65535 {
		return config{}, fmt.Errorf("mcp_port must be between 1 and 65535")
	}

	return config{
		AgentctlDB:        agentctlDB,
		CCUsageCache:      ccusageCache,
		CodexDB:           codexDB,
		ClaudeProjectsDir: claudeProjectsDir,
		PrometheusPort:    raw.PrometheusPort,
		MCPPort:           raw.MCPPort,
		CollectInterval:   interval,
	}, nil
}

func expandPath(path string) (string, error) {
	expanded := os.ExpandEnv(path)

	if expanded == "~" || strings.HasPrefix(expanded, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}

		if expanded == "~" {
			return home, nil
		}

		return home + expanded[1:], nil
	}

	return expanded, nil
}
