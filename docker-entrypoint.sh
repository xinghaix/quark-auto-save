#!/bin/sh
set -eu

if [ "$(id -u)" -eq 0 ]; then
    mkdir -p /app/config /media
    chown -R qas:qas /app/config
    # ponytail: chown the mountpoint only; recursive /media can take minutes on a library.
    chown qas:qas /media
    exec su-exec qas:qas /usr/local/bin/quark-auto-save "$@"
fi

exec /usr/local/bin/quark-auto-save "$@"
