"""
Bulk User Creator — Create Microsoft 365 users via Graph Batch API.

Generates `bot_*@<domain>` users, creates them through the `/$batch` endpoint
in chunks of 20 with 2 worker threads, optionally assigns an available license,
and returns `{created_users, failed, licensed}`.
"""
import logging
import queue
import random
import string
import threading
import time
import uuid
from typing import Optional

import requests

from admin_token_manager import AdminTokenManager

logger = logging.getLogger(__name__)

CLIENT_ID = "1950a258-227b-4e31-a9cf-717495945fc2"
GRAPH_URL = "https://graph.microsoft.com/v1.0"

DEFAULT_COUNT = 10000
BATCH_SIZE = 20
WORKERS = 2
DELAY_BETWEEN_BATCHES = 1.5

# License selection
A1_STUDENTS_PART_NUMBER = "STANDARDWOFFPACK_STUDENT"  # Office 365 A1 for students
LICENSE_ALIASES = {"a1-students": A1_STUDENTS_PART_NUMBER}


def _is_guid(value: str) -> bool:
    try:
        uuid.UUID(value)
        return True
    except (ValueError, AttributeError, TypeError):
        return False


def _eligible_skus(skus: list[dict]) -> list[dict]:
    out = []
    for sku in skus:
        enabled = sku.get("prepaidUnits", {}).get("enabled", 0)
        consumed = sku.get("consumedUnits", 0)
        if enabled - consumed > 0:
            out.append(sku)
    return out


def resolve_license_sku(skus: list[dict], preference: Optional[str]) -> Optional[str]:
    """Resolve a license preference to a concrete skuId given subscribed SKUs.

    Semantics:
      - None / "" / "auto" / known alias ("a1-students"): prefer Office 365 A1
        for students (part number STANDARDWOFFPACK_STUDENT); if it has no free
        seat, fall back to the first SKU with a free seat (logged as a warning).
      - raw GUID (skuId): use that exact skuId only if present with a free seat;
        otherwise None (no cross-product fallback — pinning is intentional).
      - any other string: treat as an exact skuPartNumber; use it if it has a
        free seat, otherwise None.
    Only SKUs with ``enabled - consumed > 0`` are eligible.
    """
    eligible = _eligible_skus(skus)
    if not eligible:
        return None

    pref = (preference or "auto").strip()
    is_auto = pref.lower() == "auto" or pref.lower() in LICENSE_ALIASES

    if pref.lower() in LICENSE_ALIASES:
        target_part = LICENSE_ALIASES[pref.lower()]
    elif is_auto:
        target_part = A1_STUDENTS_PART_NUMBER
    elif _is_guid(pref):
        for sku in eligible:
            if sku.get("skuId") == pref:
                return pref
        logger.warning("Pinned license skuId %s has no free seat; not assigning", pref[:8])
        return None
    else:
        target_part = pref  # treat as an exact skuPartNumber

    for sku in eligible:
        if sku.get("skuPartNumber") == target_part:
            return sku.get("skuId")

    if is_auto:
        fallback = eligible[0].get("skuId")
        logger.warning(
            "License %s unavailable; falling back to first available SKU %s (%s)",
            target_part, eligible[0].get("skuPartNumber"), (fallback or "")[:8],
        )
        return fallback

    logger.warning("Pinned license part-number %s has no free seat; not assigning", target_part)
    return None


FIRST_NAMES = [
    "Nguyen", "Tran", "Le", "Pham", "Hoang", "Vu", "Vo", "Dang", "Bui", "Do",
    "Ho", "Ngo", "Duong", "Ly", "An", "Minh", "Duc", "Hieu", "Hung", "Tuan",
    "Nam", "Hai", "Long", "Quang", "Thanh", "Hoa", "Linh", "Mai", "Lan", "Huong",
    "Phuc", "Dat", "Tai", "Loc", "Phong", "Kiet", "Huy", "Khoa", "Dung", "Cuong",
]

LAST_NAMES = [
    "Van", "Thi", "Huu", "Quoc", "Anh", "Bao", "Chi", "Duy", "Gia", "Ha",
    "Khanh", "Lam", "My", "Ngoc", "Phuong", "Quynh", "Son", "Tam", "Uyen", "Xuan",
    "Binh", "Cong", "Dinh", "Giang", "Hong", "Kim", "Linh", "Nhat", "Tien", "Vinh",
]


