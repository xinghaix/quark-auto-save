# Go 1.27 后端架构

## 运行边界

生产入口是 `cmd/quark-auto-save` 编译出的 Go 1.27 静态二进制，负责：

- WebUI、登录会话和全部管理 REST 路由；
- JSON 配置读取、原子写回、MCP 密钥哈希和权限控制；
- Streamable HTTP、旧版 SSE 和 stdio MCP 传输；
- cron 解析、定时调度、运行状态、日志环形缓冲区；
- 夸克分享详情、目录浏览、转存、签到、删除/重命名；
- MagicRename 预览与转存命名（regexp2，兼容 Python lookaround/反引用）；
- 内置插件（Emby/Plex/Alist/Aria2/飞牛/SmartStrm/云解压）和多渠道通知；
- CloudSaver 与 PanSou 搜索源聚合。

镜像不再包含 Python，也不再加载 `plugins/*.py`。

## 目录

```text
cmd/quark-auto-save/main.go  # 进程入口、信号和优雅退出
internal/qas/app.go          # HTTP 路由、模板渲染、REST handlers
internal/qas/engine.go       # 签到、转存、任务生命周期
internal/qas/rename.go       # MagicRename
internal/qas/preview.go      # 分享预览
internal/qas/plugin.go       # 内置插件
internal/qas/plugin_more.go  # Alist/Aria2/飞牛等
internal/qas/notify.go       # 推送渠道
internal/qas/worker.go       # 运行互斥、超时、SSE
internal/qas/quark.go        # 夸克 API
app/templates/               # Vue 3.5 + Bootstrap 5.3 页面
app/static/                  # 前端静态资源
```

## 本地运行

```shell
go version                 # 必须为 go1.27.x
mkdir -p bin
go build -trimpath -o bin/quark-auto-save ./cmd/quark-auto-save
go test ./...
./bin/quark-auto-save
```

默认监听 `0.0.0.0:5005`，配置文件默认是 `./config/quark_config.json`。

## Docker

`Dockerfile` 使用两个阶段：

1. `golang:1.27-alpine` 编译带 `BUILD_SHA`/`BUILD_TAG` 的静态二进制；
2. `alpine:3.22` 只放入二进制、页面、配置模板、`ca-certificates`、`tzdata` 和 `su-exec`。

容器内路径：

| 变量 | 值 |
|---|---|
| `CONFIG_PATH` | `/app/config/quark_config.json` |
| `CONFIG_TEMPLATE_PATH` | `/app/quark_config.json` |
| `STATIC_DIR` | `/app/app/static` |
| `TEMPLATE_DIR` | `/app/app/templates` |
| 二进制 | `/usr/local/bin/quark-auto-save` |
| 用户 | `qas` UID 1000 / GID 1001 |

入口 `docker-entrypoint.sh`：root 启动时修正 `/app/config` 权限并降权为 `qas`，再执行二进制。不要用 `--user` 覆盖。

```shell
docker compose up -d --build
docker build -t quark-auto-save .
```

多架构发布由 `.github/workflows/docker-publish.yml` 负责：`linux/amd64`、`linux/arm64`、`linux/arm/v7`。CI 只跑 `gofmt`、`go test`、`go vet`。

## 兼容性和安全

- 配置写回采用临时文件、`fsync` 和 rename，权限为 `0600`。
- MCP API key 只保存 SHA-256 哈希；MCP 配置和日志响应会递归脱敏。
- WebUI `/data` 仍需返回 Cookie 供用户编辑；MCP 的 `qas_config_get` 不返回 Cookie、密码、token 或 webhook 密钥。
- 同一时刻只跑一个转存任务，避免配置文件交叉写回。
