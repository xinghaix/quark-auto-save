package qas

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

type Engine struct {
	store *ConfigStore
	logs  *LogBuffer
}

type quarkAccount struct {
	*QuarkClient
	index       int
	isActive    bool
	nickname    string
	savepathFID map[string]string
}

func newAccount(cookie string, index int) *quarkAccount {
	return &quarkAccount{QuarkClient: NewQuarkClient(cookie), index: index + 1, savepathFID: map[string]string{"/": "0"}}
}

func (e *Engine) Run(ctx context.Context, tasks []map[string]any, test bool, cookies []string, pushConfig map[string]any, onLine func(string)) (int, bool, error) {
	if onLine == nil {
		onLine = func(string) {}
	}
	log := func(format string, args ...any) {
		msg := format
		if len(args) > 0 {
			msg = fmt.Sprintf(format, args...)
		}
		for _, line := range strings.Split(msg, "\n") {
			onLine(line)
		}
	}
	start := time.Now()
	log("===============程序开始===============")
	log("⏰ 执行时间: %s", start.Format("2006-01-02 15:04:05"))
	log("")
	cfg := e.store.Snapshot()
	notifies := []string{}
	addNotify := func(text string) { notifies = append(notifies, text) }

	if test {
		log("===============通知测试===============")
		if pushConfig == nil {
			pushConfig = map[string]any{}
		}
		sendNotify(pushConfig, "【夸克自动转存】", fmt.Sprintf("通知测试\n\n%s", time.Now().Format("2006-01-02 15:04:05")), log)
		log("")
		if len(cookies) > 0 {
			log("===============转存测试===============")
			acc := newAccount(cookies[0], 0)
			acc.doSaveCheck(ctx, "https://pan.quark.cn/s/1ed94d530d63", "/来自：分享", log)
			log("")
		}
		log("===============程序结束===============")
		log("😃 运行时长: %.2fs", time.Since(start).Seconds())
		return 0, ctx.Err() == context.DeadlineExceeded, nil
	}

	allCookies := e.store.Cookies()
	if len(allCookies) == 0 {
		log("❌ cookie 未配置")
		return 0, false, nil
	}
	accounts := make([]*quarkAccount, 0, len(allCookies))
	for i, cookie := range allCookies {
		accounts = append(accounts, newAccount(cookie, i))
	}
	specific := tasks != nil
	log("===============签到任务===============")
	if specific {
		verifyAccount(ctx, accounts[0], addNotify, log)
	} else {
		for _, acc := range accounts {
			verifyAccount(ctx, acc, addNotify, log)
			doSign(ctx, acc, cfg, addNotify, log)
		}
	}
	log("")
	if accounts[0].isActive {
		log("===============转存任务===============")
		tasklist := tasks
		if !specific {
			tasklist = taskMaps(cfg["tasklist"])
		}
		doSave(ctx, accounts[0], cfg, tasklist, addNotify, log)
		log("")
	}
	if len(notifies) > 0 {
		log("===============推送通知===============")
		sendNotify(mapValue(cfg["push_config"]), "【夸克自动转存】", strings.Join(notifies, "\n"), log)
		log("")
	}
	if err := e.store.Replace(cfg); err != nil {
		log("❌ 写回配置失败: %s", err)
	}
	log("===============程序结束===============")
	log("😃 运行时长: %.2fs", time.Since(start).Seconds())
	log("")
	return 0, ctx.Err() == context.DeadlineExceeded, nil
}

func verifyAccount(ctx context.Context, acc *quarkAccount, addNotify func(string), log func(string, ...any)) bool {
	log("▶️ 验证第%d个账号", acc.index)
	if !strings.Contains(acc.cookie, "__uid") {
		log("💡 不存在cookie必要参数，判断为仅签到")
		return false
	}
	info, err := acc.accountInfo(ctx)
	data := mapValue(info["data"])
	if err != nil || len(data) == 0 {
		addNotify(fmt.Sprintf("👤 第%d个账号登录失败，cookie无效❌", acc.index))
		return false
	}
	acc.isActive = true
	acc.nickname = asString(data["nickname"])
	log("👤 账号昵称: %s✅", acc.nickname)
	return true
}

