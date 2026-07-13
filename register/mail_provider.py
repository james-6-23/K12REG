"""Mail provider for ChatGPT registration — Outlook Token Pool only.

Extracted and simplified from chatgpt2api's mail_provider.py.
Only the OutlookTokenProvider is included since the user uses Outlook
token pools exclusively.

Format: email----password----client_id----refresh_token (one per line)

Supports loading the pool from:
- Inline "mailboxes" in config
- "mailboxes_file": "outlook.txt" (recommended for large pools)
- Auto fallback to ./outlook.txt (or next to config.toml)
"""

from __future__ import annotations

import atexit
import hashlib
import imaplib
import json
import re
import time
from datetime import datetime, timezone
from email import message_from_bytes, policy
from email.header import decode_header, make_header
from email.utils import parsedate_to_datetime
from pathlib import Path
from threading import Lock
from typing import Any, Callable

from curl_cffi import requests

# ── Data directory (stores pool state) ─────────────────────────────

DATA_DIR = Path("data")
STATE_FILE = DATA_DIR / "outlook_token_state.json"

# ── Outlook constants ───────────────────────────────────────────────

OUTLOOK_TOKEN_URL = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
OUTLOOK_GRAPH_MESSAGES_URL = "https://graph.microsoft.com/v1.0/me/messages"
OUTLOOK_GRAPH_SCOPE = "offline_access https://graph.microsoft.com/Mail.Read"
OUTLOOK_IMAP_SCOPE = "offline_access https://outlook.office.com/IMAP.AccessAsUser.All"
OUTLOOK_DEFAULT_IMAP_HOST = "outlook.office365.com"

# ── Pool state tracking ─────────────────────────────────────────────

_outlook_token_state_lock = Lock()
OUTLOOK_IN_USE_STALE_SECONDS = 3600  # 1 hour stale timeout
OUTLOOK_UNAVAILABLE_STATES = {"used", "token_invalid", "failed"}


def _read_state_file() -> dict[str, dict[str, Any]]:
    """Load pool state from disk (email_lower → {state, reason, updated_at})."""
    try:
        if not STATE_FILE.exists():
            return {}
        data = json.loads(STATE_FILE.read_text(encoding="utf-8"))
    except Exception:
        return {}
    state: dict[str, dict[str, Any]] = {}
    if isinstance(data, list):
        for item in data:
            key = str(item).strip().lower()
            if key:
                state[key] = {"state": "used", "reason": "", "updated_at": ""}
    elif isinstance(data, dict):
        for key, value in data.items():
            email = str(key).strip().lower()
            if not email:
                continue
            if isinstance(value, dict):
                state[email] = {
                    "state": str(value.get("state") or "used").strip() or "used",
                    "reason": str(value.get("reason") or ""),
                    "updated_at": str(value.get("updated_at") or ""),
                }
            else:
                state[email] = {
                    "state": str(value or "used").strip() or "used",
                    "reason": "",
                    "updated_at": "",
                }
    return state


