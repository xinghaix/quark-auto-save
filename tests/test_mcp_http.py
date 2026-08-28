#!/usr/bin/env python3
"""Flask integration checks for the authenticated /mcp endpoint."""
from pathlib import Path
import json
import os
import sys
import tempfile
from urllib.parse import parse_qs, urlparse

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "app"))


KEY = "test-mcp-http-key-1234567890"


def rpc(method, request_id=1, params=None):
    return {
        "jsonrpc": "2.0",
        "id": request_id,
        "method": method,
        "params": params or {},
    }


def demo():
    with tempfile.TemporaryDirectory() as directory:
        config_path = Path(directory) / "quark_config.json"
        config_path.write_text(json.dumps({
            "cookie": ["test-cookie"],
            "tasklist": [],
            "push_config": {
                "PUSH_KEY": "push-secret-value-123456",
                "BARK_PUSH": "bark-secret-value-123456",
                "apiKey": "camel-secret-value-123456",
                "WEBHOOK_URL": "https://hooks.example/secret-value-123456",
            },
            "source": {},
            "mcp": {
                "enabled": True,
                "api_key": KEY,
                "permissions": {
                    "tasks.read": True,
                    "logs.read": True,
                    "search.read": True,
                    "files.read": True,
                    "config.read": True,
                },
            },
        }), encoding="utf-8")
        os.environ["CONFIG_PATH"] = str(config_path)
        fake_script = Path(directory) / "fake_run.py"
        fake_script.write_text(
            "print('mcp-run-ok PUSH_KEY=child-secret-value-123456')\n"
            "print('{\"apiKey\": \"camel-child-secret-123456\", \"BARK_PUSH\": \"bark-child-secret-123456\"}')\n"
            "import time; time.sleep(0.5)\n",
            encoding="utf-8",
        )
        os.environ["SCRIPT_PATH"] = str(fake_script)
        os.environ["MCP_ALLOWED_ORIGINS"] = "https://client.example"
        from app import run as qas_run

        qas_run.init()
        client = qas_run.app.test_client()
        auth = {"Authorization": f"Bearer {KEY}", "Accept": "application/json, text/event-stream"}

        with client.session_transaction() as session:
            session["token"] = qas_run.get_login_token()
        data_response = client.get("/data")
        assert data_response.status_code == 200
        mcp_data = data_response.json["data"]["mcp"]
        assert mcp_data["api_key"] == ""
        assert mcp_data["api_key_configured"] is True
        assert "api_key_hash" not in mcp_data

        qas_run.config_data["mcp"]["enabled"] = False
        qas_run.config_data["mcp"]["api_key_hash"] = ""
        missing_key = client.post("/update", json={"mcp": {"enabled": True, "api_key": "", "permissions": {}}})
        assert missing_key.status_code == 400
        restored = client.post("/update", json={"mcp": {"enabled": True, "api_key": KEY, "permissions": {"tasks.read": True}}})
        assert restored.status_code == 200
        persisted = json.loads(config_path.read_text(encoding="utf-8"))["mcp"]
        assert persisted["api_key_hash"] != KEY
        assert persisted["api_key_hash"]

        preflight = client.options("/mcp", headers={"Origin": "https://client.example"})
        assert preflight.status_code == 204
        assert preflight.headers["Access-Control-Allow-Origin"] == "https://client.example"

        denied = client.post("/mcp", json=rpc("initialize"), headers={"Accept": "application/json"})
        assert denied.status_code == 401

        initialized = client.post(
            "/mcp",
            json=rpc("initialize", params={
                "protocolVersion": "2025-06-18",
                "capabilities": {},
                "clientInfo": {"name": "integration", "version": "1"},
            }),
            headers=auth,
        )
        assert initialized.status_code == 200, initialized.data
        session_id = initialized.headers["Mcp-Session-Id"]
        assert initialized.json["result"]["protocolVersion"] == "2025-06-18"

        session_headers = {**auth, "Mcp-Session-Id": session_id, "MCP-Protocol-Version": "2025-06-18"}
        bad_origin = client.post("/mcp", json=rpc("ping", 99), headers={**session_headers, "Origin": "https://evil.example"})
        assert bad_origin.status_code == 403
        notification = client.post(
            "/mcp",
            json={"jsonrpc": "2.0", "method": "notifications/initialized"},
            headers={**session_headers, "Origin": "https://client.example"},
        )
        assert notification.status_code == 202
        assert notification.headers["Access-Control-Allow-Origin"] == "https://client.example"

        listed = client.post("/mcp", json=rpc("tools/list", 2), headers=session_headers)
        assert listed.status_code == 200
        names = {tool["name"] for tool in listed.json["result"]["tools"]}
        assert "qas_tasks_list" in names
        assert "qas_tasks_delete" not in names
        assert "qas_config_get" in names

        redacted = client.post("/mcp", json=rpc("tools/call", 3, {
            "name": "qas_config_get",
            "arguments": {},
        }), headers=session_headers)
        assert redacted.status_code == 200
        redacted_text = redacted.get_data(as_text=True)
        assert KEY not in redacted_text
        assert "test-cookie" not in redacted_text
        for secret in (
            "push-secret-value-123456",
            "bark-secret-value-123456",
            "camel-secret-value-123456",
            "secret-value-123456",
        ):
            assert secret not in redacted_text

        qas_run.config_data["tasklist"] = [{
            "id": "stable-id",
            "taskname": "Secret Task",
            "shareurl": "https://pan.quark.cn/s/example",
            "savepath": "/Secret Task",
            "addition": {"token": "plugin-secret"},
        }]
        captured = {}
        original_start = qas_run.start_mcp_run
        def fake_start(tasklist):
            captured["tasklist"] = tasklist
            return {"success": True, "run_id": "fake", "status": "running"}
        qas_run.start_mcp_run = fake_start
        try:
            qas_run.mcp_backend.run_tasks({})
        finally:
            qas_run.start_mcp_run = original_start
        assert captured["tasklist"][0]["addition"]["token"] == "plugin-secret"

        called = client.post("/mcp", json=rpc("tools/call", 4, {
            "name": "qas_tasks_list",
            "arguments": {},
        }), headers=session_headers)
        assert called.status_code == 200
        assert called.json["result"]["structuredContent"]["count"] == 1

        qas_run.config_data["mcp"]["permissions"]["tasks.run"] = True
        first_run = qas_run.mcp_backend.run_tasks({})
        try:
            qas_run.mcp_backend.run_tasks({})
        except ValueError as exc:
            assert "运行" in str(exc)
        else:
            raise AssertionError("the same task must not run concurrently")
        assert qas_run.wait_mcp_run(first_run["run_id"])["status"] == "completed"

        run_call = client.post("/mcp", json=rpc("tools/call", 5, {
            "name": "qas_tasks_run",
            "arguments": {"wait": True},
        }), headers=session_headers)
        assert run_call.status_code == 200
        run_result = run_call.json["result"]["structuredContent"]
        assert run_result["status"] == "completed"
        assert any("mcp-run-ok" in line for line in run_result["log_tail"])
        run_log_text = "\n".join(run_result["log_tail"])
        for secret in (
            "child-secret-value-123456",
            "camel-child-secret-123456",
            "bark-child-secret-123456",
        ):
            assert secret not in run_log_text
        assert "[REDACTED]" in run_log_text

        with qas_run.MCP_RUNS_LOCK:
            saved_runs = dict(qas_run.MCP_RUNS)
            qas_run.MCP_RUNS.clear()
            qas_run.MCP_RUNS.update({f"active-{i}": {"status": "running"} for i in range(51)})
            qas_run._trim_mcp_runs_locked()
            assert len(qas_run.MCP_RUNS) == 51
            assert all(item["status"] == "running" for item in qas_run.MCP_RUNS.values())
            qas_run.MCP_RUNS.clear()
            qas_run.MCP_RUNS.update(saved_runs)

        sse = client.post(
            "/mcp",
            json=rpc("ping", 6),
            headers={**session_headers, "Accept": "text/event-stream"},
        )
        assert sse.status_code == 200
        assert sse.content_type.startswith("text/event-stream")
        assert b"event: message\ndata:" in sse.data

        get_stream = client.get("/mcp", headers={**session_headers, "Accept": "text/event-stream"})
        assert get_stream.status_code == 200
        assert get_stream.data == b": qas-mcp\n\n"

        legacy_stream = client.get("/mcp/sse", headers={**auth, "Accept": "text/event-stream"}, buffered=False)
        assert legacy_stream.status_code == 200
        endpoint_event = next(legacy_stream.response).decode("utf-8")
        assert endpoint_event.startswith("event: endpoint\ndata: ")
        endpoint = endpoint_event.split("\ndata: ", 1)[1].strip()
        legacy_session = parse_qs(urlparse(endpoint).query)["sessionId"][0]
        legacy_init = client.post(
            f"/mcp/messages?sessionId={legacy_session}",
            json=rpc("initialize", 10, {
                "protocolVersion": "2024-11-05",
                "capabilities": {},
                "clientInfo": {"name": "legacy", "version": "1"},
            }),
            headers=auth,
        )
        assert legacy_init.status_code == 202
        legacy_message = next(legacy_stream.response).decode("utf-8")
        assert legacy_message.startswith("event: message\ndata: ")
        legacy_stream.close()

        closed = client.delete("/mcp", headers=session_headers)
        assert closed.status_code == 204
        missing_session = client.post("/mcp", json=rpc("tools/list", 5), headers=auth)
        assert missing_session.status_code == 400

        qas_run.config_data["mcp"]["enabled"] = False
        disabled = client.post("/mcp", json=rpc("initialize"), headers=auth)
        assert disabled.status_code == 404
        if qas_run.scheduler.running:
            qas_run.scheduler.shutdown(wait=False)
    print("ok mcp http")


if __name__ == "__main__":
    demo()
