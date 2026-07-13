"""YesCaptcha (https://yescaptcha.com) client for Cloudflare Turnstile / 5s shield.

API style matches YesCaptcha createTask / getTaskResult::

    POST {base}/createTask
    POST {base}/getTaskResult

Task types used here:

  * ``TurnstileTaskProxyless`` — returns ``token`` for Turnstile widget
  * ``CloudFlareTask`` — returns ``cf_clearance`` (+ optional cookies) when
    the challenge needs a clearance cookie (requires proxy)

Docs: https://yescaptcha.atlassian.net/wiki/spaces/YESCAPTCHA
"""

from __future__ import annotations

import logging
import time
from typing import Any

from curl_cffi import requests

logger = logging.getLogger(__name__)

DEFAULT_BASE = "https://api.yescaptcha.com"


class YesCaptchaError(RuntimeError):
    """YesCaptcha API or solve failure."""


def _post_json(base: str, path: str, payload: dict[str, Any], timeout: float = 60) -> dict:
    url = base.rstrip("/") + path
    resp = requests.post(
        url,
        json=payload,
        timeout=timeout,
        impersonate="chrome",
        verify=False,
    )
    try:
        data = resp.json()
    except Exception as e:
        raise YesCaptchaError(
            f"YesCaptcha {path} non-JSON HTTP {resp.status_code}: {(resp.text or '')[:200]}"
        ) from e
    if not isinstance(data, dict):
        raise YesCaptchaError(f"YesCaptcha {path} unexpected body: {data!r}")
    return data


def get_balance(client_key: str, *, base_url: str = DEFAULT_BASE) -> float:
    data = _post_json(
        base_url,
        "/getBalance",
        {"clientKey": client_key},
        timeout=30,
    )
    if data.get("errorId"):
        raise YesCaptchaError(
            f"getBalance error {data.get('errorCode')}: {data.get('errorDescription')}"
        )
    try:
        return float(data.get("balance") or 0)
    except Exception:
        return 0.0


def create_task(
    client_key: str,
    task: dict[str, Any],
    *,
    base_url: str = DEFAULT_BASE,
) -> str:
    data = _post_json(
        base_url,
        "/createTask",
        {"clientKey": client_key, "task": task},
        timeout=60,
    )
    if data.get("errorId"):
        raise YesCaptchaError(
            f"createTask error {data.get('errorCode')}: {data.get('errorDescription')}"
        )
    task_id = str(data.get("taskId") or "").strip()
    if not task_id:
        raise YesCaptchaError(f"createTask missing taskId: {data}")
    return task_id


def get_task_result(
    client_key: str,
    task_id: str,
    *,
    base_url: str = DEFAULT_BASE,
    poll_interval: float = 3.0,
    max_wait: float = 120.0,
) -> dict[str, Any]:
    """Poll until status=ready; return full ``solution`` dict."""
    deadline = time.time() + max_wait
    last: dict[str, Any] = {}
    while time.time() < deadline:
        last = _post_json(
            base_url,
            "/getTaskResult",
            {"clientKey": client_key, "taskId": task_id},
            timeout=60,
        )
        if last.get("errorId"):
            raise YesCaptchaError(
                f"getTaskResult error {last.get('errorCode')}: "
                f"{last.get('errorDescription')}"
            )
        status = str(last.get("status") or "").lower()
        if status == "ready":
            sol = last.get("solution")
            if not isinstance(sol, dict):
                raise YesCaptchaError(f"ready but no solution: {last}")
            return sol
        if status in ("failed", "error"):
            raise YesCaptchaError(f"task failed: {last}")
        time.sleep(poll_interval)
    raise YesCaptchaError(
        f"task {task_id} timeout after {max_wait:.0f}s (last={last})"
    )


def solve_turnstile(
    client_key: str,
    website_url: str,
    website_key: str,
    *,
    base_url: str = DEFAULT_BASE,
    max_wait: float = 120.0,
    task_type: str = "TurnstileTaskProxyless",
    proxy: str = "",
) -> str:
    """Solve Cloudflare Turnstile; return response token string."""
    website_url = str(website_url or "").strip()
    website_key = str(website_key or "").strip()
    if not client_key or not website_url or not website_key:
        raise YesCaptchaError("client_key, website_url, website_key required")

    task: dict[str, Any] = {
        "type": task_type or "TurnstileTaskProxyless",
        "websiteURL": website_url,
        "websiteKey": website_key,
    }
    # Some deployments accept proxy fields on non-proxyless variants.
    if proxy and "Proxyless" not in task["type"]:
        task["proxy"] = proxy

    logger.info(
        "YesCaptcha Turnstile createTask type=%s key=%s… url=%s",
        task["type"],
        website_key[:12],
        website_url[:80],
    )
    task_id = create_task(client_key, task, base_url=base_url)
    sol = get_task_result(
        client_key, task_id, base_url=base_url, max_wait=max_wait
    )
    token = str(
        sol.get("token")
        or sol.get("cf-turnstile-response")
        or sol.get("gRecaptchaResponse")
        or ""
    ).strip()
    if not token:
        raise YesCaptchaError(f"empty turnstile token in solution: {sol}")
    logger.info("YesCaptcha Turnstile ok token_len=%s", len(token))
    return token


def solve_cloudflare_clearance(
    client_key: str,
    website_url: str,
    *,
    proxy: str,
    user_agent: str = "",
    base_url: str = DEFAULT_BASE,
    max_wait: float = 180.0,
) -> dict[str, Any]:
    """CloudFlareTask — returns solution with cookies / cf_clearance when supported.

    Requires a working proxy string accepted by YesCaptcha (often
    ``http://user:pass@host:port`` or their proprietary format).
    """
    website_url = str(website_url or "").strip()
    proxy = str(proxy or "").strip()
    if not client_key or not website_url or not proxy:
        raise YesCaptchaError("client_key, website_url, proxy required for CloudFlareTask")

    task: dict[str, Any] = {
        "type": "CloudFlareTask",
        "websiteURL": website_url,
        "proxy": proxy,
    }
    if user_agent:
        task["userAgent"] = user_agent

    logger.info("YesCaptcha CloudFlareTask createTask url=%s", website_url[:80])
    task_id = create_task(client_key, task, base_url=base_url)
    sol = get_task_result(
        client_key, task_id, base_url=base_url, max_wait=max_wait, poll_interval=4.0
    )
    return sol