def _write_state_file(state: dict[str, dict[str, Any]]) -> None:
    STATE_FILE.parent.mkdir(parents=True, exist_ok=True)
    ordered = {key: state[key] for key in sorted(state)}
    tmp = STATE_FILE.with_suffix(".tmp")
    tmp.write_text(
        json.dumps(ordered, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    tmp.replace(STATE_FILE)  # atomic — never leave a half-written state file


# ── In-memory pool state ────────────────────────────────────────────
#
# The pool-state file used to be re-read *and* re-written in full on every
# mailbox pick / mark, all under one global lock — an O(N) serialization point
# that dominated throughput at high concurrency.  We now hold the authoritative
# state in memory (single process) and flush to disk debounced + on exit.  All
# access must go through `_state_store()` while holding `_outlook_token_state_lock`.

_STATE: dict[str, dict[str, Any]] | None = None
_STATE_DIRTY = False
_STATE_LAST_FLUSH = 0.0
_STATE_FLUSH_INTERVAL = 2.0  # seconds — cap disk writes to ~1 every 2s


def _state_store() -> dict[str, dict[str, Any]]:
    """Return the in-memory state, loading it from disk once. Call under lock."""
    global _STATE
    if _STATE is None:
        _STATE = _read_state_file()
    return _STATE


def _flush_state(force: bool = False) -> None:
    """Persist in-memory state to disk, debounced. Call under lock."""
    global _STATE_DIRTY, _STATE_LAST_FLUSH
    if _STATE is None or not _STATE_DIRTY:
        return
    now = time.monotonic()
    if not force and (now - _STATE_LAST_FLUSH) < _STATE_FLUSH_INTERVAL:
        return
    _write_state_file(_STATE)
    _STATE_DIRTY = False
    _STATE_LAST_FLUSH = now


def _mark_state_dirty() -> None:
    """Flag state changed and flush if the debounce window elapsed. Call under lock."""
    global _STATE_DIRTY
    _STATE_DIRTY = True
    _flush_state()


def _flush_state_on_exit() -> None:
    with _outlook_token_state_lock:
        _flush_state(force=True)


atexit.register(_flush_state_on_exit)


# ── Shared access-token cache ───────────────────────────────────────
#
# All plus-aliases of one base account share a single refresh_token, but they
# run as separate registrations (separate mailbox dicts / threads).  Caching the
# exchanged access_token per base — keyed by (client_id, refresh_token, scope) —
# collapses N per-alias token exchanges into one, cutting Microsoft token calls
# ~alias_count× and easing Graph throttling.

_token_cache_lock = Lock()
_token_cache: dict[tuple[str, str, str], tuple[str, float]] = {}
_TOKEN_TTL_SECONDS = 600.0  # 10 min


def _entry_available(entry: dict[str, Any] | None) -> bool:
    """Check if this email is available for use."""
    if not isinstance(entry, dict):
        return True
    current = str(entry.get("state") or "")
    if current in OUTLOOK_UNAVAILABLE_STATES:
        return False
    if current == "in_use":
        updated_at = str(entry.get("updated_at") or "")
        try:
            ts = datetime.fromisoformat(updated_at)
            age = (
                datetime.now(timezone.utc)
                - (ts if ts.tzinfo else ts.replace(tzinfo=timezone.utc))
            ).total_seconds()
            return age >= OUTLOOK_IN_USE_STALE_SECONDS
        except Exception:
            return True
    return True


def _base_available(store: dict[str, dict[str, Any]], base_email: str) -> bool:
    """Check whether a base account is usable.

    Plus-aliases all share one inbox and one refresh_token, so if the base
    account's token is invalid every alias is dead.  We only block aliases at
    the base level for ``token_invalid`` — ``used``/``failed`` stay per-alias.
    """
    key = str(base_email or "").strip().lower()
    if not key:
        return True
    entry = store.get(key)
    if isinstance(entry, dict) and str(entry.get("state") or "") == "token_invalid":
        return False
    return True


def _set_state(address: str, state: str, reason: str = "") -> None:
    target = str(address or "").strip().lower()
    if not target:
        return
    with _outlook_token_state_lock:
        store = _state_store()
        store[target] = {
            "state": str(state),
            "reason": str(reason or ""),
            "updated_at": datetime.now(timezone.utc).isoformat(),
        }
        _mark_state_dirty()


def _release_state(address: str) -> None:
    """Release in_use state back to unused."""
    target = str(address or "").strip().lower()
    if not target:
        return
    with _outlook_token_state_lock:
        store = _state_store()
        entry = store.get(target)
        if isinstance(entry, dict) and str(entry.get("state") or "") == "in_use":
            store.pop(target, None)
            _mark_state_dirty()


# ── Used-mailbox export ─────────────────────────────────────────────

USED_MAILBOX_FILENAME = "outlook_used.txt"
_used_file_lock = Lock()


def _append_used_mailbox(mailbox: dict[str, Any]) -> None:
    """Append a successfully-used mailbox's full credential line to outlook_used.txt.

    Non-destructive: the source pool file (outlook.txt) is left untouched — dedup
    across runs is already handled by the state file.  This just gives a
    human-readable record of consumed emails, deduped by email.
    """
    path_str = str(mailbox.get("_used_file") or "").strip()
    email = str(mailbox.get("address") or "").strip()
    if not path_str or not email:
        return

    password = str(mailbox.get("password") or "")
    client_id = str(mailbox.get("client_id") or "")
    refresh_token = str(mailbox.get("refresh_token") or "")
    line = f"{email}----{password}----{client_id}----{refresh_token}"

    path = Path(path_str)
    with _used_file_lock:
        # Skip if this email is already recorded (idempotent across retries/reruns).
        try:
            if path.exists():
                for raw in path.read_text(encoding="utf-8").splitlines():
                    head = raw.split("----", 1)[0].strip().lower()
                    if head == email.lower():
                        return
        except Exception:
            pass
        try:
            path.parent.mkdir(parents=True, exist_ok=True)
            with open(path, "a", encoding="utf-8") as f:
                f.write(line + "\n")
        except Exception:
            pass


# ── Credential parsing ──────────────────────────────────────────────


def parse_outlook_credentials(text: str) -> list[dict[str, str]]:
    """Parse outlook token pool text.

    Format: email----password----令牌(refresh_token)----client_id (one per line)
    (or the reverse order; auto-corrected)

    Lines without '----' (e.g. comments) are ignored.

    The parser auto-detects and corrects common order mistakes.
    """
    credentials: list[dict[str, str]] = []
    seen: set[str] = set()
    for raw_line in str(text or "").splitlines():
        line = str(raw_line or "").strip()
        if not line or "----" not in line:
            continue
        parts = [str(p).strip() for p in line.split("----", 3)]
        if len(parts) != 4:
            continue
        email, password, client_id, refresh_token = parts
        if "@" not in email or not client_id or not refresh_token:
            continue

        # Auto-correct common extraction order mistakes.
        # client_id should be a GUID (e.g. 9e5f94bc-...), refresh_token is long (M.C... or similar)
        def looks_like_guid(s: str) -> bool:
            s = s.strip()
            return len(s) >= 32 and s.count("-") == 4 and all(c in "0123456789abcdefABCDEF-" for c in s)

        def looks_like_refresh_token(s: str) -> bool:
            s = s.strip()
            return len(s) > 100 or s.startswith(("M.C", "0.A", "Ew", "1.A"))

        if looks_like_guid(refresh_token) and looks_like_refresh_token(client_id):
            # User put long-token in 3rd position and GUID in 4th. Swap them.
            client_id, refresh_token = refresh_token, client_id

        key = email.lower()
        if key in seen:
            continue
        seen.add(key)
        credentials.append(
            {
                "email": email,
                "password": password,
                "client_id": client_id,
                "refresh_token": refresh_token,
            }
        )
    return credentials


# ── Plus-address (子地址) alias expansion ───────────────────────────


def make_plus_aliases(base_email: str, count: int, prefix: str = "") -> list[str]:
    """Generate ``count`` plus-address aliases for one base Outlook address.

    ``hrqqd60635061@outlook.com`` → ``hrqqd60635061+<tag>1@outlook.com`` …

    Outlook delivers every ``base+anything@outlook.com`` into the base inbox,
    so a single bought account can register ``count`` ChatGPT accounts while we
    still read all verification codes from the base mailbox.

    The tag is **deterministic** (a short hash of the base local-part) so the
    same aliases are produced on every run and the pool-state file stays valid
    across restarts — but it's non-obvious rather than a bare ``+1``/``+2``.
    """
    base = str(base_email or "").strip()
    if count <= 1 or "@" not in base:
        return [base]
    local, domain = base.split("@", 1)
    local = local.split("+", 1)[0]  # never stack a plus on an existing alias
    salt = hashlib.sha256(local.lower().encode("utf-8")).hexdigest()[:4]
    tag = f"{prefix}{salt}" if prefix else salt
    return [f"{local}+{tag}{i}@{domain}" for i in range(1, count + 1)]


def _expand_pool_with_aliases(
    pool: list[dict[str, str]], count: int, prefix: str = ""
) -> list[dict[str, Any]]:
    """Turn each base credential into ``count`` virtual alias credentials.

    Each virtual entry keeps the base ``client_id``/``refresh_token`` (used to
    read the shared inbox) but its ``email`` becomes the alias (used as the
    signup address).  ``base_email`` records the real inbox owner.
    """
    if count <= 1:
        return [dict(item, base_email=item["email"]) for item in pool]
    expanded: list[dict[str, Any]] = []
    for cred in pool:
        base = cred["email"]
        for alias in make_plus_aliases(base, count, prefix):
            expanded.append(dict(cred, base_email=base, email=alias))
    return expanded


def _load_mailboxes_text(entry: dict[str, Any], config_dir: str = "") -> str:
    """Resolve and read the raw mailbox pool text."""
    _sig, path, inline = _resolve_mailboxes_source(entry, config_dir)
    if path is not None:
        try:
            return path.read_text(encoding="utf-8", errors="replace")
        except Exception:
            return inline or ""
    return inline or ""


def _resolve_mailboxes_source(
    entry: dict[str, Any], config_dir: str = ""
) -> tuple[str, Path | None, str]:
    """Resolve the mailbox pool source to ``(signature, path, inline_text)``.

    Does **not** read the file — only ``stat``s it — so callers can cheaply
    compute the cache signature without paying the read on every call.

    Priority: mailboxes_file → inline mailboxes/pool → auto ./outlook.txt.
    Signature is ``file:<path>:<mtime_ns>:<size>`` (invalidates on change) or
    ``inline:<hash>``.  ``path`` is set only for a file source.
    """
    mailboxes_file = str(entry.get("mailboxes_file") or "").strip()
    mailboxes_inline = str(entry.get("mailboxes") or entry.get("pool") or "").strip()

    candidates: list[Path] = []
    if mailboxes_file:
        p = Path(mailboxes_file)
        if not p.is_absolute() and config_dir:
            candidates.append(Path(config_dir) / p)
        candidates.append(p)
    elif mailboxes_inline:
        digest = hashlib.sha256(mailboxes_inline.encode("utf-8", "replace")).hexdigest()[:16]
        return f"inline:{digest}", None, mailboxes_inline
    else:
        candidates.append(Path("outlook.txt"))
        if config_dir:
            candidates.append(Path(config_dir) / "outlook.txt")

    for p in candidates:
        try:
            if p.exists():
                st = p.stat()
                return f"file:{p.resolve()}:{st.st_mtime_ns}:{st.st_size}", p, ""
        except Exception:
            continue

    if mailboxes_inline:
        digest = hashlib.sha256(mailboxes_inline.encode("utf-8", "replace")).hexdigest()[:16]
        return f"inline:{digest}", None, mailboxes_inline
    return "", None, ""


# ── Parsed-pool cache ───────────────────────────────────────────────
#
# Every registration reconstructs an OutlookTokenProvider (twice: create_mailbox
# + wait_for_code), and each construction re-read the pool file and re-expanded
# it into tens of thousands of alias dicts.  Cache the finished pool keyed by the
# source signature + alias params so it's built once per file version; cache
# hits do a single stat and skip the file read entirely.

_pool_cache_lock = Lock()
_pool_cache: dict[str, list[dict[str, Any]]] = {}


def _resolve_pool(
    entry: dict[str, Any], config_dir: str, alias_count: int, alias_prefix: str
) -> list[dict[str, Any]]:
    """Parse + alias-expand the mailbox pool, cached by source signature."""
    sig, path, inline = _resolve_mailboxes_source(entry, config_dir)
    key = f"{sig}|{alias_count}|{alias_prefix}" if sig else ""
    if key:
        with _pool_cache_lock:
            cached = _pool_cache.get(key)
        if cached is not None:
            return cached

    if path is not None:
        try:
            text = path.read_text(encoding="utf-8", errors="replace")
        except Exception:
            text = inline
    else:
        text = inline
    base_pool = parse_outlook_credentials(text)
    pool = _expand_pool_with_aliases(base_pool, alias_count, alias_prefix)

    if key:
        with _pool_cache_lock:
            _pool_cache[key] = pool
    return pool


# ── Code extraction ─────────────────────────────────────────────────


def _extract_code(message: dict[str, Any]) -> str | None:
    """Extract 6-digit verification code from email content."""
    content = (
        f"{message.get('subject', '')}\n"
        f"{message.get('text_content', '')}\n"
        f"{message.get('html_content', '')}"
    ).strip()
    if not content:
        return None

    # OpenAI styled <p> with background-color: #F3F3F3
    match = re.search(
        r"background-color:\s*#F3F3F3[^>]*>[\s\S]*?(\d{6})[\s\S]*?</p>",
        content,
        re.I,
    )
    if match:
        return match.group(1)

    # Text patterns
    match = re.search(
        r"(?:Verification code|code is|代码为|验证码)[:\s]*(\d{6})",
        content,
        re.I,
    )
    if match and match.group(1) != "177010":
        return match.group(1)

    # Generic 6-digit codes (excluding known false positive 177010)
    for code in re.findall(r">\s*(\d{6})\s*<|(?<![#&])\b(\d{6})\b", content):
        value = code[0] or code[1]
        if value and value != "177010":
            return value

    return None


def _message_tracking_ref(message: dict[str, Any]) -> str:
    """Create a content-based tracking reference for deduplication."""
    provider = str(message.get("provider") or "").strip()
    mailbox = str(message.get("mailbox") or "").strip()
    message_id = str(message.get("message_id") or "").strip()
    if message_id:
        return f"id:{provider}:{mailbox}:{message_id}"

    received_at = message.get("received_at")
    received_value = (
        received_at.isoformat()
        if isinstance(received_at, datetime)
        else str(received_at or "")
    )
    content = "\n".join(
        str(message.get(key) or "")
        for key in ("subject", "sender", "text_content", "html_content")
    )
    digest = hashlib.sha256(content.encode("utf-8", errors="replace")).hexdigest()
    return f"content:{provider}:{mailbox}:{received_value}:{digest}"


def _message_before_code_boundary(
    mailbox: dict[str, Any], message: dict[str, Any]
) -> bool:
    """Check if message arrived before the code boundary timestamp."""
    boundary = mailbox.get("_code_not_before")
    received_at = message.get("received_at")
    if not isinstance(boundary, datetime) or not isinstance(received_at, datetime):
        return False
    if not received_at.tzinfo:
        received_at = received_at.replace(tzinfo=timezone.utc)
    return received_at < boundary


def _message_recipient_matches(
    mailbox: dict[str, Any], message: dict[str, Any]
) -> bool:
    """When the mailbox is a plus-alias, only accept mail addressed to it.

    Every alias of a base account lands in the same inbox, so without this
    filter a registration could pick up a sibling alias's verification code
    (especially when several aliases of one base register concurrently).  We
    match on the unique ``+tag`` local-part found in the message recipients.

    If the message carries no recipient info at all we accept it, to avoid
    deadlocking on providers/messages that don't expose the To header.
    """
    address = str(mailbox.get("address") or "").strip().lower()
    base = str(mailbox.get("base_email") or "").strip().lower()
    if not address or address == base:
        return True  # not an alias — no filtering needed
    local = address.split("@", 1)[0]
    tag = local.split("+", 1)[1] if "+" in local else ""
    blob = " ".join(str(r) for r in (message.get("recipients") or [])).lower()
    if not blob:
        return True
    if address in blob:
        return True
    return bool(tag) and f"+{tag}" in blob


def _parse_received_at(value: Any) -> datetime | None:
    if isinstance(value, (int, float)):
        try:
            return datetime.fromtimestamp(float(value), tz=timezone.utc)
        except Exception:
            return None
    text = str(value or "").strip()
    if not text:
        return None
    try:
        date = datetime.fromisoformat(
            text[:-1] + "+00:00" if text.endswith("Z") else text
        )
        return date if date.tzinfo else date.replace(tzinfo=timezone.utc)
    except Exception:
        pass
    try:
        date = parsedate_to_datetime(text)
        return date if date.tzinfo else date.replace(tzinfo=timezone.utc)
    except Exception:
        return None


# ── Provider classes ────────────────────────────────────────────────


class OutlookTokenError(RuntimeError):
    """refresh_token exchange failed (invalid/expired credentials)."""


class BaseMailProvider:
    """Abstract base for mail providers."""

    name = "unknown"

    def __init__(self, conf: dict, provider_ref: str = ""):
        self.conf = conf
        self.provider_ref = provider_ref

    def fetch_latest_message(self, mailbox: dict[str, Any]) -> dict[str, Any] | None:
        raise NotImplementedError

    def create_mailbox(self, username: str | None = None) -> dict[str, Any]:
        raise NotImplementedError

    def wait_for_code(
        self,
        mailbox: dict[str, Any],
        on_tick: Callable[[float, float], None] | None = None,
    ) -> str | None:
        seen_value = mailbox.setdefault("_seen_code_message_refs", [])
        if not isinstance(seen_value, list):
            seen_value = []
            mailbox["_seen_code_message_refs"] = seen_value
        seen_refs = {str(item) for item in seen_value}

        timeout = float(self.conf["wait_timeout"])
        t0 = time.monotonic()
        deadline = t0 + timeout
        last_tick = -1.0
        while time.monotonic() < deadline:
            elapsed = time.monotonic() - t0
            if on_tick is not None and elapsed - last_tick >= 2.0:
                try:
                    on_tick(elapsed, timeout)
                except Exception:
                    pass
                last_tick = elapsed
            message = self.fetch_latest_message(mailbox)
            if message:
                ref = _message_tracking_ref(message)
                if ref not in seen_refs:
                    code = _extract_code(message)
                    if code:
                        seen_value.append(ref)
                        return code
                    seen_refs.add(ref)
            time.sleep(max(0.2, self.conf["wait_interval"]))
        return None

    def close(self) -> None:
        pass


class OutlookTokenProvider(BaseMailProvider):
    """Use Outlook/Hotmail refresh_token to read verification codes.

    Supports loading pool from:
      - mailboxes_file: "outlook.txt"
      - inline mailboxes
      - auto fallback to outlook.txt

    Format per line: email----password----令牌(refresh_token)----client_id

    For special sub-accounts (子邮箱 from resellers):
      - Default permission is usually IMAP.
      - For Graph mode, set `graph_scope: "https://graph.microsoft.com/.default"`
        in the provider config (as per seller instructions).
    """

    name = "outlook_token"

    def __init__(self, entry: dict, conf: dict):
        super().__init__(conf, str(entry.get("provider_ref") or ""))
        self.label = str(entry.get("label") or self.provider_ref)

        config_dir = str(conf.get("_config_dir") or "")

        # Plus-address expansion: one bought account → `alias_count` signups,
        # all reading verification codes from the same base inbox.  The parsed +
        # expanded pool is cached module-wide (keyed by file version + params),
        # so reconstructing a provider per registration is cheap.
        self.alias_count = max(1, int(entry.get("alias_count") or 1))
        self.alias_prefix = str(entry.get("alias_prefix") or "").strip()
        self.pool = _resolve_pool(
            entry, config_dir, self.alias_count, self.alias_prefix
        )

        self.mode = str(entry.get("mode") or "graph").strip().lower() or "graph"
        if self.mode not in {"graph", "imap", "auto"}:
            self.mode = "graph"
        self.imap_host = (
            str(entry.get("imap_host") or OUTLOOK_DEFAULT_IMAP_HOST).strip()
            or OUTLOOK_DEFAULT_IMAP_HOST
        )
        self.message_limit = max(1, int(entry.get("message_limit") or 10))

        # Support custom scope for special sub-accounts (e.g. from resellers)
        # Seller hint for Graph: "https://graph.microsoft.com/.default"
        # Normal: "offline_access https://graph.microsoft.com/Mail.Read"
        self.graph_scope = str(
            entry.get("graph_scope") or OUTLOOK_GRAPH_SCOPE
        ).strip() or OUTLOOK_GRAPH_SCOPE

        self.session = self._make_session()

    def _make_session(self):
        proxy = str(self.conf.get("proxy") or "").strip()
        kwargs = {"impersonate": "chrome", "verify": False}
        if proxy:
            kwargs["proxy"] = proxy
        return requests.Session(**kwargs)

    def close(self) -> None:
        self.session.close()

    # ── Token exchange ──────────────────────────────────────────

    def _exchange_refresh_token(
        self, client_id: str, refresh_token: str, scope: str
    ) -> str:
        resp = self.session.post(
            OUTLOOK_TOKEN_URL,
            data={
                "client_id": client_id,
                "grant_type": "refresh_token",
                "refresh_token": refresh_token,
                "scope": scope,
            },
            headers={
                "Content-Type": "application/x-www-form-urlencoded",
                "User-Agent": self.conf["user_agent"],
            },
            timeout=self.conf["request_timeout"],
            verify=False,
        )
        try:
            data = resp.json()
        except Exception:
            data = {}
        if resp.status_code != 200:
            detail = (
                data.get("error_description")
                or data.get("error")
                or resp.text[:300]
            )
            raise OutlookTokenError(
                f"OutlookToken refresh failed: HTTP {resp.status_code}, {detail}"
            )
        access_token = str(data.get("access_token") or "").strip()
        if not access_token:
            raise OutlookTokenError(
                "OutlookToken refresh response missing access_token"
            )
        return access_token

    def _cached_access_token(
        self, mailbox: dict[str, Any], client_id: str, refresh_token: str, scope: str
    ) -> str:
        """Return an access_token, cached process-wide per (client_id, token, scope).

        Shared across every alias/thread of the same base account so a burst of
        registrations on one inbox performs a single refresh_token exchange
        instead of one per alias.  (``mailbox`` is kept for signature
        compatibility but no longer holds the cache.)
        """
        key = (client_id, refresh_token, scope)
        now = time.monotonic()
        with _token_cache_lock:
            cached = _token_cache.get(key)
            if cached is not None and now < cached[1]:
                return cached[0]
        token = self._exchange_refresh_token(client_id, refresh_token, scope)
        with _token_cache_lock:
            _token_cache[key] = (token, time.monotonic() + _TOKEN_TTL_SECONDS)
        return token

    # ── Mailbox creation ─────────────────────────────────────────

    def create_mailbox(self, username: str | None = None) -> dict[str, Any]:
        if not self.pool:
            raise RuntimeError(
                "OutlookToken pool is empty. "
                "Add accounts to outlook.txt and set mailboxes_file: \"outlook.txt\" "
                "(or use inline mailboxes in config)."
            )
        with _outlook_token_state_lock:
            store = _state_store()
            credential = next(
                (
                    item
                    for item in self.pool
                    if _entry_available(store.get(item["email"].strip().lower()))
                    and _base_available(store, item.get("base_email") or item["email"])
                ),
                None,
            )
            if credential is None:
                raise RuntimeError(
                    f"[{self.label}] OutlookToken pool exhausted "
                    f"({len(self.pool)} total). "
                    f"All emails used/failed. Import new emails or reset pool state."
                )
            store[credential["email"].strip().lower()] = {
                "state": "in_use",
                "reason": "",
                "updated_at": datetime.now(timezone.utc).isoformat(),
            }
            _mark_state_dirty()

        return {
            "provider": self.name,
            "provider_ref": self.provider_ref,
            "address": credential["email"],
            "base_email": credential.get("base_email") or credential["email"],
            "label": self.label,
            "password": credential["password"],
            "client_id": credential["client_id"],
            "refresh_token": credential["refresh_token"],
        }

    # ── Graph API mail reading ───────────────────────────────────

    def _read_graph(self, access_token: str) -> list[dict[str, Any]]:
        resp = self.session.get(
            OUTLOOK_GRAPH_MESSAGES_URL,
            headers={
                "Authorization": f"Bearer {access_token}",
                "Accept": "application/json",
                "User-Agent": self.conf["user_agent"],
            },
            params={
                "$top": self.message_limit,
                "$orderby": "receivedDateTime desc",
                "$select": "subject,receivedDateTime,from,toRecipients,ccRecipients,body,bodyPreview",
            },
            timeout=self.conf["request_timeout"],
            verify=False,
        )
        try:
            data = resp.json()
        except Exception:
            data = {}
        if resp.status_code != 200:
            detail = (
                data.get("error", {}).get("message")
                if isinstance(data.get("error"), dict)
                else resp.text[:300]
            )
            raise RuntimeError(
                f"OutlookToken Graph failed: HTTP {resp.status_code}, {detail}"
            )
        items = data.get("value") if isinstance(data, dict) else None
        return [item for item in items if isinstance(item, dict)] if isinstance(items, list) else []

    @staticmethod
    def _graph_sender(message: dict[str, Any]) -> str:
        sender = message.get("from") or {}
        if isinstance(sender, dict):
            address = sender.get("emailAddress") or {}
            if isinstance(address, dict):
                return str(address.get("address") or address.get("name") or "")
        return ""

    @staticmethod
    def _graph_recipients(message: dict[str, Any]) -> list[str]:
        out: list[str] = []
        for key in ("toRecipients", "ccRecipients"):
            values = message.get(key)
            if not isinstance(values, list):
                continue
            for recipient in values:
                if not isinstance(recipient, dict):
                    continue
                address = recipient.get("emailAddress") or {}
                if isinstance(address, dict):
                    value = str(address.get("address") or "").strip()
                    if value:
                        out.append(value)
        return out

    def _normalize_graph_item(
        self, mailbox: dict[str, Any], item: dict[str, Any]
    ) -> dict[str, Any]:
        body = item.get("body") if isinstance(item.get("body"), dict) else {}
        content_type = str(body.get("contentType") or "").lower()
        content = str(body.get("content") or "")
        text_content = (
            content if content_type != "html" else str(item.get("bodyPreview") or "")
        )
        html_content = content if content_type == "html" else ""
        return {
            "provider": self.name,
            "mailbox": mailbox["address"],
            "message_id": str(item.get("id") or ""),
            "subject": str(item.get("subject") or ""),
            "sender": self._graph_sender(item),
            "recipients": self._graph_recipients(item),
            "text_content": text_content,
            "html_content": html_content,
            "received_at": _parse_received_at(item.get("receivedDateTime")),
            "raw": item,
        }

    def _graph_messages(
        self, mailbox: dict[str, Any], access_token: str
    ) -> list[dict[str, Any]]:
        return [
            self._normalize_graph_item(mailbox, item)
            for item in self._read_graph(access_token)
        ]

    # ── IMAP mail reading ────────────────────────────────────────

    def _imap_messages(
        self, mailbox: dict[str, Any], access_token: str
    ) -> list[dict[str, Any]]:
        # Authenticate as the base inbox owner — plus-aliases have no login of
        # their own; they just deliver into this same mailbox.
        login_user = str(mailbox.get("base_email") or mailbox["address"])
        auth_string = (
            f"user={login_user}\x01auth=Bearer {access_token}\x01\x01"
        )
        imap = imaplib.IMAP4_SSL(self.imap_host)
        try:
            imap.authenticate("XOAUTH2", lambda _: auth_string.encode("utf-8"))
            status, _ = imap.select("INBOX", readonly=True)
            if status != "OK":
                raise RuntimeError("OutlookToken IMAP select INBOX failed")
            status, data = imap.uid("search", None, "ALL")
            if status != "OK" or not data or not data[0]:
                return []
            uids = data[0].split()[-self.message_limit :]
            messages: list[dict[str, Any]] = []
            for uid in reversed(uids):
                status, fetched = imap.uid("fetch", uid, "(RFC822)")
                if status != "OK":
                    continue
                raw_payload = next(
                    (
                        part[1]
                        for part in fetched
                        if isinstance(part, tuple) and isinstance(part[1], bytes)
                    ),
                    b"",
                )
                if raw_payload:
                    messages.append(
                        self._parse_imap_message(mailbox, raw_payload)
                    )
            return messages
        finally:
            try:
                imap.logout()
            except Exception:
                pass

    def _parse_imap_message(self, mailbox: dict[str, Any], raw: bytes) -> dict[str, Any]:
        message = message_from_bytes(raw, policy=policy.default)
        try:
            received = _parse_received_at(
                parsedate_to_datetime(str(message.get("Date") or ""))
            )
        except Exception:
            received = None
        plain: list[str] = []
        html: list[str] = []
        for part in message.walk() if message.is_multipart() else [message]:
            if part.get_content_maintype() == "multipart":
                continue
            try:
                payload = part.get_content()
            except Exception:
                continue
            if not payload:
                continue
            if part.get_content_type() == "text/html":
                html.append(str(payload))
            else:
                plain.append(str(payload))

        def _decode(value: str | None) -> str:
            if not value:
                return ""
            try:
                return str(make_header(decode_header(value)))
            except Exception:
                return value

        recipients = [
            _decode(str(message.get(header)))
            for header in ("To", "Cc", "Delivered-To", "X-Delivered-To")
            if message.get(header)
        ]

        return {
            "provider": self.name,
            "mailbox": mailbox["address"],
            "message_id": _decode(str(message.get("Message-ID") or "")),
            "subject": _decode(str(message.get("Subject") or "")),
            "sender": _decode(str(message.get("From") or "")),
            "recipients": recipients,
            "text_content": "\n".join(plain).strip(),
            "html_content": "\n".join(html).strip(),
            "received_at": received,
            "raw": None,
        }

    # ── Message fetching ─────────────────────────────────────────

    def fetch_recent_messages(self, mailbox: dict[str, Any]) -> list[dict[str, Any]]:
        client_id = str(mailbox.get("client_id") or "").strip()
        refresh_token = str(mailbox.get("refresh_token") or "").strip()
        if not client_id or not refresh_token:
            raise RuntimeError(
                "OutlookToken mailbox missing client_id or refresh_token"
            )
        errors: list[str] = []
        if self.mode in {"graph", "auto"}:
            try:
                access_token = self._cached_access_token(
                    mailbox, client_id, refresh_token, self.graph_scope
                )
                return self._graph_messages(mailbox, access_token)
            except Exception as error:
                if self.mode == "graph":
                    raise
                errors.append(f"graph: {error}")
        if self.mode in {"imap", "auto"}:
            try:
                access_token = self._cached_access_token(
                    mailbox, client_id, refresh_token, OUTLOOK_IMAP_SCOPE
                )
                return self._imap_messages(mailbox, access_token)
            except Exception as error:
                if self.mode == "imap":
                    raise
                errors.append(f"imap: {error}")
        if errors:
            raise RuntimeError("; ".join(errors))
        return []

    def fetch_latest_message(self, mailbox: dict[str, Any]) -> dict[str, Any] | None:
        messages = self.fetch_recent_messages(mailbox)
        return messages[0] if messages else None

    def wait_for_code(
        self,
        mailbox: dict[str, Any],
        on_tick: Callable[[float, float], None] | None = None,
    ) -> str | None:
        """Scan recent N messages for verification code, not just the latest."""
        seen_value = mailbox.setdefault("_seen_code_message_refs", [])
        if not isinstance(seen_value, list):
            seen_value = []
            mailbox["_seen_code_message_refs"] = seen_value
        seen_refs = {str(item) for item in seen_value}

        timeout = float(self.conf["wait_timeout"])
        t0 = time.monotonic()
        deadline = t0 + timeout
        last_tick = -1.0
        while time.monotonic() < deadline:
            elapsed = time.monotonic() - t0
            if on_tick is not None and elapsed - last_tick >= 2.0:
                try:
                    on_tick(elapsed, timeout)
                except Exception:
                    pass
                last_tick = elapsed
            for message in self.fetch_recent_messages(mailbox):
                # Skip messages from before the code boundary
                if _message_before_code_boundary(mailbox, message):
                    continue
                # Skip mail addressed to a sibling plus-alias in the same inbox
                if not _message_recipient_matches(mailbox, message):
                    continue
                ref = _message_tracking_ref(message)
                if ref in seen_refs:
                    continue
                code = _extract_code(message)
                if code:
                    seen_value.append(ref)
                    return code
                seen_refs.add(ref)
            time.sleep(max(0.2, self.conf["wait_interval"]))
        return None


# ── Public API ─────────────────────────────────────────────────────


def _make_config(mail_config: dict) -> dict:
    """Normalize mail config for provider construction."""
    return {
        "request_timeout": float(mail_config.get("request_timeout") or 30),
        "wait_timeout": float(mail_config.get("wait_timeout") or 30),
        "wait_interval": float(mail_config.get("wait_interval") or 2),
        "user_agent": str(
            mail_config.get("user_agent")
            or "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"
        ),
        "proxy": str(mail_config.get("proxy") or "").strip(),
        "_config_dir": str(mail_config.get("_config_dir") or ""),
    }


def create_mailbox(mail_config: dict, username: str | None = None) -> dict:
    """Create a mailbox from the Outlook token pool."""
    providers = (
        mail_config.get("providers")
        if isinstance(mail_config.get("providers"), list)
        else []
    )
    outlook_entries = [
        dict(item, provider_ref=f"outlook_token#{i+1}")
        for i, item in enumerate(providers)
        if isinstance(item, dict) and item.get("type") == "outlook_token"
    ]
    if not outlook_entries:
        raise RuntimeError(
            "No outlook_token provider found in mail.providers config"
        )

    conf = _make_config(mail_config)
    used_file = str(Path(conf.get("_config_dir") or ".") / USED_MAILBOX_FILENAME)
    last_error = ""
    for entry in outlook_entries:
        if not entry.get("enable", True):
            continue
        provider = OutlookTokenProvider(entry, conf)
        try:
            mailbox = provider.create_mailbox(username)
            mailbox["_code_not_before"] = datetime.now(timezone.utc)
            mailbox["_used_file"] = used_file
            return mailbox
        except RuntimeError as error:
            last_error = str(error)
        finally:
            provider.close()
    raise RuntimeError(last_error or "All Outlook providers exhausted")


def wait_for_code(
    mail_config: dict,
    mailbox: dict,
    on_tick: Callable[[float, float], None] | None = None,
) -> str | None:
    """Wait for verification code from Outlook mailbox.

    ``on_tick(elapsed_s, timeout_s)`` is called about every 2s while polling.
    """
    providers = (
        mail_config.get("providers")
        if isinstance(mail_config.get("providers"), list)
        else []
    )
    provider_ref = str(mailbox.get("provider_ref") or "")
    # Try matching by provider_ref first, then fall back to any outlook_token
    entry = next(
        (item for item in providers if item.get("provider_ref") == provider_ref),
        None,
    )
    if entry is None:
        entry = next(
            (item for item in providers
             if item.get("type") == "outlook_token" and item.get("enable", True)),
            None,
        )
    if entry is None:
        raise RuntimeError(
            f"No outlook_token provider found (ref={provider_ref})"
        )
    conf = _make_config(mail_config)
    provider = OutlookTokenProvider(entry, conf)
    try:
        return provider.wait_for_code(mailbox, on_tick=on_tick)
    finally:
        provider.close()


def mark_mailbox_result(
    mailbox: dict,
    *,
    success: bool,
    error: Exception | str | None = None,
) -> None:
    """Update pool state after registration attempt.

    - Success → mark as 'used'
    - Token invalid → mark as 'token_invalid'
    - Other failure → mark as 'failed'
    """
    if str(mailbox.get("provider") or "") != OutlookTokenProvider.name:
        return
    address = str(mailbox.get("address") or "").strip()
    if not address:
        return
    if success:
        _set_state(address, "used")
        _append_used_mailbox(mailbox)
        return
    reason = str(error or "").strip()
    if (
        isinstance(error, OutlookTokenError)
        or "OutlookToken" in reason
        or "access_token" in reason
    ):
        _set_state(address, "token_invalid", reason[:300])
        # A bad refresh_token kills every alias of this base — block them all.
        base = str(mailbox.get("base_email") or "").strip()
        if base and base.lower() != address.lower():
            _set_state(base, "token_invalid", reason[:300])
    else:
        _set_state(address, "failed", reason[:300])


def release_mailbox(mailbox: dict) -> None:
    """Release in_use state back to unused (if registration is abandoned)."""
    if str(mailbox.get("provider") or "") != OutlookTokenProvider.name:
        return
    _release_state(str(mailbox.get("address") or ""))
