# !/usr/bin/env python3
# -*- coding: utf-8 -*-

"""Legacy Flask entrypoint retained for source compatibility; production uses the Go 1.27 binary.
"""
from flask import (
    json,
    Flask,
    url_for,
    session,
    jsonify,
    request,
    redirect,
    Response,
    render_template,
    send_from_directory,
    stream_with_context,
)
from apscheduler.schedulers.background import BackgroundScheduler
from apscheduler.triggers.cron import CronTrigger
from concurrent.futures import ThreadPoolExecutor, as_completed
from sdk.cloudsaver import CloudSaver
from sdk.pansou import PanSou
from datetime import datetime, timedelta, timezone
from collections import deque
import queue
import copy
import subprocess
import time
import requests
import hashlib
import logging
import traceback
import base64
import sys
import os
import re
import shlex
import threading
import uuid
from urllib.parse import urlparse

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, parent_dir)
from quark_auto_save import Quark, Config, MagicRename
from app.mcp import (
    MCPService,
    MCPTransportError,
    DEFAULT_PROTOCOL_VERSION,
    DEFAULT_PERMISSIONS,
    mcp_config_for_ui,
    merge_mcp_config,
    normalize_mcp_config,
    run_stdio,
)


def get_app_ver():
    """获取应用版本"""
    try:
        with open("build.json", "r") as f:
            build_info = json.loads(f.read())
            BUILD_SHA = build_info["BUILD_SHA"]
            BUILD_TAG = build_info["BUILD_TAG"]
    except Exception as e:
        BUILD_SHA = os.getenv("BUILD_SHA", "")
        BUILD_TAG = os.getenv("BUILD_TAG", "")
    if BUILD_TAG[:1] == "v":
        return BUILD_TAG
    elif BUILD_SHA:
        return f"{BUILD_TAG}({BUILD_SHA[:7]})"
    else:
        return "dev"


# 文件路径
PYTHON_PATH = "python3" if os.path.exists("/usr/bin/python3") else "python"
SCRIPT_PATH = os.environ.get("SCRIPT_PATH", "./quark_auto_save.py")
CONFIG_PATH = os.environ.get("CONFIG_PATH", "./config/quark_config.json")
PLUGIN_FLAGS = os.environ.get("PLUGIN_FLAGS", "")
DEBUG = os.environ.get("DEBUG", "false").lower() == "true"
HOST = os.environ.get("HOST", "0.0.0.0")
PORT = os.environ.get("PORT", 5005)
TASK_TIMEOUT = int(os.environ.get("TASK_TIMEOUT", 1800))

config_data = {}
task_plugins_config_default = {}
CONFIG_LOCK = threading.RLock()
MCP_RUNS = {}
MCP_ACTIVE_TASKS = {}
MCP_RUNS_LOCK = threading.RLock()
LOG_BUFFER = deque(maxlen=2000)
LOG_BUFFER_LOCK = threading.Lock()
LOG_CURSOR = 0
MCP_RATE_WINDOW = 60
MCP_RATE_LIMIT = 120
MCP_RATE_BUCKETS = {}
MCP_RATE_LOCK = threading.Lock()
MCP_LEGACY_STREAMS = {}
MCP_LEGACY_STREAMS_LOCK = threading.RLock()

app = Flask(__name__)
app.config["APP_VERSION"] = get_app_ver()
app.secret_key = "ca943f6db6dd34823d36ab08d8d6f65d"
app.config["SESSION_COOKIE_NAME"] = "QUARK_AUTO_SAVE_SESSION"
app.config["PERMANENT_SESSION_LIFETIME"] = timedelta(days=31)
app.json.ensure_ascii = False
app.json.sort_keys = False
app.jinja_env.variable_start_string = "[["
app.jinja_env.variable_end_string = "]]"

scheduler = BackgroundScheduler()
logging.basicConfig(
    level=logging.DEBUG if DEBUG else logging.INFO,
    format="[%(asctime)s][%(levelname)s] %(message)s",
    datefmt="%m-%d %H:%M:%S",
)
# 过滤werkzeug日志输出
if not DEBUG:
    logging.getLogger("werkzeug").setLevel(logging.ERROR)
    logging.getLogger("apscheduler").setLevel(logging.ERROR)
    sys.modules["flask.cli"].show_server_banner = lambda *x: None


_BEARER_RE = re.compile(r"(?i)\bbearer\s+[^\s,;]+")
_SENSITIVE_LOG_RE = re.compile(
    r"(?i)([\"']?(?:authorization|api[_-]?key|password|passwd|token|stoken|"
    r"access[_-]?token|refresh[_-]?token|cookie|secret|push[_-]?key|deer[_-]?key|"
    r"qywx[_-]?key|bark[_-]?push|ntfy[_-]?(?:url|topic)|webhook[_-]?url|"
    r"smtp[_-]?(?:password|pass))\b[\"']?\s*[:=]\s*[\"']?)([^\"'\s,;}]+)"
)


def _redact_text(value):
    text = _BEARER_RE.sub("Bearer [REDACTED]", str(value))
    return _SENSITIVE_LOG_RE.sub(r"\1[REDACTED]", text)


class _MCPLogHandler(logging.Handler):
    def emit(self, record):
        global LOG_CURSOR
        try:
            entry = {
                "timestamp": datetime.fromtimestamp(record.created, timezone.utc).isoformat(),
                "level": record.levelname,
                "message": _redact_text(record.getMessage()),
                "run_id": getattr(record, "mcp_run_id", None),
            }
            with LOG_BUFFER_LOCK:
                LOG_CURSOR += 1
                entry["cursor"] = LOG_CURSOR
                LOG_BUFFER.append(entry)
        except Exception:
            self.handleError(record)


if not any(isinstance(handler, _MCPLogHandler) for handler in logging.getLogger().handlers):
    logging.getLogger().addHandler(_MCPLogHandler())


def _redact_data(value, key=""):
    sensitive_keys = {
        "cookie", "password", "token", "stoken", "api_key", "api_key_hash",
        "authorization", "secret", "webui",
    }

    def is_sensitive(name):
        normalized = re.sub(r"[^a-z0-9_]", "_", str(name).lower())
        explicit = {
            "push_key", "deer_key", "qywx_key", "bark_push", "ntfy_url",
            "ntfy_topic", "webhook_url", "smtp_email", "smtp_pass",
            "smtp_password", "tg_bot_token", "tg_user_id", "push_plus_token",
        }
        return (
            normalized in sensitive_keys
            or normalized in explicit
            or normalized.endswith("_key")
            or normalized.endswith("key")
            or any(
                part in normalized
                for part in ("password", "passwd", "token", "cookie", "secret", "authorization")
            )
        )

    if isinstance(value, dict):
        return {
            name: "[REDACTED]" if is_sensitive(name) else _redact_data(item, name)
            for name, item in value.items()
            if str(name).lower() != "webui"
        }
    if isinstance(value, list):
        return [_redact_data(item, key) for item in value]
    return value


