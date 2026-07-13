"""Pipeline orchestrator — register → join → refresh → export → import.

The full run (`run_full_pipeline`) is **per-account streaming**: each account
flows through the whole chain in its own worker, so a finished account is
exported/imported immediately rather than waiting for the entire batch:

  per account:  [1] Register → [2] Join K12 workspace → [3] Approve request
                → [4] Refresh + check → [5] Append to access_token.txt
                → [6] Import to admin API

The individual stage functions (run_register / run_join_workspace / …) remain
batch-oriented and back the standalone subcommands.

Accounts saved incrementally to registered_accounts.json.
Final output: access_token.txt (+ optional push to the account-pool API).
"""

from __future__ import annotations

import json
import logging
import os
import signal
import sys
import threading
import time
import uuid
import weakref
from concurrent.futures import (
    FIRST_COMPLETED,
    ThreadPoolExecutor,
    as_completed,
    wait,
)
from concurrent.futures.thread import (  # type: ignore[attr-defined]
    _threads_queues,
    _worker as _tp_worker,
)
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from register.registrar import register_worker
from register.browser_registrar import browser_register_worker
from register.mail_provider import create_mailbox, release_mailbox
from register.headers import USER_AGENT
from workspace.joiner import (
    join_workspaces,
    approve_join_request,
    load_manager_session,
    manager_session_is_expired,
)
from login.login_flow import re_login_for_team_token
from export.sub2api import export_sub2api_json
from export.importer import import_accounts, import_access_token, ENDPOINT_PATH
from utils.proxy import (
    get_proxy_for_index,
    load_proxies_from_file,
)
from utils import logger as plog

logger = logging.getLogger(__name__)


class DaemonThreadPoolExecutor(ThreadPoolExecutor):
    """ThreadPoolExecutor with **daemon** workers.

    Stdlib pool threads are non-daemon; after ``shutdown(wait=False)`` they
    still block interpreter exit (``threading._shutdown`` joins them). Daemon
    workers let Ctrl+C return to the shell immediately while browsers die with
    the process.
    """

    def _adjust_thread_count(self) -> None:
        # Mirror CPython 3.12–3.14, but set daemon=True before start.
        try:
            if self._idle_semaphore.acquire(timeout=0):
                return
        except Exception:
            return super()._adjust_thread_count()  # type: ignore[misc]

        def weakref_cb(_, q=self._work_queue):
            q.put(None)

        num_threads = len(self._threads)
        if num_threads >= self._max_workers:
            return

        thread_name = f"{self._thread_name_prefix or self}_{num_threads}"
        # 3.13+ uses worker context; older used initializer/initargs.
        if hasattr(self, "_create_worker_context"):
            worker_args: tuple[Any, ...] = (
                weakref.ref(self, weakref_cb),
                self._create_worker_context(),
                self._work_queue,
            )
        else:
            worker_args = (
                weakref.ref(self, weakref_cb),
                self._work_queue,
                self._initializer,
                self._initargs,
            )
        t = threading.Thread(
            name=thread_name,
            target=_tp_worker,
            args=worker_args,
            daemon=True,
        )
        t.start()
        self._threads.add(t)
        _threads_queues[t] = self._work_queue


def _flush_and_force_exit(code: int = 130) -> None:
    """Flush durable state then exit without joining non-daemon leftovers."""
    try:
        from register.mail_provider import _flush_state_on_exit

        _flush_state_on_exit()
    except Exception:
        pass
    try:
        plog.board_stop()
    except Exception:
        pass
    # Skip atexit thread joins (Playwright/driver can hang for minutes).
    os._exit(code)


def _resolve_single_proxy(proxy_cfg: dict) -> str:
    """Return a single proxy URL for sequential stages, preferring proxies_file."""
    proxies_file = str(proxy_cfg.get("proxies_file", "")).strip()
    default_proto = str(proxy_cfg.get("default_protocol", "socks5")).strip() or "socks5"
    if proxies_file:
        proxies = load_proxies_from_file(proxies_file, default_protocol=default_proto)
        if proxies:
            return proxies[0]
    return str(proxy_cfg.get("url", "")).strip()


def _proxy_pool(proxy_cfg: dict) -> list[str]:
    """Load the proxy pool (empty list if none configured)."""
    proxies_file = str(proxy_cfg.get("proxies_file", "")).strip()
    default_proto = str(proxy_cfg.get("default_protocol", "socks5")).strip() or "socks5"
    if proxies_file:
        return load_proxies_from_file(proxies_file, default_protocol=default_proto)
    return []


def _proxy_for(pool: list[str], single: str, index: int) -> str:
    """Pick a proxy for a 1-based index: rotate the pool, else the single proxy."""
    if pool:
        return get_proxy_for_index(pool, index)
    return single


def _pipeline_workers(config: dict, n_items: int) -> int:
    """Worker count for the parallel post-registration stages.

    Reuses ``registration.threads`` as the shared concurrency knob and never
    spawns more workers than there are items.
    """
    threads = int(config.get("registration", {}).get("threads", 3) or 3)
    return max(1, min(threads, max(1, n_items)))


def _tag(index: int, total: int, email: str = "") -> str:
    """Per-task log prefix so concurrent lines can be grouped by account.

    Example: ``#3/10 user@outlook.com`` — grep by ``#3`` or the email.
    (No square brackets — avoids Rich markup clashes.)
    """
    return plog.tag(index, total, email)


def _stage_done(name: str, count: int, wall: float, durations: list[float] | None = None) -> None:
    """Log a stage's total elapsed + average per account."""
    if count <= 0:
        logger.info(f"{name} done: 0 accounts | total {wall:.1f}s")
        return
    # For registration we have real per-account durations; otherwise amortize wall.
    avg = (sum(durations) / len(durations)) if durations else (wall / count)
    logger.info(
        f"{name} done: {count} accounts | total {wall:.1f}s | avg {avg:.1f}s/account"
    )


