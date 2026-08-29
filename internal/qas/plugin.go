package qas

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

type Plugin interface {
	Name() string
	Defaults() (map[string]any, map[string]any)
	Init(cfg map[string]any, log func(string, ...any))
	Active() bool
	Run(ctx context.Context, task map[string]any, acc *quarkAccount, tree *saveTree, log func(string, ...any)) map[string]any
}

func loadPlugins(cfg map[string]any, flags string, log func(string, ...any)) []Plugin {
	skip := map[string]bool{}
	for _, item := range strings.Split(flags, ",") {
		item = strings.TrimSpace(item)
		if strings.HasPrefix(item, "-") {
			skip[strings.TrimPrefix(item, "-")] = true
		}
	}
	all := []Plugin{
		&embyPlugin{}, &fnvPlugin{}, &autoUnarchivePlugin{}, &aria2Plugin{},
		&alistSyncPlugin{}, &alistStrmGenPlugin{}, &plexPlugin{}, &smartstrmPlugin{},
		&alistPlugin{}, &alistStrmPlugin{},
	}
	var out []Plugin
	for _, p := range all {
		if skip[p.Name()] {
			continue
		}
		p.Init(mapValue(cfg[p.Name()]), log)
		out = append(out, p)
	}
	return out
}

func pluginHTTP(ctx context.Context, method, rawURL string, headers map[string]string, payload any, form url.Values) (map[string]any, int, string, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
		if headers == nil {
			headers = map[string]string{}
		}
		if headers["Content-Type"] == "" {
			headers["Content-Type"] = "application/x-www-form-urlencoded"
		}
	} else if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, "", err
		}
		body = bytes.NewReader(encoded)
		if headers == nil {
			headers = map[string]string{}
		}
		if headers["Content-Type"] == "" {
			headers["Content-Type"] = "application/json"
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, 0, "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return decoded, resp.StatusCode, string(raw), nil
}

func cfgString(cfg map[string]any, key string) string { return asString(cfg[key]) }

type embyPlugin struct {
	url, token string
	active     bool
}

func (p *embyPlugin) Name() string { return "emby" }
func (p *embyPlugin) Defaults() (map[string]any, map[string]any) {
	return map[string]any{"url": "", "token": ""}, map[string]any{"try_match": true, "media_id": ""}
}
func (p *embyPlugin) Active() bool { return p.active }
func (p *embyPlugin) Init(cfg map[string]any, log func(string, ...any)) {
	p.url, p.token = cfgString(cfg, "url"), cfgString(cfg, "token")
	if p.url == "" || p.token == "" {
		return
	}
	data, _, _, err := pluginHTTP(context.Background(), http.MethodGet, strings.TrimRight(p.url, "/")+"/emby/System/Info", map[string]string{"X-Emby-Token": p.token}, nil, nil)
	if err == nil && asString(data["ServerName"]) != "" {
		log("Emby媒体库: %s v%s", asString(data["ServerName"]), asString(data["Version"]))
		p.active = true
	}
}
func (p *embyPlugin) Run(ctx context.Context, task map[string]any, _ *quarkAccount, _ *saveTree, log func(string, ...any)) map[string]any {
	cfg := mapValue(mapValue(task["addition"])["emby"])
	if asString(cfg["media_id"]) != "" && asString(cfg["media_id"]) != "0" {
		p.refresh(ctx, asString(cfg["media_id"]), log)
		return nil
	}
	if !asBoolDefault(cfg["try_match"], true) {
		return nil
	}
	id := p.search(ctx, asString(task["taskname"]), log)
	if id == "" {
		return nil
	}
	p.refresh(ctx, id, log)
	cfg["media_id"] = id
	addition := mapValue(task["addition"])
	addition["emby"] = cfg
	task["addition"] = addition
	return task
}
func (p *embyPlugin) refresh(ctx context.Context, id string, log func(string, ...any)) {
	u := fmt.Sprintf("%s/emby/Items/%s/Refresh?Recursive=true&MetadataRefreshMode=FullRefresh&ImageRefreshMode=FullRefresh&ReplaceAllMetadata=false&ReplaceAllImages=false", strings.TrimRight(p.url, "/"), id)
	_, code, raw, err := pluginHTTP(ctx, http.MethodPost, u, map[string]string{"X-Emby-Token": p.token}, nil, nil)
	if err == nil && (code == 200 || raw == "") {
		log("🎞️ 刷新Emby媒体库：成功✅")
	} else {
		log("🎞️ 刷新Emby媒体库：%s❌", raw)
	}
}
func (p *embyPlugin) search(ctx context.Context, name string, log func(string, ...any)) string {
	u := strings.TrimRight(p.url, "/") + "/emby/Items?IncludeItemTypes=Series&StartIndex=0&SortBy=SortName&SortOrder=Ascending&ImageTypeLimit=0&Recursive=true&SearchTerm=" + url.QueryEscape(name) + "&Limit=10&IncludeSearchTypes=false"
	data, _, _, err := pluginHTTP(ctx, http.MethodGet, u, map[string]string{"X-Emby-Token": p.token}, nil, nil)
	if err != nil {
		return ""
	}
	for _, item := range listValue(data["Items"]) {
		value := mapValue(item)
		if asBoolDefault(value["IsFolder"], false) {
			log("🎞️ 《%s》匹配到Emby媒体库ID：%s", asString(value["Name"]), asString(value["Id"]))
			return asString(value["Id"])
		}
	}
	return ""
}

