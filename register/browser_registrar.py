"""Browser-based ChatGPT full registration (Playwright).

Protocol OAuth signup (sentinel / PKCE / create_account code) is increasingly
blocked.  This path drives a real browser through the ChatGPT signup UI,
captures the NextAuth session cookie, then exchanges it for an access_token
via ``GET /api/auth/session``.

Returns the same account shape as :class:`register.registrar.PlatformRegistrar`
plus ``session_token`` so the rest of the pipeline (join / approve / import)
keeps working.  ``refresh_token`` is usually empty — use session refresh.
"""

from __future__ import annotations

import asyncio
import logging
import random
import re
import secrets
import string
import threading
import time
import uuid
from collections import deque
from datetime import datetime, timezone
from typing import Any
from urllib.parse import unquote, urlparse

from register.mail_provider import (
    create_mailbox,
    mark_mailbox_result,
    release_mailbox,
    wait_for_code,
)
from utils.session_auth import (
    extract_session_token_from_cookies,
    fetch_session_from_token,
    session_payload_to_account_fields,
)
from utils import logger as plog

logger = logging.getLogger(__name__)

# Hand-confirmed flow:
#   1) https://chatgpt.com/auth/login_with?callback_path=
#   2) https://auth.openai.com/create-account
CHATGPT_HOME = "https://chatgpt.com/"
CHATGPT_LOGIN_WARMUP = "https://chatgpt.com/auth/login_with?callback_path="
SIGNUP_URL_DEFAULT = "https://auth.openai.com/create-account"
AUTH_HOST_HINTS = ("auth.openai.com", "auth0.openai.com", "chatgpt.com")


class BrowserRegistrationError(RuntimeError):
    """Browser signup step failed."""


# ── Small helpers ────────────────────────────────────────────────────


def _parse_typing_ms(value: Any) -> tuple[int, int]:
    """Parse typing delay range: (45, 130) or \"45-130\" or 80."""
    if isinstance(value, (list, tuple)) and len(value) >= 2:
        return int(value[0]), int(value[1])
    if isinstance(value, (int, float)):
        v = max(10, int(value))
        return max(10, v - 20), v + 20
    s = str(value or "").strip()
    if not s:
        return 45, 130
    if "-" in s:
        a, _, b = s.partition("-")
        try:
            return int(a.strip()), int(b.strip())
        except Exception:
            return 45, 130
    try:
        v = int(s)
        return max(10, v - 20), v + 20
    except Exception:
        return 45, 130


def _random_password(length: int = 16) -> str:
    chars = string.ascii_letters + string.digits + "!@#$%"
    value = list(
        secrets.choice(string.ascii_uppercase)
        + secrets.choice(string.ascii_lowercase)
        + secrets.choice(string.digits)
        + secrets.choice("!@#$%")
        + "".join(secrets.choice(chars) for _ in range(max(0, length - 4)))
    )
    random.shuffle(value)
    return "".join(value)


# Large US-style name pools so concurrent signups rarely collide.
_FIRST_NAMES = [
    "James", "Robert", "John", "Michael", "David", "William", "Richard", "Joseph",
    "Thomas", "Christopher", "Daniel", "Matthew", "Anthony", "Mark", "Steven",
    "Andrew", "Paul", "Joshua", "Kenneth", "Kevin", "Brian", "George", "Timothy",
    "Ronald", "Edward", "Jason", "Jeffrey", "Ryan", "Jacob", "Gary", "Nicholas",
    "Eric", "Jonathan", "Stephen", "Larry", "Justin", "Scott", "Brandon", "Benjamin",
    "Samuel", "Raymond", "Gregory", "Frank", "Alexander", "Patrick", "Jack", "Dennis",
    "Jerry", "Tyler", "Aaron", "Jose", "Adam", "Nathan", "Henry", "Douglas", "Zachary",
    "Peter", "Kyle", "Noah", "Ethan", "Jeremy", "Walter", "Christian", "Keith",
    "Mary", "Patricia", "Jennifer", "Linda", "Elizabeth", "Barbara", "Susan", "Jessica",
    "Sarah", "Karen", "Lisa", "Nancy", "Betty", "Margaret", "Sandra", "Ashley",
    "Dorothy", "Kimberly", "Emily", "Donna", "Michelle", "Carol", "Amanda", "Melissa",
    "Deborah", "Stephanie", "Rebecca", "Sharon", "Laura", "Cynthia", "Kathleen",
    "Amy", "Angela", "Shirley", "Anna", "Brenda", "Pamela", "Emma", "Nicole", "Helen",
    "Samantha", "Katherine", "Christine", "Debra", "Rachel", "Carolyn", "Janet",
    "Catherine", "Maria", "Heather", "Diane", "Olivia", "Julie", "Joyce", "Victoria",
    "Ruth", "Virginia", "Lauren", "Kelly", "Christina", "Joan", "Evelyn", "Judith",
    "Andrea", "Hannah", "Megan", "Cheryl", "Jacqueline", "Martha", "Madison", "Teresa",
    "Gloria", "Sara", "Janice", "Ann", "Kathryn", "Abigail", "Sophia", "Frances",
    "Jean", "Alice", "Judy", "Isabella", "Julia", "Grace", "Amber", "Denise", "Danielle",
    "Marilyn", "Beverly", "Charlotte", "Natalie", "Theresa", "Diana", "Brittany",
    "Diana", "Kayla", "Alexis", "Lori", "Marie",
]
_LAST_NAMES = [
    "Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis",
    "Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez", "Wilson", "Anderson",
    "Thomas", "Taylor", "Moore", "Jackson", "Martin", "Lee", "Perez", "Thompson",
    "White", "Harris", "Sanchez", "Clark", "Ramirez", "Lewis", "Robinson", "Walker",
    "Young", "Allen", "King", "Wright", "Scott", "Torres", "Nguyen", "Hill", "Flores",
    "Green", "Adams", "Nelson", "Baker", "Hall", "Rivera", "Campbell", "Mitchell",
    "Carter", "Roberts", "Gomez", "Phillips", "Evans", "Turner", "Diaz", "Parker",
    "Cruz", "Edwards", "Collins", "Reyes", "Stewart", "Morris", "Morales", "Murphy",
    "Cook", "Rogers", "Gutierrez", "Ortiz", "Morgan", "Cooper", "Peterson", "Bailey",
    "Reed", "Kelly", "Howard", "Ramos", "Kim", "Cox", "Ward", "Richardson", "Watson",
    "Brooks", "Chavez", "Wood", "James", "Bennett", "Gray", "Mendoza", "Ruiz", "Hughes",
    "Price", "Alvarez", "Castillo", "Sanders", "Patel", "Myers", "Long", "Ross",
    "Foster", "Jimenez", "Powell", "Jenkins", "Perry", "Russell", "Sullivan", "Bell",
    "Coleman", "Butler", "Henderson", "Barnes", "Gonzales", "Fisher", "Vasquez",
    "Simmons", "Romero", "Jordan", "Patterson", "Alexander", "Hamilton", "Graham",
    "Reynolds", "Griffin", "Wallace", "Moreno", "West", "Cole", "Hayes", "Bryant",
    "Herrera", "Gibson", "Ellis", "Tran", "Medina", "Aguilar", "Stevens", "Murray",
    "Ford", "Castro", "Marshall", "Owens", "Harrison", "Fernandez", "McDonald",
    "Woods", "Washington", "Kennedy", "Wells", "Vargas", "Henry", "Chen", "Freeman",
    "Webb", "Tucker", "Guzman", "Burns", "Crawford", "Olson", "Simpson", "Porter",
    "Hunter", "Gordon", "Mendez", "Silva", "Shaw", "Snyder", "Mason", "Dixon",
    "Munoz", "Hunt", "Hicks", "Holmes", "Palmer", "Wagner", "Black", "Robertson",
    "Boyd", "Rose", "Stone", "Salazar", "Fox", "Warren", "Mills", "Meyer", "Rice",
    "Schmidt", "Garza", "Daniels", "Ferguson", "Nichols", "Stephens", "Soto", "Weaver",
    "Ryan", "Gardner", "Payne", "Grant", "Dunn", "Kelley", "Spencer", "Hawkins",
]

# Recently used full names (process-local) so one batch doesn't share about-you.
_RECENT_NAMES: set[str] = set()
_RECENT_NAMES_ORDER: deque[str] = deque()
_RECENT_NAMES_LOCK = threading.Lock()
_RECENT_NAMES_MAX = 256


def _random_name() -> tuple[str, str]:
    """Pick a first/last name; avoid reusing the same full name in this process."""
    with _RECENT_NAMES_LOCK:
        for _ in range(120):
            first = secrets.choice(_FIRST_NAMES)
            last = secrets.choice(_LAST_NAMES)
            full = f"{first} {last}"
            if full in _RECENT_NAMES:
                continue
            _RECENT_NAMES.add(full)
            _RECENT_NAMES_ORDER.append(full)
            while len(_RECENT_NAMES_ORDER) > _RECENT_NAMES_MAX:
                old = _RECENT_NAMES_ORDER.popleft()
                _RECENT_NAMES.discard(old)
            return first, last
        # Exhausted retries — still return a random pair (extremely unlikely).
        return secrets.choice(_FIRST_NAMES), secrets.choice(_LAST_NAMES)


def _random_age() -> str:
    """Age for about-you form (hand-test used 20–26)."""
    return str(random.randint(20, 35))


def _age_to_birthday(age: str | int) -> dict[str, str]:
    """Derive a plausible **adult** birthday from an age string/int.

    Always at least 18 years before today (never future / current year).
    OpenAI English about-you uses **MM/DD/YYYY** in the Birthday field.

    Returns::

        {
          "age": "24",
          "iso": "2001-06-15",          # YYYY-MM-DD (input[type=date])
          "us": "06/15/2001",           # MM/DD/YYYY  ← preferred for text fields
          "eu": "15/06/2001",           # DD/MM/YYYY
          "year": "2001", "month": "6", "day": "15",
          "month_2": "06", "day_2": "15",
        }
    """
    try:
        years = int(str(age).strip())
    except Exception:
        years = random.randint(20, 35)
    # OpenAI rejects under-18 and nonsense future dates.
    years = max(20, min(years, 55))
    today = datetime.now(timezone.utc).date()
    # Birthday = today minus `years`, then shift back 30–300 days so we are
    # safely past the 18th birthday and not "today".
    month = random.randint(1, 12)
    day = random.randint(1, 28)
    year = today.year - years
    # Guard: if month/day is still in the future relative to "age", step year back.
    try:
        from datetime import date as _date

        candidate = _date(year, month, day)
        min_adult = _date(today.year - 18, today.month, min(today.day, 28))
        if candidate > min_adult:
            year -= 1
            candidate = _date(year, month, day)
        if candidate.year >= today.year:
            year = today.year - years
            candidate = _date(year, month, day)
        year, month, day = candidate.year, candidate.month, candidate.day
    except Exception:
        year = today.year - years
        month, day = 6, 15

    return {
        "age": str(years),
        "iso": f"{year:04d}-{month:02d}-{day:02d}",
        "us": f"{month:02d}/{day:02d}/{year:04d}",
        "eu": f"{day:02d}/{month:02d}/{year:04d}",
        "year": str(year),
        "month": str(month),
        "day": str(day),
        "month_2": f"{month:02d}",
        "day_2": f"{day:02d}",
    }


def _birthday_looks_valid(value: str) -> bool:
    """True if parsed date is in the past and age roughly 18–80."""
    s = str(value or "").strip()
    if not s:
        return False
    today = datetime.now(timezone.utc).date()
    parsed = None
    from datetime import date as _date

    for fmt in ("%Y-%m-%d", "%m/%d/%Y", "%d/%m/%Y", "%m-%d-%Y", "%d-%m-%Y"):
        try:
            parsed = datetime.strptime(s[:10], fmt).date()
            break
        except Exception:
            continue
    if parsed is None:
        # Digits only YYYYMMDD or MMDDYYYY
        digits = re.sub(r"\D", "", s)
        try:
            if len(digits) == 8:
                y_first = int(digits[:4])
                if 1940 <= y_first <= today.year - 18:
                    parsed = _date(y_first, int(digits[4:6]), int(digits[6:8]))
                else:
                    parsed = _date(
                        int(digits[4:8]), int(digits[:2]), int(digits[2:4])
                    )
        except Exception:
            parsed = None
    if parsed is None:
        return False
    if parsed >= today:
        return False
    # Reject near-today / infant years (common bug: 07/11/2026)
    if parsed.year >= today.year - 12:
        return False
    age_years = (today - parsed).days / 365.25
    return 18.0 <= age_years <= 80.0


# Serialize Camoufox/Playwright process startup across worker threads.
# Concurrent launches race the driver and leave sticky asyncio loops on the
# reused ThreadPoolExecutor threads → "Sync API inside the asyncio loop".
_CAMOUFOX_LAUNCH_LOCK = threading.Lock()


def _reset_thread_event_loop() -> None:
    """Install a fresh asyncio event loop on **this** thread.

    Playwright Sync API errors with::

        It looks like you are using Playwright Sync API inside the asyncio loop

    when the *current thread* still has a running/sticky loop from a previous
    browser session (especially after a hung ``browser.close`` / ``pw.stop``
    abandoned on a daemon thread). Always call this before ``sync_playwright().start()``.
    """
    log = logging.getLogger(__name__)
    try:
        # Prefer closing any non-running loop left on this thread.
        try:
            old = asyncio.get_event_loop_policy().get_event_loop()
        except Exception:
            old = None
        if old is not None:
            try:
                if old.is_running():
                    # Cannot close from outside; detach so get_running_loop
                    # no longer sees it after we install a new loop.
                    log.debug("detaching running asyncio loop on worker thread")
                elif not old.is_closed():
                    old.close()
            except Exception:
                pass
        new_loop = asyncio.new_event_loop()
        asyncio.set_event_loop(new_loop)
        # Sanity: get_running_loop must fail (we are sync code).
        try:
            asyncio.get_running_loop()
            # Still "running" somehow — last resort: brand-new policy-less set
            asyncio.set_event_loop(asyncio.new_event_loop())
        except RuntimeError:
            pass  # expected: no running loop
    except Exception as e:
        log.debug("reset_thread_event_loop: %s", e)


def _playwright_proxy(proxy: str) -> dict[str, str] | None:
    """Convert a proxy URL into Playwright's ``proxy`` dict.

    Playwright (Chromium **and** Firefox/Camoufox) only supports
    username/password on **HTTP/HTTPS** proxies — not SOCKS5.
    Residential pools (Rola, IPRoyal, …) almost always hand out
    ``socks5://user:pass@host:port``; we rewrite to ``http://`` on the same
    host/port/credentials (HTTP CONNECT). Set lines to ``http://…`` yourself
    to skip the rewrite warning.
    """
    proxy = (proxy or "").strip()
    if not proxy:
        return None
    # Playwright wants scheme://host:port (+ optional username/password).
    # socks5h is not always accepted — normalize to socks5 first.
    if proxy.lower().startswith("socks5h://"):
        proxy = "socks5://" + proxy[len("socks5h://") :]
    if proxy.lower().startswith("socks://"):
        proxy = "socks5://" + proxy[len("socks://") :]

    parsed = urlparse(proxy)
    if not parsed.scheme or not parsed.hostname:
        # bare host:port → assume http
        if "://" not in proxy:
            proxy = "http://" + proxy
            parsed = urlparse(proxy)
        else:
            logger.warning(f"Unparseable proxy URL, passing as server: {proxy}")
            return {"server": proxy}

    scheme = (parsed.scheme or "http").lower()
    username = unquote(parsed.username) if parsed.username else ""
    password = unquote(parsed.password) if parsed.password else ""

    # SOCKS5 + auth is unsupported by Playwright for every browser engine.
    if scheme in ("socks5", "socks5h", "socks4", "socks4a") and (username or password):
        logger.info(
            f"Playwright cannot use {scheme} + auth; "
            f"rewriting → http://{parsed.hostname}:{parsed.port or ''} "
            f"(same user). Provider must accept HTTP CONNECT."
        )
        scheme = "http"

    server = f"{scheme}://{parsed.hostname}"
    if parsed.port:
        server += f":{parsed.port}"
    out: dict[str, str] = {"server": server}
    if username:
        out["username"] = username
    if password:
        out["password"] = password
    return out


# Thread-local: human trajectory simulation for this worker's page actions.
_human_tls = threading.local()


def _human_cfg() -> dict[str, Any]:
    cfg = getattr(_human_tls, "cfg", None)
    if isinstance(cfg, dict):
        return cfg
    return {"enabled": True, "typing_ms": (45, 130), "move_steps": (14, 32)}


def _set_human_cfg(
    *,
    enabled: bool = True,
    typing_ms: tuple[int, int] = (45, 130),
    move_steps: tuple[int, int] = (14, 32),
) -> None:
    _human_tls.cfg = {
        "enabled": bool(enabled),
        "typing_ms": typing_ms,
        "move_steps": move_steps,
    }
    # Cursor position memory for continuous paths.
    if not hasattr(_human_tls, "cursor") or _human_tls.cursor is None:
        _human_tls.cursor = (
            float(random.randint(80, 400)),
            float(random.randint(80, 300)),
        )


def _human_pause(a: float = 0.08, b: float = 0.35) -> None:
    time.sleep(random.uniform(a, b))