def _refresh_account(
    account: dict[str, Any],
    proxy: str,
    tag: str,
    preferred_workspace_ids: list[str] | None = None,
    *,
    tls_retries: int = 2,
    request_timeout: float = 25.0,
) -> str:
    """Refresh one account's access token + enrich via check API (mutates in place).

    Returns a coarse status for the caller to decide skip/retry::

      ``k12``       — plan is k12/team/…
      ``free``      — still personal/free (soft; may retry elevate)
      ``tls_fail``  — proxy/TLS exhausted (hard; skip further elevate)
      ``auth_fail`` — token invalid / 401 (hard; skip further elevate)
      ``error``     — other failure

    Shared by the batch stage (:func:`run_refresh_tokens`) and the streaming
    pipeline so the network logic lives in one place.
    """
    from utils.session_auth import (
        elevate_session_to_workspace,
        pick_account_from_check,
        refresh_via_session,
    )
    from utils.jwt import extract_account_info

    preferred = [
        str(x).strip()
        for x in (preferred_workspace_ids or [])
        if str(x).strip()
    ]
    # Also accept workspace ids recorded at join time.
    for jr in account.get("join_results") or []:
        if isinstance(jr, dict) and jr.get("ok") and jr.get("workspace_id"):
            wid = str(jr["workspace_id"]).strip()
            if wid and wid not in preferred:
                preferred.append(wid)

    from utils.proxy import (
        create_session as _create_http_session,
        is_tls_or_transport_error,
        normalize_proxy_url,
    )

    rt = str(account.get("refresh_token") or "").strip()
    st = str(account.get("session_token") or "").strip()
    proxy = normalize_proxy_url(proxy)
    req_timeout = max(8.0, float(request_timeout or 25.0))
    # Keep TLS retries short — bad residential proxies should not burn minutes.
    max_tls = max(1, min(int(tls_retries or 2), 4))

    session = None
    impersonate_cycle = ("chrome", "chrome131", "chrome124", "safari17_0")[
        :max_tls
    ]
    outcome = "free"
    saw_tls_fail = False
    saw_auth_fail = False

    def _new_session(impersonate: str = "chrome"):
        nonlocal session
        if session is not None:
            try:
                session.close()
            except Exception:
                pass
        try:
            session = _create_http_session(
                proxy=proxy, impersonate=impersonate, verify=False
            )
        except Exception:
            session = _create_http_session(
                proxy=proxy, impersonate="chrome", verify=False
            )
        return session

    def _plan_is_teamish() -> bool:
        plan = str(account.get("plan_type") or "").lower()
        if plan in ("k12", "team", "enterprise", "edu", "business"):
            return True
        at = str(account.get("access_token") or "")
        if at:
            jp = str(extract_account_info(at).get("plan_type") or "").lower()
            if jp in ("k12", "team", "enterprise", "edu", "business"):
                return True
        return False

    try:
        _new_session("chrome")

        # Step 1: Refresh the access token (with short TLS retries)
        if rt:
            last_tls = None
            for attempt, imp in enumerate(impersonate_cycle, start=1):
                try:
                    if attempt > 1:
                        _new_session(imp)
                    resp = session.post(
                        "https://auth.openai.com/oauth/token",
                        data={
                            "client_id": "app_2SKx67EdpoN0G6j64rFvigXD",
                            "grant_type": "refresh_token",
                            "refresh_token": rt,
                        },
                        headers={
                            "Content-Type": "application/x-www-form-urlencoded"
                        },
                        timeout=req_timeout,
                    )
                    last_tls = None
                    if resp.status_code == 200:
                        try:
                            data = resp.json()
                        except Exception as je:
                            logger.warning(
                                f"{tag} Token refresh error: {je}"
                            )
                            saw_auth_fail = True
                            break
                        new_at = data.get("access_token", "")
                        new_rt = data.get("refresh_token", "")
                        if new_at:
                            account["access_token"] = new_at
                        if new_rt:
                            account["refresh_token"] = new_rt
                        logger.info(f"{tag} Token refreshed (refresh_token)")
                    elif resp.status_code in (400, 401, 403):
                        logger.warning(
                            f"{tag} Token refresh failed: HTTP {resp.status_code}"
                        )
                        saw_auth_fail = True
                    else:
                        logger.warning(
                            f"{tag} Token refresh failed: HTTP {resp.status_code}"
                        )
                    break
                except Exception as e:
                    last_tls = e
                    if is_tls_or_transport_error(e) and attempt < len(
                        impersonate_cycle
                    ):
                        logger.warning(
                            f"{tag} TLS/proxy on refresh attempt {attempt}: "
                            f"{e} — retry"
                        )
                        time.sleep(0.6 * attempt)
                        continue
                    if is_tls_or_transport_error(e):
                        saw_tls_fail = True
                    logger.warning(f"{tag} Token refresh error: {e}")
                    break
            if last_tls and is_tls_or_transport_error(last_tls):
                saw_tls_fail = True
                logger.warning(f"{tag} Token refresh gave up after TLS retries")
        elif st:
            last_tls = None
            for attempt in range(1, max_tls + 1):
                try:
                    if attempt > 1:
                        _new_session(
                            impersonate_cycle[
                                min(attempt - 1, len(impersonate_cycle) - 1)
                            ]
                        )
                    fields = refresh_via_session(
                        st, proxy=proxy, timeout=req_timeout
                    )
                    if fields.get("access_token"):
                        account["access_token"] = fields["access_token"]
                    if fields.get("session_token"):
                        account["session_token"] = fields["session_token"]
                        st = fields["session_token"]
                    if fields.get("chatgpt_account_id"):
                        account["chatgpt_account_id"] = fields[
                            "chatgpt_account_id"
                        ]
                    if fields.get("plan_type"):
                        account["plan_type"] = fields["plan_type"]
                    if fields.get("expires"):
                        account["expires"] = fields["expires"]
                    logger.info(
                        f"{tag} Token refreshed (session_token) "
                        f"plan={fields.get('plan_type') or '?'}"
                    )
                    last_tls = None
                    break
                except Exception as e:
                    last_tls = e
                    if is_tls_or_transport_error(e) and attempt < max_tls:
                        logger.warning(
                            f"{tag} Session refresh TLS attempt {attempt}: {e}"
                        )
                        time.sleep(0.8 * attempt)
                        continue
                    if is_tls_or_transport_error(e):
                        saw_tls_fail = True
                    logger.warning(f"{tag} Session refresh failed: {e}")
                    break
            if last_tls and is_tls_or_transport_error(last_tls):
                saw_tls_fail = True
        else:
            logger.warning(
                f"{tag} No refresh_token or session_token — skipping refresh"
            )

        if saw_tls_fail and not str(account.get("access_token") or "").strip():
            return "tls_fail"

        # Step 1b: session-cookie elevate when still free after join.
        plan_now = str(account.get("plan_type") or "").lower()
        jwt_plan = ""
        at0 = str(account.get("access_token") or "")
        if at0:
            jwt_plan = str(extract_account_info(at0).get("plan_type") or "").lower()
        teamish_now = plan_now in ("k12", "team", "enterprise", "edu", "business") or (
            jwt_plan in ("k12", "team", "enterprise", "edu", "business")
        )
        need_elevate = (
            bool(preferred)
            and account.get("join_status") == "ok"
            and not teamish_now
        )
        if need_elevate and st:
            logger.info(
                f"{tag} Elevating session → workspace "
                f"{preferred[0][:8]}… (current plan={plan_now or jwt_plan or 'free'})"
            )
            # One elevate try here; outer loop handles multi-pass + timeout.
            elevated = None
            try:
                elevated = elevate_session_to_workspace(
                    st,
                    preferred,
                    proxy=proxy,
                    timeout=int(max(8, req_timeout)),
                )
            except Exception as e:
                if is_tls_or_transport_error(e):
                    saw_tls_fail = True
                logger.warning(f"{tag} session elevate error: {e}")
                elevated = None
            if elevated and elevated.get("access_token"):
                account["access_token"] = elevated["access_token"]
                if elevated.get("session_token"):
                    account["session_token"] = elevated["session_token"]
                    st = elevated["session_token"]
                if elevated.get("chatgpt_account_id"):
                    account["chatgpt_account_id"] = elevated["chatgpt_account_id"]
                if elevated.get("plan_type"):
                    account["plan_type"] = elevated["plan_type"]
                if elevated.get("expires"):
                    account["expires"] = elevated["expires"]
                account["workspace_scope"] = "elevated"
                if _plan_is_teamish():
                    plog.milestone(
                        "K12",
                        tag,
                        f"plan={account.get('plan_type') or '?'} "
                        f"id={plog.short_id(account.get('chatgpt_account_id') or '', 12)}",
                        logger=logger,
                    )
                else:
                    logger.info(
                        f"{tag} elevate returned AT but plan still "
                        f"{account.get('plan_type') or 'empty'} — will check API"
                    )
            else:
                logger.warning(
                    f"{tag} session elevate not k12 yet "
                    f"(will rely on check API / later retry)"
                )
        elif need_elevate and not st:
            if preferred and not account.get("chatgpt_account_id"):
                account["chatgpt_account_id"] = preferred[0]
            logger.info(
                f"{tag} no session_token — skip cookie elevate, "
                f"check API with workspace {preferred[0][:8]}…"
            )

        # Step 2: check API
        at = account.get("access_token", "")
        if at:
            device_id = account.get("device_id") or str(uuid.uuid4())
            account["device_id"] = device_id
            check_headers = {
                "accept": "*/*",
                "authorization": f"Bearer {at}",
                "content-type": "application/json",
                "oai-device-id": device_id,
                "oai-language": "en-US",
                "user-agent": USER_AGENT,
            }
            prefer_id = str(
                account.get("chatgpt_account_id")
                or (preferred[0] if preferred else "")
            ).strip()
            if prefer_id:
                check_headers["chatgpt-account-id"] = prefer_id

            resp = None
            for attempt in range(max_tls):
                try:
                    if attempt > 0 and session is not None:
                        try:
                            _new_session(
                                impersonate_cycle[
                                    min(attempt, len(impersonate_cycle) - 1)
                                ]
                            )
                        except Exception:
                            pass
                    resp = session.get(
                        "https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27",
                        headers=check_headers,
                        timeout=req_timeout,
                    )
                    if (
                        resp.status_code not in (403, 429)
                        and resp.status_code < 500
                    ):
                        break
                except Exception as e:
                    if is_tls_or_transport_error(e) and attempt + 1 < max_tls:
                        logger.warning(
                            f"{tag} check API TLS attempt {attempt + 1}: {e}"
                        )
                        time.sleep(0.8 * (attempt + 1))
                        continue
                    if is_tls_or_transport_error(e):
                        saw_tls_fail = True
                        logger.warning(f"{tag} check API TLS gave up: {e}")
                        resp = None
                        break
                    raise
                if attempt + 1 < max_tls:
                    time.sleep(0.8 * (attempt + 1))

            if resp is None:
                if saw_tls_fail:
                    return "tls_fail"
                return "error"

            if resp.status_code == 200:
                data = resp.json()
                picked = pick_account_from_check(data, preferred)
                plan = picked.get("plan_type", "")
                acct_id = picked.get("account_id", "")
                role = picked.get("role", "")
                if plan:
                    account["plan_type"] = plan
                if acct_id:
                    account["chatgpt_account_id"] = acct_id
                if role:
                    account["account_user_role"] = role
                if str(plan or "").lower() in (
                    "k12",
                    "team",
                    "enterprise",
                    "edu",
                    "business",
                ):
                    if account.get("workspace_scope") != "elevated":
                        account["workspace_scope"] = "check"
                    plog.milestone(
                        "K12",
                        tag,
                        f"plan={plan} id={plog.short_id(acct_id, 12)} (check)",
                        logger=logger,
                    )
                try:
                    account["check_accounts"] = {
                        k: (v.get("account") or {})
                        for k, v in (data.get("accounts") or {}).items()
                        if isinstance(v, dict)
                    }
                except Exception:
                    pass
                plan_l = (plan or "").lower()
                detail = (
                    f"plan={plan or '?'} "
                    f"id={plog.short_id(acct_id, 12)} "
                    f"role={role or '?'}"
                )
                if plan_l == "k12":
                    plog.milestone("CHECK", tag, detail, logger=logger)
                else:
                    logger.info(plog.format_info(f"{tag} CHECK {detail}"))
            elif resp.status_code == 401:
                saw_auth_fail = True
                logger.warning(f"{tag} Check API 401 — access_token invalid/expired")
            elif resp.status_code == 403:
                logger.warning(
                    f"{tag} Check API 403 — likely Cloudflare/WAF block "
                    f"(proxy IP flagged or headers rejected)"
                )
            else:
                logger.warning(f"{tag} Check API failed: HTTP {resp.status_code}")
        elif saw_tls_fail:
            return "tls_fail"
        elif saw_auth_fail:
            return "auth_fail"

        if _plan_is_teamish():
            return "k12"
        if saw_auth_fail:
            return "auth_fail"
        if saw_tls_fail:
            return "tls_fail"
        return "free"
    except Exception as e:
        logger.warning(f"{tag} Refresh/check error: {e}")
        if is_tls_or_transport_error(e):
            return "tls_fail"
        return "error"
    finally:
        if session:
            session.close()