def _log_snapshot(query="", level="", run_id="", cursor=0, limit=50):
    query = str(query or "").lower()
    level = str(level or "").upper()
    cursor = int(cursor or 0)
    limit = max(1, min(int(limit or 50), 200))
    with LOG_BUFFER_LOCK:
        entries = list(LOG_BUFFER)
    result = []
    for entry in entries:
        if entry["cursor"] <= cursor:
            continue
        if level and entry["level"] != level:
            continue
        if run_id and entry.get("run_id") != run_id:
            continue
        if query and query not in entry["message"].lower():
            continue
        result.append(copy.deepcopy(entry))
    return {
        "items": result[:limit],
        "next_cursor": result[min(limit, len(result)) - 1]["cursor"] if result[:limit] else cursor,
        "has_more": len(result) > limit,
    }


def print_banner():
    print(
        r"""
   ____    ___   _____
  / __ \  /   | / ___/
 / / / / / /| | \__ \
/ /_/ / / ___ |___/ /
\___\_\/_/  |_/____/

-- Quark-Auto-Save --
 """
    )
    sys.stdout.flush()


def gen_md5(string):
    md5 = hashlib.md5()
    md5.update(string.encode("utf-8"))
    return md5.hexdigest()


def get_login_token():
    username = config_data["webui"]["username"]
    password = config_data["webui"]["password"]
    return gen_md5(f"token{username}{password}+-*/")[8:24]


def is_login():
    login_token = get_login_token()
    if session.get("token") == login_token or request.args.get("token") == login_token:
        return True
    else:
        return False


def _preserve_task_ids_locked(incoming_tasks):
    """Keep IDs when an older UI submits a task list without the new field."""
    if not isinstance(incoming_tasks, list):
        return
    existing = config_data.get("tasklist", [])
    by_signature = {}
    for old_index, old_task in enumerate(existing):
        if not isinstance(old_task, dict) or not old_task.get("id"):
            continue
        signature = tuple(str(old_task.get(key, "")) for key in ("taskname", "shareurl", "savepath"))
        by_signature.setdefault(signature, []).append((old_index, old_task["id"]))
    used_ids = set()
    for index, task in enumerate(incoming_tasks):
        if not isinstance(task, dict):
            continue
        task_id = task.get("id")
        if isinstance(task_id, str) and task_id.strip() and task_id not in used_ids:
            used_ids.add(task_id)
            continue
        signature = tuple(str(task.get(key, "")) for key in ("taskname", "shareurl", "savepath"))
        candidates = by_signature.get(signature, [])
        while candidates and candidates[0][1] in used_ids:
            candidates.pop(0)
        if candidates:
            task_id = candidates.pop(0)[1]
        elif index < len(existing) and isinstance(existing[index], dict) and existing[index].get("id") not in used_ids:
            task_id = existing[index]["id"]
        else:
            task_id = uuid.uuid4().hex
        task["id"] = task_id
        used_ids.add(task_id)


