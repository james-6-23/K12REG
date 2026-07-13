"""ChatGPT session cookie → access_token helpers.

Browser registration lands with a NextAuth session cookie
(``__Secure-next-auth.session-token``). Calling::

    GET https://chatgpt.com/api/auth/session

with that cookie returns the same JSON shape as a dumped ``session.json``::

    {
      "accessToken": "...",
      "sessionToken": "...",
      "user": {...},
      "account": {"id": "...", "planType": "..."},
      "expires": "..."
    }

Protocol registration yields a ``refresh_token``; the browser path often
does not.  Use :func:`refresh_via_session` to mint a fresh access token
from the long-lived session cookie instead.
"""

from __future__ import annotations

import logging
from typing import Any

from curl_cffi import requests

logger = logging.getLogger(__name__)

SESSION_URL = "https://chatgpt.com/api/auth/session"
USER_AGENT = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/145.0.0.0 Safari/537.36"
)

# Primary NextAuth cookie names (ChatGPT / openai.com).
_SESSION_COOKIE_BASE = "__Secure-next-auth.session-token"
_SESSION_COOKIE_LEGACY = "next-auth.session-token"
_SESSION_COOKIE_CANDIDATES = (
    _SESSION_COOKIE_BASE,
    _SESSION_COOKIE_LEGACY,
    "oai-client-auth-session",
)


def extract_session_token_from_cookies(
    cookies: list[dict[str, Any]] | dict[str, str],
) -> str:
    """Pull the session token out of a Playwright/browser cookie list or map.

    Handles NextAuth chunked cookies (``.0``, ``.1``, …) by concatenating
    parts in order when the un-chunked name is absent.
    """
    if isinstance(cookies, dict):
        jar = {str(k): str(v or "") for k, v in cookies.items()}
    else:
        jar: dict[str, str] = {}
        for c in cookies or []:
            if not isinstance(c, dict):
                continue
            name = str(c.get("name") or "").strip()
            value = str(c.get("value") or "").strip()
            if name and value:
                jar[name] = value

    for name in _SESSION_COOKIE_CANDIDATES:
        val = jar.get(name, "").strip()
        if val:
            return val

    # Chunked: __Secure-next-auth.session-token.0 + .1 + ...
    for base in (_SESSION_COOKIE_BASE, _SESSION_COOKIE_LEGACY):
        parts: list[tuple[int, str]] = []
        prefix = base + "."
        for name, value in jar.items():
            if not name.startswith(prefix):
                continue
            suffix = name[len(prefix) :]
            if suffix.isdigit() and value:
                parts.append((int(suffix), value))
        if parts:
            parts.sort(key=lambda x: x[0])
            return "".join(v for _, v in parts)

    return ""


def cookies_list_to_jar(cookies: list[dict[str, Any]] | dict[str, str] | None) -> dict[str, str]:
    """Flatten Playwright cookie list or map into a simple name→value jar."""
    if not cookies:
        return {}
    if isinstance(cookies, dict):
        return {str(k): str(v or "") for k, v in cookies.items() if str(k)}
    jar: dict[str, str] = {}
    for c in cookies:
        if not isinstance(c, dict):
            continue
        name = str(c.get("name") or "").strip()
        value = str(c.get("value") or "").strip()
        if name and value:
            jar[name] = value
    return jar


