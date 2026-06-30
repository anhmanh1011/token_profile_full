"""Tests for the MANAGE_USER_LOG_FILE env override in app._resolve_log_file.

The function is pure (reads os.environ at call time), so we can assert its
return value directly with monkeypatched env. Importing app.py runs a
module-level logging.basicConfig() that opens a FileHandler at import time;
we point that at a throwaway path *before* importing so the test never writes
a stray service.log into the source tree.
"""
import os
from pathlib import Path

# Must be set BEFORE `import app` so the import-time FileHandler is harmless.
_IMPORT_LOG = Path(__file__).resolve().parent / "_import_service.log"
os.environ["MANAGE_USER_LOG_FILE"] = str(_IMPORT_LOG)

import app  # noqa: E402  (env must be configured before import)


def test_resolve_uses_env_override(monkeypatch):
    monkeypatch.setenv("MANAGE_USER_LOG_FILE", "/var/lib/token-tool/t1/service.log")
    assert app._resolve_log_file() == Path("/var/lib/token-tool/t1/service.log")


def test_resolve_falls_back_when_unset(monkeypatch):
    monkeypatch.delenv("MANAGE_USER_LOG_FILE", raising=False)
    expected = Path(app.__file__).parent / "service.log"
    assert app._resolve_log_file() == expected


def test_resolve_falls_back_when_blank(monkeypatch):
    monkeypatch.setenv("MANAGE_USER_LOG_FILE", "   ")
    expected = Path(app.__file__).parent / "service.log"
    assert app._resolve_log_file() == expected


def test_resolve_strips_surrounding_whitespace(monkeypatch):
    monkeypatch.setenv("MANAGE_USER_LOG_FILE", "  /var/lib/token-tool/t2/service.log  ")
    assert app._resolve_log_file() == Path("/var/lib/token-tool/t2/service.log")
