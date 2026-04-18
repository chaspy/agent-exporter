package mcp

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestMCPServerFlow(t *testing.T) {
	t.Parallel()

	agentctlDB, codexDB := createTestDatabases(t)
	server := NewServer(9101, agentctlDB, codexDB)
	testServer := httptest.NewServer(server.Handler)
	defer testServer.Close()

	healthResponse, err := http.Get(testServer.URL + "/health")
	if err != nil {
		t.Fatalf("health request failed: %v", err)
	}
	defer healthResponse.Body.Close()

	var healthPayload map[string]string
	if err := json.NewDecoder(healthResponse.Body).Decode(&healthPayload); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if got := healthPayload["status"]; got != "ok" {
		t.Fatalf("unexpected health status: %q", got)
	}

	initializePayload := rpcResponse{}
	doRPCRequest(t, testServer.URL+"/mcp", rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2024-11-05","capabilities":{}}`),
	}, &initializePayload)

	var initResult initializeResult
	decodeResult(t, initializePayload.Result, &initResult)
	if initResult.ProtocolVersion != mcpProtocolVersion {
		t.Fatalf("unexpected protocol version: %q", initResult.ProtocolVersion)
	}
	if initResult.ServerInfo.Name != mcpServerName {
		t.Fatalf("unexpected server name: %q", initResult.ServerInfo.Name)
	}
	if initResult.ServerInfo.Version != mcpServerVersion {
		t.Fatalf("unexpected server version: %q", initResult.ServerInfo.Version)
	}

	toolsPayload := rpcResponse{}
	doRPCRequest(t, testServer.URL+"/mcp", rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage("2"),
		Method:  "tools/list",
	}, &toolsPayload)

	var listResult toolsListResult
	decodeResult(t, toolsPayload.Result, &listResult)
	if len(listResult.Tools) != 4 {
		t.Fatalf("unexpected tool count: %d", len(listResult.Tools))
	}
	if listResult.Tools[0].Name != "get_metrics_summary" {
		t.Fatalf("unexpected first tool: %q", listResult.Tools[0].Name)
	}

	callPayload := rpcResponse{}
	doRPCRequest(t, testServer.URL+"/mcp", rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage("3"),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"get_metrics_summary","arguments":{}}`),
	}, &callPayload)

	var callResult toolCallResult
	decodeResult(t, callPayload.Result, &callResult)
	if len(callResult.Content) != 1 {
		t.Fatalf("unexpected content count: %d", len(callResult.Content))
	}
	if callResult.Content[0].Type != "text" {
		t.Fatalf("unexpected content type: %q", callResult.Content[0].Type)
	}

	var summary metricsSummaryResponse
	if err := json.Unmarshal([]byte(callResult.Content[0].Text), &summary); err != nil {
		t.Fatalf("decode summary payload: %v", err)
	}
	if len(summary.Sessions) != 2 {
		t.Fatalf("unexpected session count: %d", len(summary.Sessions))
	}
	if got := summary.Codex["gpt-5.4"]; got != 100 {
		t.Fatalf("unexpected codex usage for gpt-5.4: %d", got)
	}
	if got := summary.Codex["unknown"]; got != 50 {
		t.Fatalf("unexpected codex usage for unknown: %d", got)
	}
	if summary.TotalActionBurn != 30 {
		t.Fatalf("unexpected total action burn: %d", summary.TotalActionBurn)
	}
	if len(summary.Tasks) != 2 {
		t.Fatalf("unexpected task count: %d", len(summary.Tasks))
	}
}

func TestMCPServerUnknownTool(t *testing.T) {
	t.Parallel()

	agentctlDB, codexDB := createTestDatabases(t)
	server := NewServer(9101, agentctlDB, codexDB)
	testServer := httptest.NewServer(server.Handler)
	defer testServer.Close()

	response := rpcResponse{}
	doRPCRequest(t, testServer.URL+"/mcp", rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage("9"),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"unknown_tool","arguments":{}}`),
	}, &response)

	if response.Error == nil {
		t.Fatal("expected error response")
	}
	if response.Error.Code != -32602 {
		t.Fatalf("unexpected error code: %d", response.Error.Code)
	}
}

func createTestDatabases(t *testing.T) (string, string) {
	t.Helper()

	tempDir := t.TempDir()
	agentctlDB := filepath.Join(tempDir, "agentctl.sqlite")
	codexDB := filepath.Join(tempDir, "codex.sqlite")

	writeDatabase(t, agentctlDB, []string{
		`CREATE TABLE sessions (status TEXT, agent TEXT, repository TEXT)`,
		`CREATE TABLE tasks (status TEXT)`,
		`CREATE TABLE actions (token_burn INTEGER)`,
		`INSERT INTO sessions(status, agent, repository) VALUES
			('active', 'codex', 'chaspy/agent-exporter'),
			('active', 'codex', 'chaspy/agent-exporter'),
			('idle', 'claude', 'chaspy/myassistant')`,
		`INSERT INTO tasks(status) VALUES ('completed'), ('completed'), ('pending')`,
		`INSERT INTO actions(token_burn) VALUES (10), (20), (-5)`,
	})

	writeDatabase(t, codexDB, []string{
		`CREATE TABLE threads (model TEXT, tokens_used INTEGER)`,
		`INSERT INTO threads(model, tokens_used) VALUES
			('gpt-5.4', 100),
			('', 50),
			('gpt-5.4-mini', -7)`,
	})

	return agentctlDB, codexDB
}

func writeDatabase(t *testing.T, path string, statements []string) {
	t.Helper()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite db %q: %v", path, err)
	}
	defer db.Close()

	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
}

func doRPCRequest(t *testing.T, endpoint string, request rpcRequest, response *rpcResponse) {
	t.Helper()

	body, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	httpResponse, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer httpResponse.Body.Close()

	if err := json.NewDecoder(httpResponse.Body).Decode(response); err != nil {
		t.Fatalf("decode rpc response: %v", err)
	}
}

func decodeResult(t *testing.T, raw any, target any) {
	t.Helper()

	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal intermediate result: %v", err)
	}

	if err := json.Unmarshal(encoded, target); err != nil {
		t.Fatalf("unmarshal intermediate result: %v", err)
	}
}
