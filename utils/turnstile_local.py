"""本机 Turnstile 辅助（只服务本仓库，不连外部打码队列）。

思路对齐 ``ts_solver.py``（真 Chromium + 住宅代理产 token），但：

  * 不连 Redis / reg.venlacy.com
  * 不绑定 xAI sitekey
  * 用本仓库已有的 Playwright
  * 优先在**当前挑战页**上等 token；失败再开独立浏览器 widget 页

用法（给 browser_registrar / 协议预热调用）::

    from utils.turnstile_local import solve_turnstile_local

    token = solve_turnstile_local(
        website_url=page.url,
        website_key=sitekey,   # 可空，自动从页面抓
        proxy="http://127.0.0.1:7890",
        headless=True,
        timeout=90,
    )
"""

from __future__ import annotations

import logging
import random
import re
import threading
import time
from typing import Any, Callable
from urllib.parse import unquote, urlparse

logger = logging.getLogger(__name__)

# Serialize Playwright launch on worker threads (same class of bugs as browser reg).
_LAUNCH_LOCK = threading.Lock()


def _reset_loop() -> None:
    try:
        import asyncio

        try:
            old = asyncio.get_event_loop_policy().get_event_loop()
        except Exception:
            old = None
        if old is not None and not old.is_closed() and not old.is_running():
            try:
                old.close()
            except Exception:
                pass
        asyncio.set_event_loop(asyncio.new_event_loop())
    except Exception:
        pass


def extract_sitekey_from_html(html: str) -> str:
    html = html or ""
    for pat in (
        r'data-sitekey=["\']([^"\']+)["\']',
        r'sitekey["\']?\s*[:=]\s*["\'](0x[0-9A-Za-z_-]+)["\']',
        r'[?&](?:sitekey|k)=(0x[0-9A-Za-z_-]+)',
    ):
        m = re.search(pat, html, re.I)
        if m:
            return unquote(m.group(1)).strip()
    return ""


def extract_sitekey_from_page(page) -> str:
    """Best-effort Turnstile sitekey from a live Playwright page."""
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
    try:
        return extract_sitekey_from_html(page.content() or "")
    except Exception:
        return ""


def read_turnstile_token_from_page(page) -> str:
    """Read solved token from DOM if present."""
    try:
        tok = page.evaluate(
            """() => {
                const names = [
                    'cf-turnstile-response',
                    'g-recaptcha-response',
                    'h-captcha-response',
                ];
                for (const n of names) {
                    const el = document.querySelector(
                        `textarea[name="${n}"], input[name="${n}"]`
                    );
                    if (el && el.value && el.value.length > 40) return el.value;
                }
                if (window.__ts_token && String(window.__ts_token).length > 40)
                    return String(window.__ts_token);
                if (window.turnstile && typeof window.turnstile.getResponse === 'function') {
                    try {
                        const t = window.turnstile.getResponse();
                        if (t && t.length > 40) return t;
                    } catch (e) {}
                }
                return '';
            }"""
        )
        return str(tok or "").strip()
    except Exception:
        return ""


def _playwright_proxy(proxy: str) -> dict[str, str] | None:
    proxy = str(proxy or "").strip()
    if not proxy:
        return None
    # Reuse browser registrar logic if available
    try:
        from register.browser_registrar import _playwright_proxy as _pp

        return _pp(proxy)
    except Exception:
        pass
    from urllib.parse import urlparse, unquote as uq

    if "://" not in proxy:
        proxy = "http://" + proxy
    p = urlparse(proxy)
    if not p.hostname:
        return None
    scheme = (p.scheme or "http").lower()
    if scheme in ("socks5", "socks5h") and (p.username or p.password):
        scheme = "http"
    server = f"{scheme}://{p.hostname}"
    if p.port:
        server += f":{p.port}"
    out: dict[str, str] = {"server": server}
    if p.username:
        out["username"] = uq(p.username)
    if p.password:
        out["password"] = uq(p.password)
    return out


