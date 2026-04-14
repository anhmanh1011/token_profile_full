"""
Background Producer — Continuously create users and get tokens.

Runs in a background thread, maintains >=500 tokens in queue.
Creates batch of 100 users at a time, gets their refresh tokens,
and pushes to a thread-safe queue.
"""
import logging
import queue
import threading
import time

from creator import BulkUserCreator
from token_getter import BulkTokenGetter

logger = logging.getLogger(__name__)

MIN_QUEUE_SIZE = 500
BATCH_SIZE = 100
POLL_INTERVAL = 30  # seconds to sleep when queue is full


class TokenProducer:
    """Background producer that keeps token queue filled."""

    def __init__(self, admin: dict, token_queue: queue.Queue):
        self.admin = admin
        self.token_queue = token_queue
        self.running = False
        self.thread: threading.Thread | None = None

        # Stats
        self.total_created = 0
        self.total_tokens = 0
        self.total_failed_create = 0
        self.total_failed_token = 0
        self.stats_lock = threading.Lock()

    def start(self) -> None:
        """Start producer in background thread."""
        self.running = True
        self.thread = threading.Thread(target=self._run, daemon=True, name="producer")
        self.thread.start()
        logger.info("Producer started (min queue: %d, batch: %d)", MIN_QUEUE_SIZE, BATCH_SIZE)

    def stop(self) -> None:
        """Signal producer to stop."""
        self.running = False
        if self.thread:
            self.thread.join(timeout=10)
        logger.info("Producer stopped")

    def stats(self) -> dict:
        with self.stats_lock:
            return {
                "total_created": self.total_created,
                "total_tokens": self.total_tokens,
                "total_failed_create": self.total_failed_create,
                "total_failed_token": self.total_failed_token,
            }

    def _run(self) -> None:
        """Main producer loop."""
        while self.running:
            try:
                current_size = self.token_queue.qsize()
                if current_size >= MIN_QUEUE_SIZE:
                    logger.debug("Queue has %d tokens (>= %d), sleeping %ds",
                                 current_size, MIN_QUEUE_SIZE, POLL_INTERVAL)
                    time.sleep(POLL_INTERVAL)
                    continue

                need = min(MIN_QUEUE_SIZE - current_size, BATCH_SIZE)
                logger.info("Queue has %d tokens, producing %d more", current_size, need)
                self._produce_batch(need)

            except Exception as e:
                logger.error("Producer error: %s", e)
                time.sleep(5)

    def _produce_batch(self, count: int) -> None:
        """Create users, get tokens, push to queue."""
        # Step 1: Create users
        creator = BulkUserCreator(self.admin, count)
        create_result = creator.run()
        created_users = create_result["created_users"]

        with self.stats_lock:
            self.total_created += len(created_users)
            self.total_failed_create += create_result["failed"]

        # Track refreshed admin token
        new_rt = create_result.get("new_refresh_token")
        if new_rt:
            self.admin["refresh_token"] = new_rt

        if not created_users:
            logger.error("No users created in batch")
            return

        logger.info("Created %d users, getting tokens...", len(created_users))

        # Step 2: Get refresh tokens
        getter = BulkTokenGetter(created_users)
        token_result = getter.run()
        tokens = token_result["tokens"]

        with self.stats_lock:
            self.total_tokens += len(tokens)
            self.total_failed_token += token_result["failed"]

        # Step 3: Push tokens to queue
        pushed = 0
        for tok in tokens:
            try:
                self.token_queue.put(tok, timeout=5)
                pushed += 1
            except queue.Full:
                logger.warning("Token queue full, discarding remaining tokens")
                break

        logger.info("Produced %d tokens (created: %d, token_ok: %d, pushed: %d)",
                     count, len(created_users), len(tokens), pushed)