class QASMCPBackend:
    """Adapter exposing existing QAS operations to the MCP protocol."""

    def get_mcp_config(self):
        with CONFIG_LOCK:
            return normalize_mcp_config(config_data.get("mcp", {}))

    @staticmethod
    def _ensure_task_ids_locked():
        changed = False
        used_ids = set()
        for task in config_data.setdefault("tasklist", []):
            task_id = task.get("id") if isinstance(task, dict) else None
            if not isinstance(task_id, str) or not task_id.strip() or task_id in used_ids:
                task_id = uuid.uuid4().hex
                task["id"] = task_id
                changed = True
            used_ids.add(task_id)
        return changed

    @staticmethod
    def _public_task(task, index):
        value = _redact_data(copy.deepcopy(task))
        value["index"] = index
        return value

    @staticmethod
    def _validate_share_url(value):
        parsed = urlparse(str(value))
        if parsed.scheme not in {"http", "https"} or parsed.hostname != "pan.quark.cn":
            raise ValueError("shareurl 必须是 pan.quark.cn 分享链接")

    @staticmethod
    def _validate_task_values(task, *, require_required=False):
        if not isinstance(task, dict):
            raise ValueError("task 必须是对象")
        if require_required:
            for field in ("taskname", "shareurl", "savepath"):
                if not isinstance(task.get(field), str) or not task[field].strip():
                    raise ValueError(f"缺少必要字段: {field}")
        for field in ("taskname", "shareurl", "savepath", "pattern", "replace", "enddate", "update_subdir"):
            if field in task and not isinstance(task[field], str):
                raise ValueError(f"{field} 必须是字符串")
        if "shareurl" in task:
            QASMCPBackend._validate_share_url(task["shareurl"])
        if "savepath" in task and task["savepath"] and not task["savepath"].startswith("/"):
            raise ValueError("savepath 必须以 / 开头")
        if "runweek" in task:
            if (
                not isinstance(task["runweek"], list)
                or any(not isinstance(day, int) or isinstance(day, bool) or day not in range(1, 8) for day in task["runweek"])
            ):
                raise ValueError("runweek 必须是 1 到 7 的数组")
        if "startfid" in task and (
            not isinstance(task["startfid"], (str, int)) or isinstance(task["startfid"], bool)
        ):
            raise ValueError("startfid 必须是字符串或整数")
        if "addition" in task and not isinstance(task["addition"], dict):
            raise ValueError("addition 必须是对象")
        for field in ("ignore_extension", "update_subdir_resave_mode"):
            if field in task and not isinstance(task[field], bool):
                raise ValueError(f"{field} 必须是布尔值")

    @staticmethod
    def _selector_index_locked(args):
        selectors = [
            ("task_id", args.get("task_id")),
            ("task_name", args.get("task_name")),
            ("index", args.get("index")),
        ]
        selected = [(name, value) for name, value in selectors if value is not None and value != ""]
        if len(selected) != 1:
            raise ValueError("必须且只能提供 task_id、task_name 或 index 之一")
        selector, value = selected[0]
        tasks = config_data.setdefault("tasklist", [])
        if selector == "index":
            if isinstance(value, bool) or not isinstance(value, int) or value < 0 or value >= len(tasks):
                raise ValueError("任务 index 无效")
            return value
        for index, task in enumerate(tasks):
            if selector == "task_id" and str(task.get("id", "")) == str(value):
                return index
            if selector == "task_name" and task.get("taskname") == value:
                return index
        raise ValueError("未找到任务")

    def list_tasks(self):
        with CONFIG_LOCK:
            if self._ensure_task_ids_locked():
                Config.write_json(CONFIG_PATH, config_data)
            tasks = [self._public_task(task, index) for index, task in enumerate(config_data.get("tasklist", []))]
        return {"success": True, "tasks": tasks, "count": len(tasks)}

    def get_task(self, args):
        with CONFIG_LOCK:
            index = self._selector_index_locked(args)
            return {"success": True, "task": self._public_task(config_data["tasklist"][index], index)}

    def create_task(self, task):
        allowed = {
            "taskname", "shareurl", "savepath", "pattern", "replace", "enddate", "runweek",
            "addition", "ignore_extension", "update_subdir", "update_subdir_resave_mode", "startfid",
        }
        if not isinstance(task, dict):
            raise ValueError("task 必须是对象")
        unknown = set(task) - allowed
        if unknown:
            raise ValueError(f"未知任务字段: {', '.join(sorted(unknown))}")
        self._validate_task_values(task, require_required=True)
        value = {
            "taskname": task["taskname"].strip(),
            "shareurl": task["shareurl"],
            "savepath": task["savepath"],
            "pattern": task.get("pattern", ""),
            "replace": task.get("replace", ""),
            "enddate": task.get("enddate", ""),
            "runweek": copy.deepcopy(task.get("runweek", [1, 2, 3, 4, 5, 6, 7])),
            "addition": copy.deepcopy(task.get("addition", task_plugins_config_default)),
            "ignore_extension": task.get("ignore_extension", False),
            "update_subdir": task.get("update_subdir", ""),
            "update_subdir_resave_mode": task.get("update_subdir_resave_mode", False),
            "startfid": task.get("startfid", ""),
            "id": uuid.uuid4().hex,
        }
        with CONFIG_LOCK:
            config_data.setdefault("tasklist", []).append(value)
            Config.write_json(CONFIG_PATH, config_data)
            index = len(config_data["tasklist"]) - 1
        logging.info("MCP 创建任务: %s", value["taskname"])
        return {"success": True, "task": self._public_task(value, index)}

    def update_task(self, args):
        patch = args.get("patch")
        if not isinstance(patch, dict) or not patch:
            raise ValueError("patch 必须是非空对象")
        allowed = {
            "taskname", "shareurl", "savepath", "pattern", "replace", "enddate", "runweek",
            "addition", "ignore_extension", "update_subdir", "update_subdir_resave_mode", "startfid",
        }
        unknown = set(patch) - allowed
        if unknown:
            raise ValueError(f"未知任务字段: {', '.join(sorted(unknown))}")
        self._validate_task_values(patch)
        with CONFIG_LOCK:
            index = self._selector_index_locked(args)
            task = config_data["tasklist"][index]
            merged = copy.deepcopy(task)
            merged.update(copy.deepcopy(patch))
            self._validate_task_values(merged, require_required=True)
            if "shareurl" in patch:
                merged.pop("shareurl_ban", None)
            config_data["tasklist"][index] = merged
            Config.write_json(CONFIG_PATH, config_data)
        logging.info("MCP 修改任务: %s", merged.get("taskname", ""))
        return {"success": True, "task": self._public_task(merged, index)}

    def delete_task(self, args):
        with CONFIG_LOCK:
            index = self._selector_index_locked(args)
            removed = config_data["tasklist"].pop(index)
            Config.write_json(CONFIG_PATH, config_data)
        logging.info("MCP 删除任务: %s", removed.get("taskname", ""))
        return {"success": True, "task": self._public_task(removed, index)}

    def run_tasks(self, args):
        selector_present = any(args.get(key) is not None and args.get(key) != "" for key in ("task_id", "task_name", "index"))
        with CONFIG_LOCK:
            if self._ensure_task_ids_locked():
                Config.write_json(CONFIG_PATH, config_data)
            if selector_present:
                index = self._selector_index_locked(args)
                tasklist = [copy.deepcopy(config_data["tasklist"][index])]
            else:
                tasklist = copy.deepcopy(config_data.get("tasklist", []))
        result = start_mcp_run(tasklist)
        if args.get("wait"):
            result = wait_mcp_run(result["run_id"])
        return result

    def run_status(self, args):
        return get_mcp_run_status(args["run_id"])

    def logs_query(self, args):
        return {"success": True, **_log_snapshot(
            query=args.get("query", ""),
            level=args.get("level", ""),
            run_id=args.get("run_id", ""),
            cursor=args.get("cursor", 0),
            limit=args.get("limit", 50),
        )}

    def search_tv(self, args):
        return search_task_suggestions(args["name"], bool(args.get("deep", False)))

    @staticmethod
    def _account():
        with CONFIG_LOCK:
            cookies = config_data.get("cookie", [])
            if isinstance(cookies, str):
                cookies = [cookies]
            cookie = cookies[0] if cookies else ""
        if not cookie:
            raise ValueError("未配置夸克 Cookie")
        return Quark(cookie)

    def files_list(self, args):
        if args.get("fid") is not None:
            return {"success": True, "data": _get_file_list(fid=args["fid"])}
        return {"success": True, "data": _get_file_list(path=args.get("path") or "/")}

    def share_inspect(self, args):
        data = get_share_detail_data(args["shareurl"], args.get("stoken", ""))
        data.pop("stoken", None)
        return {"success": True, "data": data}

    def files_delete(self, args):
        fid = args.get("fid") or _path_to_fid(args.get("path"))
        if not fid:
            raise ValueError("缺失必要字段: fid 或 path")
        account = self._account()
        response = account.delete([fid])
        response["success"] = response.get("code") == 0
        return response

    def files_rename(self, args):
        fid = args.get("fid") or _path_to_fid(args.get("path"))
        if not fid or not args.get("file_name"):
            raise ValueError("缺失必要字段: fid/path 或 file_name")
        account = self._account()
        response = account.rename(fid, args["file_name"])
        response["success"] = response.get("code") == 0
        return response

    def config_get(self):
        with CONFIG_LOCK:
            snapshot = copy.deepcopy(config_data)
        return {"success": True, "config": _redact_data(snapshot)}

    def system_status(self):
        mcp_config = self.get_mcp_config()
        scheduler_state = {0: "stopped", 1: "running", 2: "paused"}.get(scheduler.state, "unknown")
        with CONFIG_LOCK:
            task_count = len(config_data.get("tasklist", []))
        return {
            "success": True,
            "version": app.config["APP_VERSION"],
            "task_count": task_count,
            "scheduler": scheduler_state,
            "mcp": {
                "enabled": mcp_config["enabled"],
                "api_key_configured": bool(mcp_config["api_key_hash"]),
                "permissions": mcp_config["permissions"],
            },
        }

    def call_tool(self, name, args):
        handlers = {
            "qas_tasks_list": lambda: self.list_tasks(),
            "qas_tasks_get": lambda: self.get_task(args),
            "qas_tasks_create": lambda: self.create_task(args["task"]),
            "qas_tasks_update": lambda: self.update_task(args),
            "qas_tasks_delete": lambda: self.delete_task(args),
            "qas_tasks_run": lambda: self.run_tasks(args),
            "qas_run_status": lambda: self.run_status(args),
            "qas_logs_query": lambda: self.logs_query(args),
            "qas_search_tv": lambda: self.search_tv(args),
            "qas_files_list": lambda: self.files_list(args),
            "qas_share_inspect": lambda: self.share_inspect(args),
            "qas_files_delete": lambda: self.files_delete(args),
            "qas_files_rename": lambda: self.files_rename(args),
            "qas_config_get": lambda: self.config_get(),
            "qas_system_status": lambda: self.system_status(),
        }
        if name not in handlers:
            raise ValueError(f"未知工具: {name}")
        return handlers[name]()


