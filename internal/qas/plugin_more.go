package qas

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type aria2Plugin struct {
	host, secret, dir, rpc string
	active                 bool
}

func (p *aria2Plugin) Name() string { return "aria2" }
func (p *aria2Plugin) Defaults() (map[string]any, map[string]any) {
	return map[string]any{"host_port": "172.17.0.1:6800", "secret": "", "dir": "/Downloads"}, map[string]any{"auto_download": false, "download_subdir": false, "save_path": "", "pause": false}
}
func (p *aria2Plugin) Active() bool { return p.active }
func (p *aria2Plugin) Init(cfg map[string]any, log func(string, ...any)) {
	p.host, p.secret, p.dir = cfgString(cfg, "host_port"), cfgString(cfg, "secret"), cfgString(cfg, "dir")
	if p.host == "" || p.secret == "" {
		return
	}
	if strings.Contains(p.host, "://") {
		p.rpc = strings.TrimRight(p.host, "/")
		if !strings.Contains(p.rpc[strings.Index(p.rpc, "://")+3:], "/") {
			p.rpc += "/jsonrpc"
		}
	} else {
		p.rpc = "http://" + p.host + "/jsonrpc"
	}
	res := p.rpcCall(context.Background(), "aria2.getVersion", nil)
	if mapValue(res["result"])["version"] != nil {
		log("Aria2下载: v%s", asString(mapValue(res["result"])["version"]))
		p.active = true
	} else {
		log("Aria2下载: 连接失败%v", res["error"])
	}
}
func (p *aria2Plugin) rpcCall(ctx context.Context, method string, params []any) map[string]any {
	if params == nil {
		params = []any{}
	}
	if p.secret != "" {
		params = append([]any{"token:" + p.secret}, params...)
	}
	data, _, _, err := pluginHTTP(ctx, http.MethodPost, p.rpc, nil, map[string]any{"jsonrpc": "2.0", "id": "quark-auto-save", "method": method, "params": params}, nil)
	if err != nil {
		return map[string]any{}
	}
	return data
}
func (p *aria2Plugin) Run(ctx context.Context, task map[string]any, acc *quarkAccount, tree *saveTree, log func(string, ...any)) map[string]any {
	cfg := mapValue(mapValue(task["addition"])["aria2"])
	if !asBoolDefault(cfg["auto_download"], false) || tree == nil || acc == nil {
		return nil
	}
	var fids []any
	var paths []string
	nodes := tree.all()
	// sort by path
	for i := 0; i < len(nodes); i++ {
		for j := i + 1; j < len(nodes); j++ {
			if asString(nodes[j].Data["path"]) < asString(nodes[i].Data["path"]) {
				nodes[i], nodes[j] = nodes[j], nodes[i]
			}
		}
	}
	for _, node := range nodes {
		if !asBoolDefault(node.Data["is_dir"], true) {
			fids = append(fids, node.Data["fid"])
			paths = append(paths, asString(node.Data["path"]))
		} else if node.ID != tree.Root && asBoolDefault(cfg["download_subdir"], false) {
			p.walkDir(ctx, acc, asString(node.Data["fid"]), asString(node.Data["path"]), &fids, &paths, log)
		}
	}
	if len(fids) == 0 {
		log("Aria2下载: 没有下载任务，跳过")
		return nil
	}
	dl, cookie, err := acc.download(ctx, fids)
	if err != nil {
		log("Aria2下载: 错误%s", err)
		return nil
	}
	if cookie == "" {
		cookie = acc.cookie
	}
	urls := listValue(dl["data"])
	for i, item := range urls {
		if i >= len(paths) {
			break
		}
		filePath := paths[i]
		log("📥 Aria2下载: %s", filePath)
		local := p.dir + filePath
		if asString(cfg["save_path"]) != "" {
			local = p.dir + "/" + strings.Trim(asString(cfg["save_path"]), "/") + "/" + filepath.Base(filePath)
		}
		p.rpcCall(ctx, "aria2.addUri", []any{[]any{asString(mapValue(item)["download_url"])}, map[string]any{
			"header": []any{"Cookie: " + cookie, "User-Agent: " + quarkUserAgent},
			"out":    filepath.Base(local),
			"dir":    filepath.Dir(local),
			"pause":  strings.ToLower(fmt.Sprint(asBoolDefault(cfg["pause"], false))),
		}})
	}
	return nil
}
func (p *aria2Plugin) walkDir(ctx context.Context, acc *quarkAccount, fid, parent string, fids *[]any, paths *[]string, log func(string, ...any)) {
	log("Aria2下载: 递归目录 %s", parent)
	listing, err := acc.listDir(ctx, fid)
	if err != nil {
		return
	}
	for _, item := range listValue(mapValue(listing["data"])["list"]) {
		value := mapValue(item)
		itemPath := asString(value["file_name"])
		if parent != "" {
			itemPath = parent + "/" + itemPath
		}
		if asBoolDefault(value["dir"], false) {
			p.walkDir(ctx, acc, asString(value["fid"]), itemPath, fids, paths, log)
		} else {
			*fids = append(*fids, value["fid"])
			*paths = append(*paths, itemPath)
		}
	}
}

