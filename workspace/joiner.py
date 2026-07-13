"""Workspace join client — send join requests to K12 parent workspace.

Adapted from the Tampermonkey userscript:
  子号加入K12母号代码.txt

The flow:
  1. Use the account's access_token as Bearer token
  2. POST /backend-api/accounts/{workspace_id}/invites/{request|accept}
  3. Auto-accepted by parent workspace (no approval needed)
"""

from __future__ import annotations

import json
import random
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from curl_cffi import requests

CHATGPT_BASE = "https://chatgpt.com"


def join_workspace(
    access_token: str,
    workspace_id: str,
    route: str = "request",
    max_retries: int = 3,
    retry_backoff_ms: int = 5000,
    session: requests.Session | None = None,
    proxy: str = "",
) -> dict:
    """Send a single workspace join request.

    Args:
        access_token: The account's Bearer token
        workspace_id: Parent workspace UUID
        route: "request" (child asks to join) or "accept" (child accepts invite)
        max_retries: Max retry attempts on non-auth errors
        retry_backoff_ms: Backoff between retries (multiplied by attempt)
        proxy: SOCKS5/HTTP proxy URL

    Returns:
        {ok: bool, status_code: int, body: str, workspace_id: str}
    """
    device_id = str(uuid.uuid4())
    url = f"{CHATGPT_BASE}/backend-api/accounts/{workspace_id}/invites/{route}"
    headers = {
        "accept": "*/*",
        "authorization": f"Bearer {access_token}",
        "content-type": "application/json",
        "oai-device-id": device_id,
        "oai-language": "en-US",
    }

    if session:
        _session = session
        should_close = False
    else:
        kwargs = {"impersonate": "chrome", "verify": False}
        if proxy:
            kwargs["proxy"] = proxy
        _session = requests.Session(**kwargs)
        should_close = True

    try:
        for attempt in range(max_retries):
            try:
                resp = _session.post(
                    url,
                    headers=headers,
                    data="",
                    timeout=30,
                )
                body = resp.text[:500] if resp.text else ""
                status = resp.status_code

                if status in (401, 403):
                    return {
                        "ok": False,
                        "status_code": status,
                        "body": body,
                        "workspace_id": workspace_id,
                        "error": "Token expired (401/403). Re-login needed.",
                    }

                if resp.ok:
                    return {
                        "ok": True,
                        "status_code": status,
                        "body": body,
                        "workspace_id": workspace_id,
                    }

                # Non-auth error — retry with backoff
                if attempt < max_retries - 1:
                    time.sleep(retry_backoff_ms * (attempt + 1) / 1000.0)

            except Exception as e:
                if attempt < max_retries - 1:
                    time.sleep(retry_backoff_ms / 1000.0)
                else:
                    return {
                        "ok": False,
                        "status_code": 0,
                        "body": "",
                        "workspace_id": workspace_id,
                        "error": str(e),
                    }

        return {
            "ok": False,
            "status_code": status if 'status' in dir() else 0,
            "body": body if 'body' in dir() else "",
            "workspace_id": workspace_id,
            "error": f"Max retries ({max_retries}) exhausted",
        }
    finally:
        if should_close:
            _session.close()


def join_workspaces(
    access_token: str,
    workspace_ids: list[str],
    route: str = "request",
    max_retries: int = 3,
    retry_backoff_ms: int = 5000,
    interval_ms: int = 1500,
    proxy: str = "",
) -> list[dict]:
    """Join multiple workspaces sequentially.

    Args:
        access_token: The account's Bearer token
        workspace_ids: List of parent workspace UUIDs
        route: "request" or "accept"
        max_retries: Max retries per workspace
        retry_backoff_ms: Backoff between retries
        interval_ms: Delay between different workspace requests
        proxy: SOCKS5/HTTP proxy URL

    Returns:
        List of result dicts, one per workspace_id
    """
    results = []
    session = None
    try:
        kwargs = {"impersonate": "chrome", "verify": False}
        if proxy:
            kwargs["proxy"] = proxy
        session = requests.Session(**kwargs)
        for i, ws_id in enumerate(workspace_ids):
            result = join_workspace(
                access_token=access_token,
                workspace_id=ws_id.strip(),
                route=route,
                max_retries=max_retries,
                retry_backoff_ms=retry_backoff_ms,
                session=session,
                proxy=proxy,
            )
            results.append(result)
            if i < len(workspace_ids) - 1:
                time.sleep(interval_ms / 1000.0)
    finally:
        if session:
            session.close()
    return results


