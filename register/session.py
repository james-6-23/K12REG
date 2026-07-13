"""curl_cffi session factory with Cloudflare clearance support.

Provides a single `create_session()` call that:
  - Applies proxy settings
  - Sets Chrome impersonation
  - Optionally fetches clearance via **local JSD** and/or FlareSolverr
  - Handles clearance refresh on Cloudflare challenge detection
"""

from __future__ import annotations

import logging
from urllib.parse import urlparse

from curl_cffi import requests

from utils.proxy import (
    ClearanceBundle,
    FlareSolverrClearanceProvider,
    JSDClearanceProvider,
    apply_clearance_to_session,
    create_session as _create_session,
    get_cached_clearance,
    normalize_proxy_url,
    set_cached_clearance,
)

logger = logging.getLogger(__name__)


def create_register_session(
    proxy: str = "",
    flaresolverr_url: str = "",
    jsd_url: str = "",
    impersonate: str = "chrome",
) -> requests.Session:
    """Create a curl_cffi Session with proxy and optional Cloudflare clearance.

    Args:
        proxy: SOCKS5/HTTP proxy URL
        flaresolverr_url: FlareSolverr endpoint (optional)
        jsd_url: Local JSD pure-compute solver, e.g. ``http://127.0.0.1:8191``
        impersonate: TLS fingerprint target
    """
    session = _create_session(proxy=proxy, impersonate=impersonate, verify=False)

    if jsd_url or flaresolverr_url:
        _prewarm_clearance(
            session,
            proxy,
            jsd_url=jsd_url,
            flaresolverr_url=flaresolverr_url,
        )

    return session


def _fetch_clearance_bundle(
    target_url: str,
    proxy: str = "",
    *,
    jsd_url: str = "",
    flaresolverr_url: str = "",
) -> ClearanceBundle | None:
    """Try JSD first (pure compute), then FlareSolverr."""
    proxy_n = normalize_proxy_url(proxy)
    # Order: JSD (fast pure-compute) → FlareSolverr (browser, slow)
    if jsd_url:
        try:
            bundle = JSDClearanceProvider(jsd_url).fetch_clearance(
                target_url, proxy_n
            )
            if bundle and bundle.cookies:
                logger.info(
                    "JSD clearance ok host=%s cookies=%s ua=%s",
                    bundle.target_host,
                    list(bundle.cookies.keys()),
                    bool(bundle.user_agent),
                )
                return bundle
            logger.warning(
                "JSD clearance empty for %s — will try FlareSolverr=%s",
                target_url,
                bool(flaresolverr_url),
            )
        except Exception as e:
            logger.warning(
                "JSD clearance failed: %s — will try FlareSolverr=%s",
                e,
                bool(flaresolverr_url),
            )

    if flaresolverr_url:
        try:
            bundle = FlareSolverrClearanceProvider(flaresolverr_url).fetch_clearance(
                target_url, proxy_n
            )
            if bundle and bundle.cookies:
                logger.info("FlareSolverr clearance ok host=%s", bundle.target_host)
                return bundle
            logger.warning("FlareSolverr clearance empty for %s", target_url)
        except Exception as e:
            logger.warning("FlareSolverr clearance failed: %s", e)
    return None


def _prewarm_clearance(
    session: requests.Session,
    proxy: str,
    *,
    jsd_url: str = "",
    flaresolverr_url: str = "",
) -> None:
    """Try to get initial Cloudflare clearance for auth.openai.com."""
    target_host = "auth.openai.com"
    target_url = f"https://{target_host}/"

    cached = get_cached_clearance(target_host, proxy)
    if cached is not None:
        apply_clearance_to_session(session, cached)
        return

    bundle = _fetch_clearance_bundle(
        target_url,
        proxy,
        jsd_url=jsd_url,
        flaresolverr_url=flaresolverr_url,
    )
    if bundle:
        set_cached_clearance(bundle)
        apply_clearance_to_session(session, bundle)


