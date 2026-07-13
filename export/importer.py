"""Import access tokens into an external account-pool admin API.

Pushes each collected access_token to a sub2api-style admin endpoint:

    POST {url}/api/admin/accounts/at
    Header:  X-Admin-Key: <admin_secret>
    Body:    {"access_token": ...}

Config only needs the server base URL and the admin key.
Used as the final delivery step after register → join → refresh.
"""

from __future__ import annotations

import logging
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any

from curl_cffi import requests

logger = logging.getLogger(__name__)

REQUEST_TIMEOUT = 30
DEFAULT_THREADS = 8
ENDPOINT_PATH = "/api/admin/accounts/at"  # appended to the configured base URL


def _label(account: dict[str, Any], index: int) -> str:
    """Human-readable label for logs (email if present, else #index)."""
    return str(account.get("email") or "").strip() or f"#{index}"


def import_access_token(
    session: "requests.Session",
    *,
    url: str,
    admin_key: str,
    access_token: str,
    timeout: int = REQUEST_TIMEOUT,
) -> dict[str, Any]:
    """POST a single access_token to the admin API.

    The API returns HTTP 200 with a body like::

        {"message": "...", "success": 1, "updated": 0, "duplicate": 0, "failed": 0}

    so the real outcome comes from the body counters, not just the status code.

    Returns {"ok", "status", "outcome", "message", "error", "body"} where
    ``outcome`` ∈ {added, updated, duplicate, failed, unknown}.
    """
    try:
        resp = session.post(
            url,
            json={"access_token": access_token},
            headers={
                "X-Admin-Key": admin_key,
                "Content-Type": "application/json",
            },
            timeout=timeout,
        )
    except Exception as e:  # network / TLS / timeout
        return {"ok": False, "status": 0, "outcome": "failed", "message": "",
                "error": str(e), "body": ""}

    body = ""
    try:
        body = resp.text or ""
    except Exception:
        pass

    http_ok = 200 <= resp.status_code < 300
    if not http_ok:
        return {"ok": False, "status": resp.status_code, "outcome": "failed",
                "message": "", "error": f"HTTP {resp.status_code}: {body[:200]}", "body": body}

    # Parse the JSON body counters to classify the outcome.
    data: dict[str, Any] = {}
    try:
        parsed = resp.json()
        if isinstance(parsed, dict):
            data = parsed
    except Exception:
        pass

    message = str(data.get("message") or "").strip()

    def _n(key: str) -> int:
        try:
            return int(data.get(key) or 0)
        except (TypeError, ValueError):
            return 0

    if not data:
        # Non-JSON 2xx response — treat as success but flag as unknown shape.
        return {"ok": True, "status": resp.status_code, "outcome": "unknown",
                "message": message, "error": "", "body": body}

    failed = _n("failed")
    duplicate = _n("duplicate")
    updated = _n("updated")
    success = _n("success")

    if failed > 0:
        outcome, ok = "failed", False
    elif duplicate > 0:
        outcome, ok = "duplicate", True
    elif updated > 0:
        outcome, ok = "updated", True
    elif success > 0:
        outcome, ok = "added", True
    else:
        # 2xx but all counters zero — nothing happened; treat as failed.
        outcome, ok = "failed", False

    return {
        "ok": ok,
        "status": resp.status_code,
        "outcome": outcome,
        "message": message,
        "error": "" if ok else (message or f"no account added ({body[:120]})"),
        "body": body,
    }


