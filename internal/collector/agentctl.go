package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	_ "modernc.org/sqlite"
)

type AgentctlCollector struct {
	dbPath                string
	ccusagePath           string
	sessionCount          *prometheus.GaugeVec
	sessionRuntimeCount   *prometheus.GaugeVec
	taskTotal             *prometheus.GaugeVec
	taskDuration          prometheus.Histogram
	actionBurn            *prometheus.CounterVec
	costPerHour           prometheus.Gauge
	protectedSessionCount prometheus.Gauge
	cleanupCandidateCount *prometheus.GaugeVec
	prMergeableCount      *prometheus.GaugeVec
	jobRunCount           *prometheus.GaugeVec
	jobLastSuccessAt      *prometheus.GaugeVec
	jobLastDuration       *prometheus.GaugeVec

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

type sessionRuntimeCountSample struct {
	status        string
	runtimeStatus string
	role          string
	isLoop        string
	isProtected   string
	count         int64
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
		sessionRuntimeCount: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "agent_session_runtime_count",
				Help: "Number of non-archived agentctl sessions grouped by status, runtime status, role, loop flag, and protected flag.",
			},
			[]string{"status", "runtime_status", "role", "is_loop", "is_protected"},
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
		protectedSessionCount: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "agent_protected_session_count",
				Help: "Number of non-dead protected sessions currently tracked by agentctl.",
			},
		),
		cleanupCandidateCount: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "agent_cleanup_candidate_count",
				Help: "Number of manual cleanup candidates derived from agentctl session state.",
			},
			[]string{"kind"},
		),
		prMergeableCount: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "agent_pr_mergeable_count",
				Help: "Number of live PR-backed sessions grouped by cached mergeable state.",
			},
			[]string{"state"},
		),
		jobRunCount: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "agent_job_run_count",
				Help: "Number of scheduler job runs grouped by job name and status.",
			},
			[]string{"job", "status"},
		),
		jobLastSuccessAt: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "agent_job_last_success_timestamp_seconds",
				Help: "Unix timestamp of the latest successful job run grouped by job name.",
			},
			[]string{"job"},
		),
		jobLastDuration: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "agent_job_last_duration_seconds",
				Help: "Duration in seconds of the latest completed job run grouped by job name.",
			},
			[]string{"job"},
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

	if err := registry.Register(c.sessionRuntimeCount); err != nil {
		return nil, fmt.Errorf("register agent_session_runtime_count: %w", err)
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

	if err := registry.Register(c.protectedSessionCount); err != nil {
		return nil, fmt.Errorf("register agent_protected_session_count: %w", err)
	}

	if err := registry.Register(c.cleanupCandidateCount); err != nil {
		return nil, fmt.Errorf("register agent_cleanup_candidate_count: %w", err)
	}

	if err := registry.Register(c.prMergeableCount); err != nil {
		return nil, fmt.Errorf("register agent_pr_mergeable_count: %w", err)
	}

	if err := registry.Register(c.jobRunCount); err != nil {
		return nil, fmt.Errorf("register agent_job_run_count: %w", err)
	}

	if err := registry.Register(c.jobLastSuccessAt); err != nil {
		return nil, fmt.Errorf("register agent_job_last_success_timestamp_seconds: %w", err)
	}

	if err := registry.Register(c.jobLastDuration); err != nil {
		return nil, fmt.Errorf("register agent_job_last_duration_seconds: %w", err)
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

	if err := c.collectSessionRuntimeCounts(ctx, db); err != nil {
		collectErrors = append(collectErrors, fmt.Errorf("collect session runtime counts: %w", err))
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

	if err := c.collectProtectedSessionCount(ctx, db); err != nil {
		collectErrors = append(collectErrors, fmt.Errorf("collect protected sessions: %w", err))
	}

	if err := c.collectCleanupCandidates(ctx, db); err != nil {
		collectErrors = append(collectErrors, fmt.Errorf("collect cleanup candidates: %w", err))
	}

	if err := c.collectPRMergeableCounts(ctx, db); err != nil {
		collectErrors = append(collectErrors, fmt.Errorf("collect PR mergeable counts: %w", err))
	}

	if err := c.collectJobRuns(ctx, db); err != nil {
		collectErrors = append(collectErrors, fmt.Errorf("collect job runs: %w", err))
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

func (c *AgentctlCollector) collectSessionRuntimeCounts(ctx context.Context, db *sql.DB) error {
	columns, err := tableColumns(ctx, db, "sessions")
	if err != nil {
		return err
	}

	runtimeStatusExpr := "'unknown'"
	if columns["runtime_status"] {
		runtimeStatusExpr = "COALESCE(NULLIF(runtime_status, ''), 'unknown')"
	}

	roleExpr := "'unknown'"
	if columns["role"] {
		roleExpr = "COALESCE(NULLIF(role, ''), 'unknown')"
	}

	isLoopExpr := "0"
	if columns["is_loop"] {
		isLoopExpr = "COALESCE(is_loop, 0)"
	}

	isProtectedExpr := "0"
	if columns["is_protected"] {
		isProtectedExpr = "COALESCE(is_protected, 0)"
	}

	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT
			COALESCE(NULLIF(status, ''), 'unknown'),
			%s,
			%s,
			CASE WHEN %s <> 0 THEN 'true' ELSE 'false' END,
			CASE WHEN %s <> 0 THEN 'true' ELSE 'false' END,
			COUNT(*)
		FROM sessions
		WHERE archived = 0
		GROUP BY 1, 2, 3, 4, 5
	`, runtimeStatusExpr, roleExpr, isLoopExpr, isProtectedExpr))
	if err != nil {
		return err
	}
	defer rows.Close()

	var samples []sessionRuntimeCountSample
	for rows.Next() {
		var sample sessionRuntimeCountSample
		if err := rows.Scan(
			&sample.status,
			&sample.runtimeStatus,
			&sample.role,
			&sample.isLoop,
			&sample.isProtected,
			&sample.count,
		); err != nil {
			return err
		}
		samples = append(samples, sample)
	}

	if err := rows.Err(); err != nil {
		return err
	}

	c.sessionRuntimeCount.Reset()
	for _, sample := range samples {
		c.sessionRuntimeCount.WithLabelValues(
			sample.status,
			sample.runtimeStatus,
			sample.role,
			sample.isLoop,
			sample.isProtected,
		).Set(float64(sample.count))
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

func (c *AgentctlCollector) collectProtectedSessionCount(ctx context.Context, db *sql.DB) error {
	columns, err := tableColumns(ctx, db, "sessions")
	if err != nil {
		return err
	}
	if !columns["is_protected"] {
		c.protectedSessionCount.Set(0)
		return nil
	}

	row := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM sessions
		WHERE archived = 0
		  AND COALESCE(is_protected, 0) <> 0
		  AND COALESCE(NULLIF(status, ''), 'unknown') <> 'dead'
	`)

	var count int64
	if err := row.Scan(&count); err != nil {
		return err
	}

	c.protectedSessionCount.Set(float64(count))
	return nil
}

func (c *AgentctlCollector) collectCleanupCandidates(ctx context.Context, db *sql.DB) error {
	columns, err := tableColumns(ctx, db, "sessions")
	if err != nil {
		return err
	}
	if !columns["last_active"] {
		c.cleanupCandidateCount.Reset()
		return nil
	}

	runtimeStatusExpr := "'unknown'"
	if columns["runtime_status"] {
		runtimeStatusExpr = "LOWER(COALESCE(NULLIF(runtime_status, ''), 'unknown'))"
	}

	roleExpr := "'worker'"
	if columns["role"] {
		roleExpr = "LOWER(COALESCE(NULLIF(role, ''), 'worker'))"
	}

	isLoopExpr := "0"
	if columns["is_loop"] {
		isLoopExpr = "COALESCE(is_loop, 0)"
	}

	isProtectedExpr := "0"
	if columns["is_protected"] {
		isProtectedExpr = "COALESCE(is_protected, 0)"
	}

	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COUNT(*)
		FROM sessions
		WHERE archived = 0
		  AND LOWER(COALESCE(NULLIF(status, ''), 'unknown')) = 'idle'
		  AND %s = 'worker'
		  AND %s IN ('running', 'exited')
		  AND %s = 0
		  AND %s = 0
		  AND last_active IS NOT NULL
		  AND ((julianday('now') - julianday(last_active)) * 1440.0) >= 360
	`, roleExpr, runtimeStatusExpr, isLoopExpr, isProtectedExpr))

	var staleIdleCount int64
	if err := row.Scan(&staleIdleCount); err != nil {
		return err
	}

	c.cleanupCandidateCount.Reset()
	c.cleanupCandidateCount.WithLabelValues("stale_idle").Set(float64(staleIdleCount))
	return nil
}

func (c *AgentctlCollector) collectPRMergeableCounts(ctx context.Context, db *sql.DB) error {
	columns, err := tableColumns(ctx, db, "sessions")
	if err != nil {
		return err
	}

	isProtectedExpr := "0"
	if columns["is_protected"] {
		isProtectedExpr = "COALESCE(s.is_protected, 0)"
	}

	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT
			UPPER(
				TRIM(
					COALESCE(
						CASE
							WHEN ms.value IS NULL OR TRIM(ms.value) = '' THEN 'UNKNOWN'
							WHEN instr(ms.value, '|') > 0 THEN substr(ms.value, 1, instr(ms.value, '|') - 1)
							ELSE ms.value
						END,
						'UNKNOWN'
					)
				)
			) AS mergeable_state,
			COUNT(*)
		FROM sessions s
		LEFT JOIN manager_state ms ON ms.key = 'mergeable_cache:' || s.pr_url
		WHERE s.archived = 0
		  AND COALESCE(NULLIF(s.pr_url, ''), '') <> ''
		  AND COALESCE(NULLIF(s.status, ''), 'unknown') <> 'dead'
		  AND %s = 0
		GROUP BY 1
	`, isProtectedExpr))
	if err != nil {
		if isMissingTableErr(err) {
			c.prMergeableCount.Reset()
			return nil
		}
		return err
	}
	defer rows.Close()

	c.prMergeableCount.Reset()
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return err
		}
		state = normalizeMetricLabel(strings.ToUpper(state))
		c.prMergeableCount.WithLabelValues(state).Set(float64(count))
	}

	return rows.Err()
}

