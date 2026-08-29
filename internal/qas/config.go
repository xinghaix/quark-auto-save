package qas

import (
	"crypto/md5" // #nosec G501 -- retained for the legacy QAS API token format.
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var defaultPermissions = map[string]bool{
	"tasks.read":   true,
	"tasks.create": false,
	"tasks.update": false,
	"tasks.delete": false,
	"tasks.run":    false,
	"logs.read":    true,
	"search.read":  true,
	"files.read":   true,
	"files.write":  false,
	"config.read":  false,
}

var allowedConfigKeys = map[string]bool{
	"cookie": true, "crontab": true, "push_config": true, "tasklist": true,
	"magic_regex": true, "plugins": true, "source": true,
}

// Built-in plugin defaults shown in the WebUI before the first run.
var defaultPluginConfigs = map[string]any{
	"emby": map[string]any{"url": "", "token": ""},
	"fnv": map[string]any{
		"base_url": "http://10.0.0.6:5666", "app_name": "trimemedia-web", "username": "",
		"password": "", "secret_string": "", "api_key": "", "token": nil,
	},
	"auto_unarchive": map[string]any{
		"tips_":         "自动云解压(zip|rar|7z)到保存目录，在任务插件选项中启用，该功能需SVIP支持",
		"global_enable": false, "max_concurrent": 3,
	},
	"aria2": map[string]any{
		"host_port": "172.17.0.1:6800", "secret": "", "dir": "/Downloads",
	},
	"alist_sync": map[string]any{
		"url": "", "token": "", "quark_storage_id": "", "save_storage_id": "", "tv_mode": "",
	},
	"alist_strm_gen": map[string]any{
		"tips_alist_refresh": "该插件需与 alist 刷新插件配合使用，否则可能出现 alist 未刷新导致无法生成 strm 的问题！",
		"url":                "", "token": "", "storage_id": "", "strm_save_dir": "/media", "strm_replace_host": "",
	},
	"plex":       map[string]any{"url": "", "token": "", "quark_root_path": ""},
	"smartstrm":  map[string]any{"webhook": "", "strmtask": "", "xlist_path_fix": ""},
	"alist":      map[string]any{"url": "", "token": "", "storage_id": ""},
	"alist_strm": map[string]any{"url": "", "cookie": "", "config_id": ""},
}

var defaultTaskPluginConfigs = map[string]any{
	"emby":           map[string]any{"try_match": true, "media_id": ""},
	"fnv":            map[string]any{"auto_refresh": false, "mdb_name": "", "mdb_dir_list": ""},
	"auto_unarchive": map[string]any{"enable": false, "auto_clean": true, "auto_clean_zipdir": false},
	"aria2":          map[string]any{"auto_download": false, "download_subdir": false, "save_path": "", "pause": false},
	"alist_sync":     map[string]any{"enable": false, "save_path": "", "verify_path": "", "full_path_mode": false},
	"alist_strm_gen": map[string]any{"auto_gen": true},
}

var legacyMagicRegexDefaults = map[string]any{
	"$TV": map[string]any{
		"pattern": `.*?([Ss]\d{1,2})?(?:[第EePpXx\.\-\_\( ]{1,2}|^)(\d{1,3})(?!\d).*?\.(mp4|mkv)`,
		"replace": `\1E\2.\3`,
	},
	"$BLACK_WORD": map[string]any{
		"pattern": `^(?!.*纯享)(?!.*加更)(?!.*超前企划)(?!.*训练室)(?!.*蒸蒸日上).*`,
		"replace": "",
	},
}

type ConfigStore struct {
	mu           sync.RWMutex
	path         string
	data         map[string]any
	username     string
	password     string
	templatePath string
}

func NewConfigStore(path, templatePath string) (*ConfigStore, error) {
	s := &ConfigStore{path: path, templatePath: templatePath}
	if err := s.ensureFile(); err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *ConfigStore) ensureFile() error {
	if _, err := os.Stat(s.path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	var data []byte
	if s.templatePath != "" {
		if value, err := os.ReadFile(s.templatePath); err == nil {
			data = value
		}
	}
	if len(data) == 0 {
		data = []byte(`{"cookie":[],"push_config":{},"plugins":{},"magic_regex":{},"tasklist":[]}`)
	}
	return os.WriteFile(s.path, data, 0o600)
}

func (s *ConfigStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("read config %s: %w", s.path, err)
	}
	if decoded == nil {
		decoded = map[string]any{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = decoded
	normalizeLegacyConfig(s.data)
	s.applyRuntimeLocked()
	ensureTaskIDs(s.data)
	s.data["mcp"] = normalizeMCP(s.data["mcp"])
	return s.saveLocked()
}

func normalizeLegacyConfig(data map[string]any) {
	for _, task := range taskList(data["tasklist"]) {
		if replace, ok := task["replace"].(string); ok {
			task["replace"] = strings.ReplaceAll(replace, "$TASKNAME", "{TASKNAME}")
		}
	}
	if len(mapValue(data["magic_regex"])) == 0 {
		data["magic_regex"] = cloneMap(legacyMagicRegexDefaults)
	}
	data["plugins"] = mergeDefaultPluginConfigs(data["plugins"])
}

func mergeDefaultPluginConfigs(raw any) map[string]any {
	existing := mapValue(raw)
	result := cloneMap(existing)
	for name, rawDefaults := range defaultPluginConfigs {
		defaults := mapValue(rawDefaults)
		merged := cloneMap(mapValue(existing[name]))
		for key, value := range defaults {
			if _, ok := merged[key]; !ok {
				merged[key] = cloneValue(value)
			}
		}
		result[name] = merged
	}
	return result
}

func (s *ConfigStore) applyRuntimeLocked() {
	webui, _ := s.data["webui"].(map[string]any)
	if webui == nil {
		webui = map[string]any{}
	}
	s.username = firstNonEmpty(os.Getenv("WEBUI_USERNAME"), asString(webui["username"]), "admin")
	s.password = firstNonEmpty(os.Getenv("WEBUI_PASSWORD"), asString(webui["password"]), "admin123")
	s.data["webui"] = map[string]any{"username": s.username, "password": s.password}
	if asString(s.data["crontab"]) == "" {
		s.data["crontab"] = "0 8,18,20 * * *"
	}
}

func (s *ConfigStore) saveLocked() error {
	encoded, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".quark-config-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

func (s *ConfigStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

// Reload refreshes the in-memory view after the compatibility worker writes
// shareurl_ban, plugin state, or other task metadata back to disk.
func (s *ConfigStore) Reload() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("reload config %s: %w", s.path, err)
	}
	if decoded == nil {
		decoded = map[string]any{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = decoded
	normalizeLegacyConfig(s.data)
	s.applyRuntimeLocked()
	ensureTaskIDs(s.data)
	s.data["mcp"] = normalizeMCP(s.data["mcp"])
	return nil
}

func (s *ConfigStore) Replace(data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = data
	normalizeLegacyConfig(s.data)
	s.applyRuntimeLocked()
	ensureTaskIDs(s.data)
	s.data["mcp"] = normalizeMCP(s.data["mcp"])
	return s.saveLocked()
}

func (s *ConfigStore) Path() string { return s.path }

func (s *ConfigStore) Snapshot() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMap(s.data)
}

func (s *ConfigStore) Tasks() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := taskList(s.data["tasklist"])
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, cloneMap(item))
	}
	return result
}

func (s *ConfigStore) Cookies() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.data["cookie"]
	if raw, ok := value.(string); ok && raw != "" {
		return strings.Split(raw, "\n")
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

func (s *ConfigStore) SetCloudSaverToken(token string) error {
	if token == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	source := mapValue(s.data["source"])
	cloud := mapValue(source["cloudsaver"])
	cloud["token"] = token
	source["cloudsaver"] = cloud
	s.data["source"] = source
	return s.saveLocked()
}

func (s *ConfigStore) UsernamePassword() (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.username, s.password
}

func (s *ConfigStore) MCP() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMap(normalizeMCP(s.data["mcp"]))
}

func (s *ConfigStore) DataForUI() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data := cloneMap(s.data)
	delete(data, "webui")
	data["mcp"] = mcpConfigForUI(data["mcp"])
	data["api_token"] = loginToken(s.username, s.password)
	data["task_plugins_config_default"] = cloneMap(defaultTaskPluginConfigs)
	return data
}

