package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	_ "modernc.org/sqlite"
)

type AgentctlCollector struct {
	dbPath       string
	ccusagePath  string
	sessionCount *prometheus.GaugeVec
	taskTotal    *prometheus.GaugeVec
	taskDuration prometheus.Histogram
	actionBurn   *prometheus.CounterVec
	costPerHour  prometheus.Gauge

	mu                sync.Mutex
	observedTaskIDs   map[int64]struct{}
	observedActionIDs map[int64]struct{}
}

type ccusageCache struct {
	Block struct {
		BurnRate struct {
			CostPerHour float64 `json:"costPerHour"`
		} `json:"burnRate"`
	} `json:"block"`
}

type sessionCountSample struct {
	status     string
	agent      string
	repository string
	count      int64
}

type taskTotalSample struct {
	status string
	count  int64
}

func NewAgentctlCollector(
	registry prometheus.Registerer,
	dbPath string,
	ccusagePath string,
) (*AgentctlCollector, error) {
	c := &AgentctlCollector{
		dbPath:      dbPath,
		ccusagePath: ccusagePath,
		sessionCount: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "agent_session_count",
				Help: "Number of agentctl sessions grouped by status, agent, and repository.",
			},
			[]string{"status", "agent", "repository"},
		),
		taskTotal: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "agent_task_total",
				Help: "Number of agentctl tasks grouped by status.",
			},
			[]string{"status"},
		),
		taskDuration: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "agent_task_duration_seconds",
				Help:    "Duration of completed agentctl tasks in seconds.",
				Buckets: []float64{30, 60, 120, 300, 600, 1800, 3600, 7200, 21600, 43200, 86400},
			},
		),
		actionBurn: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "agent_action_token_burn_total",
				Help: "Total token burn from agentctl actions grouped by agent and repository.",
			},
			[]string{"agent", "repository"},
		),
		costPerHour: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "agent_cost_per_hour",
				Help: "Current cost per hour taken from the agentctl ccusage cache.",
			},
		),
		observedTaskIDs:   make(map[int64]struct{}),
		observedActionIDs: make(map[int64]struct{}),
	}

	if err := registry.Register(c.sessionCount); err != nil {
		return nil, fmt.Errorf("register agent_session_count: %w", err)
	}

	if err := registry.Register(c.taskTotal); err != nil {
		return nil, fmt.Errorf("register agent_task_total: %w", err)
	}

	if err := registry.Register(c.taskDuration); err != nil {
		return nil, fmt.Errorf("register agent_task_duration_seconds: %w", err)
	}

	if err := registry.Register(c.actionBurn); err != nil {
		return nil, fmt.Errorf("register agent_action_token_burn_total: %w", err)
	}

	if err := registry.Register(c.costPerHour); err != nil {
		return nil, fmt.Errorf("register agent_cost_per_hour: %w", err)
	}

	return c, nil
}

func (c *AgentctlCollector) Collect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	db, err := openReadOnlyDB(ctx, c.dbPath)
	if err != nil {
		return fmt.Errorf("open agentctl db: %w", err)
	}
	defer db.Close()

	var collectErrors []error

	if err := c.collectSessionCounts(ctx, db); err != nil {
		collectErrors = append(collectErrors, fmt.Errorf("collect sessions: %w", err))
	}

	if err := c.collectTaskTotals(ctx, db); err != nil {
		collectErrors = append(collectErrors, fmt.Errorf("collect task totals: %w", err))
	}

	if err := c.collectTaskDurations(ctx, db); err != nil {
		collectErrors = append(collectErrors, fmt.Errorf("collect task durations: %w", err))
	}

	if err := c.collectActionBurn(ctx, db); err != nil {
		collectErrors = append(collectErrors, fmt.Errorf("collect action burn: %w", err))
	}

	if err := c.collectCostPerHour(); err != nil {
		collectErrors = append(collectErrors, fmt.Errorf("collect cost per hour: %w", err))
	}

	return errors.Join(collectErrors...)
}

func (c *AgentctlCollector) collectSessionCounts(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
		SELECT
			COALESCE(NULLIF(status, ''), 'unknown'),
			COALESCE(NULLIF(agent, ''), 'unknown'),
			COALESCE(NULLIF(repository, ''), 'unknown'),
			COUNT(*)
		FROM sessions
		GROUP BY 1, 2, 3
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var samples []sessionCountSample
	for rows.Next() {
		var sample sessionCountSample
		if err := rows.Scan(&sample.status, &sample.agent, &sample.repository, &sample.count); err != nil {
			return err
		}
		samples = append(samples, sample)
	}

	if err := rows.Err(); err != nil {
		return err
	}

	c.sessionCount.Reset()
	for _, sample := range samples {
		c.sessionCount.WithLabelValues(sample.status, sample.agent, sample.repository).
			Set(float64(sample.count))
	}

	return nil
}

func (c *AgentctlCollector) collectTaskTotals(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
		SELECT
			COALESCE(NULLIF(status, ''), 'unknown'),
			COUNT(*)
		FROM tasks
		GROUP BY 1
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var samples []taskTotalSample
	for rows.Next() {
		var sample taskTotalSample
		if err := rows.Scan(&sample.status, &sample.count); err != nil {
			return err
		}
		samples = append(samples, sample)
	}

	if err := rows.Err(); err != nil {
		return err
	}

	c.taskTotal.Reset()
	for _, sample := range samples {
		c.taskTotal.WithLabelValues(sample.status).Set(float64(sample.count))
	}

	return nil
}

func (c *AgentctlCollector) collectTaskDurations(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
		SELECT
			id,
			(julianday(completed_at) - julianday(assigned_at)) * 86400.0
		FROM tasks
		WHERE completed_at IS NOT NULL
		  AND assigned_at IS NOT NULL
		ORDER BY id
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id       int64
			duration sql.NullFloat64
		)

		if err := rows.Scan(&id, &duration); err != nil {
			return err
		}

		if _, alreadyObserved := c.observedTaskIDs[id]; alreadyObserved {
			continue
		}

		if duration.Valid && duration.Float64 >= 0 {
			c.taskDuration.Observe(duration.Float64)
		}

		c.observedTaskIDs[id] = struct{}{}
	}

	return rows.Err()
}

func (c *AgentctlCollector) collectActionBurn(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
		SELECT
			a.id,
			COALESCE(NULLIF(s.agent, ''), 'unknown'),
			COALESCE(NULLIF(s.repository, ''), 'unknown'),
			COALESCE(a.token_burn, 0)
		FROM actions a
		LEFT JOIN sessions s ON s.id = a.session_id
		ORDER BY a.id
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id         int64
			agent      string
			repository string
			tokenBurn  int64
		)

		if err := rows.Scan(&id, &agent, &repository, &tokenBurn); err != nil {
			return err
		}

		if _, alreadyObserved := c.observedActionIDs[id]; alreadyObserved {
			continue
		}

		if tokenBurn < 0 {
			tokenBurn = 0
		}

		c.actionBurn.WithLabelValues(agent, repository).Add(float64(tokenBurn))
		c.observedActionIDs[id] = struct{}{}
	}

	return rows.Err()
}

func (c *AgentctlCollector) collectCostPerHour() error {
	content, err := os.ReadFile(c.ccusagePath)
	if err != nil {
		return err
	}

	var cache ccusageCache
	if err := json.Unmarshal(content, &cache); err != nil {
		return err
	}

	c.costPerHour.Set(cache.Block.BurnRate.CostPerHour)
	return nil
}

func openReadOnlyDB(ctx context.Context, path string) (*sql.DB, error) {
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "mode=ro",
	}).String()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}