def _bezier_point(
    p0: tuple[float, float],
    p1: tuple[float, float],
    p2: tuple[float, float],
    p3: tuple[float, float],
    t: float,
) -> tuple[float, float]:
    u = 1.0 - t
    x = (
        u**3 * p0[0]
        + 3 * u**2 * t * p1[0]
        + 3 * u * t**2 * p2[0]
        + t**3 * p3[0]
    )
    y = (
        u**3 * p0[1]
        + 3 * u**2 * t * p1[1]
        + 3 * u * t**2 * p2[1]
        + t**3 * p3[1]
    )
    return x, y


def _human_move_to(page, x: float, y: float) -> None:
    """Move mouse along a cubic Bézier curve (human-like path)."""
    cfg = _human_cfg()
    if not cfg.get("enabled"):
        try:
            page.mouse.move(x, y)
            _human_tls.cursor = (x, y)
        except Exception:
            pass
        return

    sx, sy = getattr(_human_tls, "cursor", None) or (
        float(random.randint(40, 200)),
        float(random.randint(40, 200)),
    )
    # Control points: slight arc / overshoot like a real hand.
    dist = max(1.0, ((x - sx) ** 2 + (y - sy) ** 2) ** 0.5)
    mid_x = (sx + x) / 2 + random.uniform(-0.25, 0.25) * dist
    mid_y = (sy + y) / 2 + random.uniform(-0.35, 0.15) * dist
    # Overshoot past target then settle (sometimes).
    if random.random() < 0.35 and dist > 80:
        ox = x + random.uniform(-12, 12)
        oy = y + random.uniform(-10, 10)
        p1 = (sx + (mid_x - sx) * 0.5, sy + (mid_y - sy) * 0.4)
        p2 = (mid_x, mid_y)
        p3 = (ox, oy)
        end = (x + random.uniform(-2, 2), y + random.uniform(-2, 2))
    else:
        p1 = (sx + (x - sx) * 0.25 + random.uniform(-40, 40), sy + random.uniform(-50, 30))
        p2 = (sx + (x - sx) * 0.7 + random.uniform(-30, 30), sy + (y - sy) * 0.6 + random.uniform(-40, 40))
        p3 = (x, y)
        end = (x, y)

    lo, hi = cfg.get("move_steps") or (14, 32)
    steps = random.randint(int(lo), int(hi))
    steps = max(8, min(steps, 48))
    try:
        for i in range(steps):
            t = i / (steps - 1)
            # Ease-in-out
            te = t * t * (3 - 2 * t)
            if random.random() < 0.35 and dist > 80:
                cx, cy = _bezier_point((sx, sy), p1, p2, p3, te)
            else:
                cx, cy = _bezier_point((sx, sy), p1, p2, end, te)
            # Micro jitter
            cx += random.uniform(-0.6, 0.6)
            cy += random.uniform(-0.6, 0.6)
            page.mouse.move(cx, cy)
            time.sleep(random.uniform(0.002, 0.014))
        page.mouse.move(x, y)
        _human_tls.cursor = (x, y)
    except Exception:
        try:
            page.mouse.move(x, y)
            _human_tls.cursor = (x, y)
        except Exception:
            pass


def _locator_click_point(loc) -> tuple[float, float] | None:
    """Random point inside locator box (not dead-center)."""
    try:
        box = loc.bounding_box(timeout=2000)
    except Exception:
        box = None
    if not box:
        return None
    # Prefer center-ish but not exact midpoint (bot tell).
    px = box["x"] + box["width"] * random.uniform(0.28, 0.72)
    py = box["y"] + box["height"] * random.uniform(0.30, 0.70)
    return px, py


def _human_click_locator(page, loc, timeout_ms: int = 4000) -> bool:
    """Hover along a curve then click — falls back to loc.click()."""
    cfg = _human_cfg()
    try:
        loc.scroll_into_view_if_needed(timeout=min(timeout_ms, 3000))
    except Exception:
        pass
    _human_pause(0.05, 0.22)

    if cfg.get("enabled"):
        pt = _locator_click_point(loc)
        if pt is not None:
            try:
                _human_move_to(page, pt[0], pt[1])
                _human_pause(0.04, 0.18)
                # Occasional double micro-move (hesitation).
                if random.random() < 0.2:
                    _human_move_to(
                        page,
                        pt[0] + random.uniform(-3, 3),
                        pt[1] + random.uniform(-2, 2),
                    )
                    _human_pause(0.03, 0.12)
                page.mouse.down()
                time.sleep(random.uniform(0.03, 0.11))
                page.mouse.up()
                _human_pause(0.06, 0.25)
                return True
            except Exception:
                pass
    try:
        loc.click(timeout=timeout_ms)
        _human_pause(0.05, 0.2)
        return True
    except Exception:
        try:
            loc.click(timeout=timeout_ms, force=True)
            return True
        except Exception:
            return False


def _human_type_locator(page, loc, value: str, timeout_ms: int = 4000) -> bool:
    """Click field then type character-by-character with variable delay."""
    cfg = _human_cfg()
    value = str(value or "")
    if not value:
        return False
    try:
        loc.scroll_into_view_if_needed(timeout=min(timeout_ms, 3000))
    except Exception:
        pass

    if not _human_click_locator(page, loc, timeout_ms=timeout_ms):
        try:
            loc.click(timeout=timeout_ms)
        except Exception:
            return False

    # Clear existing value.
    try:
        loc.fill("", timeout=min(timeout_ms, 2000))
    except Exception:
        try:
            page.keyboard.press("Control+a")
            page.keyboard.press("Backspace")
        except Exception:
            pass

    _human_pause(0.08, 0.25)

    if not cfg.get("enabled"):
        try:
            loc.fill(value, timeout=timeout_ms)
            return True
        except Exception:
            try:
                loc.fill(value, timeout=timeout_ms, force=True)
                return True
            except Exception:
                return False

    lo, hi = cfg.get("typing_ms") or (45, 130)
    lo, hi = int(lo), int(hi)
    if lo > hi:
        lo, hi = hi, lo
    try:
        for i, ch in enumerate(value):
            page.keyboard.type(ch, delay=0)
            # Variable IKI (inter-key interval); slower on symbols.
            base = random.uniform(lo, hi) / 1000.0
            if not ch.isalnum():
                base *= random.uniform(1.2, 1.8)
            # Occasional longer pause (thinking / shift).
            if random.random() < 0.06:
                base += random.uniform(0.12, 0.35)
            # Burst typing sometimes
            if random.random() < 0.15:
                base *= 0.45
            time.sleep(base)
            # Rare typo + backspace (very light — emails break if wrong).
            # Skip for emails/codes to avoid corruption.
        # Blur-ish pause after typing.
        _human_pause(0.1, 0.35)
        return True
    except Exception:
        try:
            loc.fill(value, timeout=timeout_ms)
            return True
        except Exception:
            return False


def _first_visible(page, selectors: list[str], timeout_ms: int = 2500):
    """Return the first visible locator matching any selector, or None."""
    for sel in selectors:
        try:
            loc = page.locator(sel).first
            if loc.count() == 0:
                continue
            if loc.is_visible(timeout=timeout_ms):
                return loc
        except Exception:
            continue
    return None


def _click_first(page, selectors: list[str], timeout_ms: int = 4000) -> bool:
    loc = _first_visible(page, selectors, timeout_ms=timeout_ms)
    if loc is None:
        return False
    return _human_click_locator(page, loc, timeout_ms=timeout_ms)


def _fill_first(page, selectors: list[str], value: str, timeout_ms: int = 4000) -> bool:
    loc = _first_visible(page, selectors, timeout_ms=timeout_ms)
    if loc is None:
        return False
    return _human_type_locator(page, loc, value, timeout_ms=timeout_ms)


def _page_text(page) -> str:
    try:
        return (page.inner_text("body") or "")[:4000]
    except Exception:
        try:
            return (page.content() or "")[:4000]
        except Exception:
            return ""


def _looks_like_phone_challenge(page) -> bool:
    text = _page_text(page).lower()
    needles = (
        "phone number",
        "verify your phone",
        "add a phone",
        "mobile number",
        "短信",
        "手机号",
        "验证手机",
    )
    return any(n in text for n in needles)


def _looks_like_cf_challenge(page) -> bool:
    """True for CF interstitial / managed challenge / Turnstile checkbox."""
    text = _page_text(page).lower()
    url = ""
    try:
        url = (page.url or "").lower()
    except Exception:
        pass
    if (
        "challenges.cloudflare.com" in url
        or "cdn-cgi/challenge" in url
        or "__cf_chl" in url
    ):
        return True
    needles = (
        "checking your browser",
        "just a moment",
        "cf-browser-verification",
        "attention required",
        "enable javascript and cookies",
        "verify you are human",
        "verify you are a human",
        "needs to review the security",
        "performing security verification",
        "确认您是真人",
        "请完成以下操作",
    )
    if any(n in text for n in needles):
        return True
    # Widget markers (checkbox challenge on OAuth callback).
    try:
        if page.locator("iframe[src*='challenges.cloudflare.com']").count() > 0:
            return True
        if page.locator("iframe[src*='turnstile']").count() > 0:
            return True
        if page.locator(".cf-turnstile, #cf-stage, #challenge-stage").count() > 0:
            return True
        if page.locator("text=Verify you are human").count() > 0:
            return True
    except Exception:
        pass
    return False


def _looks_like_session_ended(page) -> bool:
    """OpenAI auth interstitial when create-account is opened too early."""
    text = _page_text(page).lower()
    return (
        "your session has ended" in text
        or "session has ended" in text
        or "会话已结束" in text
        or "工作阶段已结束" in text
    )


def _url_is_chatgpt_app(url: str) -> bool:
    """True when we are on the ChatGPT app shell (not auth/login intermediate pages)."""
    u = (url or "").lower()
    if "chatgpt.com" not in u:
        return False
    # Intermediate auth / OAuth hops that still live under chatgpt.com
    blocked = (
        "/auth/",
        "login_with",
        "log-in",
        "/login",
        "sign-up",
        "signup",
        "create-account",
        "email-verification",
        "about-you",
        "choose-an-account",
        "auth.openai.com",
        "/api/auth/callback",
        "callback/openai",
    )
    return not any(b in u for b in blocked)


def _url_is_auth_intermediate(url: str) -> bool:
    u = (url or "").lower()
    return any(
        x in u
        for x in (
            "login_with",
            "email-verification",
            "about-you",
            "create-account",
            "auth.openai.com/log-in",
            "auth.openai.com/login",
            "/auth/",
            "/api/auth/callback",
            "callback/openai",
            "oauth",
            "code=",
        )
    )


def _url_is_oauth_callback(url: str) -> bool:
    u = (url or "").lower()
    return (
        "/api/auth/callback" in u
        or "callback/openai" in u
        or ("chatgpt.com" in u and "code=" in u and "state=" in u)
    )


# ── Browser registrar ────────────────────────────────────────────────


# Common desktop window sizes (outer chrome); keeps screens realistic.
_CAMOUFOX_WINDOWS: list[tuple[int, int]] = [
    (1366, 768),
    (1440, 900),
    (1536, 864),
    (1600, 900),
    (1680, 1050),
    (1920, 1080),
    (1280, 800),
    (1280, 720),
    (1512, 982),  # mac-ish scaled
    (1470, 956),
]

# Locales used when geoip is off (IP/locale mismatch is a common ban signal).
_CAMOUFOX_LOCALES = [
    "en-US",
    "en-GB",
    "en-CA",
    "en-AU",
]


