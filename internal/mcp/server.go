package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	mcpProtocolVersion = "2024-11-05"
	mcpServerName      = "agent-exporter"
	mcpServerVersion   = "1.0.0"
	queryTimeout       = 10 * time.Second
)

type Server struct {
	agentctlDB string
	codexDB    string
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErrorObject `json:"error,omitempty"`
}

type rpcErrorObject struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolCallError struct {
	code    int
	message string
}

type initializeResult struct {
	ProtocolVersion string               `json:"protocolVersion"`
	Capabilities    initializeCapability `json:"capabilities"`
	ServerInfo      serverInfo           `json:"serverInfo"`
}

type initializeCapability struct {
	Tools map[string]any `json:"tools"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type toolsListResult struct {
	Tools []toolDefinition `json:"tools"`
}

type toolDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
}

type inputSchema struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type toolCallResult struct {
	Content []toolContent `json:"content"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type sessionStatsResponse struct {
	Sessions []sessionStat `json:"sessions"`
}

type sessionStat struct {
	Status     string `json:"status"`
	Agent      string `json:"agent"`
	Repository string `json:"repository"`
	Count      int64  `json:"count"`
}

type tokenUsageResponse struct {
	Codex           map[string]int64 `json:"codex"`
	TotalActionBurn int64            `json:"total_action_burn"`
}

type taskStatsResponse struct {
	Tasks []taskStat `json:"tasks"`
}

type taskStat struct {
	Status string `json:"status"`
	Count  int64  `json:"count"`
}

type metricsSummaryResponse struct {
	Sessions        []sessionStat    `json:"sessions"`
	Codex           map[string]int64 `json:"codex"`
	TotalActionBurn int64            `json:"total_action_burn"`
	Tasks           []taskStat       `json:"tasks"`
}

func NewServer(port int, agentctlDB string, codexDB string) *http.Server {
	server := &Server{
		agentctlDB: agentctlDB,
		codexDB:    codexDB,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", server.handleHealth)
	mux.HandleFunc("/mcp", server.handleMCP)

	return &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var request rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		s.writeRPCError(w, json.RawMessage("null"), -32700, "parse error")
		return
	}

	if request.JSONRPC != "2.0" || strings.TrimSpace(request.Method) == "" {
		s.writeRPCError(w, request.ID, -32600, "invalid request")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()

	switch request.Method {
	case "initialize":
		s.writeRPCResult(w, request.ID, initializeResult{
			ProtocolVersion: mcpProtocolVersion,
			Capabilities: initializeCapability{
				Tools: map[string]any{},
			},
			ServerInfo: serverInfo{
				Name:    mcpServerName,
				Version: mcpServerVersion,
			},
		})
	case "tools/list":
		s.writeRPCResult(w, request.ID, toolsListResult{Tools: toolDefinitions()})
	case "tools/call":
		result, err := s.handleToolCall(ctx, request.Params)
		if err != nil {
			var rpcErr *toolCallError
			if errors.As(err, &rpcErr) {
				s.writeRPCError(w, request.ID, rpcErr.code, rpcErr.message)
				return
			}
			s.writeRPCError(w, request.ID, -32603, "internal error")
			return
		}
		s.writeRPCResult(w, request.ID, result)
	default:
		s.writeRPCError(w, request.ID, -32601, "method not found")
	}
}

func (s *Server) handleToolCall(ctx context.Context, rawParams json.RawMessage) (toolCallResult, error) {
	var params toolCallParams
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return toolCallResult{}, newToolCallError(-32602, "invalid tool call params")
	}

	if strings.TrimSpace(params.Name) == "" {
		return toolCallResult{}, newToolCallError(-32602, "tool name is required")
	}

	if len(params.Arguments) > 0 && string(params.Arguments) != "null" {
		var arguments map[string]any
		if err := json.Unmarshal(params.Arguments, &arguments); err != nil {
			return toolCallResult{}, newToolCallError(-32602, "tool arguments must be an object")
		}
	}

	var payload any
	var err error

	switch params.Name {
	case "get_metrics_summary":
		payload, err = s.getMetricsSummary(ctx)
	case "get_session_stats":
		payload, err = s.getSessionStats(ctx)
	case "get_token_usage":
		payload, err = s.getTokenUsage(ctx)
	case "get_task_stats":
		payload, err = s.getTaskStats(ctx)
	default:
		return toolCallResult{}, newToolCallError(-32602, fmt.Sprintf("unknown tool %q", params.Name))
	}
	if err != nil {
		return toolCallResult{}, err
	}

	content, err := json.Marshal(payload)
	if err != nil {
		return toolCallResult{}, fmt.Errorf("marshal tool response: %w", err)
	}

	return toolCallResult{
		Content: []toolContent{{
			Type: "text",
			Text: string(content),
		}},
	}, nil
}

func (s *Server) getMetricsSummary(ctx context.Context) (metricsSummaryResponse, error) {
	sessionStats, err := s.getSessionStats(ctx)
	if err != nil {
		return metricsSummaryResponse{}, err
	}

	tokenUsage, err := s.getTokenUsage(ctx)
	if err != nil {
		return metricsSummaryResponse{}, err
	}

	taskStats, err := s.getTaskStats(ctx)
	if err != nil {
		return metricsSummaryResponse{}, err
	}

	return metricsSummaryResponse{
		Sessions:        sessionStats.Sessions,
		Codex:           tokenUsage.Codex,
		TotalActionBurn: tokenUsage.TotalActionBurn,
		Tasks:           taskStats.Tasks,
	}, nil
}

func (s *Server) getSessionStats(ctx context.Context) (sessionStatsResponse, error) {
	db, err := openReadOnlyDB(ctx, s.agentctlDB)
	if err != nil {
		return sessionStatsResponse{}, fmt.Errorf("open agentctl db: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT
			COALESCE(NULLIF(status, ''), 'unknown'),
			COALESCE(NULLIF(agent, ''), 'unknown'),
			COALESCE(NULLIF(repository, ''), 'unknown'),
			COUNT(*)
		FROM sessions
		GROUP BY 1, 2, 3
		ORDER BY 1, 2, 3
	`)
	if err != nil {
		return sessionStatsResponse{}, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	response := sessionStatsResponse{
		Sessions: make([]sessionStat, 0),
	}

	for rows.Next() {
		var stat sessionStat
		if err := rows.Scan(&stat.Status, &stat.Agent, &stat.Repository, &stat.Count); err != nil {
			return sessionStatsResponse{}, fmt.Errorf("scan sessions: %w", err)
		}
		response.Sessions = append(response.Sessions, stat)
	}

	if err := rows.Err(); err != nil {
		return sessionStatsResponse{}, fmt.Errorf("iterate sessions: %w", err)
	}

	return response, nil
}

func (s *Server) getTokenUsage(ctx context.Context) (tokenUsageResponse, error) {
	codexUsage, err := s.getCodexTokenUsage(ctx)
	if err != nil {
		return tokenUsageResponse{}, err
	}

	actionBurn, err := s.getTotalActionBurn(ctx)
	if err != nil {
		return tokenUsageResponse{}, err
	}

	return tokenUsageResponse{
		Codex:           codexUsage,
		TotalActionBurn: actionBurn,
	}, nil
}

func (s *Server) getCodexTokenUsage(ctx context.Context) (map[string]int64, error) {
	db, err := openReadOnlyDB(ctx, s.codexDB)
	if err != nil {
		return nil, fmt.Errorf("open codex db: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT
			COALESCE(NULLIF(model, ''), 'unknown'),
			COALESCE(SUM(CASE WHEN tokens_used < 0 THEN 0 ELSE tokens_used END), 0)
		FROM threads
		GROUP BY 1
		ORDER BY 1
	`)
	if err != nil {
		return nil, fmt.Errorf("query codex token usage: %w", err)
	}
	defer rows.Close()

	usage := make(map[string]int64)
	for rows.Next() {
		var (
			model string
			total int64
		)
		if err := rows.Scan(&model, &total); err != nil {
			return nil, fmt.Errorf("scan codex token usage: %w", err)
		}
		usage[normalizeLabel(model)] = total
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate codex token usage: %w", err)
	}

	return usage, nil
}

func (s *Server) getTotalActionBurn(ctx context.Context) (int64, error) {
	db, err := openReadOnlyDB(ctx, s.agentctlDB)
	if err != nil {
		return 0, fmt.Errorf("open agentctl db: %w", err)
	}
	defer db.Close()

	var total int64
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN token_burn < 0 THEN 0 ELSE token_burn END), 0)
		FROM actions
	`).Scan(&total); err != nil {
		return 0, fmt.Errorf("query total action burn: %w", err)
	}

	return total, nil
}

func (s *Server) getTaskStats(ctx context.Context) (taskStatsResponse, error) {
	db, err := openReadOnlyDB(ctx, s.agentctlDB)
	if err != nil {
		return taskStatsResponse{}, fmt.Errorf("open agentctl db: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT
			COALESCE(NULLIF(status, ''), 'unknown'),
			COUNT(*)
		FROM tasks
		GROUP BY 1
		ORDER BY 1
	`)
	if err != nil {
		return taskStatsResponse{}, fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()

	response := taskStatsResponse{
		Tasks: make([]taskStat, 0),
	}

	for rows.Next() {
		var stat taskStat
		if err := rows.Scan(&stat.Status, &stat.Count); err != nil {
			return taskStatsResponse{}, fmt.Errorf("scan tasks: %w", err)
		}
		response.Tasks = append(response.Tasks, stat)
	}

	if err := rows.Err(); err != nil {
		return taskStatsResponse{}, fmt.Errorf("iterate tasks: %w", err)
	}

	return response, nil
}