func formatBytes(size float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB", "EB", "ZB", "YB"}
	i := 0
	for size >= 1024 && i < len(units)-1 {
		size /= 1024
		i++
	}
	return fmt.Sprintf("%.2f %s", size, units[i])
}

func doSign(ctx context.Context, acc *quarkAccount, cfg map[string]any, addNotify func(string), log func(string, ...any)) {
	if len(acc.mparam) != 3 {
		log("⏭️ 移动端参数未设置，跳过签到")
		log("")
		return
	}
	info, err := acc.growthInfo(ctx)
	data := mapValue(info["data"])
	if err != nil || len(data) == 0 {
		log("⏭️ 签到进度读取异常，可能登录失效，跳过签到")
		log("")
		return
	}
	vip := map[string]string{"NORMAL": "普通用户", "EXP_SVIP": "88VIP", "SUPER_VIP": "SVIP", "Z_VIP": "SVIP+"}
	member := asString(data["member_type"])
	if alias, ok := vip[member]; ok {
		member = alias
	}
	growth := fmt.Sprintf("💾 %s 总空间：%s，签到累计获得：%s", member, formatBytes(number(data["total_capacity"])), formatBytes(number(mapValue(data["cap_composition"])["sign_reward"])))
	capSign := mapValue(data["cap_sign"])
	if asBoolDefault(capSign["sign_daily"], false) {
		log("📅 签到记录: 今日已签到+%.0fMB，连签进度(%.0f/%.0f)✅", number(capSign["sign_daily_reward"])/1024/1024, number(capSign["sign_progress"]), number(capSign["sign_target"]))
		log(growth)
		log("")
		return
	}
	signResp, err := acc.growthSign(ctx)
	signData := mapValue(signResp["data"])
	if err == nil && len(signData) > 0 {
		msg := fmt.Sprintf("📅 执行签到: 今日签到+%.0fMB，连签进度(%.0f/%.0f)✅\n%s", number(signData["sign_daily_reward"])/1024/1024, number(capSign["sign_progress"])+1, number(capSign["sign_target"]), growth)
		notify := strings.ToLower(asString(mapValue(cfg["push_config"])["QUARK_SIGN_NOTIFY"]))
		if notify == "false" || os.Getenv("QUARK_SIGN_NOTIFY") == "false" {
			log(msg)
		} else {
			addNotify(strings.Replace(msg, "今日", fmt.Sprintf("[%s]今日", acc.nickname), 1))
		}
	} else {
		log("📅 签到异常: %s", asString(signResp["message"]))
	}
	log("")
}

func taskInWindow(task map[string]any) bool {
	if end := asString(task["enddate"]); end != "" {
		day, err := time.Parse("2006-01-02", end)
		if err == nil && time.Now().Format("2006-01-02") > day.Format("2006-01-02") {
			return false
		}
	}
	if raw, ok := task["runweek"]; ok {
		allowed := map[int]bool{}
		for _, item := range listValue(raw) {
			allowed[int(number(item))] = true
		}
		if len(allowed) > 0 {
			wd := int(time.Now().Weekday())
			if wd == 0 {
				wd = 7
			}
			if !allowed[wd] {
				return false
			}
		}
	}
	return true
}

