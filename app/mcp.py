"""Legacy Python MCP core retained for compatibility tests; production uses the Go 1.27 MCP service.

Small dependency-free MCP server core for Quark Auto Save.

The transport is deliberately kept separate from the QAS Flask app.  The same
JSON-RPC dispatcher is used by Streamable HTTP and stdio, so protocol behavior
cannot drift between remote and local agents.
"""
from __future__ import annotations

import hashlib
import hmac
import logging
import json
import secrets
import threading
import time
from typing import Any


SUPPORTED_PROTOCOL_VERSIONS = ("2025-06-18", "2025-03-26", "2024-11-05")
DEFAULT_PROTOCOL_VERSION = SUPPORTED_PROTOCOL_VERSIONS[0]
MCP_SERVER_NAME = "quark-auto-save"
MCP_SERVER_VERSION = "1.0.0"
SESSION_TTL_SECONDS = 24 * 60 * 60

# Global permissions are intentional: the UI currently configures one API key.
# ponytail: one process-local ACL; add per-key roles only when multiple clients
# need different trust boundaries.
PERMISSION_DEFINITIONS = {
    "tasks.read": "读取任务",
    "tasks.create": "创建任务",
    "tasks.update": "修改任务",
    "tasks.delete": "删除任务",
    "tasks.run": "运行任务",
    "logs.read": "查询运行日志",
    "search.read": "搜索电视剧/资源",
    "files.read": "查看夸克文件",
    "files.write": "删除或重命名夸克文件",
    "config.read": "读取脱敏配置",
}
DEFAULT_PERMISSIONS = {
    "tasks.read": True,
    "tasks.create": False,
    "tasks.update": False,
    "tasks.delete": False,
    "tasks.run": False,
    "logs.read": True,
    "search.read": True,
    "files.read": True,
    "files.write": False,
    "config.read": False,
}


def hash_api_key(api_key: str) -> str:
    """Return a non-reversible digest for the MCP API key."""
    return hashlib.sha256(api_key.encode("utf-8")).hexdigest()


def verify_api_key(api_key: str, expected_hash: str) -> bool:
    if not api_key or not expected_hash:
        return False
    return hmac.compare_digest(hash_api_key(api_key), expected_hash)


def _config_bool(value: Any, default: bool = False) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        return value.strip().lower() in {"1", "true", "yes", "on"}
    return default if value is None else bool(value)


def normalize_mcp_config(raw: Any) -> dict[str, Any]:
    """Normalize persisted MCP settings without returning a plaintext secret."""
    raw = raw if isinstance(raw, dict) else {}
    permissions_raw = raw.get("permissions")
    permissions_raw = permissions_raw if isinstance(permissions_raw, dict) else {}
    api_key_hash = str(raw.get("api_key_hash") or "")
    # Migrate an early/manual plaintext key once, then callers can persist the
    # normalized result.  The UI never receives this field.
    legacy_key = raw.get("api_key")
    if not api_key_hash and isinstance(legacy_key, str) and legacy_key:
        api_key_hash = hash_api_key(legacy_key)
    return {
        "enabled": _config_bool(raw.get("enabled", False)),
        "api_key_hash": api_key_hash,
        "permissions": {
            name: _config_bool(permissions_raw.get(name, default), default)
            for name, default in DEFAULT_PERMISSIONS.items()
        },
    }


def mcp_config_for_ui(raw: Any) -> dict[str, Any]:
    config = normalize_mcp_config(raw)
    return {
        "enabled": config["enabled"],
        "api_key": "",
        "api_key_configured": bool(config["api_key_hash"]),
        "permissions": config["permissions"],
    }


