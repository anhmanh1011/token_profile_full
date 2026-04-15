"""
Fast Bulk User Deleter — Delete app-created users from a Microsoft 365 tenant.

Only deletes users found in Redis queue (redis-users-{N}), not all tenant users.
Admin users are still protected as a safety net.
"""
import logging
import queue
import threading
import time
from typing import Optional

import redis as redis_lib
import requests

from admin_token_manager import AdminTokenManager

logger = logging.getLogger(__name__)

GRAPH_URL = "https://graph.microsoft.com/v1.0"
BATCH_SIZE = 20
DEFAULT_WORKERS = 2
DELAY_BETWEEN_BATCHES = 2.0


class FastBulkDeleter:
    """Delete app-created users from a Microsoft 365 tenant via Graph Batch API.

    Uses Redis redis-users-{N} as source of truth for which users to delete.
    """

    def __init__(
        self,
        token_mgr: AdminTokenManager,
        queue_suffix: str = "",
        workers: int = DEFAULT_WORKERS,
        auto_confirm: bool = False,
    ):
        self.token_mgr = token_mgr
        self.domain = token_mgr.domain
        self.queue_suffix = queue_suffix
        self.workers = workers
        self.auto_confirm = auto_confirm

        self.admin_emails: set[str] = set()

        # Stats
        self.deleted = 0
        self.failed = 0
        self.skipped_admins = 0
        self.stats_lock = threading.Lock()

        # Redis client (optional)
        self.redis_client = None
        if queue_suffix:
            self.redis_client = redis_lib.Redis(
                host="localhost", port=6379, db=0, decode_responses=True
            )

    def _get_token(self) -> Optional[str]:
        return self.token_mgr.get_token()

    def _get_admin_users(self) -> set[str]:
        """Fetch all users with admin directory roles."""
        admin_emails: set[str] = set()
        token = self._get_token()
        if not token:
            logger.error("Cannot get token to fetch admin roles")
            return admin_emails

        headers = {"Authorization": f"Bearer {token}"}

        try:
            resp = self.token_mgr.session.get(
                f"{GRAPH_URL}/directoryRoles", headers=headers, timeout=30
            )
            if resp.status_code != 200:
                logger.error("Failed to get directory roles: %d", resp.status_code)
                return admin_emails

            roles = resp.json().get("value", [])
            logger.info("Found %d directory roles", len(roles))

            for role in roles:
                role_id = role["id"]
                role_name = role.get("displayName", "Unknown")
                try:
                    members_resp = self.token_mgr.session.get(
                        f"{GRAPH_URL}/directoryRoles/{role_id}/members",
                        headers=headers,
                        timeout=30,
                    )
                    if members_resp.status_code == 200:
                        for member in members_resp.json().get("value", []):
                            if (
                                member.get("@odata.type")
                                == "#microsoft.graph.user"
                            ):
                                email = member.get("userPrincipalName", "").lower()
                                if email:
                                    admin_emails.add(email)
                except requests.RequestException as e:
                    logger.warning(
                        "Error getting members of role %s: %s", role_name, e
                    )

        except requests.RequestException as e:
            logger.error("Error fetching directory roles: %s", e)

        return admin_emails

    def _get_redis_users(self) -> list[str]:
        """Fetch app-created emails from Redis Hash redis-users-{N}."""
        if not self.redis_client:
            return []
        queue_name = f"redis-users-{self.queue_suffix}"
        try:
            data = self.redis_client.hgetall(queue_name)
            emails = [email.lower() for email in data.keys()]
            logger.info("Got %d emails from Redis '%s'", len(emails), queue_name)
            return emails
        except Exception as e:
            logger.error("Failed to read Redis '%s': %s", queue_name, e)
            return []

    def _filter_non_admin_users(self, users: list[str]) -> list[str]:
        """Filter out admin users from deletion list."""
        filtered = []
        for email in users:
            if email.lower() in self.admin_emails:
                self.skipped_admins += 1
            else:
                filtered.append(email)
        return filtered

    def _process_batch(self, emails: list[str], retry: int = 0) -> list[dict]:
        """Process a batch of user deletions with retry on throttle."""
        if retry > 3:
            logger.warning(
                "Max retries reached for batch of %d users (first: %s)",
                len(emails),
                emails[0] if emails else "?",
            )
            return [
                {"email": e, "success": False, "error": "Max retries"}
                for e in emails
            ]

        token = self._get_token()
        if not token:
            return [
                {"email": e, "success": False, "error": "No token"}
                for e in emails
            ]

        headers = {
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        }
        requests_list = [
            {"id": str(i), "method": "DELETE", "url": f"/users/{email}"}
            for i, email in enumerate(emails)
        ]
        batch_data = {"requests": requests_list}

        results = []
        retry_users = []

        try:
            resp = self.token_mgr.session.post(
                f"{GRAPH_URL}/$batch",
                headers=headers,
                json=batch_data,
                timeout=60,
            )

            if resp.status_code == 429:
                retry_after = int(resp.headers.get("Retry-After", 5))
                logger.warning(
                    "Batch throttled (429), retry %d/3, waiting %ds...",
                    retry + 1, retry_after,
                )
                time.sleep(min(retry_after, 30))
                return self._process_batch(emails, retry + 1)

            if resp.status_code == 200:
                batch_resp = resp.json()
                for response in batch_resp.get("responses", []):
                    idx = int(response["id"])
                    email = emails[idx]

                    if response["status"] == 204:
                        results.append({"email": email, "success": True})
                    elif response["status"] == 429:
                        retry_users.append(email)
                    else:
                        error_code = (
                            response.get("body", {})
                            .get("error", {})
                            .get("code", "Unknown")
                        )
                        error_msg = (
                            response.get("body", {})
                            .get("error", {})
                            .get("message", "Unknown error")[:120]
                        )
                        logger.warning(
                            "User delete failed [%d]: %s - %s (%s)",
                            response["status"],
                            error_code,
                            error_msg,
                            email,
                        )
                        results.append(
                            {"email": email, "success": False, "error": error_msg}
                        )
            else:
                logger.error(
                    "Batch failed: HTTP %d - %s", resp.status_code, resp.text[:200]
                )
                for email in emails:
                    results.append(
                        {
                            "email": email,
                            "success": False,
                            "error": f"Batch failed: {resp.status_code}",
                        }
                    )
        except requests.RequestException as e:
            for email in emails:
                results.append(
                    {"email": email, "success": False, "error": str(e)[:50]}
                )

        if retry_users:
            time.sleep(2**retry)
            results.extend(self._process_batch(retry_users, retry + 1))

        return results

    def _worker(self, batch_queue: queue.Queue, total: int) -> None:
        while True:
            try:
                batch = batch_queue.get(timeout=1)
            except queue.Empty:
                break

            results = self._process_batch(batch)
            time.sleep(DELAY_BETWEEN_BATCHES)

            with self.stats_lock:
                for result in results:
                    if result["success"]:
                        self.deleted += 1
                    else:
                        self.failed += 1

                done = self.deleted + self.failed
                if done % 100 < BATCH_SIZE:
                    pct = done * 100 // total if total > 0 else 0
                    logger.info(
                        "[%d%%] Deleted: %d | Failed: %d | Skipped admins: %d",
                        pct,
                        self.deleted,
                        self.failed,
                        self.skipped_admins,
                    )

            batch_queue.task_done()

    def run(self) -> dict:
        """
        Execute bulk deletion pipeline.

        Returns:
            {"deleted": int, "failed": int, "skipped_admins": int,
             "new_refresh_token": str | None}
        """
        logger.info("=== Delete Normal Users: %s ===", self.domain)
        logger.info("Domain: %s | Workers: %d", self.domain, self.workers)

        # Auth
        if not self._get_token():
            logger.error("Authentication failed!")
            return {
                "deleted": 0,
                "failed": 0,
                "skipped_admins": 0,
                "new_refresh_token": None,
            }

        # Get admin users to protect
        logger.info("Fetching admin users to protect...")
        self.admin_emails = self._get_admin_users()
        logger.info(
            "Found %d admin user(s) to protect: %s",
            len(self.admin_emails),
            sorted(self.admin_emails),
        )

        # Fetch app-created users from Redis
        all_users = self._get_redis_users()
        if not all_users:
            logger.info("No users in Redis queue -- nothing to delete.")
            return {
                "deleted": 0,
                "failed": 0,
                "skipped_admins": 0,
                "new_refresh_token": None,
            }

        # Filter out admins as safety net
        users = self._filter_non_admin_users(all_users)
        logger.info(
            "Redis users: %d | Admin (protected): %d | To delete: %d",
            len(all_users),
            self.skipped_admins,
            len(users),
        )

        if not users:
            logger.info("All Redis users are admins -- nothing to delete.")
            return {
                "deleted": 0,
                "failed": 0,
                "skipped_admins": self.skipped_admins,
                "new_refresh_token": None,
            }

        # Confirmation
        if not self.auto_confirm:
            confirm = input(
                f"Delete {len(users):,} users? (yes/no): "
            ).strip().lower()
            if confirm not in ("yes", "y"):
                logger.info("Cancelled by user.")
                return {
                    "deleted": 0,
                    "failed": 0,
                    "skipped_admins": self.skipped_admins,
                    "new_refresh_token": None,
                }

        # Create batches and process
        batches = [users[i : i + BATCH_SIZE] for i in range(0, len(users), BATCH_SIZE)]
        logger.info("Created %d batches of %d", len(batches), BATCH_SIZE)

        batch_queue: queue.Queue = queue.Queue()
        for batch in batches:
            batch_queue.put(batch)

        start_time = time.time()
        threads = []
        for _ in range(self.workers):
            t = threading.Thread(
                target=self._worker, args=(batch_queue, len(users))
            )
            t.start()
            threads.append(t)

        for t in threads:
            t.join()

        elapsed = time.time() - start_time
        rate = self.deleted / elapsed if elapsed > 0 else 0
        logger.info(
            "Delete complete: %d deleted, %d failed in %.1fs (%.1f/s)",
            self.deleted,
            self.failed,
            elapsed,
            rate,
        )

        # Clear Redis queue -- users no longer exist
        if self.deleted > 0 and self.redis_client:
            queue_name = f"redis-users-{self.queue_suffix}"
            try:
                self.redis_client.delete(queue_name)
                logger.info("Cleared Redis queue '%s' after deletion", queue_name)
            except Exception as e:
                logger.warning("Failed to clear Redis queue '%s': %s", queue_name, e)

        return {
            "deleted": self.deleted,
            "failed": self.failed,
            "skipped_admins": self.skipped_admins,
            "new_refresh_token": None,
        }
