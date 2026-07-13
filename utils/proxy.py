"""Proxy helpers for curl_cffi sessions.

Simplified from chatgpt2api's proxy_service.py — standalone version
that provides:
  - Proxy URL normalization (socks:// → socks5h://)
  - curl_cffi session kwarg building
  - Cloudflare clearance via FlareSolverr (optional)
  - ClearanceBundle dataclass for cookie/UA caching
"""

from __future__ import annotations

import json as _json
import logging
import re
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from curl_cffi import requests

logger = logging.getLogger(__name__)


# ── Proxy URL normalization ──────────────────────────────────────────

def normalize_proxy_url(url: str | None) -> str:
    """Normalize a proxy URL for curl_cffi.

    - Strips whitespace
    - Converts ``socks://`` / ``socks5://`` → ``socks5h://`` (remote DNS)
      Remote DNS avoids many TLS/connect flakiness cases through residential pools.
    - Returns empty string if invalid
    """
    url = (url or "").strip()
    if not url:
        return ""
    if re.match(r"^socks5h://", url, re.IGNORECASE):
        return url
    if re.match(r"^socks5://", url, re.IGNORECASE):
        # Prefer socks5h so DNS goes through the proxy (curl 35 often from local DNS + bad path).
        url = "socks5h://" + url[len("socks5://") :]
    elif re.match(r"^socks://", url, re.IGNORECASE):
        url = "socks5h" + url[5:]
    elif re.match(r"^socks4a?://", url, re.IGNORECASE):
        url = re.sub(r"^socks4a?://", "socks4://", url, count=1, flags=re.IGNORECASE)
    return url


def is_tls_or_transport_error(err: BaseException | str | None) -> bool:
    """True for curl/TLS errors that often succeed on retry or new session."""
    s = str(err or "").lower()
    if not s:
        return False
    needles = (
        "curl: (35)",
        "curl: (56)",
        "curl: (7)",
        "curl: (28)",
        "curl: (52)",
        "curl: (55)",
        "tls connect",
        "ssl connect",
        "openssl",
        "invalid library",
        "connection reset",
        "connection refused",
        "timed out",
        "timeout",
        "recv failure",
        "send failure",
        "proxy connect",
        "socks",
    )
    return any(n in s for n in needles)


# ── ClearanceBundle ──────────────────────────────────────────────────

@dataclass
class ClearanceBundle:
    """Holds Cloudflare clearance cookies and User-Agent for a target host."""

    target_host: str = ""
    proxy_url: str = ""
    cookies: dict[str, str] = field(default_factory=dict)
    user_agent: str = ""
    created_at: float = field(default_factory=time.time)
    expires_at: float = 3600.0  # 1 hour from creation by default

    def is_valid_for(self, target_host: str = "", proxy_url: str = "") -> bool:
        """Check if this clearance is still fresh for the given target."""
        if time.time() > self.created_at + self.expires_at:
            return False
        if target_host and self.target_host != target_host:
            return False
        if proxy_url and normalize_proxy_url(proxy_url) != normalize_proxy_url(self.proxy_url):
            return False
        return bool(self.cookies)


# ── FlareSolverr Clearance ───────────────────────────────────────────

class FlareSolverrClearanceProvider:
    """Fetch Cloudflare clearance cookies via FlareSolverr."""

    def __init__(self, flaresolverr_url: str, timeout_ms: int = 60000):
        self._flaresolverr_url = flaresolverr_url.rstrip("/")
        self._timeout_ms = timeout_ms

    def fetch_clearance(
        self, target_url: str, proxy_url: str = ""
    ) -> ClearanceBundle | None:
        """Request clearance from FlareSolverr for target_url.

        Returns a ClearanceBundle on success, None on failure.
        """
        fs_url = f"{self._flaresolverr_url}/v1"
        payload: dict[str, Any] = {
            "cmd": "request.get",
            "url": target_url,
            "maxTimeout": self._timeout_ms,
        }
        if proxy_url:
            payload["proxy"] = {"url": proxy_url}

        try:
            response = requests.post(
                fs_url,
                json=payload,
                headers={"Content-Type": "application/json"},
                timeout=self._timeout_ms / 1000 + 15,
            )
            data = response.json()
        except Exception as e:
            logger.warning("FlareSolverr request error: %s", e)
            return None

        if not isinstance(data, dict):
            return None

        status = str(data.get("status") or "")
        message = str(data.get("message") or "")
        solution = data.get("solution", {}) if isinstance(data.get("solution"), dict) else {}
        cookies_list = solution.get("cookies", []) or []
        user_agent = solution.get("userAgent", "")

        if status and status.lower() not in ("ok", "success"):
            logger.warning(
                "FlareSolverr status=%s msg=%s",
                status,
                message[:200],
            )
            return None

        if not cookies_list:
            # Common with socks5 residential + auth.openai.com: "Challenge not detected" + 0 cookies
            logger.warning(
                "FlareSolverr empty cookies for %s (msg=%s http=%s proxy=%s)",
                target_url,
                message[:120] or "-",
                solution.get("status", "?"),
                "yes" if proxy_url else "no",
            )
            return None

        # Extract target host from URL for cookie domain scoping
        from urllib.parse import urlparse
        try:
            host = urlparse(target_url).hostname or ""
        except Exception:
            host = ""

        cookies: dict[str, str] = {}
        for cookie in cookies_list:
            name = cookie.get("name", "")
            value = cookie.get("value", "")
            if name and value:
                cookies[name] = value

        return ClearanceBundle(
            target_host=host,
            proxy_url=proxy_url,
            cookies=cookies,
            user_agent=user_agent,
            created_at=time.time(),
            expires_at=1800.0,  # 30 minutes
        )