class BrowserRegistrar:
    """Drive Playwright through ChatGPT signup and harvest session + AT."""

    def __init__(
        self,
        proxy: str = "",
        mail_config: dict | None = None,
        *,
        headless: bool = False,
        browser_channel: str = "",
        engine: str = "",
        timeout_ms: int = 120_000,
        signup_url: str = SIGNUP_URL_DEFAULT,
        slow_mo_ms: int = 0,
        keep_open_on_error: bool = False,
        humanize: bool = True,
        humanize_typing_ms: tuple[int, int] | str = (45, 130),
        camoufox_os: str = "random",
        camoufox_humanize: bool | float = True,
        camoufox_geoip: bool = True,
        camoufox_block_webrtc: bool = True,
        camoufox_block_images: bool = False,
        camoufox_locale: str = "",
        step_timeout_s: float = 90.0,
        warmup_reload_s: float = 10.0,
        otp_page_timeout_s: float = 45.0,
        about_you_settle_s: float = 5.0,
        yescaptcha: dict | None = None,
        turnstile_local: dict | None = None,
    ):
        self.proxy = str(proxy or "").strip()
        self.mail_config = mail_config or {}
        self.headless = bool(headless)
        # YesCaptcha: {enabled, client_key, base_url, task_type, max_wait}
        self.yescaptcha = dict(yescaptcha or {})
        # Local Playwright Turnstile (no external queue; ts_solver-style)
        self.turnstile_local = dict(turnstile_local or {})
        self.browser_channel = str(browser_channel or "").strip()
        # engine: "chrome" | "chromium" | "camoufox"  (channel=camoufox also works)
        eng = str(engine or "").strip().lower()
        ch = self.browser_channel.lower()
        if eng in ("camoufox", "fox") or ch in ("camoufox", "fox"):
            self.engine = "camoufox"
        elif ch in ("chrome", "msedge", "chromium", ""):
            self.engine = "chrome" if ch else (eng or "chrome")
            if eng in ("chrome", "chromium", "playwright"):
                self.engine = "chrome"
        else:
            self.engine = eng or "chrome"
        self.timeout_ms = int(timeout_ms or 120_000)
        self.signup_url = str(signup_url or SIGNUP_URL_DEFAULT).strip() or SIGNUP_URL_DEFAULT
        self.slow_mo_ms = int(slow_mo_ms or 0)
        self.keep_open_on_error = bool(keep_open_on_error)
        # Playwright-side human trajectory (mouse Bézier + keystroke timing).
        self.humanize = bool(humanize)
        self.humanize_typing_ms = _parse_typing_ms(humanize_typing_ms)
        self.camoufox_os = str(camoufox_os or "random").strip() or "random"
        # Camoufox built-in cursor humanize: True | False | max cursor seconds.
        self.camoufox_humanize = camoufox_humanize
        self.camoufox_geoip = bool(camoufox_geoip)
        self.camoufox_block_webrtc = bool(camoufox_block_webrtc)
        # Block images → faster page loads (signup is form-only; safe for speed).
        self.camoufox_block_images = bool(camoufox_block_images)
        self.camoufox_locale = str(camoufox_locale or "").strip()
        # Per-stage stuck detector (warm-up / OTP / about-you / session…).
        self.step_timeout_s = float(step_timeout_s if step_timeout_s is not None else 90.0)
        # Warm-up login_with: reload once if log-in UI not ready after N seconds.
        self.warmup_reload_s = float(
            warmup_reload_s if warmup_reload_s is not None else 10.0
        )
        # Wait for OTP/code page UI (not mailbox poll) before re-register.
        self.otp_page_timeout_s = float(
            otp_page_timeout_s if otp_page_timeout_s is not None else 45.0
        )
        # After about-you submit: max seconds to wait for URL → chatgpt.com
        # (replaces fixed 5s sleep). Values <30 are treated as 90.
        self.about_you_settle_s = float(
            about_you_settle_s if about_you_settle_s is not None else 90.0
        )
        self._stage = ""
        self._stage_t0 = 0.0
        self.device_id = str(uuid.uuid4())

    def _enter_stage(self, stage: str) -> None:
        """Mark a signup stage; resets the stuck-timer for that stage."""
        self._stage = str(stage or "").strip() or "unknown"
        self._stage_t0 = time.monotonic()

    def _check_stage_stuck(self, index: int = 0, *, detail: str = "") -> None:
        """Raise if the current stage has not advanced within ``step_timeout_s``."""
        limit = float(self.step_timeout_s or 0)
        if limit <= 0 or not self._stage or self._stage_t0 <= 0:
            return
        elapsed = time.monotonic() - self._stage_t0
        if elapsed < limit:
            return
        raise BrowserRegistrationError(
            f"stuck on [{self._stage}] for {elapsed:.0f}s "
            f"(limit {limit:.0f}s)"
            + (f" — {detail}" if detail else "")
        )

    # ── Camoufox fingerprint helpers ─────────────────────────────────

    @staticmethod
    def _normalize_camoufox_os(name: str) -> str:
        n = str(name or "").strip().lower()
        aliases = {
            "win": "windows",
            "windows": "windows",
            "mac": "macos",
            "macos": "macos",
            "osx": "macos",
            "darwin": "macos",
            "linux": "linux",
            "lin": "linux",
            "ubuntu": "linux",
        }
        return aliases.get(n, n)

    def _pick_camoufox_os(self) -> str:
        """Pick OS for this session's fingerprint (per-account diversity)."""
        raw = str(self.camoufox_os or "random").strip().lower()
        if raw in ("random", "auto", "*", "any"):
            # Residential pools skew Windows; avoid identical FP across workers.
            return random.choices(
                ["windows", "macos", "linux"],
                weights=[55, 30, 15],
                k=1,
            )[0]
        parts = [
            self._normalize_camoufox_os(p)
            for p in re.split(r"[,|/\s]+", raw)
            if p.strip()
        ]
        parts = [p for p in parts if p in ("windows", "macos", "linux")]
        if not parts:
            return "windows"
        if len(parts) == 1:
            return parts[0]
        return random.choice(parts)

    def _camoufox_humanize_value(self) -> bool | float:
        """True/False or max cursor-move seconds for Camoufox humanize."""
        h = self.camoufox_humanize
        if isinstance(h, (int, float)) and not isinstance(h, bool):
            return max(0.3, float(h))
        if h in (False, 0, "0", "false", "False", "no", "off"):
            return False
        # Default: light humanize (faster than 1.3–2.8; still non-instant clicks).
        return round(random.uniform(0.6, 1.4), 2)

    def _build_camoufox_fingerprint(self, os_name: str):
        """Generate a unique Browserforge Firefox fingerprint for one account."""
        from browserforge.fingerprints import Screen
        from camoufox.fingerprints import generate_fingerprint

        # Constrain to common laptop/desktop resolutions (avoid odd 4k/ultrawide
        # combos that rarely match residential proxies).
        screen = Screen(
            min_width=1280,
            max_width=1920,
            min_height=720,
            max_height=1080,
        )
        return generate_fingerprint(os=os_name, screen=screen)

    def _camoufox_window_size(self) -> tuple[int, int]:
        return random.choice(_CAMOUFOX_WINDOWS)

    def _camoufox_firefox_prefs(self) -> dict[str, Any]:
        """Extra prefs layered on Camoufox defaults (safe anti-leak set)."""
        prefs: dict[str, Any] = {
            # Camoufox already spoofs most APIs; keep WebRTC fully off unless
            # the user disabled block_webrtc (geoip path still fine).
            "media.peerconnection.enabled": not self.camoufox_block_webrtc,
            "media.navigator.enabled": False,
            "dom.battery.enabled": False,
            "dom.webdriver.enabled": False,
            "privacy.trackingprotection.enabled": True,
            "network.dns.disablePrefetch": True,
            "network.prefetch-next": False,
            "browser.cache.disk.enable": False,
            "browser.sessionstore.resume_from_crash": False,
            # Reduce noisy background pings during short signup sessions.
            "datareporting.healthreport.uploadEnabled": False,
            "datareporting.policy.dataSubmissionEnabled": False,
            "toolkit.telemetry.enabled": False,
            "toolkit.telemetry.unified": False,
        }
        return prefs

    def register(
        self,
        index: int = 0,
        mailbox: dict | None = None,
        *,
        soft_error: bool = False,
    ) -> dict[str, Any]:
        """Run full browser signup. Returns account record with tokens.

        ``mailbox`` may be pre-allocated by the pipeline so the live board
        can show the email before the browser starts.

        ``soft_error=True``: on failure do not mark mailbox/REG as failed
        (used for one automatic re-register after a stuck stage).
        """
        own_mailbox = mailbox is None
        if mailbox is None:
            mailbox = create_mailbox(self.mail_config, username=None)
        email = str(mailbox.get("address") or "").strip()
        if not email:
            if own_mailbox:
                release_mailbox(mailbox)
            raise BrowserRegistrationError("Mail provider did not return an address")

        # Bind board row as early as possible (pipeline may have done this too).
        try:
            plog.board_bind(index, email, note="registering…")
        except Exception:
            pass

        password = _random_password()  # may be unused (current flow is OTP-only)
        first_name, last_name = _random_name()
        full_name = f"{first_name} {last_name}"
        age = _random_age()

        # Enable human mouse/typing for all helpers on this worker thread.
        _set_human_cfg(
            enabled=self.humanize,
            typing_ms=self.humanize_typing_ms,
            move_steps=(14, 32) if self.humanize else (1, 2),
        )

        log = logging.getLogger(__name__)
        proxy_hint = self.proxy
        if "@" in proxy_hint:
            # user:pass@host → ***@host (don't leak credentials in logs)
            proxy_hint = "***@" + proxy_hint.rsplit("@", 1)[-1]
        log.info(
            f"[Task {index}] Browser register start: {email}"
            + (f" via {proxy_hint}" if proxy_hint else " (no proxy)")
            + f" engine={self.engine} headless={self.headless}"
            + f" step_timeout={self.step_timeout_s:.0f}s"
            + f" humanize={self.humanize}"
        )
        log.info(
            f"[Task {index}] Flow: create-account → email → OTP → "
            f"about-you(name+age={age}) → /api/auth/session"
        )

        # Lifecycle handles (engine-specific).
        pw = None  # playwright driver (chrome path)
        camoufox_cm = None  # Camoufox context manager
        browser = None
        context = None
        page = None
        try:
            # Always clean loop before launch (threads=1 reuses worker otherwise).
            _reset_thread_event_loop()
            eng = self.engine or "chrome"
            self._enter_stage("launch")
            plog.board_status(
                index,
                f"launch {eng}"
                + (f" via {proxy_hint}" if proxy_hint else " (direct)")
                + "…",
                email=email,
            )
            if self.engine == "camoufox":
                browser, context, page, camoufox_cm = self._launch_camoufox(index)
            else:
                browser, context, page, pw = self._launch_chrome(index)

            plog.board_status(index, f"{eng} ready · start signup…", email=email)
            context.set_default_timeout(self.timeout_ms)
            try:
                self._run_signup_ui(
                    page,
                    email=email,
                    password=password,
                    full_name=full_name,
                    age=age,
                    mailbox=mailbox,
                    index=index,
                )
            except BrowserRegistrationError:
                raise
            except Exception as e:
                url = ""
                try:
                    url = page.url or ""
                except Exception:
                    pass
                raise BrowserRegistrationError(
                    f"Signup UI failed at {url or '?'}: {e}"
                ) from e

            self._enter_stage("harvest")
            session_token, session_data = self._harvest_session(context, page, index)
            fields = session_payload_to_account_fields(session_data)
            if not fields.get("session_token"):
                fields["session_token"] = session_token

            mark_mailbox_result(mailbox, success=True)

            account = {
                "email": email,
                "password": password,
                "access_token": fields.get("access_token", ""),
                "refresh_token": "",
                "id_token": "",
                "session_token": fields.get("session_token", session_token),
                "chatgpt_account_id": fields.get("chatgpt_account_id", ""),
                "chatgpt_user_id": fields.get("chatgpt_user_id", ""),
                "plan_type": fields.get("plan_type", "") or "free",
                "expires": fields.get("expires", ""),
                "source_type": "browser",
                "browser_engine": self.engine,
                "created_at": datetime.now(timezone.utc).isoformat(),
                "device_id": self.device_id,
            }
            if not account["access_token"]:
                raise BrowserRegistrationError(
                    "Signup finished but no access_token from /api/auth/session"
                )
            # Close browser *before* return — finally alone can hang forever on
            # chatgpt.com WebSockets, leaving the board at "access_token ok".
            plog.board_status(index, "access_token ok · close browser…", email=email)
            self._force_close_browser(
                page=page,
                context=context,
                browser=browser,
                camoufox_cm=camoufox_cm,
                pw=pw,
                index=index,
            )
            page = context = browser = camoufox_cm = pw = None
            plog.board_status(index, "REG done · handoff JOIN…", email=email)
            log.info(
                f"[Task {index}] browser OK {email} "
                f"plan={account.get('plan_type') or '?'} engine={self.engine}"
            )
            return account

        except Exception as error:
            if not soft_error:
                mark_mailbox_result(mailbox, success=False, error=error)
                # Index-only who so live board can still mark REG fail if known.
                plog.fail("REG", f"#{index}/?", str(error)[:120], logger=log)
            else:
                log.warning(
                    f"[Task {index}] soft fail (will retry): {error}"
                )
            if self.keep_open_on_error and browser is not None and not soft_error:
                log.warning(
                    f"[Task {index}] keep_open_on_error: browser stays open "
                    f"120s — check the window, then it will close"
                )
                try:
                    time.sleep(120)
                except KeyboardInterrupt:
                    log.info(f"[Task {index}] inspection interrupted")
            raise
        finally:
            # Best-effort; timed so a stuck Playwright close cannot freeze REG.
            self._force_close_browser(
                page=page,
                context=context,
                browser=browser,
                camoufox_cm=camoufox_cm,
                pw=pw,
                index=index,
            )

    @staticmethod
    def _safe_close(obj, method: str = "close", label: str = "close") -> None:
        """Best-effort close **on this thread** (never cross-thread Playwright).

        Closing Playwright from another thread used to leave a sticky asyncio
        loop on the worker → next account failed with
        ``Sync API inside the asyncio loop`` even at threads=1.
        """
        if obj is None:
            return
        try:
            fn = getattr(obj, method, None)
            if callable(fn):
                fn()
        except Exception as e:
            logging.getLogger(__name__).debug("%s: %s", label, e)

    def _force_close_browser(
        self,
        *,
        page=None,
        context=None,
        browser=None,
        camoufox_cm=None,
        pw=None,
        index: int = 0,
    ) -> None:
        """Close page → context → browser → driver on **this** thread, then reset loop."""
        log = logging.getLogger(__name__)
        # Order matters: drop pages (WS) before browser/driver.
        self._safe_close(page, "close", f"page#{index}")
        self._safe_close(context, "close", f"ctx#{index}")
        self._safe_close(browser, "close", f"browser#{index}")
        if camoufox_cm is not None:
            try:
                camoufox_cm.__exit__(None, None, None)
            except Exception as e:
                log.debug("camoufox exit: %s", e)
        if pw is not None:
            try:
                pw.stop()
            except Exception as e:
                log.debug("pw.stop: %s", e)
        # Critical: next launch on a recycled pool thread needs a clean loop.
        _reset_thread_event_loop()
        log.debug(f"[Task {index}] force-close browser finished")

    def _launch_chrome(self, index: int):
        """Playwright Chromium/Chrome path. Returns (browser, context, page, pw)."""
        try:
            from playwright.sync_api import sync_playwright
        except ImportError as e:
            raise BrowserRegistrationError(
                "playwright is not installed. Run: "
                "pip install playwright && playwright install chromium"
            ) from e

        log = logging.getLogger(__name__)
        # Same-thread reset — required after any prior Chrome session on this worker.
        _reset_thread_event_loop()
        try:
            pw = sync_playwright().start()
        except Exception as e:
            _reset_thread_event_loop()
            # One more try after hard reset
            try:
                pw = sync_playwright().start()
            except Exception as e2:
                raise BrowserRegistrationError(
                    f"Failed to start Playwright: {e2}"
                ) from e2
        launch_kwargs: dict[str, Any] = {
            "headless": self.headless,
            "args": [
                "--disable-blink-features=AutomationControlled",
                "--no-sandbox",
                "--disable-dev-shm-usage",
            ],
        }
        if self.slow_mo_ms > 0:
            launch_kwargs["slow_mo"] = self.slow_mo_ms
        if self.browser_channel and self.browser_channel.lower() not in (
            "camoufox",
            "fox",
            "chromium",
            "",
        ):
            launch_kwargs["channel"] = self.browser_channel
        elif self.browser_channel.lower() == "chromium":
            pass  # default bundled chromium

        try:
            browser = pw.chromium.launch(**launch_kwargs)
        except Exception as e:
            raise BrowserRegistrationError(
                f"Failed to launch browser "
                f"(channel={self.browser_channel or 'chromium'}): {e}"
            ) from e

        context_kwargs: dict[str, Any] = {
            "locale": "en-US",
            "timezone_id": "America/New_York",
            "viewport": {"width": 1280, "height": 900},
            "user_agent": (
                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
                "AppleWebKit/537.36 (KHTML, like Gecko) "
                "Chrome/145.0.0.0 Safari/537.36"
            ),
            "extra_http_headers": {"Accept-Language": "en-US,en;q=0.9"},
        }
        proxy_cfg = _playwright_proxy(self.proxy)
        if proxy_cfg:
            context_kwargs["proxy"] = proxy_cfg
            log.info(
                f"[Task {index}] Playwright proxy server={proxy_cfg.get('server')}"
            )

        try:
            context = browser.new_context(**context_kwargs)
        except Exception as e:
            raise BrowserRegistrationError(
                f"Failed to create browser context (proxy?): {e}"
            ) from e

        page = context.new_page()
        page.add_init_script(
            "Object.defineProperty(navigator, 'webdriver', "
            "{get: () => undefined});"
        )
        return browser, context, page, pw

    def _launch_camoufox(self, index: int):
        """Camoufox anti-detect Firefox. Returns (browser, context, page, cm).

        Each call builds a **fresh Browserforge fingerprint** (OS / screen /
        WebGL / fonts / navigator) so concurrent workers do not share a
        device identity. Playwright 1.61 + Camoufox needs ``no_viewport=True``.
        """
        log = logging.getLogger(__name__)
        try:
            from camoufox.sync_api import Camoufox
        except ImportError as e:
            raise BrowserRegistrationError(
                "camoufox is not installed. Run:\n"
                "  uv pip install -U 'camoufox[geoip]'\n"
                "  python -m camoufox fetch"
            ) from e

        # Always HTTP when proxy has user/pass (Playwright forbids SOCKS5 auth).
        proxy_cfg = _playwright_proxy(self.proxy)
        os_name = self._pick_camoufox_os()
        window = self._camoufox_window_size()
        humanize = self._camoufox_humanize_value()

        # Unique fingerprint per account (navigator, screen, fonts, codecs…).
        fingerprint = None
        fp_ua = ""
        fp_hw = ""
        try:
            fingerprint = self._build_camoufox_fingerprint(os_name)
            nav = getattr(fingerprint, "navigator", None)
            fp_ua = str(getattr(nav, "userAgent", "") or "")[:96]
            cores = getattr(nav, "hardwareConcurrency", None)
            plat = getattr(nav, "platform", None)
            fp_hw = f"cores={cores} platform={plat}"
        except Exception as e:
            log.warning(
                f"[Task {index}] fingerprint generate failed ({e}); "
                f"Camoufox will auto-generate from os={os_name}"
            )

        kwargs: dict[str, Any] = {
            "headless": self.headless,
            "os": os_name,
            "humanize": humanize,
            "window": window,
            # Helps Cloudflare Turnstile / cross-origin widgets.
            "disable_coop": True,
            "i_know_what_im_doing": True,
            # WebRTC IP leak is a top ban signal behind proxies.
            "block_webrtc": self.camoufox_block_webrtc,
            # Don't persist disk cache across signups on the same host.
            "enable_cache": False,
            "firefox_user_prefs": self._camoufox_firefox_prefs(),
        }
        if self.camoufox_block_images:
            kwargs["block_images"] = True
        if fingerprint is not None:
            kwargs["fingerprint"] = fingerprint

        # Locale: prefer geoip (matches proxy egress). Else fixed/random list.
        if proxy_cfg:
            kwargs["proxy"] = proxy_cfg
            if self.camoufox_geoip:
                kwargs["geoip"] = True
            log.info(
                f"[Task {index}] Camoufox proxy server={proxy_cfg.get('server')} "
                f"user={bool(proxy_cfg.get('username'))} "
                f"geoip={bool(kwargs.get('geoip'))}"
            )
        else:
            log.info(f"[Task {index}] Camoufox launch (no proxy)")

        if not kwargs.get("geoip"):
            loc = self.camoufox_locale or random.choice(_CAMOUFOX_LOCALES)
            kwargs["locale"] = loc

        log.info(
            f"[Task {index}] Camoufox FP os={os_name} window={window[0]}x{window[1]} "
            f"humanize={humanize} webrtc_block={self.camoufox_block_webrtc} "
            f"block_images={self.camoufox_block_images} "
            f"{fp_hw} ua={fp_ua or '(auto)'}"
        )

        # Only serialize the browser binary start (driver race). Context/page
        # creation runs outside the lock so other workers can launch sooner.
        cm = None
        browser = None
        with _CAMOUFOX_LAUNCH_LOCK:
            _reset_thread_event_loop()
            try:
                cm = Camoufox(**kwargs)
                browser = cm.__enter__()
            except Exception as e:
                if cm is not None:
                    try:
                        cm.__exit__(None, None, None)
                    except Exception:
                        pass
                _reset_thread_event_loop()
                raise BrowserRegistrationError(
                    f"Failed to launch Camoufox: {e}"
                ) from e

        # no_viewport required for Camoufox + modern Playwright.
        # Do NOT override user_agent / locale / timezone — fights FP.
        try:
            ctx_kwargs: dict[str, Any] = {
                "no_viewport": True,
                "java_script_enabled": True,
                "ignore_https_errors": True,
            }
            if proxy_cfg:
                ctx_kwargs["proxy"] = proxy_cfg
            context = browser.new_context(**ctx_kwargs)
            page = context.new_page()
            self._harden_camoufox_page(page, index)
        except Exception as e:
            try:
                cm.__exit__(None, None, None)
            except Exception:
                pass
            _reset_thread_event_loop()
            raise BrowserRegistrationError(
                f"Failed to create Camoufox context/page: {e}"
            ) from e

        return browser, context, page, cm

    def _harden_camoufox_page(self, page, index: int) -> None:
        """Reduce Camoufox/Playwright driver crashes from page JS errors.

        PW 1.61 + Camoufox can die with:
          TypeError: Cannot read properties of undefined (reading 'url')
        when Firefox emits a pageerror without location. Swallow what we can
        from the Python side and avoid heavy in-page evaluate during harvest.
        """
        log = logging.getLogger(__name__)

        def _on_pageerror(err) -> None:
            try:
                msg = str(err)[:160]
            except Exception:
                msg = "pageerror"
            log.debug(f"[Task {index}] Camoufox pageerror (ignored): {msg}")

        def _on_crash(_page) -> None:
            log.warning(f"[Task {index}] Camoufox page crashed")

        try:
            page.on("pageerror", _on_pageerror)
            page.on("crash", _on_crash)
        except Exception:
            pass
        # Extra: don't let console errors bubble as hard failures.
        try:
            page.on("console", lambda msg: None)
        except Exception:
            pass

    # ── UI steps (hand-confirmed) ────────────────────────────────────
    #
    #   0. https://chatgpt.com/auth/login_with?callback_path=  (warm-up)
    #   1. https://auth.openai.com/create-account
    #   2. email → button[name=intent][value=email] 「继续」
    #   3. /email-verification OTP
    #      → button[name=intent][value=validate] 「继续」
    #   4. /about-you name + age
    #      → button 「完成帐户创建」
    #   5. lands on chatgpt.com → /api/auth/session

    def _run_signup_ui(
        self,
        page,
        *,
        email: str,
        password: str,
        full_name: str,
        age: str,
        mailbox: dict,
        index: int,
    ) -> None:
        log = logging.getLogger(__name__)

        # Step 0 — fully load login_with first (required before create-account).
        # Jumping to create-account too early → "Your session has ended".
        self._enter_stage("warm-up")
        plog.board_status(index, "warm-up login_with…", email=email)
        log.info(
            f"[Task {index}] Step0 warm-up {CHATGPT_LOGIN_WARMUP} (wait until ready)"
        )
        self._warmup_chatgpt(page, index)
        self._check_stage_stuck(index, detail="warm-up")

        # Step 1 — create-account (only after warm-up is fully ready)
        self._enter_stage("create-account")
        url = (self.signup_url or SIGNUP_URL_DEFAULT).strip() or SIGNUP_URL_DEFAULT
        if "create-account" not in url:
            url = SIGNUP_URL_DEFAULT
        plog.board_status(index, "open create-account…", email=email)
        log.info(f"[Task {index}] Step1 open {url}")
        page.goto(url, wait_until="domcontentloaded")
        self._wait_out_cloudflare(page, index)
        self._recover_session_ended(page, index)

        # Step 2 — email + intent=email 「继续」
        self._enter_stage("email")
        plog.board_status(index, f"fill email → {email}", email=email)
        log.info(f"[Task {index}] Step2 fill email + click intent=email")
        if not self._wait_and_fill_email(page, email, index):
            raise BrowserRegistrationError(
                f"Could not find email input on {page.url} "
                f"(CF challenge or wrong page?)"
            )
        plog.board_status(index, "submit email…", email=email)
        if not self._click_intent(page, "email", labels=("继续", "Continue")):
            # Fallback generic continue
            self._click_continue(page)
        time.sleep(0.7)
        self._raise_if_hard_block(page, "after email")

        # Intermediate: https://auth.openai.com/create-account/password
        # Prefer 「Sign up with a one-time code」; if click doesn't navigate,
        # fall back to fill-password + Continue (also ends at email OTP).
        self._enter_stage("password")
        reached_otp = False
        saw_create_password = False
        pw_t0 = time.monotonic()
        pw_deadline = time.time() + 18.0
        click_attempts = 0
        while time.time() < pw_deadline:
            self._check_stage_stuck(index, detail="password page")
            elapsed = time.monotonic() - pw_t0
            url_now = (page.url or "").lower()
            if self._find_otp_input(page, timeout_ms=400) is not None or self._is_otp_page(
                page
            ):
                log.info(f"[Task {index}] OTP page already up after email")
                reached_otp = True
                break
            if "email-verification" in url_now or "about-you" in url_now:
                reached_otp = True
                break

            if self._is_create_password_page(page):
                saw_create_password = True
                plog.board_status(
                    index,
                    f"password page · one-time code… {elapsed:.0f}s",
                    email=email,
                )
                click_attempts += 1
                log.info(
                    f"[Task {index}] On create-account/password ({page.url}) "
                    f"try#{click_attempts} passwordless OTP"
                )
                clicked = self._try_passwordless_otp_signup(page, index)
                if clicked:
                    # Wait for real navigation — "clicked" alone is not enough.
                    leave_ok = False
                    for _ in range(8):
                        time.sleep(0.45)
                        if (
                            self._find_otp_input(page, timeout_ms=500) is not None
                            or self._is_otp_page(page)
                            or not self._is_create_password_page(page)
                        ):
                            leave_ok = True
                            break
                    if leave_ok:
                        reached_otp = True
                        plog.board_status(index, "left password page → OTP…", email=email)
                        break
                    plog.board_status(
                        index,
                        f"password page stuck after click · {elapsed:.0f}s",
                        email=email,
                    )
                    log.warning(
                        f"[Task {index}] passwordless click did not leave "
                        f"/password (url={page.url}) — retry/fallback"
                    )
                time.sleep(0.4)
                continue

            # Not on password page yet; opportunistic click if button appears early.
            if self._try_passwordless_otp_signup(page, index):
                time.sleep(0.8)
                if (
                    self._find_otp_input(page, timeout_ms=800) is not None
                    or self._is_otp_page(page)
                    or not self._is_create_password_page(page)
                ):
                    reached_otp = True
                    break
            time.sleep(0.4)

        # Fallback: OpenAI classic path — set password then email OTP.
        # Use when passwordless button never navigates (common soft-block / lag).
        if not reached_otp and (
            saw_create_password or self._is_create_password_page(page)
        ):
            plog.board_status(
                index, "password page · fallback fill password…", email=email
            )
            log.warning(
                f"[Task {index}] passwordless stuck — fallback fill password "
                f"+ Continue (url={page.url})"
            )
            if self._fill_password(page, password, timeout_ms=2500):
                self._click_continue(
                    page,
                    also=(
                        "Create account",
                        "Sign up",
                        "Continue",
                        "继续",
                        "下一步",
                        "Create Password",
                    ),
                )
                time.sleep(1.2)
                # Some UIs still show one-time code after password continue.
                self._try_passwordless_otp_signup(page, index)
                time.sleep(0.6)
            else:
                # Last tries on passwordless only.
                for _ in range(3):
                    if self._try_passwordless_otp_signup(page, index):
                        time.sleep(1.0)
                        if not self._is_create_password_page(page):
                            break
                    time.sleep(0.6)
        elif not reached_otp and self._fill_password(page, password, timeout_ms=1200):
            plog.board_status(index, "fill password (legacy)…", email=email)
            self._click_continue(
                page,
                also=("Create account", "Sign up", "Continue", "继续", "下一步"),
            )
            time.sleep(1.0)
        elif not reached_otp:
            log.info(f"[Task {index}] No password step (OTP-only flow)")

        # Step 3 — OTP + intent=validate 「继续」
        self._enter_stage("otp")
        plog.board_status(index, "wait OTP page…", email=email)
        log.info(f"[Task {index}] Step3 email OTP")
        self._handle_otp(page, mailbox, index, password=password)

        # Step 4 — about-you + 「完成帐户创建」(age 或 birthday 两种 UI)
        self._enter_stage("about-you")
        plog.board_status(
            index, f"about-you name={full_name} age/bday…", email=email
        )
        log.info(
            f"[Task {index}] Step4 about-you name={full_name!r} age={age} "
            f"(or birthday derived from age)"
        )
        self._handle_about_you(page, full_name=full_name, age=age, index=index)

        # Step 5 — wait OAuth + session cookie (harvest uses browser then HTTP)
        self._enter_stage("session")
        plog.board_status(index, "wait session cookie…", email=email)
        log.info(f"[Task {index}] Step5 wait for session cookie / oauth settle")
        self._wait_for_logged_in(page, index)
        plog.board_status(index, "session ready · harvest AT…", email=email)

    def _warmup_ready_reason(self, page) -> str:
        """Return a short reason string if warm-up UI is ready, else empty.

        Expected chain:
          chatgpt.com/auth/login_with?callback_path=  →  auth.openai.com/log-in
        Ready = log-in (or equivalent) form/UI is painted, not blank/CF.
        """
        if _looks_like_cf_challenge(page):
            return ""
        if _looks_like_session_ended(page):
            return ""

        url = ""
        try:
            url = (page.url or "").lower()
        except Exception:
            pass

        # Strong signals: email field / continue on auth log-in.
        strong = [
            'input[name="email"]',
            'input[type="email"]',
            'input[autocomplete="email"]',
            'input[name="username"]',
            'button[type="submit"][name="intent"]',
            'button[data-dd-action-name="Continue"]',
            'button[type="submit"]:has-text("Continue")',
            'button[type="submit"]:has-text("继续")',
            'button:has-text("Continue")',
            'button:has-text("继续")',
        ]
        if _first_visible(page, strong, timeout_ms=350) is not None:
            if "auth.openai.com" in url and (
                "log-in" in url or "login" in url or "create-account" in url
            ):
                return f"auth_form@{url}"
            if "auth.openai.com" in url or "chatgpt.com" in url:
                return f"email_or_continue@{url}"

        # Secondary: sign-up / log-in chrome without form yet.
        weak = [
            "button:has-text('Log in')",
            "button:has-text('Sign up')",
            "a:has-text('Sign up')",
            "a:has-text('Log in')",
            "text=Welcome",
            "text=Log in",
            "text=Sign up",
            "text=登录",
            "text=注册",
        ]
        if _first_visible(page, weak, timeout_ms=300) is not None:
            if "auth.openai.com/log-in" in url or "auth.openai.com/login" in url:
                return f"log_in_chrome@{url}"

        return ""

    def _warmup_chatgpt(self, page, index: int) -> None:
        """Open login_with, poll until redirected UI is ready, then continue ASAP.

        Chain (hand-confirmed)::

            https://chatgpt.com/auth/login_with?callback_path=
              → often redirects to →
            https://auth.openai.com/log-in

        Do **not** wait for networkidle (SPAs hang ~15s). Poll the URL/DOM
        every few hundred ms and proceed as soon as log-in UI is ready.

        If still not ready after ``warmup_reload_s`` (default **10s**), reload
        ``login_with`` **once** — common hang on blank first hop.
        """
        log = logging.getLogger(__name__)
        warmup_url = CHATGPT_LOGIN_WARMUP
        t0 = time.monotonic()
        # Seconds without ready UI before one automatic reload.
        reload_after = float(getattr(self, "warmup_reload_s", 10.0) or 10.0)
        reload_after = max(3.0, min(reload_after, 45.0))

        def _goto_warmup(*, label: str, timeout_ms: int | None = None) -> None:
            to = int(timeout_ms if timeout_ms is not None else self.timeout_ms)
            try:
                page.goto(
                    warmup_url,
                    wait_until="domcontentloaded",
                    timeout=to,
                )
            except Exception as e:
                raise BrowserRegistrationError(
                    f"Failed to open login_with warm-up ({label}): {e}"
                ) from e

        # domcontentloaded is enough — redirect + React paint handled by poll.
        _goto_warmup(label="initial")

        log.info(
            f"[Task {index}] warm-up landed url={page.url} "
            f"({time.monotonic() - t0:.1f}s) — polling until log-in ready "
            f"(reload once if stuck >{reload_after:.0f}s)"
        )
        plog.board_status(index, "warm-up landed · wait log-in UI…")

        last_url = ""
        last_status_t = 0.0
        deadline = time.time() + 60
        ready_reason = ""
        reloaded = False
        # Timer for "how long since last load without ready UI"
        segment_t0 = t0

        while time.time() < deadline:
            self._check_stage_stuck(index, detail="warm-up login_with")
            try:
                url = page.url or ""
            except Exception:
                url = ""
            elapsed = time.monotonic() - t0
            seg_elapsed = time.monotonic() - segment_t0

            if url != last_url:
                log.info(
                    f"[Task {index}] warm-up poll url={url} "
                    f"({elapsed:.1f}s)"
                )
                last_url = url
                host = url.split("/")[2] if "://" in url else url[:40]
                plog.board_status(index, f"warm-up → {host} ({elapsed:.0f}s)")
                last_status_t = elapsed
            elif elapsed - last_status_t >= 2.0:
                extra = " · will reload" if (
                    not reloaded and seg_elapsed >= reload_after - 2
                ) else ""
                plog.board_status(
                    index,
                    f"warm-up wait log-in… {elapsed:.0f}s{extra}",
                )
                last_status_t = elapsed

            if _looks_like_cf_challenge(page):
                plog.board_status(index, f"CF challenge (warm-up)… {elapsed:.0f}s")
                # Short CF wait inside the poll loop (don't block 45s).
                self._wait_out_cloudflare(page, index, max_wait=8.0)
                # CF counts as activity — don't reload mid-challenge immediately.
                segment_t0 = time.monotonic()
                continue

            if _looks_like_session_ended(page):
                log.warning(
                    f"[Task {index}] session-ended during warm-up — reopen login_with"
                )
                plog.board_status(index, "session ended · reopen login_with…")
                try:
                    page.goto(
                        warmup_url,
                        wait_until="domcontentloaded",
                        timeout=30_000,
                    )
                except Exception:
                    pass
                reloaded = True  # counts as the one reload
                segment_t0 = time.monotonic()
                time.sleep(0.5)
                continue

            ready_reason = self._warmup_ready_reason(page)
            if ready_reason:
                break

            # Stuck on first hop / blank log-in: reload login_with once @ 10s.
            if not reloaded and seg_elapsed >= reload_after:
                reloaded = True
                log.warning(
                    f"[Task {index}] warm-up stuck {seg_elapsed:.0f}s "
                    f"(url={url}) — reload login_with once"
                )
                plog.board_status(
                    index,
                    f"warm-up stuck {seg_elapsed:.0f}s · reload once…",
                )
                try:
                    _goto_warmup(label="reload", timeout_ms=30_000)
                except BrowserRegistrationError as e:
                    log.warning(f"[Task {index}] warm-up reload failed: {e}")
                segment_t0 = time.monotonic()
                last_url = ""
                time.sleep(0.4)
                continue

            time.sleep(0.25)

        elapsed = time.monotonic() - t0
        if not ready_reason:
            # Soft accept: already on auth log-in with some body text.
            try:
                url = (page.url or "").lower()
                body_len = len(_page_text(page))
            except Exception:
                url, body_len = "", 0
            if (
                "auth.openai.com" in url
                and ("log-in" in url or "login" in url)
                and body_len > 40
                and not _looks_like_cf_challenge(page)
            ):
                ready_reason = f"soft_log_in@{url}"
            else:
                plog.board_status(index, f"warm-up timeout {elapsed:.0f}s")
                raise BrowserRegistrationError(
                    f"Warm-up never became ready after {elapsed:.1f}s "
                    f"(last url={page.url}). Expected auth.openai.com/log-in UI."
                )

        # Tiny settle only — cookies usually set by the time form is visible.
        settle = 0.4
        log.info(
            f"[Task {index}] warm-up READY reason={ready_reason} "
            f"url={page.url} total={elapsed:.1f}s reloaded={reloaded} "
            f"— settle {settle:.1f}s then create-account"
        )
        plog.board_status(index, f"warm-up ok ({elapsed:.0f}s) · go signup…")
        time.sleep(settle)

    def _recover_session_ended(self, page, index: int) -> None:
        """If create-account shows 「Your session has ended」, re-warm and retry."""
        log = logging.getLogger(__name__)
        if not _looks_like_session_ended(page):
            return
        log.warning(
            f"[Task {index}] create-account shows 'session has ended' — "
            f"re-warm login_with then reopen signup"
        )
        self._warmup_chatgpt(page, index)
        page.goto(SIGNUP_URL_DEFAULT, wait_until="domcontentloaded")
        self._wait_out_cloudflare(page, index)
        time.sleep(1.0)
        if _looks_like_session_ended(page):
            # Try Sign up from the warm-up page, else force create-account.
            if not _click_first(
                page,
                [
                    "a:has-text('Sign up')",
                    "button:has-text('Sign up')",
                    "text=Sign up",
                    "a:has-text('注册')",
                    "text=注册",
                    "a:has-text('Create account')",
                    "text=Create account",
                ],
                timeout_ms=4000,
            ):
                page.goto(SIGNUP_URL_DEFAULT, wait_until="domcontentloaded")
            self._wait_out_cloudflare(page, index)
            time.sleep(1.0)
        if _looks_like_session_ended(page):
            raise BrowserRegistrationError(
                "Stuck on 'Your session has ended' after login_with warm-up. "
                "Page may not have fully initialized — try headed mode, "
                "slower proxy, or increase registration.browser.timeout_ms"
            )

    def _wait_and_fill_email(self, page, email: str, index: int) -> bool:
        """Wait for CF/React to paint the email field, then fill it."""
        log = logging.getLogger(__name__)
        deadline = time.time() + 45
        while time.time() < deadline:
            self._wait_out_cloudflare(page, index, max_wait=15.0)
            if _looks_like_session_ended(page):
                self._recover_session_ended(page, index)
            if self._fill_email(page, email):
                return True
            time.sleep(0.8)
        log.warning(f"[Task {index}] email input never appeared; url={page.url}")
        return False

    def _fill_email(self, page, email: str) -> bool:
        selectors = [
            'input[name="email"]',
            'input[type="email"]',
            'input[id*="email" i]',
            'input[autocomplete="email"]',
            'input[placeholder*="email" i]',
            'input[placeholder*="邮件" i]',
            'input[placeholder*="郵箱" i]',
            'input[name="username"]',
        ]
        if _fill_first(page, selectors, email, timeout_ms=2500):
            return True
        try:
            loc = page.get_by_label(re.compile(r"email|邮件|郵箱|電子", re.I)).first
            if loc.count():
                loc.fill(email)
                return True
        except Exception:
            pass
        return False

    def _fill_password(self, page, password: str, timeout_ms: int = 4000) -> bool:
        selectors = [
            'input[name="password"]',
            'input[type="password"]',
            'input[id*="password" i]',
            'input[autocomplete="new-password"]',
            'input[autocomplete="current-password"]',
        ]
        return _fill_first(page, selectors, password, timeout_ms=timeout_ms)

    def _is_create_password_page(self, page) -> bool:
        """True on https://auth.openai.com/create-account/password (or equivalent UI)."""
        url = ""
        try:
            url = (page.url or "").lower()
        except Exception:
            pass
        # Canonical intermediate after email submit.
        if "create-account/password" in url or (
            "auth.openai.com" in url
            and "/password" in url
            and "create-account" in url
        ):
            return True
        text = _page_text(page).lower()
        if any(
            n in text
            for n in (
                "create a password",
                "创建密码",
                "建立密碼",
                "sign up with a one-time code",
                "使用一次性验证码注册",
                "使用一次性驗證碼註冊",
                "使用一次性代码注册",
            )
        ):
            return True
        # Button present even if heading text is delayed.
        if _first_visible(
            page,
            [
                'button[name="intent"][value="passwordless_signup_send_otp"]',
                'button[value="passwordless_signup_send_otp"]',
            ],
            timeout_ms=400,
        ) is not None:
            return True
        return False

    def _try_passwordless_otp_signup(self, page, index: int) -> bool:
        """Click 「Sign up with a one-time code」 on /create-account/password.

        URL: https://auth.openai.com/create-account/password
        After email submit OpenAI often lands here. Prefer passwordless path:

          <button name="intent" value="passwordless_signup_send_otp">
            Sign up with a one-time code
          </button>

        Returns True only if a click was issued (caller must verify navigation).
        """
        log = logging.getLogger(__name__)
        intent_selectors = [
            'button[name="intent"][value="passwordless_signup_send_otp"]',
            'button[type="submit"][name="intent"][value="passwordless_signup_send_otp"]',
            'button[value="passwordless_signup_send_otp"]',
            'form button[name="intent"][value="passwordless_signup_send_otp"]',
        ]
        text_selectors = [
            'button:has-text("Sign up with a one-time code")',
            'button:has-text("one-time code")',
            'button:has-text("使用一次性验证码")',
            'button:has-text("使用一次性驗證碼")',
            'button:has-text("一次性验证码")',
            'button:has-text("一次性驗證碼")',
            'button:has-text("验证码注册")',
            'button:has-text("验证码註冊")',
        ]

        on_pw_page = self._is_create_password_page(page)

        def _force_click(loc) -> bool:
            try:
                loc.scroll_into_view_if_needed(timeout=1500)
            except Exception:
                pass
            try:
                loc.click(timeout=3000, force=True)
                return True
            except Exception:
                try:
                    loc.evaluate("el => el.click()")
                    return True
                except Exception:
                    return False

        # Exact intent button → always click (most reliable).
        for sel in intent_selectors:
            try:
                loc = page.locator(sel).first
                if loc.count() and loc.is_visible(timeout=600):
                    if _force_click(loc):
                        plog.board_status(index, "clicked one-time code…")
                        log.info(
                            f"[Task {index}] Clicked passwordless OTP signup "
                            f"(intent=passwordless_signup_send_otp) url={page.url}"
                        )
                        return True
            except Exception:
                continue
        if _click_first(page, intent_selectors, timeout_ms=1200):
            plog.board_status(index, "clicked one-time code…")
            return True

        # Text / role fallback only when create-password UI is showing.
        if not on_pw_page:
            return False

        if _click_first(page, text_selectors, timeout_ms=2000):
            plog.board_status(index, "clicked one-time code…")
            log.info(
                f"[Task {index}] Clicked 「Sign up with a one-time code」 "
                f"(text match) url={page.url}"
            )
            return True

        for pattern in (
            re.compile(r"sign up with a one-time code", re.I),
            re.compile(r"one[- ]time code", re.I),
            re.compile(r"一次性验证码|一次性驗證碼|验证码注册|驗證碼註冊"),
        ):
            try:
                loc = page.get_by_role("button", name=pattern).first
                if loc.count() and loc.is_visible(timeout=800):
                    if _force_click(loc):
                        plog.board_status(index, "clicked one-time code…")
                        log.info(
                            f"[Task {index}] Clicked passwordless OTP via role "
                            f"name={pattern.pattern!r}"
                        )
                        return True
            except Exception:
                continue
        return False

    def _click_intent(
        self,
        page,
        value: str,
        *,
        labels: tuple[str, ...] = ("继续", "Continue"),
        timeout_ms: int = 6000,
    ) -> bool:
        """Click OpenAI auth submit buttons keyed by name=intent value=...

        Hand-confirmed:
          email page:  <button type=submit name=intent value=email>继续</button>
          OTP page:    <button type=submit name=intent value=validate>继续</button>
          password page alt:
            <button name=intent value=passwordless_signup_send_otp>
              Sign up with a one-time code
            </button>
        """
        selectors = [
            f'button[type="submit"][name="intent"][value="{value}"]',
            f'button[name="intent"][value="{value}"]',
            f'button[data-dd-action-name="Continue"][value="{value}"]',
        ]
        if _click_first(page, selectors, timeout_ms=timeout_ms):
            return True
        # Text fallback (Chinese / English)
        for label in labels:
            if _click_first(
                page,
                [
                    f'button[type="submit"]:has-text("{label}")',
                    f'button:has-text("{label}")',
                ],
                timeout_ms=2000,
            ):
                return True
        return False

    def _click_finish_account(self, page) -> bool:
        """About-you primary button: 「完成帐户创建」."""
        selectors = [
            'button[type="submit"]:has-text("完成帐户创建")',
            'button[type="submit"]:has-text("完成账户创建")',
            'button:has-text("完成帐户创建")',
            'button:has-text("完成账户创建")',
            'button[type="submit"]:has-text("Create account")',
            'button[type="submit"]:has-text("Finish")',
            'button[data-dd-action-name="Continue"][type="submit"]',
            'button[type="submit"]',
        ]
        return _click_first(page, selectors, timeout_ms=6000)

    def _click_continue(
        self,
        page,
        also: tuple[str, ...] = (
            "Continue",
            "Next",
            "Submit",
            "继续",
            "下一步",
            "提交",
            "确认",
            "確認",
            "完成帐户创建",
            "完成账户创建",
        ),
    ) -> None:
        selectors = [
            'button[type="submit"][name="intent"]',
            'button[type="submit"]',
            'button[data-dd-action-name="Continue"]',
            '[data-testid="continue-button"]',
            '[data-testid="login-button"]',
        ]
        for label in also:
            selectors.append(f"button:has-text('{label}')")
            selectors.append(f"text={label}")
        if not _click_first(page, selectors, timeout_ms=6000):
            try:
                page.keyboard.press("Enter")
            except Exception:
                pass

    def _is_otp_page(self, page) -> bool:
        """True when the email verification / Check your inbox UI is showing."""
        url = ""
        try:
            url = (page.url or "").lower()
        except Exception:
            pass
        if any(
            x in url
            for x in (
                "email-verification",
                "email_verification",
                "verify-email",
                "otp",
                "check-email",
            )
        ):
            return True
        text = _page_text(page).lower()
        needles = (
            "check your inbox",
            "verification code",
            "we just sent",
            "enter the code",
            "验证码",
            "查收邮件",
            "查看你的收件箱",
            "我们刚刚发送",
            "resend email",
            "重新发送",
        )
        return any(n in text for n in needles)

    def _find_otp_input(self, page, timeout_ms: int = 2000):
        """Locate the OTP / Code field on the verification page."""
        otp_selectors = [
            'input[name="code"]',
            'input[autocomplete="one-time-code"]',
            'input[inputmode="numeric"]',
            'input[name="otp"]',
            'input[id*="code" i]',
            'input[placeholder*="code" i]',
            'input[placeholder*="Code" i]',
            'input[placeholder*="验证码" i]',
            'input[placeholder*="驗證碼" i]',
            'input[type="text"][maxlength="6"]',
            'input[type="tel"]',
        ]
        loc = _first_visible(page, otp_selectors, timeout_ms=timeout_ms)
        if loc is not None:
            return loc
        # Floating label "Code" (React Aria) — get_by_label / role.
        for pattern in (
            re.compile(r"^code$", re.I),
            re.compile(r"verification code", re.I),
            re.compile(r"验证码"),
            re.compile(r"驗證碼"),
        ):
            try:
                loc = page.get_by_label(pattern).first
                if loc.count() and loc.is_visible(timeout=800):
                    return loc
            except Exception:
                continue
            try:
                loc = page.get_by_placeholder(pattern).first
                if loc.count() and loc.is_visible(timeout=500):
                    return loc
            except Exception:
                continue
        return None

    def _handle_otp(
        self,
        page,
        mailbox: dict,
        index: int,
        *,
        password: str = "",
    ) -> None:
        """Fill email OTP. Never skip just because some auth cookie exists.

        Previous bug: leftover ``oai-client-auth-session`` / warm-up cookies made
        ``_cookie_has_session`` true on the 「Check your inbox」 page, so we
        skipped mailbox polling while the Code field sat empty.

        Waiting for the OTP **page/input** is capped by ``otp_page_timeout_s``
        (default 45s). Timeout raises → worker soft re-register (max once).
        Mailbox polling uses its own longer timeout afterward.
        """
        log = logging.getLogger(__name__)
        page_limit = float(self.otp_page_timeout_s or 45.0)
        page_limit = max(15.0, min(page_limit, 120.0))
        page_deadline = time.time() + page_limit
        otp_field = None

        last_passwordless_try = 0.0
        password_fallback_used = False
        resend_tried = False
        otp_wait_t0 = time.monotonic()
        last_board_t = -1.0
        # Align stuck detector with page wait (not full step_timeout 90s).
        prev_step_to = self.step_timeout_s
        self.step_timeout_s = page_limit + 5.0
        self._enter_stage("otp-page")

        try:
            while time.time() < page_deadline:
                self._check_stage_stuck(index, detail="wait OTP page")
                self._raise_if_hard_block(page, "waiting for OTP page")
                url = (page.url or "").lower()
                elapsed = time.monotonic() - otp_wait_t0

                if elapsed - last_board_t >= 1.5:
                    plog.board_status(
                        index,
                        f"wait OTP page… {elapsed:.0f}/{page_limit:.0f}s",
                    )
                    last_board_t = elapsed

                otp_field = self._find_otp_input(page, timeout_ms=1200)
                if otp_field is not None:
                    break

                # Stuck on /create-account/password? Click passwordless OTP path.
                now = time.time()
                if now - last_passwordless_try >= 1.5 and (
                    self._is_create_password_page(page)
                    or "create-account/password" in url
                ):
                    last_passwordless_try = now
                    plog.board_status(
                        index,
                        f"still on /password · retry one-time code… "
                        f"{elapsed:.0f}/{page_limit:.0f}s",
                    )
                    if self._try_passwordless_otp_signup(page, index):
                        time.sleep(1.0)
                        continue
                    # Mid OTP-wait fallback: fill the real account password once.
                    if (
                        elapsed > 10
                        and not password_fallback_used
                        and password
                        and self._fill_password(page, password, timeout_ms=1500)
                    ):
                        password_fallback_used = True
                        plog.board_status(
                            index,
                            f"OTP-wait fallback · fill password… {elapsed:.0f}s",
                        )
                        self._click_continue(
                            page,
                            also=("Continue", "Create account", "Sign up", "继续"),
                        )
                        time.sleep(1.0)
                        continue

                # On verification page but no input yet — try Resend once.
                if (
                    not resend_tried
                    and elapsed > 12
                    and (
                        self._is_otp_page(page)
                        or "email-verification" in url
                    )
                ):
                    resend_tried = True
                    plog.board_status(index, f"OTP page · resend… {elapsed:.0f}s")
                    _click_first(
                        page,
                        [
                            'button:has-text("Resend")',
                            'button:has-text("resend")',
                            'button:has-text("重新发送")',
                            'a:has-text("Resend")',
                            'button:has-text("Send code")',
                        ],
                        timeout_ms=2000,
                    )
                    time.sleep(0.8)

                # Opportunistic passwordless even if URL lags.
                if (
                    elapsed > 8
                    and time.time() - last_passwordless_try >= 4.0
                    and not self._is_otp_page(page)
                ):
                    last_passwordless_try = time.time()
                    self._try_passwordless_otp_signup(page, index)

                # Truly past OTP → about-you (only if NOT also showing OTP UI).
                if not self._is_otp_page(page) and _first_visible(
                    page,
                    [
                        'input[name="name"]',
                        'input[name="age"]',
                        'input[placeholder*="全名" i]',
                        'input[placeholder*="年龄" i]',
                    ],
                    timeout_ms=500,
                ):
                    log.info(
                        f"[Task {index}] OTP skipped (about-you already visible)"
                    )
                    return

                # Real chatgpt session — skip OTP.
                if (
                    "chatgpt.com" in url
                    and "auth" not in url
                    and self._cookie_has_session(page)
                    and not self._is_otp_page(page)
                ):
                    log.info(
                        f"[Task {index}] Already on chatgpt.com with session — skip OTP"
                    )
                    return

                if self._is_otp_page(page) or "email-verification" in url:
                    log.info(
                        f"[Task {index}] OTP page detected ({page.url}) "
                        f"— waiting for Code input ({elapsed:.0f}s)"
                    )
                    time.sleep(0.5)
                    continue
                time.sleep(0.6)
        finally:
            self.step_timeout_s = prev_step_to

        if otp_field is None:
            elapsed = time.monotonic() - otp_wait_t0
            plog.board_status(
                index,
                f"OTP page timeout {elapsed:.0f}s · re-register…",
            )
            raise BrowserRegistrationError(
                f"stuck on wait OTP page after {elapsed:.0f}s "
                f"(limit {page_limit:.0f}s). url={page.url}"
            )

        log.info(f"[Task {index}] On OTP page ({page.url}) — polling mailbox for code…")
        plog.board_status(index, "OTP page · polling mailbox…")

        def _otp_tick(elapsed: float, timeout: float) -> None:
            plog.board_status(
                index, f"OTP wait mail… {elapsed:.0f}/{timeout:.0f}s"
            )

        code = wait_for_code(self.mail_config, mailbox, on_tick=_otp_tick)
        if not code:
            plog.board_status(index, "OTP timeout · no code")
            raise BrowserRegistrationError(
                "Timed out waiting for verification code from mailbox"
            )
        code = str(code).strip()
        log.info(f"[Task {index}] Got verification code: {code}")
        plog.board_status(index, f"OTP got {code} → submit…")

        # Re-find field (DOM may have re-rendered while we polled mail).
        otp_field = self._find_otp_input(page, timeout_ms=3000) or otp_field
        try:
            otp_field.click(timeout=3000)
            otp_field.fill("")
            otp_field.fill(code)
        except Exception as e:
            if not _fill_first(
                page,
                [
                    'input[name="code"]',
                    'input[autocomplete="one-time-code"]',
                    'input[type="text"][maxlength="6"]',
                    'input[inputmode="numeric"]',
                ],
                code,
                timeout_ms=3000,
            ):
                raise BrowserRegistrationError(f"Failed to fill OTP: {e}") from e

        log.info(f"[Task {index}] OTP filled — clicking Continue (intent=validate)")
        plog.board_status(index, "OTP submitted…")
        # Prefer intent=validate; also plain Continue (English UI screenshot).
        if not self._click_intent(
            page, "validate", labels=("继续", "Continue", "验证", "Verify")
        ):
            # Avoid clicking "Continue with password" — more specific first.
            if not _click_first(
                page,
                [
                    'button[type="submit"][name="intent"][value="validate"]',
                    'button[type="submit"][data-dd-action-name="Continue"]',
                    'button[type="submit"]:has-text("Continue")',
                    'button[type="submit"]:has-text("继续")',
                    'button.primary:has-text("Continue")',
                    'button:has-text("Continue") >> nth=0',
                ],
                timeout_ms=5000,
            ):
                self._click_continue(
                    page,
                    also=("Continue", "Verify", "Submit", "Next", "继续", "验证"),
                )
        time.sleep(1.5)
        self._raise_if_hard_block(page, "after OTP")

        # Still on OTP page? Wrong code, slow validate, or missed click — retry once.
        if self._is_otp_page(page) and self._find_otp_input(page, timeout_ms=1000):
            log.warning(
                f"[Task {index}] Still on OTP page after submit — url={page.url}"
            )
            plog.board_status(index, "OTP still open · re-click Continue…")
            if not self._click_intent(
                page, "validate", labels=("继续", "Continue", "验证", "Verify")
            ):
                self._click_continue(
                    page,
                    also=("Continue", "Verify", "Submit", "Next", "继续", "验证"),
                )
            time.sleep(2.0)

    def _fill_about_you_birthday(self, page, bday: dict[str, str], index: int) -> bool:
        """Fill birthday when the about-you form asks for DOB instead of age.

        English OpenAI UI uses a single **Birthday** text field as MM/DD/YYYY.
        Never type digit-by-digit (masks break and can show e.g. 07/11/2026).
        Use whole-value ``fill`` + input events; validate year is adult past.
        """
        log = logging.getLogger(__name__)

        def _set_value(loc, preferred: list[str]) -> bool:
            for val in preferred:
                try:
                    loc.click(timeout=1500)
                    # Select-all clear then set full value (no humanize typing).
                    try:
                        loc.fill("", timeout=800)
                    except Exception:
                        pass
                    try:
                        loc.press("Control+a")
                        loc.press("Backspace")
                    except Exception:
                        pass
                    loc.fill(val, timeout=2000)
                    # Force React/controlled inputs to accept the value.
                    loc.evaluate(
                        """(el, v) => {
                            const proto = window.HTMLInputElement
                                ? window.HTMLInputElement.prototype : null;
                            const desc = proto
                                ? Object.getOwnPropertyDescriptor(proto, 'value')
                                : null;
                            if (desc && desc.set) desc.set.call(el, v);
                            else el.value = v;
                            el.dispatchEvent(new Event('input', {bubbles: true}));
                            el.dispatchEvent(new Event('change', {bubbles: true}));
                            el.dispatchEvent(new Event('blur', {bubbles: true}));
                        }""",
                        val,
                    )
                    time.sleep(0.15)
                    try:
                        got = (loc.input_value(timeout=800) or "").strip()
                    except Exception:
                        got = val
                    if _birthday_looks_valid(got) or _birthday_looks_valid(val):
                        # Reject obvious "today / this year" leftovers.
                        if "2026" in got and bday["year"] != "2026":
                            continue
                        if got[:4].isdigit() and int(got[:4]) >= 2010:
                            # ISO still wrong year
                            if not _birthday_looks_valid(got):
                                continue
                        log.info(
                            f"[Task {index}] birthday set ok val={val!r} got={got!r}"
                        )
                        return True
                    log.warning(
                        f"[Task {index}] birthday rejected after fill "
                        f"val={val!r} got={got!r}"
                    )
                except Exception as e:
                    log.debug(f"[Task {index}] birthday fill {val!r}: {e}")
                    continue
            return False

        # ── 1) Single date / birthday field ───────────────────────────
        date_selectors = [
            'input[name="birthday"]',
            'input[name="birthdate"]',
            'input[name="birth_date"]',
            'input[name="date_of_birth"]',
            'input[name="dateOfBirth"]',
            'input[id*="birthday" i]',
            'input[id*="birthdate" i]',
            'input[id*="birth" i]',
            'input[autocomplete="bday"]',
            'input[placeholder*="birth" i]',
            'input[placeholder*="Birthday" i]',
            'input[placeholder*="Date of birth" i]',
            'input[placeholder*="MM/DD" i]',
            'input[placeholder*="出生" i]',
            'input[placeholder*="生日" i]',
            'input[type="date"]',
        ]
        for sel in date_selectors:
            loc = _first_visible(page, [sel], timeout_ms=400)
            if loc is None:
                continue
            try:
                itype = (loc.get_attribute("type") or "").lower()
            except Exception:
                itype = ""
            # type=date must be ISO; text Birthday field is MM/DD/YYYY on en-US.
            if itype == "date":
                preferred = [bday["iso"]]
            else:
                preferred = [bday["us"], bday["iso"], bday["eu"]]
            if _set_value(loc, preferred):
                log.info(
                    f"[Task {index}] about-you birthday via {sel!r} → {bday['us']}"
                )
                return True

        # Label / role fallback (React Aria floating labels "Birthday").
        for pattern in (
            re.compile(r"^birthday$", re.I),
            re.compile(r"birthday|date of birth|birth date", re.I),
            re.compile(r"出生日期|生日|出生"),
        ):
            try:
                loc = page.get_by_label(pattern).first
                if loc.count() and loc.is_visible(timeout=600):
                    if _set_value(loc, [bday["us"], bday["iso"], bday["eu"]]):
                        log.info(
                            f"[Task {index}] about-you birthday via label → {bday['us']}"
                        )
                        return True
            except Exception:
                pass
            try:
                loc = page.get_by_placeholder(pattern).first
                if loc.count() and loc.is_visible(timeout=500):
                    if _set_value(loc, [bday["us"], bday["iso"]]):
                        return True
            except Exception:
                pass

        # ── 2) Split month / day / year ───────────────────────────────
        month_sels = [
            'select[name*="month" i]',
            'select[id*="month" i]',
            'input[name*="month" i]',
            'input[id*="month" i]',
            'input[placeholder*="month" i]',
            'input[placeholder*="月" i]',
            'select[aria-label*="month" i]',
        ]
        day_sels = [
            'select[name*="day" i]',
            'select[id*="day" i]',
            'input[name*="day" i]',
            'input[id*="day" i]',
            'input[placeholder*="day" i]',
            'input[placeholder*="日" i]',
            'select[aria-label*="day" i]',
        ]
        year_sels = [
            'select[name*="year" i]',
            'select[id*="year" i]',
            'input[name*="year" i]',
            'input[id*="year" i]',
            'input[placeholder*="year" i]',
            'input[placeholder*="年" i]',
            'select[aria-label*="year" i]',
        ]

        def _set_part(selectors: list[str], value: str, value_alt: str = "") -> bool:
            loc = _first_visible(page, selectors, timeout_ms=500)
            if loc is None:
                return False
            for v in (value, value_alt):
                if not v:
                    continue
                try:
                    tag = (loc.evaluate("el => el.tagName") or "").lower()
                except Exception:
                    tag = ""
                try:
                    if tag == "select":
                        try:
                            loc.select_option(value=v, timeout=2000)
                        except Exception:
                            loc.select_option(label=v, timeout=2000)
                    else:
                        loc.click(timeout=1000)
                        loc.fill(v, timeout=2000)
                    return True
                except Exception:
                    continue
            return False

        m_ok = _set_part(month_sels, bday["month_2"], bday["month"])
        d_ok = _set_part(day_sels, bday["day_2"], bday["day"])
        y_ok = _set_part(year_sels, bday["year"])
        if m_ok and d_ok and y_ok:
            log.info(
                f"[Task {index}] about-you birthday parts "
                f"→ {bday['iso']} (m/d/y all set)"
            )
            return True
        if y_ok and (m_ok or d_ok):
            log.warning(
                f"[Task {index}] birthday parts partial m={m_ok} d={d_ok} y={y_ok}"
            )
            return bool(y_ok and m_ok and d_ok)

        return False

    def _about_you_has_bad_info_error(self, page) -> bool:
        text = _page_text(page).lower()
        needles = (
            "can't create an account with that info",
            "cannot create an account with that info",
            "we can't create an account",
            "try again",
            "invalid birthday",
            "invalid date",
            "must be at least 18",
            "unable to create account",
            "无法创建",
            "信息有误",
        )
        # "try again" alone is weak; require combo with account/birthday context.
        if "can't create an account with that info" in text:
            return True
        if "cannot create an account with that info" in text:
            return True
        if "must be at least 18" in text or "invalid birthday" in text:
            return True
        if "we can't create an account" in text:
            return True
        return False

    def _handle_about_you(
        self,
        page,
        *,
        full_name: str,
        age: str,
        index: int,
    ) -> None:
        """Fill name + age **or birthday** on /about-you, then wait for chatgpt.com.

        Most locales show an **age** number field; some show **birthday / DOB**.
        We detect and fill whichever is present.
        """
        log = logging.getLogger(__name__)
        deadline = time.time() + 45
        bday = _age_to_birthday(age)

        name_selectors = [
            'input[name="name"]',
            'input[name="fullName"]',
            'input[autocomplete="name"]',
            'input[placeholder*="name" i]',
            'input[placeholder*="全名" i]',
            'input[placeholder*="姓名" i]',
            'input[id*="name" i]',
        ]
        # Age-only (exclude birthday fields that also match *age* poorly).
        age_selectors = [
            'input[name="age"]',
            'input[id="age"]',
            'input[id*="age" i]:not([id*="birth" i])',
            'input[placeholder*="age" i]:not([placeholder*="birth" i])',
            'input[placeholder*="年龄" i]',
            'input[placeholder*="年齡" i]',
            'input[name="age"][type="number"]',
            'input[type="number"][name="age"]',
        ]
        birthday_probe = [
            'input[type="date"]',
            'input[name="birthday"]',
            'input[name="birthdate"]',
            'input[name="birth_date"]',
            'input[name="date_of_birth"]',
            'input[autocomplete="bday"]',
            'input[placeholder*="birth" i]',
            'input[placeholder*="出生" i]',
            'input[placeholder*="生日" i]',
            'select[name*="year" i]',
            'select[id*="year" i]',
        ]

        # Wait until the form is present (or we already left signup for real).
        form_ready = False
        while time.time() < deadline:
            self._check_stage_stuck(index, detail="about-you wait form")
            self._raise_if_hard_block(page, "about-you")
            url = (page.url or "").lower()
            if "choose-an-account" in url or _url_is_chatgpt_app(url):
                log.info(f"[Task {index}] Skipped about-you (already at {page.url})")
                return
            if _first_visible(
                page,
                name_selectors + age_selectors + birthday_probe,
                timeout_ms=500,
            ) is not None:
                form_ready = True
                break
            time.sleep(0.3)

        if not form_ready:
            # login_with mid-hop is NOT success — keep waiting a bit more.
            url = (page.url or "").lower()
            if _url_is_auth_intermediate(url) and "about-you" not in url:
                log.warning(
                    f"[Task {index}] about-you not shown yet (url={page.url}) — continuing to wait"
                )
            raise BrowserRegistrationError(
                f"about-you form not found (name/age/birthday). url={page.url}"
            )

        name_ok = _fill_first(page, name_selectors, full_name, timeout_ms=2000)

        # Prefer age when that field is visible; otherwise birthday/DOB.
        has_age = (
            _first_visible(page, age_selectors, timeout_ms=600) is not None
        )
        has_bday = (
            _first_visible(page, birthday_probe, timeout_ms=600) is not None
        )
        # English UI label "Birthday" counts as birthday even without name=.
        try:
            if page.get_by_label(re.compile(r"^birthday$", re.I)).count():
                has_bday = True
        except Exception:
            pass

        def _fill_second_field(bday_dict: dict[str, str]) -> tuple[str, bool, bool]:
            """Return (mode, age_ok, bday_ok)."""
            a_ok = b_ok = False
            md = "none"
            if has_bday and not has_age:
                b_ok = self._fill_about_you_birthday(page, bday_dict, index)
                md = "birthday" if b_ok else "none"
            elif has_age and not has_bday:
                a_ok = _fill_first(page, age_selectors, str(age), timeout_ms=2000)
                md = "age" if a_ok else "none"
            else:
                # Birthday label UI is common on en-US — try birthday first when
                # probe saw birth-ish fields, else age.
                if has_bday:
                    b_ok = self._fill_about_you_birthday(page, bday_dict, index)
                    if b_ok:
                        return "birthday", False, True
                a_ok = _fill_first(
                    page,
                    [
                        'input[name="age"]',
                        'input[placeholder*="age" i]:not([placeholder*="birth" i])',
                        'input[placeholder*="年龄" i]',
                    ],
                    str(age),
                    timeout_ms=1200,
                )
                if a_ok:
                    md = "age"
                else:
                    b_ok = self._fill_about_you_birthday(page, bday_dict, index)
                    md = "birthday" if b_ok else "none"
            return md, a_ok, b_ok

        mode, age_ok, bday_ok = _fill_second_field(bday)

        plog.board_status(
            index,
            f"about-you name={full_name} "
            + (f"age={age}" if mode == "age" else f"bday={bday['us']}"),
        )
        log.info(
            f"[Task {index}] about-you filled name={name_ok} mode={mode} "
            f"age_ok={age_ok} bday_ok={bday_ok} age={age} bday={bday['us']}"
        )
        if not name_ok and not age_ok and not bday_ok:
            raise BrowserRegistrationError(
                f"Could not fill about-you fields (name/age/birthday). url={page.url}"
            )

        def _click_finish() -> None:
            if not self._click_finish_account(page):
                self._click_continue(
                    page,
                    also=(
                        "完成帐户创建",
                        "完成账户创建",
                        "Finish creating account",
                        "Continue",
                        "Done",
                        "Submit",
                        "继续",
                        "完成",
                    ),
                )

        _click_finish()
        time.sleep(1.0)

        # Bad birthday (e.g. 07/11/2026 future) → red error; re-fill adult DOB once.
        if self._about_you_has_bad_info_error(page) or (
            mode == "birthday" and self._about_you_has_bad_info_error(page)
        ):
            log.warning(
                f"[Task {index}] about-you rejected info — retry birthday/age"
            )
            plog.board_status(index, "about-you bad info · retry adult bday…")
            # Force a clearly adult birthday (25–35).
            age2 = str(random.randint(25, 35))
            bday2 = _age_to_birthday(age2)
            mode, age_ok, bday_ok = _fill_second_field(bday2)
            bday = bday2
            plog.board_status(
                index,
                f"about-you retry bday={bday['us']}",
            )
            log.info(
                f"[Task {index}] about-you retry mode={mode} bday={bday['us']}"
            )
            _click_finish()
            time.sleep(1.2)
            if self._about_you_has_bad_info_error(page):
                raise BrowserRegistrationError(
                    "about-you rejected after birthday retry "
                    f"({bday.get('us')}). url={page.url}"
                )

        # After 「完成帐户创建」: wait until URL is on chatgpt.com
        # (login_with / callback / app shell all count), then next step.
        # Do not use a fixed sleep — poll until domain switches.
        max_wait = float(getattr(self, "about_you_settle_s", 90.0) or 90.0)
        # Compat: old config used 5 as fixed sleep; treat small values as too short.
        if max_wait < 30:
            max_wait = 90.0
        max_wait = max(30.0, min(max_wait, 180.0))
        self._enter_stage("wait-chatgpt.com")
        plog.board_status(index, "about-you clicked · wait chatgpt.com…")
        log.info(
            f"[Task {index}] about-you submitted — wait until chatgpt.com "
            f"(max {max_wait:.0f}s)"
        )

        leave_deadline = time.time() + max_wait
        last_status = -1.0
        last_resubmit = 0.0
        t0 = time.monotonic()
        while time.time() < leave_deadline:
            try:
                url = (page.url or "").lower()
            except Exception:
                url = ""
            elapsed = time.monotonic() - t0

            # Success: landed on chatgpt.com (any path under that host).
            if "chatgpt.com" in url:
                log.info(
                    f"[Task {index}] about-you → chatgpt.com "
                    f"({elapsed:.1f}s) url={page.url}"
                )
                plog.board_status(
                    index, f"chatgpt.com ready · {elapsed:.0f}s"
                )
                # Tiny paint settle before session harvest step.
                time.sleep(0.4)
                return

            # Session cookie without chatgpt URL yet — still OK to proceed.
            if self._cookie_has_session(page):
                log.info(
                    f"[Task {index}] session cookie before chatgpt.com "
                    f"(url={page.url}) — continue"
                )
                plog.board_status(index, "session cookie ok · continue…")
                return

            if _looks_like_cf_challenge(page) or "__cf_chl" in url:
                # OAuth callback + CF checkbox (common after about-you).
                self._enter_stage("oauth-callback")
                if elapsed - last_status >= 2.0:
                    plog.board_status(
                        index, f"CF on callback · verify human… {elapsed:.0f}s"
                    )
                    last_status = elapsed
                # Longer soft wait + checkbox clicks; do not kill auth code yet.
                self._wait_out_cloudflare(
                    page, index, max_wait=min(55.0, max_wait - elapsed), soft=True
                )
                if "chatgpt.com" in (page.url or "").lower() and not _looks_like_cf_challenge(
                    page
                ):
                    return
                if self._cookie_has_session(page):
                    return
                time.sleep(0.4)
                continue

            if elapsed - last_status >= 2.0:
                host = ""
                try:
                    host = (page.url or "").split("/")[2] if "://" in (page.url or "") else "?"
                except Exception:
                    host = "?"
                plog.board_status(
                    index,
                    f"wait chatgpt.com… {elapsed:.0f}s ({host})",
                )
                last_status = elapsed

            # Still on about-you form: re-click finish occasionally.
            if "about-you" in url or _first_visible(
                page, ['input[name="name"]', 'input[name="age"]'], timeout_ms=300
            ):
                self._check_stage_stuck(index, detail="about-you still on form")
                if elapsed - last_resubmit >= 10.0:
                    last_resubmit = elapsed
                    plog.board_status(
                        index, f"about-you re-submit… {elapsed:.0f}s"
                    )
                    self._click_finish_account(page) or self._click_continue(
                        page,
                        also=("完成帐户创建", "完成账户创建", "Continue", "Done", "完成"),
                    )
                _click_first(
                    page,
                    [
                        "button:has-text('Accept')",
                        "button:has-text('I agree')",
                        "button:has-text('Agree')",
                        "button:has-text('Skip')",
                        "button:has-text('同意')",
                        "button:has-text('跳过')",
                    ],
                    timeout_ms=300,
                )
            else:
                # Mid-auth hop (auth.openai.com etc.) — keep waiting.
                self._check_stage_stuck(index, detail="wait chatgpt.com")

            time.sleep(0.4)

        try:
            url = (page.url or "").lower()
        except Exception:
            url = ""
        if "chatgpt.com" in url or self._cookie_has_session(page):
            log.warning(
                f"[Task {index}] about-you wait timeout but ok "
                f"({page.url}) — continue"
            )
            return

        raise BrowserRegistrationError(
            f"stuck after about-you — never reached chatgpt.com "
            f"(waited {max_wait:.0f}s). url={page.url}"
        )

    def _handle_choose_account(self, page, index: int) -> None:
        """If stuck on /choose-an-account, pick the newest / first workspace card."""
        log = logging.getLogger(__name__)
        url = (page.url or "").lower()
        if "choose-an-account" not in url and "choose_account" not in url:
            return
        log.info(f"[Task {index}] On choose-an-account — selecting an account")
        # Prefer explicit account rows / buttons.
        clicked = _click_first(
            page,
            [
                'button[data-dd-action-name*="account" i]',
                'a[href*="chatgpt.com"]',
                'button:has-text("@")',
                '[role="button"]:has-text("@")',
                'button:has-text("Continue")',
                'button:has-text("继续")',
                # first clickable card often works
                'main button >> nth=0',
                'main [role="button"] >> nth=0',
            ],
            timeout_ms=4000,
        )
        if not clicked:
            log.warning(f"[Task {index}] choose-an-account: no clickable account found")
        time.sleep(1.2)

    def _wait_for_logged_in(self, page, index: int) -> None:
        """Wait until OAuth finishes and NextAuth session cookie is usable.

        Do **not** harvest while still on ``/api/auth/callback`` or
        ``login_with`` — cookie may be missing or CF will 403 pure HTTP.
        Camoufox: cookie alone is enough (avoid app shell / WebSockets).
        Chrome: prefer cookie + left mid-auth (app shell optional).
        """
        log = logging.getLogger(__name__)
        deadline = time.time() + max(90, self.timeout_ms / 1000)
        nudged = False
        t0 = time.monotonic()
        last_status_t = -1.0
        camoufox = self.engine == "camoufox"

        while time.time() < deadline:
            self._check_stage_stuck(index, detail="wait session cookie")
            elapsed = time.monotonic() - t0
            if elapsed - last_status_t >= 2.0:
                plog.board_status(index, f"wait session cookie… {elapsed:.0f}s")
                last_status_t = elapsed
            try:
                self._raise_if_hard_block(page, "waiting for login")
            except BrowserRegistrationError:
                raise
            except Exception:
                pass

            try:
                url = (page.url or "").lower()
            except Exception as e:
                log.warning(f"[Task {index}] page.url failed (driver?): {e}")
                if self._cookie_has_session(page):
                    log.info(
                        f"[Task {index}] Driver flaky but session cookie present — continue"
                    )
                    plog.board_status(index, "session cookie ok (driver flaky)")
                    return
                time.sleep(0.4)
                continue

            if "choose-an-account" in url:
                plog.board_status(index, "choose-an-account · pick…")
                self._handle_choose_account(page, index)
                time.sleep(0.5)
                continue

            # Still exchanging OAuth code — wait, do not harvest yet.
            if _url_is_oauth_callback(url) or _looks_like_cf_challenge(page):
                if _looks_like_cf_challenge(page):
                    plog.board_status(
                        index, f"CF Verify human on callback… {elapsed:.0f}s"
                    )
                    self._wait_out_cloudflare(
                        page, index, max_wait=20.0, soft=True
                    )
                else:
                    plog.board_status(index, f"oauth callback… {elapsed:.0f}s")
                time.sleep(0.5)
                continue

            has_cookie = self._cookie_has_session(page)
            mid_auth = _url_is_auth_intermediate(url) and not _url_is_chatgpt_app(url)

            # Ready when we have a real session cookie and are not mid OAuth.
            if has_cookie and not mid_auth:
                log.info(
                    f"[Task {index}] Session cookie ready "
                    f"(engine={self.engine}, url={page.url})"
                )
                plog.board_status(index, "session cookie ok · harvest…")
                # Brief settle so Set-Cookie / CF cookies stick before harvest.
                time.sleep(0.35)
                return

            # Chrome app shell + cookie (strong signal).
            if not camoufox and _url_is_chatgpt_app(url) and has_cookie:
                log.info(f"[Task {index}] Session cookie + app shell ({page.url})")
                time.sleep(0.3)
                return

            # Cookie present but still bouncing login_with: wait a bit longer
            # for redirect; only force-exit if cookie has been stable long enough.
            if has_cookie and mid_auth and elapsed > 12:
                log.info(
                    f"[Task {index}] Cookie present mid-auth after {elapsed:.0f}s "
                    f"— proceed to harvest ({page.url})"
                )
                return

            # Nudge only when cookie missing and not mid OAuth callback.
            if (
                not has_cookie
                and not nudged
                and not _url_is_oauth_callback(url)
                and elapsed > (8.0 if camoufox else 5.0)
            ):
                # Prefer home over /api/auth/session for Chrome — session URL
                # alone often returns CF HTML when cookies aren't ready.
                target = "https://chatgpt.com/"
                log.info(f"[Task {index}] Nudge → {target}")
                try:
                    page.goto(
                        target,
                        wait_until="domcontentloaded",
                        timeout=20_000,
                    )
                except Exception as e:
                    log.warning(f"[Task {index}] nudge navigate: {e}")
                    if self._cookie_has_session(page):
                        return
                nudged = True

            try:
                _click_first(
                    page,
                    [
                        "button:has-text('Okay')",
                        "button:has-text('Got it')",
                        "button:has-text('Next')",
                        "button:has-text('Skip')",
                        "button:has-text('Start chatting')",
                        "button:has-text('好的')",
                        "button:has-text('开始')",
                    ],
                    timeout_ms=300,
                )
            except Exception:
                pass
            time.sleep(0.4)

        if self._cookie_has_session(page):
            log.warning(
                f"[Task {index}] Timeout but session cookie present — continue harvest"
            )
            return

        if not camoufox:
            try:
                page.goto(
                    "https://chatgpt.com/",
                    wait_until="domcontentloaded",
                    timeout=15_000,
                )
                time.sleep(0.5)
                if self._cookie_has_session(page):
                    return
            except Exception:
                pass

        raise BrowserRegistrationError(
            "Timed out waiting for session cookie after signup UI"
        )

    def _cookie_has_session(self, page) -> bool:
        """True only for a real ChatGPT NextAuth session cookie on chatgpt.com.

        Do **not** treat intermediate auth.openai.com cookies (e.g.
        ``oai-client-auth-session`` from login_with warm-up) as logged-in —
        that incorrectly skipped the email OTP step.
        """
        try:
            cookies = page.context.cookies()
        except Exception:
            return False
        # Prefer chatgpt.com host cookies only.
        chatgpt_cookies = [
            c
            for c in (cookies or [])
            if isinstance(c, dict)
            and "chatgpt.com" in str(c.get("domain") or "")
        ]
        token = extract_session_token_from_cookies(chatgpt_cookies)
        if token:
            return True
        # Fallback: full jar but only next-auth names (ignore oai-client-*).
        jar = {}
        for c in cookies or []:
            if not isinstance(c, dict):
                continue
            name = str(c.get("name") or "")
            if name in (
                "__Secure-next-auth.session-token",
                "next-auth.session-token",
            ) or name.startswith("__Secure-next-auth.session-token."):
                jar[name] = str(c.get("value") or "")
        return bool(extract_session_token_from_cookies(jar))

    def _yescaptcha_enabled(self) -> bool:
        yc = self.yescaptcha or {}
        return bool(yc.get("enabled")) and bool(
            str(yc.get("client_key") or yc.get("api_key") or "").strip()
        )

    def _extract_turnstile_sitekey(self, page) -> str:
        """Best-effort Turnstile sitekey from DOM / iframes."""
        # 1) data-sitekey attributes
        try:
            keys = page.eval_on_selector_all(
                "[data-sitekey]",
                "els => els.map(e => e.getAttribute('data-sitekey')).filter(Boolean)",
            )
            for k in keys or []:
                k = str(k or "").strip()
                if k.startswith("0x") or len(k) >= 20:
                    return k
        except Exception:
            pass
        # 2) iframe src query params
        try:
            srcs = page.eval_on_selector_all(
                "iframe[src*='turnstile'], iframe[src*='challenges.cloudflare']",
                "els => els.map(e => e.src || '')",
            )
            for src in srcs or []:
                m = re.search(r"[?&](?:sitekey|k)=([^&]+)", str(src), re.I)
                if m:
                    return unquote(m.group(1)).strip()
        except Exception:
            pass
        # 3) page HTML regex
        try:
            html = page.content() or ""
        except Exception:
            html = ""
        for pat in (
            r'data-sitekey=["\']([^"\']+)["\']',
            r'sitekey["\']?\s*[:=]\s*["\'](0x[0-9A-Za-z_-]+)["\']',
            r'turnstile\.render\([^)]*["\'](0x[0-9A-Za-z_-]+)["\']',
        ):
            m = re.search(pat, html, re.I)
            if m:
                return m.group(1).strip()
        return ""

    def _inject_turnstile_token(self, page, token: str) -> bool:
        """Put YesCaptcha Turnstile token into the page and fire callbacks."""
        token = str(token or "").strip()
        if not token:
            return False
        try:
            ok = page.evaluate(
                """(tok) => {
                    let filled = false;
                    const names = [
                        'cf-turnstile-response',
                        'g-recaptcha-response',
                        'h-captcha-response',
                    ];
                    for (const n of names) {
                        document.querySelectorAll(
                            `textarea[name="${n}"], input[name="${n}"]`
                        ).forEach((el) => {
                            el.value = tok;
                            el.innerHTML = tok;
                            filled = true;
                        });
                    }
                    document.querySelectorAll('[name*="turnstile" i]').forEach((el) => {
                        try { el.value = tok; filled = true; } catch (e) {}
                    });
                    // Common callback hooks
                    const cbs = [
                        window.tsCallback,
                        window.turnstileCallback,
                        window.cfCallback,
                        window.onTurnstileSuccess,
                    ];
                    for (const cb of cbs) {
                        if (typeof cb === 'function') {
                            try { cb(tok); filled = true; } catch (e) {}
                        }
                    }
                    try {
                        if (window.turnstile && typeof window.turnstile.getResponse === 'function') {
                            // cannot override easily; dispatch event
                        }
                    } catch (e) {}
                    try {
                        document.dispatchEvent(new CustomEvent('turnstile-callback', {detail: tok}));
                    } catch (e) {}
                    return filled;
                }""",
                token,
            )
            return bool(ok)
        except Exception as e:
            logging.getLogger(__name__).warning(
                "inject turnstile token failed: %s", e
            )
            return False

    def _turnstile_local_enabled(self) -> bool:
        tl = self.turnstile_local or {}
        # default ON when section exists with enabled!=false; explicit false disables
        if "enabled" in tl:
            return bool(tl.get("enabled"))
        # auto-enable if user set any local-specific knobs
        return bool(tl)

    def _try_turnstile_local(self, page, index: int) -> bool:
        """Solve Turnstile with local Playwright (no external queue)."""
        if not self._turnstile_local_enabled():
            return False
        log = logging.getLogger(__name__)
        tl = self.turnstile_local or {}
        timeout = float(tl.get("timeout") or tl.get("max_wait") or 90)
        headless = bool(tl.get("headless", True))
        channel = str(tl.get("channel") or self.browser_channel or "chrome").strip()
        # Prefer same proxy as registration (clearance/IP bound).
        proxy = str(tl.get("proxy") or self.proxy or "").strip()
        sitekey = str(tl.get("sitekey") or "").strip()
        if not sitekey:
            sitekey = self._extract_turnstile_sitekey(page)
        try:
            url = page.url or ""
        except Exception:
            url = "https://chatgpt.com/"

        plog.board_status(index, "local Turnstile · solve…")
        log.info(
            f"[Task {index}] local Turnstile url={url[:70]} "
            f"key={sitekey[:14] or '?'} headless={headless}"
        )

        def _st(msg: str) -> None:
            try:
                plog.board_status(index, msg)
            except Exception:
                pass

        try:
            from utils.turnstile_local import solve_turnstile_local

            token = solve_turnstile_local(
                website_url=url,
                website_key=sitekey,
                proxy=proxy,
                headless=headless,
                timeout=timeout,
                channel=channel,
                page=page,
                on_status=_st,
            )
        except Exception as e:
            log.warning(f"[Task {index}] local Turnstile failed: {e}")
            plog.board_status(index, f"local TS fail · {str(e)[:36]}")
            return False

        if self._inject_turnstile_token(page, token):
            plog.board_status(index, "local TS token injected")
            log.info(f"[Task {index}] local Turnstile injected len={len(token)}")
            time.sleep(1.2)
            try:
                _click_first(
                    page,
                    [
                        'button:has-text("Verify")',
                        'button:has-text("Continue")',
                        'button[type="submit"]',
                    ],
                    timeout_ms=1500,
                )
            except Exception:
                pass
            time.sleep(1.5)
            return True
        try:
            page.evaluate(
                """(tok) => {
                    let ta = document.querySelector('[name="cf-turnstile-response"]');
                    if (!ta) {
                        ta = document.createElement('input');
                        ta.type = 'hidden';
                        ta.name = 'cf-turnstile-response';
                        document.body.appendChild(ta);
                    }
                    ta.value = tok;
                }""",
                token,
            )
            plog.board_status(index, "local TS token forced into DOM")
            time.sleep(1.5)
            return True
        except Exception as e:
            log.warning(f"[Task {index}] local TS inject: {e}")
            return False

    def _try_yescaptcha_turnstile(self, page, index: int) -> bool:
        """Solve Turnstile via YesCaptcha and inject token. Returns True if applied."""
        if not self._yescaptcha_enabled():
            return False
        log = logging.getLogger(__name__)
        yc = self.yescaptcha
        client_key = str(yc.get("client_key") or yc.get("api_key") or "").strip()
        base_url = str(
            yc.get("base_url") or "https://api.yescaptcha.com"
        ).strip()
        max_wait = float(yc.get("max_wait") or 120)
        task_type = str(
            yc.get("task_type") or "TurnstileTaskProxyless"
        ).strip() or "TurnstileTaskProxyless"

        try:
            url = page.url or ""
        except Exception:
            url = "https://chatgpt.com/"

        sitekey = str(yc.get("sitekey") or "").strip()
        if not sitekey:
            sitekey = self._extract_turnstile_sitekey(page)
        if not sitekey:
            log.warning(
                f"[Task {index}] YesCaptcha: no Turnstile sitekey on page"
            )
            plog.board_status(index, "YesCaptcha · no sitekey")
            return False

        plog.board_status(index, f"YesCaptcha Turnstile… key={sitekey[:10]}…")
        log.info(
            f"[Task {index}] YesCaptcha Turnstile sitekey={sitekey} url={url[:80]}"
        )
        try:
            from utils.yescaptcha import solve_turnstile

            token = solve_turnstile(
                client_key,
                url,
                sitekey,
                base_url=base_url,
                max_wait=max_wait,
                task_type=task_type,
                proxy=self.proxy,
            )
        except Exception as e:
            log.warning(f"[Task {index}] YesCaptcha solve failed: {e}")
            plog.board_status(index, f"YesCaptcha fail · {str(e)[:40]}")
            return False

        if self._inject_turnstile_token(page, token):
            plog.board_status(index, "YesCaptcha token injected")
            log.info(f"[Task {index}] YesCaptcha token injected (len={len(token)})")
            time.sleep(1.2)
            # Nudge: press Enter / click verify-ish buttons
            try:
                _click_first(
                    page,
                    [
                        'button:has-text("Verify")',
                        'button:has-text("Continue")',
                        'button[type="submit"]',
                    ],
                    timeout_ms=1500,
                )
            except Exception:
                pass
            time.sleep(1.5)
            return True

        # Fallback: set cookie-less callback via evaluate again with force fields
        try:
            page.evaluate(
                """(tok) => {
                    let ta = document.querySelector('[name="cf-turnstile-response"]');
                    if (!ta) {
                        ta = document.createElement('input');
                        ta.type = 'hidden';
                        ta.name = 'cf-turnstile-response';
                        document.body.appendChild(ta);
                    }
                    ta.value = tok;
                }""",
                token,
            )
            plog.board_status(index, "YesCaptcha token forced into DOM")
            time.sleep(1.5)
            return True
        except Exception as e:
            log.warning(f"[Task {index}] YesCaptcha inject fallback: {e}")
            return False

    def _try_solve_cf_checkbox(self, page, index: int) -> bool:
        """Best-effort click Cloudflare 「Verify you are human」 / Turnstile.

        Order: local Playwright solver → YesCaptcha → human-like checkbox click.
        """
        log = logging.getLogger(__name__)
        # 1) Local (本机真浏览器，不连外部队列)
        if self._turnstile_local_enabled():
            try:
                if self._try_turnstile_local(page, index):
                    return True
            except Exception as e:
                log.warning(f"[Task {index}] local Turnstile path error: {e}")
        # 2) YesCaptcha
        if self._yescaptcha_enabled():
            try:
                if self._try_yescaptcha_turnstile(page, index):
                    return True
            except Exception as e:
                log.warning(f"[Task {index}] YesCaptcha path error: {e}")
        clicked = False

        # 1) Main page label / checkbox
        try:
            if _click_first(
                page,
                [
                    "text=Verify you are human",
                    "label:has-text('Verify you are human')",
                    "text=确认您是真人",
                    "input[type='checkbox']",
                ],
                timeout_ms=1200,
            ):
                clicked = True
        except Exception:
            pass

        # 2) Frames (challenge widget lives in iframe)
        try:
            frames = list(page.frames)
        except Exception:
            frames = []
        for frame in frames:
            try:
                furl = (frame.url or "").lower()
            except Exception:
                furl = ""
            if (
                "cloudflare" not in furl
                and "turnstile" not in furl
                and "challenge" not in furl
                and furl
                and furl != (page.url or "").lower()
            ):
                # Still try unmarked iframes that contain the label.
                pass
            try:
                # Checkbox / body click in challenge iframe
                for sel in (
                    "input[type='checkbox']",
                    "label",
                    ".ctp-checkbox-label",
                    "#challenge-stage",
                    "body",
                ):
                    try:
                        loc = frame.locator(sel).first
                        if loc.count() == 0:
                            continue
                        if not loc.is_visible(timeout=400):
                            continue
                        box = loc.bounding_box(timeout=800)
                        if box:
                            # Frame coords are relative to frame; use locator click
                            # with human path via element handle center.
                            try:
                                loc.scroll_into_view_if_needed(timeout=500)
                            except Exception:
                                pass
                            _human_pause(0.15, 0.45)
                            loc.click(timeout=2000, force=False)
                            clicked = True
                            log.info(
                                f"[Task {index}] CF iframe click {sel!r} "
                                f"frame={furl[:60]}"
                            )
                            break
                    except Exception:
                        continue
                if clicked:
                    break
                # Text match inside frame
                try:
                    loc = frame.get_by_text(
                        re.compile(r"verify you are human|确认您是真人", re.I)
                    ).first
                    if loc.count() and loc.is_visible(timeout=500):
                        loc.click(timeout=2000)
                        clicked = True
                        break
                except Exception:
                    pass
            except Exception:
                continue

        # 3) Generic: click center of first cf iframe on page
        if not clicked:
            try:
                iframe = page.locator(
                    "iframe[src*='challenges.cloudflare.com'], "
                    "iframe[src*='turnstile'], iframe[src*='cloudflare']"
                ).first
                if iframe.count() and iframe.is_visible(timeout=800):
                    box = iframe.bounding_box(timeout=1000)
                    if box:
                        cx = box["x"] + box["width"] * random.uniform(0.2, 0.45)
                        cy = box["y"] + box["height"] * random.uniform(0.35, 0.65)
                        _human_move_to(page, cx, cy)
                        _human_pause(0.1, 0.3)
                        page.mouse.click(cx, cy)
                        clicked = True
                        log.info(f"[Task {index}] CF iframe bbox click")
            except Exception:
                pass

        if clicked:
            _human_pause(0.8, 1.8)
        return clicked

    def _wait_out_cloudflare(
        self,
        page,
        index: int,
        max_wait: float = 45.0,
        *,
        soft: bool = False,
    ) -> bool:
        """If a CF interstitial is showing, try click + wait for clear.

        Returns True if challenge cleared (or none). False if still present
        after ``max_wait`` when ``soft=True``; raises when ``soft=False``.

        OAuth callback checkbox (about-you → chatgpt.com/api/auth/callback)
        needs headed browser + clean proxy; we attempt human-like checkbox clicks.
        """
        log = logging.getLogger(__name__)
        if not _looks_like_cf_challenge(page):
            return True
        log.warning(
            f"[Task {index}] Cloudflare challenge detected — waiting up to "
            f"{max_wait:.0f}s (headless=false + residential proxy helps)"
        )
        t0 = time.monotonic()
        plog.board_status(index, f"CF Verify human… 0/{max_wait:.0f}s")
        deadline = time.time() + max_wait
        last_click = 0.0
        local_tried = False
        yescaptcha_tried = False
        while time.time() < deadline:
            elapsed = time.monotonic() - t0
            # 1) Local Turnstile once early
            if (
                self._turnstile_local_enabled()
                and not local_tried
                and elapsed >= 0.5
            ):
                local_tried = True
                try:
                    if self._try_turnstile_local(page, index):
                        plog.board_status(index, "local TS applied · wait clear…")
                        time.sleep(2.0)
                        if not _looks_like_cf_challenge(page):
                            return True
                except Exception as e:
                    log.warning(f"[Task {index}] local TS: {e}")
            # 2) YesCaptcha once
            if (
                self._yescaptcha_enabled()
                and not yescaptcha_tried
                and elapsed >= 1.0
            ):
                yescaptcha_tried = True
                try:
                    if self._try_yescaptcha_turnstile(page, index):
                        plog.board_status(index, "YesCaptcha applied · wait clear…")
                        time.sleep(2.0)
                        if not _looks_like_cf_challenge(page):
                            return True
                except Exception as e:
                    log.warning(f"[Task {index}] YesCaptcha: {e}")
            # Periodic human-like checkbox / turnstile click.
            if elapsed - last_click >= 3.0:
                try:
                    if self._try_solve_cf_checkbox(page, index):
                        plog.board_status(
                            index,
                            f"CF clicked checkbox… {elapsed:.0f}s",
                        )
                except Exception as e:
                    log.debug(f"[Task {index}] CF click attempt: {e}")
                last_click = elapsed

            time.sleep(1.2)
            elapsed = time.monotonic() - t0
            if not _looks_like_cf_challenge(page):
                log.info(f"[Task {index}] Cloudflare challenge cleared")
                plog.board_status(index, f"CF cleared ({elapsed:.0f}s)")
                # Brief settle so Set-Cookie lands after redirect.
                time.sleep(0.6)
                return True
            # Progress: session cookie may appear even mid-CF widget.
            if self._cookie_has_session(page) and not _looks_like_cf_challenge(page):
                return True
            plog.board_status(
                index, f"CF Verify human… {elapsed:.0f}/{max_wait:.0f}s"
            )

        if soft:
            log.warning(
                f"[Task {index}] CF still present after {max_wait:.0f}s (soft continue)"
            )
            plog.board_status(index, f"CF still up after {max_wait:.0f}s")
            return False
        raise BrowserRegistrationError(
            "Stuck on Cloudflare 'Verify you are human'. "
            "Use headless=false, Chrome, residential proxy; "
            "or click the checkbox manually if keep_open_on_error."
        )

    def _raise_if_hard_block(self, page, stage: str, index: int = 0) -> None:
        if _looks_like_phone_challenge(page):
            raise BrowserRegistrationError(
                f"Phone verification required ({stage}) — not supported in automation"
            )
        # CF checkbox / managed challenge: try click+wait before hard-fail.
        if _looks_like_cf_challenge(page):
            ok = self._wait_out_cloudflare(
                page, index, max_wait=35.0, soft=True
            )
            if not ok and _looks_like_cf_challenge(page):
                raise BrowserRegistrationError(
                    f"Cloudflare 'Verify you are human' blocked signup ({stage}). "
                    f"Use headless=false + residential proxy, or click manually."
                )
        text = _page_text(page).lower()
        hard = (
            "unable to create account",
            "something went wrong",
            "too many requests",
            "try again later",
            "not eligible",
            "access denied",
        )
        # Only raise when the page is *mostly* an error (short body + keyword).
        if any(h in text for h in hard) and len(text) < 800:
            raise BrowserRegistrationError(f"Hard error on page ({stage}): {text[:200]}")

    def _snapshot_cookies(self, context, page=None) -> list[dict]:
        try:
            cookies = context.cookies() or []
            if cookies:
                return list(cookies)
        except Exception:
            pass
        if page is not None:
            try:
                return list(page.context.cookies() or [])
            except Exception:
                pass
        return []

    def _read_session_token(self, context, page=None) -> str:
        """Best-effort pull of NextAuth session token from browser cookies."""
        cookies = self._snapshot_cookies(context, page)
        chatgpt = [
            c
            for c in cookies
            if isinstance(c, dict) and "chatgpt.com" in str(c.get("domain") or "")
        ]
        return (
            extract_session_token_from_cookies(chatgpt)
            or extract_session_token_from_cookies(cookies)
            or ""
        )

    def _harvest_in_browser(self, page, index: int) -> dict | None:
        """Fetch /api/auth/session inside the browser (carries CF cookies)."""
        log = logging.getLogger(__name__)
        try:
            cur = (page.url or "").lower()
        except Exception:
            cur = ""
        # Ensure we are on chatgpt.com origin so credentials:include works.
        if "chatgpt.com" not in cur:
            try:
                page.goto(
                    "https://chatgpt.com/",
                    wait_until="domcontentloaded",
                    timeout=20_000,
                )
                time.sleep(0.4)
            except Exception as e:
                log.warning(f"[Task {index}] harvest goto home: {e}")

        try:
            data = page.evaluate(
                """async () => {
                    const r = await fetch('/api/auth/session', {
                        credentials: 'include',
                        headers: { 'accept': 'application/json' },
                        cache: 'no-store',
                    });
                    const text = await r.text();
                    if (!r.ok) {
                        return { __http: r.status, __body: text.slice(0, 160) };
                    }
                    try { return JSON.parse(text); }
                    catch (e) {
                        return { __http: r.status, __body: text.slice(0, 160) };
                    }
                }"""
            )
        except Exception as e:
            log.warning(f"[Task {index}] in-browser session fetch failed: {e}")
            return None

        if not isinstance(data, dict):
            return None
        if data.get("__http") or not (
            data.get("accessToken") or data.get("access_token")
        ):
            log.warning(f"[Task {index}] in-browser session weak: {data}")
            return None
        return data

    def _harvest_session(self, context, page, index: int) -> tuple[str, dict]:
        """Exchange session cookie → access_token (browser-first, HTTP fallback).

        Pure HTTP with only the NextAuth cookie often gets **403 Cloudflare HTML**.
        Prefer in-page ``fetch`` (Chrome) which reuses the browser's CF clearance.
        Camoufox still prefers close-then-HTTP, but ships the **full cookie jar**.
        """
        log = logging.getLogger(__name__)
        t0 = time.monotonic()
        camoufox = self.engine == "camoufox"
        plog.board_status(index, "harvest · read session cookie…")

        cookies = self._snapshot_cookies(context, page)
        session_token = self._read_session_token(context, page)

        if not session_token and not camoufox:
            plog.board_status(index, "harvest · open chatgpt.com for cookie…")
            try:
                page.goto(
                    "https://chatgpt.com/",
                    wait_until="domcontentloaded",
                    timeout=20_000,
                )
                time.sleep(0.5)
            except Exception as e:
                log.warning(f"[Task {index}] harvest mint cookie: {e}")
            cookies = self._snapshot_cookies(context, page)
            session_token = self._read_session_token(context, page)

        if not session_token:
            raise BrowserRegistrationError(
                "No __Secure-next-auth.session-token cookie after signup"
            )

        data: dict | None = None

        # ── 1) In-browser fetch (best for CF) — skip on Camoufox to avoid WS ──
        if not camoufox:
            plog.board_status(index, "harvest · in-browser /api/auth/session…")
            data = self._harvest_in_browser(page, index)
            if data is None:
                # One more settle + retry (cookie propagation).
                time.sleep(0.8)
                data = self._harvest_in_browser(page, index)

        # Snapshot cookies again after any navigation (cf_clearance may update).
        cookies = self._snapshot_cookies(context, page) or cookies
        session_token = (
            self._read_session_token(context, page) or session_token
        )

        # ── 2) HTTP with full jar (Camoufox primary / Chrome fallback) ──
        if data is None:
            plog.board_status(index, "harvest · HTTP /api/auth/session…")
            log.info(
                f"[Task {index}] Harvest via HTTP "
                f"(cookie_len={len(session_token)}, jar={len(cookies)}, "
                f"engine={self.engine})"
            )
            # Close page first for Camoufox stability; Chrome can keep page.
            if camoufox:
                try:
                    page.close()
                except Exception:
                    pass
                try:
                    context.close()
                except Exception:
                    pass
            try:
                data = fetch_session_from_token(
                    session_token,
                    proxy=self.proxy,
                    timeout=20,
                    max_retries=4,
                    extra_cookies=cookies,
                )
            except Exception as e:
                # Chrome last resort: try in-browser once more if page still open.
                if not camoufox:
                    log.warning(
                        f"[Task {index}] HTTP harvest failed ({e}); retry in-browser"
                    )
                    plog.board_status(index, "harvest · retry in-browser…")
                    time.sleep(1.0)
                    data = self._harvest_in_browser(page, index)
                if data is None:
                    raise BrowserRegistrationError(
                        f"Failed to harvest access_token: {e}"
                    ) from e

        if not isinstance(data, dict):
            raise BrowserRegistrationError("Invalid session payload")

        access = str(data.get("accessToken") or data.get("access_token") or "").strip()
        if not access:
            raise BrowserRegistrationError(
                "Harvested session JSON but no accessToken"
            )

        # Prefer cookie we already hold if body omits sessionToken.
        body_st = str(
            data.get("sessionToken") or data.get("session_token") or ""
        ).strip()
        if body_st:
            session_token = body_st

        elapsed = time.monotonic() - t0
        plog.board_status(index, f"access_token ok · {elapsed:.1f}s")
        log.info(
            f"[Task {index}] access_token harvested in {elapsed:.1f}s "
            f"(len={len(access)}, path={'browser' if not camoufox else 'http'})"
        )
        # Detach page ASAP so register()'s browser.close won't sit on ChatGPT WS.
        if not camoufox and page is not None:
            try:
                page.close()
            except Exception:
                pass
        return session_token, data