func (s *ConfigStore) Update(payload map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if raw, exists := payload["mcp"]; exists {
		merged, err := mergeMCP(s.data["mcp"], raw)
		if err != nil {
			return err
		}
		s.data["mcp"] = merged
	}
	for key, value := range payload {
		if !allowedConfigKeys[key] {
			continue
		}
		if key == "tasklist" {
			value = cloneValue(value)
			preserveTaskIDs(taskList(s.data["tasklist"]), taskList(value))
			ensureTaskIDs(map[string]any{"tasklist": value})
		}
		s.data[key] = cloneValue(value)
	}
	return s.saveLocked()
}

func (s *ConfigStore) AddTask(task map[string]any) (map[string]any, error) {
	for _, field := range []string{"taskname", "shareurl", "savepath"} {
		if strings.TrimSpace(asString(task[field])) == "" {
			return nil, fmt.Errorf("缺少必要字段: %s", field)
		}
	}
	value := cloneMap(task)
	if asString(value["id"]) == "" {
		value["id"] = newID()
	}
	if value["addition"] == nil {
		value["addition"] = cloneMap(defaultTaskPluginConfigs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := ensureTaskListLocked(s.data)
	items = append(items, value)
	s.data["tasklist"] = items
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return cloneMap(value), nil
}

func (s *ConfigStore) DeleteTask(index int) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := ensureTaskListLocked(s.data)
	if index < 0 || index >= len(items) {
		return nil, errors.New("未找到任务")
	}
	removed := items[index]
	items = append(items[:index], items[index+1:]...)
	s.data["tasklist"] = items
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return cloneMap(removed), nil
}

func (s *ConfigStore) UpdateTask(index int, patch map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := ensureTaskListLocked(s.data)
	if index < 0 || index >= len(items) {
		return nil, errors.New("未找到任务")
	}
	candidate := cloneMap(items[index])
	for key, value := range patch {
		candidate[key] = cloneValue(value)
	}
	if strings.TrimSpace(asString(candidate["taskname"])) == "" || strings.TrimSpace(asString(candidate["shareurl"])) == "" || strings.TrimSpace(asString(candidate["savepath"])) == "" {
		return nil, errors.New("任务缺少必要字段")
	}
	if _, changed := patch["shareurl"]; changed {
		delete(candidate, "shareurl_ban")
	}
	items[index] = candidate
	s.data["tasklist"] = items
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return cloneMap(candidate), nil
}

func (s *ConfigStore) ResolveTask(args map[string]any) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := taskList(s.data["tasklist"])
	selectors := 0
	var selected string
	for _, key := range []string{"task_id", "task_name", "index"} {
		if value, ok := args[key]; ok && value != nil && asString(value) != "" {
			selectors++
			selected = key
		}
	}
	if selectors != 1 {
		return -1, errors.New("必须且只能提供 task_id、task_name 或 index 之一")
	}
	switch selected {
	case "index":
		index := int(asFloat(args["index"]))
		if index < 0 || index >= len(items) || asFloat(args["index"]) != float64(index) {
			return -1, errors.New("任务 index 无效")
		}
		return index, nil
	case "task_id":
		value := asString(args[selected])
		for index, task := range items {
			if asString(task["id"]) == value {
				return index, nil
			}
		}
	case "task_name":
		value := asString(args[selected])
		for index, task := range items {
			if asString(task["taskname"]) == value {
				return index, nil
			}
		}
	}
	return -1, errors.New("未找到任务")
}