# ── Local JSD pure-compute clearance (JSD/ cloudflare-jsd-solver) ────

class JSDClearanceProvider:
    """Fetch Cloudflare JSD clearance via local pure-compute solver.

    Expects a running service (default ``http://127.0.0.1:8191``)::

        POST /solve
        {"url":"https://auth.openai.com","mode":"full","proxy":"..."}

    Returns ``cf_clearance`` + cookies + user_agent. Clearance is **IP+UA bound**
    — use the same proxy for solve and subsequent protocol requests.
    """

    def __init__(self, jsd_url: str = "http://127.0.0.1:8191", timeout: float = 45.0):
        self._base = str(jsd_url or "").rstrip("/")
        self._timeout = float(timeout or 45.0)

    def fetch_clearance(
        self, target_url: str, proxy_url: str = ""
    ) -> ClearanceBundle | None:
        if not self._base:
            return None
        target_url = str(target_url or "").strip()
        if not target_url:
            return None
        if "://" not in target_url:
            target_url = "https://" + target_url

        # Prefer /solve; some builds also expose /harvest
        endpoint = self._base
        if not endpoint.endswith("/solve") and not endpoint.endswith("/harvest"):
            endpoint = endpoint + "/solve"

        payload: dict[str, Any] = {
            "url": target_url,
            "mode": "full",
        }
        proxy_url = normalize_proxy_url(proxy_url)
        if proxy_url:
            payload["proxy"] = proxy_url

        try:
            response = requests.post(
                endpoint,
                json=payload,
                headers={"Content-Type": "application/json"},
                timeout=self._timeout + 15,
                impersonate="chrome",
                verify=False,
            )
            data = response.json()
        except Exception as e:
            logger.warning("JSD request error: %s", e)
            return None

        if not isinstance(data, dict):
            return None
        if not data.get("success"):
            err = data.get("error") or data.get("message") or data
            logger.warning("JSD solve failed: %s", str(err)[:240])
            return None

        from urllib.parse import urlparse

        try:
            host = urlparse(target_url).hostname or ""
        except Exception:
            host = ""

        cookies: dict[str, str] = {}
        # Prefer map
        raw_cookies = data.get("cookies")
        if isinstance(raw_cookies, dict):
            for k, v in raw_cookies.items():
                if k and v is not None:
                    cookies[str(k)] = str(v)
        # Cookie header fallback
        hdr = str(data.get("cookie_header") or data.get("Cookie") or "")
        if hdr and not cookies:
            for part in hdr.split(";"):
                part = part.strip()
                if "=" in part:
                    n, _, v = part.partition("=")
                    if n.strip():
                        cookies[n.strip()] = v.strip()
        # Explicit cf_clearance
        cf = str(data.get("cf_clearance") or "").strip()
        if cf:
            cookies["cf_clearance"] = cf

        if not cookies:
            return None

        ua = str(data.get("user_agent") or data.get("User-Agent") or "").strip()
        return ClearanceBundle(
            target_host=host,
            proxy_url=proxy_url,
            cookies=cookies,
            user_agent=ua,
            created_at=time.time(),
            expires_at=1800.0,
        )


# ── Session building ─────────────────────────────────────────────────