mcp_backend = QASMCPBackend()
mcp_service = MCPService(mcp_backend)


def _run_status_payload(record):
    return {
        "success": True,
        "run_id": record["run_id"],
        "status": record["status"],
        "started_at": record["started_at"],
        "finished_at": record.get("finished_at"),
        "returncode": record.get("returncode"),
        "timed_out": record.get("timed_out", False),
        "task_count": record["task_count"],
        "log_tail": [_redact_text(item) for item in list(record.get("output", []))[-100:]],
    }


def _terminate_run_after_timeout(run_id, process):
    if TASK_TIMEOUT <= 0:
        return
    with MCP_RUNS_LOCK:
        record = MCP_RUNS.get(run_id)
        done_event = record.get("done_event") if record else None
    if done_event and done_event.wait(TASK_TIMEOUT):
        return
    if process.poll() is None:
        with MCP_RUNS_LOCK:
            record = MCP_RUNS.get(run_id)
            if record:
                record["timed_out"] = True
        logging.error("MCP run %s timed out after %ss", run_id, TASK_TIMEOUT, extra={"mcp_run_id": run_id})
        process.kill()


def _task_run_keys(tasklist):
    keys = []
    for index, task in enumerate(tasklist):
        task_id = str(task.get("id") or "").strip() if isinstance(task, dict) else ""
        keys.append(f"task:{task_id or f'index:{index}'}")
    return tuple(dict.fromkeys(keys))


def _release_mcp_tasks_locked(run_id, record):
    for task_key in record.get("task_keys", ()):
        if MCP_ACTIVE_TASKS.get(task_key) == run_id:
            MCP_ACTIVE_TASKS.pop(task_key, None)


def _collect_mcp_run(run_id, process):
    try:
        for line in iter(process.stdout.readline, ""):
            text = line.rstrip("\r\n")
            if not text:
                continue
            text = _redact_text(text[:4000])
            with MCP_RUNS_LOCK:
                record = MCP_RUNS.get(run_id)
                if record:
                    record["output"].append(text)
            logging.info(text, extra={"mcp_run_id": run_id})
        returncode = process.wait()
        with MCP_RUNS_LOCK:
            record = MCP_RUNS.get(run_id)
            if record:
                record["returncode"] = returncode
                record["status"] = "timed_out" if record.get("timed_out") else ("completed" if returncode == 0 else "failed")
                record["finished_at"] = datetime.now(timezone.utc).isoformat()
                _release_mcp_tasks_locked(run_id, record)
    except Exception as exc:
        logging.error("MCP run %s collector failed: %s", run_id, _redact_text(exc), extra={"mcp_run_id": run_id})
        with MCP_RUNS_LOCK:
            record = MCP_RUNS.get(run_id)
            if record:
                record["status"] = "failed"
                record["error"] = _redact_text(exc)
                record["finished_at"] = datetime.now(timezone.utc).isoformat()
                _release_mcp_tasks_locked(run_id, record)
    finally:
        if process.stdout:
            process.stdout.close()
        with MCP_RUNS_LOCK:
            record = MCP_RUNS.get(run_id)
            if record:
                _release_mcp_tasks_locked(run_id, record)
                if record.get("done_event"):
                    record["done_event"].set()
            _trim_mcp_runs_locked({run_id})


def _trim_mcp_runs_locked(protected_ids=()):
    protected_ids = set(protected_ids)
    # Keep active runs addressable; completed history is the bounded portion.
    for old_id, old_record in list(MCP_RUNS.items()):
        if len(MCP_RUNS) <= 50:
            break
        if old_id in protected_ids or old_record.get("status") == "running":
            continue
        MCP_RUNS.pop(old_id, None)


def start_mcp_run(tasklist):
    run_id = uuid.uuid4().hex
    task_keys = _task_run_keys(tasklist)
    with MCP_RUNS_LOCK:
        conflicts = sorted({MCP_ACTIVE_TASKS[key] for key in task_keys if key in MCP_ACTIVE_TASKS})
        if conflicts:
            raise ValueError("任务正在运行: " + ", ".join(conflicts))
        for task_key in task_keys:
            MCP_ACTIVE_TASKS[task_key] = run_id

    process_env = os.environ.copy()
    process_env["PYTHONIOENCODING"] = "utf-8"
    process_env["TASKLIST"] = json.dumps(tasklist, ensure_ascii=False)
    command = [PYTHON_PATH, "-u", SCRIPT_PATH, CONFIG_PATH]
    try:
        process = subprocess.Popen(
            command,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            universal_newlines=True,
            encoding="utf-8",
            errors="replace",
            bufsize=1,
            env=process_env,
        )
    except Exception:
        with MCP_RUNS_LOCK:
            for task_key in task_keys:
                if MCP_ACTIVE_TASKS.get(task_key) == run_id:
                    MCP_ACTIVE_TASKS.pop(task_key, None)
        raise

    record = {
        "run_id": run_id,
        "status": "running",
        "started_at": datetime.now(timezone.utc).isoformat(),
        "finished_at": None,
        "returncode": None,
        "timed_out": False,
        "task_count": len(tasklist),
        "task_keys": task_keys,
        "output": deque(maxlen=200),
        "done_event": threading.Event(),
        "process": process,
    }
    with MCP_RUNS_LOCK:
        MCP_RUNS[run_id] = record
        # ponytail: retain 50 completed runs in-process; active runs stay addressable.
        _trim_mcp_runs_locked({run_id})
    threading.Thread(target=_collect_mcp_run, args=(run_id, process), daemon=True).start()
    threading.Thread(target=_terminate_run_after_timeout, args=(run_id, process), daemon=True).start()
    logging.info("MCP run %s started (%s task(s))", run_id, len(tasklist), extra={"mcp_run_id": run_id})
    return _run_status_payload(record)


def get_mcp_run_status(run_id):
    with MCP_RUNS_LOCK:
        record = MCP_RUNS.get(run_id)
        if not record:
            raise ValueError("未找到 run_id")
        return _run_status_payload(record)


def wait_mcp_run(run_id):
    deadline = time.time() + max(1, TASK_TIMEOUT + 5)
    while time.time() < deadline:
        with MCP_RUNS_LOCK:
            record = MCP_RUNS.get(run_id)
            if not record:
                raise ValueError("未找到 run_id")
            if record["status"] != "running":
                return _run_status_payload(record)
        time.sleep(0.1)
    return get_mcp_run_status(run_id)


