package qas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	quarkBaseURL   = "https://drive-pc.quark.cn"
	quarkMobileURL = "https://drive-m.quark.cn"
	quarkUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) quark-cloud-drive/3.14.2 Chrome/112.0.5615.165 Electron/24.1.3.8 Safari/537.36 Channel/pckk_other_ch"
)

var (
	shareIDRE     = regexp.MustCompile(`/s/([A-Za-z0-9_]+)`)
	passcodeRE    = regexp.MustCompile(`(?:^|[?&#])pwd=([A-Za-z0-9_]+)`)
	pathFIDRE     = regexp.MustCompile(`/([A-Za-z0-9]{32})-?([^/]*)`)
	mparamKPSRE   = regexp.MustCompile(`(?:^|[;\s])kps=([^;\s]+)`)
	mparamSignRE  = regexp.MustCompile(`(?:^|[;\s])sign=([^;\s]+)`)
	mparamVCodeRE = regexp.MustCompile(`(?:^|[;\s])vcode=([^;\s]+)`)
)

type QuarkClient struct {
	cookie string
	mparam map[string]string
	client *http.Client
}

func NewQuarkClient(cookie string) *QuarkClient {
	return &QuarkClient{
		cookie: strings.TrimSpace(cookie),
		mparam: matchMParam(cookie),
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func matchMParam(cookie string) map[string]string {
	result := map[string]string{}
	for key, expression := range map[string]*regexp.Regexp{"kps": mparamKPSRE, "sign": mparamSignRE, "vcode": mparamVCodeRE} {
		if match := expression.FindStringSubmatch(cookie); len(match) == 2 {
			value := strings.ReplaceAll(match[1], "%25", "%")
			result[key] = value
		}
	}
	if len(result) != 3 {
		return map[string]string{}
	}
	return result
}

func (q *QuarkClient) request(ctx context.Context, method, path string, params url.Values, payload any, headers http.Header) (map[string]any, http.Header, error) {
	base := quarkBaseURL
	mobile := len(q.mparam) == 3 && (strings.Contains(path, "share") || strings.Contains(path, "capacity/growth"))
	if mobile {
		base = quarkMobileURL
		if params == nil {
			params = url.Values{}
		}
		if strings.Contains(path, "share") {
			for key, value := range map[string]string{
				"device_model": "M2011K2C", "entry": "default_clouddrive", "_t_group": "0%3A_s_vp%3A1", "dmn": "Mi%2B11", "fr": "android", "pf": "3300", "bi": "35937", "ve": "7.4.5.680", "ss": "411x875", "mi": "M2011K2C", "nt": "5", "nw": "0", "kt": "4", "pr": "ucpro", "sv": "release", "dt": "phone", "data_from": "ucapi", "app": "clouddrive", "kkkk": "1",
			} {
				params.Set(key, value)
			}
		}
		for key, value := range q.mparam {
			params.Set(key, value)
		}
	}
	rawURL := base + path
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", quarkUserAgent)
	if q.cookie != "" && !mobile {
		req.Header.Set("Cookie", q.cookie)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	response, err := q.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16*1024*1024))
	if err != nil {
		return nil, response.Header, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, response.Header, fmt.Errorf("夸克 API 返回非 JSON (%d)", response.StatusCode)
	}
	return decoded, response.Header, nil
}

func (q *QuarkClient) get(ctx context.Context, path string, params url.Values) (map[string]any, error) {
	data, _, err := q.request(ctx, http.MethodGet, path, params, nil, nil)
	return data, err
}

func (q *QuarkClient) post(ctx context.Context, path string, params url.Values, payload any) (map[string]any, error) {
	data, _, err := q.request(ctx, http.MethodPost, path, params, payload, nil)
	return data, err
}

func (q *QuarkClient) getStoken(ctx context.Context, pwdID, passcode string) (map[string]any, error) {
	return q.post(ctx, "/1/clouddrive/share/sharepage/token", url.Values{"pr": {"ucpro"}, "fr": {"pc"}}, map[string]any{"pwd_id": pwdID, "passcode": passcode})
}

func (q *QuarkClient) getDetail(ctx context.Context, pwdID, stoken, pdirFID string, fetchShare, fullPath int) (map[string]any, error) {
	merged := []any{}
	var response map[string]any
	for page := 1; page <= 200; page++ {
		params := url.Values{}
		params.Set("pr", "ucpro")
		params.Set("fr", "pc")
		params.Set("pwd_id", pwdID)
		params.Set("stoken", stoken)
		params.Set("pdir_fid", pdirFID)
		params.Set("force", "0")
		params.Set("_page", fmt.Sprint(page))
		params.Set("_size", "50")
		params.Set("_fetch_banner", "0")
		params.Set("_fetch_share", fmt.Sprint(fetchShare))
		params.Set("_fetch_total", "1")
		params.Set("_sort", "file_type:asc,updated_at:desc")
		params.Set("ver", "2")
		params.Set("fetch_share_full_path", fmt.Sprint(fullPath))
		var err error
		response, err = q.get(ctx, "/1/clouddrive/share/sharepage/detail", params)
		if err != nil {
			return nil, err
		}
		if number(response["code"]) != 0 {
			return response, nil
		}
		data := mapValue(response["data"])
		items := listValue(data["list"])
		merged = append(merged, items...)
		if len(items) == 0 || len(merged) >= int(number(mapValue(response["metadata"])["_total"])) {
			data["list"] = merged
			response["data"] = data
			return response, nil
		}
	}
	return response, nil
}

func (q *QuarkClient) getFIDs(ctx context.Context, paths []string) ([]map[string]any, error) {
	result := []map[string]any{}
	for len(paths) > 0 {
		batch := paths
		if len(batch) > 50 {
			batch = paths[:50]
		}
		response, err := q.post(ctx, "/1/clouddrive/file/info/path_list", url.Values{"pr": {"ucpro"}, "fr": {"pc"}}, map[string]any{"file_path": batch, "namespace": "0"})
		if err != nil {
			return nil, err
		}
		if number(response["code"]) != 0 {
			return result, nil
		}
		for _, item := range listValue(response["data"]) {
			if value, ok := item.(map[string]any); ok {
				result = append(result, value)
			}
		}
		paths = paths[len(batch):]
	}
	return result, nil
}

func (q *QuarkClient) listDir(ctx context.Context, fid any) (map[string]any, error) {
	merged := []any{}
	var response map[string]any
	for page := 1; page <= 200; page++ {
		params := url.Values{}
		params.Set("pr", "ucpro")
		params.Set("fr", "pc")
		params.Set("uc_param_str", "")
		params.Set("pdir_fid", asString(fid))
		params.Set("_page", fmt.Sprint(page))
		params.Set("_size", "50")
		params.Set("_fetch_total", "1")
		params.Set("_fetch_sub_dirs", "0")
		params.Set("_sort", "file_type:asc,updated_at:desc")
		params.Set("_fetch_full_path", "0")
		params.Set("fetch_all_file", "1")
		params.Set("fetch_risk_file_name", "1")
		var err error
		response, err = q.get(ctx, "/1/clouddrive/file/sort", params)
		if err != nil {
			return nil, err
		}
		if number(response["code"]) != 0 {
			return response, nil
		}
		data := mapValue(response["data"])
		items := listValue(data["list"])
		merged = append(merged, items...)
		if len(items) == 0 || len(merged) >= int(number(mapValue(response["metadata"])["_total"])) {
			data["list"] = merged
			response["data"] = data
			return response, nil
		}
	}
	return response, nil
}

func (q *QuarkClient) rename(ctx context.Context, fid any, name string) (map[string]any, error) {
	return q.post(ctx, "/1/clouddrive/file/rename", url.Values{"pr": {"ucpro"}, "fr": {"pc"}, "uc_param_str": {""}}, map[string]any{"fid": fid, "file_name": name})
}

func (q *QuarkClient) delete(ctx context.Context, fids []any) (map[string]any, error) {
	return q.post(ctx, "/1/clouddrive/file/delete", url.Values{"pr": {"ucpro"}, "fr": {"pc"}, "uc_param_str": {""}}, map[string]any{"action_type": 2, "filelist": fids, "exclude_fids": []any{}})
}

func (q *QuarkClient) mkdir(ctx context.Context, path string) (map[string]any, error) {
	return q.post(ctx, "/1/clouddrive/file", url.Values{"pr": {"ucpro"}, "fr": {"pc"}, "uc_param_str": {""}}, map[string]any{"pdir_fid": "0", "file_name": "", "dir_path": path, "dir_init_lock": false})
}

func extractShareURL(raw string) (string, string, string, []map[string]any, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() != "pan.quark.cn" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", "", "", nil, errors.New("shareurl 必须是 pan.quark.cn 分享链接")
	}
	idMatch := shareIDRE.FindStringSubmatch(raw)
	if len(idMatch) != 2 {
		return "", "", "", nil, errors.New("分享链接缺少 pwd_id")
	}
	passcode := ""
	if match := passcodeRE.FindStringSubmatch(raw); len(match) == 2 {
		passcode = match[1]
	}
	paths := []map[string]any{}
	matches := pathFIDRE.FindAllStringSubmatch(raw, -1)
	for _, match := range matches {
		name, _ := url.PathUnescape(strings.ReplaceAll(match[2], "*101", "-"))
		paths = append(paths, map[string]any{"fid": match[1], "name": name})
	}
	pdir := "0"
	if len(matches) > 0 {
		pdir = matches[len(matches)-1][1]
	}
	return idMatch[1], passcode, pdir, paths, nil
}