def merge_mcp_config(current: Any, incoming: Any) -> dict[str, Any]:
    """Merge a UI payload while preserving a configured key on blank input."""
    if not isinstance(incoming, dict):
        raise ValueError("mcp 配置必须是对象")
    current_config = normalize_mcp_config(current)
    permissions = incoming.get("permissions", {})
    if not isinstance(permissions, dict):
        raise ValueError("mcp.permissions 必须是对象")
    unknown = set(permissions) - set(DEFAULT_PERMISSIONS)
    if unknown:
        raise ValueError(f"未知 MCP 权限: {', '.join(sorted(unknown))}")
    if any(not isinstance(value, bool) for value in permissions.values()):
        raise ValueError("MCP 权限值必须是布尔值")
    if "enabled" in incoming and not isinstance(incoming["enabled"], bool):
        raise ValueError("MCP enabled 必须是布尔值")
    api_key = incoming.get("api_key", "")
    if api_key is None:
        api_key = ""
    if not isinstance(api_key, str):
        raise ValueError("MCP API key 必须是字符串")
    api_key_hash = current_config["api_key_hash"]
    if api_key:
        if len(api_key) < 20:
            raise ValueError("MCP API key 至少需要 20 个字符")
        api_key_hash = hash_api_key(api_key)
    enabled = incoming.get("enabled", current_config["enabled"])
    if enabled and not api_key_hash:
        raise ValueError("启用 MCP 前必须设置 API key")
    return {
        "enabled": enabled,
        "api_key_hash": api_key_hash,
        "permissions": {
            name: bool(permissions.get(name, current_config["permissions"].get(name, default)))
            for name, default in DEFAULT_PERMISSIONS.items()
        },
    }


def _schema(properties: dict[str, Any], required: list[str] | None = None) -> dict[str, Any]:
    return {
        "type": "object",
        "properties": properties,
        "required": required or [],
        "additionalProperties": False,
    }


_SELECTOR_PROPERTIES = {
    "task_id": {"type": "string", "description": "任务稳定 ID"},
    "task_name": {"type": "string", "description": "任务名称（精确匹配）"},
    "index": {"type": "integer", "minimum": 0, "description": "兼容旧配置的列表下标"},
}

TASK_FIELDS = {
    "taskname": {"type": "string"},
    "shareurl": {"type": "string"},
    "savepath": {"type": "string"},
    "pattern": {"type": "string"},
    "replace": {"type": "string"},
    "enddate": {"type": "string"},
    "runweek": {"type": "array", "items": {"type": "integer", "minimum": 1, "maximum": 7}},
    "addition": {"type": "object"},
    "ignore_extension": {"type": "boolean"},
    "update_subdir": {"type": "string"},
    "update_subdir_resave_mode": {"type": "boolean"},
    "startfid": {"type": ["string", "integer"]},
}