# ── Manager approval of K12 join requests ───────────────────────────


class _ManagerApprover:
    """Approves K12 join requests with the parent workspace's manager token.

    Approves one account at a time, matched by its email
    (``approve_join_request`` → PATCH the specific invite). Precise — it never
    admits requests that aren't ours — and takes effect synchronously (the
    bulk accept-all endpoint is eventually consistent, so we don't use it).
    All calls go out on a single fixed proxy so the manager identity stays on
    one IP.

    Concurrent pipeline workers share one approver: approvals are serialized
    with a lock so list+PATCH against the manager token does not race.
    """

    def __init__(self, token: str, account_id: str, proxy: str, max_attempts: int = 6) -> None:
        self.token = token
        self.account_id = account_id
        self.proxy = proxy
        self.max_attempts = max(1, int(max_attempts))
        self._lock = threading.Lock()

    def approve(self, email: str, tag: str) -> bool:
        # Serialize manager API (shared token / invite list under concurrency).
        with self._lock:
            res = approve_join_request(
                self.token, self.account_id, email,
                proxy=self.proxy, max_attempts=self.max_attempts,
            )
        if res.get("ok"):
            tries = res.get("attempts", 1)
            suffix = f" (after {tries} tries)" if tries and tries > 1 else ""
            plog.milestone(
                "APPR", tag, f"{plog.short_email(email)}{suffix}", logger=logger
            )
            return True
        code = res.get("status_code")
        if code == 401:
            plog.fail(
                "APPR",
                tag,
                "HTTP 401 — refresh session.json",
                logger=logger,
            )
        elif res.get("matched") is False and not str(res.get("error", "")).startswith("list"):
            plog.fail(
                "APPR",
                tag,
                f"no pending after {res.get('attempts', '?')} tries",
                logger=logger,
            )
        else:
            detail = res.get("error") or str(res.get("body") or "")[:80]
            plog.fail("APPR", tag, f"HTTP {code} {detail}".rstrip(), logger=logger)
        return False


def _build_approver(config: dict[str, Any]) -> _ManagerApprover | None:
    """Build a manager approver from config, or None if disabled/unusable.

    Reads ``[workspace] approve_requests`` + ``manager_session_file`` and the
    manager proxy (a single stable proxy for the manager identity). Logs and
    returns None on any problem so the pipeline still runs (children just stay
    pending until approved manually).
    """
    ws_cfg = config.get("workspace", {})
    if not ws_cfg.get("approve_requests", False):
        return None

    session_file = str(ws_cfg.get("manager_session_file", "session.json")).strip()
    if not session_file:
        logger.warning("approve_requests on but manager_session_file empty — skipping approval")
        return None

    path = Path(session_file)
    if not path.is_absolute():
        path = Path(config.get("_config_dir", ".")) / path

    try:
        sess = load_manager_session(path)
    except Exception as e:
        logger.warning(f"Cannot load manager session ({path}): {e} — join requests won't be approved")
        return None

    account_id = sess["account_id"]
    if not account_id:
        # Fall back to the first configured workspace id.
        ids = ws_cfg.get("ids", []) or []
        account_id = str(ids[0]).strip() if ids else ""
    if not account_id:
        logger.warning("Manager session has no account id and no workspace ids configured — skipping approval")
        return None

    if manager_session_is_expired(sess.get("expires", "")):
        logger.warning(
            f"Manager session expired at {sess.get('expires')} — approvals will likely fail (HTTP 401)"
        )

    manager_proxy = _resolve_single_proxy(config.get("proxy", {}))
    max_attempts = int(ws_cfg.get("approve_max_attempts", 12) or 12)
    logger.info(
        f"Manager approval enabled (workspace {account_id}, session {path.name}, "
        f"max_attempts={max_attempts})"
    )
    return _ManagerApprover(sess["token"], account_id, manager_proxy, max_attempts=max_attempts)





def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _timestamp() -> str:
    return datetime.now().strftime("%Y%m%d-%H%M%S")


def load_accounts(path: Path) -> list[dict[str, Any]]:
    """Load registered accounts from JSON file."""
    if not path.exists():
        return []
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
        return data if isinstance(data, list) else []
    except Exception:
        return []