def search_task_suggestions(query, deep=False):
    query = str(query or "").strip().lower()
    if not query:
        raise ValueError("搜索名称不能为空")
    with CONFIG_LOCK:
        source = copy.deepcopy(config_data.get("source", {}))
    cs_data = source.get("cloudsaver", {})
    ps_data = source.get("pansou", {})

    def cs_search():
        if cs_data.get("server") and cs_data.get("username") and cs_data.get("password"):
            cs = CloudSaver(cs_data.get("server"))
            cs.set_auth(cs_data.get("username", ""), cs_data.get("password", ""), cs_data.get("token", ""))
            search = cs.auto_login_search(query)
            if search.get("success"):
                if search.get("new_token"):
                    with CONFIG_LOCK:
                        config_data.setdefault("source", {}).setdefault("cloudsaver", {})["token"] = search["new_token"]
                        Config.write_json(CONFIG_PATH, config_data)
                return cs.clean_search_results(search.get("data", []))
        return []

    def ps_search():
        if ps_data.get("server"):
            return PanSou(ps_data.get("server")).search(query, deep)
        return []

    try:
        search_results = []
        with ThreadPoolExecutor(max_workers=3) as executor:
            futures = []
            if str(cs_data.get("enable", "true")).lower() == "true":
                futures.append(executor.submit(cs_search))
            if str(ps_data.get("enable", "true")).lower() == "true":
                futures.append(executor.submit(ps_search))
            for future in as_completed(futures):
                search_results.extend(future.result())
        results = []
        seen = set()
        search_results.sort(key=lambda item: item.get("datetime", ""), reverse=True)
        for item in search_results:
            url = item.get("shareurl", "")
            if url and url not in seen:
                seen.add(url)
                results.append(item)
        return {"success": True, "data": results, "query": query, "deep": bool(deep)}
    except Exception as exc:
        logging.exception("资源搜索失败")
        return {"success": False, "message": _redact_text(exc), "data": []}


def get_share_detail_data(shareurl, stoken=""):
    mcp_backend._validate_share_url(shareurl)
    account = Quark()
    pwd_id, passcode, pdir_fid, paths = account.extract_url(shareurl)
    if not stoken:
        token_result = account.get_stoken(pwd_id, passcode)
        if token_result.get("status") == 200:
            stoken = token_result["data"]["stoken"]
        else:
            raise ValueError(token_result.get("message", "获取分享 token 失败"))
    share_detail = account.get_detail(pwd_id, stoken, pdir_fid, _fetch_share=1, fetch_share_full_path=1)
    if share_detail.get("code") != 0:
        raise ValueError(share_detail.get("message", "获取分享详情失败"))
    data = share_detail["data"]
    data["paths"] = [
        {"fid": item["fid"], "name": item["file_name"]}
        for item in share_detail["data"].get("full_path", [])
    ] or paths
    data["stoken"] = stoken
    if os.getenv("FILTER_INVALID_VIDEO", "true") == "true":
        for share_file in data.get("list", []):
            if (
                share_file.get("file_name", "").lower().endswith((".mp4", ".mkv"))
                and not share_file.get("dir")
                and share_file.get("obj_category") != "video"
            ):
                raise ValueError("无效视频格式")
    return data


# 设置icon
@app.route("/favicon.ico")
def favicon():
    return send_from_directory(
        os.path.join(app.root_path, "static"),
        "favicon.ico",
        mimetype="image/vnd.microsoft.icon",
    )


# 登录页面
@app.route("/login", methods=["GET", "POST"])
def login():
    if request.method == "POST":
        username = config_data["webui"]["username"]
        password = config_data["webui"]["password"]
        # 验证用户名和密码
        if (username == request.form.get("username")) and (
            password == request.form.get("password")
        ):
            logging.info(f">>> 用户 {username} 登录成功")
            session.permanent = True
            session["token"] = get_login_token()
            return redirect(url_for("index"))
        else:
            logging.info(f">>> 用户 {username} 登录失败")
            return render_template("login.html", message="登录失败")

    if is_login():
        return redirect(url_for("index"))
    return render_template("login.html", error=None)


# 退出登录
@app.route("/logout")
def logout():
    session.pop("token", None)
    return redirect(url_for("login"))


# 管理页面
@app.route("/")
def index():
    if not is_login():
        return redirect(url_for("login"))
    return render_template(
        "index.html", version=app.config["APP_VERSION"], plugin_flags=PLUGIN_FLAGS
    )


def _mcp_origin_allowed():
    origin = request.headers.get("Origin")
    if not origin:
        return True
    origin = origin.rstrip("/")
    configured = {
        value.strip().rstrip("/")
        for value in os.environ.get("MCP_ALLOWED_ORIGINS", "").split(",")
        if value.strip()
    }
    # Do not trust Host-derived same-origin values: DNS rebinding can control
    # both Host and Origin. Browser callers must use an explicit allowlist.
    return origin in configured


def _mcp_rate_allowed():
    now = time.time()
    client = request.remote_addr or "unknown"
    with MCP_RATE_LOCK:
        bucket = MCP_RATE_BUCKETS.setdefault(client, deque())
        while bucket and bucket[0] <= now - MCP_RATE_WINDOW:
            bucket.popleft()
        if len(bucket) >= MCP_RATE_LIMIT:
            return False
        bucket.append(now)
        if len(MCP_RATE_BUCKETS) > 1024:
            for key in list(MCP_RATE_BUCKETS):
                if not MCP_RATE_BUCKETS[key] or MCP_RATE_BUCKETS[key][-1] <= now - MCP_RATE_WINDOW:
                    MCP_RATE_BUCKETS.pop(key, None)
        return True


def _mcp_token_from_request():
    authorization = request.headers.get("Authorization", "")
    scheme, _, token = authorization.partition(" ")
    if scheme.lower() == "bearer" and token.strip():
        return token.strip()
    return request.headers.get("X-API-Key", "").strip()


def _mcp_add_cors(response):
    origin = request.headers.get("Origin")
    if origin and _mcp_origin_allowed():
        response.headers["Access-Control-Allow-Origin"] = origin
        response.headers["Access-Control-Allow-Credentials"] = "false"
        response.headers.add("Vary", "Origin")
    return response


def _mcp_http_error(status, message):
    return _mcp_add_cors(jsonify({"error": message})), status


def _mcp_guard():
    if not mcp_service.enabled():
        return _mcp_http_error(404, "MCP is disabled")
    if not _mcp_origin_allowed():
        return _mcp_http_error(403, "Origin is not allowed")
    if not _mcp_rate_allowed():
        return _mcp_http_error(429, "MCP rate limit exceeded")
    if request.method == "OPTIONS":
        response = _mcp_add_cors(Response(status=204))
        response.headers["Access-Control-Allow-Methods"] = "GET, POST, DELETE, OPTIONS"
        response.headers["Access-Control-Allow-Headers"] = "Authorization, Content-Type, Mcp-Session-Id, MCP-Protocol-Version, X-API-Key"
        return response
    if not mcp_service.verify_token(_mcp_token_from_request()):
        response = _mcp_http_error(401, "Invalid MCP API key")
        response[0].headers["WWW-Authenticate"] = "Bearer"
        return response
    return None