class BulkUserCreator:
    """Create Microsoft 365 users in bulk via Graph Batch API."""

    def __init__(
        self,
        token_mgr: AdminTokenManager,
        count: int = DEFAULT_COUNT,
        license_sku: Optional[str] = None,
        usage_location: str = "US",
    ):
        self.token_mgr = token_mgr
        self.domain = token_mgr.domain
        self.count = count
        self.license_pref = license_sku  # raw preference: alias / guid / part / "auto"
        self.license_sku: Optional[str] = None  # resolved skuId, set in run()
        self.usage_location = usage_location

        # Results
        self.created_users: list[dict] = []
        self.created_count = 0
        self.licensed_count = 0
        self.failed_count = 0
        self.stats_lock = threading.Lock()

    @staticmethod
    def _generate_password() -> str:
        upper = random.choices(string.ascii_uppercase, k=3)
        lower = random.choices(string.ascii_lowercase, k=4)
        digits = random.choices(string.digits, k=4)
        special = random.choices("!@#$%&*", k=3)
        password = upper + lower + digits + special
        random.shuffle(password)
        return "".join(password)

    def _generate_user_data(self) -> dict:
        # bot_ prefix for easy identification and cleanup
        random_id = "".join(random.choices(string.ascii_lowercase + string.digits, k=8))
        username = f"bot_{random_id}"
        password = self._generate_password()

        return {
            "userPrincipalName": f"{username}@{self.domain}",
            "displayName": f"Bot {random_id}",
            "mailNickname": username,
            "accountEnabled": True,
            "usageLocation": self.usage_location,
            "_password": password,
            "passwordProfile": {
                "password": password,
                "forceChangePasswordNextSignIn": False,
            },
        }

    def _get_token(self) -> Optional[str]:
        return self.token_mgr.get_token()

    def _resolve_license_sku(self) -> Optional[str]:
        token = self._get_token()
        if not token:
            return None
        headers = {"Authorization": f"Bearer {token}"}
        try:
            resp = self.token_mgr.session.get(
                f"{GRAPH_URL}/subscribedSkus", headers=headers, timeout=30
            )
            if resp.status_code == 200:
                return resolve_license_sku(resp.json().get("value", []), self.license_pref)
        except requests.RequestException as e:
            logger.warning("Failed to get licenses: %s", e)
        return None

    def _process_batch(self, users: list[dict], retry: int = 0) -> list[dict]:
        if retry > 3:
            logger.warning(
                "Max retries reached for batch of %d users (first: %s)",
                len(users),
                users[0]["userPrincipalName"] if users else "?",
            )
            return [
                {
                    "email": u["userPrincipalName"],
                    "password": u["_password"],
                    "success": False,
                    "error": "Max retries",
                }
                for u in users
            ]

        token = self._get_token()
        if not token:
            return [
                {
                    "email": u["userPrincipalName"],
                    "password": u["_password"],
                    "success": False,
                    "error": "No token",
                }
                for u in users
            ]

        headers = {
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        }

        requests_list = []
        for i, user in enumerate(users):
            api_data = {k: v for k, v in user.items() if not k.startswith("_")}
            requests_list.append(
                {
                    "id": str(i),
                    "method": "POST",
                    "url": "/users",
                    "headers": {"Content-Type": "application/json"},
                    "body": api_data,
                }
            )

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
                logger.warning("Throttled, waiting %ds...", retry_after)
                time.sleep(min(retry_after, 30))
                return self._process_batch(users, retry + 1)

            if resp.status_code == 200:
                batch_resp = resp.json()
                for response in batch_resp.get("responses", []):
                    idx = int(response["id"])
                    user = users[idx]

                    if response["status"] == 201:
                        results.append(
                            {
                                "email": user["userPrincipalName"],
                                "password": user["_password"],
                                "success": True,
                                "user_id": response["body"].get("id"),
                            }
                        )
                    elif response["status"] == 429:
                        retry_users.append(user)
                    else:
                        error_code = (
                            response.get("body", {})
                            .get("error", {})
                            .get("code", "Unknown")
                        )
                        error_msg = (
                            response.get("body", {})
                            .get("error", {})
                            .get("message", "Unknown")[:120]
                        )
                        logger.warning(
                            "User create failed [%d]: %s - %s (%s)",
                            response["status"],
                            error_code,
                            error_msg,
                            user["userPrincipalName"],
                        )
                        results.append(
                            {
                                "email": user["userPrincipalName"],
                                "password": user["_password"],
                                "success": False,
                                "error": error_msg,
                            }
                        )
            else:
                logger.error("Batch failed: HTTP %d", resp.status_code)
                for user in users:
                    results.append(
                        {
                            "email": user["userPrincipalName"],
                            "password": user["_password"],
                            "success": False,
                            "error": f"HTTP {resp.status_code}",
                        }
                    )
        except requests.RequestException as e:
            logger.error("Batch exception: %s", e)
            for user in users:
                results.append(
                    {
                        "email": user["userPrincipalName"],
                        "password": user["_password"],
                        "success": False,
                        "error": str(e)[:50],
                    }
                )

        if retry_users:
            time.sleep(2**retry)
            results.extend(self._process_batch(retry_users, retry + 1))

        return results

    def _assign_license_batch(self, user_ids: list[str]) -> int:
        if not self.license_sku or not user_ids:
            return 0

        token = self._get_token()
        if not token:
            return 0

        headers = {
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        }
        requests_list = [
            {
                "id": str(i),
                "method": "POST",
                "url": f"/users/{uid}/assignLicense",
                "headers": {"Content-Type": "application/json"},
                "body": {
                    "addLicenses": [
                        {"skuId": self.license_sku, "disabledPlans": []}
                    ],
                    "removeLicenses": [],
                },
            }
            for i, uid in enumerate(user_ids)
        ]

        try:
            resp = self.token_mgr.session.post(
                f"{GRAPH_URL}/$batch",
                headers=headers,
                json={"requests": requests_list},
                timeout=60,
            )
            if resp.status_code == 200:
                return sum(
                    1
                    for r in resp.json().get("responses", [])
                    if r["status"] == 200
                )
        except requests.RequestException:
            pass
        return 0

    def _worker(self, batch_queue: queue.Queue) -> None:
        while True:
            try:
                batch = batch_queue.get(timeout=1)
            except queue.Empty:
                break

            results = self._process_batch(batch)
            time.sleep(DELAY_BETWEEN_BATCHES)

            success_ids = []

            with self.stats_lock:
                for result in results:
                    if result["success"]:
                        self.created_count += 1
                        self.created_users.append(
                            {"email": result["email"], "password": result["password"]}
                        )
                        if result.get("user_id"):
                            success_ids.append(result["user_id"])
                    else:
                        self.failed_count += 1

            # Assign licenses
            if success_ids and self.license_sku:
                licensed = self._assign_license_batch(success_ids)
                with self.stats_lock:
                    self.licensed_count += licensed

            # Progress
            with self.stats_lock:
                done = self.created_count + self.failed_count
                if done % 100 < BATCH_SIZE:
                    pct = done * 100 // self.count
                    logger.info(
                        "[%d%%] Created: %d | Licensed: %d | Failed: %d",
                        pct,
                        self.created_count,
                        self.licensed_count,
                        self.failed_count,
                    )

            batch_queue.task_done()

    def run(self) -> dict:
        """
        Execute bulk user creation pipeline.

        Returns:
            {
                "created_users": [{"email": str, "password": str}, ...],
                "failed": int,
                "licensed": int,
                "new_refresh_token": str | None,
            }
        """
        logger.info("=== Create Users: %s ===", self.domain)

        # Auth
        if not self._get_token():
            logger.error("Authentication failed!")
            return {
                "created_users": [],
                "failed": self.count,
                "licensed": 0,
                "new_refresh_token": None,
            }
        logger.info("Authenticated successfully")

        # Resolve license preference → concrete skuId
        self.license_sku = self._resolve_license_sku()
        if self.license_sku:
            logger.info("License resolved (pref=%s): skuId=%s", self.license_pref, self.license_sku)
        else:
            logger.warning("No license assigned (pref=%s)", self.license_pref)

        # Generate users
        logger.info("Generating %d users...", self.count)
        users = [self._generate_user_data() for _ in range(self.count)]

        # Create batches
        batches = [users[i : i + BATCH_SIZE] for i in range(0, len(users), BATCH_SIZE)]
        logger.info("Created %d batches of %d", len(batches), BATCH_SIZE)

        batch_queue: queue.Queue = queue.Queue()
        for batch in batches:
            batch_queue.put(batch)

        # Workers
        logger.info("Starting %d workers...", WORKERS)
        start_time = time.time()

        threads = []
        for _ in range(WORKERS):
            t = threading.Thread(target=self._worker, args=(batch_queue,))
            t.start()
            threads.append(t)

        for t in threads:
            t.join()

        elapsed = time.time() - start_time
        rate = self.created_count / elapsed if elapsed > 0 else 0
        logger.info(
            "Create complete: %d created, %d licensed, %d failed in %.1fs (%.1f/s)",
            self.created_count,
            self.licensed_count,
            self.failed_count,
            elapsed,
            rate,
        )

        return {
            "created_users": self.created_users,
            "failed": self.failed_count,
            "licensed": self.licensed_count,
            "new_refresh_token": None,
        }