TOOL_DEFINITIONS = (
    {
        "name": "qas_tasks_list",
        "title": "列出 QAS 任务",
        "description": "列出所有自动转存任务及其稳定 task_id。",
        "scope": "tasks.read",
        "inputSchema": _schema({}),
    },
    {
        "name": "qas_tasks_get",
        "title": "读取 QAS 任务",
        "description": "按 task_id、任务名称或列表下标读取一个任务。",
        "scope": "tasks.read",
        "inputSchema": _schema(_SELECTOR_PROPERTIES),
    },
    {
        "name": "qas_tasks_create",
        "title": "创建 QAS 任务",
        "description": "创建自动转存任务。至少需要 taskname、shareurl、savepath。",
        "scope": "tasks.create",
        "inputSchema": _schema({"task": {"type": "object", "properties": TASK_FIELDS, "additionalProperties": False}}, ["task"]),
    },
    {
        "name": "qas_tasks_update",
        "title": "修改 QAS 任务",
        "description": "按稳定 task_id、任务名称或下标部分更新任务。",
        "scope": "tasks.update",
        "inputSchema": _schema({**_SELECTOR_PROPERTIES, "patch": {"type": "object", "properties": TASK_FIELDS, "additionalProperties": False}}, ["patch"]),
    },
    {
        "name": "qas_tasks_delete",
        "title": "删除 QAS 任务",
        "description": "删除一个自动转存任务。",
        "scope": "tasks.delete",
        "inputSchema": _schema(_SELECTOR_PROPERTIES),
    },
    {
        "name": "qas_tasks_run",
        "title": "运行 QAS 任务",
        "description": "异步运行全部任务或指定任务，并返回 run_id。",
        "scope": "tasks.run",
        "inputSchema": _schema({**_SELECTOR_PROPERTIES, "wait": {"type": "boolean", "default": False}}),
    },
    {
        "name": "qas_run_status",
        "title": "查询运行状态",
        "description": "查询 qas_tasks_run 返回的运行实例状态和日志摘要。",
        "scope": "logs.read",
        "inputSchema": _schema({"run_id": {"type": "string"}}, ["run_id"]),
    },
    {
        "name": "qas_logs_query",
        "title": "查询 QAS 日志",
        "description": "查询内存日志环形缓冲区，支持关键词、级别、run_id、游标和条数。",
        "scope": "logs.read",
        "inputSchema": _schema({
            "query": {"type": "string"},
            "level": {"type": "string", "enum": ["DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"]},
            "run_id": {"type": "string"},
            "cursor": {"type": "integer", "minimum": 0},
            "limit": {"type": "integer", "minimum": 1, "maximum": 200},
        }),
    },
    {
        "name": "qas_search_tv",
        "title": "搜索电视剧或资源",
        "description": "按剧名/任务名调用已配置的 CloudSaver、PanSou 等搜索源。",
        "scope": "search.read",
        "inputSchema": _schema({"name": {"type": "string", "minLength": 1}, "deep": {"type": "boolean", "default": False}}, ["name"]),
    },
    {
        "name": "qas_files_list",
        "title": "列出夸克文件",
        "description": "按保存路径或 fid 查看夸克目录。",
        "scope": "files.read",
        "inputSchema": _schema({"path": {"type": "string"}, "fid": {"type": ["string", "integer"]}}),
    },
    {
        "name": "qas_share_inspect",
        "title": "检查分享内容",
        "description": "读取夸克分享链接的目录与文件详情。",
        "scope": "files.read",
        "inputSchema": _schema({"shareurl": {"type": "string", "minLength": 1}, "stoken": {"type": "string"}}, ["shareurl"]),
    },
    {
        "name": "qas_files_delete",
        "title": "删除夸克文件",
        "description": "按 fid 或路径删除夸克文件/目录。此操作不可逆。",
        "scope": "files.write",
        "inputSchema": _schema({"fid": {"type": ["string", "integer"]}, "path": {"type": "string"}}),
    },
    {
        "name": "qas_files_rename",
        "title": "重命名夸克文件",
        "description": "按 fid 或路径重命名夸克文件/目录。",
        "scope": "files.write",
        "inputSchema": _schema({"fid": {"type": ["string", "integer"]}, "path": {"type": "string"}, "file_name": {"type": "string", "minLength": 1}}, ["file_name"]),
    },
    {
        "name": "qas_config_get",
        "title": "读取脱敏配置",
        "description": "读取不含 Cookie、密码、token 和 API key 的配置摘要。",
        "scope": "config.read",
        "inputSchema": _schema({}),
    },
    {
        "name": "qas_system_status",
        "title": "读取系统状态",
        "description": "读取 QAS 版本、任务数量、调度器和 MCP 状态。",
        "scope": "tasks.read",
        "inputSchema": _schema({}),
    },
)

_TOOL_BY_NAME = {tool["name"]: tool for tool in TOOL_DEFINITIONS}