# ── Manager-side approval ────────────────────────────────────────────
#
# K12 workspaces no longer auto-accept join requests: a child's
# ``POST /invites/request`` only creates a *pending* request that the parent
# workspace admin (manager) must approve.  We approve one child at a time,
# matched by its email: list the pending requests, find the matching invite,
# and ``PATCH`` it with ``accept_request=true`` — this takes effect
# immediately (unlike the bulk accept-all-requests sweep, which is eventually
# consistent).


def load_manager_session(path: str | Path) -> dict:
    """Load the parent/manager session dump for approving join requests.

    Accepts the ChatGPT session JSON shape (the ``session.json`` you get from
    the admin browser session)::

        {"accessToken": "...", "account": {"id": "..."}, "expires": "..."}

    Returns ``{token, account_id, expires}``.  ``expires`` is the raw ISO
    string (empty if absent) so the caller can warn on a stale session.

    Raises ``FileNotFoundError`` if the file is missing and ``ValueError`` if
    it carries no usable token.
    """
    p = Path(path)
    if not p.exists():
        raise FileNotFoundError(f"manager session file not found: {p}")
    data = json.loads(p.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise ValueError(f"manager session file is not a JSON object: {p}")

    token = str(data.get("accessToken") or data.get("access_token") or "").strip()
    account = data.get("account") if isinstance(data.get("account"), dict) else {}
    account_id = str(
        account.get("id") or data.get("account_id") or data.get("chatgpt_account_id") or ""
    ).strip()
    expires = str(data.get("expires") or "").strip()

    if not token:
        raise ValueError(f"no accessToken/access_token in {p}")
    return {"token": token, "account_id": account_id, "expires": expires}


def manager_session_is_expired(expires: str) -> bool:
    """True if an ISO ``expires`` timestamp is in the past (best-effort)."""
    if not expires:
        return False
    try:
        exp = datetime.fromisoformat(expires.replace("Z", "+00:00"))
        if exp.tzinfo is None:
            exp = exp.replace(tzinfo=timezone.utc)
        return exp <= datetime.now(timezone.utc)
    except Exception:
        return False


def _manager_headers(manager_token: str, account_id: str, target_path: str, device_id: str = "") -> dict:
    """Browser-ish header set for the workspace admin (manager) endpoints."""
    return {
        "accept": "*/*",
        "accept-language": "zh-CN,zh;q=0.9",
        "authorization": f"Bearer {manager_token}",
        "chatgpt-account-id": account_id,
        "content-type": "application/json",
        "oai-device-id": device_id or str(uuid.uuid4()),
        "oai-language": "en-US",
        "x-openai-target-path": target_path,
        "referer": "https://chatgpt.com/admin/members?tab=requests",
        "sec-fetch-dest": "empty",
        "sec-fetch-mode": "cors",
        "sec-fetch-site": "same-origin",
    }


def list_join_requests(
    manager_token: str,
    account_id: str,
    query: str = "",
    proxy: str = "",
    session: requests.Session | None = None,
    limit: int = 25,
    device_id: str = "",
    timeout: int = 30,
) -> dict:
    """List pending join requests (manager admin view).

    ``GET /backend-api/accounts/{account_id}/invites?include_requests=true``.
    ``query`` narrows the search server-side (matches email / name).

    Returns ``{ok, status_code, items}`` where each item looks like::

        {"id": "<invite_id>", "email_address": "...", "role": "standard-user",
         "seat_type": "default", "status": 1, ...}
    """
    url = f"{CHATGPT_BASE}/backend-api/accounts/{account_id}/invites"
    headers = _manager_headers(
        manager_token, account_id,
        f"/backend-api/accounts/{account_id}/invites", device_id,
    )
    params = {
        "include_pending": "false",
        "include_requests": "true",
        "offset": "0",
        "limit": str(limit),
        "query": query,
    }

    if session:
        _session, should_close = session, False
    else:
        kwargs = {"impersonate": "chrome", "verify": False}
        if proxy:
            kwargs["proxy"] = proxy
        _session, should_close = requests.Session(**kwargs), True

    try:
        resp = _session.get(url, headers=headers, params=params, timeout=timeout)
        items: list[dict] = []
        if resp.ok:
            try:
                data = resp.json()
                if isinstance(data, dict) and isinstance(data.get("items"), list):
                    items = data["items"]
            except Exception:
                pass
        return {"ok": resp.ok, "status_code": resp.status_code, "items": items}
    except Exception as e:
        return {"ok": False, "status_code": 0, "items": [], "error": str(e)}
    finally:
        if should_close:
            _session.close()


def _approve_retry_delay(attempt: int, base_ms: int, step_ms: int, jitter_ms: int) -> float:
    """Backoff (seconds) with jitter for the approval retry loop.

    Grows linearly per attempt and adds random jitter so concurrent workers
    don't all re-query the manager endpoint at the same instant.
    """
    span = base_ms + step_ms * attempt + random.uniform(0, jitter_ms)
    return span / 1000.0


def _email_match_variants(email: str) -> list[str]:
    """Queries / match keys for plus-alias Outlook addresses."""
    e = str(email or "").strip().lower()
    if not e:
        return []
    out = [e]
    # local+tag@domain → also try local@domain and local part before +
    if "@" in e:
        local, _, domain = e.partition("@")
        if "+" in local:
            base_local = local.split("+", 1)[0]
            base = f"{base_local}@{domain}"
            if base not in out:
                out.append(base)
            if base_local not in out:
                out.append(base_local)
        # server query sometimes prefers bare local-part
        if local not in out:
            out.append(local)
    return out


def _find_pending_item(items: list[dict], email: str) -> dict | None:
    """Match pending invite by email (exact, plus-alias, or contains)."""
    want = str(email or "").strip().lower()
    if not want:
        return None
    variants = set(_email_match_variants(want))
    # 1) exact / known variants
    for it in items:
        got = str(it.get("email_address") or it.get("email") or "").strip().lower()
        if got and got in variants:
            return it
    # 2) plus-alias: list shows base, we have base+tag (or reverse)
    if "@" in want:
        w_local, _, w_dom = want.partition("@")
        w_base = w_local.split("+", 1)[0]
        for it in items:
            got = str(it.get("email_address") or it.get("email") or "").strip().lower()
            if not got or "@" not in got:
                continue
            g_local, _, g_dom = got.partition("@")
            g_base = g_local.split("+", 1)[0]
            if w_dom == g_dom and w_base == g_base:
                return it
    return None


def approve_join_request(
    manager_token: str,
    account_id: str,
    email: str,
    proxy: str = "",
    session: requests.Session | None = None,
    device_id: str = "",
    timeout: int = 30,
    max_attempts: int = 12,
    retry_base_ms: int = 2000,
    retry_step_ms: int = 1200,
    retry_jitter_ms: int = 2000,
) -> dict:
    """Approve one pending join request, identified by the child's email.

    A child's ``POST /invites/request`` does not appear in the manager's
    invites list instantly — there's a short propagation delay.  So we retry
    the list+match with jittered backoff (``max_attempts`` times) until the
    request shows up, then:

    1. Find the item whose ``email_address`` matches (case-insensitive / plus-alias).
    2. ``PATCH /backend-api/accounts/{account_id}/invites/{invite_id}`` with
       ``{"role", "seat_type", "accept_request": true}`` (role/seat_type
       carried over from the request so we don't downgrade a custom role).

    Transient list failures (TLS/proxy blips → HTTP 0) are retried too.
    Listing tries full email, base email (strip +tag), and unfiltered list.

    Returns ``{ok, status_code, invite_id, matched, body, attempts}``.
    ``matched`` is False when no pending request appears even after retries.
    """
    want = str(email or "").strip().lower()
    if not want:
        return {"ok": False, "status_code": 0, "invite_id": "", "matched": False,
                "body": "", "attempts": 0, "error": "no email given"}

    if session:
        _session, should_close = session, False
    else:
        kwargs = {"impersonate": "chrome", "verify": False}
        if proxy:
            kwargs["proxy"] = proxy
        _session, should_close = requests.Session(**kwargs), True

    attempts = max(1, int(max_attempts))
    queries = _email_match_variants(want) + [""]  # "" = unfiltered list
    last: dict = {"ok": False, "status_code": 0, "invite_id": "", "matched": False,
                  "body": "", "attempts": 0, "error": "no attempts run"}
    try:
        for attempt in range(attempts):
            item = None
            listing_status = 0
            listing_ok = False
            # Rotate query strategy each attempt to beat +alias / lag.
            q_order = queries[attempt % len(queries) :] + queries[: attempt % len(queries)]
            for q in q_order:
                listing = list_join_requests(
                    manager_token,
                    account_id,
                    query=q,
                    session=_session,
                    device_id=device_id,
                    timeout=timeout,
                    limit=50,
                )
                listing_status = int(listing.get("status_code") or 0)
                if not listing.get("ok"):
                    continue
                listing_ok = True
                item = _find_pending_item(listing.get("items") or [], want)
                if item is not None:
                    break

            if not listing_ok:
                last = {
                    "ok": False,
                    "status_code": listing_status,
                    "invite_id": "",
                    "matched": False,
                    "body": "",
                    "attempts": attempt + 1,
                    "error": f"list failed HTTP {listing_status}",
                }
                if attempt < attempts - 1:
                    time.sleep(
                        _approve_retry_delay(
                            attempt, retry_base_ms, retry_step_ms, retry_jitter_ms
                        )
                    )
                    continue
                return last

            if item is None:
                # Not visible yet — wait for the request to propagate, retry.
                last = {
                    "ok": False,
                    "status_code": listing_status,
                    "invite_id": "",
                    "matched": False,
                    "body": "",
                    "attempts": attempt + 1,
                    "error": "no pending request for email",
                }
                if attempt < attempts - 1:
                    time.sleep(
                        _approve_retry_delay(
                            attempt, retry_base_ms, retry_step_ms, retry_jitter_ms
                        )
                    )
                    continue
                return last

            invite_id = str(item.get("id") or "").strip()
            if not invite_id:
                return {
                    "ok": False,
                    "status_code": listing_status,
                    "invite_id": "",
                    "matched": True,
                    "body": "",
                    "attempts": attempt + 1,
                    "error": "request item has no id",
                }

            url = f"{CHATGPT_BASE}/backend-api/accounts/{account_id}/invites/{invite_id}"
            headers = _manager_headers(
                manager_token,
                account_id,
                f"/backend-api/accounts/{account_id}/invites/{invite_id}",
                device_id,
            )
            payload = {
                "role": str(item.get("role") or "standard-user"),
                "seat_type": str(item.get("seat_type") or "default"),
                "accept_request": True,
            }
            try:
                resp = _session.patch(
                    url, headers=headers, json=payload, timeout=timeout
                )
                body = resp.text[:500] if resp.text else ""
            except Exception as e:
                # Transient PATCH failure — retry the whole find+accept.
                last = {"ok": False, "status_code": 0, "invite_id": invite_id,
                        "matched": True, "body": "", "attempts": attempt + 1, "error": str(e)}
                if attempt < attempts - 1:
                    time.sleep(_approve_retry_delay(
                        attempt, retry_base_ms, retry_step_ms, retry_jitter_ms))
                    continue
                return last

            if not resp.ok and resp.status_code in (429, 500, 502, 503, 504) and attempt < attempts - 1:
                last = {"ok": False, "status_code": resp.status_code, "invite_id": invite_id,
                        "matched": True, "body": body, "attempts": attempt + 1}
                time.sleep(_approve_retry_delay(
                    attempt, retry_base_ms, retry_step_ms, retry_jitter_ms))
                continue

            return {"ok": resp.ok, "status_code": resp.status_code,
                    "invite_id": invite_id, "matched": True, "body": body,
                    "attempts": attempt + 1}
        return last
    except Exception as e:
        return {"ok": False, "status_code": 0, "invite_id": "", "matched": False,
                "body": "", "attempts": 0, "error": str(e)}
    finally:
        if should_close:
            _session.close()