@app.route("/mcp", methods=["GET", "POST", "DELETE", "OPTIONS"])
def mcp_endpoint():
    guard_response = _mcp_guard()
    if guard_response:
        return guard_response

    session_id = request.headers.get("Mcp-Session-Id")
    protocol_version = request.headers.get("MCP-Protocol-Version")
    if request.method == "GET":
        if "text/event-stream" not in request.headers.get("Accept", ""):
            return _mcp_http_error(405, "MCP GET requires text/event-stream")
        try:
            session = mcp_service.validate_session(session_id, protocol_version) if session_id else None
        except MCPTransportError as exc:
            return _mcp_http_error(exc.status, exc.message)
        response = _mcp_add_cors(Response(": qas-mcp\n\n", mimetype="text/event-stream"))
        response.headers["Cache-Control"] = "no-cache"
        if session:
            response.headers["MCP-Protocol-Version"] = session["version"]
        return response
    if request.method == "DELETE":
        try:
            mcp_service.close_session(session_id)
        except MCPTransportError as exc:
            return _mcp_http_error(exc.status, exc.message)
        return _mcp_add_cors(Response(status=204))

    payload = request.get_json(silent=True)
    if payload is None:
        return _mcp_http_error(400, "Request body must be a JSON-RPC object")
    if isinstance(payload, list):
        return _mcp_http_error(400, "JSON-RPC batches are not supported")
    try:
        response_data, new_session, negotiated = mcp_service.dispatch(
            payload,
            session_id=session_id,
            transport="http",
            protocol_version=protocol_version,
        )
    except MCPTransportError as exc:
        return _mcp_http_error(exc.status, exc.message)
    if response_data is None:
        return _mcp_add_cors(Response(status=202))
    response_headers = {
        "Cache-Control": "no-cache",
        "MCP-Protocol-Version": negotiated or protocol_version or DEFAULT_PROTOCOL_VERSION,
    }
    if new_session:
        response_headers["Mcp-Session-Id"] = new_session
    accept = request.headers.get("Accept", "application/json")
    if "text/event-stream" in accept and "application/json" not in accept:
        body = "event: message\ndata: " + json.dumps(response_data, ensure_ascii=False, separators=(",", ":")) + "\n\n"
        response = _mcp_add_cors(Response(body, mimetype="text/event-stream"))
        response.headers.update(response_headers)
        return response
    response = _mcp_add_cors(jsonify(response_data))
    response.headers.update(response_headers)
    return response


@app.route("/mcp/sse", methods=["GET", "OPTIONS"])
def mcp_legacy_sse():
    guard_response = _mcp_guard()
    if guard_response:
        return guard_response
    if "text/event-stream" not in request.headers.get("Accept", ""):
        return _mcp_http_error(405, "MCP SSE requires text/event-stream")
    session_id = mcp_service.create_session("2024-11-05")
    stream_queue = queue.Queue()
    with MCP_LEGACY_STREAMS_LOCK:
        MCP_LEGACY_STREAMS[session_id] = stream_queue
    endpoint = url_for("mcp_legacy_messages", sessionId=session_id, _external=True)

    def event_stream():
        try:
            yield f"event: endpoint\ndata: {endpoint}\n\n"
            while True:
                try:
                    message = stream_queue.get(timeout=15)
                except queue.Empty:
                    yield ": keepalive\n\n"
                    continue
                if message is None:
                    return
                yield "event: message\ndata: " + json.dumps(message, ensure_ascii=False, separators=(",", ":")) + "\n\n"
        finally:
            with MCP_LEGACY_STREAMS_LOCK:
                MCP_LEGACY_STREAMS.pop(session_id, None)
            try:
                mcp_service.close_session(session_id)
            except MCPTransportError:
                pass

    response = _mcp_add_cors(Response(stream_with_context(event_stream()), mimetype="text/event-stream"))
    response.headers["Cache-Control"] = "no-cache"
    response.headers["X-Accel-Buffering"] = "no"
    return response


@app.route("/mcp/messages", methods=["POST", "OPTIONS"])
def mcp_legacy_messages():
    guard_response = _mcp_guard()
    if guard_response:
        return guard_response
    session_id = request.args.get("sessionId", "")
    if not session_id:
        return _mcp_http_error(400, "sessionId is required")
    payload = request.get_json(silent=True)
    if payload is None or isinstance(payload, list):
        return _mcp_http_error(400, "Request body must be a single JSON-RPC object")
    try:
        mcp_service.validate_session(session_id, "2024-11-05")
        response_data, _, _ = mcp_service.dispatch(
            payload,
            session_id=session_id,
            transport="http",
            protocol_version="2024-11-05",
            allow_existing_initialize=True,
        )
    except MCPTransportError as exc:
        return _mcp_http_error(exc.status, exc.message)
    if response_data is not None:
        with MCP_LEGACY_STREAMS_LOCK:
            stream_queue = MCP_LEGACY_STREAMS.get(session_id)
        if stream_queue is None:
            return _mcp_http_error(404, "MCP SSE stream is not connected")
        stream_queue.put(response_data)
    return _mcp_add_cors(Response(status=202))


# 获取配置数据
@app.route("/data")
def get_data():
    if not is_login():
        return jsonify({"success": False, "message": "未登录"})
    with CONFIG_LOCK:
        data = Config.read_json(CONFIG_PATH)
        data.pop("webui", None)
        data["mcp"] = mcp_config_for_ui(data.get("mcp", {}))
        data["api_token"] = get_login_token()
        data["task_plugins_config_default"] = copy.deepcopy(task_plugins_config_default)
    return jsonify({"success": True, "data": data})


# 更新数据
@app.route("/update", methods=["POST"])
def update():
    global config_data
    if not is_login():
        return jsonify({"success": False, "message": "未登录"})
    payload = request.get_json(silent=True)
    if not isinstance(payload, dict):
        return jsonify({"success": False, "message": "请求数据无效"}), 400
    # 使用允许列表防止批量赋值攻击
    allowed_keys = ["cookie", "crontab", "push_config", "tasklist",
                    "magic_regex", "plugins", "source"]
    with CONFIG_LOCK:
        merged_mcp = None
        if "mcp" in payload:
            try:
                merged_mcp = merge_mcp_config(config_data.get("mcp", {}), payload["mcp"])
            except ValueError as exc:
                return jsonify({"success": False, "message": str(exc)}), 400
        for key, value in payload.items():
            if key in allowed_keys:
                if key == "tasklist":
                    value = copy.deepcopy(value)
                    _preserve_task_ids_locked(value)
                config_data.update({key: value})
        if merged_mcp is not None:
            config_data["mcp"] = merged_mcp
        Config.write_json(CONFIG_PATH, config_data)
        # 重新加载任务
        if reload_tasks():
            logging.info(f">>> 配置更新成功")
            return jsonify({"success": True, "message": "配置更新成功"})
        else:
            logging.info(f">>> 配置更新失败")
            return jsonify({"success": False, "message": "配置更新失败"})


