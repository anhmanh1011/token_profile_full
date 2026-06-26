"""
Outbound source-IP (callout IP) binding.

On a VPS with multiple public addresses (e.g. one IPv4 and one IPv6), each
tenant instance is pinned to a single egress IP so Microsoft/Graph sees a
stable, per-tenant source address. This module turns the configured
``local_ip`` value into the two artefacts the codebase needs:

* ``SourceAddressAdapter`` — a ``requests`` transport adapter that binds every
  socket opened by an ``AdminTokenManager`` session to ``local_ip``.
* ``curl_interface`` — the value passed to ``curl_cffi``'s ``interface`` option
  so the Teams browser flow (libcurl) binds to the same IP.

Binding happens at the socket layer (``SO_BINDTODEVICE``-free; it uses the
source-address form of ``bind()``), so it only affects *direct* connections —
which is exactly the deployment model here: each tenant dials out directly from
its own VPS IP, no external proxy.
"""
from __future__ import annotations

import ipaddress
import logging
from typing import Optional

from requests.adapters import HTTPAdapter

logger = logging.getLogger(__name__)


def parse_local_ip(raw: Optional[str]) -> Optional[str]:
    """Validate and normalise a callout IP.

    Accepts an IPv4 or IPv6 literal. Returns the normalised string form, or
    ``None`` for empty input. A non-empty but malformed value logs a warning
    and returns ``None`` so the caller degrades to the OS default route rather
    than crashing the whole tenant on a typo.
    """
    if not raw:
        return None
    raw = raw.strip()
    if not raw:
        return None
    try:
        return str(ipaddress.ip_address(raw))
    except ValueError:
        logger.warning("source_ip: ignoring invalid local_ip %r", raw)
        return None


def curl_interface(local_ip: Optional[str]) -> Optional[str]:
    """Return the ``interface`` value for curl_cffi, or ``None``.

    libcurl's ``CURLOPT_INTERFACE`` accepts an interface name, host name, or IP
    address. We force the IP-address interpretation with the ``host!`` prefix so
    libcurl never mistakes a numeric-looking value for an interface name and
    never performs a name lookup.
    """
    ip = parse_local_ip(local_ip)
    if ip is None:
        return None
    return f"host!{ip}"


class SourceAddressAdapter(HTTPAdapter):
    """``HTTPAdapter`` that binds outgoing sockets to a fixed source IP.

    urllib3 threads ``source_address=(host, 0)`` down to ``socket.bind`` before
    ``connect``, so the kernel picks the matching local interface. Port 0 lets
    the kernel choose an ephemeral source port.
    """

    def __init__(self, local_ip: str, **kwargs):
        self._source_address = (local_ip, 0)
        super().__init__(**kwargs)

    def init_poolmanager(self, *args, **kwargs):
        kwargs["source_address"] = self._source_address
        super().init_poolmanager(*args, **kwargs)

    def proxy_manager_for(self, *args, **kwargs):
        kwargs["source_address"] = self._source_address
        return super().proxy_manager_for(*args, **kwargs)