func shareDetail(ctx context.Context, shareURL, token string) (map[string]any, error) {
	q := NewQuarkClient("")
	pwdID, passcode, pdir, paths, err := extractShareURL(shareURL)
	if err != nil {
		return nil, err
	}
	if token == "" {
		response, err := q.getStoken(ctx, pwdID, passcode)
		if err != nil {
			return nil, err
		}
		if number(response["status"]) != 200 {
			return nil, errors.New(asString(response["message"]))
		}
		token = asString(mapValue(response["data"])["stoken"])
	}
	response, err := q.getDetail(ctx, pwdID, token, pdir, 1, 1)
	if err != nil {
		return nil, err
	}
	if number(response["code"]) != 0 {
		return nil, errors.New(asString(response["message"]))
	}
	data := mapValue(response["data"])
	fullPath := []map[string]any{}
	for _, item := range listValue(data["full_path"]) {
		value := mapValue(item)
		if len(value) > 0 {
			fullPath = append(fullPath, map[string]any{"fid": value["fid"], "name": value["file_name"]})
		}
	}
	if len(fullPath) == 0 {
		fullPath = paths
	}
	data["paths"] = fullPath
	data["stoken"] = token
	if osEnvBool("FILTER_INVALID_VIDEO", true) {
		for _, item := range listValue(data["list"]) {
			value := mapValue(item)
			name := strings.ToLower(asString(value["file_name"]))
			if (strings.HasSuffix(name, ".mp4") || strings.HasSuffix(name, ".mkv")) && !asBoolDefault(value["dir"], false) && asString(value["obj_category"]) != "video" {
				return nil, errors.New("无效视频格式")
			}
		}
	}
	return data, nil
}

