<div align="center">

![quark-logo](img/icon.png)

# 夸克网盘自动转存

夸克网盘签到、自动转存、命名整理、发推送提醒和刷新媒体库一条龙。

对于一些持续更新的资源，隔段时间去转存十分麻烦。

定期执行本脚本自动转存和文件名整理，并通过内置插件扩展媒体库处理。

> **本仓库说明（xinghaix fork）**
>
> 本仓库是 `xinghaix/quark-auto-save` 的 Fork，代码和镜像以本仓库为准，协议为 AGPL-3.0。
> 已包含现代化 WebUI（Vue 3.5 + Bootstrap 5.3、浅色/深色/海洋/日落主题、中英界面）、原生 MCP 远程管理和 GHCR 发布流程。
> 主题与语言不写入服务器配置；Cookie、推送密钥等仍只存在你自己的 `config` 目录。
> 镜像地址：`ghcr.io/xinghaix/quark-auto-save`。

[![github releases][gitHub-releases-image]][github-url] [![GHCR image][ghcr-image]][docker-url]

[gitHub-releases-image]: https://img.shields.io/github/v/release/xinghaix/quark-auto-save?logo=github
[ghcr-image]: https://img.shields.io/badge/GHCR-quark--auto--save-2496ED?logo=docker&logoColor=white
[github-url]: https://github.com/xinghaix/quark-auto-save
[docker-url]: https://github.com/xinghaix/quark-auto-save/pkgs/container/quark-auto-save

![run_log](img/run_log.png)

</div>

> [!CAUTION]
> 注意：资源不会持续高频更新，不要设置过高的定时运行频率，以免触发账号风控并增加夸克服务压力。


## 功能

- 部署方式
  - [x] Docker 部署，WebUI 配置
  - [x] 现代化 WebUI：Vue 3 + Bootstrap 5、多主题、中/英界面、更清晰的设置分组

- 分享链接
  - [x] 支持分享链接的子目录
  - [x] 记录失效分享并跳过任务
  - [x] 支持需提取码的分享链接
  - [x] 智能搜索资源并自动填充

- 文件管理
  - [x] 目标目录不存在时自动新建
  - [x] 跳过已转存过的文件
  - [x] 正则过滤要转存的文件名
  - [x] 转存后文件名整理（正则替换）
  - [x] 可选忽略文件后缀

- 任务管理
  - [x] 支持多组任务
  - [x] 任务结束期限，期限后不执行此任务
  - [x] 可单独指定子任务星期几执行

- 媒体库整合
  - [x] 根据任务名触发媒体库处理
  - [x] 追更或整理后自动刷新媒体库
  - [x] 插件模块化，允许自行开发和挂载[插件](./plugins)

- 其它
  - [x] 每日签到领空间
  - [x] 支持多个通知推送渠道
  - [x] 支持多账号（多账号签到，仅首账号转存）

## 部署

### Docker 部署

本 fork 的镜像发布到 GHCR。当前包如果是私有的，首次拉取前使用拥有 `read:packages` 权限的 GitHub PAT 登录；不要把 PAT 写进仓库：

```shell
echo "$GHCR_PAT" | docker login ghcr.io -u <GitHub用户名> --password-stdin
```

Docker 部署提供 WebUI 进行管理配置：

```shell
# 5005:5005 中前一个端口可改，后一个端口固定
# :latest 可替换为 :v0.8.8 固定版本

docker run -d \
  --name quark-auto-save \
  -p 5005:5005 \
  -e WEBUI_USERNAME=admin \
  -e WEBUI_PASSWORD=admin123 \
  -v ./quark-auto-save/config:/app/config \
  -v ./quark-auto-save/media:/media \
  --network bridge \
  --restart unless-stopped \
  ghcr.io/xinghaix/quark-auto-save:latest
```

docker-compose.yml

```yaml
name: quark-auto-save
services:
  quark-auto-save:
    image: ghcr.io/xinghaix/quark-auto-save:latest
    container_name: quark-auto-save
    network_mode: bridge
    ports:
      - 5005:5005
    restart: unless-stopped
    environment:
      WEBUI_USERNAME: "admin"
      WEBUI_PASSWORD: "admin123"
    volumes:
      - ./quark-auto-save/config:/app/config
      - ./quark-auto-save/media:/media
```

管理地址：http://yourhost:5005

### MCP 远程管理

在 WebUI「系统配置 → MCP 服务」中启用 MCP，先设置至少 20 个字符的 API key，再勾选需要暴露的权限并保存。MCP 与管理后台共用端口，标准地址为：

```text
http://yourhost:5005/mcp
```

服务支持标准 **Streamable HTTP**，POST 请求可返回 JSON 或 `text/event-stream`；需要兼容旧版 SSE 客户端时使用 `GET /mcp/sse` 和 `POST /mcp/messages?sessionId=...`。同时支持带 API key 的 stdio，方便本机 Agent 使用。HTTP 请求使用以下任一认证方式：

```http
Authorization: Bearer <MCP_API_KEY>
```

或：

```http
X-API-Key: <MCP_API_KEY>
```

MCP 客户端可以这样配置：

```yaml
mcp_servers:
  qas:
    url: "http://yourhost:5005/mcp"
    headers:
      Authorization: "Bearer <MCP_API_KEY>"
```

