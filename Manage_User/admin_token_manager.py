"""
Admin Token Manager — Shared OAuth token management for Microsoft Graph API.

Single source of truth for admin access_token and refresh_token rotation.
Thread-safe, used by all modules (creator, deleter, cleanup, app).
"""
import json
import logging
import threading
import time
from pathlib import Path
from typing import Optional

import requests
from requests.adapters import HTTPAdapter

from proxy_config import parse_proxy, proxies_dict
from pool_config import resolve_pool_id

logger = logging.getLogger(__name__)

CLIENT_ID = "1950a258-227b-4e31-a9cf-717495945fc2"
CONFIG_FILE = Path(__file__).resolve().parent.parent / "admin_token_config_global.json"
_CONFIG_SAVE_LOCK = threading.Lock()


class AdminTokenManager:
    """Thread-safe admin OAuth token manager with auto-refresh and rotation tracking."""

    def __init__(
        self,
        admin: dict,
        config_file: Path | None = None,
        config_pool_id: str | None = None,
    ):
        self.tenant_id = admin["tenant_id"]
        self.refresh_token = admin["refresh_token"]
        self.domain = admin.get("domain", "")
        self.proxy_url: Optional[str] = parse_proxy(admin.get("proxy"))
        self.config_file = config_file or CONFIG_FILE
        self.config_pool_id = config_pool_id or admin.get("pool_id") or resolve_pool_id(admin)

        self._access_token: Optional[str] = None
        self._token_expires: float = 0
        self._lock = threading.Lock()

        # Track if refresh_token was rotated by Microsoft
        self._original_refresh_token = self.refresh_token
        self._refresh_token_updated = False

        # Shared session with connection pooling
        self.session = requests.Session()
        adapter = HTTPAdapter(pool_connections=100, pool_maxsize=100, max_retries=3)
        self.session.mount("https://", adapter)
        if self.proxy_url:
            self.session.proxies.update(proxies_dict(self.proxy_url))
            logger.info("AdminTokenManager: SOCKS5 proxy enabled")

    def get_token(self) -> Optional[str]:
        """Get a valid access_token, refreshing if expired. Thread-safe."""
        with self._lock:
            if self._access_token and time.time() < self._token_expires:
                return self._access_token

            token_url = (
                f"https://login.microsoftonline.com/{self.tenant_id}/oauth2/v2.0/token"
            )
            data = {
                "client_id": CLIENT_ID,
                "scope": "https://graph.microsoft.com/.default offline_access",
                "refresh_token": self.refresh_token,
                "grant_type": "refresh_token",
            }

            try:
                resp = self.session.post(token_url, data=data, timeout=30)
                if resp.status_code == 200:
                    token_data = resp.json()
                    self._access_token = token_data["access_token"]
                    self._token_expires = (
                        time.time() + token_data.get("expires_in", 3600) - 300
                    )
                    if "refresh_token" in token_data:
                        self.refresh_token = token_data["refresh_token"]
                        self._refresh_token_updated = True
                        self.save_if_updated()
                    return self._access_token
                else:
                    logger.error("Admin token refresh failed: %s", resp.text[:200])
            except requests.RequestException as e:
                logger.error("Admin token request failed: %s", e)
            return None

    def get_headers(self) -> dict:
        """Get Authorization headers with current access_token."""
        token = self.get_token()
        if not token:
            return {}
        return {"Authorization": f"Bearer {token}"}

    @property
    def was_refresh_token_updated(self) -> bool:
        """Check if Microsoft rotated the refresh_token."""
        return self._refresh_token_updated

    def save_if_updated(self) -> None:
        """Save updated refresh_token to the matching global config pool."""
        if not self._refresh_token_updated:
            return

        try:
            with _CONFIG_SAVE_LOCK:
                with open(self.config_file, "r", encoding="utf-8") as f:
                    admins = json.load(f)

                if not isinstance(admins, list):
                    raise ValueError(f"{self.config_file} must be a JSON array")

                updated = False
                for admin in admins:
                    if not isinstance(admin, dict):
                        continue
                    try:
                        pool_id = resolve_pool_id(admin)
                    except ValueError:
                        continue
                    if pool_id != self.config_pool_id:
                        continue
                    admin["refresh_token"] = self.refresh_token
                    updated = True
                    break

                if not updated:
                    logger.error(
                        "Failed to save refresh_token: pool_id %s not found in %s",
                        self.config_pool_id,
                        self.config_file,
                    )
                    return

                tmp_file = self.config_file.with_suffix(self.config_file.suffix + ".tmp")
                with open(tmp_file, "w", encoding="utf-8") as f:
                    json.dump(admins, f, indent=4, ensure_ascii=False)
                    f.write("\n")
                tmp_file.replace(self.config_file)
                self._refresh_token_updated = False
                self._original_refresh_token = self.refresh_token
                logger.info(
                    "Saved updated refresh_token for pool %s to %s",
                    self.config_pool_id,
                    self.config_file.name,
                )
        except Exception as e:
            logger.error("Failed to save refresh_token: %s", e)
