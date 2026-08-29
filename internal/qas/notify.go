package qas

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

func cfgGet(cfg map[string]any, key string) string { return strings.TrimSpace(asString(cfg[key])) }

func sendNotify(cfg map[string]any, title, content string, log func(string, ...any)) {
	if cfg == nil {
		cfg = map[string]any{}
	}
	if strings.TrimSpace(content) == "" {
		log("%s 推送内容为空！", title)
		return
	}
	if skip := os.Getenv("SKIP_PUSH_TITLE"); skip != "" {
		for _, item := range strings.Split(skip, "\n") {
			if item == title {
				log("%s 在SKIP_PUSH_TITLE环境变量内，跳过推送！", title)
				return
			}
		}
	}
	if v := cfg["HITOKOTO"]; asBoolDefault(v, false) && strings.ToLower(asString(v)) != "false" {
		content += "\n\n" + hitokoto()
	}
	var fns []func()
	add := func(ok bool, fn func()) {
		if ok {
			fns = append(fns, fn)
		}
	}
	add(cfgGet(cfg, "BARK_PUSH") != "", func() { notifyBark(cfg, title, content, log) })
	add(cfg["CONSOLE"] != nil, func() {
		if strings.ToLower(asString(cfg["CONSOLE"])) != "false" {
			log("%s\n\n%s", title, content)
		}
	})
	add(cfgGet(cfg, "DD_BOT_TOKEN") != "" && cfgGet(cfg, "DD_BOT_SECRET") != "", func() { notifyDing(cfg, title, content, log) })
	add(cfgGet(cfg, "FSKEY") != "", func() {
		notifyJSON(log, "飞书", "https://open.feishu.cn/open-apis/bot/v2/hook/"+cfgGet(cfg, "FSKEY"), map[string]any{"msg_type": "text", "content": map[string]any{"text": title + "\n\n" + content}})
	})
	add(cfgGet(cfg, "GOBOT_URL") != "" && cfgGet(cfg, "GOBOT_QQ") != "", func() {
		u := fmt.Sprintf("%s?access_token=%s&%s&message=%s", cfgGet(cfg, "GOBOT_URL"), url.QueryEscape(cfgGet(cfg, "GOBOT_TOKEN")), cfgGet(cfg, "GOBOT_QQ"), url.QueryEscape("标题:"+title+"\n内容:"+content))
		notifyGet(log, "go-cqhttp", u)
	})
	add(cfgGet(cfg, "GOTIFY_URL") != "" && cfgGet(cfg, "GOTIFY_TOKEN") != "", func() {
		notifyForm(log, "gotify", cfgGet(cfg, "GOTIFY_URL")+"/message?token="+url.QueryEscape(cfgGet(cfg, "GOTIFY_TOKEN")), url.Values{"title": {title}, "message": {content}, "priority": {cfgGet(cfg, "GOTIFY_PRIORITY")}})
	})
	add(cfgGet(cfg, "IGOT_PUSH_KEY") != "", func() {
		notifyForm(log, "iGot", "https://push.hellyw.com/"+cfgGet(cfg, "IGOT_PUSH_KEY"), url.Values{"title": {title}, "content": {content}})
	})
	add(cfgGet(cfg, "PUSH_KEY") != "", func() { notifyServerJ(cfg, title, content, log) })
	add(cfgGet(cfg, "DEER_KEY") != "", func() {
		u := "https://api2.pushdeer.com/message/push"
		if cfgGet(cfg, "DEER_URL") != "" {
			u = cfgGet(cfg, "DEER_URL")
		}
		notifyForm(log, "PushDeer", u, url.Values{"text": {title}, "desp": {content}, "type": {"markdown"}, "pushkey": {cfgGet(cfg, "DEER_KEY")}})
	})
	add(cfgGet(cfg, "CHAT_URL") != "" && cfgGet(cfg, "CHAT_TOKEN") != "", func() {
		body := "payload=" + url.QueryEscape(fmt.Sprintf(`{"text":%q}`, title+"\n"+content))
		notifyRaw(log, "Chat", http.MethodPost, cfgGet(cfg, "CHAT_URL")+cfgGet(cfg, "CHAT_TOKEN"), "application/x-www-form-urlencoded", []byte(body))
	})
	add(cfgGet(cfg, "PUSH_PLUS_TOKEN") != "", func() {
		notifyJSON(log, "PUSHPLUS", "https://www.pushplus.plus/send", map[string]any{"token": cfgGet(cfg, "PUSH_PLUS_TOKEN"), "title": title, "content": content, "topic": cfgGet(cfg, "PUSH_PLUS_USER"), "template": cfgGet(cfg, "PUSH_PLUS_TEMPLATE"), "channel": cfgGet(cfg, "PUSH_PLUS_CHANNEL"), "webhook": cfgGet(cfg, "PUSH_PLUS_WEBHOOK"), "callbackUrl": cfgGet(cfg, "PUSH_PLUS_CALLBACKURL"), "to": cfgGet(cfg, "PUSH_PLUS_TO")})
	})
	add(cfgGet(cfg, "WE_PLUS_BOT_TOKEN") != "", func() {
		tpl := "txt"
		if len(content) > 800 {
			tpl = "html"
		}
		notifyJSON(log, "微加机器人", "https://www.weplusbot.com/send", map[string]any{"token": cfgGet(cfg, "WE_PLUS_BOT_TOKEN"), "title": title, "content": content, "template": tpl, "receiver": cfgGet(cfg, "WE_PLUS_BOT_RECEIVER"), "version": cfgGet(cfg, "WE_PLUS_BOT_VERSION")})
	})
	add(cfgGet(cfg, "QMSG_KEY") != "" && cfgGet(cfg, "QMSG_TYPE") != "", func() {
		notifyForm(log, "qmsg", fmt.Sprintf("https://qmsg.zendee.cn/%s/%s", cfgGet(cfg, "QMSG_TYPE"), cfgGet(cfg, "QMSG_KEY")), url.Values{"msg": {title + "\n\n" + strings.ReplaceAll(content, "----", "-")}})
	})
	add(cfgGet(cfg, "QYWX_AM") != "", func() { notifyWecomApp(cfg, title, content, log) })
	add(cfgGet(cfg, "QYWX_KEY") != "", func() {
		origin := cfgGet(cfg, "QYWX_ORIGIN")
		if origin == "" {
			origin = "https://qyapi.weixin.qq.com"
		}
		notifyJSON(log, "企业微信机器人", origin+"/cgi-bin/webhook/send?key="+cfgGet(cfg, "QYWX_KEY"), map[string]any{"msgtype": "text", "text": map[string]any{"content": title + "\n\n" + content}})
	})
	add(cfgGet(cfg, "TG_BOT_TOKEN") != "" && cfgGet(cfg, "TG_USER_ID") != "", func() { notifyTelegram(cfg, title, content, log) })
	add(cfgGet(cfg, "AIBOTK_KEY") != "" && cfgGet(cfg, "AIBOTK_TYPE") != "" && cfgGet(cfg, "AIBOTK_NAME") != "", func() {
		u := "https://api-bot.aibotk.com/openapi/v1/chat/contact"
		payload := map[string]any{"apiKey": cfgGet(cfg, "AIBOTK_KEY"), "name": cfgGet(cfg, "AIBOTK_NAME"), "message": map[string]any{"type": 1, "content": "【青龙快讯】\n\n" + title + "\n" + content}}
		if cfgGet(cfg, "AIBOTK_TYPE") == "room" {
			u = "https://api-bot.aibotk.com/openapi/v1/chat/room"
			payload = map[string]any{"apiKey": cfgGet(cfg, "AIBOTK_KEY"), "roomName": cfgGet(cfg, "AIBOTK_NAME"), "message": map[string]any{"type": 1, "content": "【青龙快讯】\n\n" + title + "\n" + content}}
		}
		notifyJSON(log, "智能微秘书", u, payload)
	})
	add(cfgGet(cfg, "SMTP_SERVER") != "" && cfgGet(cfg, "SMTP_SSL") != "" && cfgGet(cfg, "SMTP_EMAIL") != "" && cfgGet(cfg, "SMTP_PASSWORD") != "" && cfgGet(cfg, "SMTP_NAME") != "", func() {
		notifySMTP(cfg, title, content, log)
	})
	add(cfgGet(cfg, "PUSHME_KEY") != "", func() {
		u := cfgGet(cfg, "PUSHME_URL")
		if u == "" {
			u = "https://push.i-i.me/"
		}
		notifyForm(log, "PushMe", u, url.Values{"push_key": {cfgGet(cfg, "PUSHME_KEY")}, "title": {title}, "content": {content}})
	})
	add(cfgGet(cfg, "CHRONOCAT_URL") != "" && cfgGet(cfg, "CHRONOCAT_QQ") != "" && cfgGet(cfg, "CHRONOCAT_TOKEN") != "", func() { notifyChronocat(cfg, title, content, log) })
	add(cfgGet(cfg, "DODO_BOTTOKEN") != "" && cfgGet(cfg, "DODO_BOTID") != "" && cfgGet(cfg, "DODO_LANDSOURCEID") != "" && cfgGet(cfg, "DODO_SOURCEID") != "", func() {
		notifyJSON(log, "DoDo", "https://botopen.imdodo.com/api/v2/personal/message/send", map[string]any{"islandSourceId": cfgGet(cfg, "DODO_LANDSOURCEID"), "dodoSourceId": cfgGet(cfg, "DODO_SOURCEID"), "messageType": 1, "messageBody": map[string]any{"content": title + "\n\n" + content}})
	})
	add(cfgGet(cfg, "WEBHOOK_URL") != "" && cfgGet(cfg, "WEBHOOK_METHOD") != "", func() { notifyWebhook(cfg, title, content, log) })
	add(cfgGet(cfg, "NTFY_TOPIC") != "", func() { notifyNtfy(cfg, title, content, log) })
	add(cfgGet(cfg, "WXPUSHER_APP_TOKEN") != "" && (cfgGet(cfg, "WXPUSHER_TOPIC_IDS") != "" || cfgGet(cfg, "WXPUSHER_UIDS") != ""), func() { notifyWxpusher(cfg, title, content, log) })
	if len(fns) == 0 {
		log("无推送渠道，请检查通知变量是否正确")
		return
	}
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func(f func()) { defer wg.Done(); f() }(fn)
	}
	wg.Wait()
}

