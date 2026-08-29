package qas

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func jitterDT() int { return int((1 + rand.Float64()*4) * 60 * 1000) }

func (q *QuarkClient) saveFile(ctx context.Context, fids, tokens []any, toPdir, pwdID, stoken string) (map[string]any, error) {
	params := url.Values{"pr": {"ucpro"}, "fr": {"pc"}, "uc_param_str": {""}, "app": {"clouddrive"}, "__dt": {fmt.Sprint(jitterDT())}, "__t": {fmt.Sprintf("%d", time.Now().Unix())}}
	return q.post(ctx, "/1/clouddrive/share/sharepage/save", params, map[string]any{
		"fid_list": fids, "fid_token_list": tokens, "to_pdir_fid": toPdir, "pwd_id": pwdID, "stoken": stoken, "pdir_fid": "0", "scene": "link",
	})
}

func (q *QuarkClient) queryTask(ctx context.Context, taskID string, log func(string)) (map[string]any, error) {
	if log == nil {
		log = func(string) {}
	}
	for retry := 0; retry < 1200; retry++ {
		params := url.Values{"pr": {"ucpro"}, "fr": {"pc"}, "uc_param_str": {""}, "task_id": {taskID}, "retry_index": {fmt.Sprint(retry)}, "__dt": {fmt.Sprint(jitterDT())}, "__t": {fmt.Sprintf("%d", time.Now().Unix())}}
		response, err := q.get(ctx, "/1/clouddrive/task", params)
		if err != nil {
			return nil, err
		}
		if number(response["status"]) != 200 {
			return response, nil
		}
		if number(mapValue(response["data"])["status"]) == 2 {
			if retry > 0 {
				log("")
			}
			return response, nil
		}
		title := asString(mapValue(response["data"])["task_title"])
		if retry == 0 {
			log(fmt.Sprintf("正在等待[%s]执行结果", title))
		} else {
			log(".")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("查询任务超时")
}

func (q *QuarkClient) download(ctx context.Context, fids []any) (map[string]any, string, error) {
	data, header, err := q.request(ctx, http.MethodPost, "/1/clouddrive/file/download", url.Values{"pr": {"ucpro"}, "fr": {"pc"}, "uc_param_str": {""}}, map[string]any{"fids": fids}, nil)
	if err != nil {
		return nil, "", err
	}
	var cookies []string
	for _, raw := range header.Values("Set-Cookie") {
		if part, _, _ := strings.Cut(raw, ";"); part != "" {
			cookies = append(cookies, part)
		}
	}
	return data, strings.Join(cookies, "; "), nil
}

func (q *QuarkClient) unarchive(ctx context.Context, fid, toPdir any) (map[string]any, error) {
	return q.post(ctx, "/1/clouddrive/archive/unarchive", url.Values{"uc_param_str": {""}, "fr": {"pc"}, "pr": {"ucpro"}}, map[string]any{
		"fid": fid, "to_pdir_fid": toPdir, "conflict_mode": 3, "suffix_type": 0, "pwd": "", "select_mode": 0,
	})
}

func (q *QuarkClient) moveFiles(ctx context.Context, fids []any, toPdir any) (map[string]any, error) {
	return q.post(ctx, "/1/clouddrive/file/move", url.Values{"uc_param_str": {""}, "fr": {"pc"}, "pr": {"ucpro"}}, map[string]any{
		"filelist": fids, "to_pdir_fid": toPdir, "exclude_fids": []any{}, "action_type": 1,
	})
}

func (q *QuarkClient) recycleList(ctx context.Context) ([]map[string]any, error) {
	response, err := q.get(ctx, "/1/clouddrive/file/recycle/list", url.Values{"_page": {"1"}, "_size": {"30"}, "pr": {"ucpro"}, "fr": {"pc"}, "uc_param_str": {""}})
	if err != nil {
		return nil, err
	}
	var items []map[string]any
	for _, item := range listValue(mapValue(response["data"])["list"]) {
		if value := mapValue(item); len(value) > 0 {
			items = append(items, value)
		}
	}
	return items, nil
}

func (q *QuarkClient) recycleRemove(ctx context.Context, records []any) (map[string]any, error) {
	return q.post(ctx, "/1/clouddrive/file/recycle/remove", url.Values{"uc_param_str": {""}, "fr": {"pc"}, "pr": {"ucpro"}}, map[string]any{"select_mode": 2, "record_list": records})
}

func (q *QuarkClient) accountInfo(ctx context.Context) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://pan.quark.cn/account/info?fr=pc&platform=pc", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", q.cookie)
	req.Header.Set("User-Agent", quarkUserAgent)
	resp, err := q.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func (q *QuarkClient) growthInfo(ctx context.Context) (map[string]any, error) {
	return q.get(ctx, "/1/clouddrive/capacity/growth/info", url.Values{})
}

func (q *QuarkClient) growthSign(ctx context.Context) (map[string]any, error) {
	return q.post(ctx, "/1/clouddrive/capacity/growth/sign", url.Values{}, map[string]any{"sign_cyclic": true})
}

func (q *QuarkClient) listFullPath(ctx context.Context, fid string) (string, error) {
	if fid == "" || fid == "0" {
		return "/", nil
	}
	params := url.Values{"pr": {"ucpro"}, "fr": {"pc"}, "uc_param_str": {""}, "pdir_fid": {fid}, "_page": {"1"}, "_size": {"50"}, "_fetch_total": {"1"}, "_fetch_sub_dirs": {"0"}, "_sort": {"file_type:asc,updated_at:desc"}, "_fetch_full_path": {"1"}}
	response, err := q.get(ctx, "/1/clouddrive/file/sort", params)
	if err != nil {
		return "", err
	}
	if number(response["code"]) != 0 {
		return "", nil
	}
	path := ""
	for _, item := range listValue(mapValue(response["data"])["full_path"]) {
		path += "/" + asString(mapValue(item)["file_name"])
	}
	return path, nil
}

func extractShareLoose(raw string) (pwdID, passcode, pdir string) {
	if match := shareIDRE.FindStringSubmatch(raw); len(match) == 2 {
		pwdID = match[1]
	}
	if match := passcodeRE.FindStringSubmatch(raw); len(match) == 2 {
		passcode = match[1]
	}
	matches := pathFIDRE.FindAllStringSubmatch(raw, -1)
	pdir = "0"
	if len(matches) > 0 {
		pdir = matches[len(matches)-1][1]
	}
	return pwdID, passcode, pdir
}
