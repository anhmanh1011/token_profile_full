"""save_if_updated() must persist the rotated refresh_token to the per-tenant
--config file, not the module-level admin_token.json."""
import json
from pathlib import Path

import admin_token_manager as atm
from admin_token_manager import AdminTokenManager


def _write_cfg(path: Path, token: str) -> None:
    path.write_text(json.dumps([{
        "tenant_id": "tid", "refresh_token": token,
        "username": "admin@t1.example", "domain": "t1.example",
    }]), encoding="utf-8")


def test_save_writes_to_config_path(tmp_path):
    cfg = tmp_path / "admin_token.t1.json"
    _write_cfg(cfg, "OLD")
    mgr = AdminTokenManager(json.loads(cfg.read_text())[0], config_path=cfg)
    # Simulate Microsoft rotating the refresh_token.
    mgr.refresh_token = "NEW"
    mgr._refresh_token_updated = True
    mgr.save_if_updated()
    assert json.loads(cfg.read_text())[0]["refresh_token"] == "NEW"
    # The module-level default file is never touched.
    assert atm.CONFIG_FILE != cfg


def test_config_path_defaults_to_module_file(tmp_path):
    mgr = AdminTokenManager({"tenant_id": "t", "refresh_token": "r"})
    assert mgr.config_path == atm.CONFIG_FILE