func notifyClient() *http.Client { return &http.Client{Timeout: 15 * time.Second} }

func notifyJSON(log func(string, ...any), name, rawURL string, payload any) {
	body, _ := json.Marshal(payload)
	notifyRaw(log, name, http.MethodPost, rawURL, "application/json;charset=utf-8", body)
}

func notifyForm(log func(string, ...any), name, rawURL string, form url.Values) {
	notifyRaw(log, name, http.MethodPost, rawURL, "application/x-www-form-urlencoded", []byte(form.Encode()))
}

func notifyGet(log func(string, ...any), name, rawURL string) {
	notifyRaw(log, name, http.MethodGet, rawURL, "", nil)
}

func notifyRaw(log func(string, ...any), name, method, rawURL, contentType string, body []byte) {
	log("%s 服务启动", name)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, reader)
	if err != nil {
		log("%s 推送失败！%s", name, err)
		return
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := notifyClient().Do(req)
	if err != nil {
		log("%s 推送失败！%s", name, err)
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log("%s 推送成功！", name)
		return
	}
	log("%s 推送失败！%s", name, strings.TrimSpace(string(raw)))
}

func notifyBark(cfg map[string]any, title, content string, log func(string, ...any)) {
	u := cfgGet(cfg, "BARK_PUSH")
	if !strings.HasPrefix(u, "http") {
		u = "https://api.day.app/" + u
	}
	data := map[string]any{"title": title, "body": content}
	mapping := map[string]string{"BARK_ARCHIVE": "isArchive", "BARK_GROUP": "group", "BARK_SOUND": "sound", "BARK_ICON": "icon", "BARK_LEVEL": "level", "BARK_URL": "url"}
	for k, v := range mapping {
		if cfgGet(cfg, k) != "" {
			data[v] = cfgGet(cfg, k)
		}
	}
	notifyJSON(log, "bark", u, data)
}

