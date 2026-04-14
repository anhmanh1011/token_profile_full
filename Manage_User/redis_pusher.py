"""
Redis Pusher — Push user credentials and tokens to Redis Hash queues.
Uses HSET with pipeline for bulk insert performance.
"""
import logging
from datetime import datetime
from pathlib import Path
from typing import Optional

import redis

logger = logging.getLogger(__name__)

BACKUP_DIR = Path(__file__).parent / "backup"


class RedisPusher:
    """Push data to Redis Hash queues with backup file support."""

    def __init__(
        self,
        host: str = "localhost",
        port: int = 6379,
        db: int = 0,
        password: Optional[str] = None,
    ):
        self.client = redis.Redis(
            host=host, port=port, db=db, password=password, decode_responses=True
        )

    def ping(self) -> bool:
        """Check Redis connectivity."""
        try:
            return self.client.ping()
        except redis.ConnectionError:
            return False

    def clear_queue(self, queue_name: str) -> int:
        """Delete a hash queue. Returns 1 if deleted, 0 if not found."""
        deleted = self.client.delete(queue_name)
        logger.info("Cleared queue '%s' (existed: %s)", queue_name, bool(deleted))
        return deleted

    def push_users(self, queue_name: str, users: list[dict]) -> int:
        """
        Push users to Redis Hash: HSET queue_name email password.

        Args:
            queue_name: Redis key name, e.g. "redis-users-1"
            users: List of {"email": str, "password": str}

        Returns:
            Number of fields added.
        """
        if not users:
            return 0

        pipe = self.client.pipeline()
        for user in users:
            pipe.hset(queue_name, user["email"], user["password"])
        results = pipe.execute()
        count = sum(1 for r in results if r)
        logger.info("Pushed %d users to '%s'", len(users), queue_name)
        return count

    def push_tokens(self, queue_name: str, tokens: list[dict]) -> int:
        """
        Push tokens to Redis Hash: HSET queue_name email "email|refresh_token|tenant_id".

        Args:
            queue_name: Redis key name, e.g. "redis-tokens-1"
            tokens: List of {"email": str, "refresh_token": str, "tenant_id": str}

        Returns:
            Number of fields added.
        """
        if not tokens:
            return 0

        pipe = self.client.pipeline()
        for token in tokens:
            value = f"{token['email']}|{token['refresh_token']}|{token['tenant_id']}"
            pipe.hset(queue_name, token["email"], value)
        results = pipe.execute()
        count = sum(1 for r in results if r)
        logger.info("Pushed %d tokens to '%s'", len(tokens), queue_name)
        return count

    def get_queue_size(self, queue_name: str) -> int:
        """Return number of fields in a hash queue."""
        return self.client.hlen(queue_name)


def write_backup_users(queue_suffix: str, users: list[dict]) -> Path:
    """Write user backup file: email:password per line."""
    BACKUP_DIR.mkdir(exist_ok=True)
    date_str = datetime.now().strftime("%Y-%m-%d")
    path = BACKUP_DIR / f"{date_str}_redis-users-{queue_suffix}.txt"
    with open(path, "w", encoding="utf-8") as f:
        for user in users:
            f.write(f"{user['email']}:{user['password']}\n")
    logger.info("Backup users written to %s (%d entries)", path, len(users))
    return path


def write_backup_tokens(queue_suffix: str, tokens: list[dict]) -> Path:
    """Write token backup file: email|refresh_token|tenant_id per line."""
    BACKUP_DIR.mkdir(exist_ok=True)
    date_str = datetime.now().strftime("%Y-%m-%d")
    path = BACKUP_DIR / f"{date_str}_redis-tokens-{queue_suffix}.txt"
    with open(path, "w", encoding="utf-8") as f:
        for token in tokens:
            f.write(
                f"{token['email']}|{token['refresh_token']}|{token['tenant_id']}\n"
            )
    logger.info("Backup tokens written to %s (%d entries)", path, len(tokens))
    return path