func fileList(ctx context.Context, cookie string, fid any, path string) (map[string]any, error) {
	q := NewQuarkClient(cookie)
	paths := []map[string]any{}
	if fid == nil || asString(fid) == "" {
		path = normalizePath(path)
		if path == "/" {
			fid = 0
		} else {
			parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
			pathList := []string{}
			current := ""
			for _, part := range parts {
				if part == "" {
					continue
				}
				current += "/" + part
				pathList = append(pathList, current)
			}
			found, err := q.getFIDs(ctx, pathList)
			if err != nil {
				return nil, err
			}
			if len(found) == 0 {
				return nil, errors.New("获取 fid 失败")
			}
			fid = found[len(found)-1]["fid"]
			for index, item := range found {
				name := ""
				if index < len(parts) {
					name = parts[index]
				}
				paths = append(paths, map[string]any{"fid": item["fid"], "name": name})
			}
		}
	}
	response, err := q.listDir(ctx, fid)
	if err != nil {
		return nil, err
	}
	if number(response["code"]) != 0 {
		return nil, errors.New(asString(response["message"]))
	}
	return map[string]any{"fid": fid, "list": listValue(mapValue(response["data"])["list"]), "paths": paths}, nil
}

func pathToFID(ctx context.Context, cookie, path string) (any, error) {
	path = normalizePath(path)
	if path == "/" {
		return 0, nil
	}
	directory, name := splitPath(path)
	listing, err := fileList(ctx, cookie, nil, directory)
	if err != nil {
		return nil, err
	}
	for _, raw := range listValue(listing["list"]) {
		item := mapValue(raw)
		if asString(item["file_name"]) == name {
			return item["fid"], nil
		}
	}
	return nil, fmt.Errorf("未找到文件: %s", path)
}