type autoUnarchivePlugin struct {
	global    bool
	max       int
	autoClean bool
	cleanDir  bool
}

func (p *autoUnarchivePlugin) Name() string { return "auto_unarchive" }
func (p *autoUnarchivePlugin) Defaults() (map[string]any, map[string]any) {
	return map[string]any{"tips_": "自动云解压(zip|rar|7z)到保存目录，在任务插件选项中启用，该功能需SVIP支持", "global_enable": false, "max_concurrent": 3}, map[string]any{"enable": false, "auto_clean": true, "auto_clean_zipdir": false}
}
func (p *autoUnarchivePlugin) Active() bool { return true }
func (p *autoUnarchivePlugin) Init(cfg map[string]any, _ func(string, ...any)) {
	p.global = asBoolDefault(cfg["global_enable"], false)
	p.max = int(number(cfg["max_concurrent"]))
	if p.max <= 0 {
		p.max = 3
	}
}
func (p *autoUnarchivePlugin) Run(ctx context.Context, task map[string]any, acc *quarkAccount, tree *saveTree, log func(string, ...any)) map[string]any {
	cfg := mapValue(mapValue(task["addition"])["auto_unarchive"])
	if !p.global && !asBoolDefault(cfg["enable"], false) {
		return task
	}
	p.autoClean = asBoolDefault(cfg["auto_clean"], true)
	p.cleanDir = asBoolDefault(cfg["auto_clean_zipdir"], false)
	savepath := regexp.MustCompile(`/{2,}`).ReplaceAllString("/"+asString(task["savepath"]), "/")
	target := acc.savepathFID[savepath]
	if target == "" || tree == nil {
		return task
	}
	zipRE := regexp.MustCompile(`(?i)\.(zip|rar|7z)$`)
	var wait []*saveNode
	for _, node := range tree.all() {
		if node.Data != nil && !asBoolDefault(node.Data["is_dir"], false) && zipRE.MatchString(asString(node.Tag)) {
			wait = append(wait, node)
		}
	}
	if len(wait) == 0 {
		return task
	}
	log("📦 [%s] 共有 %d 个任务，控制并发数为: %d", asString(task["taskname"]), len(wait), p.max)
	type active struct{ taskID, zipFID, main, zipName string }
	var running []active
	var move, clean []any
	for len(wait) > 0 || len(running) > 0 {
		for len(running) < p.max && len(wait) > 0 {
			node := wait[0]
			wait = wait[1:]
			zipFID := asString(node.Data["fid"])
			zipName := asString(node.Data["file_name_re"])
			main := strings.TrimSuffix(zipName, filepath.Ext(zipName))
			res, err := acc.unarchive(ctx, zipFID, target)
			if err == nil && number(res["code"]) == 0 {
				running = append(running, active{asString(mapValue(res["data"])["task_id"]), zipFID, main, zipName})
				log("  ▶️ 提交解压: %s", zipName)
			} else {
				log("  ❌ 提交失败: %s (%s)", zipName, asString(res["message"]))
				if strings.Contains(asString(res["message"]), "concurrent") {
					wait = append([]*saveNode{node}, wait...)
					break
				}
			}
			time.Sleep(time.Second)
		}
		still := running[:0]
		for _, job := range running {
			q, _ := acc.queryTask(ctx, job.taskID, func(string) {})
			if number(q["code"]) == 0 {
				log("  ✅ 解压完成: %s", job.zipName)
				p.finish(ctx, acc, job.main, job.zipName, job.zipFID, q, target, &move, &clean, log)
			} else if number(q["code"]) == 1 {
				still = append(still, job)
			} else {
				log("  ⚠️ 任务异常: %s %s", job.zipName, asString(q["message"]))
			}
		}
		running = still
		if len(running) > 0 {
			time.Sleep(5 * time.Second)
		}
	}
	if len(move) > 0 {
		log("🚀 任务全部解压完成，开始批量移动 %d 个文件...", len(move))
		if moved, _ := acc.moveFiles(ctx, move, target); number(moved["code"]) == 0 && len(clean) > 0 {
			acc.delete(ctx, clean)
			log("🧹 批量清理完成")
		}
	}
	return task
}
func (p *autoUnarchivePlugin) finish(ctx context.Context, acc *quarkAccount, main, zipName, zipFID string, q map[string]any, target string, move, clean *[]any, log func(string, ...any)) {
	var sub string
	for _, item := range listValue(mapValue(mapValue(q["data"])["unarchive_result"])["list"]) {
		value := mapValue(item)
		if asString(value["file_name"]) == main {
			sub = asString(value["fid"])
			break
		}
	}
	if sub == "" {
		return
	}
	if p.autoClean {
		*clean = append(*clean, zipFID)
		if p.cleanDir {
			*clean = append(*clean, sub)
		} else {
			acc.rename(ctx, sub, zipName)
		}
	} else {
		*clean = append(*clean, sub)
	}
	listing, _ := acc.listDir(ctx, sub)
	items := listValue(mapValue(listing["data"])["list"])
	for _, item := range items {
		*move = append(*move, mapValue(item)["fid"])
	}
	if len(items) == 1 {
		item := mapValue(items[0])
		newName := main + filepath.Ext(asString(item["file_name"]))
		acc.rename(ctx, item["fid"], newName)
		log("    └─ 重命名: %s", newName)
	}
}