def fetch_session_from_token(
    session_token: str,
    *,
    proxy: str = "",
    timeout: int = 20,
    account_id: str = "",
    max_retries: int = 4,
    extra_cookies: dict[str, str] | list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """Call ``/api/auth/session`` with a session cookie; return the JSON body.

    Optional ``account_id`` selects a workspace (K12/team) by setting the
    account cookies + ``Chatgpt-Account-Id`` header — otherwise the default
    personal free workspace is returned.

    Pass ``extra_cookies`` (full browser jar) when available so Cloudflare
    ``cf_clearance`` / ``__cf_bm`` ride along — bare session cookie often
    gets HTTP 403 HTML.

    Retries transient failures (timeout / 403 CF / 429 / 5xx / empty AT).

    Raises ``ValueError`` if the token is empty or the response has no
    ``accessToken``.
    """
    import time

    session_token = str(session_token or "").strip()
    if not session_token:
        raise ValueError("empty session_token")

    account_id = str(account_id or "").strip()
    cookies: dict[str, str] = cookies_list_to_jar(extra_cookies)
    # Ensure the NextAuth session cookie is present / wins over stale jar values.
    cookies[_SESSION_COOKIE_BASE] = session_token

    headers: dict[str, str] = {
        "accept": "application/json",
        "user-agent": USER_AGENT,
        "referer": "https://chatgpt.com/",
        "origin": "https://chatgpt.com",
    }
    if account_id:
        # Community scripts switch workspace via these cookie names.
        cookies["oai-account-id"] = account_id
        cookies["_account"] = account_id
        cookies["chatgpt-account-id"] = account_id
        headers["chatgpt-account-id"] = account_id
        headers["Chatgpt-Account-Id"] = account_id

    kwargs: dict[str, Any] = {
        "impersonate": "chrome",
        "verify": False,
        "timeout": timeout,
        "headers": headers,
        "cookies": cookies,
    }
    if proxy:
        kwargs["proxy"] = proxy

    # Bust caches when switching workspace.
    url = SESSION_URL
    if account_id:
        url = f"{SESSION_URL}?account_id={account_id}"

    attempts = max(1, int(max_retries or 1))
    last_err: Exception | None = None

    for attempt in range(1, attempts + 1):
        try:
            resp = requests.get(url, **kwargs)
        except Exception as e:
            last_err = e
            logger.debug(
                "session fetch attempt %s/%s network error: %s",
                attempt,
                attempts,
                e,
            )
            if attempt < attempts:
                time.sleep(0.5 * attempt)
                continue
            raise ValueError(f"/api/auth/session network error: {e}") from e

        status = int(getattr(resp, "status_code", 0) or 0)
        body_preview = ""
        try:
            body_preview = (resp.text or "")[:200]
        except Exception:
            pass

        # 403 HTML is usually Cloudflare — retry a few times; may clear.
        retryable = status in (403, 429, 500, 502, 503, 504) or (
            status == 200 and "<html" in body_preview.lower()
        )
        if retryable and attempt < attempts:
            logger.debug(
                "session fetch attempt %s/%s HTTP %s — retry (%s)",
                attempt,
                attempts,
                status,
                body_preview[:60].replace("\n", " "),
            )
            time.sleep(0.7 * attempt)
            continue

        if status != 200:
            raise ValueError(f"/api/auth/session HTTP {status}: {body_preview}")

        # Reject CF / HTML even with 200.
        ctype = ""
        try:
            ctype = str(resp.headers.get("content-type") or "").lower()
        except Exception:
            pass
        if "html" in ctype or body_preview.lstrip().lower().startswith("<!doctype") or (
            body_preview.lstrip().lower().startswith("<html")
        ):
            last_err = ValueError(
                f"/api/auth/session returned HTML (likely CF): {body_preview[:120]}"
            )
            if attempt < attempts:
                time.sleep(0.7 * attempt)
                continue
            raise last_err

        try:
            data = resp.json()
        except Exception as e:
            last_err = e
            if attempt < attempts:
                time.sleep(0.4 * attempt)
                continue
            raise ValueError(f"/api/auth/session invalid JSON: {e}") from e

        if not isinstance(data, dict):
            raise ValueError("/api/auth/session returned non-object JSON")

        access = str(data.get("accessToken") or data.get("access_token") or "").strip()
        if not access:
            # Empty body often means cookie not fully propagated yet — retry.
            last_err = ValueError(
                "/api/auth/session returned no accessToken "
                "(session cookie invalid or expired)"
            )
            if attempt < attempts:
                logger.debug(
                    "session fetch attempt %s/%s empty accessToken — retry",
                    attempt,
                    attempts,
                )
                time.sleep(0.8 * attempt)
                continue
            raise last_err
        return data

    if last_err:
        raise ValueError(str(last_err)) from last_err
    raise ValueError("/api/auth/session failed after retries")


def session_payload_to_account_fields(data: dict[str, Any]) -> dict[str, Any]:
    """Map a ``/api/auth/session`` JSON body onto our account record keys."""
    user = data.get("user") if isinstance(data.get("user"), dict) else {}
    account = data.get("account") if isinstance(data.get("account"), dict) else {}

    access_token = str(
        data.get("accessToken") or data.get("access_token") or ""
    ).strip()
    session_token = str(
        data.get("sessionToken") or data.get("session_token") or ""
    ).strip()

    return {
        "access_token": access_token,
        "session_token": session_token,
        "expires": str(data.get("expires") or "").strip(),
        "chatgpt_account_id": str(account.get("id") or "").strip(),
        "plan_type": str(account.get("planType") or account.get("plan_type") or "").strip(),
        "chatgpt_user_id": str(user.get("id") or "").strip(),
        "email": str(user.get("email") or "").strip(),
        "auth_provider": str(data.get("authProvider") or data.get("auth_provider") or "").strip(),
    }


def refresh_via_session(
    session_token: str,
    *,
    proxy: str = "",
    timeout: int = 30,
    account_id: str = "",
) -> dict[str, Any]:
    """Refresh access_token (and related fields) from a session cookie.

    Returns the same flat dict as :func:`session_payload_to_account_fields`.
    Pass ``account_id`` (K12 workspace UUID) to request a workspace-scoped
    access token instead of the personal free default.
    """
    data = fetch_session_from_token(
        session_token,
        proxy=proxy,
        timeout=timeout,
        account_id=account_id,
    )
    fields = session_payload_to_account_fields(data)
    # Prefer the cookie we already have if the body omits sessionToken.
    if not fields.get("session_token"):
        fields["session_token"] = session_token
    return fields


# Plans that indicate a non-personal / usable team-like seat.
_TEAMISH_PLANS = frozenset(
    {"k12", "team", "enterprise", "edu", "business", "plus", "pro"}
)


def pick_account_from_check(
    check_data: dict[str, Any],
    preferred_workspace_ids: list[str] | None = None,
) -> dict[str, str]:
    """Pick the best workspace entry from ``/accounts/check`` JSON.

    Prefer (in order):
      1. account_id in ``preferred_workspace_ids`` (configured K12 ids)
      2. plan_type k12 / team / enterprise / …
      3. ``default`` entry
      4. first entry

    Returns ``{plan_type, account_id, role, key}`` (strings, may be empty).
    """
    preferred = {
        str(x).strip().lower()
        for x in (preferred_workspace_ids or [])
        if str(x).strip()
    }
    accts = check_data.get("accounts") if isinstance(check_data, dict) else None
    if not isinstance(accts, dict):
        return {"plan_type": "", "account_id": "", "role": "", "key": ""}

    entries: list[dict[str, str]] = []
    for key, val in accts.items():
        if not isinstance(val, dict):
            continue
        account = val.get("account") if isinstance(val.get("account"), dict) else {}
        plan = str(account.get("plan_type") or "").strip()
        aid = str(account.get("account_id") or key or "").strip()
        role = str(
            account.get("account_user_role") or account.get("role") or ""
        ).strip()
        entries.append(
            {
                "plan_type": plan,
                "account_id": aid,
                "role": role,
                "key": str(key),
            }
        )

    if not entries:
        return {"plan_type": "", "account_id": "", "role": "", "key": ""}

    for e in entries:
        if e["account_id"].lower() in preferred:
            return e
    for e in entries:
        if e["plan_type"].lower() in _TEAMISH_PLANS:
            return e
    for e in entries:
        if e["key"] == "default":
            return e
    return entries[0]


def elevate_session_to_workspace(
    session_token: str,
    workspace_ids: list[str],
    *,
    proxy: str = "",
    timeout: int = 30,
) -> dict[str, Any] | None:
    """Try each workspace id until session returns a non-free / matching plan.

    Returns account fields dict on success, else None.
    """
    session_token = str(session_token or "").strip()
    if not session_token:
        return None

    for wid in workspace_ids:
        wid = str(wid or "").strip()
        if not wid:
            continue
        try:
            fields = refresh_via_session(
                session_token,
                proxy=proxy,
                timeout=timeout,
                account_id=wid,
            )
        except Exception as e:
            logger.debug("elevate session for %s failed: %s", wid[:12], e)
            continue

        plan = str(fields.get("plan_type") or "").lower()
        aid = str(fields.get("chatgpt_account_id") or "").strip()
        # Success if plan is teamish OR account id matches the workspace we asked for.
        if plan in _TEAMISH_PLANS or (aid and aid.lower() == wid.lower()):
            fields["chatgpt_account_id"] = aid or wid
            if not fields.get("plan_type") and aid.lower() == wid.lower():
                # Session may omit planType; mark as k12 when id matches config.
                fields["plan_type"] = "k12"
            return fields
    return None
