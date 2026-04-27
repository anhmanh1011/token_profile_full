"""
Manage_User API Service

Long-running HTTP service that replaces the one-shot pipeline orchestrator.
Provides endpoints for token consumption, on-demand user deletion, and status.

Endpoints:
    GET  /tokens/next    — Pop next token from queue (202 if empty)
    POST /users/delete   — Delete a list of users by email
    GET  /status         — Queue and producer stats
"""
import json
import logging
import queue
import sys
import threading
import time
from pathlib import Path

from flask import Flask, jsonify, request

from admin_token_manager import AdminTokenManager
from cleanup import StartupCleaner
from deleter import FastBulkDeleter
from producer import TokenProducer

# ── Configuration ────────────────────────────────────────────────────────────

CONFIG_FILE = Path(__file__).parent / "admin_token.json"
LOG_FILE = Path(__file__).parent / "service.log"

# ── Logging ──────────────────────────────────────────────────────────────────

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

# ── Global state ─────────────────────────────────────────────────────────────

token_queue: queue.Queue = queue.Queue(maxsize=1000)

_delete_stats: dict = {"total_deleted": 0, "total_failed_delete": 0}
_delete_stats_lock = threading.Lock()

_producer: TokenProducer | None = None
_admin_config: dict | None = None
_token_mgr: AdminTokenManager | None = None

# ── Flask app ────────────────────────────────────────────────────────────────

app = Flask(__name__)


# ── Helpers ───────────────────────────────────────────────────────────────────


def _load_admin_config() -> dict:
    """Load the first admin entry from admin_token.json.

    Returns:
        The first admin dict (1 VPS = 1 admin).

    Raises:
        FileNotFoundError: If admin_token.json does not exist.
        ValueError: If the config file is empty or not a list.
    """
    with open(CONFIG_FILE, "r", encoding="utf-8") as f:
        admins = json.load(f)

    if not isinstance(admins, list) or not admins:
        raise ValueError(f"{CONFIG_FILE} must be a non-empty JSON array")

    return admins[0]


GRAPH_URL = "https://graph.microsoft.com/v1.0"


def _purge_users(emails: list[str]) -> None:
    """Permanently delete users from recycle bin by email (best-effort)."""
    if not _token_mgr:
        return
    token = _token_mgr.get_token()
    if not token:
        return

    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}

    # Find deleted user IDs by email
    user_ids = []
    for email in emails:
        try:
            resp = _token_mgr.session.get(
                f"{GRAPH_URL}/directory/deletedItems/microsoft.graph.user"
                f"?$filter=userPrincipalName eq '{email}'&$select=id",
                headers=headers, timeout=10,
            )
            if resp.status_code == 200:
                for u in resp.json().get("value", []):
                    user_ids.append(u["id"])
        except Exception:
            pass

    if not user_ids:
        return

    # Batch permanently delete
    reqs = [
        {"id": str(i), "method": "DELETE", "url": f"/directory/deletedItems/{uid}"}
        for i, uid in enumerate(user_ids)
    ]
    try:
        _token_mgr.session.post(
            f"{GRAPH_URL}/$batch", headers=headers,
            json={"requests": reqs}, timeout=60,
        )
    except Exception as e:
        logger.warning("Purge batch error: %s", e)


# ── Endpoints ─────────────────────────────────────────────────────────────────


@app.get("/tokens/next")
def get_next_token():
    """Pop tokens from the in-memory queue.

    Query params:
        count: number of tokens to return (default 100, max 200)

    Returns:
        200: {"tokens": [{...}, ...], "count": N}
        202: {"waiting": true, "count": 0}  — queue is currently empty
    """
    count = min(request.args.get("count", 100, type=int), 500)
    tokens = []
    for _ in range(count):
        try:
            tok = token_queue.get_nowait()
            tokens.append({
                "email": tok["email"],
                "refresh_token": tok["refresh_token"],
                "tenant_id": tok["tenant_id"],
            })
        except queue.Empty:
            break

    if not tokens:
        return jsonify({"waiting": True, "count": 0}), 202

    return jsonify({"tokens": tokens, "count": len(tokens)}), 200


