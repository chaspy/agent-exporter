package collector

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const claudeJSONLMaxLineSize = 16 * 1024 * 1024

type ClaudeCollector struct {
	projectsDir string

	inputTokensTotal         *prometheus.CounterVec
	outputTokensTotal        *prometheus.CounterVec
	cacheCreationTokensTotal *prometheus.CounterVec
	cacheReadTokensTotal     *prometheus.CounterVec

	mu                 sync.Mutex
	observedMessageIDs map[string]struct{}
}

type claudeJSONLRecord struct {
	Type       string             `json:"type"`
	Model      string             `json:"model"`
	MessageID  string             `json:"messageId"`
	ParentUUID string             `json:"parentUuid"`
	Message    claudeJSONLMessage `json:"message"`
}

type claudeJSONLMessage struct {
	Role  string           `json:"role"`
	Usage claudeJSONLUsage `json:"usage"`
}

type claudeJSONLUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func NewClaudeCollector(registry prometheus.Registerer, projectsDir string) (*ClaudeCollector, error) {
	c := &ClaudeCollector{
		projectsDir: projectsDir,
		inputTokensTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "claude_input_tokens_total",
				Help: "Total Claude input tokens grouped by model.",
			},
			[]string{"model"},
		),
		outputTokensTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "claude_output_tokens_total",
				Help: "Total Claude output tokens grouped by model.",
			},
			[]string{"model"},
		),
		cacheCreationTokensTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "claude_cache_creation_tokens_total",
				Help: "Total Claude cache creation input tokens grouped by model.",
			},
			[]string{"model"},
		),
		cacheReadTokensTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "claude_cache_read_tokens_total",
				Help: "Total Claude cache read input tokens grouped by model.",
			},
			[]string{"model"},
		),
		observedMessageIDs: make(map[string]struct{}),
	}

	if err := registry.Register(c.inputTokensTotal); err != nil {
		return nil, fmt.Errorf("register claude_input_tokens_total: %w", err)
	}

	if err := registry.Register(c.outputTokensTotal); err != nil {
		return nil, fmt.Errorf("register claude_output_tokens_total: %w", err)
	}

	if err := registry.Register(c.cacheCreationTokensTotal); err != nil {
		return nil, fmt.Errorf("register claude_cache_creation_tokens_total: %w", err)
	}

	if err := registry.Register(c.cacheReadTokensTotal); err != nil {
		return nil, fmt.Errorf("register claude_cache_read_tokens_total: %w", err)
	}

	return c, nil
}

func (c *ClaudeCollector) Collect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	files, err := c.listJSONLFiles(ctx)
	if err != nil {
		return fmt.Errorf("list claude project files: %w", err)
	}

	for _, path := range files {
		if err := c.collectFile(ctx, path); err != nil {
			return err
		}
	}

	return nil
}

func (c *ClaudeCollector) listJSONLFiles(ctx context.Context) ([]string, error) {
	files := make([]string, 0)

	err := filepath.WalkDir(c.projectsDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if entry.IsDir() {
			return nil
		}

		if filepath.Ext(path) == ".jsonl" {
			files = append(files, path)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(files)
	return files, nil
}

func (c *ClaudeCollector) collectFile(ctx context.Context, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open claude jsonl %q: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), claudeJSONLMaxLineSize)

	project := extractClaudeProject(path, c.projectsDir)
	lineNumber := 0
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var record claudeJSONLRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return fmt.Errorf("parse claude jsonl %q line %d: %w", path, lineNumber, err)
		}

		if !isClaudeAssistantRecord(record) {
			continue
		}

		messageID := strings.TrimSpace(record.MessageID)
		if messageID == "" {
			messageID = strings.TrimSpace(record.ParentUUID)
		}
		if messageID == "" {
			continue
		}

		observationKey := buildClaudeObservationKey(project, path, messageID)
		if _, alreadyObserved := c.observedMessageIDs[observationKey]; alreadyObserved {
			continue
		}

		model := normalizeMetricLabel(record.Model)
		c.inputTokensTotal.WithLabelValues(model).Add(float64(clampNonNegative(record.Message.Usage.InputTokens)))
		c.outputTokensTotal.WithLabelValues(model).Add(float64(clampNonNegative(record.Message.Usage.OutputTokens)))
		c.cacheCreationTokensTotal.WithLabelValues(model).Add(float64(clampNonNegative(record.Message.Usage.CacheCreationInputTokens)))
		c.cacheReadTokensTotal.WithLabelValues(model).Add(float64(clampNonNegative(record.Message.Usage.CacheReadInputTokens)))
		c.observedMessageIDs[observationKey] = struct{}{}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan claude jsonl %q: %w", path, err)
	}

	return nil
}

func isClaudeAssistantRecord(record claudeJSONLRecord) bool {
	return strings.EqualFold(record.Type, "assistant") && strings.EqualFold(record.Message.Role, "assistant")
}

func buildClaudeObservationKey(project string, path string, messageID string) string {
	if project == "unknown" {
		return path + ":" + messageID
	}

	return project + ":" + messageID
}

func extractClaudeProject(path string, projectsDir string) string {
	relativePath, err := filepath.Rel(projectsDir, path)
	if err != nil {
		return "unknown"
	}

	parts := strings.Split(relativePath, string(filepath.Separator))
	if len(parts) < 2 {
		return "unknown"
	}

	const marker = "github-com-"
	encodedPath := parts[0]
	index := strings.LastIndex(encodedPath, marker)
	if index < 0 {
		return "unknown"
	}

	remainder := encodedPath[index+len(marker):]
	segments := strings.Split(remainder, "-")
	if len(segments) < 2 || segments[0] == "" || segments[1] == "" {
		return "unknown"
	}

	return segments[0] + "/" + segments[1]
}

func clampNonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}

	return value
}

func normalizeMetricLabel(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "unknown"
	}

	return trimmed
}
