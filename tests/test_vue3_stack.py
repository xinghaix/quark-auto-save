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
