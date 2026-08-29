package qas

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

var supportedMCPVersions = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

const defaultMCPVersion = "2025-06-18"

type mcpSession struct {
	Version     string
	Initialized bool
	LastSeen    time.Time
}

type MCPService struct {
	app      *App
	mu       sync.Mutex
	sessions map[string]*mcpSession
}

type mcpProtocolError struct {
	Code    int
	Message string
	Data    any
}

func NewMCPService(app *App) *MCPService {
	return &MCPService{app: app, sessions: map[string]*mcpSession{}}
}

func (m *MCPService) Enabled() bool {
	return asBoolDefault(m.app.store.MCP()["enabled"], false)
}

func (m *MCPService) VerifyToken(token string) bool {
	return verifyAPIKey(token, asString(m.app.store.MCP()["api_key_hash"]))
}

func (m *MCPService) newSession(version string) string {
	id := newID()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	m.sessions[id] = &mcpSession{Version: version, LastSeen: time.Now()}
	return id
}

func (m *MCPService) expireLocked() {
	deadline := time.Now().Add(-24 * time.Hour)
	for id, session := range m.sessions {
		if session.LastSeen.Before(deadline) {
			delete(m.sessions, id)
		}
	}
}

func (m *MCPService) validateSession(id, version string) (*mcpSession, error) {
	if id == "" {
		return nil, errors.New("Mcp-Session-Id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	session := m.sessions[id]
	if session == nil {
		return nil, errors.New("Unknown MCP session")
	}
	if version != "" && version != session.Version {
		return nil, errors.New("MCP-Protocol-Version does not match the session")
	}
	session.LastSeen = time.Now()
	return &mcpSession{Version: session.Version, Initialized: session.Initialized, LastSeen: session.LastSeen}, nil
}

func (m *MCPService) closeSession(id string) error {
	if id == "" {
		return errors.New("Mcp-Session-Id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[id]; !ok {
		return errors.New("Unknown MCP session")
	}
	delete(m.sessions, id)
	return nil
}

func (m *MCPService) setInitialized(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if session := m.sessions[id]; session != nil {
		session.Initialized = true
		session.LastSeen = time.Now()
	}
}

func (m *MCPService) guard(w http.ResponseWriter, r *http.Request) bool {
	if !m.Enabled() {
		mcpHTTPError(w, r, http.StatusNotFound, "MCP is disabled")
		return true
	}
	if !m.originAllowed(r) {
		mcpHTTPError(w, r, http.StatusForbidden, "Origin is not allowed")
		return true
	}
	if !m.rateAllowed(r) {
		mcpHTTPError(w, r, http.StatusTooManyRequests, "MCP rate limit exceeded")
		return true
	}
	if r.Method == http.MethodOptions {
		mcpCORS(w, r)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Session-Id, MCP-Protocol-Version, X-API-Key")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if !m.VerifyToken(mcpToken(r)) {
		mcpCORS(w, r)
		w.Header().Set("WWW-Authenticate", "Bearer")
		mcpHTTPError(w, r, http.StatusUnauthorized, "Invalid MCP API key")
		return true
	}
	return false
}

func (m *MCPService) originAllowed(r *http.Request) bool {
	origin := strings.TrimRight(r.Header.Get("Origin"), "/")
	if origin == "" {
		return true
	}
	for _, configured := range strings.Split(osEnv("MCP_ALLOWED_ORIGINS", ""), ",") {
		if strings.TrimRight(strings.TrimSpace(configured), "/") == origin {
			return true
		}
	}
	return false
}

func (m *MCPService) rateAllowed(r *http.Request) bool {
	key := r.RemoteAddr
	if key == "" {
		key = "unknown"
	}
	now := time.Now()
	m.app.rateMu.Lock()
	defer m.app.rateMu.Unlock()
	bucket := m.app.rateBuckets[key]
	cutoff := now.Add(-time.Minute)
	kept := bucket[:0]
	for _, stamp := range bucket {
		if stamp.After(cutoff) {
			kept = append(kept, stamp)
		}
	}
	if len(kept) >= 120 {
		m.app.rateBuckets[key] = kept
		return false
	}
	m.app.rateBuckets[key] = append(kept, now)
	if len(m.app.rateBuckets) > 1024 {
		for address, stamps := range m.app.rateBuckets {
			if len(stamps) == 0 || stamps[len(stamps)-1].Before(cutoff) {
				delete(m.app.rateBuckets, address)
			}
		}
	}
	return true
}

func mcpToken(r *http.Request) string {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

func mcpCORS(w http.ResponseWriter, r *http.Request) {
	origin := strings.TrimRight(r.Header.Get("Origin"), "/")
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "false")
		w.Header().Add("Vary", "Origin")
	}
}

func mcpHTTPError(w http.ResponseWriter, r *http.Request, status int, message string) {
	mcpCORS(w, r)
	writeJSON(w, status, map[string]any{"error": message})
}

func (a *App) handleMCP(w http.ResponseWriter, r *http.Request) {
	if a.mcp.guard(w, r) {
		return
	}
	sessionID := r.Header.Get("Mcp-Session-Id")
	protocol := r.Header.Get("MCP-Protocol-Version")
	switch r.Method {
	case http.MethodGet:
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			mcpHTTPError(w, r, http.StatusMethodNotAllowed, "MCP GET requires text/event-stream")
			return
		}
		var session *mcpSession
		var err error
		if sessionID != "" {
			session, err = a.mcp.validateSession(sessionID, protocol)
			if err != nil {
				mcpHTTPError(w, r, http.StatusNotFound, err.Error())
				return
			}
		}
		mcpCORS(w, r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		if session != nil {
			w.Header().Set("MCP-Protocol-Version", session.Version)
		}
		_, _ = io.WriteString(w, ": qas-mcp\n\n")
		return
	case http.MethodDelete:
		if err := a.mcp.closeSession(sessionID); err != nil {
			mcpHTTPError(w, r, http.StatusNotFound, err.Error())
			return
		}
		mcpCORS(w, r)
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
		// handled below
	default:
		w.Header().Set("Allow", "GET, POST, DELETE, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	payload, err := decodeObject(w, r)
	if err != nil {
		mcpHTTPError(w, r, http.StatusBadRequest, "Request body must be a JSON-RPC object")
		return
	}
	response, newSession, negotiated, transportErr := a.mcp.dispatch(payload, sessionID, "http", protocol, false)
	if transportErr != nil {
		mcpHTTPError(w, r, transportErr.status, transportErr.message)
		return
	}
	mcpCORS(w, r)
	if response == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("MCP-Protocol-Version", firstNonEmpty(negotiated, protocol, defaultMCPVersion))
	if newSession != "" {
		w.Header().Set("Mcp-Session-Id", newSession)
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") && !strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "text/event-stream")
		encoded, _ := json.Marshal(response)
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", encoded)
		return
	}
	writeJSONWithoutStatus(w, response)
}

type mcpTransportError struct {
	status  int
	message string
}

func (a *App) handleLegacySSE(w http.ResponseWriter, r *http.Request) {
	if a.mcp.guard(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		mcpHTTPError(w, r, http.StatusMethodNotAllowed, "MCP SSE requires text/event-stream")
		return
	}
	sessionID := a.mcp.newSession("2024-11-05")
	stream := make(chan map[string]any, 16)
	a.legacyMu.Lock()
	a.legacy[sessionID] = stream
	a.legacyMu.Unlock()
	defer func() {
		a.legacyMu.Lock()
		if current := a.legacy[sessionID]; current == stream {
			delete(a.legacy, sessionID)
			// Keep the channel alive for any in-flight sender; the mapping deletion
			// below is sufficient cleanup and avoids a send-on-closed panic.
		}
		a.legacyMu.Unlock()
		_ = a.mcp.closeSession(sessionID)
	}()
	baseURL := strings.TrimRight(osEnv("MCP_PUBLIC_ORIGIN", ""), "/")
	if baseURL == "" {
		baseURL = "http://" + r.Host
	}
	endpoint := baseURL + "/mcp/messages?sessionId=" + sessionID
	mcpCORS(w, r)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpoint)
	if flusher != nil {
		flusher.Flush()
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case message, ok := <-stream:
			if !ok {
				return
			}
			encoded, _ := json.Marshal(message)
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", encoded)
			if flusher != nil {
				flusher.Flush()
			}
		case <-ticker.C:
			_, _ = io.WriteString(w, ": keepalive\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func (a *App) handleLegacyMessages(w http.ResponseWriter, r *http.Request) {
	if a.mcp.guard(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		mcpHTTPError(w, r, http.StatusBadRequest, "sessionId is required")
		return
	}
	payload, err := decodeObject(w, r)
	if err != nil {
		mcpHTTPError(w, r, http.StatusBadRequest, "Request body must be a single JSON-RPC object")
		return
	}
	if _, err := a.mcp.validateSession(sessionID, "2024-11-05"); err != nil {
		mcpHTTPError(w, r, http.StatusNotFound, err.Error())
		return
	}
	response, _, _, transportErr := a.mcp.dispatch(payload, sessionID, "http", "2024-11-05", true)
	if transportErr != nil {
		mcpHTTPError(w, r, transportErr.status, transportErr.message)
		return
	}
	if response != nil {
		a.legacyMu.Lock()
		stream := a.legacy[sessionID]
		if stream == nil {
			a.legacyMu.Unlock()
			mcpHTTPError(w, r, http.StatusNotFound, "MCP SSE stream is not connected")
			return
		}
		queueFull := false
		select {
		case stream <- response:
		default:
			queueFull = true
		}
		a.legacyMu.Unlock()
		if queueFull {
			mcpHTTPError(w, r, http.StatusTooManyRequests, "MCP SSE queue is full")
			return
		}
	}
	mcpCORS(w, r)
	w.WriteHeader(http.StatusAccepted)
}

func (m *MCPService) dispatch(message map[string]any, sessionID, transport, protocol string, allowExistingInitialize bool) (map[string]any, string, string, *mcpTransportError) {
	requestID, hasID := message["id"]
	if asString(message["jsonrpc"]) != "2.0" {
		return mcpErrorResponse(requestID, true, -32600, "Invalid Request", nil), sessionID, protocol, nil
	}
	method := asString(message["method"])
	_, hasResult := message["result"]
	_, hasError := message["error"]
	if method == "" && (hasResult || hasError) {
		if transport == "http" {
			if _, err := m.validateSession(sessionID, protocol); err != nil {
				return nil, sessionID, protocol, &mcpTransportError{status: http.StatusBadRequest, message: err.Error()}
			}
		}
		return nil, sessionID, protocol, nil
	}
	if method == "" {
		if hasID {
			return mcpErrorResponse(requestID, true, -32600, "Invalid Request", nil), sessionID, protocol, nil
		}
		return nil, sessionID, protocol, nil
	}
	params := mapValue(message["params"])
	if raw, exists := message["params"]; exists && raw != nil {
		if _, ok := message["params"].(map[string]any); !ok {
			return mcpErrorResponse(requestID, hasID, -32602, "params must be an object", nil), sessionID, protocol, nil
		}
	}
	if method == "initialize" {
		if transport == "http" && sessionID != "" {
			if !allowExistingInitialize {
				return nil, sessionID, protocol, &mcpTransportError{status: http.StatusBadRequest, message: "initialize must not include a session"}
			}
			session, err := m.validateSession(sessionID, protocol)
			if err != nil {
				return nil, sessionID, protocol, &mcpTransportError{status: http.StatusBadRequest, message: err.Error()}
			}
			version, err := negotiateMCPVersion(params)
			if err != nil || version != session.Version {
				return nil, sessionID, protocol, &mcpTransportError{status: http.StatusBadRequest, message: "initialize protocol version does not match the session"}
			}
			if !hasID {
				return nil, sessionID, version, nil
			}
			return initializeResult(requestID, version), sessionID, version, nil
		}
		if !hasID {
			return nil, sessionID, protocol, nil
		}
		version, err := negotiateMCPVersion(params)
		if err != nil {
			return mcpErrorResponse(requestID, true, -32602, err.Error(), nil), sessionID, protocol, nil
		}
		newID := m.newSession(version)
		return initializeResult(requestID, version), newID, version, nil
	}
	if transport == "http" {
		session, err := m.validateSession(sessionID, protocol)
		if err != nil {
			return nil, sessionID, protocol, &mcpTransportError{status: http.StatusBadRequest, message: err.Error()}
		}
		if protocol != "" && !supportedMCPVersions[protocol] {
			return nil, sessionID, protocol, &mcpTransportError{status: http.StatusBadRequest, message: "Unsupported MCP-Protocol-Version"}
		}
		protocol = session.Version
	} else if protocol != "" && !supportedMCPVersions[protocol] {
		return nil, sessionID, protocol, &mcpTransportError{status: http.StatusBadRequest, message: "Unsupported MCP protocol version"}
	}
	if method == "notifications/initialized" || method == "notifications/cancelled" {
		if sessionID != "" {
			m.setInitialized(sessionID)
		}
		return nil, sessionID, protocol, nil
	}
	var response map[string]any
	switch method {
	case "ping":
		response = resultResponse(requestID, map[string]any{})
	case "tools/list":
		response = resultResponse(requestID, map[string]any{"tools": m.toolList()})
	case "tools/call":
		response = m.callTool(requestID, params)
	default:
		if !hasID {
			return nil, sessionID, protocol, nil
		}
		response = mcpErrorResponse(requestID, true, -32601, "Method not found: "+method, nil)
	}
	if !hasID {
		return nil, sessionID, protocol, nil
	}
	return response, sessionID, protocol, nil
}

func negotiateMCPVersion(params map[string]any) (string, error) {
	requested := asString(params["protocolVersion"])
	if requested == "" {
		return "", errors.New("protocolVersion is required")
	}
	if supportedMCPVersions[requested] {
		return requested, nil
	}
	return defaultMCPVersion, nil
}

func initializeResult(id any, version string) map[string]any {
	return resultResponse(id, map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "quark-auto-save", "version": "1.0.0"},
		"instructions":    "Use qas_tasks_* for task management, qas_logs_query for runtime logs, and qas_search_tv for resource search. Destructive tools are permission-gated.",
	})
}

func resultResponse(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func mcpErrorResponse(id any, hasID bool, code int, message string, data any) map[string]any {
	if !hasID {
		return nil
	}
	value := map[string]any{"code": code, "message": message}
	if data != nil {
		value["data"] = data
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": value}
}

func (m *MCPService) permissions() map[string]any {
	return mapValue(m.app.store.MCP()["permissions"])
}

type mcpTool struct {
	name        string
	title       string
	description string
	scope       string
	schema      map[string]any
}

func selectorSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"task_id":   map[string]any{"type": "string"},
		"task_name": map[string]any{"type": "string"},
		"index":     map[string]any{"type": "integer", "minimum": 0},
	}}
}

func allMCPTools() []mcpTool {
	taskFields := map[string]any{"taskname": map[string]any{"type": "string"}, "shareurl": map[string]any{"type": "string"}, "savepath": map[string]any{"type": "string"}, "pattern": map[string]any{"type": "string"}, "replace": map[string]any{"type": "string"}, "enddate": map[string]any{"type": "string"}, "runweek": map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 1, "maximum": 7}}, "addition": map[string]any{"type": "object"}, "ignore_extension": map[string]any{"type": "boolean"}, "update_subdir": map[string]any{"type": "string"}, "update_subdir_resave_mode": map[string]any{"type": "boolean"}, "startfid": map[string]any{"type": []any{"string", "integer"}}}
	object := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	selector := selectorSchema()["properties"].(map[string]any)
	tools := []mcpTool{
		{"qas_tasks_list", "列出 QAS 任务", "列出所有自动转存任务及其稳定 task_id。", "tasks.read", object(map[string]any{})},
		{"qas_tasks_get", "读取 QAS 任务", "按 task_id、任务名称或列表下标读取一个任务。", "tasks.read", object(selector)},
		{"qas_tasks_create", "创建 QAS 任务", "创建自动转存任务。", "tasks.create", object(map[string]any{"task": map[string]any{"type": "object", "properties": taskFields, "required": []string{"taskname", "shareurl", "savepath"}, "additionalProperties": false}}, "task")},
		{"qas_tasks_update", "修改 QAS 任务", "按稳定 task_id、任务名称或下标部分更新任务。", "tasks.update", object(map[string]any{"task_id": selector["task_id"], "task_name": selector["task_name"], "index": selector["index"], "patch": map[string]any{"type": "object", "properties": taskFields, "additionalProperties": false}}, "patch")},
		{"qas_tasks_delete", "删除 QAS 任务", "删除一个自动转存任务。", "tasks.delete", object(selector)},
		{"qas_tasks_run", "运行 QAS 任务", "异步运行全部任务或指定任务，并返回 run_id。", "tasks.run", object(map[string]any{"task_id": selector["task_id"], "task_name": selector["task_name"], "index": selector["index"], "wait": map[string]any{"type": "boolean"}})},
		{"qas_run_status", "查询运行状态", "查询运行实例状态和日志摘要。", "logs.read", object(map[string]any{"run_id": map[string]any{"type": "string"}}, "run_id")},
		{"qas_logs_query", "查询 QAS 日志", "查询内存日志环形缓冲区。", "logs.read", object(map[string]any{"query": map[string]any{"type": "string"}, "level": map[string]any{"type": "string", "enum": []string{"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"}}, "run_id": map[string]any{"type": "string"}, "cursor": map[string]any{"type": "integer", "minimum": 0}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 200}})},
		{"qas_search_tv", "搜索电视剧或资源", "调用已配置的 CloudSaver、PanSou 搜索源。", "search.read", object(map[string]any{"name": map[string]any{"type": "string", "minLength": 1}, "deep": map[string]any{"type": "boolean"}}, "name")},
		{"qas_files_list", "列出夸克文件", "按保存路径或 fid 查看夸克目录。", "files.read", object(map[string]any{"path": map[string]any{"type": "string"}, "fid": map[string]any{"type": []any{"string", "integer"}}})},
		{"qas_share_inspect", "检查分享内容", "读取夸克分享链接的目录与文件详情。", "files.read", object(map[string]any{"shareurl": map[string]any{"type": "string", "minLength": 1}, "stoken": map[string]any{"type": "string"}}, "shareurl")},
		{"qas_files_delete", "删除夸克文件", "按 fid 或路径删除夸克文件/目录。", "files.write", object(map[string]any{"fid": map[string]any{"type": []any{"string", "integer"}}, "path": map[string]any{"type": "string"}})},
		{"qas_files_rename", "重命名夸克文件", "按 fid 或路径重命名夸克文件/目录。", "files.write", object(map[string]any{"fid": map[string]any{"type": []any{"string", "integer"}}, "path": map[string]any{"type": "string"}, "file_name": map[string]any{"type": "string", "minLength": 1}}, "file_name")},
		{"qas_config_get", "读取脱敏配置", "读取不含敏感字段的配置摘要。", "config.read", object(map[string]any{})},
		{"qas_system_status", "读取系统状态", "读取 QAS 版本、任务数量、调度器和 MCP 状态。", "tasks.read", object(map[string]any{})},
	}
	return tools
}