func notifyDing(cfg map[string]any, title, content string, log func(string, ...any)) {
	ts := fmt.Sprintf("%d", time.Now().UnixMilli())
	mac := hmac.New(sha256.New, []byte(cfgGet(cfg, "DD_BOT_SECRET")))
	mac.Write([]byte(ts + "\n" + cfgGet(cfg, "DD_BOT_SECRET")))
	sign := url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	u := fmt.Sprintf("https://oapi.dingtalk.com/robot/send?access_token=%s&timestamp=%s&sign=%s", cfgGet(cfg, "DD_BOT_TOKEN"), ts, sign)
	notifyJSON(log, "钉钉机器人", u, map[string]any{"msgtype": "text", "text": map[string]any{"content": title + "\n\n" + content}})
}

func notifyServerJ(cfg map[string]any, title, content string, log func(string, ...any)) {
	key := cfgGet(cfg, "PUSH_KEY")
	u := "https://sctapi.ftqq.com/" + key + ".send"
	if m := regexp.MustCompile(`^sctp(\d+)t`).FindStringSubmatch(key); len(m) == 2 {
		u = fmt.Sprintf("https://%s.push.ft07.com/send/%s.send", m[1], key)
	}
	notifyForm(log, "serverJ", u, url.Values{"text": {title}, "desp": {strings.ReplaceAll(content, "\n", "\n\n")}})
}