# 处理运行脚本请求
@app.route("/run_script_now", methods=["POST"])
def run_script_now():
    if not is_login():
        return jsonify({"success": False, "message": "未登录"})
    tasklist = request.json.get("tasklist", [])
    command = [PYTHON_PATH, "-u", SCRIPT_PATH, CONFIG_PATH]
    logging.info(
        f">>> 手动运行任务 [{tasklist[0].get('taskname') if len(tasklist)>0 else 'ALL'}] 开始执行..."
    )

    def generate_output():
        # 设置环境变量
        process_env = os.environ.copy()
        process_env["PYTHONIOENCODING"] = "utf-8"
        if request.json.get("quark_test"):
            process_env["QUARK_TEST"] = "true"
            process_env["COOKIE"] = json.dumps(
                request.json.get("cookie", []), ensure_ascii=False
            )
            process_env["PUSH_CONFIG"] = json.dumps(
                request.json.get("push_config", {}), ensure_ascii=False
            )
        if tasklist:
            process_env["TASKLIST"] = json.dumps(tasklist, ensure_ascii=False)
        process = subprocess.Popen(
            command,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            universal_newlines=True,
            encoding="utf-8",
            errors="replace",
            bufsize=1,
            env=process_env,
        )
        try:
            for line in iter(process.stdout.readline, ""):
                logging.info(line.strip())
                yield f"data: {line}\n\n"
            yield "data: [DONE]\n\n"
        finally:
            process.stdout.close()
            process.wait()

    return Response(
        stream_with_context(generate_output()),
        content_type="text/event-stream;charset=utf-8",
    )


@app.route("/task_suggestions")
def get_task_suggestions():
    if not is_login():
        return jsonify({"success": False, "message": "未登录"})
    result = search_task_suggestions(
        request.args.get("q", ""),
        request.args.get("d", "").lower() == "1",
    )
    return jsonify(result)


@app.route("/get_share_detail", methods=["POST"])
def get_share_detail():
    if not is_login():
        return jsonify({"success": False, "message": "未登录"})
    shareurl = request.json.get("shareurl", "")
    stoken = request.json.get("stoken", "")
    account = Quark()
    pwd_id, passcode, pdir_fid, paths = account.extract_url(shareurl)
    if not stoken:
        get_stoken = account.get_stoken(pwd_id, passcode)
        if get_stoken.get("status") == 200:
            stoken = get_stoken["data"]["stoken"]
        else:
            return jsonify(
                {"success": False, "data": {"error": get_stoken.get("message")}}
            )
    share_detail = account.get_detail(
        pwd_id, stoken, pdir_fid, _fetch_share=1, fetch_share_full_path=1
    )

    if share_detail.get("code") != 0:
        return jsonify(
            {"success": False, "data": {"error": share_detail.get("message")}}
        )

    data = share_detail["data"]
    data["paths"] = [
        {"fid": i["fid"], "name": i["file_name"]}
        for i in share_detail["data"].get("full_path", [])
    ] or paths
    data["stoken"] = stoken

    # 过滤 01x.mp4 类型无效视频格式
    if os.getenv("FILTER_INVALID_VIDEO", "true") == "true":
        for share_file in data["list"]:
            if (
                share_file["file_name"].lower().endswith((".mp4", ".mkv"))
                and not share_file["dir"]
                and share_file["obj_category"] != "video"
            ):
                return jsonify({"success": False, "data": {"error": "无效视频格式"}})

    # 正则处理预览
    def preview_regex(data):
        task = request.json.get("task", {})
        magic_regex = request.json.get("magic_regex", {})
        mr = MagicRename(magic_regex)
        mr.set_taskname(task.get("taskname", ""))
        account = Quark(config_data["cookie"][0])
        get_fids = account.get_fids([task.get("savepath", "")])
        if get_fids:
            dir_file_list = account.ls_dir(get_fids[0]["fid"])["data"]["list"]
            dir_filename_list = [dir_file["file_name"] for dir_file in dir_file_list]
        else:
            dir_file_list = []
            dir_filename_list = []

        pattern, replace = mr.magic_regex_conv(
            task.get("pattern", ""), task.get("replace", "")
        )
        for share_file in data["list"]:
            search_pattern = (
                task["update_subdir"]
                if share_file["dir"] and task.get("update_subdir")
                else pattern
            )
            if re.search(search_pattern, share_file["file_name"]):
                # 文件名重命名，目录不重命名
                file_name_re = (
                    share_file["file_name"]
                    if share_file["dir"]
                    else mr.sub(pattern, replace, share_file["file_name"])
                )
                if file_name_saved := mr.is_exists(
                    file_name_re,
                    dir_filename_list,
                    (task.get("ignore_extension") and not share_file["dir"]),
                ):
                    share_file["file_name_saved"] = file_name_saved
                else:
                    share_file["file_name_re"] = file_name_re

        # 文件列表排序
        if re.search(r"\{I+\}", replace):
            mr.set_dir_file_list(dir_file_list, replace)
            mr.sort_file_list(data["list"])

    if request.json.get("task"):
        preview_regex(data)

    return jsonify({"success": True, "data": data})


@app.route("/get_savepath_detail")
def get_savepath_detail():
    if not is_login():
        return jsonify({"success": False, "message": "未登录"})
    try:
        if fid := request.args.get("fid", None):
            file_list = _get_file_list(fid=fid)
        elif path := request.args.get("path", "/"):
            file_list = _get_file_list(path=path)
        return jsonify({"success": True, "data": file_list})
    except Exception as e:
        return jsonify({"success": False, "data": {"error": str(e)}})


def _get_file_list(fid: str = None, path: str = None):
    account = Quark(config_data["cookie"][0])
    paths = []
    if path and not fid:
        path = re.sub(r"/+", "/", path)
        if path == "/":
            fid = 0
        else:
            dir_names = path.split("/")
            if dir_names[0] == "":
                dir_names.pop(0)
            path_fids = []
            current_path = ""
            for dir_name in dir_names:
                current_path += "/" + dir_name
                path_fids.append(current_path)
            if get_fids := account.get_fids(path_fids):
                fid = get_fids[-1]["fid"]
                paths = [
                    {"fid": get_fid["fid"], "name": dir_name}
                    for get_fid, dir_name in zip(get_fids, dir_names)
                ]
            else:
                raise FileNotFoundError("获取fid失败")
    file_list = {
        "fid": fid,
        "list": account.ls_dir(fid)["data"]["list"],
        "paths": paths,
    }
    return file_list


def _path_to_fid(path):
    """根据路径获取文件的fid"""
    if not path:
        raise ValueError("路径不能为空")
    path = re.sub(r"/+", "/", path)
    if path == "/":
        return 0
    file_list = _get_file_list(None, os.path.dirname(path))
    for file in file_list["list"]:
        if file["file_name"] == os.path.basename(path):
            return file["fid"]
    raise FileNotFoundError(f"未找到文件: {path}")