type fnvPlugin struct {
	base, app, user, pass, secret, api, token string
	active                                    bool
}

func (p *fnvPlugin) Name() string { return "fnv" }
func (p *fnvPlugin) Defaults() (map[string]any, map[string]any) {
	return map[string]any{"base_url": "http://10.0.0.6:5666", "app_name": "trimemedia-web", "username": "", "password": "", "secret_string": "", "api_key": "", "token": nil}, map[string]any{"auto_refresh": false, "mdb_name": "", "mdb_dir_list": ""}
}
func (p *fnvPlugin) Active() bool { return p.active }
func (p *fnvPlugin) Init(cfg map[string]any, log func(string, ...any)) {
	p.base, p.app, p.user, p.pass = strings.TrimRight(cfgString(cfg, "base_url"), "/"), cfgString(cfg, "app_name"), cfgString(cfg, "username"), cfgString(cfg, "password")
	p.secret, p.api, p.token = cfgString(cfg, "secret_string"), cfgString(cfg, "api_key"), cfgString(cfg, "token")
	if p.base == "" || p.user == "" || p.pass == "" || p.secret == "" || p.api == "" {
		return
	}
	if p.token == "" {
		p.login(context.Background(), log)
	}
	p.active = p.token != ""
	if p.active {
		log("fnv: 插件已激活 ✅")
	}
}
func fnvMD5(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
func (p *fnvPlugin) sign(method, rel string, params url.Values, data map[string]any) string {
	nonce := fmt.Sprintf("%d", 100000+time.Now().UnixNano()%900000)
	ts := fmt.Sprintf("%d", time.Now().UnixMilli())
	serialized := ""
	if strings.ToLower(method) == "get" && params != nil {
		serialized = params.Encode()
	} else if data != nil {
		raw, _ := json.Marshal(data)
		serialized = string(raw)
	}
	bodyHash := fnvMD5(serialized)
	final := fnvMD5(strings.Join([]string{p.secret, rel, nonce, ts, bodyHash, p.api}, "_"))
	return "nonce=" + nonce + "&timestamp=" + ts + "&sign=" + final
}
func (p *fnvPlugin) call(ctx context.Context, method, rel string, data map[string]any, log func(string, ...any)) map[string]any {
	for i := 0; i < 3; i++ {
		headers := map[string]string{"Content-Type": "application/json", "authx": p.sign(method, rel, nil, data)}
		if p.token != "" {
			headers["Authorization"] = p.token
		}
		res, _, _, err := pluginHTTP(ctx, method, p.base+rel, headers, data, nil)
		if err != nil {
			return nil
		}
		if number(res["code"]) == 0 {
			return res
		}
		if number(res["code"]) == -2 && rel != "/v/api/v1/login" {
			p.login(ctx, log)
			continue
		}
		return res
	}
	return nil
}
func (p *fnvPlugin) login(ctx context.Context, log func(string, ...any)) bool {
	log("飞牛影视: 正在尝试登录...")
	res := p.call(ctx, http.MethodPost, "/v/api/v1/login", map[string]any{"username": p.user, "password": p.pass, "app_name": p.app}, log)
	p.token = asString(mapValue(res["data"])["token"])
	return p.token != ""
}
func (p *fnvPlugin) Run(ctx context.Context, task map[string]any, _ *quarkAccount, _ *saveTree, log func(string, ...any)) map[string]any {
	if !p.active {
		return nil
	}
	cfg := mapValue(mapValue(task["addition"])["fnv"])
	if !asBoolDefault(cfg["auto_refresh"], false) || asString(cfg["mdb_name"]) == "" {
		return nil
	}
	res := p.call(ctx, http.MethodGet, "/v/api/v1/mdb/list", nil, log)
	var id string
	for _, item := range listValue(res["data"]) {
		if asString(mapValue(item)["name"]) == asString(cfg["mdb_name"]) {
			id = asString(mapValue(item)["guid"])
			break
		}
	}
	if id == "" {
		log("飞牛影视: 未在媒体库列表中找到名为 '%s' 的媒体库 ❌", asString(cfg["mdb_name"]))
		return nil
	}
	var dirs []string
	for _, d := range strings.Split(asString(cfg["mdb_dir_list"]), ",") {
		if s := strings.TrimSpace(d); s != "" {
			dirs = append(dirs, s)
		}
	}
	body := map[string]any{}
	if len(dirs) > 0 {
		body["dir_list"] = dirs
	}
	p.call(ctx, http.MethodPost, "/v/api/v1/mdb/scan/"+id, body, log)
	log("飞牛影视: 发送刷新指令成功 ✅")
	return nil
}

func alistHeaders(token string) map[string]string {
	return map[string]string{"Authorization": token, "Content-Type": "application/json"}
}

type alistPlugin struct {
	url, token, storageID, mount, quarkRoot string
	active                                  bool
}

func (p *alistPlugin) Name() string { return "alist" }
func (p *alistPlugin) Defaults() (map[string]any, map[string]any) {
	return map[string]any{"url": "", "token": "", "storage_id": ""}, nil
}
func (p *alistPlugin) Active() bool { return p.active }
func (p *alistPlugin) Init(cfg map[string]any, log func(string, ...any)) {
	p.url, p.token, p.storageID = strings.TrimRight(cfgString(cfg, "url"), "/"), cfgString(cfg, "token"), cfgString(cfg, "storage_id")
	if p.url == "" || p.token == "" {
		return
	}
	data, _, _, err := pluginHTTP(context.Background(), http.MethodGet, p.url+"/api/admin/setting/list?group=1", alistHeaders(p.token), nil, nil)
	if err != nil || number(data["code"]) != 200 {
		return
	}
	ok, mount, root := storageToPath(context.Background(), p.url, p.token, p.storageID, log)
	if ok {
		p.mount, p.quarkRoot, p.active = mount, root, true
	}
}
func (p *alistPlugin) Run(ctx context.Context, task map[string]any, _ *quarkAccount, _ *saveTree, log func(string, ...any)) map[string]any {
	if asString(task["savepath"]) == "" || !strings.HasPrefix(asString(task["savepath"]), p.quarkRoot) {
		return nil
	}
	alistPath := pathJoin(p.mount, strings.TrimPrefix(strings.TrimPrefix(asString(task["savepath"]), p.quarkRoot), "/"))
	p.refresh(ctx, alistPath, log)
	return nil
}
func (p *alistPlugin) refresh(ctx context.Context, path string, log func(string, ...any)) {
	data, _, _, err := pluginHTTP(ctx, http.MethodPost, p.url+"/api/fs/list", alistHeaders(p.token), map[string]any{"path": path, "refresh": true, "password": "", "page": 1, "per_page": 0}, nil)
	if err == nil && number(data["code"]) == 200 {
		log("📁 Alist刷新：目录[%s] 成功✅", path)
		return
	}
	if strings.Contains(asString(data["message"]), "object not found") {
		if path == "/" || path == p.mount {
			log("📁 Alist刷新：根目录不存在，请检查 Alist 配置")
			return
		}
		p.refresh(ctx, filepath.Dir(path), log)
		return
	}
	log("📁 Alist刷新：失败❌ %s", asString(data["message"]))
}

func pathJoin(a, b string) string {
	return strings.ReplaceAll(filepath.ToSlash(filepath.Clean(a+"/"+b)), "//", "/")
}

func storageToPath(ctx context.Context, base, token, storageID string, log func(string, ...any)) (bool, string, string) {
	if match := regexp.MustCompile(`^(/[^:]*):(/[^:]*)$`).FindStringSubmatch(storageID); len(match) == 3 {
		return true, match[1], match[2]
	}
	if !regexp.MustCompile(`^\d+$`).MatchString(storageID) {
		return false, "", ""
	}
	data, _, _, err := pluginHTTP(ctx, http.MethodGet, base+"/api/admin/storage/get?id="+url.QueryEscape(storageID), alistHeaders(token), nil, nil)
	if err != nil || number(data["code"]) != 200 {
		return false, "", ""
	}
	info := mapValue(data["data"])
	if asString(info["driver"]) != "Quark" {
		log("Alist: 不支持[%s]驱动 ❌", asString(info["driver"]))
		return false, "", ""
	}
	var addition map[string]any
	_ = json.Unmarshal([]byte(asString(info["addition"])), &addition)
	root := "/"
	if asString(addition["root_folder_id"]) != "0" && asString(addition["root_folder_id"]) != "" {
		if q := NewQuarkClient(asString(addition["cookie"])); q != nil {
			if path, err := q.listFullPath(ctx, asString(addition["root_folder_id"])); err == nil && path != "" {
				root = path
			}
		}
	}
	return true, asString(info["mount_path"]), root
}

type alistStrmGenPlugin struct {
	url, token, storageID, saveDir, host, mount, quarkRoot, server string
	active                                                         bool
}

func (p *alistStrmGenPlugin) Name() string { return "alist_strm_gen" }
func (p *alistStrmGenPlugin) Defaults() (map[string]any, map[string]any) {
	return map[string]any{"tips_alist_refresh": "该插件需与 alist 刷新插件配合使用，否则可能出现 alist 未刷新导致无法生成 strm 的问题！", "url": "", "token": "", "storage_id": "", "strm_save_dir": "/media", "strm_replace_host": ""}, map[string]any{"auto_gen": true}
}
func (p *alistStrmGenPlugin) Active() bool { return p.active }
func (p *alistStrmGenPlugin) Init(cfg map[string]any, log func(string, ...any)) {
	p.url, p.token, p.storageID = strings.TrimRight(cfgString(cfg, "url"), "/"), cfgString(cfg, "token"), cfgString(cfg, "storage_id")
	p.saveDir, p.host = cfgString(cfg, "strm_save_dir"), strings.TrimSpace(cfgString(cfg, "strm_replace_host"))
	if p.url == "" || p.token == "" || p.storageID == "" {
		return
	}
	ok, mount, root := storageToPath(context.Background(), p.url, p.token, p.storageID, log)
	if !ok {
		return
	}
	p.mount, p.quarkRoot, p.active = mount, root, true
	if p.host != "" {
		if strings.HasPrefix(p.host, "http") {
			p.server = strings.TrimRight(p.host, "/") + "/d"
		} else {
			p.server = "http://" + p.host + "/d"
		}
	} else {
		p.server = p.url + "/d"
	}
	log("Alist-Strm生成: [%s:%s]", mount, root)
}
func (p *alistStrmGenPlugin) Run(ctx context.Context, task map[string]any, _ *quarkAccount, _ *saveTree, log func(string, ...any)) map[string]any {
	cfg := mapValue(mapValue(task["addition"])["alist_strm_gen"])
	if !asBoolDefault(cfg["auto_gen"], true) || !strings.HasPrefix(asString(task["savepath"]), p.quarkRoot) {
		return nil
	}
	alistPath := pathJoin(p.mount, strings.TrimPrefix(strings.TrimPrefix(asString(task["savepath"]), p.quarkRoot), "/"))
	p.checkDir(ctx, alistPath, log)
	return nil
}
func (p *alistStrmGenPlugin) checkDir(ctx context.Context, path string, log func(string, ...any)) {
	data, _, _, err := pluginHTTP(ctx, http.MethodPost, p.url+"/api/fs/list", alistHeaders(p.token), map[string]any{"path": path, "refresh": false, "password": "", "page": 1, "per_page": 0}, nil)
	if err != nil || number(data["code"]) != 200 {
		log("📺 Alist-Strm生成: 获取文件列表失败❌%s", asString(data["message"]))
		return
	}
	for _, item := range listValue(mapValue(data["data"])["content"]) {
		value := mapValue(item)
		itemPath := strings.ReplaceAll(path+"/"+asString(value["name"]), "//", "/")
		if asBoolDefault(value["is_dir"], false) {
			p.checkDir(ctx, itemPath, log)
			continue
		}
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(itemPath), "."))
		switch ext {
		case "mp4", "mkv", "flv", "mov", "m4v", "avi", "webm", "wmv":
			strm := strings.ReplaceAll(p.saveDir+strings.TrimSuffix(itemPath, filepath.Ext(itemPath))+".strm", "//", "/")
			if _, err := os.Stat(strm); err == nil {
				continue
			}
			_ = os.MkdirAll(filepath.Dir(strm), 0o755)
			sign := ""
			if asString(value["sign"]) != "" {
				sign = "?sign=" + asString(value["sign"])
			}
			_ = os.WriteFile(strm, []byte(p.server+itemPath+sign), 0o644)
			log("📺 生成STRM文件 %s 成功✅", strm)
		}
	}
}