func notifyTelegram(cfg map[string]any, title, content string, log func(string, ...any)) {
	host := cfgGet(cfg, "TG_API_HOST")
	if host == "" {
		host = "https://api.telegram.org"
	}
	u := strings.TrimRight(host, "/") + "/bot" + cfgGet(cfg, "TG_BOT_TOKEN") + "/sendMessage"
	form := url.Values{"chat_id": {cfgGet(cfg, "TG_USER_ID")}, "text": {title + "\n\n" + content}, "disable_web_page_preview": {"true"}}
	client := notifyClient()
	if cfgGet(cfg, "TG_PROXY_HOST") != "" && cfgGet(cfg, "TG_PROXY_PORT") != "" {
		proxy := "http://" + cfgGet(cfg, "TG_PROXY_HOST") + ":" + cfgGet(cfg, "TG_PROXY_PORT")
		if cfgGet(cfg, "TG_PROXY_AUTH") != "" && !strings.Contains(cfgGet(cfg, "TG_PROXY_HOST"), "@") {
			proxy = "http://" + cfgGet(cfg, "TG_PROXY_AUTH") + "@" + cfgGet(cfg, "TG_PROXY_HOST") + ":" + cfgGet(cfg, "TG_PROXY_PORT")
		}
		if parsed, err := url.Parse(proxy); err == nil {
			client = &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(parsed)}}
		}
	}
	log("tg 服务启动")
	resp, err := client.PostForm(u, form)
	if err != nil {
		log("tg 推送失败！%s", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		log("tg 推送成功！")
	} else {
		log("tg 推送失败！")
	}
}

func notifySMTP(cfg map[string]any, title, content string, log func(string, ...any)) {
	log("SMTP 邮件 服务启动")
	from := cfgGet(cfg, "SMTP_EMAIL")
	to := cfgGet(cfg, "SMTP_EMAIL_TO")
	if to == "" {
		to = from
	}
	recipients := strings.Split(to, ",")
	msg := []byte("From: " + from + "\r\nTo: " + strings.Join(recipients, ",") + "\r\nSubject: " + title + "\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n" + content)
	addr := cfgGet(cfg, "SMTP_SERVER")
	if !strings.Contains(addr, ":") {
		if cfgGet(cfg, "SMTP_SSL") == "true" {
			addr += ":465"
		} else {
			addr += ":25"
		}
	}
	auth := smtp.PlainAuth("", from, cfgGet(cfg, "SMTP_PASSWORD"), strings.Split(addr, ":")[0])
	var err error
	if cfgGet(cfg, "SMTP_SSL") == "true" {
		err = smtpTLS(addr, auth, from, recipients, msg)
	} else {
		err = smtp.SendMail(addr, auth, from, recipients, msg)
	}
	if err != nil {
		log("SMTP 邮件 推送失败！%s", err)
		return
	}
	log("SMTP 邮件 推送成功！")
}