def is_cloudflare_challenge(resp: requests.Response | None) -> bool:
    """Detect if a response is a Cloudflare challenge page."""
    if resp is None:
        return False
    try:
        status_code = int(getattr(resp, "status_code", 0) or 0)
    except (TypeError, ValueError):
        status_code = 0
    if status_code not in (403, 503):
        return False
    text = str(getattr(resp, "text", "") or "").lower()
    return (
        "<title>just a moment" in text
        or "<title>attention required! | cloudflare" in text
        or "cf-chl-" in text
        or "__cf_chl_" in text
        or "cf-browser-verification" in text
        or "cloudflare" in text
    )


def refresh_clearance_and_retry(
    session: requests.Session,
    target_url: str,
    proxy: str = "",
    flaresolverr_url: str = "",
    jsd_url: str = "",
) -> bool:
    """Refresh Cloudflare clearance and apply to session.

    Returns True if clearance was successfully refreshed.
    """
    if not jsd_url and not flaresolverr_url:
        return False

    bundle = _fetch_clearance_bundle(
        target_url,
        proxy,
        jsd_url=jsd_url,
        flaresolverr_url=flaresolverr_url,
    )
    if bundle is None or not bundle.cookies:
        return False

    set_cached_clearance(bundle)
    apply_clearance_to_session(session, bundle)
    return True


def request_with_retry(
    session: requests.Session,
    method: str,
    url: str,
    retry_attempts: int = 4,
    timeout: int = 30,
    **kwargs,
) -> tuple[requests.Response | None, str]:
    """Make an HTTP request with retry on network / TLS errors.

    curl_cffi often raises ``curl: (35) TLS connect error`` through flaky
    residential proxies — short backoff + retry usually recovers.

    Returns (response, error_string). error_string is empty on success.
    """
    from utils.proxy import is_tls_or_transport_error

    last_err = ""
    method = (method or "get").lower()
    attempts = max(1, int(retry_attempts))
    for attempt in range(1, attempts + 1):
        try:
            fn = getattr(session, method, None)
            if not callable(fn):
                return None, f"unsupported method {method}"
            resp = fn(url, timeout=timeout, **kwargs)
            return resp, ""
        except Exception as e:
            last_err = str(e)
            transient = is_tls_or_transport_error(e)
            if attempt >= attempts:
                break
            # TLS/proxy errors: longer backoff; other errors: short
            delay = (1.2 * attempt) if transient else (0.4 * attempt)
            logger.debug(
                "request_with_retry %s %s attempt %s/%s: %s (sleep %.1fs)",
                method,
                url[:60],
                attempt,
                attempts,
                last_err[:80],
                delay,
            )
            import time

            time.sleep(delay)
    return None, last_err


def cloudflare_retry_pattern(
    session: requests.Session,
    url: str,
    proxy: str,
    flaresolverr_url: str,
    make_request,
    index: int = 0,
    jsd_url: str = "",
):
    """Legacy helper used by some call sites — try request, refresh CF, retry once."""
    resp, error = make_request()

    if not is_cloudflare_challenge(resp) and resp is not None:
        return resp

    if not flaresolverr_url and not jsd_url:
        raise RuntimeError(
            f"Cloudflare intercepted (no JSD/FlareSolverr). "
            f"Status: {getattr(resp, 'status_code', '?')}. "
            f"Set proxy.jsd_url (e.g. http://127.0.0.1:8191) or proxy.flaresolverr_url."
        )

    target_host = urlparse(url).hostname or "auth.openai.com"
    if not refresh_clearance_and_retry(
        session,
        f"https://{target_host}/",
        proxy,
        flaresolverr_url=flaresolverr_url,
        jsd_url=jsd_url,
    ):
        raise RuntimeError(
            f"Cloudflare clearance refresh failed for {target_host}. "
            f"Check JSD is running (http://127.0.0.1:8191) and proxy matches."
        )

    resp2, error2 = make_request()
    if is_cloudflare_challenge(resp2) or resp2 is None:
        raise RuntimeError(
            error2 or f"Still blocked by Cloudflare after clearance refresh. "
            f"Status: {getattr(resp2, 'status_code', '?')}"
        )
    return resp2