def save_accounts(path: Path, accounts: list[dict[str, Any]]) -> None:
    """Save accounts to JSON file (atomic write)."""
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(accounts, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    tmp.replace(path)


# ── Pipeline stages ─────────────────────────────────────────────────


def _registration_mode(config: dict[str, Any]) -> str:
    """Return ``browser`` or ``protocol`` (default)."""
    mode = str(config.get("registration", {}).get("mode", "protocol") or "protocol")
    mode = mode.strip().lower()
    if mode in ("browser", "playwright", "ui"):
        return "browser"
    return "protocol"


def _browser_cfg(config: dict[str, Any]) -> dict[str, Any]:
    """Browser-registration settings (nested under ``registration.browser``)."""
    reg = config.get("registration", {}) or {}
    raw = reg.get("browser")
    if isinstance(raw, dict):
        return raw
    # Also accept flat keys for convenience.
    return {
        "headless": reg.get("headless", False),
        "channel": reg.get("browser_channel", reg.get("channel", "")),
        "timeout_ms": reg.get("timeout_ms", 120_000),
        "signup_url": reg.get(
            "signup_url", "https://auth.openai.com/create-account"
        ),
        "slow_mo_ms": reg.get("slow_mo_ms", 0),
        "keep_open_on_error": reg.get("keep_open_on_error", False),
    }


def _submit_register(
    *,
    mode: str,
    index: int,
    proxy: str,
    flaresolverr_url: str,
    mail_config: dict,
    browser_cfg: dict,
    mailbox: dict | None = None,
    jsd_url: str = "",
):
    """Dispatch to protocol or browser worker (same result shape)."""
    if mode == "browser":
        return browser_register_worker(
            index=index,
            proxy=proxy,
            flaresolverr_url=flaresolverr_url,
            mail_config=mail_config,
            browser_cfg=browser_cfg,
            mailbox=mailbox,
        )
    return register_worker(
        index=index,
        proxy=proxy,
        flaresolverr_url=flaresolverr_url,
        jsd_url=jsd_url,
        mail_config=mail_config,
        mailbox=mailbox,
    )


def run_register(
    config: dict[str, Any],
    accounts_file: Path,
    count: int | None = None,
) -> list[dict[str, Any]]:
    """Stage 1: Register N ChatGPT accounts.

    Returns list of newly registered account records.
    """
    reg_cfg = config.get("registration", {})
    mail_cfg = config.get("mail", {})
    proxy_cfg = config.get("proxy", {})
    mode = _registration_mode(config)
    browser_cfg = _browser_cfg(config)

    total = count or int(reg_cfg.get("total", 10))
    threads = int(reg_cfg.get("threads", 3))
    # Browser instances are heavy; cap concurrency unless user insists.
    if mode == "browser" and threads > 3 and not reg_cfg.get("allow_high_browser_concurrency"):
        logger.info(
            f"Browser mode: clamping threads {threads} → 3 "
            f"(set registration.allow_high_browser_concurrency=true to override)"
        )
        threads = 3
    single_proxy = str(proxy_cfg.get("url", "")).strip()
    flaresolverr_url = str(proxy_cfg.get("flaresolverr_url", "")).strip()
    jsd_url = str(proxy_cfg.get("jsd_url", "") or "").strip()

    # Load proxy pool if proxies_file is configured
    proxies_file = str(proxy_cfg.get("proxies_file", "")).strip()
    default_proto = str(proxy_cfg.get("default_protocol", "socks5")).strip() or "socks5"
    proxies: list[str] = []
    if proxies_file:
        proxies = load_proxies_from_file(proxies_file, default_protocol=default_proto)
        if proxies:
            logger.info(f"Loaded {len(proxies)} proxies from {proxies_file} (protocol: {default_proto})")
        else:
            logger.warning(f"proxies_file specified but no proxies loaded from {proxies_file}")

    logger.info(
        f"Starting registration: {total} accounts, {threads} threads, mode={mode}"
    )
    if mode == "browser":
        eng = str(
            browser_cfg.get("engine") or browser_cfg.get("channel") or "chrome"
        )
        logger.info(
            f"Browser: engine={eng} headless={browser_cfg.get('headless', False)} "
            f"channel={browser_cfg.get('channel') or '-'} "
            f"signup_url={browser_cfg.get('signup_url') or 'create-account'}"
        )
    if proxies:
        logger.info(f"Using proxy pool rotation ({len(proxies)} proxies)")
    elif single_proxy:
        logger.info(f"Proxy: {single_proxy}")
    if jsd_url:
        logger.info(f"JSD pure-compute: {jsd_url}")
    if flaresolverr_url:
        logger.info(f"FlareSolverr: {flaresolverr_url}")

    results: list[dict[str, Any]] = []
    existing = load_accounts(accounts_file)
    success_count = 0
    fail_count = 0
    costs: list[float] = []
    stage_start = time.monotonic()
    per_account: list[dict[str, Any]] = []

    # Live board (same Rich board as full pipeline; JOIN/APPR/… stay pending)
    plog.board_start(total, mode=mode)

    # Pre-allocate mailboxes so the board shows emails immediately.
    pre_mailboxes: dict[int, dict] = {}
    for i in range(1, total + 1):
        try:
            mb = create_mailbox(mail_cfg)
            addr = str(mb.get("address") or "").strip()
            if not addr:
                release_mailbox(mb)
                logger.warning(f"{_tag(i, total)} pre-allocate mailbox: empty address")
                continue
            pre_mailboxes[i] = mb
            plog.board_bind(
                i,
                addr,
                note="queued · protocol" if mode == "protocol" else "queued",
            )
        except Exception as e:
            logger.warning(f"{_tag(i, total)} pre-allocate mailbox failed: {e}")

    Executor = DaemonThreadPoolExecutor
    with Executor(max_workers=threads, thread_name_prefix="reg") as executor:
        futures = {}
        for i in range(1, total + 1):
            if proxies:
                task_proxy = get_proxy_for_index(proxies, i)
            else:
                task_proxy = single_proxy
            pre_mb = pre_mailboxes.pop(i, None)
            if pre_mb is not None:
                plog.board_bind(
                    i,
                    str(pre_mb.get("address") or ""),
                    note="protocol register…" if mode == "protocol" else "launch…",
                )
            futures[
                executor.submit(
                    _submit_register,
                    mode=mode,
                    index=i,
                    proxy=task_proxy,
                    flaresolverr_url=flaresolverr_url,
                    mail_config=mail_cfg,
                    browser_cfg=browser_cfg,
                    mailbox=pre_mb,
                    jsd_url=jsd_url,
                )
            ] = i

        for future in as_completed(futures):
            idx = futures.get(future, 0)
            try:
                result = future.result()
            except Exception as e:
                fail_count += 1
                plog.fail("REG", _tag(int(idx) if idx else 0, total), str(e), logger=logger)
                continue
            cost = float(result.get("cost_seconds", 0) or 0)
            costs.append(cost)
            if result["ok"]:
                success_count += 1
                account = result["result"]
                results.append(account)
                existing.append(account)
                save_accounts(accounts_file, existing)
                email = str(account.get("email") or "")
                tag = _tag(result["index"], total, email)
                plog.milestone("REG", tag, f"{cost:.1f}s", logger=logger)
                plog.board_status(
                    int(result["index"]),
                    f"registered · {cost:.1f}s",
                    email=email,
                )
                per_account.append(
                    {
                        "index": int(result["index"]),
                        "email": email,
                        "join": "-",
                        "plan": account.get("plan_type") or "-",
                        "has_token": bool(account.get("access_token")),
                        "imported": None,
                    }
                )
            else:
                fail_count += 1
                err = str(result.get("error", "unknown"))
                plog.fail(
                    "REG",
                    _tag(int(result.get("index") or idx or 0), total),
                    err[:120],
                    logger=logger,
                )
                plog.board_status(
                    int(result.get("index") or idx or 0),
                    f"fail · {err[:48]}",
                )

    wall = time.monotonic() - stage_start
    try:
        plog.board_stop()
    except Exception:
        pass
    logger.info(
        f"Registration complete: {success_count} success, {fail_count} failed"
    )
    _stage_done("Registration", success_count + fail_count, wall, durations=costs or None)
    # Compact summary card (same helper as full pipeline when board was used)
    try:
        plog.print_run_summary(
            {
                "registered": success_count,
                "joined": 0,
                "refreshed": 0,
                "exported": success_count,
                "elapsed_seconds": round(wall, 1),
                "avg_seconds": round(
                    (sum(costs) / len(costs)) if costs else 0.0, 1
                ),
                "per_account": sorted(per_account, key=lambda r: r["index"]),
                "accounts_file": str(accounts_file),
                "access_tokens_file": "",
                "import": {},
                "reg_failed": fail_count,
            }
        )
    except Exception:
        pass
    return results


def run_join_workspace(
    config: dict[str, Any],
    accounts: list[dict[str, Any]],
) -> list[dict[str, Any]]:
    """Stage 2: Join each account to the K12 parent workspace.

    Modifies account records in-place with join status.
    """
    ws_cfg = config.get("workspace", {})
    if not ws_cfg.get("enabled", True):
        logger.info("Workspace join disabled — skipping")
        return accounts

    workspace_ids = ws_cfg.get("ids", [])
    if not workspace_ids:
        logger.warning("No workspace IDs configured — skipping join")
        return accounts

    route = str(ws_cfg.get("route", "request")).strip() or "request"
    max_retries = int(ws_cfg.get("max_retries", 3))
    retry_backoff = int(ws_cfg.get("retry_backoff_ms", 5000))
    proxy_cfg = config.get("proxy", {})
    pool = _proxy_pool(proxy_cfg)
    single_proxy = _resolve_single_proxy(proxy_cfg)

    workers = _pipeline_workers(config, len(accounts))
    total = len(accounts)
    logger.info(
        f"Joining {total} accounts to {len(workspace_ids)} workspace(s) "
        f"({workers} workers)"
    )
    stage_start = time.monotonic()

    def _join_one(index: int, account: dict[str, Any]) -> None:
        email = account.get("email", "?")
        tag = _tag(index, total, email)
        access_token = account.get("access_token", "")
        if not access_token:
            logger.warning(f"{tag} No access_token — skipping join")
            account["join_status"] = "skipped"
            return

        try:
            results = join_workspaces(
                access_token=access_token,
                workspace_ids=workspace_ids,
                route=route,
                max_retries=max_retries,
                retry_backoff_ms=retry_backoff,
                proxy=_proxy_for(pool, single_proxy, index),
            )
        except Exception as e:  # defensive — join_workspaces normally self-handles
            account["join_status"] = "failed"
            account["join_results"] = [{"ok": False, "error": str(e)}]
            logger.warning(f"{tag} ✗ Join error: {e}")
            return

        all_ok = all(r["ok"] for r in results)
        account["join_status"] = "ok" if all_ok else "failed"
        account["join_results"] = results

        if all_ok:
            plog.milestone(
                "JOIN", tag, f"{len(workspace_ids)} workspace", logger=logger
            )
        else:
            errors = [r.get("error", "?") for r in results if not r["ok"]]
            logger.warning(f"{tag} ✗ Join failed: {', '.join(errors)}")

    with ThreadPoolExecutor(max_workers=workers) as executor:
        futures = [
            executor.submit(_join_one, i, account)
            for i, account in enumerate(accounts, 1)
        ]
        for future in as_completed(futures):
            future.result()  # surface unexpected exceptions

    _stage_done("Workspace join", total, time.monotonic() - stage_start)
    return accounts


def run_re_login(
    config: dict[str, Any],
    accounts: list[dict[str, Any]],
) -> list[dict[str, Any]]:
    """Stage 3: Re-login each account with Team space selection.

    Gets team-scoped tokens for accounts that successfully joined.
    NOTE: This step requires browser-based OAuth login flow and is
    currently skipped by default. Use registration tokens directly.
    """
    ws_cfg = config.get("workspace", {})
    re_login_enabled = ws_cfg.get("re_login_enabled", False)

    if not re_login_enabled:
        logger.info("Team re-login disabled — using registration tokens for export")
        for account in accounts:
            account["team_login_status"] = "skipped"
        return accounts

    mail_cfg = config.get("mail", {})
    proxy_cfg = config.get("proxy", {})
    proxy = _resolve_single_proxy(proxy_cfg)
    flaresolverr_url = str(proxy_cfg.get("flaresolverr_url", "")).strip()
    workspace_ids = ws_cfg.get("ids", [])

    logger.info(f"Re-logging {len(accounts)} accounts for team-scoped tokens")

    for account in accounts:
        email = account.get("email", "")
        password = account.get("password", "")
        join_status = account.get("join_status", "")

        if join_status != "ok":
            logger.info(f"[{email}] Join failed/skipped — skipping re-login")
            account["team_login_status"] = "skipped"
            continue

        if not email or not password:
            logger.warning(f"[{email}] Missing email or password — skipping re-login")
            account["team_login_status"] = "skipped"
            continue

        try:
            team_tokens = re_login_for_team_token(
                email=email,
                password=password,
                mail_config=mail_cfg,
                proxy=proxy,
                flaresolverr_url=flaresolverr_url,
                workspace_id=workspace_ids[0] if workspace_ids else "",
            )

            # Store team-scoped tokens in a separate field
            account["team_access_token"] = team_tokens["access_token"]
            account["team_refresh_token"] = team_tokens["refresh_token"]
            account["team_id_token"] = team_tokens["id_token"]
            account["team_login_status"] = "ok"

            logger.info(f"[{email}] ✓ Team login successful")
        except Exception as e:
            logger.warning(f"[{email}] ✗ Team login failed: {e}")
            account["team_login_status"] = "failed"
            account["team_login_error"] = str(e)

    return accounts


def run_refresh_tokens(
    config: dict[str, Any],
    accounts: list[dict[str, Any]],
) -> list[dict[str, Any]]:
    """Refresh access tokens and enrich with workspace info from check API.

    After joining a workspace, refreshing the token ensures the token
    is valid for the current context.  Then we call /accounts/check
    to get the real plan_type and account_id (the JWT doesn't carry
    workspace claims).  Runs one worker per account concurrently.
    """
    from curl_cffi import requests

    proxy_cfg = config.get("proxy", {})
    pool = _proxy_pool(proxy_cfg)
    single_proxy = _resolve_single_proxy(proxy_cfg)
    ws_cfg = config.get("workspace", {}) or {}

    workers = _pipeline_workers(config, len(accounts))
    total = len(accounts)
    logger.info(
        f"Refreshing tokens and checking account info for {total} accounts "
        f"({workers} workers)"
    )
    stage_start = time.monotonic()

    # K12 join requests now need manager approval. These accounts already sent
    # their requests, so approve each by email just before its refresh.
    approver = _build_approver(config)

    preferred_ws = [
        str(x).strip() for x in (ws_cfg.get("ids") or []) if str(x).strip()
    ]

    def _refresh_one(index: int, account: dict[str, Any]) -> None:
        tag = _tag(index, total, account.get("email", ""))
        if approver is not None:
            approver.approve(str(account.get("email") or ""), tag)
        _refresh_account(
            account,
            _proxy_for(pool, single_proxy, index),
            tag,
            preferred_workspace_ids=preferred_ws,
        )

    with ThreadPoolExecutor(max_workers=workers) as executor:
        futures = [
            executor.submit(_refresh_one, i, account)
            for i, account in enumerate(accounts, 1)
        ]
        for future in as_completed(futures):
            future.result()  # surface unexpected exceptions

    _stage_done("Token refresh", total, time.monotonic() - stage_start)
    return accounts


def run_export(
    config: dict[str, Any],
    accounts: list[dict[str, Any]],
    output_file: Path | None = None,
) -> str:
    """Stage 4: Export accounts as sub2api JSON.

    Uses team-scoped tokens (team_access_token) when available,
    falls back to personal tokens.
    """
    sub2api_cfg = config.get("sub2api", {})

    # Prepare accounts for export — use registration tokens directly
    # (Team-scoped tokens would require browser-based re-login, not yet implemented)
    export_accounts = []
    for account in accounts:
        export = dict(account)
        if account.get("team_login_status") == "ok":
            export["access_token"] = account.get("team_access_token", account.get("access_token", ""))
            export["refresh_token"] = account.get("team_refresh_token", account.get("refresh_token", ""))
            export["id_token"] = account.get("team_id_token", account.get("id_token", ""))
            export["source_type"] = "team_relogin"
        # else: use registration tokens as-is
        export_accounts.append(export)

    output_path = Path(output_file) if output_file else Path(
        config.get("_config_dir", ".")
    ) / f"sub2api-{_timestamp()}.json"

    json_str, actual_path = export_sub2api_json(export_accounts, output_path)
    logger.info(f"Exported {len(export_accounts)} accounts to {actual_path}")
    return actual_path


def run_export_access_tokens(
    config: dict[str, Any],
    accounts: list[dict[str, Any]],
    output_file: Path | None = None,
) -> str:
    """Export to access_token.txt

    One access_token per line.
    Prefers team_access_token if present, falls back to personal.
    """
    config_dir = Path(config.get("_config_dir", "."))
    if output_file:
        out_path = Path(output_file)
    else:
        out_path = config_dir / "access_token.txt"

    lines: list[str] = []
    for account in accounts:
        # prefer team-scoped if available
        at = (
            account.get("team_access_token")
            or account.get("access_token")
            or ""
        )
        at = str(at).strip()
        if at:
            lines.append(at)

    content = "\n".join(lines) + "\n" if lines else ""
    out_path.write_text(content, encoding="utf-8")
    logger.info(f"Exported {len(lines)} access tokens to {out_path}")
    return str(out_path)


def run_import_access_tokens(
    config: dict[str, Any],
    accounts: list[dict[str, Any]],
) -> dict[str, Any]:
    """Push each account's access_token to the external admin API.

    Driven by the ``import_api`` config section. Returns the import
    summary dict from :func:`export.importer.import_accounts`.
    """
    import_cfg = config.get("import_api", {})
    threads = _pipeline_workers(config, len(accounts))
    return import_accounts(accounts, import_cfg, threads=threads)


# ── Full pipeline ───────────────────────────────────────────────────


def run_full_pipeline(
    config: dict[str, Any],
    count: int | None = None,
    output_file: str | None = None,
    accounts_file: str | None = None,
) -> dict[str, Any]:
    """Run the pipeline with **decoupled browser registration**.

    Browser slots (``registration.threads``) only hold the REG step. As soon as
    an account has a session / access_token, the browser worker is freed and the
    next pending account starts registering. Join → approve → K12 → import run
    on a separate post-process pool so they never block new browser launches.

    Args:
        config: Full config dict from config.toml
        count: Override registration count
        output_file: (unused for access export, kept for compatibility)
        accounts_file: Override accounts storage path

    Returns:
        Summary dict with counts + timings + per-account breakdown.
    """
    from curl_cffi import requests

    config_dir = Path(config.get("_config_dir", "."))
    af = Path(accounts_file) if accounts_file else config_dir / "registered_accounts.json"
    token_path = config_dir / "access_token.txt"

    reg_cfg = config.get("registration", {})
    total = count or int(reg_cfg.get("total", 10))
    threads = max(1, min(int(reg_cfg.get("threads", 3) or 3), total))
    mode = _registration_mode(config)
    browser_cfg = _browser_cfg(config)
    # reg | full | full_success — when to start the next account
    _gate_raw = str(reg_cfg.get("pipeline_gate") or "reg").strip().lower()
    if _gate_raw in ("full_success", "success", "all_success"):
        pipeline_gate = "full_success"
    elif _gate_raw in ("full", "complete", "serial_full", "wait_full"):
        pipeline_gate = "full"
    else:
        pipeline_gate = "reg"
    if mode == "browser" and threads > 3 and not reg_cfg.get("allow_high_browser_concurrency"):
        logger.info(
            f"Browser mode: clamping threads {threads} → 3 "
            f"(set registration.allow_high_browser_concurrency=true to override)"
        )
        threads = min(3, total)

    mail_cfg = config.get("mail", {})
    proxy_cfg = config.get("proxy", {})
    pool = _proxy_pool(proxy_cfg)
    single_proxy = str(proxy_cfg.get("url", "")).strip()
    flaresolverr_url = str(proxy_cfg.get("flaresolverr_url", "")).strip()
    jsd_url = str(proxy_cfg.get("jsd_url", "") or "").strip()
    if pool:
        logger.info(f"Loaded {len(pool)} proxies for rotation")
    if jsd_url:
        logger.info(f"JSD pure-compute: {jsd_url}")
    if flaresolverr_url:
        logger.info(f"FlareSolverr: {flaresolverr_url}")

    ws_cfg = config.get("workspace", {})
    ws_enabled = ws_cfg.get("enabled", True)
    workspace_ids = ws_cfg.get("ids", []) or []
    route = str(ws_cfg.get("route", "request")).strip() or "request"
    max_retries = int(ws_cfg.get("max_retries", 3))
    retry_backoff = int(ws_cfg.get("retry_backoff_ms", 5000))
    # Elevate stage: skip stuck TLS/auth instead of retrying for minutes.
    elevate_timeout_s = float(ws_cfg.get("elevate_timeout_s", 90) or 90)
    elevate_max_passes = max(1, min(int(ws_cfg.get("elevate_max_passes", 2) or 2), 5))
    elevate_tls_retries = max(1, min(int(ws_cfg.get("elevate_tls_retries", 2) or 2), 4))
    elevate_request_timeout = float(ws_cfg.get("elevate_request_timeout", 20) or 20)

    # Manager approval: after a child's join request, the parent workspace
    # admin must approve it before the child is really a K12 member.
    approver = _build_approver(config)

    import_cfg = config.get("import_api", {})
    import_enabled = bool(import_cfg.get("enabled", False))
    import_url = str(import_cfg.get("url", "")).strip().rstrip("/") + ENDPOINT_PATH
    admin_key = str(import_cfg.get("admin_key", "")).strip()

    # Fresh token file for this run; append as accounts complete.
    token_path.write_text("", encoding="utf-8")
    saved_accounts = load_accounts(af)  # preserve any prior history

    lock = threading.Lock()
    counters = {
        "registered": 0, "reg_failed": 0, "join_ok": 0, "refreshed_k12": 0,
        "imported_ok": 0, "added": 0, "updated": 0, "duplicate": 0, "import_failed": 0,
    }
    reg_costs: list[float] = []
    per_account: list[dict[str, Any]] = []

    # Post-process pool can be larger — no browser, only HTTP.
    post_threads = max(
        threads,
        min(total, int(reg_cfg.get("post_threads", 0) or threads * 3)),
    )

    pipeline_start = time.monotonic()
    if pipeline_gate == "reg":
        gate_desc = "REG done → next"
    elif pipeline_gate == "full":
        gate_desc = "full pipeline done → next"
    else:
        gate_desc = "full SUCCESS only → next (fail stops queue)"
    logger.info(
        f"Pipeline start: {total} accounts, slots={threads}, "
        f"post_workers={post_threads}, mode={mode}, gate={pipeline_gate} "
        f"({gate_desc})"
    )
    plog.board_start(total, mode=mode)

    # Pre-allocate mailboxes so the live board shows emails immediately
    # (not empty / #1 placeholders while browsers start).
    pre_mailboxes: dict[int, dict] = {}
    for i in range(1, total + 1):
        try:
            mb = create_mailbox(mail_cfg)
            addr = str(mb.get("address") or "").strip()
            if not addr:
                release_mailbox(mb)
                logger.warning(f"{_tag(i, total)} pre-allocate mailbox: empty address")
                continue
            pre_mailboxes[i] = mb
            plog.board_bind(
                i,
                addr,
                note=(
                    "queued · waiting protocol slot"
                    if mode == "protocol"
                    else "queued · waiting browser slot"
                ),
            )
        except Exception as e:
            logger.warning(f"{_tag(i, total)} pre-allocate mailbox failed: {e}")

    # Stagger only between concurrent browser launches (not index×N seconds).
    stagger_s = 0.0
    if mode == "browser":
        eng = str(
            browser_cfg.get("engine") or browser_cfg.get("channel") or ""
        ).strip().lower()
        if "stagger_seconds" in browser_cfg:
            stagger_s = float(browser_cfg.get("stagger_seconds") or 0)
        elif "stagger_seconds" in reg_cfg:
            stagger_s = float(reg_cfg.get("stagger_seconds") or 0)
        else:
            stagger_s = 0.0 if eng in ("camoufox", "fox") else 1.2

    stop_event = threading.Event()
    interrupted = False
    launch_lock = threading.Lock()
    last_launch_t = [0.0]  # mutable box for nested funcs

    def _sleep_interruptible(seconds: float) -> bool:
        """Sleep in small slices; return False if stop was requested."""
        end = time.monotonic() + max(0.0, seconds)
        while time.monotonic() < end:
            if stop_event.is_set():
                return False
            time.sleep(min(0.2, max(0.0, end - time.monotonic())))
        return not stop_event.is_set()

    def _throttle_browser_launch(index: int) -> bool:
        """Space out browser cold-starts across slots (not by account index)."""
        if mode != "browser" or stagger_s <= 0:
            return True
        with launch_lock:
            now = time.monotonic()
            wait_s = stagger_s - (now - last_launch_t[0])
            if wait_s > 0:
                logger.info(
                    f"{_tag(index, total)} browser launch throttle {wait_s:.1f}s"
                )
            else:
                wait_s = 0.0
            # Release lock while sleeping so other logic can progress; re-check.
        if wait_s > 0 and not _sleep_interruptible(wait_s):
            return False
        with launch_lock:
            last_launch_t[0] = time.monotonic()
        return not stop_event.is_set()

    def _do_register(index: int) -> dict[str, Any] | None:
        """Browser/protocol REG only. Returns payload for post pool, or None."""
        if stop_event.is_set():
            return None
        proxy = _proxy_for(pool, single_proxy, index)

        if not _throttle_browser_launch(index):
            logger.info(f"{_tag(index, total)} cancelled before browser (Ctrl+C)")
            return None
        if stop_event.is_set():
            return None

        pre_mb = pre_mailboxes.pop(index, None)
        if pre_mb is not None:
            addr = str(pre_mb.get("address") or "").strip()
            if addr:
                plog.board_bind(
                    index,
                    addr,
                    note=(
                        "protocol · start…"
                        if mode == "protocol"
                        else "launch browser…"
                    ),
                )

        res = _submit_register(
            mode=mode,
            index=index,
            proxy=proxy,
            flaresolverr_url=flaresolverr_url,
            mail_config=mail_cfg,
            browser_cfg=browser_cfg,
            mailbox=pre_mb,
            jsd_url=jsd_url,
        )

        cost = float(res.get("cost_seconds", 0) or 0)
        if stop_event.is_set():
            logger.info(f"{_tag(index, total)} stop after register (Ctrl+C)")
            if pre_mb is not None and not res.get("ok"):
                try:
                    release_mailbox(pre_mb)
                except Exception:
                    pass
            return None

        if not res.get("ok"):
            with lock:
                counters["reg_failed"] += 1
                reg_costs.append(cost)
            logger.warning(
                f"{_tag(index, total)} ✗ register: {res.get('error', 'unknown')}"
            )
            return None

        account = res["result"]
        email = str(account.get("email") or "")
        tag = _tag(index, total, email)
        if pipeline_gate == "reg":
            reg_note = f"{cost:.1f}s · slot free → next"
        else:
            reg_note = f"{cost:.1f}s · hold slot until full pipeline"
        plog.milestone("REG", tag, reg_note, logger=logger)
        has_rt = "yes" if account.get("refresh_token") else "no"
        has_st = "yes" if account.get("session_token") else "no"
        plog.board_status(
            index,
            f"REG ok · {cost:.1f}s · RT={has_rt} ST={has_st} · handoff JOIN…",
            email=email,
        )
        return {
            "index": index,
            "account": account,
            "cost": cost,
            "proxy": proxy,
            "email": email,
            "tag": tag,
        }

    def _do_post(payload: dict[str, Any]) -> dict[str, Any]:
        """JOIN → APPR → K12 → save → IMP (no browser).

        Returns ``{"full_success": bool, "index": int, ...}`` for pipeline_gate.
        """
        if stop_event.is_set() or not payload:
            return {"full_success": False, "index": 0, "reason": "empty"}
        index = int(payload["index"])
        account = payload["account"]
        cost = float(payload.get("cost") or 0)
        proxy = str(payload.get("proxy") or "")
        email = str(payload.get("email") or account.get("email") or "")
        tag = str(payload.get("tag") or _tag(index, total, email))
        imported: bool | None = None
        teamish = False

        try:
            if stop_event.is_set():
                return {"full_success": False, "index": index, "reason": "stopped"}

            n_ws = len(workspace_ids) if workspace_ids else 0
            ws_hint = ""
            if workspace_ids:
                first = str(workspace_ids[0] or "")
                ws_hint = f" · first={plog.short_id(first, 10)}"
            plog.board_status(
                index,
                (
                    f"join workspace ×{n_ws} · route={route or 'default'}{ws_hint}…"
                    if n_ws
                    else "join skipped · no workspace ids"
                ),
                email=email,
            )
            access_token = account.get("access_token", "")
            if not ws_enabled or not workspace_ids:
                account["join_status"] = "skipped"
            elif not access_token:
                account["join_status"] = "skipped"
            else:
                try:
                    jr = join_workspaces(
                        access_token=access_token,
                        workspace_ids=workspace_ids,
                        route=route,
                        max_retries=max_retries,
                        retry_backoff_ms=retry_backoff,
                        proxy=proxy,
                    )
                    all_ok = all(r["ok"] for r in jr)
                    account["join_status"] = "ok" if all_ok else "failed"
                    account["join_results"] = jr
                    if all_ok:
                        plog.milestone(
                            "JOIN",
                            tag,
                            f"{len(workspace_ids)} workspace",
                            logger=logger,
                        )
                    else:
                        errs = [r.get("error", "?") for r in jr if not r["ok"]]
                        plog.fail("JOIN", tag, ", ".join(errs), logger=logger)
                except Exception as e:
                    account["join_status"] = "failed"
                    plog.fail("JOIN", tag, str(e), logger=logger)

            if stop_event.is_set():
                return {"full_success": False, "index": index, "reason": "stopped"}

            # 3. Manager approve — multi-wave (join list lags under concurrency)
            approve_ok = approver is None  # no approver → treat as ok
            if approver is not None and account.get("join_status") == "ok":
                # Give OpenAI time to surface the pending request before first list.
                plog.board_status(
                    index,
                    "join settle 2s · wait pending list before approve…",
                    email=email,
                )
                _sleep_interruptible(2.0)
                waves = 3
                for wave in range(1, waves + 1):
                    if stop_event.is_set():
                        break
                    plog.board_status(
                        index,
                        f"manager approve · wave {wave}/{waves} · list+PATCH…",
                        email=email,
                    )
                    if approver.approve(email, tag):
                        approve_ok = True
                        account["approve_status"] = "ok"
                        break
                    account["approve_status"] = "failed"
                    # Longer gap between waves — dual-pool floods pending list.
                    if wave < waves:
                        plog.board_status(
                            index,
                            f"approve miss · not in pending yet · retry wave {wave + 1}…",
                            email=email,
                        )
                        _sleep_interruptible(2.5 + wave * 1.5)
                if approve_ok:
                    # OpenAI often needs 5–20s after PATCH before k12 is visible.
                    plog.board_status(
                        index,
                        "approve ok · membership settle 6s before elevate…",
                        email=email,
                    )
                    _sleep_interruptible(6.0)
                else:
                    plog.board_status(
                        index,
                        "approve failed · elevate anyway (membership lag?)…",
                        email=email,
                    )
                    _sleep_interruptible(1.0)
            elif account.get("join_status") != "ok":
                account["approve_status"] = "skipped"

            if stop_event.is_set():
                return {"full_success": False, "index": index, "reason": "stopped"}

            # 4. Elevate → K12 (deadline + hard-fail skip on TLS/auth)
            preferred = list(workspace_ids) if workspace_ids else None
            elev_deadline = time.monotonic() + max(15.0, elevate_timeout_s)
            elev_skip_reason = ""
            for elev_pass in range(1, elevate_max_passes + 1):
                if stop_event.is_set():
                    elev_skip_reason = "cancelled"
                    break
                remaining = elev_deadline - time.monotonic()
                if remaining <= 0:
                    elev_skip_reason = f"timeout {elevate_timeout_s:.0f}s"
                    break

                plan_before = str(account.get("plan_type") or "free") or "free"
                ws0 = plog.short_id(str(preferred[0]), 10) if preferred else "-"
                has_st = "yes" if account.get("session_token") else "no"
                plog.board_status(
                    index,
                    f"elevate → k12 · pass {elev_pass}/{elevate_max_passes} "
                    f"· plan={plan_before} · ws={ws0} · session_token={has_st} "
                    f"· t_left={remaining:.0f}s…",
                    email=email,
                )
                status = _refresh_account(
                    account,
                    proxy,
                    tag,
                    preferred_workspace_ids=preferred,
                    tls_retries=elevate_tls_retries,
                    request_timeout=min(
                        elevate_request_timeout, max(8.0, remaining)
                    ),
                )
                plan = str(account.get("plan_type") or "").lower()
                acct_id = plog.short_id(
                    str(account.get("chatgpt_account_id") or ""), 12
                )
                teamish = plan in ("k12", "team", "enterprise", "edu", "business")
                if teamish or status == "k12":
                    teamish = True
                    plog.board_status(
                        index,
                        f"elevate ok · plan={plan or account.get('plan_type') or '?'} "
                        f"· id={acct_id or '-'} · pass {elev_pass}/{elevate_max_passes}",
                        email=email,
                    )
                    break

                # Hard failures: proxy/TLS or dead token — skip, do not loop.
                if status in ("tls_fail", "auth_fail", "error"):
                    elev_skip_reason = status
                    plog.board_status(
                        index,
                        f"elevate skip · {status} · no more retries",
                        email=email,
                    )
                    break
                if account.get("join_status") != "ok":
                    elev_skip_reason = "join_not_ok"
                    break
                if elev_pass >= elevate_max_passes:
                    elev_skip_reason = (
                        f"still free after {elevate_max_passes} passes"
                    )
                    break

                remaining = elev_deadline - time.monotonic()
                if remaining < 5:
                    elev_skip_reason = f"timeout {elevate_timeout_s:.0f}s"
                    break

                # Soft free: brief re-approve + short wait (bounded by deadline).
                plog.board_status(
                    index,
                    f"still free after elevate · re-approve + retry "
                    f"({elev_pass}/{elevate_max_passes}) · plan={plan or 'free'}…",
                    email=email,
                )
                if approver is not None:
                    if approver.approve(email, tag):
                        approve_ok = True
                        account["approve_status"] = "ok"
                wait_s = min(3.0 * elev_pass, max(0.0, remaining - 2.0))
                if wait_s < 1.0:
                    elev_skip_reason = f"timeout {elevate_timeout_s:.0f}s"
                    break
                plog.board_status(
                    index,
                    f"wait membership {wait_s:.0f}s · then elevate again…",
                    email=email,
                )
                if not _sleep_interruptible(wait_s):
                    elev_skip_reason = "cancelled"
                    break

            if not teamish and account.get("join_status") == "ok":
                reason = elev_skip_reason or "still free after elevate"
                account["elevate_status"] = "skipped"
                account["elevate_skip_reason"] = reason
                plog.fail(
                    "K12",
                    tag,
                    f"skip elevate ({reason})",
                    logger=logger,
                )
            elif teamish:
                account["elevate_status"] = "ok"

            plan_disp = str(account.get("plan_type") or "?")
            acct_disp = plog.short_id(
                str(account.get("chatgpt_account_id") or ""), 12
            )
            plog.board_status(
                index,
                f"plan={plan_disp} · id={acct_disp or '-'} · save accounts…",
                email=email,
            )

            at = str(account.get("access_token") or "").strip()
            with lock:
                counters["registered"] += 1
                reg_costs.append(cost)
                if account.get("join_status") == "ok":
                    counters["join_ok"] += 1
                if str(account.get("plan_type") or "").lower() in (
                    "k12",
                    "team",
                    "enterprise",
                    "edu",
                ):
                    counters["refreshed_k12"] += 1
                saved_accounts.append(account)
                save_accounts(af, saved_accounts)
                # Always keep token file for debugging; import may still filter.
                if at:
                    with open(token_path, "a", encoding="utf-8") as f:
                        f.write(at + "\n")

            # 5. Import — only plan_type == k12 (free / elevate-fail never POST).
            plan_for_import = str(account.get("plan_type") or "").strip().lower()
            is_k12 = plan_for_import == "k12"
            import_require_k12 = bool(import_cfg.get("require_k12", True))
            if import_enabled and at and not stop_event.is_set():
                if import_require_k12 and not is_k12:
                    imported = False
                    account["import_status"] = "skipped"
                    account["import_skip_reason"] = f"plan={plan_for_import or 'free'}"
                    plog.board_status(
                        index,
                        f"IMP skip · plan={plan_disp or plan_for_import or 'free'} · not k12",
                        email=email,
                    )
                    plog.fail(
                        "IMP",
                        tag,
                        f"skip plan={plan_for_import or 'free'} (not k12)",
                        logger=logger,
                    )
                else:
                    plog.board_status(
                        index,
                        f"import → sub2api · plan={plan_disp} · POST…",
                        email=email,
                    )
                    sess = None
                    try:
                        sess = requests.Session(impersonate="chrome", verify=False)
                        r = import_access_token(
                            sess,
                            url=import_url,
                            admin_key=admin_key,
                            access_token=at,
                        )
                        outcome = r.get("outcome", "unknown")
                        if r.get("ok"):
                            imported = True
                            with lock:
                                counters["imported_ok"] += 1
                                if outcome in ("added", "updated", "duplicate"):
                                    counters[outcome] += 1
                            plog.milestone(
                                "IMP",
                                tag,
                                f"{outcome} · plan={plan_disp} · id={acct_disp or '-'}",
                                logger=logger,
                            )
                        else:
                            imported = False
                            with lock:
                                counters["import_failed"] += 1
                            err_imp = str(r.get("error", "?"))
                            plog.fail("IMP", tag, err_imp, logger=logger)
                            plog.board_status(
                                index,
                                f"IMP fail · {err_imp[:80]}",
                                email=email,
                            )
                    except Exception as e:
                        imported = False
                        with lock:
                            counters["import_failed"] += 1
                        plog.fail("IMP", tag, str(e), logger=logger)
                        plog.board_status(
                            index,
                            f"IMP fail · {str(e)[:80]}",
                            email=email,
                        )
                    finally:
                        if sess:
                            sess.close()

            with lock:
                per_account.append({
                    "index": index,
                    "email": email,
                    "join": account.get("join_status", "-"),
                    "plan": account.get("plan_type", "-"),
                    "has_token": bool(at),
                    "imported": imported,
                })

            # full_success: k12 when workspace on; import ok when import enabled.
            plan_ok = str(account.get("plan_type") or "").lower()
            success = True
            if ws_enabled and workspace_ids:
                success = plan_ok == "k12"
            if success and import_enabled:
                success = imported is True
            if success and not str(account.get("access_token") or "").strip():
                success = False
            if success:
                plog.board_status(
                    index,
                    f"full ok · plan={plan_ok or '?'} · ready for next"
                    if pipeline_gate != "reg"
                    else f"done · plan={plan_ok or '?'}",
                    email=email,
                )
            return {
                "full_success": success,
                "index": index,
                "plan": plan_ok,
                "imported": imported,
            }
        except Exception as e:
            if stop_event.is_set():
                logger.info(f"{tag} stopped (Ctrl+C): {e}")
                return {"full_success": False, "index": index, "reason": "stopped"}
            logger.exception(f"{tag} ✗ post-register pipeline error: {e}")
            with lock:
                per_account.append({
                    "index": index,
                    "email": email,
                    "join": account.get("join_status", "error"),
                    "plan": account.get("plan_type", "-"),
                    "has_token": bool(account.get("access_token")),
                    "imported": False,
                })
            return {"full_success": False, "index": index, "reason": str(e)[:120]}

    def _build_summary() -> dict[str, Any]:
        registered = counters["registered"]
        elapsed = time.monotonic() - pipeline_start
        avg = (sum(reg_costs) / len(reg_costs)) if reg_costs else 0.0
        rows = sorted(list(per_account), key=lambda r: r["index"])
        import_summary: dict[str, Any] = {}
        if import_enabled:
            import_summary = {
                "total": registered,
                "ok": counters["imported_ok"],
                "added": counters["added"],
                "updated": counters["updated"],
                "duplicate": counters["duplicate"],
                "failed": counters["import_failed"],
            }
        if registered == 0 and not interrupted:
            logger.error("No accounts registered — pipeline produced nothing")
        if interrupted:
            plog.fail(
                "DONE",
                "",
                f"INTERRUPTED reg={registered} join={counters['join_ok']} "
                f"k12={counters['refreshed_k12']} {elapsed:.1f}s",
                logger=logger,
            )
        return {
            "registered": registered,
            "joined": counters["join_ok"],
            "refreshed": counters["refreshed_k12"],
            "exported": registered,
            "elapsed_seconds": round(elapsed, 1),
            "avg_seconds": round(avg, 1),
            "per_account": rows,
            "accounts_file": str(af),
            "access_tokens_file": str(token_path),
            "import": import_summary,
            "interrupted": interrupted,
            "reg_failed": counters["reg_failed"],
        }

    reg_executor = DaemonThreadPoolExecutor(
        max_workers=threads,
        thread_name_prefix="reg",
    )
    # full / full_success: one account holds a slot through post → cap post workers
    post_workers = (
        min(post_threads, threads)
        if pipeline_gate in ("full", "full_success")
        else post_threads
    )
    post_executor = DaemonThreadPoolExecutor(
        max_workers=post_workers,
        thread_name_prefix="post",
    )

    # Seed slots; when to open next depends on pipeline_gate.
    next_idx = 1
    reg_futures: dict[Any, int] = {}
    post_futures: set[Any] = set()
    gate_stopped = False  # full_success: fail → stop queue

    def _submit_reg(i: int) -> None:
        fut = reg_executor.submit(_do_register, i)
        reg_futures[fut] = i

    def _in_flight() -> int:
        return len(reg_futures) + len(post_futures)

    def _maybe_start_next(*, why: str = "") -> None:
        """Start next account if gate + slot budget allow."""
        nonlocal next_idx, gate_stopped
        if gate_stopped or stop_event.is_set() or next_idx > total:
            return
        if pipeline_gate == "reg":
            # Only count REG slots (post is separate).
            if len(reg_futures) < threads:
                _submit_reg(next_idx)
                next_idx += 1
                if why:
                    logger.debug(f"gate=reg start #{next_idx - 1} ({why})")
            return
        # full / full_success: one in-flight account = reg or post occupies a slot
        if _in_flight() < threads:
            _submit_reg(next_idx)
            next_idx += 1
            if why:
                logger.debug(f"gate={pipeline_gate} start #{next_idx - 1} ({why})")

    while next_idx <= total and _in_flight() < threads:
        # reg mode: seed only reg futures; full mode: same seed of regs
        if pipeline_gate == "reg" and len(reg_futures) >= threads:
            break
        _submit_reg(next_idx)
        next_idx += 1
        if pipeline_gate == "reg" and len(reg_futures) >= threads:
            break

    # Double Ctrl+C → immediate force exit.
    sigint_hits = {"n": 0}
    prev_handler = signal.getsignal(signal.SIGINT)

    def _sigint_handler(signum, frame):  # noqa: ANN001
        sigint_hits["n"] += 1
        stop_event.set()
        if sigint_hits["n"] >= 2:
            try:
                sys.stderr.write("\nForce exit (2× Ctrl+C).\n")
                sys.stderr.flush()
            except Exception:
                pass
            _flush_and_force_exit(130)
        if callable(prev_handler) and prev_handler not in (
            signal.SIG_DFL,
            signal.SIG_IGN,
            signal.default_int_handler,
        ):
            try:
                prev_handler(signum, frame)
                return
            except KeyboardInterrupt:
                raise
            except Exception:
                pass
        raise KeyboardInterrupt

    try:
        signal.signal(signal.SIGINT, _sigint_handler)
    except Exception:
        prev_handler = None

    try:
        while reg_futures or post_futures:
            if stop_event.is_set() and not reg_futures and not post_futures:
                break

            wait_set = set(reg_futures.keys()) | set(post_futures)
            if not wait_set:
                break
            done, _ = wait(wait_set, timeout=0.5, return_when=FIRST_COMPLETED)
            if not done:
                if stop_event.is_set():
                    for fut in list(reg_futures):
                        fut.cancel()
                    for fut in list(post_futures):
                        fut.cancel()
                    break
                continue

            for fut in done:
                if fut in reg_futures:
                    idx = reg_futures.pop(fut)
                    try:
                        payload = fut.result()
                    except Exception as e:
                        logger.warning(
                            f"{_tag(idx, total)} register worker error: {e}"
                        )
                        payload = None

                    if payload and not stop_event.is_set():
                        # Hand off to post; slot still held in full modes.
                        post_futures.add(post_executor.submit(_do_post, payload))
                        if pipeline_gate == "reg":
                            _maybe_start_next(why="reg done → post")
                    else:
                        # REG failed / empty — free slot.
                        if pipeline_gate == "full_success":
                            gate_stopped = True
                            logger.warning(
                                f"{_tag(idx, total)} gate=full_success: REG failed — "
                                f"stop starting further accounts"
                            )
                        elif not stop_event.is_set():
                            _maybe_start_next(why="reg fail free slot")

                elif fut in post_futures:
                    post_futures.discard(fut)
                    post_ok = False
                    try:
                        post_res = fut.result() or {}
                        post_ok = bool(post_res.get("full_success"))
                    except Exception as e:
                        logger.warning(f"post worker error: {e}")
                        post_ok = False

                    if pipeline_gate == "reg":
                        pass  # next already started at REG end
                    elif pipeline_gate == "full":
                        _maybe_start_next(why="full pipeline done")
                    elif pipeline_gate == "full_success":
                        if post_ok:
                            _maybe_start_next(why="full success")
                        else:
                            gate_stopped = True
                            logger.warning(
                                "gate=full_success: pipeline not fully successful "
                                "(need k12"
                                + (" + import" if import_enabled else "")
                                + ") — stop starting further accounts"
                            )

    except KeyboardInterrupt:
        interrupted = True
        stop_event.set()
        logger.warning(
            "Ctrl+C — cancel pending · partial results saved · "
            "exit now (2× Ctrl+C = force kill)"
        )
        for fut in list(reg_futures) + list(post_futures):
            fut.cancel()
        try:
            reg_executor.shutdown(wait=False, cancel_futures=True)
        except TypeError:
            reg_executor.shutdown(wait=False)
        try:
            post_executor.shutdown(wait=False, cancel_futures=True)
        except TypeError:
            post_executor.shutdown(wait=False)
        try:
            plog.board_stop()
        except Exception:
            pass
        summary = _build_summary()
        summary["force_exit"] = True
        return summary
    else:
        try:
            reg_executor.shutdown(wait=True, cancel_futures=False)
        except TypeError:
            reg_executor.shutdown(wait=True)
        try:
            post_executor.shutdown(wait=True, cancel_futures=False)
        except TypeError:
            post_executor.shutdown(wait=True)
    finally:
        if prev_handler is not None:
            try:
                signal.signal(signal.SIGINT, prev_handler)
            except Exception:
                pass

    try:
        plog.board_stop()
    except Exception:
        pass
    return _build_summary()