func doSave(ctx context.Context, acc *quarkAccount, cfg map[string]any, tasklist []map[string]any, addNotify func(string), log func(string, ...any)) {
	log("🧩 载入插件")
	plugins := loadPlugins(mapValue(cfg["plugins"]), os.Getenv("PLUGIN_FLAGS"), log)
	if cfg["plugins"] == nil {
		cfg["plugins"] = map[string]any{}
	}
	for _, p := range plugins {
		defaults, _ := p.Defaults()
		if _, ok := mapValue(cfg["plugins"])[p.Name()]; !ok {
			mapValue(cfg["plugins"])[p.Name()] = defaults
		}
	}
	log("")
	log("转存账号: %s", acc.nickname)
	acc.updateSavepathFID(ctx, tasklist, log)
	taskPluginDefaults := map[string]any{}
	for _, p := range plugins {
		_, taskCfg := p.Defaults()
		if len(taskCfg) > 0 {
			taskPluginDefaults[p.Name()] = taskCfg
		}
	}
	for index, task := range tasklist {
		log("")
		log("#%d------------------", index+1)
		log("任务名称: %s", asString(task["taskname"]))
		log("分享链接: %s", asString(task["shareurl"]))
		log("保存路径: %s", asString(task["savepath"]))
		if asString(task["pattern"]) != "" {
			log("正则匹配: %s", asString(task["pattern"]))
		}
		if asString(task["replace"]) != "" {
			log("正则替换: %s", asString(task["replace"]))
		}
		if asString(task["update_subdir"]) != "" {
			log("更子目录: %s", asString(task["update_subdir"]))
		}
		if task["runweek"] != nil || asString(task["enddate"]) != "" {
			log("运行周期: WK%v ~ %s", task["runweek"], orString(asString(task["enddate"]), "forever"))
		}
		log("")
		if !taskInWindow(task) {
			log("任务不在运行周期内，跳过")
			continue
		}
		tree := acc.doSaveTask(ctx, cfg, task, addNotify, log)
		task["addition"] = mergeMaps(mapValue(task["addition"]), cloneMap(taskPluginDefaults))
		if tree != nil && tree.sizeAt(1) > 0 {
			log("🧩 调用插件")
			for _, p := range plugins {
				if p.Active() {
					if updated := p.Run(ctx, task, acc, tree, log); updated != nil {
						task = updated
						tasklist[index] = task
					}
				}
			}
		}
	}
	log("")
	log("===============插件收尾===============")
	log("")
}

func orString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func mergeMaps(a, b map[string]any) map[string]any {
	result := cloneMap(a)
	for key, value := range b {
		if existing, ok := result[key].(map[string]any); ok {
			if nested, ok := value.(map[string]any); ok {
				result[key] = mergeMaps(existing, nested)
				continue
			}
		}
		if _, ok := result[key]; !ok {
			result[key] = value
		}
	}
	return result
}

func (acc *quarkAccount) updateSavepathFID(ctx context.Context, tasklist []map[string]any, log func(string, ...any)) {
	var paths []string
	seen := map[string]bool{}
	for _, item := range tasklist {
		if asString(item["enddate"]) != "" {
			day, err := time.Parse("2006-01-02", asString(item["enddate"]))
			if err == nil && time.Now().Format("2006-01-02") > day.Format("2006-01-02") {
				continue
			}
		}
		path := regexp.MustCompile(`/{2,}`).ReplaceAllString("/"+asString(item["savepath"]), "/")
		if !seen[path] {
			paths = append(paths, path)
			seen[path] = true
		}
	}
	if len(paths) == 0 {
		return
	}
	exist, _ := acc.getFIDs(ctx, paths)
	existSet := map[string]bool{}
	for _, item := range exist {
		existSet[asString(item["file_path"])] = true
		acc.savepathFID[asString(item["file_path"])] = asString(item["fid"])
	}
	for _, path := range paths {
		if path == "/" || existSet[path] {
			continue
		}
		mkdir, err := acc.mkdir(ctx, path)
		if err == nil && number(mkdir["code"]) == 0 {
			fid := asString(mapValue(mkdir["data"])["fid"])
			acc.savepathFID[path] = fid
			log("创建文件夹：%s", path)
		} else {
			log("创建文件夹：%s 失败, %s", path, asString(mkdir["message"]))
		}
	}
}