func resolveDestructiveFID(ctx context.Context, cookie string, fid any, path string) (any, error) {
	fidText := ""
	switch value := fid.(type) {
	case nil:
	case string:
		fidText = strings.TrimSpace(value)
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || math.Trunc(value) != value {
			return nil, errors.New("fid 必须是非负整数或字符串")
		}
		fidText = strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return nil, errors.New("fid 必须是非负整数或字符串")
	}
	pathText := strings.TrimSpace(path)
	if fidText != "" && pathText != "" {
		return nil, errors.New("fid 与 path 只能提供一个")
	}
	if fidText != "" {
		if fidText == "0" {
			return nil, errors.New("不能操作根目录")
		}
		return fid, nil
	}
	if pathText == "" || normalizePath(pathText) == "/" {
		return nil, errors.New("必须提供文件 fid 或非根路径")
	}
	return pathToFID(ctx, cookie, pathText)
}

func normalizePath(path string) string {
	if path == "" {
		return "/"
	}
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' })
	return "/" + strings.Join(parts, "/")
}

func splitPath(path string) (string, string) {
	index := strings.LastIndex(path, "/")
	if index <= 0 {
		return "/", strings.TrimPrefix(path, "/")
	}
	return path[:index], path[index+1:]
}

func mapValue(value any) map[string]any {
	if result, ok := value.(map[string]any); ok && result != nil {
		return result
	}
	return map[string]any{}
}

func listValue(value any) []any {
	if result, ok := value.([]any); ok && result != nil {
		return result
	}
	return []any{}
}

