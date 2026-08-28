#!/usr/bin/env python3
"""Dependency-free MCP protocol and permission regression checks."""
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from app.mcp import (
    DEFAULT_PERMISSIONS,
    MCPService,
    merge_mcp_config,
    hash_api_key,
    verify_api_key,
)


class FakeBackend:
    def __init__(self):
        self.config = {
            "enabled": True,
            "api_key_hash": hash_api_key("test-mcp-key-1234567890"),
            "permissions": dict(DEFAULT_PERMISSIONS),
        }
        self.config["permissions"]["tasks.create"] = True
        self.config["permissions"]["tasks.update"] = False
        self.called = []

    def get_mcp_config(self):
        return self.config

    def call_tool(self, name, args):
        self.called.append((name, args))
        if name == "qas_tasks_list":
            return {"success": True, "tasks": [], "count": 0}
        if name == "qas_tasks_create":
            return {"success": True, "task": args["task"]}
        raise AssertionError(name)


def test_protocol_and_permissions():
    backend = FakeBackend()
    service = MCPService(backend)
    assert service.enabled()
    assert service.verify_token("test-mcp-key-1234567890")
    assert not service.verify_token("wrong-key")

    initialized, session_id, version = service.dispatch(
        {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": "2025-06-18",
                "capabilities": {},
                "clientInfo": {"name": "test", "version": "1"},
            },
        },
        transport="http",
    )
    assert initialized["result"]["protocolVersion"] == "2025-06-18"
    assert session_id and version == "2025-06-18"

    listed, _, _ = service.dispatch(
        {"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}},
        session_id=session_id,
        transport="http",
        protocol_version=version,
    )
    names = {tool["name"] for tool in listed["result"]["tools"]}
    assert "qas_tasks_list" in names
    assert "qas_tasks_create" in names
    assert "qas_tasks_update" not in names
    assert "qas_tasks_delete" not in names
    assert "qas_tasks_run" not in names

    result, _, _ = service.dispatch(
        {
            "jsonrpc": "2.0",
            "id": 3,
            "method": "tools/call",
            "params": {"name": "qas_tasks_list", "arguments": {}},
        },
        session_id=session_id,
        transport="http",
        protocol_version=version,
    )
    assert result["result"]["isError"] is False
    assert result["result"]["structuredContent"]["count"] == 0

    notification, _, _ = service.dispatch(
        {"jsonrpc": "2.0", "method": "tools/list", "params": {}},
        session_id=session_id,
        transport="http",
        protocol_version=version,
    )
    assert notification is None
    response_message, _, _ = service.dispatch(
        {"jsonrpc": "2.0", "id": 6, "result": {}},
        session_id=session_id,
        transport="http",
        protocol_version=version,
    )
    assert response_message is None

    invalid_type, _, _ = service.dispatch(
        {
            "jsonrpc": "2.0",
            "id": 7,
            "method": "tools/call",
            "params": {"name": "qas_logs_query", "arguments": {"limit": "10"}},
        },
        session_id=session_id,
        transport="http",
        protocol_version=version,
    )
    assert invalid_type["error"]["code"] == -32602

    denied, _, _ = service.dispatch(
        {
            "jsonrpc": "2.0",
            "id": 4,
            "method": "tools/call",
            "params": {"name": "qas_tasks_update", "arguments": {}},
        },
        session_id=session_id,
        transport="http",
        protocol_version=version,
    )
    assert denied["error"]["code"] == -32601

    invalid, _, _ = service.dispatch(
        {
            "jsonrpc": "2.0",
            "id": 5,
            "method": "tools/call",
            "params": {"name": "qas_tasks_create", "arguments": {"unexpected": True}},
        },
        session_id=session_id,
        transport="http",
        protocol_version=version,
    )
    assert invalid["error"]["code"] == -32602


def test_key_merge_and_validation():
    old_key = "existing-mcp-key-1234567890"
    current = {"enabled": False, "api_key_hash": hash_api_key(old_key), "permissions": {}}
    merged = merge_mcp_config(current, {"enabled": True, "api_key": "", "permissions": {}})
    assert merged["enabled"] is True
    assert merged["api_key_hash"] == hash_api_key(old_key)

    try:
        merge_mcp_config({}, {"enabled": True, "api_key": "", "permissions": {}})
    except ValueError as exc:
        assert "API key" in str(exc)
    else:
        raise AssertionError("enabling without a key must fail")

    try:
        merge_mcp_config(current, {"enabled": True, "api_key": "", "permissions": {"tasks.read": "false"}})
    except ValueError as exc:
        assert "布尔值" in str(exc)
    else:
        raise AssertionError("string permission values must fail closed")

    new_key = "rotated-mcp-key-1234567890"
    rotated = merge_mcp_config(current, {"enabled": True, "api_key": new_key, "permissions": {}})
    assert verify_api_key(new_key, rotated["api_key_hash"])
    assert not verify_api_key(old_key, rotated["api_key_hash"])


def demo():
    test_protocol_and_permissions()
    test_key_merge_and_validation()
    print("ok mcp protocol")


if __name__ == "__main__":
    demo()