func (acc *quarkAccount) doSaveCheck(ctx context.Context, shareURL, savepath string, log func(string, ...any)) bool {
	pwdID, passcode, pdir := extractShareLoose(shareURL)
	stokenResp, err := acc.getStoken(ctx, pwdID, passcode)
	if err != nil {
		log("❌ 转存测试失败: %s", err)
		return false
	}
	stoken := asString(mapValue(stokenResp["data"])["stoken"])
	detail, err := acc.getDetail(ctx, pwdID, stoken, pdir, 0, 0)
	if err != nil {
		log("❌ 转存测试失败: %s", err)
		return false
	}
	list := listValue(mapValue(detail["data"])["list"])
	log("获取分享: %v", list)
	var fids, tokens []any
	for _, item := range list {
		value := mapValue(item)
		fids = append(fids, value["fid"])
		tokens = append(tokens, value["share_fid_token"])
	}
	found, _ := acc.getFIDs(ctx, []string{savepath})
	toPdir := ""
	if len(found) > 0 {
		toPdir = asString(found[0]["fid"])
	} else {
		mkdir, _ := acc.mkdir(ctx, savepath)
		toPdir = asString(mapValue(mkdir["data"])["fid"])
	}
	saved, err := acc.saveFile(ctx, fids, tokens, toPdir, pwdID, stoken)
	log("转存文件: %v", saved)
	if err != nil || number(saved["code"]) != 0 {
		log("❌ 转存测试失败: 中断")
		return false
	}
	queried, _ := acc.queryTask(ctx, asString(mapValue(saved["data"])["task_id"]), func(s string) { log("%s", s) })
	log("查询转存: %v", queried)
	if number(queried["code"]) != 0 {
		log("❌ 转存测试失败: 中断")
		return false
	}
	var del []any
	for _, item := range listValue(mapValue(mapValue(queried["data"])["save_as"])["save_as_top_fids"]) {
		del = append(del, item)
	}
	if len(del) > 0 {
		deleted, _ := acc.delete(ctx, del)
		log("删除转存: %v", deleted)
		recycle, _ := acc.recycleList(ctx)
		var records []any
		for _, item := range recycle {
			for _, fid := range del {
				if asString(item["fid"]) == asString(fid) {
					records = append(records, item["record_id"])
				}
			}
		}
		removed, _ := acc.recycleRemove(ctx, records)
		log("清理转存: %v", removed)
		log("✅ 转存测试成功")
		return true
	}
	log("❌ 转存测试失败: 中断")
	return false
}

func (acc *quarkAccount) doSaveTask(ctx context.Context, cfg, task map[string]any, addNotify func(string), log func(string, ...any)) *saveTree {
	if asString(task["shareurl_ban"]) != "" {
		log("《%s》：%s", asString(task["taskname"]), asString(task["shareurl_ban"]))
		return nil
	}
	pwdID, passcode, pdir := extractShareLoose(asString(task["shareurl"]))
	stokenResp, err := acc.getStoken(ctx, pwdID, passcode)
	if err != nil || number(stokenResp["status"]) == 500 {
		log("跳过任务：网络异常 %s", asString(stokenResp["message"]))
		return nil
	}
	if number(stokenResp["status"]) != 200 {
		msg := asString(stokenResp["message"])
		addNotify(fmt.Sprintf("❌《%s》：%s\n", asString(task["taskname"]), msg))
		task["shareurl_ban"] = msg
		return nil
	}
	stoken := asString(mapValue(stokenResp["data"])["stoken"])
	tree := acc.dirCheckAndSave(ctx, cfg, task, pwdID, stoken, pdir, "", addNotify, log)
	if tree != nil && tree.sizeAt(1) > 0 {
		acc.doRename(ctx, tree, log)
		log("")
		addNotify(fmt.Sprintf("✅《%s》添加追更：\n%s", asString(task["taskname"]), tree))
		return tree
	}
	log("任务结束：没有新的转存任务")
	return nil
}

func fileIcon(f map[string]any) string {
	if asBoolDefault(f["dir"], false) {
		return "📁"
	}
	switch asString(f["obj_category"]) {
	case "video":
		return "🎞️"
	case "image":
		return "🖼️"
	case "audio":
		return "🎵"
	case "doc":
		return "📄"
	case "archive":
		return "📦"
	}
	return ""
}

