package qas

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	projectRoot := filepath.Join(root, "..", "..")
	dir := t.TempDir()
	t.Setenv("CONFIG_PATH", filepath.Join(dir, "config", "quark_config.json"))
	t.Setenv("CONFIG_TEMPLATE_PATH", filepath.Join(projectRoot, "quark_config.json"))
	t.Setenv("TEMPLATE_DIR", filepath.Join(projectRoot, "app", "templates"))
	t.Setenv("STATIC_DIR", filepath.Join(projectRoot, "app", "static"))
	t.Setenv("PORT", "5099")
	app, err := NewApp(Options{BuildTag: "vtest", BuildSHA: "testsha"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Shutdown)
	return app
}

func enableTestMCP(t *testing.T, app *App) string {
	t.Helper()
	key := "test-mcp-key-1234567890"
	err := app.store.Update(map[string]any{
		"mcp": map[string]any{
			"enabled": true,
			"api_key": key,
			"permissions": map[string]any{
				"tasks.read":  true,
				"logs.read":   true,
				"search.read": true,
				"files.read":  true,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func rpc(method string, id int, params map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
}

func postJSON(t *testing.T, client *http.Client, url string, payload any, headers map[string]string) *http.Response {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func postRawJSON(t *testing.T, client *http.Client, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestGoBackendLoginConfigAndMCP(t *testing.T) {
	app := testApp(t)
	key := enableTestMCP(t, app)
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}

	response, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusFound || response.Header.Get("Location") != "/login" {
		t.Fatalf("unauthenticated root: %d %s", response.StatusCode, response.Header.Get("Location"))
	}
	response.Body.Close()

	response, err = client.PostForm(server.URL+"/login", map[string][]string{"username": {"admin"}, "password": {"admin123"}})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusFound {
		t.Fatalf("login status: %d", response.StatusCode)
	}
	response.Body.Close()

	response, err = client.Get(server.URL + "/data")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || strings.Contains(string(data), "api_key_hash") {
		t.Fatalf("unexpected /data response: %d %s", response.StatusCode, data)
	}

	unauthorized := postJSON(t, &http.Client{}, server.URL+"/mcp", rpc("initialize", 1, map[string]any{"protocolVersion": defaultMCPVersion}), map[string]string{})
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized MCP status: %d", unauthorized.StatusCode)
	}
	unauthorized.Body.Close()

	initialized := postJSON(t, client, server.URL+"/mcp", rpc("initialize", 1, map[string]any{"protocolVersion": defaultMCPVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "go-test", "version": "1"}}), map[string]string{"Authorization": "Bearer " + key})
	if initialized.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(initialized.Body)
		initialized.Body.Close()
		t.Fatalf("initialize status: %d %s", initialized.StatusCode, body)
	}
	var initializeBody map[string]any
	if err := json.NewDecoder(initialized.Body).Decode(&initializeBody); err != nil {
		t.Fatal(err)
	}
	initialized.Body.Close()
	sessionID := initialized.Header.Get("Mcp-Session-Id")
	if sessionID == "" || initializeBody["result"] == nil {
		t.Fatalf("missing MCP session/result: %#v", initializeBody)
	}
	if initializeBody["result"].(map[string]any)["serverInfo"].(map[string]any)["version"] != "vtest" {
		t.Fatalf("MCP server version drifted: %#v", initializeBody["result"])
	}

	headers := map[string]string{"Authorization": "Bearer " + key, "Mcp-Session-Id": sessionID, "MCP-Protocol-Version": defaultMCPVersion}
	listed := postJSON(t, client, server.URL+"/mcp", rpc("tools/list", 2, map[string]any{}), headers)
	if listed.StatusCode != http.StatusOK {
		t.Fatalf("tools/list status: %d", listed.StatusCode)
	}
	var listBody map[string]any
	if err := json.NewDecoder(listed.Body).Decode(&listBody); err != nil {
		t.Fatal(err)
	}
	listed.Body.Close()
	encoded, _ := json.Marshal(listBody)
	if !strings.Contains(string(encoded), "qas_tasks_list") || strings.Contains(string(encoded), "qas_tasks_delete") {
		t.Fatalf("unexpected tool permissions: %s", encoded)
	}

	called := postJSON(t, client, server.URL+"/mcp", rpc("tools/call", 3, map[string]any{"name": "qas_tasks_list", "arguments": map[string]any{}}), headers)
	if called.StatusCode != http.StatusOK {
		t.Fatalf("tools/call status: %d", called.StatusCode)
	}
	var callBody map[string]any
	if err := json.NewDecoder(called.Body).Decode(&callBody); err != nil {
		t.Fatal(err)
	}
	called.Body.Close()
	if callBody["result"] == nil || callBody["result"].(map[string]any)["isError"] != false {
		t.Fatalf("unexpected tools/call response: %#v", callBody)
	}

	badLevel := postJSON(t, client, server.URL+"/mcp", rpc("tools/call", 4, map[string]any{"name": "qas_logs_query", "arguments": map[string]any{"level": "TRACE"}}), headers)
	var badLevelBody map[string]any
	if err := json.NewDecoder(badLevel.Body).Decode(&badLevelBody); err != nil {
		t.Fatal(err)
	}
	badLevel.Body.Close()
	if badLevel.StatusCode != http.StatusOK || badLevelBody["error"].(map[string]any)["code"] != float64(-32602) {
		t.Fatalf("invalid log level was accepted: %#v", badLevelBody)
	}

	extra := postRawJSON(t, client, server.URL+"/mcp", "{}{}", headers)
	if extra.StatusCode != http.StatusBadRequest {
		t.Fatalf("trailing JSON was accepted: %d", extra.StatusCode)
	}
	extra.Body.Close()

	options, err := http.NewRequest(http.MethodOptions, server.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	options.Header.Set("Origin", "https://client.example")
	optionsResponse, err := client.Do(options)
	if err != nil {
		t.Fatal(err)
	}
	if optionsResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("unconfigured origin should be denied: %d", optionsResponse.StatusCode)
	}
	optionsResponse.Body.Close()
}

func TestTaskIDRedactionAndCron(t *testing.T) {
	app := testApp(t)
	if err := app.store.Update(map[string]any{"tasklist": []any{map[string]any{"taskname": "demo", "shareurl": "https://pan.quark.cn/s/example", "savepath": "/demo"}}}); err != nil {
		t.Fatal(err)
	}
	tasks := app.store.Tasks()
	if len(tasks) != 1 || asString(tasks[0]["id"]) == "" {
		t.Fatalf("stable task id missing: %#v", tasks)
	}
	oldID := asString(tasks[0]["id"])
	if err := app.store.Update(map[string]any{"tasklist": []any{map[string]any{"taskname": "demo", "shareurl": "https://pan.quark.cn/s/example", "savepath": "/demo"}}}); err != nil {
		t.Fatal(err)
	}
	if got := asString(app.store.Tasks()[0]["id"]); got != oldID {
		t.Fatalf("task id was not preserved: old=%s new=%s", oldID, got)
	}
	redacted := redactData(map[string]any{"cookie": []any{"secret-cookie"}, "nested": map[string]any{"apiKey": "secret-key", "BARK_PUSH": "push-secret", "WEBHOOK_URL": "https://secret.example"}}).(map[string]any)
	if redacted["cookie"] == nil || strings.Contains(string(mustJSON(redacted)), "secret-cookie") || strings.Contains(string(mustJSON(redacted)), "secret-key") {
		t.Fatalf("redaction failed: %#v", redacted)
	}
	for _, expression := range []string{"0 8,18,20 * * *", "*/5 * * * *", "0 8 * * 1-5"} {
		if _, err := parseCron(expression); err != nil {
			t.Fatalf("valid cron rejected %q: %v", expression, err)
		}
	}
	if _, err := parseCron("not-a-cron"); err == nil {
		t.Fatal("invalid cron accepted")
	}
	scheduler := NewScheduler(nil)
	if err := scheduler.Reload("*/5 * * * *"); err != nil || scheduler.State() != "running" {
		t.Fatalf("scheduler did not start: state=%s err=%v", scheduler.State(), err)
	}
	if err := scheduler.Reload(""); err != nil || scheduler.State() != "running" {
		t.Fatalf("empty cron changed scheduler state: state=%s err=%v", scheduler.State(), err)
	}
	scheduler.Shutdown()
	if scheduler.State() != "stopped" {
		t.Fatalf("scheduler did not stop: %s", scheduler.State())
	}
	if got := buildVersion("v1.2.3", "abcdefghi"); got != "v1.2.3" {
		t.Fatalf("tag version: %s", got)
	}
	if got := buildVersion("dev", "abcdefghi"); got != "dev(abcdefg)" {
		t.Fatalf("sha version: %s", got)
	}
	_ = time.Now()
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

func TestGoBackendStdio(t *testing.T) {
	app := testApp(t)
	key := enableTestMCP(t, app)
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-06-18\"}}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\",\"params\":{}}\n")
	var output bytes.Buffer
	if err := app.RunStdio(input, &output, key); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdio response count: %d (%s)", len(lines), output.String())
	}
	for _, line := range lines {
		var message map[string]any
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatal(err)
		}
		if message["jsonrpc"] != "2.0" || message["error"] != nil {
			t.Fatalf("invalid stdio response: %#v", message)
		}
	}
}

func TestRunManagerRejectsConcurrentWorkers(t *testing.T) {
	script := filepath.Join(t.TempDir(), "worker.py")
	if err := os.WriteFile(script, []byte("import time\nprint('worker', flush=True)\ntime.sleep(0.4)\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{python: "python3", script: script, config: script, timeout: 2 * time.Second, logs: NewLogBuffer(100)}
	manager := NewRunManager(worker, NewLogBuffer(100))
	taskA := []map[string]any{{"id": "task-a", "taskname": "A"}}
	taskB := []map[string]any{{"id": "task-b", "taskname": "B"}}
	first, err := manager.Start(context.Background(), taskA, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(context.Background(), taskA, false); err == nil {
		t.Fatal("same task was allowed to run twice")
	}
	if _, err := manager.Start(context.Background(), taskB, false); err == nil {
		t.Fatal("different task overlapped an active worker")
	}
	if _, err := manager.Start(context.Background(), nil, false); err == nil {
		t.Fatal("run-all was allowed while a worker was active")
	}
	if _, err := manager.Wait(context.Background(), asString(first["run_id"])); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Start(context.Background(), taskB, false)
	if err != nil {
		t.Fatalf("follow-up task was blocked: %v", err)
	}
	if _, err := manager.Wait(context.Background(), asString(second["run_id"])); err != nil {
		t.Fatal(err)
	}
	if manager.Busy() {
		t.Fatal("manager remained busy after task completion")
	}
}

func TestTaskMutationsRemainVisibleInMemory(t *testing.T) {
	app := testApp(t)
	added, err := app.store.AddTask(map[string]any{
		"taskname": "added", "shareurl": "https://pan.quark.cn/s/example", "savepath": "/added",
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks := app.store.Tasks()
	last := len(tasks) - 1
	if len(tasks) != 5 || asString(tasks[last]["id"]) != asString(added["id"]) {
		t.Fatalf("added task is not visible: %#v", tasks)
	}
	updated, err := app.store.UpdateTask(last, map[string]any{"taskname": "updated"})
	if err != nil {
		t.Fatal(err)
	}
	if got := asString(app.store.Tasks()[last]["taskname"]); got != "updated" || asString(updated["taskname"]) != "updated" {
		t.Fatalf("updated task is not visible: %#v", app.store.Tasks())
	}
	if _, err := app.store.DeleteTask(last); err != nil {
		t.Fatal(err)
	}
	if len(app.store.Tasks()) != 4 {
		t.Fatalf("deleted task remains visible: %#v", app.store.Tasks())
	}
}

type captureRoundTripper func(*http.Request) (*http.Response, error)

func (f captureRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestQuarkClientKeepsAccountCookieForNonShareRequests(t *testing.T) {
	cookie := "kps=abc+def; sign=signature; vcode=code; __uid=user"
	var captured *http.Request
	client := NewQuarkClient(cookie)
	client.client = &http.Client{Transport: captureRoundTripper(func(r *http.Request) (*http.Response, error) {
		captured = r
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"code":0}`)),
			Request:    r,
		}, nil
	})}
	if got := matchMParam(cookie)["kps"]; got != "abc+def" {
		t.Fatalf("mparam plus was decoded: %q", got)
	}
	if _, _, err := client.request(context.Background(), http.MethodGet, "/1/clouddrive/file/sort", url.Values{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if captured == nil || captured.Header.Get("Cookie") != cookie {
		t.Fatalf("account cookie missing from non-share request: %#v", captured)
	}

	if _, _, err := client.request(context.Background(), http.MethodGet, "/1/clouddrive/share/sharepage/detail", url.Values{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if captured == nil || captured.URL.Host != "drive-m.quark.cn" || captured.Header.Get("Cookie") != "" {
		t.Fatalf("share request did not use mobile transport safely: %#v", captured)
	}
	if got := captured.URL.Query().Get("kps"); got != "abc+def" {
		t.Fatalf("share kps plus was lost: raw=%q got=%q", captured.URL.RawQuery, got)
	}
}

func TestDestructiveFileTargetMustBeExplicit(t *testing.T) {
	invalid := []struct {
		fid  any
		path string
	}{
		{nil, ""},
		{nil, "/"},
		{"0", "/ignored"},
		{float64(0), ""},
		{true, ""},
		{float64(1.5), ""},
		{[]any{"fid"}, ""},
	}
	for _, item := range invalid {
		if _, err := resolveDestructiveFID(context.Background(), "cookie", item.fid, item.path); err == nil {
			t.Fatalf("invalid destructive target accepted: fid=%#v path=%q", item.fid, item.path)
		}
	}
	if got, err := resolveDestructiveFID(context.Background(), "cookie", "fid-1", ""); err != nil || asString(got) != "fid-1" {
		t.Fatalf("valid fid rejected: got=%#v err=%v", got, err)
	}
}

func TestHTTPDestructiveHandlersRejectRootTargets(t *testing.T) {
	app := testApp(t)
	if err := app.store.Update(map[string]any{"cookie": []any{"kps=abc+def; sign=signature; vcode=code"}}); err != nil {
		t.Fatal(err)
	}
	token := asString(app.store.DataForUI()["api_token"])
	for _, endpoint := range []string{"/delete_file", "/rename_file"} {
		req := httptest.NewRequest(http.MethodPost, endpoint+"?token="+url.QueryEscape(token), strings.NewReader(`{"path":"/"}`))
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, req)
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusOK || asBoolDefault(body["success"], true) {
			t.Fatalf("%s accepted root target: status=%d body=%#v", endpoint, recorder.Code, body)
		}
	}
}

func TestRunManagerStreamSharesManagedRunGate(t *testing.T) {
	script := filepath.Join(t.TempDir(), "worker.py")
	if err := os.WriteFile(script, []byte("import time\nprint('worker', flush=True)\ntime.sleep(0.3)\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{python: "python3", script: script, config: script, timeout: 2 * time.Second, logs: NewLogBuffer(100)}
	manager := NewRunManager(worker, NewLogBuffer(100))
	completed := make(chan struct{})
	manager.SetOnComplete(func() { close(completed) })
	lineSeen := make(chan struct{})
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- manager.Stream(context.Background(), nil, false, nil, nil, func(string) error {
			select {
			case <-lineSeen:
			default:
				close(lineSeen)
			}
			return nil
		})
	}()
	select {
	case <-lineSeen:
	case <-time.After(time.Second):
		t.Fatal("manual stream did not start")
	}
	if _, err := manager.Start(context.Background(), []map[string]any{{"id": "task-a"}}, false); err == nil {
		t.Fatal("managed run overlapped an active manual stream")
	}
	if err := <-streamDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("manual stream did not invoke completion callback")
	}
	if manager.Busy() {
		t.Fatal("manager remained busy after manual stream completion")
	}
}

func TestRunManagerConfigModeLeavesTaskListUnset(t *testing.T) {
	t.Setenv("TASKLIST", "stale-task-context")
	output := filepath.Join(t.TempDir(), "tasklist.txt")
	script := filepath.Join(t.TempDir(), "worker.py")
	program := "import os\nfrom pathlib import Path\nPath(" + strconv.Quote(output) + ").write_text(os.environ.get('TASKLIST', '<unset>'))\n"
	if err := os.WriteFile(script, []byte(program), 0o700); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{python: "python3", script: script, config: script, timeout: 2 * time.Second, logs: NewLogBuffer(100)}
	manager := NewRunManager(worker, NewLogBuffer(100))
	run, err := manager.Start(context.Background(), []map[string]any{{"id": "task-a"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Wait(context.Background(), asString(run["run_id"])); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "<unset>" {
		t.Fatalf("config-mode worker received TASKLIST: %q", data)
	}
}

func TestWorkerExplicitTestContextIsolated(t *testing.T) {
	output := filepath.Join(t.TempDir(), "worker-env.txt")
	script := filepath.Join(t.TempDir(), "worker.py")
	program := "import os\nfrom pathlib import Path\nPath(" + strconv.Quote(output) + ").write_text(os.environ['COOKIE'] + '|' + os.environ['PUSH_CONFIG'] + '|' + os.environ['QUARK_TEST'])\n"
	if err := os.WriteFile(script, []byte(program), 0o700); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{python: "python3", script: script, config: script, timeout: 2 * time.Second, logs: NewLogBuffer(100)}
	manager := NewRunManager(worker, NewLogBuffer(100))
	if err := manager.Stream(context.Background(), nil, true, []string{"account-cookie"}, map[string]any{"enabled": true}, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `["account-cookie"]|{"enabled":true}|true` {
		t.Fatalf("explicit worker context was not isolated: %q", data)
	}
}

func TestConfigCompatibilityDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config", "quark_config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"cookie":[],"plugins":{"emby":{"url":"","token":""},"custom":{"enabled":true,"secret":"keep-me"}},"magic_regex":{},"tasklist":[{"taskname":"legacy","shareurl":"https://pan.quark.cn/s/example","savepath":"/legacy","replace":"$TASKNAME.S01E01.mp4"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewConfigStore(path, "")
	if err != nil {
		t.Fatal(err)
	}
	tasks := store.Tasks()
	if len(tasks) != 1 || asString(tasks[0]["replace"]) != "{TASKNAME}.S01E01.mp4" {
		t.Fatalf("legacy replacement was not migrated: %#v", tasks)
	}
	data := store.DataForUI()
	if len(mapValue(data["magic_regex"])) == 0 || len(mapValue(data["plugins"])) < 6 || len(mapValue(data["task_plugins_config_default"])) < 6 {
		t.Fatalf("compatibility defaults missing: %#v", data)
	}
	custom := mapValue(mapValue(data["plugins"])["custom"])
	if !asBoolDefault(custom["enabled"], false) || asString(custom["secret"]) != "keep-me" {
		t.Fatalf("custom plugin configuration was dropped: %#v", data["plugins"])
	}
	if err := store.SetCloudSaverToken("refreshed-token"); err != nil {
		t.Fatal(err)
	}
	if got := asString(mapValue(mapValue(store.Snapshot()["source"])["cloudsaver"])["token"]); got != "refreshed-token" {
		t.Fatalf("CloudSaver token was not persisted: %q", got)
	}
	added, err := store.AddTask(map[string]any{"taskname": "new", "shareurl": "https://pan.quark.cn/s/example", "savepath": "/new"})
	if err != nil {
		t.Fatal(err)
	}
	if len(mapValue(added["addition"])) < 6 {
		t.Fatalf("new task did not receive plugin defaults: %#v", added)
	}
}

func TestMCPRejectsNonObjectArgumentsAndReturnsToolErrors(t *testing.T) {
	app := testApp(t)
	invalid := app.mcp.callTool(1, map[string]any{"name": "qas_tasks_list", "arguments": "not-an-object"})
	if invalid["error"] == nil || invalid["result"] != nil {
		t.Fatalf("non-object MCP arguments accepted: %#v", invalid)
	}
	failed := app.mcp.callTool(2, map[string]any{"name": "qas_tasks_get", "arguments": map[string]any{}})
	result := mapValue(failed["result"])
	if failed["error"] != nil || !asBoolDefault(result["isError"], false) {
		t.Fatalf("backend tool failure was not returned as tool error: %#v", failed)
	}
}
