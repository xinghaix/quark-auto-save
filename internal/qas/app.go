package qas

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Options struct {
	BuildSHA string
	BuildTag string
}

type App struct {
	store       *ConfigStore
	logs        *LogBuffer
	worker      *Worker
	runs        *RunManager
	scheduler   *Scheduler
	version     string
	host        string
	port        int
	pluginFlags string
	templateDir string
	staticDir   string
	legacyMu    sync.Mutex
	legacy      map[string]chan map[string]any
	rateMu      sync.Mutex
	rateBuckets map[string][]time.Time
	mcp         *MCPService
}

func NewApp(options Options) (*App, error) {
	configPath := envOr("CONFIG_PATH", "./config/quark_config.json")
	templatePath := envOr("CONFIG_TEMPLATE_PATH", "./quark_config.json")
	store, err := NewConfigStore(configPath, templatePath)
	if err != nil {
		return nil, err
	}
	logs := NewLogBuffer(2000)
	worker := NewWorker(configPath, logs)
	app := &App{
		store:       store,
		logs:        logs,
		worker:      worker,
		version:     buildVersion(options.BuildTag, options.BuildSHA),
		host:        envOr("HOST", "0.0.0.0"),
		port:        envInt("PORT", 5005),
		pluginFlags: os.Getenv("PLUGIN_FLAGS"),
		templateDir: envOr("TEMPLATE_DIR", "./app/templates"),
		staticDir:   envOr("STATIC_DIR", "./app/static"),
		legacy:      map[string]chan map[string]any{},
		rateBuckets: map[string][]time.Time{},
	}
	app.runs = NewRunManager(worker, logs)
	app.runs.SetOnComplete(app.reloadAfterWorker)
	app.mcp = NewMCPService(app)
	app.scheduler = NewScheduler(app.runScheduled)
	if err := app.scheduler.Reload(asString(app.store.Snapshot()["crontab"])); err != nil {
		return nil, err
	}
	app.logs.Add("INFO", "", "Go 1.27 backend initialized, version=%s", app.version)
	return app, nil
}

func (a *App) Address() string     { return fmt.Sprintf("%s:%d", a.host, a.port) }
func (a *App) Version() string     { return a.version }
func (a *App) Store() *ConfigStore { return a.store }
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleRoot)
	mux.HandleFunc("/login", a.handleLogin)
	mux.HandleFunc("/logout", a.handleLogout)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(a.staticDir))))
	mux.HandleFunc("/favicon.ico", a.handleFavicon)
	mux.HandleFunc("/mcp", a.handleMCP)
	mux.HandleFunc("/mcp/sse", a.handleLegacySSE)
	mux.HandleFunc("/mcp/messages", a.handleLegacyMessages)
	mux.HandleFunc("/data", a.handleData)
	mux.HandleFunc("/update", a.handleUpdate)
	mux.HandleFunc("/run_script_now", a.handleRunScript)
	mux.HandleFunc("/task_suggestions", a.handleSuggestions)
	mux.HandleFunc("/get_share_detail", a.handleShareDetail)
	mux.HandleFunc("/get_savepath_detail", a.handleSavePathDetail)
	mux.HandleFunc("/delete_file", a.handleDeleteFile)
	mux.HandleFunc("/rename_file", a.handleRenameFile)
	mux.HandleFunc("/api/add_task", a.handleAddTask)
	return loggingMiddleware(mux)
}

func (a *App) Shutdown() {
	if a.scheduler != nil {
		a.scheduler.Shutdown()
	}
	a.legacyMu.Lock()
	// Remove mappings without closing channels that an in-flight POST may be
	// holding; the SSE handler owns and closes its channel on disconnect.
	for id := range a.legacy {
		delete(a.legacy, id)
	}
	a.legacyMu.Unlock()
}

func (a *App) reloadAfterWorker() {
	if err := a.store.Reload(); err != nil {
		a.logs.Add("ERROR", "", "reload config after worker failed: %s", err)
	}
}

func (a *App) runScheduled() {
	if a.runs.Busy() {
		a.logs.Add("WARNING", "", "scheduled run skipped: another task run is active")
		return
	}
	if _, err := a.runs.Start(context.Background(), a.store.Tasks(), true); err != nil {
		a.logs.Add("ERROR", "", "scheduled run failed to start: %s", err)
	}
}

func (a *App) isAuthenticated(r *http.Request) bool {
	username, password := a.store.UsernamePassword()
	expected := loginToken(username, password)
	if token := r.URL.Query().Get("token"); token != "" && constantTimeEqual(token, expected) {
		return true
	}
	cookie, err := r.Cookie("QAS_SESSION")
	return err == nil && constantTimeEqual(cookie.Value, expected)
}

func (a *App) requireLogin(w http.ResponseWriter, r *http.Request) bool {
	if a.isAuthenticated(r) {
		return true
	}
	if r.URL.Path == "/api/add_task" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "code": 1, "message": "未登录"})
	} else {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": "未登录"})
	}
	return false
}