@app.post("/users/delete")
def delete_users():
    """Delete a list of Microsoft 365 users by email.

    Request body (JSON):
        {"emails": ["user@domain.com", ...]}

    Returns:
        200: {"deleted": N, "failed": N}
        400: {"error": "..."}  — malformed request
        503: {"error": "..."}  — service not initialised yet
    """
    if _token_mgr is None:
        return jsonify({"error": "Service not initialised"}), 503

    body = request.get_json(silent=True)
    if not body or not isinstance(body.get("emails"), list):
        return jsonify({"error": "Request body must be JSON with 'emails' list"}), 400

    emails: list[str] = body["emails"]
    if not emails:
        return jsonify({"deleted": 0, "failed": 0}), 200

    # Soft-delete via Graph API
    deleter = FastBulkDeleter(_token_mgr)
    results = deleter._process_batch(emails)

    deleted_emails = [r["email"] for r in results if r["success"]]
    deleted = len(deleted_emails)
    failed = len(results) - deleted

    # Permanently delete from recycle bin
    if deleted_emails:
        _purge_users(deleted_emails)

    with _delete_stats_lock:
        _delete_stats["total_deleted"] += deleted
        _delete_stats["total_failed_delete"] += failed

    logger.info("/users/delete: %d deleted, %d failed", deleted, failed)
    return jsonify({"deleted": deleted, "failed": failed}), 200


@app.get("/proxy")
def get_proxy():
    """Return the SOCKS5 proxy URL configured for the current admin.

    Returns:
        200: {"proxy": "socks5h://..."} or {"proxy": null} if no proxy configured.
        503: {"error": "..."}  — service not initialised yet
    """
    if _token_mgr is None:
        return jsonify({"error": "Service not initialised"}), 503
    return jsonify({"proxy": _token_mgr.proxy_url}), 200


@app.get("/status")
def get_status():
    """Return current service stats.

    Returns:
        200: {
            "queue_size": int,
            "total_created": int,
            "total_tokens": int,
            "total_deleted": int,
            "total_failed_delete": int,
        }
    """
    producer_stats = _producer.stats() if _producer is not None else {
        "total_created": 0,
        "total_tokens": 0,
    }

    with _delete_stats_lock:
        delete_snapshot = dict(_delete_stats)

    return jsonify(
        {
            "queue_size": token_queue.qsize(),
            "total_created": producer_stats.get("total_created", 0),
            "total_tokens": producer_stats.get("total_tokens", 0),
            "total_deleted": delete_snapshot["total_deleted"],
            "total_failed_delete": delete_snapshot["total_failed_delete"],
        }
    ), 200


# ── Service startup ───────────────────────────────────────────────────────────


def start_service(host: str = "0.0.0.0", port: int = 5000) -> None:
    """Initialise and start the API service.

    Sequence:
        1. Load admin config (first admin only).
        2. Run StartupCleaner to remove orphaned bot_ users.
        3. Start TokenProducer background thread.
        4. Start Flask HTTP server.
    """
    global _admin_config, _producer, _token_mgr

    logger.info("=" * 60)
    logger.info("  Manage_User API Service starting on %s:%d", host, port)
    logger.info("=" * 60)

    # Step 1: Load admin and create shared token manager
    try:
        _admin_config = _load_admin_config()
        _token_mgr = AdminTokenManager(_admin_config)
        logger.info(
            "Loaded admin config: domain=%s", _admin_config.get("domain", "?")
        )
    except Exception as e:
        logger.error("Failed to load admin config: %s", e)
        sys.exit(1)

    # Step 2: Startup cleanup — delete orphaned bot_ users
    logger.info("Running startup cleanup...")
    try:
        cleaner = StartupCleaner(_token_mgr)
        cleanup_result = cleaner.run()
        logger.info(
            "Startup cleanup done: found=%d deleted=%d failed=%d purged=%d",
            cleanup_result["found"],
            cleanup_result["deleted"],
            cleanup_result["failed"],
            cleanup_result.get("purged", 0),
        )
    except Exception as e:
        logger.warning("Startup cleanup encountered an error: %s", e)

    # Step 3: Start token producer and wait for queue to fill
    logger.info("Starting TokenProducer background thread...")
    _producer = TokenProducer(_token_mgr, token_queue)
    _producer.start()

    # Step 4: Wait until queue has >= MIN_QUEUE_SIZE tokens
    from producer import MIN_QUEUE_SIZE
    logger.info("Waiting for queue to fill to %d tokens...", MIN_QUEUE_SIZE)
    while token_queue.qsize() < MIN_QUEUE_SIZE:
        size = token_queue.qsize()
        logger.info("Queue: %d / %d tokens, waiting...", size, MIN_QUEUE_SIZE)
        time.sleep(5)
    logger.info("Queue ready: %d tokens", token_queue.qsize())

    # Step 5: Run Flask (blocking)
    logger.info("Flask ready — listening on %s:%d", host, port)
    app.run(host=host, port=port, threaded=True)


# ── CLI entry point ───────────────────────────────────────────────────────────

if __name__ == "__main__":
    import argparse

    parser = argparse.ArgumentParser(description="Manage_User API Service")
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=5000)
    args = parser.parse_args()
    start_service(host=args.host, port=args.port)
