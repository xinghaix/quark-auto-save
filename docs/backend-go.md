# Go 1.27 后端架构

## 运行边界

生产入口是 `cmd/quark-auto-save` 编译出的 Go 1.27 二进制，负责：

- WebUI、登录会话和全部管理 REST 路由；
- JSON 配置读取、原子写回、MCP 密钥哈希和权限控制；
- Streamable HTTP、旧版 SSE 和 stdio MCP 传输；
- cron 解析、定时调度、运行状态、日志环形缓冲区；
- 夸克分享详情、目录浏览、文件删除/重命名；
- CloudSaver 与 PanSou 搜索源聚合。

为保持现有用户配置和插件行为，转存执行阶段暂时由 Go 通过 `exec.CommandContext` 启动受控的 Python compatibility worker：

- `quark_auto_save.py`：现有转存算法、Python 正则和任务生命周期；
- `plugins/*.py`：现有插件 ABI；
- `notify.py`：现有多渠道通知实现。

因此这是 **Go 主服务 + Python 兼容 worker**，不是把 Python 伪装成 Go，也不是立即删除旧插件生态。后续若要做纯 Go 镜像，需要单独迁移 Python 正则语义、插件接口和通知渠道，并为每一项增加行为回归测试。

## 目录

```text
cmd/quark-auto-save/main.go  # Go 1.27 进程入口、信号和优雅退出
internal/qas/app.go          # HTTP 路由、模板渲染、REST handlers
internal/qas/config.go       # JSON 配置、脱敏、API token、稳定 task_id
internal/qas/cron.go         # 五字段 cron 解析和调度器
internal/qas/log.go          # 脱敏日志和查询游标
internal/qas/mcp.go          # MCP JSON-RPC、HTTP/SSE/stdio、权限
internal/qas/quark.go        # 夸克 API、CloudSaver、PanSou
internal/qas/worker.go       # Python worker、超时、运行状态
internal/qas/preview.go      # MagicRename 预览兼容桥
app/runtime/preview.py       # 旧 Python 正则预览 helper
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

默认监听 `0.0.0.0:5005`，配置文件默认是 `./config/quark_config.json`。执行真实转存任务或复杂正则预览时，还需要 Python 3 和 `requirements.txt` 中的 worker 依赖；只测试 WebUI、配置或 MCP（不调用转存/预览）时不需要启动 worker。

## Docker

`Dockerfile` 使用两个阶段：

1. `golang:1.27-alpine` 编译带 `BUILD_SHA`/`BUILD_TAG` 的静态 Go 二进制；
2. `python:3.13-alpine` 提供 worker 兼容层，并复制 Go 服务、页面、SDK、插件、转存脚本和预览 helper。

镜像入口是 `docker-entrypoint.sh`：root 启动时修正 `/app/config` 权限并降权为 `qas`（UID 1000/GID 1001），再执行 `/usr/local/bin/quark-auto-save`。历史命令 `python3 /app/app/run.py --mcp-stdio` 通过 shim 转发到 Go。多架构发布由 `.github/workflows/docker-publish.yml` 负责，目标为 `linux/amd64`、`linux/arm64` 和 `linux/arm/v7`。

## 兼容性和安全

- 源码中的旧 `app/run.py` / `app/mcp.py` 保留用于兼容测试和迁移对照；镜像内 `/app/app/run.py` 是 shim，会把历史命令转发到 Go 二进制。
- Go 运行任务使用参数数组，不经过 shell；超时由 `CommandContext` 终止 worker。
- 配置写回采用临时文件、`fsync` 和 rename，权限为 `0600`。
- MCP API key 只保存 SHA-256 哈希；MCP 配置和日志响应会递归脱敏。
- WebUI `/data` 仍需返回 Cookie 供用户编辑；MCP 的 `qas_config_get` 不返回 Cookie、密码、token 或 webhook 密钥。