type plexPlugin struct {
	url, token, root string
	active           bool
	libraries        []map[string]any
}

func (p *plexPlugin) Name() string { return "plex" }
func (p *plexPlugin) Defaults() (map[string]any, map[string]any) {
	return map[string]any{"url": "", "token": "", "quark_root_path": ""}, nil
}
func (p *plexPlugin) Active() bool { return p.active }
func (p *plexPlugin) Init(cfg map[string]any, log func(string, ...any)) {
	p.url, p.token, p.root = cfgString(cfg, "url"), cfgString(cfg, "token"), cfgString(cfg, "quark_root_path")
	if p.url == "" || p.token == "" || p.root == "" {
		return
	}
	data, code, _, err := pluginHTTP(context.Background(), http.MethodGet, strings.TrimRight(p.url, "/")+"/", map[string]string{"Accept": "application/json", "X-Plex-Token": p.token}, nil, nil)
	if err == nil && code == 200 {
		info := mapValue(data["MediaContainer"])
		log("Plex媒体库: %s v%s", asString(info["friendlyName"]), asString(info["version"]))
		p.active = true
	}
}
func (p *plexPlugin) Run(ctx context.Context, task map[string]any, _ *quarkAccount, _ *saveTree, log func(string, ...any)) map[string]any {
	if asString(task["savepath"]) == "" {
		return nil
	}
	if p.libraries == nil {
		data, _, _, err := pluginHTTP(ctx, http.MethodGet, strings.TrimRight(p.url, "/")+"/library/sections", map[string]string{"Accept": "application/json", "X-Plex-Token": p.token}, nil, nil)
		if err == nil {
			for _, item := range listValue(mapValue(data["MediaContainer"])["Directory"]) {
				p.libraries = append(p.libraries, mapValue(item))
			}
		}
	}
	full := path.Clean(p.root + "/" + strings.TrimPrefix(asString(task["savepath"]), "/"))
	for _, lib := range p.libraries {
		for _, loc := range listValue(lib["Location"]) {
			locPath := asString(mapValue(loc)["path"])
			if locPath != "" && (full == locPath || strings.HasPrefix(full, strings.TrimRight(locPath, "/")+"/")) {
				u := fmt.Sprintf("%s/library/sections/%s/refresh?path=%s", strings.TrimRight(p.url, "/"), asString(lib["key"]), url.QueryEscape(full))
				_, code, _, err := pluginHTTP(ctx, http.MethodGet, u, map[string]string{"Accept": "application/json", "X-Plex-Token": p.token}, nil, nil)
				if err == nil && code == 200 {
					log("🎞️ 刷新Plex媒体库：%s [%s] 成功✅", asString(lib["title"]), full)
					return nil
				}
			}
		}
	}
	log("🎞️ 刷新Plex媒体库：%s 未找到匹配的媒体库❌", full)
	return nil
}

type smartstrmPlugin struct {
	webhook, task, fix string
	active             bool
}

func (p *smartstrmPlugin) Name() string { return "smartstrm" }
func (p *smartstrmPlugin) Defaults() (map[string]any, map[string]any) {
	return map[string]any{"webhook": "", "strmtask": "", "xlist_path_fix": ""}, nil
}
func (p *smartstrmPlugin) Active() bool { return p.active }
func (p *smartstrmPlugin) Init(cfg map[string]any, log func(string, ...any)) {
	p.webhook, p.task, p.fix = cfgString(cfg, "webhook"), cfgString(cfg, "strmtask"), cfgString(cfg, "xlist_path_fix")
	if p.webhook == "" || p.task == "" {
		return
	}
	data, _, _, err := pluginHTTP(context.Background(), http.MethodGet, p.webhook, nil, nil, nil)
	if err == nil && asBoolDefault(data["success"], false) {
		log("SmartStrm 触发任务: 连接成功 %s", asString(data["version"]))
		p.active = true
	}
}
func (p *smartstrmPlugin) Run(ctx context.Context, task map[string]any, _ *quarkAccount, _ *saveTree, log func(string, ...any)) map[string]any {
	data, _, _, err := pluginHTTP(ctx, http.MethodPost, p.webhook, nil, map[string]any{"event": "qas_strm", "data": map[string]any{"strmtask": p.task, "savepath": task["savepath"], "xlist_path_fix": p.fix}}, nil)
	if err == nil && asBoolDefault(data["success"], false) {
		log("SmartStrm 触发任务: [%s] %s 成功✅", asString(mapValue(data["task"])["name"]), asString(mapValue(data["task"])["storage_path"]))
	} else if err != nil {
		log("SmartStrm 触发任务：出错 %s", err)
	} else {
		log("SmartStrm 触发任务: %s", asString(data["message"]))
	}
	return nil
}
