"""
Manage_User Pipeline Orchestrator

Daily pipeline per admin:
1. Delete all normal users (protect admins)
2. Create 10K new users
3. Push user credentials to Redis (redis-users-{N})
4. Get refresh tokens via browser flow
5. Push tokens to Redis (redis-tokens-{N})
"""
import json
import logging
import sys
import time
from datetime import datetime
from pathlib import Path

from creator import BulkUserCreator
from deleter import FastBulkDeleter
from redis_pusher import (
    RedisPusher,
    write_backup_tokens,
    write_backup_users,
)
from token_getter import BulkTokenGetter

CONFIG_FILE = Path(__file__).parent / "admin_token.json"
LOG_FILE = Path(__file__).parent / "pipeline.log"
DEFAULT_USER_COUNT = 10000

# Configure logging: console + file
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


def load_admin_config() -> list[dict]:
    """Load admin list from admin_token.json."""
    with open(CONFIG_FILE, "r", encoding="utf-8") as f:
        return json.load(f)


def save_admin_config(admins: list[dict]) -> None:
    """Save updated admin config (e.g. refreshed tokens)."""
    with open(CONFIG_FILE, "w", encoding="utf-8") as f:
        json.dump(admins, f, indent=4, ensure_ascii=False)


def run_pipeline(admin: dict, count: int = DEFAULT_USER_COUNT) -> dict:
    """
    Run full pipeline for a single admin.

    Returns summary dict with stats from each step.
    """
    queue_suffix = admin["redis_queue"]
    domain = admin.get("domain", "unknown")
    users_queue = f"redis-users-{queue_suffix}"
    tokens_queue = f"redis-tokens-{queue_suffix}"

    logger.info("=" * 60)
    logger.info("PIPELINE START: %s (queue suffix: %s)", domain, queue_suffix)
    logger.info("=" * 60)

    pipeline_start = time.time()
    summary = {
        "domain": domain,
        "queue_suffix": queue_suffix,
        "delete": None,
        "create": None,
        "push_users": None,
        "get_tokens": None,
        "push_tokens": None,
    }

    # ── Step 1: Delete all normal users ──────────────────────
    logger.info("── Step 1/5: Delete normal users ──")
    deleter = FastBulkDeleter(admin, queue_suffix=queue_suffix, auto_confirm=True)
    delete_result = deleter.run()
    summary["delete"] = delete_result
    logger.info(
        "Delete result: %d deleted, %d failed, %d admins protected",
        delete_result["deleted"],
        delete_result["failed"],
        delete_result["skipped_admins"],
    )

    # Track new refresh token from deleter
    new_rt = delete_result.get("new_refresh_token")
    if new_rt:
        admin["refresh_token"] = new_rt

    # ── Step 2: Create new users ─────────────────────────────
    logger.info("── Step 2/5: Create %d users ──", count)
    creator = BulkUserCreator(admin, count)
    create_result = creator.run()
    created_users = create_result["created_users"]
    summary["create"] = {
        "created": len(created_users),
        "failed": create_result["failed"],
        "licensed": create_result["licensed"],
    }
    logger.info(
        "Create result: %d created, %d failed",
        len(created_users),
        create_result["failed"],
    )

    # Track new refresh token from creator
    new_rt = create_result.get("new_refresh_token")
    if new_rt:
        admin["refresh_token"] = new_rt

    if not created_users:
        logger.error("No users created, stopping pipeline for %s", domain)
        return summary

    # ── Step 3: Push users to Redis ──────────────────────────
    logger.info("── Step 3/5: Push users to Redis '%s' ──", users_queue)

    # Always write backup first
    write_backup_users(queue_suffix, created_users)

    try:
        redis = RedisPusher()
        if not redis.ping():
            raise ConnectionError("Redis not reachable")
        redis.clear_queue(users_queue)
        redis.push_users(users_queue, created_users)
        queue_size = redis.get_queue_size(users_queue)
        summary["push_users"] = {"pushed": queue_size, "status": "ok"}
        logger.info("Pushed %d users to Redis '%s'", queue_size, users_queue)
    except Exception as e:
        logger.error("Redis push users failed: %s", e)
        summary["push_users"] = {"pushed": 0, "status": "error", "error": str(e)}
        logger.warning("Continuing with token retrieval despite Redis failure")

    # ── Step 4: Get refresh tokens ───────────────────────────
    logger.info("── Step 4/5: Get refresh tokens for %d users ──", len(created_users))
    getter = BulkTokenGetter(created_users)
    token_result = getter.run()
    tokens = token_result["tokens"]
    summary["get_tokens"] = {
        "success": len(tokens),
        "failed": token_result["failed"],
    }
    logger.info(
        "Token result: %d success, %d failed",
        len(tokens),
        token_result["failed"],
    )

    if not tokens:
        logger.error("No tokens obtained, skipping Redis push for %s", domain)
        return summary

    # ── Step 5: Push tokens to Redis ─────────────────────────
    logger.info("── Step 5/5: Push tokens to Redis '%s' ──", tokens_queue)

    # Always write backup first
    write_backup_tokens(queue_suffix, tokens)

    try:
        redis = RedisPusher()
        if not redis.ping():
            raise ConnectionError("Redis not reachable")
        redis.clear_queue(tokens_queue)
        redis.push_tokens(tokens_queue, tokens)
        queue_size = redis.get_queue_size(tokens_queue)
        summary["push_tokens"] = {"pushed": queue_size, "status": "ok"}
        logger.info("Pushed %d tokens to Redis '%s'", queue_size, tokens_queue)
    except Exception as e:
        logger.error("Redis push tokens failed: %s", e)
        summary["push_tokens"] = {"pushed": 0, "status": "error", "error": str(e)}

    elapsed = time.time() - pipeline_start
    logger.info("PIPELINE COMPLETE for %s in %.1fs", domain, elapsed)

    return summary