func (a *App) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !a.isAuthenticated(r) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	data, err := os.ReadFile(filepath.Join(a.templateDir, "index.html"))
	if err != nil {
		http.Error(w, "index template unavailable", http.StatusInternalServerError)
		return
	}
	version, _ := json.Marshal(a.version)
	flags, _ := json.Marshal(a.pluginFlags)
	page := strings.ReplaceAll(string(data), "[[ version|tojson ]]", string(version))
	page = strings.ReplaceAll(page, "[[ plugin_flags|tojson ]]", string(flags))
	writeHTML(w, http.StatusOK, []byte(page))
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if a.isAuthenticated(r) {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		a.renderLogin(w, "")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderLogin(w, "登录失败")
		return
	}
	username, password := a.store.UsernamePassword()
	providedUser, providedPassword := r.Form.Get("username"), r.Form.Get("password")
	if constantTimeEqual(providedUser, username) && constantTimeEqual(providedPassword, password) {
		value := loginToken(username, password)
		http.SetCookie(w, &http.Cookie{Name: "QAS_SESSION", Value: value, Path: "/", MaxAge: 31 * 24 * 60 * 60, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		a.logs.Add("INFO", "", "user login succeeded")
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	a.logs.Add("INFO", "", "user login failed")
	a.renderLogin(w, "登录失败")
}

func (a *App) renderLogin(w http.ResponseWriter, message string) {
	data, err := os.ReadFile(filepath.Join(a.templateDir, "login.html"))
	if err != nil {
		http.Error(w, "login template unavailable", http.StatusInternalServerError)
		return
	}
	page := string(data)
	block := regexp.MustCompile(`(?s)\{% if message %\}.*?\{% endif %\}`)
	if message == "" {
		page = block.ReplaceAllString(page, "")
	} else {
		page = strings.ReplaceAll(page, "{% if message %}", "")
		page = strings.ReplaceAll(page, "{% endif %}", "")
		page = strings.ReplaceAll(page, "[[ message ]]", html.EscapeString(message))
	}
	writeHTML(w, http.StatusOK, []byte(page))
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "QAS_SESSION", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (a *App) handleFavicon(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(a.staticDir, "favicon.ico"))
}

func (a *App) handleData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || !a.requireLogin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": a.store.DataForUI()})
}

func (a *App) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !a.requireLogin(w, r) {
		return
	}
	payload, err := decodeObject(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "请求数据无效"})
		return
	}
	if raw, ok := payload["crontab"]; ok {
		if expr := strings.TrimSpace(asString(raw)); expr != "" {
			if _, err := parseCron(expr); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": err.Error()})
				return
			}
		}
	}
	if err := a.store.Update(payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": err.Error()})
		return
	}
	crontab := asString(a.store.Snapshot()["crontab"])
	if err := a.scheduler.Reload(crontab); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": err.Error()})
		return
	}
	if strings.TrimSpace(crontab) == "" {
		a.logs.Add("INFO", "", "configuration updated without a crontab")
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": "配置更新失败"})
		return
	}
	a.logs.Add("INFO", "", "configuration updated")
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "配置更新成功"})
}

func (a *App) handleRunScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !a.requireLogin(w, r) {
		return
	}
	payload, err := decodeObject(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "请求数据无效"})
		return
	}
	tasks := taskMaps(payload["tasklist"])
	test := asBoolDefault(payload["quark_test"], false)
	cookies := stringSlice(payload["cookie"])
	push, _ := payload["push_config"].(map[string]any)
	w.Header().Set("Content-Type", "text/event-stream;charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	writeEvent := func(line string) error {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(line, "\n", " ")); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	if err := a.runs.Stream(r.Context(), tasks, test, cookies, push, writeEvent); err != nil {
		a.logs.Add("ERROR", "", "manual run failed: %s", err)
		_ = writeEvent("运行失败: " + redactText(err))
	}
	_ = writeEvent("[DONE]")
}

func (a *App) handleSuggestions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || !a.requireLogin(w, r) {
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	deep := r.URL.Query().Get("d") == "1" || strings.EqualFold(r.URL.Query().Get("d"), "true")
	source := mapValue(a.store.Snapshot()["source"])
	items, refreshedToken, err := searchSuggestions(r.Context(), source, query, deep)
	if refreshedToken != "" {
		if saveErr := a.store.SetCloudSaverToken(refreshedToken); saveErr != nil {
			a.logs.Add("ERROR", "", "CloudSaver token persistence failed: %s", redactText(saveErr))
		}
	}
	if err != nil && len(items) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": redactText(err), "data": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": items, "query": query, "deep": deep})
}

