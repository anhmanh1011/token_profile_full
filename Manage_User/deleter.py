"""
Bulk User Deleter — Delete Microsoft 365 users via Graph Batch API.

Used by the HTTP service (`app.py`) to soft-delete a caller-supplied list of
users. The caller is responsible for chunking into groups of at most 20 emails
(the Graph `/$batch` per-request limit).
"""
import logging
import time
from typing import Optional

import requests

from admin_token_manager import AdminTokenManager

logger = logging.getLogger(__name__)

GRAPH_URL = "https://graph.microsoft.com/v1.0"
BATCH_SIZE = 20
MAX_RETRIES = 3


class FastBulkDeleter:
    """Soft-delete Microsoft 365 users via Graph `/$batch`.

    Exposes `_process_batch` for direct use by the HTTP service; no admin-role
    discovery, no Redis, no interactive prompts — those belonged to the legacy
    standalone pipeline and have been removed.
    """

    def __init__(self, token_mgr: AdminTokenManager):
        self.token_mgr = token_mgr

    def _get_token(self) -> Optional[str]:
        return self.token_mgr.get_token()

    def _process_batch(self, emails: list[str], retry: int = 0) -> list[dict]:
        """Soft-delete up to BATCH_SIZE users in one Graph `/$batch` request.

        Args:
            emails: User principal names. Length must be <= BATCH_SIZE (20).
            retry:  Internal retry counter for 429 throttling.

        Returns:
            One result dict per input email: {"email", "success", ["error"]}.
        """
        if retry > MAX_RETRIES:
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

        results: list[dict] = []
        retry_emails: list[str] = []

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
                    "Batch throttled (429), retry %d/%d, waiting %ds...",
                    retry + 1, MAX_RETRIES, retry_after,
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
            results.extend(self._process_batch(retry_emails, retry + 1))

        return results
