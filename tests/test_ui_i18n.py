#!/usr/bin/env python3
"""zh/en key parity and template key coverage for WebUI i18n."""
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parents[1]


def block_keys(js: str, name: str) -> set[str]:
    m = re.search(rf"\n  {name}: \{{(.*?)\n  \}}", js, re.S)
    assert m, f"missing i18n block {name}"
    return set(re.findall(r"^\s{4}([A-Za-z0-9_]+):", m.group(1), re.M))


def used_keys(text: str) -> set[str]:
    keys = set(re.findall(r"\bt\('([A-Za-z0-9_]+)'", text))
    keys |= set(re.findall(r"QAS_t\([^,]+,\s*'([A-Za-z0-9_]+)'", text))
    return keys


def main() -> int:
    js = (ROOT / "app/static/js/i18n.js").read_text(encoding="utf-8")
    index = (ROOT / "app/templates/index.html").read_text(encoding="utf-8")
    login = (ROOT / "app/templates/login.html").read_text(encoding="utf-8")
    zh, en = block_keys(js, "zh"), block_keys(js, "en")
    assert zh == en, f"key mismatch en-zh={sorted(en-zh)} zh-en={sorted(zh-en)}"
    used = used_keys(index) | used_keys(login)
    missing = sorted(used - zh)
    assert not missing, f"missing translations: {missing}"
    for path in (ROOT / "app/static/css/theme.css", ROOT / "app/static/js/i18n.js"):
        assert path.exists() and path.stat().st_size > 100
    themes = ["light", "dark", "ocean", "sunset"]
    css = (ROOT / "app/static/css/theme.css").read_text(encoding="utf-8")
    for theme in themes:
        assert f'data-theme="{theme}"' in css or f"data-theme=\"{theme}\"" in css or f'[data-theme="{theme}"]' in css
    print(f"ok keys={len(zh)} used={len(used)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