本机 stdio 配置中的路径必须是 MCP 客户端所在环境里的实际路径；容器内路径示例：

```yaml
mcp_servers:
  qas-local:
    command: "python3"
    args: ["/app/app/run.py", "--mcp-stdio"]
    env:
      QAS_MCP_API_KEY: "<MCP_API_KEY>"
```

当前工具包括任务查询/创建/修改/删除/运行、运行状态与日志查询、电视剧/资源搜索、分享详情、夸克文件浏览/删除/重命名、脱敏配置读取和系统状态查询。默认只开放读取类权限；删除、重命名、修改和运行等写操作必须在设置中心显式开启。API key 只保存哈希，不会通过 `/data` 或 MCP 返回。

浏览器跨域调用默认关闭；确需使用时，通过环境变量 `MCP_ALLOWED_ORIGINS` 指定逗号分隔的完整 Origin，禁止使用 `*`。

| 环境变量              | 默认       | 备注                                      |
| --------------------- | ---------- | ----------------------------------------- |
| `WEBUI_USERNAME`      | `admin`    | 管理账号                                  |
| `WEBUI_PASSWORD`      | `admin123` | 管理密码                                  |
| `PORT`                | `5005`     | 管理后台端口                              |
| `PLUGIN_FLAGS`        |            | 插件标志，如 `-emby,-aria2` 禁用某些插件  |
| `TASK_TIMEOUT`        | `1800`     | 任务执行超时时间（秒），超时则任务结束    |
| `MCP_ALLOWED_ORIGINS` | 空         | MCP 浏览器跨域允许的完整 Origin，逗号分隔  |
| `QAS_MCP_API_KEY`     | 空         | 仅用于 `--mcp-stdio` 的明文 API key        |

#### 本 fork 发布（维护者）

推送 `v*` tag 会触发 `.github/workflows/docker-publish.yml`：构建 `linux/amd64`、`linux/arm64`、`linux/arm/v7` 镜像，推送到 GHCR，并创建 GitHub Release。

```shell
git fetch origin main
git tag -a vX.Y.Z -m "Release vX.Y.Z"
git push origin vX.Y.Z
```

镜像同时生成 `vX.Y.Z`、`X.Y.Z`、`X.Y`、`X` 和 `latest` 标签。

<details open>
<summary>WebUI 预览</summary>

![screenshot_webui](img/screenshot_webui-1.png)

![screenshot_webui](img/screenshot_webui-2.png)

</details>

## 使用说明

### 正则处理示例

| pattern                                | replace                 | 效果                                                                   |
| -------------------------------------- | ----------------------- | ---------------------------------------------------------------------- |
| `.*`                                   |                         | 无脑转存所有文件，不整理                                               |
| `\.mp4$`                               |                         | 转存所有 `.mp4` 后缀的文件                                             |
| `^【电影TT】花好月圆(\d+)\.(mp4\|mkv)` | `\1.\2`                 | 【电影TT】花好月圆01.mp4 → 01.mp4<br>【电影TT】花好月圆02.mkv → 02.mkv |
| `^(\d+)\.mp4`                          | `S02E\1.mp4`            | 01.mp4 → S02E01.mp4<br>02.mp4 → S02E02.mp4                             |
| `$TV`                                  |                         | [魔法匹配](#魔法匹配)剧集文件                                          |
| `^(\d+)\.mp4`                          | `{TASKNAME}.S02E\1.mp4` | 01.mp4 → 任务名.S02E01.mp4                                             |

正则匹配示例见本节；任务支持 Python 正则表达式。

> [!TIP]
>
> **魔法匹配和魔法变量**：在正则处理中，我们定义了一些“魔法匹配”模式，如果 表达式 的值以 $ 开头且 替换式 留空，程序将自动使用预设的正则表达式进行匹配和替换。
>
> 自 v0.6.0 开始，支持更多以 {} 包裹的我称之为“魔法变量”，可以更灵活地进行重命名。
>
> 魔法变量可直接用于 `replace`，例如 `{TASKNAME}.S{SXX}E{E}.{EXT}`。

### 刷新媒体库

在有新转存时，可通过插件刷新媒体库或生成 `.strm` 文件。

媒体库模块以插件方式集成，扩展代码位于 [plugins](./plugins) 目录。

## 声明

本项目为个人兴趣开发，旨在通过程序自动化提高网盘使用效率。

程序没有任何破解行为，只是对于夸克已有的API进行封装，所有数据来自于夸克官方API；本人不对网盘内容负责、不对夸克官方API未来可能的变动导致的影响负责，请自行斟酌使用。

开源仅供学习与交流使用，未盈利也未授权商业使用，严禁用于非法用途。

## LICENSE

本项目基于 [AGPL-3.0](LICENSE)  协议开源，这意味着：

1. **传染性与开源义务**：任何使用了本项目代码的作品（包括修改或链接）在分发时，必须以 AGPL-3.0 开源。
2. **网络服务也需开源**：基于该项目衍生的网络服务（如 API、Web 等）需要向用户提供修改后源代码的获取方式。
3. **修改与分发要求**：必须保留原版权声明和许可证、修改后的文件需标注修改说明、分发时必须提供完整的源代码。
