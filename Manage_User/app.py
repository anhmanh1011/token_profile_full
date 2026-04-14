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
from pathlib import Path

from flask import Flask, jsonify, request

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


# ── Endpoints ─────────────────────────────────────────────────────────────────


@app.get("/tokens/next")
def get_next_token():
    """Pop the next token from the in-memory queue.

    Returns:
        200: {"email": str, "refresh_token": str, "tenant_id": str}
        202: {"waiting": true}  — queue is currently empty
    """
    try:
        token = token_queue.get_nowait()
        return jsonify(
            {
                "email": token["email"],
                "refresh_token": token["refresh_token"],
                "tenant_id": token["tenant_id"],
            }
        ), 200
    except queue.Empty:
        return jsonify({"waiting": True}), 202


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
    if _admin_config is None:
        return jsonify({"error": "Service not initialised"}), 503

    body = request.get_json(silent=True)
    if not body or not isinstance(body.get("emails"), list):
        return jsonify({"error": "Request body must be JSON with 'emails' list"}), 400

    emails: list[str] = body["emails"]
    if not emails:
        return jsonify({"deleted": 0, "failed": 0}), 200

    # FastBulkDeleter with queue_suffix="" skips Redis (Task 5 contract)
    deleter = FastBulkDeleter(_admin_config, auto_confirm=True)
    results = deleter._process_batch(emails)

    deleted = sum(1 for r in results if r["success"])
    failed = len(results) - deleted

    with _delete_stats_lock:
        _delete_stats["total_deleted"] += deleted
        _delete_stats["total_failed_delete"] += failed

    logger.info("/users/delete: %d deleted, %d failed", deleted, failed)
    return jsonify({"deleted": deleted, "failed": failed}), 200


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
    global _admin_config, _producer

    logger.info("=" * 60)
    logger.info("  Manage_User API Service starting on %s:%d", host, port)
    logger.info("=" * 60)

    # Step 1: Load admin
    try:
        _admin_config = _load_admin_config()
        logger.info(
            "Loaded admin config: domain=%s", _admin_config.get("domain", "?")
        )
    except Exception as e:
        logger.error("Failed to load admin config: %s", e)
        sys.exit(1)

    # Step 2: Startup cleanup — delete orphaned bot_ users
    logger.info("Running startup cleanup...")
    try:
        cleaner = StartupCleaner(_admin_config)
        cleanup_result = cleaner.run()
        logger.info(
            "Startup cleanup done: found=%d deleted=%d failed=%d",
            cleanup_result["found"],
            cleanup_result["deleted"],
            cleanup_result["failed"],
        )
    except Exception as e:
        logger.warning("Startup cleanup encountered an error: %s", e)

    # Step 3: Start token producer
    logger.info("Starting TokenProducer background thread...")
    _producer = TokenProducer(_admin_config, token_queue)
    _producer.start()

    # Step 4: Run Flask (blocking)
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