func (m *MCPService) toolList() []map[string]any {
	permissions := m.permissions()
	result := []map[string]any{}
	for _, tool := range allMCPTools() {
		if !asBoolDefault(permissions[tool.scope], false) {
			continue
		}
		result = append(result, map[string]any{
			"name": tool.name, "title": tool.title, "description": tool.description, "inputSchema": tool.schema,
			"annotations": map[string]any{"readOnlyHint": strings.HasSuffix(tool.scope, ".read"), "destructiveHint": tool.scope == "tasks.delete" || tool.scope == "files.write" || tool.scope == "tasks.run", "openWorldHint": tool.scope == "search.read"},
		})
	}
	return result
}

func (m *MCPService) callTool(id any, params map[string]any) map[string]any {
	name := asString(params["name"])
	args := mapValue(params["arguments"])
	if name == "" {
		return mcpErrorResponse(id, true, -32602, "Tool name is required", nil)
	}
	var definition *mcpTool
	for _, candidate := range allMCPTools() {
		if candidate.name == name {
			copy := candidate
			definition = &copy
			break
		}
	}
	if definition == nil || !asBoolDefault(m.permissions()[definition.scope], false) {
		return mcpErrorResponse(id, true, -32601, "Unknown tool: "+name, nil)
	}
	if err := validateMCPValue(args, definition.schema, "arguments"); err != nil {
		return mcpErrorResponse(id, true, -32602, err.Error(), nil)
	}
	data, err := m.backendCall(name, args)
	if err != nil {
		return mcpErrorResponse(id, true, -32602, redactText(err), nil)
	}
	encoded, _ := json.Marshal(data)
	structured := data
	if structured == nil {
		structured = map[string]any{}
	}
	isError := false
	if object := mapValue(structured); object["success"] != nil {
		isError = !asBoolDefault(object["success"], false)
	}
	return resultResponse(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": string(encoded)}}, "isError": isError, "structuredContent": structured})
}

func validateMCPValue(value any, schema map[string]any, path string) error {
	expected := schema["type"]
	match := func(kind string) bool {
		switch kind {
		case "object":
			_, ok := value.(map[string]any)
			return ok
		case "array":
			_, ok := value.([]any)
			return ok
		case "string":
			_, ok := value.(string)
			return ok
		case "integer":
			_, ok := value.(float64)
			return ok && number(value) == float64(int(number(value)))
		case "boolean":
			_, ok := value.(bool)
			return ok
		default:
			return true
		}
	}
	matched := false
	if kinds, ok := expected.([]any); ok {
		for _, raw := range kinds {
			if match(asString(raw)) {
				matched = true
			}
		}
	} else if expected != nil {
		matched = match(asString(expected))
	}
	if expected != nil && !matched {
		return fmt.Errorf("%s has an invalid type", path)
	}
	if enum := schema["enum"]; enum != nil && !mcpEnumContains(enum, value) {
		return fmt.Errorf("%s has an invalid value", path)
	}
	if stringValue, ok := value.(string); ok {
		if minimum := int(number(schema["minLength"])); minimum > 0 && len([]rune(stringValue)) < minimum {
			return fmt.Errorf("%s is too short", path)
		}
	}
	if numberValue, ok := value.(float64); ok {
		if schema["minimum"] != nil && numberValue < number(schema["minimum"]) {
			return fmt.Errorf("%s is below the minimum", path)
		}
		if schema["maximum"] != nil && numberValue > number(schema["maximum"]) {
			return fmt.Errorf("%s is above the maximum", path)
		}
	}
	if items, ok := value.([]any); ok && schema["items"] != nil {
		for index, item := range items {
			if err := validateMCPValue(item, mapValue(schema["items"]), fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	}
	if object, ok := value.(map[string]any); ok {
		properties := mapValue(schema["properties"])
		for _, raw := range stringSlice(schema["required"]) {
			if _, exists := object[raw]; !exists {
				return fmt.Errorf("Missing required argument: %s.%s", path, raw)
			}
		}
		if schema["additionalProperties"] == false {
			for name := range object {
				if _, exists := properties[name]; !exists {
					return fmt.Errorf("Unknown argument(s): %s", name)
				}
			}
		}
		for name, child := range object {
			if property, exists := properties[name]; exists {
				if err := validateMCPValue(child, mapValue(property), path+"."+name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func mcpEnumContains(enum any, value any) bool {
	switch items := enum.(type) {
	case []string:
		for _, item := range items {
			if item == asString(value) {
				return true
			}
		}
	case []any:
		for _, item := range items {
			if fmt.Sprint(item) == fmt.Sprint(value) {
				return true
			}
		}
	}
	return false
}

func (m *MCPService) backendCall(name string, args map[string]any) (any, error) {
	switch name {
	case "qas_tasks_list":
		tasks := m.app.store.Tasks()
		output := make([]map[string]any, 0, len(tasks))
		for index, task := range tasks {
			value := redactData(task).(map[string]any)
			value["index"] = index
			output = append(output, value)
		}
		return map[string]any{"success": true, "tasks": output, "count": len(output)}, nil
	case "qas_tasks_get":
		value, err := m.app.store.ResolveTaskPublic(args)
		if err != nil {
			return nil, err
		}
		return map[string]any{"success": true, "task": value}, nil
	case "qas_tasks_create":
		task := mapValue(args["task"])
		if err := validateTaskShare(task); err != nil {
			return nil, err
		}
		task = cloneMap(task)
		if task["runweek"] == nil {
			task["runweek"] = []any{float64(1), float64(2), float64(3), float64(4), float64(5), float64(6), float64(7)}
		}
		if task["ignore_extension"] == nil {
			task["ignore_extension"] = false
		}
		if task["update_subdir_resave_mode"] == nil {
			task["update_subdir_resave_mode"] = false
		}
		if task["startfid"] == nil {
			task["startfid"] = ""
		}
		if task["addition"] == nil {
			task["addition"] = map[string]any{}
		}
		value, err := m.app.store.AddTask(task)
		if err != nil {
			return nil, err
		}
		return map[string]any{"success": true, "task": redactData(value)}, nil
	case "qas_tasks_update":
		index, err := m.app.store.ResolveTask(args)
		if err != nil {
			return nil, err
		}
		patch := mapValue(args["patch"])
		if err := validateTaskPatch(patch); err != nil {
			return nil, err
		}
		value, err := m.app.store.UpdateTask(index, patch)
		if err != nil {
			return nil, err
		}
		return map[string]any{"success": true, "task": redactData(value)}, nil
	case "qas_tasks_delete":
		index, err := m.app.store.ResolveTask(args)
		if err != nil {
			return nil, err
		}
		value, err := m.app.store.DeleteTask(index)
		if err != nil {
			return nil, err
		}
		return map[string]any{"success": true, "task": redactData(value)}, nil
	case "qas_tasks_run":
		tasks := m.app.store.Tasks()
		hasSelector := hasTaskSelector(args)
		if hasSelector {
			index, err := m.app.store.ResolveTask(args)
			if err != nil {
				return nil, err
			}
			tasks = []map[string]any{tasks[index]}
		}
		result, err := m.app.runs.Start(context.Background(), tasks)
		if err != nil {
			return nil, err
		}
		if asBoolDefault(args["wait"], false) {
			return m.app.runs.Wait(context.Background(), asString(result["run_id"]))
		}
		return result, nil
	case "qas_run_status":
		return m.app.runs.Status(asString(args["run_id"]))
	case "qas_logs_query":
		result := m.app.logs.Query(asString(args["query"]), asString(args["level"]), asString(args["run_id"]), int64(number(args["cursor"])), int(number(args["limit"])))
		result["success"] = true
		return result, nil
	case "qas_search_tv":
		items, err := searchSuggestions(context.Background(), mapValue(m.app.store.Snapshot()["source"]), asString(args["name"]), asBoolDefault(args["deep"], false))
		return map[string]any{"success": err == nil, "data": items, "query": asString(args["name"])}, nil
	case "qas_files_list":
		cookies := m.app.store.Cookies()
		if len(cookies) == 0 {
			return nil, errors.New("未配置夸克 Cookie")
		}
		data, err := fileList(context.Background(), cookies[0], args["fid"], asString(args["path"]))
		if err != nil {
			return nil, err
		}
		return map[string]any{"success": true, "data": data}, nil
	case "qas_share_inspect":
		data, err := shareDetail(context.Background(), asString(args["shareurl"]), asString(args["stoken"]))
		if err != nil {
			return nil, err
		}
		return map[string]any{"success": true, "data": data}, nil
	case "qas_files_delete":
		cookies := m.app.store.Cookies()
		if len(cookies) == 0 {
			return nil, errors.New("未配置夸克 Cookie")
		}
		fid := args["fid"]
		var err error
		if fid == nil || asString(fid) == "" {
			fid, err = pathToFID(context.Background(), cookies[0], asString(args["path"]))
			if err != nil {
				return nil, err
			}
		}
		result, err := NewQuarkClient(cookies[0]).delete(context.Background(), []any{fid})
		if err == nil {
			result["success"] = number(result["code"]) == 0
		}
		return result, err
	case "qas_files_rename":
		cookies := m.app.store.Cookies()
		if len(cookies) == 0 {
			return nil, errors.New("未配置夸克 Cookie")
		}
		fid := args["fid"]
		var err error
		if fid == nil || asString(fid) == "" {
			fid, err = pathToFID(context.Background(), cookies[0], asString(args["path"]))
			if err != nil {
				return nil, err
			}
		}
		result, err := NewQuarkClient(cookies[0]).rename(context.Background(), fid, asString(args["file_name"]))
		if err == nil {
			result["success"] = number(result["code"]) == 0
		}
		return result, err
	case "qas_config_get":
		return map[string]any{"success": true, "config": redactData(m.app.store.Snapshot())}, nil
	case "qas_system_status":
		mcp := m.app.store.MCP()
		return map[string]any{"success": true, "version": m.app.version, "task_count": len(m.app.store.Tasks()), "scheduler": m.app.scheduler.State(), "mcp": map[string]any{"enabled": mcp["enabled"], "api_key_configured": asString(mcp["api_key_hash"]) != "", "permissions": mcp["permissions"]}}, nil
	default:
		return nil, errors.New("未知工具: " + name)
	}
}

func hasTaskSelector(args map[string]any) bool {
	for _, key := range []string{"task_id", "task_name"} {
		if strings.TrimSpace(asString(args[key])) != "" {
			return true
		}
	}
	return args["index"] != nil
}

func validateTaskPatch(patch map[string]any) error {
	for key := range patch {
		switch key {
		case "taskname", "shareurl", "savepath", "pattern", "replace", "enddate", "runweek", "addition", "ignore_extension", "update_subdir", "update_subdir_resave_mode", "startfid":
		default:
			return fmt.Errorf("未知任务字段: %s", key)
		}
	}
	if share := asString(patch["shareurl"]); share != "" {
		_, _, _, _, err := extractShareURL(share)
		if err != nil {
			return err
		}
	}
	if savepath := asString(patch["savepath"]); savepath != "" && !strings.HasPrefix(savepath, "/") {
		return errors.New("savepath 必须以 / 开头")
	}
	return nil
}

func (a *App) RunStdio(reader io.Reader, writer io.Writer, token string) error {
	if !a.mcp.Enabled() || !a.mcp.VerifyToken(token) {
		return errors.New("MCP stdio disabled or invalid QAS_MCP_API_KEY")
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			encoded, _ := json.Marshal(mcpErrorResponse(nil, true, -32700, "Parse error", nil))
			_, _ = fmt.Fprintln(writer, string(encoded))
			continue
		}
		response, _, _, _ := a.mcp.dispatch(payload, "", "stdio", "", false)
		if response != nil {
			encoded, _ := json.Marshal(response)
			_, _ = fmt.Fprintln(writer, string(encoded))
		}
	}
	return scanner.Err()
}
