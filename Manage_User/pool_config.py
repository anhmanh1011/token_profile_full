"""Global pool configuration for Manage_User.

The service reads only ``admin_token_config_global.json``. Each enabled entry
becomes one independent token pool with its own admin token manager, proxy,
bot prefix, queue, producer, and delete stats.
"""
from __future__ import annotations

import json
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any

POOL_ID_PATTERN = re.compile(r"^[A-Za-z0-9_.-]+$")


@dataclass(frozen=True)
class PoolConfig:
    pool_id: str
    refresh_token: str
    tenant_id: str
    username: str
    domain: str
    proxy: str | None
    email_file: str
    bot_prefix: str
    enabled: bool = True

    def as_admin_dict(self) -> dict[str, Any]:
        return {
            "refresh_token": self.refresh_token,
            "tenant_id": self.tenant_id,
            "username": self.username,
            "domain": self.domain,
            "proxy": self.proxy,
            "bot_prefix": self.bot_prefix,
            "pool_id": self.pool_id,
        }


def load_pool_configs(config_file: Path) -> list[PoolConfig]:
    with open(config_file, "r", encoding="utf-8") as f:
        entries = json.load(f)

    if not isinstance(entries, list) or not entries:
        raise ValueError(f"{config_file} must be a non-empty JSON array")

    pools: list[PoolConfig] = []
    seen_pool_ids: set[str] = set()
    seen_bot_prefixes: set[str] = set()

    for idx, entry in enumerate(entries):
        if not isinstance(entry, dict):
            raise ValueError(f"config entry #{idx} must be an object")
        if entry.get("enabled") is False:
            continue

        pool = _parse_pool_config(entry, idx)

        if pool.pool_id in seen_pool_ids:
            raise ValueError(f"duplicate pool_id: {pool.pool_id}")
        if pool.bot_prefix in seen_bot_prefixes:
            raise ValueError(f"duplicate bot_prefix: {pool.bot_prefix}")

        seen_pool_ids.add(pool.pool_id)
        seen_bot_prefixes.add(pool.bot_prefix)
        pools.append(pool)

    if not pools:
        raise ValueError(f"{config_file} has no enabled pools")

    # Cleanup matches users by ``startswith(bot_prefix)``. Distinct strings are
    # not enough: if one prefix is a prefix of another (e.g. "bot_p0" vs
    # "bot_p01_"), the shorter pool's cleanup would delete the longer pool's
    # users. Reject any such overlap so pools stay isolated.
    for outer in pools:
        for inner in pools:
            if outer is inner:
                continue
            if inner.bot_prefix.startswith(outer.bot_prefix):
                raise ValueError(
                    f"bot_prefix {outer.bot_prefix!r} (pool {outer.pool_id}) is a prefix of "
                    f"{inner.bot_prefix!r} (pool {inner.pool_id}); prefixes must not overlap"
                )

    return pools


def resolve_pool_id(entry: dict[str, Any]) -> str:
    explicit = str(entry.get("pool_id") or "").strip()
    if explicit:
        return _validate_pool_id(explicit)

    bot_prefix = str(entry.get("bot_prefix") or "").strip()
    candidate = bot_prefix.rstrip("_")
    if not candidate:
        candidate = str(entry.get("username") or "").strip()
    if not candidate:
        raise ValueError("pool_id is required when bot_prefix is empty")
    return _validate_pool_id(candidate)


def _parse_pool_config(entry: dict[str, Any], idx: int) -> PoolConfig:
    required = ("refresh_token", "tenant_id", "domain", "bot_prefix", "email_file")
    missing = [key for key in required if not str(entry.get(key) or "").strip()]
    if missing:
        raise ValueError(f"config entry #{idx} missing required field(s): {', '.join(missing)}")

    username = str(entry.get("username") or "").strip()
    if not username:
        username = f"pool_{idx}"

    return PoolConfig(
        pool_id=resolve_pool_id(entry),
        refresh_token=str(entry["refresh_token"]).strip(),
        tenant_id=str(entry["tenant_id"]).strip(),
        username=username,
        domain=str(entry["domain"]).strip().lower(),
        proxy=(str(entry.get("proxy")).strip() if entry.get("proxy") else None),
        email_file=str(entry["email_file"]).strip(),
        bot_prefix=str(entry["bot_prefix"]).strip(),
        enabled=entry.get("enabled", True) is not False,
    )


def _validate_pool_id(pool_id: str) -> str:
    if not POOL_ID_PATTERN.fullmatch(pool_id):
        raise ValueError(
            f"invalid pool_id {pool_id!r}; use only letters, numbers, '.', '_', '-'"
        )
    return pool_id
