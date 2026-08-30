---
name: quark-auto-save
description: Manage quark-auto-save tasks via curl or MCP. (QAS, 夸克自动转存, 夸克转存, 夸克订阅, 管理任务, 运行任务, 修复失效链接, pan.quark.cn)
metadata:
  openclaw:
    emoji: "💾"
    homepage: "https://github.com/xinghaix/quark-auto-save"
    requires:
      env:
        - QAS_BASE_URL
        - QAS_TOKEN
      bins:
        - curl
    primaryEnv: QAS_TOKEN
---

# quark-auto-save

Manage quark-auto-save via HTTP (`?token=$QAS_TOKEN`) or MCP. Server is Go; there is no Python client.

When the user sends `https://pan.quark.cn/s/***`, get share detail, then add or run a task.

## Env

- `QAS_BASE_URL` e.g. `http://192.168.1.x:5005`
- `QAS_TOKEN` WebUI/API token

Set in skill env (`openclaw.json` → `skills.entries.quark-auto-save.env`). Restart gateway after editing.

## MCP

Enable in WebUI「系统配置 → MCP 服务」:

- HTTP: `${QAS_BASE_URL}/mcp`
- Auth: `Authorization: Bearer <MCP_API_KEY>` or `X-API-Key`
- SSE: `${QAS_BASE_URL}/mcp/sse` → `/mcp/messages?sessionId=...`
- Write/run/delete permissions default off

stdio: `/usr/local/bin/quark-auto-save --mcp-stdio` with `QAS_MCP_API_KEY`.

## HTTP helpers

```bash
qas_get() { curl -sS -G "$QAS_BASE_URL$1" --data-urlencode "token=$QAS_TOKEN" "${@:2}"; }
qas_post() { curl -sS -X POST -H 'Content-Type: application/json' "$QAS_BASE_URL$1?token=$QAS_TOKEN" -d "$2"; }
```

```bash
qas_get /data
qas_get /task_suggestions --data-urlencode "q=query" --data-urlencode "d=1"
qas_post /get_share_detail '{"shareurl":"https://pan.quark.cn/s/xxx"}'
qas_get /get_savepath_detail --data-urlencode "path=/video/tv/Name"
qas_post /delete_file '{"path":"/path/to/file"}'
qas_post /rename_file '{"path":"/path/to/file","file_name":"new_name"}'
qas_post /api/add_task '{"taskname":"Name","shareurl":"https://pan.quark.cn/s/xxx","savepath":"/video/tv/Name","pattern":"$TV"}'
qas_post /run_script_now '{}'
qas_post /run_script_now '{"tasklist":[{"taskname":"Name",...}]}'
qas_post /update '{"crontab":"0 9 * * *"}'
```

Update/delete a saved task: GET `/data`, change `tasklist`, POST `/update` with `{"tasklist":[...]}`. Clear `shareurl_ban` when replacing a dead link.

## First configuration

After token is set, GET `/data`, extract savepath/pattern/replace habits, write them to TOOLS.md.

```markdown
### quark-auto-save habits
#### TV Series
   - Directory: `/video/tv/{name}`
   - naming preferences: `{TASKNAME}.{SXX}E{E}.{EXT}`
```

## Task schema

Required: `taskname`, `shareurl`, `savepath`. Optional: `pattern`, `replace`, `update_subdir`, `ignore_extension`, `runweek`, `addition`.

```json
{
  "taskname": "MediaName",
  "shareurl": "https://pan.quark.cn/s/xxx#/list/share/fid",
  "savepath": "/video/tv/MediaName",
  "pattern": "$TV",
  "replace": "",
  "runweek": [1,2,3,4,5,6,7]
}
```

Share URL: `https://pan.quark.cn/s/{id}` or with `#/list/share/{fid}`. Prefer video (mp4/mkv) and higher resolution.

### pattern / replace

| Pattern | Replace | Result |
|---|---|---|
| `.*` | | Save all |
| `\.(mp4\|mkv)$` | | Videos only |
| `^(\d+)\.mp4` | `S02E\1.mp4` | 01.mp4 → S02E01.mp4 |
| `$TV` | | Magic TV regex |

Magic vars: `{TASKNAME}` `{II}` `{EXT}` `{SXX}` `{E}` `{DATE}`.

## Workflows

### Add

1. Search `/task_suggestions?q=Name&d=1`
2. POST `/get_share_detail` — pick a valid share, prefer matching video ext
3. Match TOOLS.md naming. Source already matches → `pattern: ".*"`. Else capture groups + magic vars.
4. One-time (完结/全集/movie) → `/run_script_now` with `tasklist`. Subscription → `/api/add_task` then `/run_script_now` with that task.

### Fix dead link

1. `/data` — tasks with `shareurl_ban`
2. `/get_savepath_detail` — latest episode / naming
3. Search + `/get_share_detail` — same ext, episodes beyond latest
4. PATCH via `/update` (`shareurl`, clear `shareurl_ban`, maybe pattern/replace)
5. `/run_script_now` that task

### Run

- all: `qas_post /run_script_now '{}'`
- one saved task: include it in `tasklist`
- unsaved: `{"tasklist":[{...}]}`
