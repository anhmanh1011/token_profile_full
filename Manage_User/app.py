"""Manage_User API service.

The service reads only ``admin_token_config_global.json``. Each enabled config
entry becomes an independent pool with its own admin account, SOCKS5 proxy,
token queue, producer, cleanup prefix, and delete stats.
"""
from __future__ import annotations

import logging
import queue
import re
import sys
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from flask import Flask, jsonify, request

from admin_token_manager import AdminTokenManager
from cleanup import StartupCleaner
from deleter import FastBulkDeleter
from pool_config import PoolConfig, load_pool_configs
from producer import TokenProducer

CONFIG_FILE = Path(__file__).resolve().parent.parent / "admin_token_config_global.json"
LOG_FILE = Path(__file__).parent / "service.log"
MAX_DELETE_EMAILS = 20
MAX_TOKEN_FETCH = 500
EMAIL_PATTERN = re.compile(
    r"^[A-Za-z0-9.!#$%&*+/=?^_`{|}~-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$"
)
GRAPH_URL = "https://graph.microsoft.com/v1.0"

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(name)s - %(levelname)s - %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
    handlers=[
        logging.StreamHandler(sys.stdout),
        logging.FileHandler(LOG_FILE, encoding="utf-8"),
    ],
)
logger = logging.getLogger(__name__)

app = Flask(__name__)


@dataclass
class PoolState:
    config: PoolConfig
    token_mgr: AdminTokenManager
    token_queue: queue.Queue
    producer: TokenProducer | None = None
    delete_stats: dict[str, int] = field(
        default_factory=lambda: {"total_deleted": 0, "total_failed_delete": 0}
    )
    delete_stats_lock: threading.Lock = field(default_factory=threading.Lock)


_pools: dict[str, PoolState] = {}
_pools_lock = threading.Lock()


def _validate_delete_emails(emails: list[Any]) -> str | None:
    if len(emails) > MAX_DELETE_EMAILS:
        return f"emails list must contain at most {MAX_DELETE_EMAILS} items"

    for email in emails:
        if not isinstance(email, str):
            return "all emails must be strings"
        if len(email) > 254 or not EMAIL_PATTERN.fullmatch(email):
            return f"invalid email: {email!r}"

    return None


def _validate_pool_delete_emails(pool: PoolState, emails: list[str]) -> str | None:
    prefix = pool.config.bot_prefix.lower()
    domain = pool.config.domain.lower()

    for email in emails:
        local, sep, email_domain = email.lower().partition("@")
        if sep != "@" or email_domain != domain:
            return f"email {email!r} is outside pool {pool.config.pool_id}"
        if not local.startswith(prefix):
            return f"email {email!r} does not match pool prefix {pool.config.bot_prefix!r}"

    return None


def _pool_status(pool: PoolState) -> dict[str, Any]:
    producer_stats = (
        pool.producer.stats()
        if pool.producer is not None
        else {
            "total_created": 0,
            "total_tokens": 0,
            "total_failed_create": 0,
            "total_failed_token": 0,
        }
    )
    with pool.delete_stats_lock:
        delete_snapshot = dict(pool.delete_stats)

    return {
        "pool_id": pool.config.pool_id,
        "domain": pool.config.domain,
        "username": pool.config.username,
        "email_file": pool.config.email_file,
        "bot_prefix": pool.config.bot_prefix,
        "proxy_configured": bool(pool.token_mgr.proxy_url),
        "queue_size": pool.token_queue.qsize(),
        "producer_running": bool(pool.producer and pool.producer.running),
        "total_created": producer_stats.get("total_created", 0),
        "total_tokens": producer_stats.get("total_tokens", 0),
        "total_failed_create": producer_stats.get("total_failed_create", 0),
        "total_failed_token": producer_stats.get("total_failed_token", 0),
        "total_deleted": delete_snapshot["total_deleted"],
        "total_failed_delete": delete_snapshot["total_failed_delete"],
    }


def _resolve_pool(pool_id: str | None = None):
    requested = (pool_id or request.args.get("pool_id") or "").strip()
    if not requested and request.method == "POST":
        body = request.get_json(silent=True) or {}
        if isinstance(body, dict):
            requested = str(body.get("pool_id") or "").strip()

    with _pools_lock:
        if not _pools:
            return None, (jsonify({"error": "Service not initialised"}), 503)
        if not requested:
            if len(_pools) == 1:
                return next(iter(_pools.values())), None
            return None, (
                jsonify({"error": "pool_id is required when multiple pools are configured"}),
                400,
            )
        pool = _pools.get(requested)

    if pool is None:
        return None, (jsonify({"error": f"unknown pool_id: {requested}"}), 404)
    return pool, None