def _click_cf_checkbox_on_page(page) -> bool:
    """Human-ish attempt to click Verify you are human / iframe checkbox."""
    clicked = False
    try:
        for sel in (
            "text=Verify you are human",
            "label:has-text('Verify you are human')",
            "text=确认您是真人",
        ):
            loc = page.locator(sel).first
            if loc.count() and loc.is_visible(timeout=500):
                loc.click(timeout=2000)
                clicked = True
                break
    except Exception:
        pass
    try:
        for frame in page.frames:
            try:
                furl = (frame.url or "").lower()
            except Exception:
                furl = ""
            if furl and "cloudflare" not in furl and "turnstile" not in furl and "challenge" not in furl:
                continue
            for sel in (
                "input[type='checkbox']",
                "label",
                ".ctp-checkbox-label",
                "body",
            ):
                try:
                    loc = frame.locator(sel).first
                    if loc.count() and loc.is_visible(timeout=400):
                        loc.click(timeout=2000)
                        clicked = True
                        break
                except Exception:
                    continue
            if clicked:
                break
    except Exception:
        pass
    if not clicked:
        try:
            iframe = page.locator(
                "iframe[src*='challenges.cloudflare.com'], "
                "iframe[src*='turnstile']"
            ).first
            if iframe.count() and iframe.is_visible(timeout=600):
                box = iframe.bounding_box(timeout=800)
                if box:
                    cx = box["x"] + box["width"] * random.uniform(0.15, 0.4)
                    cy = box["y"] + box["height"] * random.uniform(0.35, 0.65)
                    page.mouse.click(cx, cy)
                    clicked = True
        except Exception:
            pass
    return clicked


def wait_token_on_page(
    page,
    *,
    timeout: float = 60.0,
    click_checkbox: bool = True,
    on_status: Callable[[str], None] | None = None,
) -> str:
    """Poll current page until Turnstile token appears (or timeout)."""
    deadline = time.time() + max(5.0, timeout)
    last_click = 0.0
    t0 = time.monotonic()
    while time.time() < deadline:
        tok = read_turnstile_token_from_page(page)
        if tok and len(tok) >= 40:
            return tok
        elapsed = time.monotonic() - t0
        if on_status and int(elapsed) % 3 == 0:
            try:
                on_status(f"local TS wait token… {elapsed:.0f}s")
            except Exception:
                pass
        if click_checkbox and (time.time() - last_click) >= 3.0:
            try:
                _click_cf_checkbox_on_page(page)
            except Exception:
                pass
            last_click = time.time()
        time.sleep(0.6)
    return ""


_WIDGET_HTML = """<!DOCTYPE html>
<html><head>
<meta charset="utf-8"/>
<title>local-turnstile</title>
<script src="https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit" async defer></script>
</head>
<body style="margin:40px;font-family:sans-serif;background:#111;color:#eee">
<h3>local turnstile solver</h3>
<div id="cf-widget"></div>
<script>
window.__ts_token = "";
window.__ts_err = "";
function onTsSuccess(t) { window.__ts_token = t || ""; }
function onTsError(e) { window.__ts_err = String(e || "error"); }
function onTsExpired() { window.__ts_token = ""; }
function boot() {
  if (!window.turnstile) { setTimeout(boot, 200); return; }
  turnstile.render("#cf-widget", {
    sitekey: %SITEKEY%,
    callback: onTsSuccess,
    "error-callback": onTsError,
    "expired-callback": onTsExpired,
    theme: "light"
  });
}
boot();
</script>
</body></html>
"""