# ── Worker (thread-pool entry) ───────────────────────────────────────


def browser_register_worker(
    index: int,
    proxy: str = "",
    flaresolverr_url: str = "",  # unused; kept for signature parity
    mail_config: dict | None = None,
    browser_cfg: dict | None = None,
    mailbox: dict | None = None,
) -> dict:
    """Single browser registration worker. Returns {ok, index, result|error}.

    If a signup stage is stuck longer than ``step_timeout_s``, the attempt
    aborts and is **re-run once** (``register_retries``, default 1).
    """
    start = time.time()
    browser_cfg = browser_cfg or {}
    log = logging.getLogger(__name__)

    # register_retries=1 → up to 2 attempts total.
    retries = int(browser_cfg.get("register_retries", 1) or 0)
    retries = max(0, min(retries, 2))  # hard cap: at most 2 retries
    max_attempts = 1 + retries
    step_timeout_s = float(browser_cfg.get("step_timeout_s", 90) or 90)

    def _make_registrar() -> BrowserRegistrar:
        return BrowserRegistrar(
            proxy=proxy,
            mail_config=mail_config,
            headless=bool(browser_cfg.get("headless", False)),
            browser_channel=str(browser_cfg.get("channel", "") or "").strip(),
            engine=str(browser_cfg.get("engine", "") or "").strip(),
            timeout_ms=int(browser_cfg.get("timeout_ms", 120_000) or 120_000),
            signup_url=str(
                browser_cfg.get("signup_url", SIGNUP_URL_DEFAULT) or SIGNUP_URL_DEFAULT
            ).strip(),
            slow_mo_ms=int(browser_cfg.get("slow_mo_ms", 0) or 0),
            keep_open_on_error=bool(browser_cfg.get("keep_open_on_error", False)),
            humanize=bool(browser_cfg.get("humanize", True)),
            humanize_typing_ms=browser_cfg.get("humanize_typing_ms", "45-130"),
            camoufox_os=str(
                browser_cfg.get("camoufox_os", "random") or "random"
            ).strip(),
            camoufox_humanize=browser_cfg.get("camoufox_humanize", True),
            camoufox_geoip=bool(browser_cfg.get("camoufox_geoip", True)),
            camoufox_block_webrtc=bool(
                browser_cfg.get("camoufox_block_webrtc", True)
            ),
            camoufox_block_images=bool(
                browser_cfg.get("camoufox_block_images", False)
            ),
            camoufox_locale=str(browser_cfg.get("camoufox_locale", "") or "").strip(),
            step_timeout_s=step_timeout_s,
            warmup_reload_s=float(
                browser_cfg.get("warmup_reload_s", 10) or 10
            ),
            otp_page_timeout_s=float(
                browser_cfg.get("otp_page_timeout_s", 45) or 45
            ),
            about_you_settle_s=float(
                browser_cfg.get("about_you_settle_s", 90) or 90
            ),
            yescaptcha=(
                browser_cfg.get("yescaptcha")
                if isinstance(browser_cfg.get("yescaptcha"), dict)
                else {}
            ),
            turnstile_local=(
                browser_cfg.get("turnstile_local")
                if isinstance(browser_cfg.get("turnstile_local"), dict)
                else {}
            ),
        )

    last_err: Exception | None = None
    # Full signup timeout per attempt (warm-up + OTP + about-you + harvest).
    attempt_timeout = float(browser_cfg.get("attempt_timeout_s", 0) or 0)
    if attempt_timeout <= 0:
        attempt_timeout = max(180.0, step_timeout_s * 6)

    for attempt in range(1, max_attempts + 1):
        soft = attempt < max_attempts
        if attempt > 1:
            plog.board_status(
                index,
                f"stuck · re-register {attempt}/{max_attempts}…",
            )
            log.warning(
                f"[Task {index}] Re-register attempt {attempt}/{max_attempts} "
                f"after: {last_err}"
            )
            time.sleep(1.0)

        # Run Playwright on a **fresh dedicated thread** so:
        #  1) pool-thread asyncio pollution cannot affect the next account
        #  2) a hung browser.close cannot poison threads=1 sequential runs
        box: dict[str, Any] = {}

        def _run_attempt() -> None:
            try:
                _reset_thread_event_loop()
                registrar = _make_registrar()
                box["result"] = registrar.register(
                    index, mailbox=mailbox, soft_error=soft
                )
                box["ok"] = True
            except BaseException as e:  # noqa: BLE001
                box["ok"] = False
                box["error"] = e
            finally:
                try:
                    _reset_thread_event_loop()
                except Exception:
                    pass

        t = threading.Thread(
            target=_run_attempt,
            name=f"reg-{index}-a{attempt}",
            daemon=True,
        )
        t.start()
        t.join(timeout=attempt_timeout)

        if t.is_alive():
            last_err = BrowserRegistrationError(
                f"register attempt hung >{attempt_timeout:.0f}s "
                f"(abandoned thread; next attempt uses a clean thread)"
            )
            log.warning(f"[Task {index}] {last_err}")
            plog.board_status(index, f"attempt hung · abandon {attempt_timeout:.0f}s")
            if soft:
                continue
            break

        if box.get("ok"):
            cost = time.time() - start
            if attempt > 1:
                log.info(
                    f"[Task {index}] Re-register succeeded on attempt {attempt}"
                )
            return {
                "ok": True,
                "index": index,
                "result": box["result"],
                "cost_seconds": round(cost, 1),
                "attempts": attempt,
            }

        err = box.get("error")
        last_err = err if isinstance(err, Exception) else Exception(str(err))
        log.warning(
            f"[Task {index}] Register attempt {attempt}/{max_attempts} failed: {last_err}"
        )
        if soft:
            continue
        break

    cost = time.time() - start
    if mailbox is not None:
        try:
            release_mailbox(mailbox)
        except Exception:
            pass
    return {
        "ok": False,
        "index": index,
        "error": str(last_err or "register failed"),
        "cost_seconds": round(cost, 1),
        "attempts": max_attempts,
    }
