#!/usr/bin/env python3
"""Apply the legacy MagicRename preview to already fetched share data."""
from __future__ import annotations

import json
import re
import sys
from pathlib import Path

# The helper is shipped below /app/app/runtime while the compatibility module
# lives at /app (and at the repository root during local development).
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from quark_auto_save import MagicRename  # noqa: E402


def main() -> int:
    payload = json.load(sys.stdin)
    data = payload.get("data") or {}
    task = payload.get("task") or {}
    magic_regex = payload.get("magic_regex") or {}
    existing = payload.get("existing") or []

    matcher = MagicRename(magic_regex)
    matcher.set_taskname(task.get("taskname", ""))
    existing_files = [item for item in existing if isinstance(item, dict)]
    existing_names = [item.get("file_name", "") for item in existing_files]
    pattern, replace = matcher.magic_regex_conv(
        task.get("pattern", ""), task.get("replace", "")
    )

    for share_file in data.get("list", []):
        if not isinstance(share_file, dict):
            continue
        name = share_file.get("file_name", "")
        search_pattern = (
            task.get("update_subdir", "")
            if share_file.get("dir") and task.get("update_subdir")
            else pattern
        )
        if re.search(search_pattern, name):
            renamed = name if share_file.get("dir") else matcher.sub(pattern, replace, name)
            saved = matcher.is_exists(
                renamed,
                existing_names,
                bool(task.get("ignore_extension")) and not share_file.get("dir"),
            )
            if saved:
                share_file["file_name_saved"] = saved
            else:
                share_file["file_name_re"] = renamed

    if re.search(r"\{I+\}", replace):
        matcher.set_dir_file_list(existing_files, replace)
        matcher.sort_file_list(data.get("list", []))

    json.dump(data, sys.stdout, ensure_ascii=False, separators=(",", ":"))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