# Per-proxy clearance cache (simple dict, no persistence)
_clearance_cache: dict[str, ClearanceBundle] = {}
_clearance_lock = threading.Lock()


def build_session_kwargs(
    proxy: str = "",
    impersonate: str = "chrome",
    verify: bool = False,
) -> dict[str, Any]:
    """Build kwargs for constructing a curl_cffi requests.Session.

    Args:
        proxy: SOCKS5/HTTP proxy URL
        impersonate: TLS fingerprint target (default: chrome)
        verify: Whether to verify SSL certs
    """
    kwargs: dict[str, Any] = {
        "impersonate": impersonate,
        "verify": verify,
    }
    proxy_url = normalize_proxy_url(proxy)
    if proxy_url:
        kwargs["proxy"] = proxy_url
    return kwargs


def create_session(
    proxy: str = "",
    impersonate: str = "chrome",
    verify: bool = False,
) -> requests.Session:
    """Create a curl_cffi requests.Session with proxy and TLS impersonation."""
    kwargs = build_session_kwargs(proxy=proxy, impersonate=impersonate, verify=verify)
    return requests.Session(**kwargs)


def apply_clearance_to_session(
    session: requests.Session,
    bundle: ClearanceBundle | None,
) -> None:
    """Inject clearance cookies and User-Agent into a curl_cffi session."""
    if bundle is None:
        return
    if bundle.user_agent:
        session.headers["User-Agent"] = bundle.user_agent
        session.headers["user-agent"] = bundle.user_agent
    for name, value in bundle.cookies.items():
        try:
            session.cookies.set(name, value, domain=f".{bundle.target_host or 'openai.com'}")
        except Exception:
            pass


def get_cached_clearance(target_host: str, proxy: str = "") -> ClearanceBundle | None:
    """Get a cached clearance bundle for the target host + proxy combo."""
    key = f"{normalize_proxy_url(proxy)}::{target_host}"
    with _clearance_lock:
        bundle = _clearance_cache.get(key)
        if bundle and bundle.is_valid_for(target_host, proxy):
            return bundle
    return None


def set_cached_clearance(bundle: ClearanceBundle) -> None:
    """Cache a clearance bundle."""
    key = f"{normalize_proxy_url(bundle.proxy_url)}::{bundle.target_host}"
    with _clearance_lock:
        _clearance_cache[key] = bundle


# ── Proxy pool loading ───────────────────────────────────────────────

def _normalize_proxy_line(line: str, default_protocol: str = "socks5") -> str:
    """Normalize one proxy line into a URL.

    Supports:
      - ``socks5://user:pass@host:port`` / ``http://...``
      - ``user:pass@host:port``
      - ``host:port``
      - ``host:port:user:pass`` (common residential export format)
      - ``host:port:user:pass`` with extra ``:`` in password (last two = user/pass
        only when host looks like host and port is numeric)
    """
    line = (line or "").strip()
    if not line:
        return ""
    if "://" in line:
        return line

    # user:pass@host:port
    if "@" in line:
        return f"{default_protocol}://{line}"

    parts = line.split(":")
    # host:port
    if len(parts) == 2 and parts[1].isdigit():
        return f"{default_protocol}://{line}"
    # host:port:user:pass  (pass may contain ':')
    if len(parts) >= 4 and parts[1].isdigit():
        host, port, user = parts[0], parts[1], parts[2]
        password = ":".join(parts[3:])
        return f"{default_protocol}://{user}:{password}@{host}:{port}"

    # Fallback: prefix scheme and hope the provider format is already URL-like
    return f"{default_protocol}://{line}"


def load_proxies_from_file(
    path: str | Path, default_protocol: str = "socks5"
) -> list[str]:
    """Load a list of proxies from a text file (one per line).

    Supports:
      - ``user:pass@host:port``
      - ``socks5://user:pass@host:port`` / ``http://...``
      - ``host:port:user:pass`` (residential common)

    If no scheme is present, prefix with ``default_protocol://`` (default socks5).
    Lines starting with # are ignored.
    """
    p = Path(path)
    if not p.exists():
        return []

    proto = (default_protocol or "socks5").strip() or "socks5"
    proxies: list[str] = []
    for raw in p.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        url = _normalize_proxy_line(line, default_protocol=proto)
        if url:
            proxies.append(url)
    return proxies


def get_proxy_for_index(proxies: list[str], index: int) -> str:
    """Return a proxy for the given 1-based index (round-robin)."""
    if not proxies:
        return ""
    return proxies[(index - 1) % len(proxies)]