@app.route("/delete_file", methods=["POST"])
def delete_file():
    if not is_login():
        return jsonify({"success": False, "message": "未登录"})
    try:
        fid = request.json.get("fid") or _path_to_fid(request.json.get("path"))
        if fid:
            account = Quark(config_data["cookie"][0])
            response = account.delete([fid])
            response["success"] = response["code"] == 0
            return jsonify(response)
        else:
            raise ValueError("缺失必要字段: fid 或 path")
    except Exception as e:
        return jsonify({"success": False, "message": str(e)})


@app.route("/rename_file", methods=["POST"])
def rename_file():
    if not is_login():
        return jsonify({"success": False, "message": "未登录"})
    try:
        fid = request.json.get("fid") or _path_to_fid(request.json.get("path"))
        file_name = request.json.get("file_name")
        if fid and file_name:
            account = Quark(config_data["cookie"][0])
            response = account.rename(fid, file_name)
            response["success"] = response["code"] == 0
            return jsonify(response)
        else:
            raise ValueError("缺失必要字段: fid, file_name")
    except Exception as e:
        return jsonify({"success": False, "message": str(e)})


# 添加任务接口
@app.route("/api/add_task", methods=["POST"])
def add_task():
    global config_data
    # 验证token
    if not is_login():
        return jsonify({"success": False, "code": 1, "message": "未登录"}), 401
    # 必选字段
    request_data = request.get_json(silent=True)
    if not isinstance(request_data, dict):
        return jsonify({"success": False, "code": 2, "message": "请求数据无效"}), 400
    required_fields = ["taskname", "shareurl", "savepath"]
    for field in required_fields:
        if field not in request_data or not request_data[field]:
            return (
                jsonify(
                    {"success": False, "code": 2, "message": f"缺少必要字段: {field}"}
                ),
                400,
            )
    if not request_data.get("addition"):
        request_data["addition"] = copy.deepcopy(task_plugins_config_default)
    request_data.setdefault("id", uuid.uuid4().hex)
    # 添加任务
    with CONFIG_LOCK:
        config_data.setdefault("tasklist", []).append(request_data)
        Config.write_json(CONFIG_PATH, config_data)
    logging.info(f">>> 通过API添加任务: {request_data['taskname']}")
    return jsonify(
        {"success": True, "code": 0, "message": "任务添加成功", "data": request_data}
    )


# 定时任务执行的函数
def run_python(args):
    logging.info(f">>> 定时运行任务")
    try:
        result = subprocess.run(
            [PYTHON_PATH, *shlex.split(args)],
            timeout=TASK_TIMEOUT,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
        )
        # 输出执行日志
        if result.stdout:
            for line in result.stdout.strip().split("\n"):
                if line.strip():
                    logging.info(line)

        if result.returncode == 0:
            logging.info(f">>> 任务执行成功")
        else:
            logging.error(f">>> 任务执行失败，返回码: {result.returncode}")
            if result.stderr:
                logging.error(f"错误信息: {result.stderr[:500]}")
    except subprocess.TimeoutExpired as e:
        logging.error(f">>> 任务执行超时(>{TASK_TIMEOUT}s)，强制终止")
    except Exception as e:
        logging.error(f">>> 任务执行异常: {str(e)}")
        logging.error(traceback.format_exc())
    finally:
        # 确保函数能够正常返回
        logging.debug(f">>> run_python 函数执行完成")


# 重新加载任务
def reload_tasks():
    # 读取定时规则
    if crontab := config_data.get("crontab"):
        if scheduler.state == 1:
            scheduler.pause()  # 暂停调度器
        trigger = CronTrigger.from_crontab(crontab)
        scheduler.remove_all_jobs()
        scheduler.add_job(
            run_python,
            trigger=trigger,
            args=[f"{SCRIPT_PATH} {CONFIG_PATH}"],
            id=SCRIPT_PATH,
            max_instances=1,  # 最多允许1个实例运行
            coalesce=True,  # 合并错过的任务，避免堆积
            misfire_grace_time=300,  # 错过任务的宽限期(秒)，超过则跳过
            replace_existing=True,  # 替换已存在的同ID任务
        )
        if scheduler.state == 0:
            scheduler.start()
        elif scheduler.state == 2:
            scheduler.resume()
        scheduler_state_map = {0: "停止", 1: "运行", 2: "暂停"}
        logging.info(">>> 重载调度器")
        logging.info(f"调度状态: {scheduler_state_map[scheduler.state]}")
        logging.info(f"定时规则: {crontab}")
        logging.info(f"现有任务: {scheduler.get_jobs()}")
        return True
    else:
        logging.info(">>> no crontab")
        return False


def init():
    global config_data, task_plugins_config_default
    logging.info(">>> 初始化配置")
    # 检查配置文件是否存在
    if not os.path.exists(CONFIG_PATH):
        if not os.path.exists(os.path.dirname(CONFIG_PATH)):
            os.makedirs(os.path.dirname(CONFIG_PATH))
        with open("quark_config.json", "rb") as src, open(CONFIG_PATH, "wb") as dest:
            dest.write(src.read())

    # 读取配置
    config_data = Config.read_json(CONFIG_PATH)
    Config.breaking_change_update(config_data)
    if not config_data.get("magic_regex"):
        config_data["magic_regex"] = MagicRename().magic_regex

    # 默认管理账号
    config_data["webui"] = {
        "username": os.environ.get("WEBUI_USERNAME")
        or config_data.get("webui", {}).get("username", "admin"),
        "password": os.environ.get("WEBUI_PASSWORD")
        or config_data.get("webui", {}).get("password", "admin123"),
    }
    config_data["mcp"] = normalize_mcp_config(config_data.get("mcp", {}))

    # 默认定时规则
    if not config_data.get("crontab"):
        config_data["crontab"] = "0 8,18,20 * * *"

    # 初始化插件配置
    _, plugins_config_default, task_plugins_config_default = Config.load_plugins()
    for name, config in plugins_config_default.items():
        for key, value in config.items():
            config[key] = (
                config_data.setdefault("plugins", {})
                .setdefault(name, {})
                .get(key, value)
            )
    config_data["plugins"] = plugins_config_default
    _preserve_task_ids_locked(config_data.setdefault("tasklist", []))

    # 更新配置
    Config.write_json(CONFIG_PATH, config_data)


if __name__ == "__main__":
    stdio_mode = "--mcp-stdio" in sys.argv
    original_stdout = sys.stdout
    if stdio_mode:
        sys.stdout = sys.stderr
    try:
        init()
        reload_tasks()
    finally:
        if stdio_mode:
            sys.stdout = original_stdout
    if stdio_mode:
        raise SystemExit(run_stdio(mcp_service, os.environ.get("QAS_MCP_API_KEY", "")))
    print_banner()
    logging.info(">>> 启动Web服务")
    logging.info(f"运行在: http://{HOST}:{PORT}")
    app.run(
        debug=DEBUG,
        host=HOST,
        port=PORT,
    )