func (acc *quarkAccount) dirCheckAndSave(ctx context.Context, cfg, task map[string]any, pwdID, stoken, pdir, subdir string, addNotify func(string), log func(string, ...any)) *saveTree {
	tree := newSaveTree()
	detail, err := acc.getDetail(ctx, pwdID, stoken, pdir, 0, 0)
	if err != nil {
		return tree
	}
	shareList := listValue(mapValue(detail["data"])["list"])
	if len(shareList) == 0 {
		if subdir == "" {
			task["shareurl_ban"] = "分享为空，文件已被分享者删除"
			addNotify(fmt.Sprintf("❌《%s》：%s\n", asString(task["taskname"]), task["shareurl_ban"]))
		}
		return tree
	}
	if len(shareList) == 1 && asBoolDefault(mapValue(shareList[0])["dir"], false) && subdir == "" {
		log("🧠 该分享是一个文件夹，读取文件夹内列表")
		inner, err := acc.getDetail(ctx, pwdID, stoken, asString(mapValue(shareList[0])["fid"]), 0, 0)
		if err == nil {
			shareList = listValue(mapValue(inner["data"])["list"])
		}
	}
	savepath := regexp.MustCompile(`/{2,}`).ReplaceAllString("/"+asString(task["savepath"])+subdir, "/")
	if acc.savepathFID[savepath] == "" {
		found, _ := acc.getFIDs(ctx, []string{savepath})
		if len(found) > 0 {
			acc.savepathFID[savepath] = asString(found[0]["fid"])
		} else {
			log("❌ 目录 %s fid获取失败，跳过转存", savepath)
			return tree
		}
	}
	toPdir := acc.savepathFID[savepath]
	listing, err := acc.listDir(ctx, toPdir)
	if err != nil {
		return tree
	}
	dirFiles := listValue(mapValue(listing["data"])["list"])
	var dirNames []string
	var dirMaps []map[string]any
	for _, item := range dirFiles {
		value := mapValue(item)
		dirMaps = append(dirMaps, value)
		dirNames = append(dirNames, asString(value["file_name"]))
	}
	tree.create(savepath, pdir, "", map[string]any{"is_dir": true})
	mr := NewMagicRename(mapValue(cfg["magic_regex"]))
	mr.SetTaskname(asString(task["taskname"]))
	pattern, replace := mr.Conv(asString(task["pattern"]), asString(task["replace"]))
	var need []map[string]any
	for _, raw := range shareList {
		share := mapValue(raw)
		searchPattern := pattern
		if asBoolDefault(share["dir"], false) && asString(task["update_subdir"]) != "" {
			searchPattern = asString(task["update_subdir"])
		}
		if _, ok := pythonSearch(searchPattern, asString(share["file_name"])); ok {
			ignore := asBoolDefault(task["ignore_extension"], false) && !asBoolDefault(share["dir"], false)
			if mr.Exists(asString(share["file_name"]), dirNames, ignore) == "" {
				if asBoolDefault(share["dir"], false) || subdir != "" {
					share["file_name_re"] = share["file_name"]
					need = append(need, share)
				} else {
					renamed := mr.Sub(pattern, replace, asString(share["file_name"]))
					if mr.Exists(renamed, dirNames, asBoolDefault(task["ignore_extension"], false)) == "" {
						share["file_name_re"] = renamed
						need = append(need, share)
					}
				}
			} else if asBoolDefault(share["dir"], false) && asString(task["update_subdir"]) != "" {
				if _, ok := pythonSearch(asString(task["update_subdir"]), asString(share["file_name"])); ok {
					if asBoolDefault(task["update_subdir_resave_mode"], false) {
						log("重存子目录：%s/%s", savepath, asString(share["file_name"]))
						var subdirFID any
						for _, f := range dirMaps {
							if asString(f["file_name"]) == asString(share["file_name"]) {
								subdirFID = f["fid"]
								break
							}
						}
						if subdirFID != nil {
							deleted, _ := acc.delete(ctx, []any{subdirFID})
							acc.queryTask(ctx, asString(mapValue(deleted["data"])["task_id"]), func(s string) { log("%s", s) })
							recycle, _ := acc.recycleList(ctx)
							var records []any
							for _, item := range recycle {
								if asString(item["fid"]) == asString(subdirFID) {
									records = append(records, item["record_id"])
								}
							}
							acc.recycleRemove(ctx, records)
						}
						share["file_name_re"] = share["file_name"]
						need = append(need, share)
					} else {
						log("检查子目录：%s/%s", savepath, asString(share["file_name"]))
						sub := acc.dirCheckAndSave(ctx, cfg, task, pwdID, stoken, asString(share["fid"]), subdir+"/"+asString(share["file_name"]), addNotify, log)
						if sub.sizeAt(1) > 0 {
							tree.create("📁"+asString(share["file_name"]), asString(share["fid"]), pdir, map[string]any{"is_dir": share["dir"]})
							tree.merge(asString(share["fid"]), sub)
						}
					}
				}
			}
		}
		if asString(share["fid"]) == asString(task["startfid"]) && asString(task["startfid"]) != "" {
			break
		}
	}
	if regexp.MustCompile(`\{I+\}`).MatchString(replace) {
		mr.SetDirFileList(dirMaps, replace)
		mr.SortFileList(need)
	}
	var fids, tokens []any
	for _, item := range need {
		fids = append(fids, item["fid"])
		tokens = append(tokens, item["share_fid_token"])
	}
	var top []any
	for len(fids) > 0 {
		batch, batchTok := fids, tokens
		if len(batch) > 100 {
			batch, batchTok = fids[:100], tokens[:100]
		}
		fids, tokens = fids[len(batch):], tokens[len(batchTok):]
		saved, err := acc.saveFile(ctx, batch, batchTok, toPdir, pwdID, stoken)
		if err != nil || number(saved["code"]) != 0 {
			addNotify(fmt.Sprintf("❌《%s》转存失败：%s\n", asString(task["taskname"]), asString(saved["message"])))
			continue
		}
		queried, err := acc.queryTask(ctx, asString(mapValue(saved["data"])["task_id"]), func(s string) { log("%s", s) })
		if err != nil || number(queried["code"]) != 0 {
			addNotify(fmt.Sprintf("❌《%s》转存失败：%s\n", asString(task["taskname"]), asString(queried["message"])))
			continue
		}
		top = append(top, listValue(mapValue(mapValue(queried["data"])["save_as"])["save_as_top_fids"])...)
	}
	if len(need) == len(top) {
		for i, item := range need {
			tree.create(fileIcon(item)+asString(item["file_name_re"]), asString(item["fid"]), pdir, map[string]any{
				"file_name":    item["file_name"],
				"file_name_re": item["file_name_re"],
				"fid":          fmt.Sprint(top[i]),
				"path":         savepath + "/" + asString(item["file_name_re"]),
				"is_dir":       item["dir"],
				"obj_category": item["obj_category"],
			})
		}
	}
	return tree
}

func (acc *quarkAccount) doRename(ctx context.Context, tree *saveTree, log func(string, ...any)) {
	if tree == nil {
		return
	}
	for _, child := range tree.children(tree.Root) {
		file := child.Data
		if asBoolDefault(file["is_dir"], false) {
			continue
		}
		if asString(file["file_name_re"]) != "" && asString(file["file_name_re"]) != asString(file["file_name"]) {
			ret, _ := acc.rename(ctx, file["fid"], asString(file["file_name_re"]))
			log("重命名：%s → %s", asString(file["file_name"]), asString(file["file_name_re"]))
			if number(ret["code"]) != 0 {
				log("      ↑ 失败，%s", asString(ret["message"]))
			}
		}
	}
}