type alistStrmPlugin struct {
	url, cookie, configID string
	active                bool
}

func (p *alistStrmPlugin) Name() string { return "alist_strm" }
func (p *alistStrmPlugin) Defaults() (map[string]any, map[string]any) {
	return map[string]any{"url": "", "cookie": "", "config_id": ""}, nil
}
func (p *alistStrmPlugin) Active() bool { return p.active }
func (p *alistStrmPlugin) Init(cfg map[string]any, log func(string, ...any)) {
	p.url, p.cookie, p.configID = strings.TrimRight(cfgString(cfg, "url"), "/"), cfgString(cfg, "cookie"), cfgString(cfg, "config_id")
	if p.url == "" || p.cookie == "" || p.configID == "" {
		return
	}
	_, _, raw, err := pluginHTTP(context.Background(), http.MethodGet, p.url+"/configs", map[string]string{"Cookie": p.cookie}, nil, nil)
	if err == nil && regexp.MustCompile(`value="(\d*)">\s*<strong>名称:</strong>([^<]+)`).MatchString(raw) {
		p.active = true
		log("alist-strm配置运行: 已连接")
	}
}
func (p *alistStrmPlugin) Run(ctx context.Context, _ map[string]any, _ *quarkAccount, _ *saveTree, log func(string, ...any)) map[string]any {
	form := url.Values{"action": {"run_selected"}}
	for _, id := range strings.Split(p.configID, ",") {
		id = strings.TrimSpace(id)
		if id != "" {
			form.Add("selected_configs", id)
		}
	}
	_, code, raw, err := pluginHTTP(ctx, http.MethodPost, p.url+"/run_selected_configs", map[string]string{"Cookie": p.cookie, "Content-Type": "application/x-www-form-urlencoded"}, nil, form)
	if err == nil && code == 200 {
		if m := regexp.MustCompile(`role="alert">\s*([^<]+)\s*<button`).FindStringSubmatch(raw); len(m) == 2 {
			log("🔗 alist-strm配置运行: %s✅", strings.TrimSpace(m[1]))
			return nil
		}
	}
	log("🔗 alist-strm配置运行: 失败❌")
	return nil
}

