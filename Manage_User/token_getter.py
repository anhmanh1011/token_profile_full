"""
Bulk Token Getter — Obtain Microsoft Graph refresh tokens via Teams browser flow.

`TeamOutLook` drives the Teams MSAL flow with curl_cffi Firefox impersonation
(authorize → login → optional forced password change → KMSI → code exchange)
and stores the resulting refresh_token + tenant_id. `BulkTokenGetter` runs a
pool of 30 worker threads over a user list and returns `{tokens, failed}`.

Go side exchanges refresh_token → Loki-scoped access_token lazily per worker
to take advantage of the ~24h refresh_token TTL (vs ~1h access_token TTL).
"""
import json
import logging
import os
import queue
import re
import signal
import sys
import threading
import time
from pathlib import Path
from typing import Optional

from curl_cffi import requests

from proxy_config import parse_proxy, proxies_dict

logger = logging.getLogger(__name__)

DEFAULT_WORKERS = 30


class TeamOutLook:
    """Browser-flow login to Microsoft Teams to obtain refresh tokens."""

    def __init__(self, email: str, password: str, proxy: Optional[str] = None):
        self.mail = email
        self.pwd = password
        self.newpwd = self.pwd + "1"
        self.session = requests.Session(impersonate="firefox135", timeout=60)
        self.tenant_id = ""

        # Accept either the legacy host:port:user:pass form or a full URL.
        # parse_proxy returns a socks5h:// URL ready for libcurl.
        self.proxy_url: Optional[str] = parse_proxy(proxy)
        self.data_proxy: dict = proxies_dict(self.proxy_url)
        if self.data_proxy:
            self.session.proxies.update(self.data_proxy)

        # Results
        self.refresh_token: Optional[str] = None
        self.final_password: Optional[str] = None

    def clear_cookies(self) -> None:
        self.session = requests.Session(impersonate="chrome131", timeout=60)
        if self.data_proxy:
            self.session.proxies.update(self.data_proxy)

    def authorize_common(self) -> Optional[str]:
        try:
            headers = {
                "accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
                "accept-language": "vi-VN,vi;q=0.9",
                "priority": "u=0, i",
                "sec-ch-ua": '"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"',
                "sec-ch-ua-mobile": "?0",
                "sec-ch-ua-platform": '"Windows"',
                "sec-fetch-dest": "document",
                "sec-fetch-mode": "navigate",
                "sec-fetch-site": "none",
                "sec-fetch-user": "?1",
                "upgrade-insecure-requests": "1",
                "user-agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
            }

            params = {
                "client_id": "5e3ce6c0-2b1f-4285-8d4b-75ee78787346",
                "scope": "openId profile openid offline_access",
                "redirect_uri": "https://teams.microsoft.com/v2",
                "client-request-id": "019a2b63-12e6-751c-9549-6d43e8b3f31a",
                "response_mode": "fragment",
                "response_type": "code",
                "x-client-SKU": "msal.js.browser",
                "x-client-VER": "3.30.0",
                "client_info": "1",
                "code_challenge": "Io4JEa0ouaqyhy23EhsWfW8ukRmBhwyx-Lx9YOUvlII",
                "code_challenge_method": "S256",
                "nonce": "019a2b63-12e6-7bdd-9e0c-ffd0d81dfa23",
                "state": "eyJpZCI6IjAxOWEyYjYzLTEyZTYtNzA1Ni04NWQ4LTllNmQ5YTc2OGNiMCIsIm1ldGEiOnsiaW50ZXJhY3Rpb25UeXBlIjoicmVkaXJlY3QifX0=|https://teams.microsoft.com/v2/?culture=vi-vn&country=vn&enablemcasfort21=true",
                "sso_reload": "true",
            }

            response = self.session.get(
                "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
                params=params,
                headers=headers,
            )
            return response.text

        except Exception as e:
            logger.debug("%s - Error Authorize: %s", self.mail, e)
            return None

    def login_common(self, response: str) -> Optional[str]:
        try:
            sFT = re.findall(r'"sFT":"(.*?)"', response)[0]
            sCtx = re.findall(r'"sCtx":"(.*?)"', response)[0]
            canary = bytes(
                re.findall(r'"canary":"(.*?)"', response)[0], "utf-8"
            ).decode("unicode-escape")
        except (IndexError, UnicodeDecodeError):
            logger.debug("%s - Not Found Params In Login Common", self.mail)
            return None

        headers = {
            "accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
            "accept-language": "vi-VN,vi;q=0.9",
            "cache-control": "max-age=0",
            "content-type": "application/x-www-form-urlencoded",
            "origin": "https://login.microsoftonline.com",
            "priority": "u=0, i",
            "referer": "https://login.microsoftonline.com/common/oauth2/v2.0/authorize?client_id=5e3ce6c0-2b1f-4285-8d4b-75ee78787346&scope=openId%20profile%20openid%20offline_access&redirect_uri=https%3A%2F%2Fteams.microsoft.com%2Fv2&client-request-id=019a2b63-12e6-751c-9549-6d43e8b3f31a&response_mode=fragment&response_type=code&x-client-SKU=msal.js.browser&x-client-VER=3.30.0&client_info=1&code_challenge=D4kIaiwH89KuodNjLbswl4azyJfVvYFJjLITCkcW0Cc&code_challenge_method=S256&nonce=019a2b63-12e6-7bdd-9e0c-ffd0d81dfa23&state=eyJpZCI6IjAxOWEyYjYzLTEyZTYtNzA1Ni04NWQ4LTllNmQ5YTc2OGNiMCIsIm1ldGEiOnsiaW50ZXJhY3Rpb25UeXBlIjoicmVkaXJlY3QifX0%3D%7Chttps%3A%2F%2Fteams.microsoft.com%2Fv2%2F%3Fculture%3Dvi-vn%26country%3Dvn%26enablemcasfort21%3Dtrue&sso_reload=true",
            "sec-ch-ua": '"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"',
            "sec-ch-ua-mobile": "?0",
            "sec-ch-ua-platform": '"Windows"',
            "sec-fetch-dest": "document",
            "sec-fetch-mode": "navigate",
            "sec-fetch-site": "same-origin",
            "sec-fetch-user": "?1",
            "upgrade-insecure-requests": "1",
            "user-agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
        }

        data = {
            "i13": "0",
            "login": self.mail,
            "loginfmt": self.mail,
            "type": "11",
            "LoginOptions": "3",
            "lrt": "",
            "lrtPartition": "",
            "hisRegion": "",
            "hisScaleUnit": "",
            "passwd": self.pwd,
            "ps": "2",
            "psRNGCDefaultType": "",
            "psRNGCEntropy": "",
            "psRNGCSLK": "",
            "canary": canary,
            "ctx": sCtx,
            "hpgrequestid": "de0904a5-1b6a-423c-9fcc-4f52f15e8700",
            "flowToken": sFT,
            "PPSX": "",
            "NewUser": "1",
            "FoundMSAs": "",
            "fspost": "0",
            "i21": "0",
            "CookieDisclosure": "0",
            "IsFidoSupported": "1",
            "isSignupPost": "0",
            "DfpArtifact": "",
            "i19": "20095",
        }

        response = self.session.post(
            "https://login.microsoftonline.com/common/login?sso_reload=true",
            headers=headers,
            data=data,
            allow_redirects=False,
        )

        try:
            self.tenant_id = re.findall(
                r'tenant=(.*?)"', response.headers.get("reporting-endpoints", "")
            )[0]
        except IndexError:
            logger.debug("%s - Could not extract tenant_id", self.mail)

        return response.text

    def converged_change_password(self, response: str) -> bool:
        logger.info("%s - Changing password", self.mail)
        try:
            sFT = re.findall(r'"sFT":"(.*?)"', response)[0]
            sCtx = re.findall(r'"sCtx":"(.*?)"', response)[0]
            canary = bytes(
                re.findall(r'"canary":"(.*?)"', response)[0], "utf-8"
            ).decode("unicode-escape")
        except (IndexError, UnicodeDecodeError):
            logger.debug(
                "%s - Not Found Params In Converged Change Password", self.mail
            )
            return False

        try:
            headers = {
                "accept": "application/json",
                "accept-language": "vi-VN,vi;q=0.9",
                "client-request-id": "019a2b63-12e6-751c-9549-6d43e8b3f31a",
                "content-type": "application/json; charset=UTF-8",
                "hpgact": "2000",
                "hpgid": "1116",
                "hpgrequestid": "e9537b48-02b0-43cf-a6c4-9c2fe1650800",
                "origin": "https://login.microsoftonline.com",
                "priority": "u=1, i",
                "referer": "https://login.microsoftonline.com/common/login?sso_reload=true",
                "sec-ch-ua": '"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"',
                "sec-ch-ua-mobile": "?0",
                "sec-ch-ua-platform": '"Windows"',
                "sec-fetch-dest": "empty",
                "sec-fetch-mode": "cors",
                "sec-fetch-site": "same-origin",
                "user-agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
            }

            # Begin password change
            json_data = {
                "FlowToken": sFT,
                "Ctx": sCtx,
                "OldPassword": self.pwd,
                "NewPassword": self.newpwd,
            }

            response = self.session.post(
                "https://login.microsoftonline.com/common/SSPR/Begin",
                headers=headers,
                json=json_data,
            )
            sFT = response.json()["FlowToken"]
            sCtx = response.json()["Ctx"]

            # Poll
            json_data = {
                "FlowToken": sFT,
                "Ctx": sCtx,
                "CoupledDataCenter": "PS1P",
                "CoupledScaleUnit": "a",
            }

            response = self.session.post(
                "https://login.microsoftonline.com/common/SSPR/Poll",
                headers=headers,
                json=json_data,
            )
            sFT = response.json()["FlowToken"]
            sCtx = response.json()["Ctx"]

            # End password change
            end_headers = {
                "accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
                "accept-language": "vi-VN,vi;q=0.9",
                "cache-control": "max-age=0",
                "content-type": "application/x-www-form-urlencoded",
                "origin": "https://login.microsoftonline.com",
                "priority": "u=0, i",
                "referer": "https://login.microsoftonline.com/common/login?sso_reload=true",
                "sec-ch-ua": '"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"',
                "sec-ch-ua-mobile": "?0",
                "sec-ch-ua-platform": '"Windows"',
                "sec-fetch-dest": "document",
                "sec-fetch-mode": "navigate",
                "sec-fetch-site": "same-origin",
                "sec-fetch-user": "?1",
                "upgrade-insecure-requests": "1",
                "user-agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
            }

            data = {
                "ctx": sCtx,
                "hpgrequestid": "e9537b48-02b0-43cf-a6c4-9c2fe1650800",
                "flowToken": sFT,
                "currentpasswd": self.pwd,
                "newpasswd": self.newpwd,
                "confirmnewpasswd": self.newpwd,
                "canary": canary,
                "i19": "24498",
            }

            self.session.post(
                "https://login.microsoftonline.com/common/SSPR/End",
                headers=end_headers,
                data=data,
            )

            self.pwd = self.newpwd
            self.final_password = self.newpwd
            logger.info("%s - Password changed", self.mail)
            time.sleep(5)

        except Exception as e:
            logger.debug("%s - Error change password: %s", self.mail, e)
            return False

        return True

    def login_kmsi(self, response: str) -> Optional[str]:
        try:
            sFT = re.findall(r'"sFT":"(.*?)"', response)[0]
            sCtx = re.findall(r'"sCtx":"(.*?)"', response)[0]
            canary = bytes(
                re.findall(r'"canary":"(.*?)"', response)[0], "utf-8"
            ).decode("unicode-escape")
        except (IndexError, UnicodeDecodeError):
            logger.debug("%s - Not Found Params In Login KMSI", self.mail)
            return None

        headers = {
            "accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
            "accept-language": "vi-VN,vi;q=0.9",
            "cache-control": "max-age=0",
            "content-type": "application/x-www-form-urlencoded",
            "origin": "https://login.microsoftonline.com",
            "Host": "login.microsoftonline.com",
            "Referer": "https://login.microsoftonline.com/common/login?sso_reload=true",
            "priority": "u=0, i",
            "sec-ch-ua": '"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"',
            "sec-ch-ua-mobile": "?0",
            "sec-ch-ua-platform": '"Windows"',
            "sec-fetch-dest": "document",
            "sec-fetch-mode": "navigate",
            "sec-fetch-site": "same-origin",
            "sec-fetch-user": "?1",
            "upgrade-insecure-requests": "1",
            "user-agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
        }

        data = {
            "LoginOptions": "1",
            "type": "28",
            "ctx": sCtx,
            "hpgrequestid": "61ae3a18-a16a-43a9-8d7d-88a726a32700",
            "flowToken": sFT,
            "canary": canary,
            "i19": "1720006",
        }

        response = self.session.post(
            "https://login.microsoftonline.com/kmsi",
            headers=headers,
            data=data,
        )

        try:
            post_params = json.loads(
                re.findall(
                    r'"oPostParams":(.*?),"iMaxStackForKnockoutAsyncComponents"',
                    response.text,
                )[0]
            )
            url_post = re.findall(r'"urlPost":"(.*?)"', response.text)[0].replace(
                "\\u0026", "&"
            )
        except (IndexError, json.JSONDecodeError):
            logger.debug("%s - Not Found Params In Post KMSI", self.mail)
            return None

        response = self.session.post(
            "https://login.microsoftonline.com" + url_post,
            headers=headers,
            data=post_params,
            allow_redirects=False,
        )

        if "Object moved" not in response.text:
            logger.debug("%s - Wrong password or auth error", self.mail)
            return None

        try:
            code = re.findall(r"code=(.*?)&", response.headers.get("location", ""))[0]
        except IndexError:
            logger.debug("%s - Could not extract auth code", self.mail)
            return None

        return code

    def get_refresh_token(self, code: str) -> bool:
        headers = {
            "accept": "*/*",
            "accept-language": "vi-VN,vi;q=0.9",
            "content-type": "application/x-www-form-urlencoded;charset=utf-8",
            "origin": "https://teams.microsoft.com",
            "priority": "u=1, i",
            "referer": "https://teams.microsoft.com/",
            "sec-ch-ua": '"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"',
            "sec-ch-ua-mobile": "?0",
            "sec-ch-ua-platform": '"Windows"',
            "sec-fetch-dest": "empty",
            "sec-fetch-mode": "cors",
            "sec-fetch-site": "cross-site",
            "user-agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
        }

        params = {
            "client-request-id": "019a9d34-4a19-734e-b5de-edf940f3dd53",
        }

        data = (
            f"client_id=5e3ce6c0-2b1f-4285-8d4b-75ee78787346"
            f"&redirect_uri=https%3A%2F%2Fteams.microsoft.com%2Fv2"
            f"&scope=openId%20profile%20openid%20offline_access"
            f"&code={code}"
            f"&x-client-SKU=msal.js.browser&x-client-VER=3.30.0"
            f"&x-ms-lib-capability=retry-after, h429"
            f"&x-client-current-telemetry=5|865,0,,,|,"
            f"&x-client-last-telemetry=5|0|863,Core-2e7cf48a-7c96-48cc-a60a-db7619709e55|login_required|1,0"
            f"&code_verifier=_jGDmd391UKSoXF8aeTSAEPmlmTpPuFM_eRuv0q_V9Y"
            f"&grant_type=authorization_code&client_info=1"
            f"&X-AnchorMailbox=Oid%3A106ad7cc-d337-4db3-a450-qweqweqwewqewqeqweqwe%401b22c983-1cd9-4e21-9dab-be096b10f075"
        )

        response = self.session.post(
            "https://login.microsoftonline.com/common/oauth2/v2.0/token",
            params=params,
            headers=headers,
            data=data,
        )

        if "refresh_token" not in response.text:
            logger.debug("%s - No refresh_token in response", self.mail)
            return False

        self.refresh_token = response.json()["refresh_token"]
        logger.info("%s - Refresh token obtained (tenant: %s)", self.mail, self.tenant_id)
        return True

    def do_task(self) -> bool:
        """Execute full browser flow: authorize → login → change pwd → get refresh token."""
        try:
            res_authorize = self.authorize_common()
            if not res_authorize:
                return False

            res_login = self.login_common(res_authorize)
            if not res_login:
                return False

            if "ConvergedChangePassword" in res_login:
                if not self.converged_change_password(res_login):
                    return False

            self.clear_cookies()

            res_authorize = self.authorize_common()
            if not res_authorize:
                return False

            res_login = self.login_common(res_authorize)
            if not res_login:
                return False

            code_auth = self.login_kmsi(res_login)
            if not code_auth:
                return False

            return self.get_refresh_token(code_auth)

        except Exception as e:
            logger.debug("%s - Error: %s", self.mail, e)
            return False


