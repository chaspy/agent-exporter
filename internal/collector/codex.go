package collector

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

type CodexCollector struct {
	dbPath                string
	threadTokensTotal     *prometheus.CounterVec
	threadTokensHistogram *prometheus.HistogramVec

	mu                sync.Mutex
	observedThreadIDs map[string]struct{}
}

func NewCodexCollector(registry prometheus.Registerer, dbPath string) (*CodexCollector, error) {
	c := &CodexCollector{
		dbPath: dbPath,
		threadTokensTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "codex_thread_tokens_total",
				Help: "Total tokens consumed by Codex threads grouped by model and repository.",
			},
			[]string{"model", "repository"},
		),
		threadTokensHistogram: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "codex_thread_tokens_histogram",
				Help:    "Distribution of tokens consumed per Codex thread grouped by model.",
				Buckets: []float64{10000, 50000, 100000, 500000, 1000000, 5000000, 10000000},
			},
			[]string{"model"},
		),
		observedThreadIDs: make(map[string]struct{}),
	}

	if err := registry.Register(c.threadTokensTotal); err != nil {
		return nil, fmt.Errorf("register codex_thread_tokens_total: %w", err)
	}

	if err := registry.Register(c.threadTokensHistogram); err != nil {
		return nil, fmt.Errorf("register codex_thread_tokens_histogram: %w", err)
	}

	return c, nil
}

func (c *CodexCollector) Collect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	db, err := openReadOnlyDB(ctx, c.dbPath)
	if err != nil {
		return fmt.Errorf("open codex db: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT
			id,
			COALESCE(NULLIF(model, ''), 'unknown'),
			COALESCE(tokens_used, 0),
			git_origin_url
		FROM threads
		ORDER BY id
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id           string
			model        string
			tokensUsed   int64
			gitOriginURL sql.NullString
		)

		if err := rows.Scan(&id, &model, &tokensUsed, &gitOriginURL); err != nil {
			return err
		}

		if _, alreadyObserved := c.observedThreadIDs[id]; alreadyObserved {
			continue
		}

		if tokensUsed < 0 {
			tokensUsed = 0
		}

		model = normalizeMetricLabel(model)
		repository := extractRepositoryFromGitOrigin(gitOriginURL.String)
		c.threadTokensTotal.WithLabelValues(model, repository).Add(float64(tokensUsed))
		c.threadTokensHistogram.WithLabelValues(model).Observe(float64(tokensUsed))
		c.observedThreadIDs[id] = struct{}{}
	}

	return rows.Err()
}

func extractRepositoryFromGitOrigin(origin string) string {
	trimmed := strings.TrimSpace(origin)
	if trimmed == "" {
		return "unknown"
	}

	if strings.HasPrefix(trimmed, "git@") {
		if index := strings.Index(trimmed, ":"); index >= 0 && index < len(trimmed)-1 {
			path := strings.TrimSuffix(strings.Trim(trimmed[index+1:], "/"), ".git")
			if path != "" {
				return path
			}
		}
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "unknown"
	}

	path := strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git")
	if path == "" || !strings.Contains(path, "/") {
		return "unknown"
	}

	return path
}