def capture_turnstile_in_browser(
    *,
    website_url: str = "",
    website_key: str = "",
    proxy: str = "",
    headless: bool = True,
    timeout: float = 90.0,
    channel: str = "chrome",
    on_status: Callable[[str], None] | None = None,
) -> str:
    """Open a short-lived Chromium, solve Turnstile, return token.

    Prefer ``website_url`` when it is the real challenge page (domain-bound).
    If only ``website_key`` is known, load an explicit widget page (may fail
    if sitekey is domain-restricted — then pass the real challenge URL).
    """
    website_url = str(website_url or "").strip()
    website_key = str(website_key or "").strip()
    if not website_url and not website_key:
        raise ValueError("website_url or website_key required")

    try:
        from playwright.sync_api import sync_playwright
    except ImportError as e:
        raise RuntimeError("playwright not installed") from e

    def status(msg: str) -> None:
        logger.info("turnstile_local: %s", msg)
        if on_status:
            try:
                on_status(msg)
            except Exception:
                pass

    token = ""
    with _LAUNCH_LOCK:
        _reset_loop()
        pw = sync_playwright().start()
        try:
            launch_kwargs: dict[str, Any] = {
                "headless": bool(headless),
                "args": [
                    "--disable-blink-features=AutomationControlled",
                    "--no-sandbox",
                    "--disable-dev-shm-usage",
                ],
            }
            ch = str(channel or "").strip().lower()
            if ch and ch not in ("chromium", "camoufox", "fox", ""):
                launch_kwargs["channel"] = ch
            browser = pw.chromium.launch(**launch_kwargs)
            try:
                ctx_kwargs: dict[str, Any] = {
                    "viewport": {"width": 1280, "height": 900},
                    "locale": "en-US",
                    "user_agent": (
                        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
                        "AppleWebKit/537.36 (KHTML, like Gecko) "
                        "Chrome/145.0.0.0 Safari/537.36"
                    ),
                }
                px = _playwright_proxy(proxy)
                if px:
                    ctx_kwargs["proxy"] = px
                context = browser.new_context(**ctx_kwargs)
                page = context.new_page()
                page.set_default_timeout(int(min(timeout, 120) * 1000))

                if website_url:
                    status(f"goto challenge url… {website_url[:60]}")
                    try:
                        page.goto(
                            website_url,
                            wait_until="domcontentloaded",
                            timeout=min(60000, int(timeout * 1000)),
                        )
                    except Exception as e:
                        logger.warning("goto challenge: %s", e)
                    if not website_key:
                        website_key = extract_sitekey_from_page(page)
                    token = wait_token_on_page(
                        page,
                        timeout=min(45.0, timeout * 0.55),
                        click_checkbox=True,
                        on_status=on_status,
                    )

                # Widget page fallback (explicit render)
                if (not token or len(token) < 40) and website_key:
                    status(f"widget page sitekey={website_key[:12]}…")
                    html = _WIDGET_HTML.replace(
                        "%SITEKEY%", json_dumps_str(website_key)
                    )
                    # Origin matters for some keys — try set content on about:blank
                    # then optionally navigate to a same-site blank if url given.
                    if website_url and "://" in website_url:
                        try:
                            origin = (
                                f"{urlparse(website_url).scheme}://"
                                f"{urlparse(website_url).netloc}/"
                            )
                            page.goto(origin, wait_until="domcontentloaded", timeout=30000)
                        except Exception:
                            pass
                    page.set_content(html, wait_until="domcontentloaded")
                    deadline = time.time() + min(50.0, timeout * 0.6)
                    while time.time() < deadline:
                        try:
                            tok = page.evaluate("() => window.__ts_token || ''")
                        except Exception:
                            tok = ""
                        tok = str(tok or "").strip()
                        if len(tok) >= 40:
                            token = tok
                            break
                        # also try clicking if interactive
                        try:
                            _click_cf_checkbox_on_page(page)
                        except Exception:
                            pass
                        time.sleep(0.7)

                if not token or len(token) < 40:
                    token = read_turnstile_token_from_page(page)
            finally:
                try:
                    browser.close()
                except Exception:
                    pass
        finally:
            try:
                pw.stop()
            except Exception:
                pass
            _reset_loop()

    token = str(token or "").strip()
    if len(token) < 40:
        raise RuntimeError(
            "local turnstile: no token "
            f"(url={website_url[:60]!r} key={website_key[:16]!r})"
        )
    status(f"token ok len={len(token)}")
    return token


def json_dumps_str(s: str) -> str:
    import json

    return json.dumps(str(s))


def solve_turnstile_local(
    *,
    website_url: str = "",
    website_key: str = "",
    proxy: str = "",
    headless: bool = True,
    timeout: float = 90.0,
    channel: str = "chrome",
    page=None,
    on_status: Callable[[str], None] | None = None,
) -> str:
    """Unified entry: try existing page first, then short-lived browser.

    Args:
        page: optional live Playwright page (registration browser). If set,
            wait/click on it first before spawning a second browser.
    """
    # 1) On the live challenge page
    if page is not None:
        if on_status:
            try:
                on_status("local TS · on current page…")
            except Exception:
                pass
        if not website_key:
            try:
                website_key = extract_sitekey_from_page(page)
            except Exception:
                website_key = ""
        if not website_url:
            try:
                website_url = page.url or ""
            except Exception:
                website_url = ""
        tok = wait_token_on_page(
            page,
            timeout=min(25.0, max(8.0, timeout * 0.3)),
            click_checkbox=True,
            on_status=on_status,
        )
        if tok and len(tok) >= 40:
            return tok

    # 2) Dedicated browser capture
    return capture_turnstile_in_browser(
        website_url=website_url,
        website_key=website_key,
        proxy=proxy,
        headless=headless,
        timeout=timeout,
        channel=channel,
        on_status=on_status,
    )
