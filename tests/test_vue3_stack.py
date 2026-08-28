#!/usr/bin/env python3
"""Assert WebUI is Vue 3 + Bootstrap 5, no jQuery/Vue2."""
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parents[1]
STATIC = ROOT / "app/static"
INDEX = (ROOT / "app/templates/index.html").read_text(encoding="utf-8")
LOGIN = (ROOT / "app/templates/login.html").read_text(encoding="utf-8")
HTML = INDEX + LOGIN


def main() -> int:
    assert "vue@2" not in HTML
    assert "jquery" not in HTML.lower()
    assert "Vue.createApp" in INDEX
    assert "new Vue(" not in INDEX
    assert "this.$set" not in INDEX
    assert "this.$delete" not in INDEX
    assert "Vue.set" not in INDEX
    assert "filters:" not in INDEX
    assert "beforeDestroy" not in INDEX
    assert "data-toggle=" not in HTML
    assert "data-target=" not in HTML
    assert "data-dismiss=" not in HTML
    assert '<link rel="stylesheet" href="./static/css/jsoneditor.min.css">' not in INDEX
    assert '<script src="./static/js/jsoneditor.min.js"></script>' not in INDEX
    assert "qasLoadJsonEditor" in INDEX
    assert "await qasLoadJsonEditor()" in INDEX
    assert "editorReadyTasks" in INDEX
    assert "jsonEditorLoadFailed" in INDEX
    assert "retry" in INDEX
    assert "editorError" in INDEX
    assert '@click="prepareEditor(task)"' in INDEX
    assert '<v-jsoneditor v-if="editorReadyTasks.includes(task)"' in INDEX
    assert "axios.post('/update', this.formData)" in INDEX
    assert "editorReady: true" not in INDEX
    assert 'class="qas-mobile-nav"' in INDEX
    assert 'd-lg-none' in INDEX
    assert 'window.innerWidth < 992' in INDEX
    assert 'getOrCreateInstance(sidebar).hide()' in INDEX
    assert 'qas-task-header' in INDEX
    assert 'class="btn w-100 text-start qas-task-toggle"' in INDEX
    assert ':aria-controls="' in INDEX
    assert not re.search(r'<div[^>]*data-bs-toggle="collapse"', INDEX)
    assert 'class="collapse qas-task-body' in INDEX
    assert 'class="qas-log-console"' in INDEX
    assert 'qas-task-savebar' in INDEX
    assert 'qas-config-savebar' in INDEX
    assert 'qas-global-save' in INDEX
    assert 'qas-file-action' in INDEX
    assert 'qas-mcp-card' in INDEX
    assert "formData.mcp.api_key" in INDEX
    assert "mcpPermissionOptions" in INDEX
    assert "mcpEndpoint" in INDEX
    assert "generateMcpKey" in INDEX
    assert "mcpShowKey" in INDEX
    assert 'openLog()' in INDEX
    assert 'v-html="versionTips"' not in INDEX
    assert 'latestVersion' in INDEX
    assert 'version: [[ version|tojson ]]' in INDEX
    assert 'plugin_flags: [[ plugin_flags|tojson ]]' in INDEX
    assert 'async copyText' in INDEX
    assert 'await navigator.clipboard.writeText(text)' in INDEX
    assert r'partialData.split(/\r?\n/)' in INDEX
    assert '#logModal .modal-body' in INDEX
    assert 'text/event-stream' in INDEX
    assert 'class="login-shell"' in LOGIN
    dashboard_css = (STATIC / "css/dashboard.css").read_text(encoding="utf-8")
    assert ".qas-config-view" in dashboard_css
    assert ".qas-task-body.collapse.show" in dashboard_css
    assert "@media (max-width: 767px)" in dashboard_css
    assert ".qas-task-actions .btn { width: 44px; min-height: 44px; }" in dashboard_css
    assert ".qas-jsoneditor { min-width: 0; overflow: visible;" in dashboard_css
    assert ".qas-mcp-permissions" in dashboard_css
    assert "var(--qas-surface-sub)" in dashboard_css
    assert "grid-template-columns" in dashboard_css
    assert "--qas-canvas-bg" in (STATIC / "css/theme.css").read_text(encoding="utf-8")
    assert "input-group-prepend" not in HTML
    assert "input-group-append" not in HTML
    assert "| size" not in INDEX
    assert "| ts2date" not in INDEX

    css = (STATIC / "css/bootstrap.min.css").read_text(encoding="utf-8", errors="replace")[:400]
    assert "Bootstrap" in css and "v5." in css, css[:120]

    vue = (STATIC / "js/vue.global.prod.js").read_text(encoding="utf-8", errors="replace")[:800]
    assert re.search(r"Vue|createApp", vue)

    assert not (STATIC / "js/jquery-3.5.1.slim.min.js").exists()
    assert not (STATIC / "js/vue@2.js").exists()
    assert not (STATIC / "js/v-jsoneditor.min.js").exists()
    assert (STATIC / "js/jsoneditor.min.js").exists()
    assert (STATIC / "css/jsoneditor.min.css").exists()
    assert (STATIC / "js/axios.min.js").exists()
    assert (STATIC / "js/bootstrap.bundle.min.js").exists()

    bundle = (STATIC / "js/bootstrap.bundle.min.js").read_text(encoding="utf-8", errors="replace")[:300]
    assert "Bootstrap" in bundle and "v5." in bundle, bundle[:120]

    icons = (STATIC / "css/bootstrap-icons.min.css").read_text(encoding="utf-8", errors="replace")[:250]
    assert "Bootstrap Icons v1.13" in icons or "bootstrap-icons" in icons.lower()

    print("ok vue3 stack")
    return 0


if __name__ == "__main__":
    sys.exit(main())