func number(value any) float64 {
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

func osEnvBool(name string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func searchSuggestions(ctx context.Context, source map[string]any, keyword string, deep bool) ([]map[string]any, string, error) {
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	if keyword == "" {
		return nil, "", errors.New("搜索名称不能为空")
	}
	var all []map[string]any
	var refreshedToken string
	var mu sync.Mutex
	var wg sync.WaitGroup
	errCount := 0
	searchSource := func(fn func() ([]map[string]any, string, error)) {
		defer wg.Done()
		items, token, err := fn()
		mu.Lock()
		defer mu.Unlock()
		if token != "" {
			refreshedToken = token
		}
		if err != nil {
			errCount++
			return
		}
		all = append(all, items...)
	}
	cloud := mapValue(source["cloudsaver"])
	if asBoolDefault(cloud["enable"], true) && asString(cloud["server"]) != "" {
		wg.Add(1)
		go searchSource(func() ([]map[string]any, string, error) { return cloudSaverSearch(ctx, cloud, keyword) })
	}
	pan := mapValue(source["pansou"])
	if asBoolDefault(pan["enable"], true) && asString(pan["server"]) != "" {
		wg.Add(1)
		go searchSource(func() ([]map[string]any, string, error) {
			items, err := panSouSearch(ctx, pan, keyword, deep)
			return items, "", err
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	result := make([]map[string]any, 0, len(all))
	sort.SliceStable(all, func(i, j int) bool { return asString(all[i]["datetime"]) > asString(all[j]["datetime"]) })
	for _, item := range all {
		link := asString(item["shareurl"])
		if link != "" && !seen[link] {
			seen[link] = true
			result = append(result, item)
		}
	}
	if len(result) == 0 && errCount > 0 {
		return result, refreshedToken, errors.New("搜索源请求失败")
	}
	return result, refreshedToken, nil
}

func externalJSON(ctx context.Context, method, rawURL string, params url.Values, payload any, headers http.Header) (map[string]any, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if payload != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16*1024*1024))
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("搜索源返回非 JSON (%d)", response.StatusCode)
	}
	return decoded, nil
}

func cloudSaverSearch(ctx context.Context, config map[string]any, keyword string) ([]map[string]any, string, error) {
	server := strings.TrimRight(asString(config["server"]), "/")
	headers := http.Header{"Content-Type": {"application/json"}}
	refreshedToken := ""
	if token := asString(config["token"]); token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	query := url.Values{"keyword": {keyword}, "lastMessageId": {""}}
	response, err := externalJSON(ctx, http.MethodGet, server+"/api/search", query, nil, headers)
	if err != nil {
		return nil, refreshedToken, err
	}
	if !asBoolDefault(response["success"], false) && (asString(response["message"]) == "无效的 token" || asString(response["message"]) == "未提供 token") {
		login, loginErr := externalJSON(ctx, http.MethodPost, server+"/api/user/login", nil, map[string]any{"username": config["username"], "password": config["password"]}, headers)
		if loginErr != nil || !asBoolDefault(login["success"], false) {
			return nil, refreshedToken, errors.New("CloudSaver 登录失败")
		}
		if token := asString(mapValue(login["data"])["token"]); token != "" {
			refreshedToken = token
			headers.Set("Authorization", "Bearer "+token)
			response, err = externalJSON(ctx, http.MethodGet, server+"/api/search", query, nil, headers)
			if err != nil {
				return nil, refreshedToken, err
			}
		}
	}
	if !asBoolDefault(response["success"], false) {
		return nil, refreshedToken, errors.New(asString(response["message"]))
	}
	result := []map[string]any{}
	seen := map[string]bool{}
	for _, channel := range listValue(response["data"]) {
		for _, raw := range listValue(mapValue(channel)["list"]) {
			item := mapValue(raw)
			for _, rawLink := range listValue(item["cloudLinks"]) {
				link := mapValue(rawLink)
				if asString(link["cloudType"]) != "quark" {
					continue
				}
				share := asString(link["link"])
				if share == "" || seen[share] {
					continue
				}
				seen[share] = true
				title := asString(item["title"])
				if index := strings.IndexAny(title, ":："); index >= 0 && (strings.Contains(title[:index], "名称") || strings.Contains(title[:index], "标题")) {
					title = title[index+1:]
				}
				result = append(result, map[string]any{"shareurl": share, "taskname": strings.TrimSpace(strings.ReplaceAll(title, "&amp;", "&")), "content": strings.TrimSpace(asString(item["content"])), "datetime": asString(item["pubDate"]), "tags": item["tags"], "channel": item["channelId"], "source": "CloudSaver"})
			}
		}
	}
	return result, refreshedToken, nil
}

func panSouSearch(ctx context.Context, config map[string]any, keyword string, deep bool) ([]map[string]any, error) {
	server := strings.TrimRight(asString(config["server"]), "/")
	query := url.Values{"kw": {keyword}, "cloud_types": {"quark"}, "res": {"merge"}, "refresh": {fmt.Sprint(deep)}}
	response, err := externalJSON(ctx, http.MethodGet, server+"/api/search", query, nil, nil)
	if err != nil {
		return nil, err
	}
	if number(response["code"]) != 0 {
		return nil, errors.New(asString(response["message"]))
	}
	result := []map[string]any{}
	seen := map[string]bool{}
	data := mapValue(mapValue(response["data"])["merged_by_type"])
	for _, raw := range listValue(data["quark"]) {
		item := mapValue(raw)
		share := asString(item["url"])
		if share == "" || seen[share] {
			continue
		}
		seen[share] = true
		note := asString(item["note"])
		title, content := note, ""
		if index := strings.IndexAny(note, ":："); index >= 0 {
			title, content = note[:index], note[index+1:]
		}
		result = append(result, map[string]any{"shareurl": share, "taskname": strings.TrimSpace(title), "content": strings.TrimSpace(content), "datetime": asString(item["datetime"]), "channel": item["source"], "source": "PanSou"})
	}
	return result, nil
}
