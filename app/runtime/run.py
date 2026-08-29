#!/usr/bin/env python3
"""Compatibility shim: preserve the historical command while running Go."""
import os
import sys

binary = os.environ.get("QAS_GO_BINARY", "/usr/local/bin/quark-auto-save")
os.execv(binary, [binary, *sys.argv[1:]])
