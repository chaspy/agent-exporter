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
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const collectionTimeout = 30 * time.Second

type fileConfig struct {
	AgentctlDB      string `yaml:"agentctl_db"`
	CCUsageCache    string `yaml:"ccusage_cache"`
	PrometheusPort  int    `yaml:"prometheus_port"`
	CollectInterval string `yaml:"collect_interval"`
}

type config struct {
	AgentctlDB      string
	CCUsageCache    string
	PrometheusPort  int
	CollectInterval time.Duration
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
		return fmt.Errorf("initialize collector: %w", err)
	}

	if err := collectOnce(ctx, agentctlCollector); err != nil {
		logger.Warn("initial collection failed", "error", err)
	}

	server := exporter.NewServer(cfg.PrometheusPort, registry)
	serverErrCh := make(chan error, 1)

	go func() {
		logger.Info(
			"starting exporter",
			"addr", server.Addr,
			"agentctl_db", cfg.AgentctlDB,
			"ccusage_cache", cfg.CCUsageCache,
			"collect_interval", cfg.CollectInterval.String(),
		)

		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- err
		}
	}()

	ticker := time.NewTicker(cfg.CollectInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			if err := server.Shutdown(shutdownCtx); err != nil {
				return fmt.Errorf("shutdown metrics server: %w", err)
			}

			logger.Info("exporter stopped")
			return nil
		case <-ticker.C:
			if err := collectOnce(ctx, agentctlCollector); err != nil {
				logger.Warn("periodic collection failed", "error", err)
			}
		case err := <-serverErrCh:
			return fmt.Errorf("metrics server failed: %w", err)
		}
	}
}

func collectOnce(parent context.Context, agentctlCollector *collector.AgentctlCollector) error {
	ctx, cancel := context.WithTimeout(parent, collectionTimeout)
	defer cancel()

	return agentctlCollector.Collect(ctx)
}

func loadConfig(path string) (config, error) {
	raw := fileConfig{
		AgentctlDB:      "${HOME}/.agentctl/manager.db",
		CCUsageCache:    "${HOME}/.agentctl/ccusage-cache.json",
		PrometheusPort:  9100,
		CollectInterval: "5m",
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

	return config{
		AgentctlDB:      agentctlDB,
		CCUsageCache:    ccusageCache,
		PrometheusPort:  raw.PrometheusPort,
		CollectInterval: interval,
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