def _purge_users(pool: PoolState, emails: list[str]) -> None:
    """Permanently delete users from recycle bin by email (best-effort)."""
    token = pool.token_mgr.get_token()
    if not token:
        return

    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}

    user_ids = []
    for email in emails:
        try:
            resp = pool.token_mgr.session.get(
                f"{GRAPH_URL}/directory/deletedItems/microsoft.graph.user"
                f"?$filter=userPrincipalName eq '{email}'&$select=id",
                headers=headers,
                timeout=10,
            )
            if resp.status_code == 200:
                for user in resp.json().get("value", []):
                    user_ids.append(user["id"])
        except Exception:
            logger.debug("Purge lookup failed for %s", email, exc_info=True)

    if not user_ids:
        return

    requests_list = [
        {"id": str(i), "method": "DELETE", "url": f"/directory/deletedItems/{uid}"}
        for i, uid in enumerate(user_ids)
    ]
    try:
        pool.token_mgr.session.post(
            f"{GRAPH_URL}/$batch",
            headers=headers,
            json={"requests": requests_list},
            timeout=60,
        )
    except Exception as e:
        logger.warning("Purge batch error for pool %s: %s", pool.config.pool_id, e)


def _tokens_response(pool: PoolState):
    count = request.args.get("count", 100, type=int)
    if count is None or count < 1:
        return jsonify({"error": "count must be a positive integer"}), 400
    count = min(count, MAX_TOKEN_FETCH)

    tokens = []
    for _ in range(count):
        try:
            tok = pool.token_queue.get_nowait()
            tokens.append(
                {
                    "email": tok["email"],
                    "refresh_token": tok["refresh_token"],
                    "tenant_id": tok["tenant_id"],
                    "pool_id": pool.config.pool_id,
                }
            )
        except queue.Empty:
            break

    if not tokens:
        return jsonify({"waiting": True, "count": 0, "pool_id": pool.config.pool_id}), 202

    return jsonify({"tokens": tokens, "count": len(tokens), "pool_id": pool.config.pool_id}), 200


@app.get("/pools")
def list_pools():
    with _pools_lock:
        pools = [_pool_status(pool) for pool in _pools.values()]
    return jsonify({"pool_count": len(pools), "pools": pools}), 200


@app.get("/pools/<pool_id>/status")
def get_pool_status(pool_id: str):
    pool, error = _resolve_pool(pool_id)
    if error:
        return error
    return jsonify(_pool_status(pool)), 200


@app.get("/pools/<pool_id>/tokens/next")
def get_next_token_for_pool(pool_id: str):
    pool, error = _resolve_pool(pool_id)
    if error:
        return error
    return _tokens_response(pool)


@app.get("/tokens/next")
def get_next_token():
    pool, error = _resolve_pool()
    if error:
        return error
    return _tokens_response(pool)


@app.post("/pools/<pool_id>/users/delete")
def delete_users_for_pool(pool_id: str):
    return _delete_users(pool_id)


@app.post("/users/delete")
def delete_users():
    return _delete_users()


def _delete_users(pool_id: str | None = None):
    body = request.get_json(silent=True)
    if not body or not isinstance(body.get("emails"), list):
        return jsonify({"error": "Request body must be JSON with 'emails' list"}), 400

    emails: list[str] = body["emails"]
    validation_error = _validate_delete_emails(emails)
    if validation_error:
        return jsonify({"error": validation_error}), 400

    pool, error = _resolve_pool(pool_id)
    if error:
        return error

    pool_validation_error = _validate_pool_delete_emails(pool, emails)
    if pool_validation_error:
        return jsonify({"error": pool_validation_error}), 400

    if not emails:
        return jsonify({"deleted": 0, "failed": 0, "pool_id": pool.config.pool_id}), 200

    deleter = FastBulkDeleter(pool.token_mgr)
    results = deleter._process_batch(emails)

    deleted_emails = [r["email"] for r in results if r["success"]]
    deleted = len(deleted_emails)
    failed = len(results) - deleted

    if deleted_emails:
        _purge_users(pool, deleted_emails)

    with pool.delete_stats_lock:
        pool.delete_stats["total_deleted"] += deleted
        pool.delete_stats["total_failed_delete"] += failed

    logger.info(
        "/users/delete pool=%s: %d deleted, %d failed",
        pool.config.pool_id,
        deleted,
        failed,
    )
    return jsonify({"deleted": deleted, "failed": failed, "pool_id": pool.config.pool_id}), 200