func (s *Server) writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, rpcResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(id),
		Result:  result,
	})
}

func (s *Server) writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeJSON(w, http.StatusOK, rpcResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(id),
		Error: &rpcErrorObject{
			Code:    code,
			Message: message,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func toolDefinitions() []toolDefinition {
	return []toolDefinition{
		{
			Name:        "get_metrics_summary",
			Description: "Get a snapshot of all current metrics",
			InputSchema: emptyInputSchema(),
		},
		{
			Name:        "get_session_stats",
			Description: "Get session counts grouped by status, agent, and repository",
			InputSchema: emptyInputSchema(),
		},
		{
			Name:        "get_token_usage",
			Description: "Get token consumption breakdown by model and agent type",
			InputSchema: emptyInputSchema(),
		},
		{
			Name:        "get_task_stats",
			Description: "Get task success rate and counts by status",
			InputSchema: emptyInputSchema(),
		},
	}
}

func emptyInputSchema() inputSchema {
	return inputSchema{
		Type:       "object",
		Properties: map[string]any{},
	}
}

func normalizeID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}

	return id
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

func normalizeLabel(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "unknown"
	}

	return trimmed
}

func newToolCallError(code int, message string) error {
	return &toolCallError{
		code:    code,
		message: message,
	}
}

func (e *toolCallError) Error() string {
	return e.message
}
