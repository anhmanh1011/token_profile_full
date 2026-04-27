"""
Proxy configuration parser.

Reads the `proxy` field from admin_token.json and converts the legacy
`host:port:user:pass` 4-tuple format into a SOCKS5 URL usable by both
`requests` (via PySocks) and `curl_cffi` (via libcurl).

`socks5h://` is used so DNS resolution happens on the proxy side — this
prevents DNS leaks and keeps geolocation consistent for Microsoft logins.
"""
from typing import Optional
from urllib.parse import quote


def parse_proxy(raw: Optional[str]) -> Optional[str]:
    """Convert `host:port` or `host:port:user:pass` to `socks5h://[user:pass@]host:port`.

    Already-formed URLs (containing `://`) are returned as-is, allowing the
    field to carry an explicit `socks5h://` / `http://` value if a future
    admin entry needs one. Returns None for empty input or malformed values.
    """
    if not raw:
        return None
    raw = raw.strip()
    if not raw:
        return None
    if "://" in raw:
        return raw

    parts = raw.split(":")
    if len(parts) == 2:
        host, port = parts
        return f"socks5h://{host}:{port}"
    if len(parts) == 4:
        host, port, user, pwd = parts
        return f"socks5h://{quote(user, safe='')}:{quote(pwd, safe='')}@{host}:{port}"
    return None


def proxies_dict(socks5_url: Optional[str]) -> dict:
    """Build the `proxies` dict for `requests.Session` / `curl_cffi`."""
    if not socks5_url:
        return {}
    return {"http": socks5_url, "https": socks5_url}