func (s *ConfigStore) ResolveTaskPublic(args map[string]any) (map[string]any, error) {
	index, err := s.ResolveTask(args)
	if err != nil {
		return nil, err
	}
	tasks := s.Tasks()
	value := cloneMap(tasks[index])
	value["index"] = index
	return redactData(value).(map[string]any), nil
}

func loginToken(username, password string) string {
	sum := md5.Sum([]byte("token" + username + password + "+-*/"))
	return hex.EncodeToString(sum[:])[8:24]
}

func hashAPIKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func verifyAPIKey(value, expected string) bool {
	if value == "" || expected == "" {
		return false
	}
	actual := hashAPIKey(value)
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

func normalizeMCP(raw any) map[string]any {
	input, _ := raw.(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	permissions, _ := input["permissions"].(map[string]any)
	if permissions == nil {
		permissions = map[string]any{}
	}
	hash := asString(input["api_key_hash"])
	if hash == "" && asString(input["api_key"]) != "" {
		hash = hashAPIKey(asString(input["api_key"]))
	}
	result := map[string]any{
		"enabled":      asBoolDefault(input["enabled"], false),
		"api_key_hash": hash,
		"permissions":  map[string]any{},
	}
	out := result["permissions"].(map[string]any)
	for name, defaultValue := range defaultPermissions {
		out[name] = asBoolDefault(permissions[name], defaultValue)
	}
	return result
}

func mcpConfigForUI(raw any) map[string]any {
	config := normalizeMCP(raw)
	return map[string]any{
		"enabled":            config["enabled"],
		"api_key":            "",
		"api_key_configured": asString(config["api_key_hash"]) != "",
		"permissions":        config["permissions"],
	}
}

func mergeMCP(current, incoming any) (map[string]any, error) {
	input, ok := incoming.(map[string]any)
	if !ok {
		return nil, errors.New("mcp 配置必须是对象")
	}
	old := normalizeMCP(current)
	permissions, _ := input["permissions"].(map[string]any)
	if permissions == nil && input["permissions"] != nil {
		return nil, errors.New("mcp.permissions 必须是对象")
	}
	if permissions == nil {
		permissions = map[string]any{}
	}
	for name, value := range permissions {
		if _, ok := defaultPermissions[name]; !ok {
			return nil, fmt.Errorf("未知 MCP 权限: %s", name)
		}
		if _, ok := value.(bool); !ok {
			return nil, errors.New("MCP 权限值必须是布尔值")
		}
	}
	enabled := asBoolDefault(old["enabled"], false)
	if value, exists := input["enabled"]; exists {
		var ok bool
		enabled, ok = value.(bool)
		if !ok {
			return nil, errors.New("MCP enabled 必须是布尔值")
		}
	}
	hash := asString(old["api_key_hash"])
	if value := asString(input["api_key"]); value != "" {
		if len(value) < 20 {
			return nil, errors.New("MCP API key 至少需要 20 个字符")
		}
		hash = hashAPIKey(value)
	}
	if enabled && hash == "" {
		return nil, errors.New("启用 MCP 前必须设置 API key")
	}
	result := map[string]any{"enabled": enabled, "api_key_hash": hash, "permissions": map[string]any{}}
	out := result["permissions"].(map[string]any)
	oldPermissions, _ := old["permissions"].(map[string]any)
	for name, defaultValue := range defaultPermissions {
		value := asBoolDefault(oldPermissions[name], defaultValue)
		if incomingValue, exists := permissions[name]; exists {
			value = incomingValue.(bool)
		}
		out[name] = value
	}
	return result, nil
}

func preserveTaskIDs(existing, incoming []map[string]any) {
	bySignature := map[string][]string{}
	for _, task := range existing {
		id := asString(task["id"])
		if id != "" {
			bySignature[taskSignature(task)] = append(bySignature[taskSignature(task)], id)
		}
	}
	used := map[string]bool{}
	for index, task := range incoming {
		id := asString(task["id"])
		if id != "" && !used[id] {
			used[id] = true
			continue
		}
		candidates := bySignature[taskSignature(task)]
		for len(candidates) > 0 && used[candidates[0]] {
			candidates = candidates[1:]
		}
		if len(candidates) > 0 {
			id = candidates[0]
			bySignature[taskSignature(task)] = candidates[1:]
		} else if index < len(existing) {
			id = asString(existing[index]["id"])
		}
		if id == "" || used[id] {
			id = newID()
		}
		task["id"] = id
		used[id] = true
	}
}

func taskSignature(task map[string]any) string {
	return asString(task["taskname"]) + "\x00" + asString(task["shareurl"]) + "\x00" + asString(task["savepath"])
}

func ensureTaskIDs(data map[string]any) {
	items, ok := data["tasklist"].([]any)
	if !ok {
		return
	}
	seen := map[string]bool{}
	for _, raw := range items {
		task, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := asString(task["id"])
		if id == "" || seen[id] {
			id = newID()
			task["id"] = id
		}
		seen[id] = true
	}
}

func ensureTaskListLocked(data map[string]any) []map[string]any {
	items := taskList(data["tasklist"])
	for _, task := range items {
		if asString(task["id"]) == "" {
			task["id"] = newID()
		}
	}
	return items
}

func taskList(raw any) []map[string]any {
	switch items := raw.(type) {
	case []map[string]any:
		return append([]map[string]any(nil), items...)
	case []any:
		result := make([]map[string]any, 0, len(items))
		for _, item := range items {
			if task, ok := item.(map[string]any); ok {
				result = append(result, task)
			}
		}
		return result
	default:
		return nil
	}
}

func newID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	result, _ := cloneValue(value).(map[string]any)
	return result
}

func cloneValue(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var result any
	if json.Unmarshal(encoded, &result) != nil {
		return nil
	}
	return result
}

func redactData(value any) any {
	sensitive := func(key string) bool {
		normalized := strings.ToLower(strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(key))
		if normalized == "webui" || normalized == "cookie" || normalized == "password" || normalized == "passwd" || normalized == "token" || normalized == "secret" || normalized == "authorization" || normalized == "api_key" || normalized == "api_key_hash" {
			return true
		}
		return strings.HasSuffix(normalized, "_key") || strings.HasSuffix(normalized, "key") || strings.Contains(normalized, "password") || strings.Contains(normalized, "token") || strings.Contains(normalized, "secret") || normalized == "bark_push" || normalized == "deer_url" || normalized == "qywx_origin" || normalized == "qywx_am" || normalized == "qywx_key" || normalized == "webhook_url" || normalized == "webhook_body" || normalized == "webhook_headers" || normalized == "smtp_email" || normalized == "smtp_password"
	}
	var walk func(any, string) any
	walk = func(item any, key string) any {
		switch typed := item.(type) {
		case map[string]any:
			result := map[string]any{}
			for name, child := range typed {
				if strings.EqualFold(name, "webui") {
					continue
				}
				if sensitive(name) {
					result[name] = "[REDACTED]"
				} else {
					result[name] = walk(child, name)
				}
			}
			return result
		case []any:
			result := make([]any, len(typed))
			for index, child := range typed {
				result[index] = walk(child, key)
			}
			return result
		default:
			return item
		}
	}
	return walk(value, "")
}

func asString(value any) string {
	if value == nil {
		return ""
	}
	if result, ok := value.(string); ok {
		return result
	}
	return fmt.Sprint(value)
}

func asFloat(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	default:
		return 0
	}
}

func asBoolDefault(value any, fallback bool) bool {
	if value == nil {
		return fallback
	}
	if result, ok := value.(bool); ok {
		return result
	}
	if result, ok := value.(string); ok {
		switch strings.ToLower(strings.TrimSpace(result)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