func smtpTLS(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	host, _, _ := net.SplitHostPort(addr)
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host})
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Auth(auth); err != nil {
		return err
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(strings.TrimSpace(rcpt)); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func notifyWecomApp(cfg map[string]any, title, content string, log func(string, ...any)) {
	parts := strings.Split(cfgGet(cfg, "QYWX_AM"), ",")
	if len(parts) < 4 || len(parts) > 5 {
		log("QYWX_AM 设置错误!!")
		return
	}
	log("企业微信 APP 服务启动")
	tokenURL := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/gettoken?corpid=%s&corpsecret=%s", url.QueryEscape(parts[0]), url.QueryEscape(parts[1]))
	data, _, _, err := pluginHTTP(context.Background(), http.MethodGet, tokenURL, nil, nil, nil)
	token := asString(data["access_token"])
	if err != nil || token == "" {
		log("企业微信推送失败！")
		return
	}
	payload := map[string]any{"touser": parts[2], "agentid": parts[3], "msgtype": "text", "text": map[string]any{"content": title + "\n\n" + content}, "safe": "0"}
	notifyJSON(log, "企业微信", "https://qyapi.weixin.qq.com/cgi-bin/message/send?access_token="+token, payload)
}

func notifyChronocat(cfg map[string]any, title, content string, log func(string, ...any)) {
	log("CHRONOCAT 服务启动")
	users := regexp.MustCompile(`user_id=(\d+)`).FindAllStringSubmatch(cfgGet(cfg, "CHRONOCAT_QQ"), -1)
	groups := regexp.MustCompile(`group_id=(\d+)`).FindAllStringSubmatch(cfgGet(cfg, "CHRONOCAT_QQ"), -1)
	send := func(chatType int, id string) {
		notifyJSON(log, "CHRONOCAT", strings.TrimRight(cfgGet(cfg, "CHRONOCAT_URL"), "/")+"/api/message/send", map[string]any{
			"peer":     map[string]any{"chatType": chatType, "peerUin": id},
			"elements": []any{map[string]any{"elementType": 1, "textElement": map[string]any{"content": title + "\n\n" + content}}},
		})
	}
	for _, m := range users {
		send(1, m[1])
	}
	for _, m := range groups {
		send(2, m[1])
	}
}

func notifyNtfy(cfg map[string]any, title, content string, log func(string, ...any)) {
	log("ntfy 服务启动")
	encoded := "=?utf-8?B?" + base64.StdEncoding.EncodeToString([]byte(title)) + "?="
	headers := map[string]string{"Title": encoded, "Priority": orString(cfgGet(cfg, "NTFY_PRIORITY"), "3"), "Icon": "https://qn.whyour.cn/logo.png"}
	if cfgGet(cfg, "NTFY_TOKEN") != "" {
		headers["Authorization"] = "Bearer " + cfgGet(cfg, "NTFY_TOKEN")
	} else if cfgGet(cfg, "NTFY_USERNAME") != "" {
		headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(cfgGet(cfg, "NTFY_USERNAME")+":"+cfgGet(cfg, "NTFY_PASSWORD")))
	}
	req, _ := http.NewRequest(http.MethodPost, strings.TrimRight(cfgGet(cfg, "NTFY_URL"), "/")+"/"+cfgGet(cfg, "NTFY_TOPIC"), strings.NewReader(content))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := notifyClient().Do(req)
	if err != nil || resp.StatusCode != 200 {
		log("Ntfy 推送失败！")
		return
	}
	resp.Body.Close()
	log("Ntfy 推送成功！")
}

func notifyWxpusher(cfg map[string]any, title, content string, log func(string, ...any)) {
	var topics []int
	for _, id := range strings.Split(cfgGet(cfg, "WXPUSHER_TOPIC_IDS"), ";") {
		id = strings.TrimSpace(id)
		if n, err := fmt.Sscanf(id, "%d", new(int)); n == 1 && err == nil {
			var v int
			fmt.Sscanf(id, "%d", &v)
			topics = append(topics, v)
		}
	}
	var uids []string
	for _, id := range strings.Split(cfgGet(cfg, "WXPUSHER_UIDS"), ";") {
		if s := strings.TrimSpace(id); s != "" {
			uids = append(uids, s)
		}
	}
	notifyJSON(log, "wxpusher", "https://wxpusher.zjiecode.com/api/send/message", map[string]any{
		"appToken": cfgGet(cfg, "WXPUSHER_APP_TOKEN"), "content": fmt.Sprintf("<h1>%s</h1><br/><div style='white-space: pre-wrap;'>%s</div>", title, content),
		"summary": title, "contentType": 2, "topicIds": topics, "uids": uids, "verifyPayType": 0,
	})
}

func notifyWebhook(cfg map[string]any, title, content string, log func(string, ...any)) {
	rawURL := cfgGet(cfg, "WEBHOOK_URL")
	body := cfgGet(cfg, "WEBHOOK_BODY")
	if !strings.Contains(rawURL, "$title") && !strings.Contains(body, "$title") {
		log("请求头或者请求体中必须包含 $title 和 $content")
		return
	}
	repl := strings.NewReplacer("$title", strings.ReplaceAll(title, "\n", "\\n"), "$content", strings.ReplaceAll(content, "\n", "\\n"))
	formatted := strings.ReplaceAll(strings.ReplaceAll(rawURL, "$title", url.QueryEscape(title)), "$content", url.QueryEscape(content))
	notifyRaw(log, "自定义通知", cfgGet(cfg, "WEBHOOK_METHOD"), formatted, cfgGet(cfg, "WEBHOOK_CONTENT_TYPE"), []byte(repl.Replace(body)))
}

func hitokoto() string {
	data, _, _, err := pluginHTTP(context.Background(), http.MethodGet, "https://v1.hitokoto.cn/", nil, nil, nil)
	if err != nil {
		return ""
	}
	return asString(data["hitokoto"]) + "    ----" + asString(data["from"])
}