def import_accounts(
    accounts: list[dict[str, Any]],
    import_cfg: dict[str, Any],
    threads: int = DEFAULT_THREADS,
) -> dict[str, Any]:
    """Import every account's access_token into the admin API concurrently.

    Args:
        accounts: account records (need at least ``access_token``).
        import_cfg: the ``import_api`` config section (``url`` + ``admin_key``).
        threads: max concurrent POSTs (each worker thread reuses one session).

    Returns:
        Summary dict: {"total", "ok", "failed", "skipped", "results"}.
    """
    base_url = str(import_cfg.get("url", "")).strip()
    admin_key = str(import_cfg.get("admin_key", "")).strip()

    if not base_url:
        logger.error("import_api.url is empty — nothing to import")
        return {"total": 0, "added": 0, "updated": 0, "duplicate": 0,
                "ok": 0, "failed": 0, "skipped": 0, "results": []}
    if not admin_key:
        logger.warning("import_api.admin_key is empty — the API will likely reject requests")

    # Config holds only the server base URL; the endpoint path is fixed.
    url = base_url.rstrip("/") + ENDPOINT_PATH

    total = len(accounts)
    workers = max(1, min(int(threads or DEFAULT_THREADS), max(1, total)))
    logger.info(f"Importing {total} access token(s) → {url} ({workers} workers)")

    # One curl_cffi session per worker thread (Session is not thread-safe).
    _local = threading.local()
    _sessions: list["requests.Session"] = []
    _sessions_lock = threading.Lock()

    def _session() -> "requests.Session":
        s = getattr(_local, "session", None)
        if s is None:
            s = requests.Session(impersonate="chrome", verify=False)
            _local.session = s
            with _sessions_lock:
                _sessions.append(s)
        return s

    require_k12 = bool(import_cfg.get("require_k12", True))

    def _worker(index: int, account: dict[str, Any]) -> dict[str, Any]:
        label = _label(account, index)
        plan = str(account.get("plan_type") or "").strip().lower()
        if require_k12 and plan != "k12":
            return {
                "label": label,
                "ok": False,
                "skipped": True,
                "reason": f"plan={plan or 'free'} (require k12)",
            }
        access_token = str(
            account.get("team_access_token")
            or account.get("access_token")
            or ""
        ).strip()
        if not access_token:
            return {"label": label, "ok": False, "skipped": True, "reason": "no access_token"}
        res = import_access_token(
            _session(),
            url=url,
            admin_key=admin_key,
            access_token=access_token,
        )
        res["label"] = label
        return res

    ok_count = 0
    fail_count = 0
    skip_count = 0
    added = 0
    updated = 0
    duplicate = 0
    results: list[dict[str, Any]] = [{} for _ in range(total)]
    stage_start = time.monotonic()

    # icon per outcome for readable per-account logging
    marks = {"added": "✓ added", "updated": "~ updated", "duplicate": "• duplicate"}

    try:
        with ThreadPoolExecutor(max_workers=workers) as executor:
            futures = {
                executor.submit(_worker, i, account): i - 1
                for i, account in enumerate(accounts, 1)
            }
            for future in as_completed(futures):
                idx = futures[future]
                res = future.result()
                results[idx] = res
                tag = f"[#{idx + 1}/{total} {res['label']}]"
                # main-thread logging keeps output readable under concurrency
                if res.get("skipped"):
                    skip_count += 1
                    why = str(res.get("reason") or "no access_token")
                    logger.info(f"{tag} skip — {why}")
                elif res.get("ok"):
                    ok_count += 1
                    outcome = res.get("outcome", "unknown")
                    if outcome == "added":
                        added += 1
                    elif outcome == "updated":
                        updated += 1
                    elif outcome == "duplicate":
                        duplicate += 1
                    mark = marks.get(outcome, "✓ imported")
                    msg = f" — {res['message']}" if res.get("message") else ""
                    logger.info(f"{tag} {mark}{msg}")
                else:
                    fail_count += 1
                    logger.warning(f"{tag} ✗ {res.get('error', '?')}")
    finally:
        for s in _sessions:
            try:
                s.close()
            except Exception:
                pass

    wall = time.monotonic() - stage_start
    avg = wall / total if total else 0.0
    logger.info(
        f"Import complete: {ok_count} ok "
        f"(added={added}, updated={updated}, duplicate={duplicate}), "
        f"{fail_count} failed, {skip_count} skipped "
        f"| total {wall:.1f}s | avg {avg:.1f}s/account"
    )
    return {
        "total": total,
        "added": added,
        "updated": updated,
        "duplicate": duplicate,
        "ok": ok_count,
        "failed": fail_count,
        "skipped": skip_count,
        "results": results,
    }