@app.get("/pools/<pool_id>/proxy")
def get_pool_proxy(pool_id: str):
    pool, error = _resolve_pool(pool_id)
    if error:
        return error
    return jsonify({"proxy": pool.token_mgr.proxy_url, "pool_id": pool.config.pool_id}), 200


@app.get("/proxy")
def get_proxy():
    pool, error = _resolve_pool()
    if error:
        return error
    return jsonify({"proxy": pool.token_mgr.proxy_url, "pool_id": pool.config.pool_id}), 200


@app.get("/status")
def get_status():
    with _pools_lock:
        pools = [_pool_status(pool) for pool in _pools.values()]

    return jsonify(
        {
            "pool_count": len(pools),
            "total_queue_size": sum(pool["queue_size"] for pool in pools),
            "total_created": sum(pool["total_created"] for pool in pools),
            "total_tokens": sum(pool["total_tokens"] for pool in pools),
            "total_deleted": sum(pool["total_deleted"] for pool in pools),
            "total_failed_delete": sum(pool["total_failed_delete"] for pool in pools),
            "pools": pools,
        }
    ), 200


def _stop_existing_producers() -> None:
    with _pools_lock:
        pools = list(_pools.values())
        _pools.clear()

    for pool in pools:
        if pool.producer is not None:
            pool.producer.stop()


def _initialise_pools(config_file: Path) -> None:
    configs = load_pool_configs(config_file)
    new_pools: dict[str, PoolState] = {}

    for config in configs:
        token_mgr = AdminTokenManager(
            config.as_admin_dict(),
            config_file=config_file,
            config_pool_id=config.pool_id,
        )
        token_queue: queue.Queue = queue.Queue(maxsize=1000)
        pool = PoolState(config=config, token_mgr=token_mgr, token_queue=token_queue)

        logger.info(
            "Loaded pool %s: domain=%s email_file=%s bot_prefix=%s proxy=%s",
            config.pool_id,
            config.domain,
            config.email_file,
            config.bot_prefix,
            "yes" if token_mgr.proxy_url else "no",
        )

        logger.info("Running startup cleanup for pool %s...", config.pool_id)
        try:
            cleaner = StartupCleaner(token_mgr, bot_prefix=config.bot_prefix)
            cleanup_result = cleaner.run()
            logger.info(
                "Cleanup pool %s done: found=%d deleted=%d failed=%d purged=%d",
                config.pool_id,
                cleanup_result["found"],
                cleanup_result["deleted"],
                cleanup_result["failed"],
                cleanup_result.get("purged", 0),
            )
        except Exception as e:
            logger.warning("Startup cleanup failed for pool %s: %s", config.pool_id, e)

        producer = TokenProducer(
            token_mgr,
            token_queue,
            bot_prefix=config.bot_prefix,
            pool_id=config.pool_id,
        )
        producer.start()
        pool.producer = producer
        new_pools[config.pool_id] = pool

    with _pools_lock:
        _pools.clear()
        _pools.update(new_pools)


def start_service(
    host: str = "0.0.0.0",
    port: int = 5000,
    config_file: Path = CONFIG_FILE,
) -> None:
    logger.info("=" * 60)
    logger.info("  Manage_User API Service starting on %s:%d", host, port)
    logger.info("  Config: %s", config_file)
    logger.info("=" * 60)

    try:
        _stop_existing_producers()
        _initialise_pools(Path(config_file))
    except Exception as e:
        logger.error("Failed to initialise pools: %s", e)
        sys.exit(1)

    logger.info("Flask ready - listening on %s:%d", host, port)
    app.run(host=host, port=port, threaded=True)


if __name__ == "__main__":
    import argparse

    parser = argparse.ArgumentParser(description="Manage_User API Service")
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=5000)
    parser.add_argument("--config", type=Path, default=CONFIG_FILE)
    args = parser.parse_args()
    start_service(host=args.host, port=args.port, config_file=args.config)
