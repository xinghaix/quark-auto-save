# 插件

插件已编译进 Go 二进制，不再加载 `plugins/*.py`。

可用 `PLUGIN_FLAGS` 禁用，例如 `-emby,-aria2`。

内置：`emby`、`plex`、`fnv`、`aria2`、`auto_unarchive`、`alist`、`alist_sync`、`alist_strm`、`alist_strm_gen`、`smartstrm`。配置字段仍写在 `quark_config.json` 的 `plugins` 与任务 `addition` 中。