class BulkTokenGetter:
    """Get refresh tokens for a list of users using browser flow."""

    def __init__(
        self,
        users: list[dict],
        workers: int = DEFAULT_WORKERS,
        proxy: Optional[str] = None,
    ):
        """
        Args:
            users: List of {"email": str, "password": str}
            workers: Number of concurrent threads
            proxy: Optional proxy string
        """
        self.users = users
        self.workers = workers
        self.proxy = proxy

        # Results
        self.tokens: list[dict] = []
        self.success_count = 0
        self.failed_count = 0
        self.stats_lock = threading.Lock()

    def _worker(self, task_queue: queue.Queue) -> None:
        while True:
            try:
                user = task_queue.get(timeout=1)
            except queue.Empty:
                return

            try:
                obj = TeamOutLook(user["email"], user["password"], self.proxy)
                if obj.do_task():
                    with self.stats_lock:
                        self.success_count += 1
                        self.tokens.append(
                            {
                                "email": user["email"],
                                "refresh_token": obj.refresh_token,
                                "tenant_id": obj.tenant_id,
                            }
                        )
                else:
                    with self.stats_lock:
                        self.failed_count += 1
            except Exception as e:
                logger.error("Worker error for %s: %s", user["email"], e)
                with self.stats_lock:
                    self.failed_count += 1
            finally:
                task_queue.task_done()

                # Progress log
                with self.stats_lock:
                    done = self.success_count + self.failed_count
                    if done % 50 == 0 or done == len(self.users):
                        logger.info(
                            "Token progress: %d/%d (success: %d, failed: %d)",
                            done,
                            len(self.users),
                            self.success_count,
                            self.failed_count,
                        )

    def run(self) -> dict:
        """
        Execute bulk refresh token retrieval.

        Returns:
            {
                "tokens": [{"email": str, "refresh_token": str, "tenant_id": str}, ...],
                "failed": int,
            }
        """
        if not self.users:
            logger.warning("No users to process for token retrieval")
            return {"tokens": [], "failed": 0}

        logger.info(
            "=== Get Tokens: %d users, %d workers ===",
            len(self.users),
            self.workers,
        )

        task_queue: queue.Queue = queue.Queue()
        for user in self.users:
            task_queue.put(user)

        start_time = time.time()

        threads = []
        for _ in range(min(self.workers, len(self.users))):
            t = threading.Thread(target=self._worker, args=(task_queue,), daemon=True)
            t.start()
            threads.append(t)

        # Wait for completion
        task_queue.join()

        elapsed = time.time() - start_time
        rate = self.success_count / elapsed if elapsed > 0 else 0
        logger.info(
            "Token complete: %d success, %d failed in %.1fs (%.1f/s)",
            self.success_count,
            self.failed_count,
            elapsed,
            rate,
        )

        return {
            "tokens": self.tokens,
            "failed": self.failed_count,
        }