func (c *AgentctlCollector) collectJobRuns(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
		SELECT
			j.name,
			COALESCE(NULLIF(jr.status, ''), 'unknown'),
			COALESCE(jr.started_at, ''),
			COALESCE(jr.finished_at, '')
		FROM job_runs jr
		JOIN jobs j ON j.id = jr.job_id
		ORDER BY jr.id DESC
	`)
	if err != nil {
		if isMissingTableErr(err) {
			c.jobRunCount.Reset()
			c.jobLastSuccessAt.Reset()
			c.jobLastDuration.Reset()
			return nil
		}
		return err
	}
	defer rows.Close()

	type lastRun struct {
		started  time.Time
		finished time.Time
	}

	counts := make(map[string]map[string]int64)
	lastSuccess := make(map[string]time.Time)
	lastCompleted := make(map[string]lastRun)
	seenLatest := make(map[string]struct{})

	for rows.Next() {
		var (
			jobName        string
			status         string
			startedAtText  string
			finishedAtText string
		)
		if err := rows.Scan(&jobName, &status, &startedAtText, &finishedAtText); err != nil {
			return err
		}

		jobName = normalizeMetricLabel(jobName)
		status = normalizeMetricLabel(status)

		if _, ok := counts[jobName]; !ok {
			counts[jobName] = make(map[string]int64)
		}
		counts[jobName][status]++

		startedAt, hasStarted := parseAgentctlTimestamp(startedAtText)
		finishedAt, hasFinished := parseAgentctlTimestamp(finishedAtText)

		if status == "success" && hasFinished {
			if previous, ok := lastSuccess[jobName]; !ok || finishedAt.After(previous) {
				lastSuccess[jobName] = finishedAt
			}
		}

		if _, ok := seenLatest[jobName]; !ok && hasStarted && hasFinished && !finishedAt.Before(startedAt) {
			lastCompleted[jobName] = lastRun{started: startedAt, finished: finishedAt}
			seenLatest[jobName] = struct{}{}
		}
	}

	if err := rows.Err(); err != nil {
		return err
	}

	c.jobRunCount.Reset()
	for jobName, statuses := range counts {
		for status, count := range statuses {
			c.jobRunCount.WithLabelValues(jobName, status).Set(float64(count))
		}
	}

	c.jobLastSuccessAt.Reset()
	for jobName, finishedAt := range lastSuccess {
		c.jobLastSuccessAt.WithLabelValues(jobName).Set(float64(finishedAt.Unix()))
	}

	c.jobLastDuration.Reset()
	for jobName, run := range lastCompleted {
		c.jobLastDuration.WithLabelValues(jobName).Set(run.finished.Sub(run.started).Seconds())
	}

	return nil
}

func tableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns := make(map[string]bool)
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notNull    int
			defaultV   sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &defaultV, &primaryKey); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}

func isMissingTableErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such table")
}

func parseAgentctlTimestamp(value string) (time.Time, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, false
	}

	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		parsed, err := time.Parse(layout, trimmed)
		if err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
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