func (a *App) handleShareDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !a.requireLogin(w, r) {
		return
	}
	payload, err := decodeObject(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "请求数据无效"})
		return
	}
	data, err := shareDetail(r.Context(), asString(payload["shareurl"]), asString(payload["stoken"]))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "data": map[string]any{"error": redactText(err)}})
		return
	}
	if task := mapValue(payload["task"]); len(task) > 0 {
		existing := []map[string]any{}
		if cookies := a.store.Cookies(); len(cookies) > 0 && strings.TrimSpace(asString(task["savepath"])) != "" {
			if listing, listErr := fileList(r.Context(), cookies[0], nil, asString(task["savepath"])); listErr == nil {
				existing = taskMaps(listing["list"])
			} else {
				a.logs.Add("WARNING", "", "share preview target lookup failed: %s", redactText(listErr))
			}
		}
		if previewed, previewErr := a.applySharePreview(r.Context(), data, task, mapValue(payload["magic_regex"]), existing); previewErr == nil {
			data = previewed
		} else {
			a.logs.Add("WARNING", "", "share preview skipped: %s", redactText(previewErr))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": data})
}

func (a *App) handleSavePathDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || !a.requireLogin(w, r) {
		return
	}
	cookies := a.store.Cookies()
	if len(cookies) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "data": map[string]any{"error": "未配置夸克 Cookie"}})
		return
	}
	var fid any
	if raw := r.URL.Query().Get("fid"); raw != "" {
		fid = raw
	}
	data, err := fileList(r.Context(), cookies[0], fid, r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "data": map[string]any{"error": redactText(err)}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": data})
}

func (a *App) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !a.requireLogin(w, r) {
		return
	}
	payload, err := decodeObject(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "请求数据无效"})
		return
	}
	cookies := a.store.Cookies()
	if len(cookies) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": "未配置夸克 Cookie"})
		return
	}
	fid, err := resolveDestructiveFID(r.Context(), cookies[0], payload["fid"], asString(payload["path"]))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": redactText(err)})
		return
	}
	response, err := NewQuarkClient(cookies[0]).delete(r.Context(), []any{fid})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": redactText(err)})
		return
	}
	response["success"] = number(response["code"]) == 0
	writeJSON(w, http.StatusOK, response)
}

func (a *App) handleRenameFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !a.requireLogin(w, r) {
		return
	}
	payload, err := decodeObject(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "请求数据无效"})
		return
	}
	name := asString(payload["file_name"])
	if name == "" {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": "缺失必要字段: fid, file_name"})
		return
	}
	cookies := a.store.Cookies()
	if len(cookies) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": "未配置夸克 Cookie"})
		return
	}
	fid, err := resolveDestructiveFID(r.Context(), cookies[0], payload["fid"], asString(payload["path"]))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": redactText(err)})
		return
	}
	response, err := NewQuarkClient(cookies[0]).rename(r.Context(), fid, name)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": redactText(err)})
		return
	}
	response["success"] = number(response["code"]) == 0
	writeJSON(w, http.StatusOK, response)
}

func (a *App) handleAddTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !a.requireLogin(w, r) {
		return
	}
	payload, err := decodeObject(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "code": 2, "message": "请求数据无效"})
		return
	}
	value, err := a.store.AddTask(payload)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "code": 2, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "code": 0, "message": "任务添加成功", "data": value})
}

func validateTaskShare(task map[string]any) error {
	for _, field := range []string{"taskname", "shareurl", "savepath"} {
		if strings.TrimSpace(asString(task[field])) == "" {
			return fmt.Errorf("缺少必要字段: %s", field)
		}
	}
	if !strings.HasPrefix(asString(task["savepath"]), "/") {
		return errors.New("savepath 必须以 / 开头")
	}
	_, _, _, _, err := extractShareURL(asString(task["shareurl"]))
	return err
}

func decodeObject(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024*1024)
	decoder := json.NewDecoder(r.Body)
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return nil, errors.New("invalid JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("request must contain exactly one JSON object")
	}
	return payload, nil
}
func taskMaps(value any) []map[string]any {
	items, _ := value.([]any)
	result := make([]map[string]any, 0, len(items))
	for _, raw := range items {
		if item, ok := raw.(map[string]any); ok {
			result = append(result, cloneMap(item))
		}
	}
	return result
}

func stringSlice(value any) []string {
	if raw, ok := value.(string); ok && raw != "" {
		return strings.Split(raw, "\n")
	}
	if items, ok := value.([]string); ok {
		return append([]string(nil), items...)
	}
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if value := asString(item); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONWithoutStatus(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}

func writeHTML(w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

func constantTimeEqual(left, right string) bool {
	if left == "" || right == "" || len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func buildVersion(tag, sha string) string {
	tag = firstNonEmpty(tag, os.Getenv("BUILD_TAG"))
	sha = firstNonEmpty(sha, os.Getenv("BUILD_SHA"))
	if strings.HasPrefix(tag, "v") {
		return tag
	}
	if sha != "" {
		if len(sha) > 7 {
			sha = sha[:7]
		}
		return fmt.Sprintf("%s(%s)", tag, sha)
	}
	return "dev"
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func osEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
