"""
Startup Cleanup — List and delete all bot_ prefix users from a Microsoft 365 tenant.

Called at service startup to remove orphaned bot_ users from previous runs
(crash recovery). The bot_ prefix in userPrincipalName is the source of truth —
any user with that prefix was created by this application.
"""
import logging
import time
from typing import Optional

import requests

from admin_token_manager import AdminTokenManager

logger = logging.getLogger(__name__)

GRAPH_URL = "https://graph.microsoft.com/v1.0"
BATCH_SIZE = 20
BOT_PREFIX = "bot_"


class StartupCleaner:
    """List and delete all bot_ prefix users from a Microsoft 365 tenant.

    Uses the bot_ userPrincipalName prefix as the sole source of truth.
    Designed for startup crash recovery — no Redis dependency.
    """

    def __init__(self, token_mgr: AdminTokenManager) -> None:
        self.token_mgr = token_mgr
        self.domain = token_mgr.domain

        # Stats
        self.found = 0
        self.deleted = 0
        self.failed = 0

    def _get_token(self) -> Optional[str]:
        """Delegate token retrieval to the shared AdminTokenManager."""
        return self.token_mgr.get_token()

    def _list_bot_users(self) -> list[str]:
        """List all bot_ prefix users via Graph API with pagination.

        Returns:
            List of userPrincipalName strings for all bot_ users found.
        """
        token = self._get_token()
        if not token:
            logger.error("Cannot get token to list bot users")
            return []

        emails: list[str] = []
        url = (
            f"{GRAPH_URL}/users"
            f"?$filter=startsWith(userPrincipalName,'{BOT_PREFIX}')"
            f"&$select=userPrincipalName"
            f"&$top=999"
        )
        headers = {"Authorization": f"Bearer {token}"}

        while url:
            try:
                resp = self.token_mgr.session.get(url, headers=headers, timeout=30)

                if resp.status_code == 429:
                    retry_after = int(resp.headers.get("Retry-After", 5))
                    logger.warning(
                        "List throttled (429), waiting %ds...", retry_after
                    )
                    time.sleep(min(retry_after, 30))
                    # Refresh token in case it expired during the wait
                    token = self._get_token() or token
                    headers = {"Authorization": f"Bearer {token}"}
                    continue

                if resp.status_code != 200:
                    logger.error(
                        "Failed to list bot users: HTTP %d - %s",
                        resp.status_code,
                        resp.text[:200],
                    )
                    break

                page = resp.json()
                page_users = page.get("value", [])
                emails.extend(u["userPrincipalName"] for u in page_users)
                logger.debug("Page fetched: %d users (total so far: %d)", len(page_users), len(emails))

                url = page.get("@odata.nextLink")

            except requests.RequestException as e:
                logger.error("Error listing bot users: %s", e)
                break

        return emails

    def _batch_delete(self, emails: list[str], retry: int = 0) -> list[dict]:
        """Delete users via Graph Batch API (POST /$batch, 20 per batch).

        Args:
            emails: List of userPrincipalName strings to delete.
            retry:  Current retry count (max 3).

        Returns:
            List of result dicts with keys: email, success, error (on failure).
        """
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

        results: list[dict] = []
        retry_emails: list[str] = []

        try:
            resp = self.token_mgr.session.post(
                f"{GRAPH_URL}/$batch",
                headers=headers,
                json={"requests": requests_list},
                timeout=60,
            )

            if resp.status_code == 429:
                retry_after = int(resp.headers.get("Retry-After", 5))
                logger.warning(
                    "Batch throttled (429), retry %d/3, waiting %ds...",
                    retry + 1,
                    retry_after,
                )
                time.sleep(min(retry_after, 30))
                return self._batch_delete(emails, retry + 1)

            if resp.status_code == 200:
                for response in resp.json().get("responses", []):
                    idx = int(response["id"])
                    email = emails[idx]

                    if response["status"] == 204:
                        results.append({"email": email, "success": True})
                    elif response["status"] == 429:
                        retry_emails.append(email)
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
                            "Delete failed [%d]: %s - %s (%s)",
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
                    "Batch request failed: HTTP %d - %s",
                    resp.status_code,
                    resp.text[:200],
                )
                results.extend(
                    {
                        "email": e,
                        "success": False,
                        "error": f"Batch failed: {resp.status_code}",
                    }
                    for e in emails
                )

        except requests.RequestException as e:
            logger.error("Batch exception: %s", e)
            results.extend(
                {"email": em, "success": False, "error": str(e)[:50]}
                for em in emails
            )

        if retry_emails:
            time.sleep(2 ** retry)
            results.extend(self._batch_delete(retry_emails, retry + 1))

        return results

    def run(self) -> dict:
        """Orchestrate startup cleanup: list bot_ users, batch delete, return stats.

        Returns:
            {"found": int, "deleted": int, "failed": int}
        """
        logger.info("=== Startup Cleanup: removing %s* users from %s ===", BOT_PREFIX, self.domain)

        if not self._get_token():
            logger.error("Authentication failed — skipping startup cleanup")
            return {"found": 0, "deleted": 0, "failed": 0}

        logger.info("Listing all %s* users...", BOT_PREFIX)
        emails = self._list_bot_users()
        self.found = len(emails)
        logger.info("Found %d bot_ user(s) to clean up", self.found)

        if not emails:
            return {"found": 0, "deleted": 0, "failed": 0}

        # Process in BATCH_SIZE chunks
        batches = [
            emails[i : i + BATCH_SIZE] for i in range(0, len(emails), BATCH_SIZE)
        ]
        logger.info("Deleting in %d batch(es) of up to %d...", len(batches), BATCH_SIZE)

        start_time = time.time()

        for batch_num, batch in enumerate(batches, start=1):
            results = self._batch_delete(batch)
            for result in results:
                if result["success"]:
                    self.deleted += 1
                else:
                    self.failed += 1

            logger.debug(
                "[%d/%d] Running totals — deleted: %d | failed: %d",
                batch_num,
                len(batches),
                self.deleted,
                self.failed,
            )

        elapsed = time.time() - start_time
        logger.info(
            "Soft-delete complete: %d found, %d deleted, %d failed in %.1fs",
            self.found,
            self.deleted,
            self.failed,
            elapsed,
        )

        # Permanently delete from recycle bin
        purged = self._purge_deleted_bot_users()
        logger.info("Permanently purged %d users from recycle bin", purged)

        return {"found": self.found, "deleted": self.deleted, "failed": self.failed, "purged": purged}

    def _list_deleted_bot_users(self) -> list[dict]:
        """List bot_ users in the recycle bin (deletedItems).

        Returns:
            List of {"id": str, "upn": str} for permanently deleting.
        """
        token = self._get_token()
        if not token:
            return []

        headers = {"Authorization": f"Bearer {token}"}
        url = (
            f"{GRAPH_URL}/directory/deletedItems/microsoft.graph.user"
            f"?$select=id,userPrincipalName&$top=999"
        )
        deleted_users: list[dict] = []

        while url:
            try:
                resp = self.token_mgr.session.get(url, headers=headers, timeout=30)
                if resp.status_code == 429:
                    retry_after = int(resp.headers.get("Retry-After", 5))
                    time.sleep(min(retry_after, 30))
                    token = self._get_token() or token
                    headers = {"Authorization": f"Bearer {token}"}
                    continue
                if resp.status_code != 200:
                    logger.error("List deleted users failed: %d", resp.status_code)
                    break

                page = resp.json()
                for user in page.get("value", []):
                    upn = user.get("userPrincipalName", "")
                    if upn.startswith(BOT_PREFIX):
                        deleted_users.append({"id": user["id"], "upn": upn})

                url = page.get("@odata.nextLink")
            except requests.RequestException as e:
                logger.error("Error listing deleted users: %s", e)
                break

        return deleted_users

    def _purge_deleted_bot_users(self) -> int:
        """Permanently delete all bot_ users from recycle bin via batch API.

        Returns:
            Number of users permanently purged.
        """
        deleted_users = self._list_deleted_bot_users()
        if not deleted_users:
            logger.info("No bot_ users in recycle bin to purge")
            return 0

        logger.info("Found %d bot_ users in recycle bin, purging...", len(deleted_users))

        purged = 0
        batches = [
            deleted_users[i : i + BATCH_SIZE]
            for i in range(0, len(deleted_users), BATCH_SIZE)
        ]

        for batch in batches:
            token = self._get_token()
            if not token:
                break

            headers = {
                "Authorization": f"Bearer {token}",
                "Content-Type": "application/json",
            }
            requests_list = [
                {"id": str(i), "method": "DELETE", "url": f"/directory/deletedItems/{u['id']}"}
                for i, u in enumerate(batch)
            ]

            try:
                resp = self.token_mgr.session.post(
                    f"{GRAPH_URL}/$batch",
                    headers=headers,
                    json={"requests": requests_list},
                    timeout=60,
                )
                if resp.status_code == 429:
                    retry_after = int(resp.headers.get("Retry-After", 5))
                    time.sleep(min(retry_after, 30))
                    continue
                if resp.status_code == 200:
                    for r in resp.json().get("responses", []):
                        if r["status"] == 204:
                            purged += 1
                        else:
                            logger.debug("Purge failed [%d]: %s", r["status"],
                                         r.get("body", {}).get("error", {}).get("code", ""))
            except requests.RequestException as e:
                logger.error("Purge batch error: %s", e)

            time.sleep(1)

        return purged