type alistSyncPlugin struct {
	url, token, quarkID, saveID, tv string
	active                          bool
}

func (p *alistSyncPlugin) Name() string { return "alist_sync" }
func (p *alistSyncPlugin) Defaults() (map[string]any, map[string]any) {
	return map[string]any{"url": "", "token": "", "quark_storage_id": "", "save_storage_id": "", "tv_mode": ""}, map[string]any{"enable": false, "save_path": "", "verify_path": "", "full_path_mode": false}
}
func (p *alistSyncPlugin) Active() bool { return p.active }
func (p *alistSyncPlugin) Init(cfg map[string]any, log func(string, ...any)) {
	p.url, p.token = strings.TrimRight(cfgString(cfg, "url"), "/"), cfgString(cfg, "token")
	p.quarkID, p.saveID, p.tv = cfgString(cfg, "quark_storage_id"), cfgString(cfg, "save_storage_id"), cfgString(cfg, "tv_mode")
	if p.url == "" || p.token == "" {
		return
	}
	data, _, _, err := pluginHTTP(context.Background(), http.MethodGet, p.url+"/api/me", alistHeaders(p.token), nil, nil)
	if err == nil && number(data["code"]) == 200 && asString(mapValue(data["data"])["username"]) != "guest" {
		log("Alist登陆成功，当前用户: %s", asString(mapValue(data["data"])["username"]))
		p.active = true
	}
}
func (p *alistSyncPlugin) Run(ctx context.Context, task map[string]any, _ *quarkAccount, _ *saveTree, log func(string, ...any)) map[string]any {
	cfg := mapValue(mapValue(task["addition"])["alist_sync"])
	if !asBoolDefault(cfg["enable"], false) {
		return nil
	}
	log("开始进行alist同步")
	srcData, _, _, err := pluginHTTP(ctx, http.MethodGet, p.url+"/api/admin/storage/get?id="+url.QueryEscape(p.quarkID), alistHeaders(p.token), nil, nil)
	if err != nil || number(srcData["code"]) != 200 {
		return nil
	}
	src := mapValue(srcData["data"])
	if asString(src["driver"]) != "Quark" {
		log("Alist同步: 存储%s非夸克存储❌ %s", p.quarkID, asString(src["driver"]))
		return nil
	}
	quarkMount := asString(src["mount_path"])
	dstMount := ""
	if p.saveID != "" && p.saveID != "0" {
		dstData, _, _, _ := pluginHTTP(ctx, http.MethodGet, p.url+"/api/admin/storage/get?id="+url.QueryEscape(p.saveID), alistHeaders(p.token), nil, nil)
		dstMount = asString(mapValue(mapValue(dstData["data"]))["mount_path"])
		if dstMount == "" {
			dstMount = asString(mapValue(dstData["data"])["mount_path"])
		}
	}
	savePath := asString(cfg["save_path"])
	if savePath == "" {
		savePath = dstMount + "/" + asString(task["savepath"])
	} else if !asBoolDefault(cfg["full_path_mode"], false) {
		savePath = dstMount + "/" + strings.Trim(savePath, "/")
	}
	sourcePath := quarkMount + "/" + asString(task["savepath"])
	srcList := p.list(ctx, sourcePath)
	if srcList == nil {
		log("获取夸克文件列表失败，请检查网络或手动刷新alist中的夸克目录")
		return nil
	}
	verify := asString(cfg["verify_path"])
	if verify == "" {
		verify = savePath
	}
	exist := p.list(ctx, verify)
	var names []string
	tv := p.tv != "" && p.tv != "0"
	taskname := asString(task["taskname"])
	for _, item := range srcList {
		name := asString(item["name"])
		if asBoolDefault(item["is_dir"], false) {
			continue
		}
		skip := false
		for _, other := range exist {
			if asString(other["name"]) == name || (tv && strings.EqualFold(strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(name), strings.ToLower(taskname+".")), filepath.Ext(name)), strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(asString(other["name"])), strings.ToLower(taskname+".")), filepath.Ext(asString(other["name"]))))) {
				skip = true
				break
			}
		}
		if tv && !regexp.MustCompile("(?i)"+regexp.QuoteMeta(taskname)+`\.s\d{1,3}e\d{1,3}\.(mkv|mp4)`).MatchString(name) {
			skip = true
		}
		if !skip {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		log("没有需要同步的文件")
		return nil
	}
	pluginHTTP(ctx, http.MethodPost, p.url+"/api/fs/copy", alistHeaders(p.token), map[string]any{"src_dir": sourcePath, "dst_dir": savePath, "names": names}, nil)
	log("Alist创建任务成功")
	for _, name := range names {
		log("└── 🎞️%s", name)
	}
	return nil
}
func (p *alistSyncPlugin) list(ctx context.Context, path string) []map[string]any {
	data, _, _, err := pluginHTTP(ctx, http.MethodPost, p.url+"/api/fs/list", alistHeaders(p.token), map[string]any{"path": path, "password": "", "page": 1, "per_page": 0, "refresh": true}, nil)
	if err != nil || number(data["code"]) != 200 {
		return nil
	}
	var out []map[string]any
	for _, item := range listValue(mapValue(data["data"])["content"]) {
		out = append(out, mapValue(item))
	}
	return out
}

var _ = strconv.Itoa