class MCPProtocolError(Exception):
    def __init__(self, code: int, message: str, data: Any = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.data = data


class MCPTransportError(Exception):
    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status
        self.message = message


def _error_response(request_id: Any, error: MCPProtocolError) -> dict[str, Any]:
    payload: dict[str, Any] = {"code": error.code, "message": error.message}
    if error.data is not None:
        payload["data"] = error.data
    return {"jsonrpc": "2.0", "id": request_id, "error": payload}


def _result_response(request_id: Any, result: Any) -> dict[str, Any]:
    return {"jsonrpc": "2.0", "id": request_id, "result": result}


def _validate_message(message: Any) -> tuple[Any, str, dict[str, Any], bool]:
    if not isinstance(message, dict) or message.get("jsonrpc") != "2.0":
        raise MCPProtocolError(-32600, "Invalid Request")
    request_id = message.get("id")
    is_notification = "id" not in message
    method = message.get("method")
    # A client may send a response to a server-initiated request. QAS never
    # initiates one, so accept it and acknowledge at the transport layer.
    if method is None and ("result" in message or "error" in message):
        return request_id, "", {}, True
    if not isinstance(method, str) or not method:
        raise MCPProtocolError(-32600, "Invalid Request")
    params = message.get("params", {})
    if not isinstance(params, dict):
        raise MCPProtocolError(-32602, "params must be an object")
    return request_id, method, params, is_notification


def _type_matches(value: Any, expected: str) -> bool:
    return {
        "object": isinstance(value, dict),
        "array": isinstance(value, list),
        "string": isinstance(value, str),
        "integer": isinstance(value, int) and not isinstance(value, bool),
        "number": isinstance(value, (int, float)) and not isinstance(value, bool),
        "boolean": isinstance(value, bool),
        "null": value is None,
    }.get(expected, True)


def _validate_schema_value(value: Any, schema: dict[str, Any], path: str) -> None:
    expected = schema.get("type")
    expected_types = expected if isinstance(expected, list) else [expected]
    if expected_types and not any(_type_matches(value, item) for item in expected_types):
        raise MCPProtocolError(-32602, f"{path} has an invalid type")
    if "enum" in schema and value not in schema["enum"]:
        raise MCPProtocolError(-32602, f"{path} has an invalid value")
    if isinstance(value, str):
        if len(value) < schema.get("minLength", 0):
            raise MCPProtocolError(-32602, f"{path} is too short")
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        if "minimum" in schema and value < schema["minimum"]:
            raise MCPProtocolError(-32602, f"{path} is below the minimum")
        if "maximum" in schema and value > schema["maximum"]:
            raise MCPProtocolError(-32602, f"{path} is above the maximum")
    if isinstance(value, list) and schema.get("items"):
        for index, item in enumerate(value):
            _validate_schema_value(item, schema["items"], f"{path}[{index}]")
    if isinstance(value, dict):
        properties = schema.get("properties", {})
        for name in schema.get("required", []):
            if name not in value:
                raise MCPProtocolError(-32602, f"Missing required argument: {path}.{name}")
        if schema.get("additionalProperties") is False:
            unknown = set(value) - set(properties)
            if unknown:
                raise MCPProtocolError(-32602, f"Unknown argument(s): {', '.join(sorted(unknown))}")
        for name, child in properties.items():
            if name in value:
                _validate_schema_value(value[name], child, f"{path}.{name}")


class MCPService:
    """Protocol dispatcher backed by a small application-specific adapter."""

    def __init__(self, backend: Any):
        self.backend = backend
        self._sessions: dict[str, dict[str, Any]] = {}
        self._lock = threading.RLock()

    def enabled(self) -> bool:
        return bool(normalize_mcp_config(self.backend.get_mcp_config())["enabled"])

    def verify_token(self, token: str) -> bool:
        config = normalize_mcp_config(self.backend.get_mcp_config())
        return verify_api_key(token, config["api_key_hash"])

    def _expire_sessions(self) -> None:
        deadline = time.time() - SESSION_TTL_SECONDS
        for session_id, session in list(self._sessions.items()):
            if session["last_seen"] < deadline:
                self._sessions.pop(session_id, None)

    def _new_session(self, version: str) -> str:
        with self._lock:
            self._expire_sessions()
            session_id = secrets.token_urlsafe(32)
            self._sessions[session_id] = {
                "version": version,
                "initialized": False,
                "created_at": time.time(),
                "last_seen": time.time(),
            }
            return session_id

    def validate_session(self, session_id: str | None, protocol_version: str | None = None) -> dict[str, Any]:
        if not session_id:
            raise MCPTransportError(400, "Mcp-Session-Id is required")
        with self._lock:
            self._expire_sessions()
            session = self._sessions.get(session_id)
            if not session:
                raise MCPTransportError(404, "Unknown MCP session")
            if protocol_version and protocol_version != session["version"]:
                raise MCPTransportError(400, "MCP-Protocol-Version does not match the session")
            session["last_seen"] = time.time()
            return session

    def close_session(self, session_id: str | None) -> None:
        if not session_id:
            raise MCPTransportError(400, "Mcp-Session-Id is required")
        with self._lock:
            if session_id not in self._sessions:
                raise MCPTransportError(404, "Unknown MCP session")
            self._sessions.pop(session_id, None)

    def _negotiate_version(self, params: dict[str, Any]) -> str:
        requested = params.get("protocolVersion")
        if not isinstance(requested, str):
            raise MCPProtocolError(-32602, "protocolVersion is required")
        if requested in SUPPORTED_PROTOCOL_VERSIONS:
            return requested
        # MCP version negotiation allows the server to select another version.
        return DEFAULT_PROTOCOL_VERSION

    def create_session(self, version: str = DEFAULT_PROTOCOL_VERSION) -> str:
        if version not in SUPPORTED_PROTOCOL_VERSIONS:
            raise MCPTransportError(400, "Unsupported MCP protocol version")
        return self._new_session(version)

    @staticmethod
    def _initialize_result(request_id: Any, version: str) -> dict[str, Any]:
        result = {
            "protocolVersion": version,
            "capabilities": {"tools": {"listChanged": False}},
            "serverInfo": {"name": MCP_SERVER_NAME, "version": MCP_SERVER_VERSION},
            "instructions": "Use qas_tasks_* for task management, qas_logs_query for runtime logs, and qas_search_tv for resource search. Destructive tools are permission-gated.",
        }
        return _result_response(request_id, result)

    def _initialize(self, request_id: Any, params: dict[str, Any]) -> tuple[dict[str, Any], str, str]:
        version = self._negotiate_version(params)
        session_id = self._new_session(version)
        return self._initialize_result(request_id, version), session_id, version

    def _permissions(self) -> dict[str, bool]:
        return normalize_mcp_config(self.backend.get_mcp_config())["permissions"]

    def _tool_list(self) -> list[dict[str, Any]]:
        permissions = self._permissions()
        tools = []
        for definition in TOOL_DEFINITIONS:
            if not permissions.get(definition["scope"], False):
                continue
            item = {key: value for key, value in definition.items() if key != "scope"}
            item["annotations"] = {
                "readOnlyHint": definition["scope"].endswith(".read") or definition["scope"] == "tasks.read",
                "destructiveHint": definition["scope"] in {"tasks.delete", "files.write", "tasks.run"},
                "openWorldHint": definition["scope"] == "search.read",
            }
            tools.append(item)
        return tools

    @staticmethod
    def _validate_tool_args(definition: dict[str, Any], args: dict[str, Any]) -> None:
        _validate_schema_value(args, definition["inputSchema"], "arguments")

    def _call_tool(self, request_id: Any, params: dict[str, Any]) -> dict[str, Any]:
        name = params.get("name")
        args = params.get("arguments", {})
        if not isinstance(name, str) or not name:
            raise MCPProtocolError(-32602, "Tool name is required")
        if not isinstance(args, dict):
            raise MCPProtocolError(-32602, "arguments must be an object")
        definition = _TOOL_BY_NAME.get(name)
        if not definition or not self._permissions().get(definition["scope"], False):
            raise MCPProtocolError(-32601, f"Unknown tool: {name}")
        self._validate_tool_args(definition, args)
        logging.info("MCP tool call: %s", name)
        try:
            data = self.backend.call_tool(name, args)
        except MCPProtocolError:
            raise
        except PermissionError as exc:
            raise MCPProtocolError(-32003, str(exc) or "Permission denied")
        except ValueError as exc:
            raise MCPProtocolError(-32602, str(exc) or "Invalid tool arguments")
        except Exception:  # never leak a traceback or credential-bearing error
            return _result_response(request_id, {
                "content": [{"type": "text", "text": "工具执行失败"}],
                "isError": True,
                "structuredContent": {"success": False, "message": "工具执行失败"},
            })
        structured = data if isinstance(data, dict) else {"items": data}
        is_error = isinstance(structured, dict) and structured.get("success") is False
        return _result_response(request_id, {
            "content": [{"type": "text", "text": json.dumps(structured, ensure_ascii=False)}],
            "isError": is_error,
            "structuredContent": structured,
        })

    def dispatch(self, message: Any, *, session_id: str | None = None, transport: str = "http", protocol_version: str | None = None, allow_existing_initialize: bool = False) -> tuple[dict[str, Any] | None, str | None, str | None]:
        """Dispatch one JSON-RPC message.

        Returns (response, session_id, negotiated_version). Notifications have
        a None response and are handled by the transport with HTTP 202/stdout
        silence as required by MCP.
        """
        is_notification = False
        try:
            request_id, method, params, is_notification = _validate_message(message)
            if method == "initialize":
                if transport == "http" and session_id:
                    if not allow_existing_initialize:
                        raise MCPTransportError(400, "initialize must not include a session")
                    session = self.validate_session(session_id, protocol_version)
                    version = self._negotiate_version(params)
                    if version != session["version"]:
                        raise MCPTransportError(400, "initialize protocol version does not match the session")
                    if is_notification:
                        return None, session_id, version
                    return self._initialize_result(request_id, version), session_id, version
                if is_notification:
                    return None, session_id, protocol_version
                response, created_session, version = self._initialize(request_id, params)
                return response, created_session, version
            if not method:
                if transport == "http":
                    self.validate_session(session_id, protocol_version)
                return None, session_id, protocol_version
            if transport == "http":
                session = self.validate_session(session_id, protocol_version)
                if protocol_version and protocol_version not in SUPPORTED_PROTOCOL_VERSIONS:
                    raise MCPTransportError(400, "Unsupported MCP-Protocol-Version")
            elif protocol_version and protocol_version not in SUPPORTED_PROTOCOL_VERSIONS:
                raise MCPTransportError(400, "Unsupported MCP protocol version")
            if method in {"notifications/initialized", "notifications/cancelled"}:
                if transport == "http" and session_id:
                    with self._lock:
                        self._sessions[session_id]["initialized"] = True
                return None, session_id, protocol_version
            if method == "ping":
                response = _result_response(request_id, {})
                return (None if is_notification else response), session_id, protocol_version
            if method == "tools/list":
                response = _result_response(request_id, {"tools": self._tool_list()})
                return (None if is_notification else response), session_id, protocol_version
            if method == "tools/call":
                response = self._call_tool(request_id, params)
                return (None if is_notification else response), session_id, protocol_version
            if is_notification:
                return None, session_id, protocol_version
            raise MCPProtocolError(-32601, f"Method not found: {method}")
        except MCPTransportError:
            raise
        except MCPProtocolError as exc:
            if is_notification:
                return None, session_id, protocol_version
            request_id = message.get("id") if isinstance(message, dict) else None
            return _error_response(request_id, exc), session_id, protocol_version


def run_stdio(service: MCPService, token: str) -> int:
    """Run newline-delimited JSON-RPC stdio transport."""
    import sys

    if not service.enabled() or not service.verify_token(token):
        print("MCP stdio disabled or invalid QAS_MCP_API_KEY", file=sys.stderr)
        return 1
    for raw_line in sys.stdin:
        line = raw_line.strip()
        if not line:
            continue
        try:
            message = json.loads(line)
            response, _, _ = service.dispatch(message, transport="stdio")
            if response is not None:
                sys.stdout.write(json.dumps(response, ensure_ascii=False, separators=(",", ":")) + "\n")
                sys.stdout.flush()
        except MCPTransportError as exc:
            response = _error_response(None, MCPProtocolError(-32000, exc.message))
            sys.stdout.write(json.dumps(response, ensure_ascii=False, separators=(",", ":")) + "\n")
            sys.stdout.flush()
        except Exception:
            response = _error_response(None, MCPProtocolError(-32700, "Parse error"))
            sys.stdout.write(json.dumps(response, ensure_ascii=False, separators=(",", ":")) + "\n")
            sys.stdout.flush()
    return 0