def main() -> None:
    logger.info("=" * 60)
    logger.info("  Manage_User Pipeline - %s", datetime.now().strftime("%Y-%m-%d %H:%M:%S"))
    logger.info("=" * 60)

    # Load admins
    admins = load_admin_config()
    logger.info("Loaded %d admin(s) from %s", len(admins), CONFIG_FILE.name)

    all_summaries = []
    token_updated = False

    for idx, admin in enumerate(admins):
        logger.info("\n>>> Processing admin %d/%d: %s", idx + 1, len(admins), admin.get("domain", "?"))

        original_rt = admin.get("refresh_token", "")
        summary = run_pipeline(admin)
        all_summaries.append(summary)

        # Check if refresh token was updated during pipeline
        if admin.get("refresh_token", "") != original_rt:
            admins[idx] = admin
            token_updated = True

    # Save updated tokens
    if token_updated:
        save_admin_config(admins)
        logger.info("Updated refresh token(s) saved to %s", CONFIG_FILE.name)

    # Final summary
    logger.info("\n" + "=" * 60)
    logger.info("  FINAL SUMMARY")
    logger.info("=" * 60)
    for s in all_summaries:
        logger.info("Domain: %s (queue: %s)", s["domain"], s["queue_suffix"])
        if s["delete"]:
            logger.info("  Delete: %d deleted", s["delete"]["deleted"])
        if s["create"]:
            logger.info("  Create: %d created", s["create"]["created"])
        if s["push_users"]:
            logger.info("  Redis users: %s", s["push_users"].get("status", "skipped"))
        if s["get_tokens"]:
            logger.info("  Tokens: %d obtained", s["get_tokens"]["success"])
        if s["push_tokens"]:
            logger.info("  Redis tokens: %s", s["push_tokens"].get("status", "skipped"))
    logger.info("=" * 60)


if __name__ == "__main__":
    main()
