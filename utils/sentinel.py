"""OpenAI Sentinel Token generation for auth registration / login.

Contract (aligned with successful manual HAR + 2026 SDK reverse):

  POST sentinel.openai.com/backend-api/sentinel/req
    body: {"p": requirements_token, "id": device_id, "flow": flow}
  response:
    token                          → c  (and oai-sc cookie = "0" + token)
    proofofwork {seed,difficulty}  → p  (PoW solve, prefix gAAAAAB)
    turnstile.dx                   → t  (JSVMP via Node sdkvm; MUST NOT hardcode "")
    so.snapshot_dx / so.dx         → so (same VM when required)

  Headers on create_account:
    openai-sentinel-token    = {"p","t","c","id","flow"}  with t non-empty when dx present
    openai-sentinel-so-token = {"so","c","id","flow"}     when so present

SDK version must match latest successful HAR (currently 20260219f9f6).
After CF clearance retry, always regenerate — never reuse old tokens.

Turnstile / so dx execution:
  Prefer Node JSVMP runner at utils/sentinel_vm/run_sentinel_vm.js
  (XOR key = the exact `p` sent to /sentinel/req).
  Falls back to pure-Python mini-VM if Node is unavailable.
"""

from __future__ import annotations

import base64
import json
import logging
import os
import random
import shutil
import subprocess
import time
import uuid
from datetime import datetime, timezone
from typing import Any

from curl_cffi.requests import Session

logger = logging.getLogger(__name__)

# ── SDK fingerprint (update from latest successful HAR) ─────────────

SENTINEL_SDK_VERSION = "20260219f9f6"
SENTINEL_SDK_URL = f"https://sentinel.openai.com/sentinel/{SENTINEL_SDK_VERSION}/sdk.js"
SENTINEL_REQ_URL = "https://sentinel.openai.com/backend-api/sentinel/req"
SENTINEL_FRAME_REFERER = (
    f"https://sentinel.openai.com/backend-api/sentinel/frame.html"
    f"?sv={SENTINEL_SDK_VERSION}"
)

_VM_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "sentinel_vm")
_VM_RUNNER = os.path.join(_VM_DIR, "run_sentinel_vm.js")

DEFAULT_SENTINEL_USER_AGENT = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/145.0.0.0 Safari/537.36"
)
DEFAULT_SENTINEL_SEC_CH_UA = (
    '"Chromium";v="145", "Google Chrome";v="145", "Not/A)Brand";v="99"'
)


# ── PoW generator ───────────────────────────────────────────────────


class SentinelTokenGenerator:
    """Sentinel PoW (FNV-1a) — must match sentinel SDK algorithm."""

    MAX_ATTEMPTS = 500_000
    ERROR_PREFIX = "wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D"

    def __init__(self, device_id: str, ua: str):
        self.device_id = device_id
        self.user_agent = ua
        self.sid = str(uuid.uuid4())
        # Per-instance seed used for requirements token (same as SDK)
        self.requirements_seed = str(random.random())

    @staticmethod
    def _fnv1a_32(text: str) -> str:
        h = 2166136261
        for ch in text:
            h ^= ord(ch)
            h = (h * 16777619) & 0xFFFFFFFF
        h ^= h >> 16
        h = (h * 2246822507) & 0xFFFFFFFF
        h ^= h >> 13
        h = (h * 3266489909) & 0xFFFFFFFF
        h ^= h >> 16
        return format(h & 0xFFFFFFFF, "08x")

    def _get_config(self) -> list:
        """25-element browser fingerprint (2026 SDK _getConfig)."""
        now = datetime.now(timezone.utc)
        date_str = now.strftime(
            "%a %b %d %Y %H:%M:%S GMT+0000 (Coordinated Universal Time)"
        )
        perf_now = random.uniform(1000, 50000)

        nav_props = [
            "vendorSub",
            "productSub",
            "vendor",
            "maxTouchPoints",
            "scheduling",
            "userActivation",
            "doNotTrack",
            "geolocation",
            "connection",
            "plugins",
            "mimeTypes",
            "pdfViewerEnabled",
            "hardwareConcurrency",
            "cookieEnabled",
            "credentials",
            "mediaDevices",
            "permissions",
            "locks",
        ]
        # SDK uses U+2212 (minus) not ASCII hyphen
        nav_val = f"{random.choice(nav_props)}\u2212undefined"

        return [
            1920 + 1080,  # [0] screen.width + screen.height
            date_str,  # [1]
            4294705152,  # [2] jsHeapSizeLimit
            random.random(),  # [3] ← nonce
            self.user_agent,  # [4]
            SENTINEL_SDK_URL,  # [5]
            None,  # [6] data-build
            "en-US",  # [7]
            "en-US,en",  # [8]
            random.random(),  # [9] ← elapsed
            nav_val,  # [10]
            random.choice(
                ["location", "implementation", "URL", "documentURI", "compatMode"]
            ),
            random.choice(
                ["Object", "Function", "Array", "Number", "parseFloat", "undefined"]
            ),
            perf_now,  # [13]
            self.sid,  # [14]
            "",  # [15]
            random.choice([4, 8, 12, 16]),  # [16]
            time.time() * 1000 - perf_now,  # [17]
            0,
            0,
            0,
            0,
            0,
            0,
            0,  # [18-24] feature flags
        ]

    @staticmethod
    def _b64(data) -> str:
        return base64.b64encode(
            json.dumps(data, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        ).decode("ascii")

    def _run_check(
        self, start_time: float, seed: str, difficulty: str, config: list, nonce: int
    ) -> str | None:
        config[3] = nonce
        config[9] = round((time.time() - start_time) * 1000)
        data = self._b64(config)
        hash_hex = self._fnv1a_32(seed + data)
        if hash_hex[: len(difficulty)] <= difficulty:
            return data + "~S"
        return None

    def generate_requirements_token(self) -> str:
        """Requirements token (p sent to /req).

        2026 SDK: FNV-1a PoW with instance seed + difficulty "0",
        prefix gAAAAAC, suffix ~S. This exact string is also the XOR
        key for turnstile.dx / so.snapshot_dx — must be fully valid.
        """
        seed = self.requirements_seed
        difficulty = "0"
        start = time.time()
        config = self._get_config()
        for i in range(self.MAX_ATTEMPTS):
            result = self._run_check(start, seed, difficulty, config, i)
            if result:
                return "gAAAAAC" + result
        return "gAAAAAC" + self.ERROR_PREFIX + self._b64(str(None))

    def generate_token(self, seed: str, difficulty: str) -> str:
        """Enforcement PoW token (p field in final sentinel header)."""
        start = time.time()
        config = self._get_config()
        difficulty = str(difficulty or "0")
        for i in range(self.MAX_ATTEMPTS):
            result = self._run_check(start, seed, difficulty, config, i)
            if result:
                return "gAAAAAB" + result
        return "gAAAAAB" + self.ERROR_PREFIX + self._b64(str(None))


# ── dx solver: Node JSVMP (preferred) + pure-Python fallback ────────


def solve_dx(
    dx: str,
    xor_key: str,
    *,
    user_agent: str = "",
    location_href: str = "https://auth.openai.com/",
) -> str:
    """Decode sentinel dx (turnstile or so) and return wire token for `t`/`so`.

    xor_key MUST be the exact requirements token (p) sent in /sentinel/req.
    """
    if not dx or not xor_key:
        return ""

    # 1) Prefer real JSVMP via Node
    node_result = _solve_dx_node(
        dx, xor_key, user_agent=user_agent, location_href=location_href
    )
    if node_result:
        return node_result

    # 2) Pure-Python mini-VM fallback (weaker; logs when used)
    logger.debug("Node dx VM unavailable/failed; trying pure-Python fallback")
    return _solve_dx_python(dx, xor_key)


def _solve_dx_node(
    dx: str,
    secret: str,
    *,
    user_agent: str = "",
    location_href: str = "https://auth.openai.com/",
    timeout_s: float = 20.0,
) -> str:
    if not os.path.isfile(_VM_RUNNER):
        logger.warning("sentinel VM runner missing: %s", _VM_RUNNER)
        return ""
    node_bin = shutil.which("node")
    if not node_bin:
        logger.warning("node not in PATH; cannot run turnstile.dx JSVMP")
        return ""

    payload = {
        "secret": secret,
        "encodedPayload": dx,
        "userAgent": user_agent or DEFAULT_SENTINEL_USER_AGENT,
        "locationHref": location_href,
        "timeoutMs": 2000,
    }
    try:
        proc = subprocess.run(
            [node_bin, _VM_RUNNER],
            input=json.dumps(payload),
            capture_output=True,
            text=True,
            encoding="utf-8",
            timeout=timeout_s,
            cwd=_VM_DIR,
        )
    except FileNotFoundError:
        logger.warning("node executable not found")
        return ""
    except subprocess.TimeoutExpired:
        logger.warning("turnstile.dx VM timed out after %ss", timeout_s)
        return ""
    except Exception as e:
        logger.warning("turnstile.dx VM spawn failed: %s", e)
        return ""

    if proc.returncode != 0:
        logger.warning(
            "turnstile.dx VM exit=%s stderr=%s",
            proc.returncode,
            (proc.stderr or "")[-300:],
        )
        return ""

    try:
        out = json.loads(proc.stdout or "{}")
    except Exception as e:
        logger.warning("turnstile.dx VM bad JSON: %s stdout=%s", e, (proc.stdout or "")[:200])
        return ""

    if not out.get("ok"):
        logger.warning("turnstile.dx VM error: %s", str(out.get("error"))[:300])
        return ""

    result = out.get("result") or {}
    channel = result.get("channel")
    encoded = result.get("encodedValue")
    if channel == "resolve" and encoded:
        return str(encoded)
    if channel == "reject":
        logger.warning(
            "turnstile.dx VM reject: %s", str(result.get("value"))[:200]
        )
        return ""
    if channel == "timeout":
        logger.warning("turnstile.dx VM channel=timeout step=%s", result.get("stepCount"))
        return ""
    logger.warning("turnstile.dx VM unknown channel=%s", channel)
    return ""


def _xor_string(text: str, key: str) -> str:
    if not key:
        return text
    return "".join(chr(ord(ch) ^ ord(key[i % len(key)])) for i, ch in enumerate(text))


def _solve_dx_python(dx: str, xor_key: str) -> str:
    """Lightweight pure-Python dx VM (subset of opcodes). Prefer Node."""
    try:
        decoded = base64.b64decode(dx).decode("latin-1")
        plain = _xor_string(decoded, xor_key)
        token_list = json.loads(plain)
    except Exception as e:
        logger.debug("solve_dx python decode failed: %s", e)
        return ""

    if not isinstance(token_list, list):
        return ""

    return _run_dx_vm(token_list, xor_key) or ""


class _OrderedMap:
    def __init__(self) -> None:
        self.keys: list[str] = []
        self.values: dict[str, Any] = {}

    def add(self, key: str, value: Any) -> None:
        if key not in self.values:
            self.keys.append(key)
        self.values[key] = value


def _dx_to_str(value: Any) -> str:
    if value is None:
        return "undefined"
    if isinstance(value, float):
        return str(value)
    if isinstance(value, str):
        special = {
            "window.Math": "[object Math]",
            "window.Reflect": "[object Reflect]",
            "window.performance": "[object Performance]",
            "window.localStorage": "[object Storage]",
            "window.Object": "function Object() { [native code] }",
            "window.Reflect.set": "function set() { [native code] }",
            "window.performance.now": "function () { [native code] }",
            "window.Object.create": "function create() { [native code] }",
            "window.Object.keys": "function keys() { [native code] }",
            "window.Math.random": "function random() { [native code] }",
        }
        return special.get(value, value)
    if isinstance(value, list) and all(isinstance(item, str) for item in value):
        return ",".join(value)
    return str(value)


def _run_dx_vm(token_list: list, p: str) -> str:
    process_map: dict[Any, Any] = {}
    start_time = time.time()
    result = ""

    def func_1(e: float, t: float) -> None:
        process_map[e] = _xor_string(
            _dx_to_str(process_map.get(e)), _dx_to_str(process_map.get(t))
        )

    def func_2(e: float, t: Any) -> None:
        process_map[e] = t

    def func_3(e: str) -> None:
        nonlocal result
        result = base64.b64encode(str(e).encode()).decode()

    def func_5(e: float, t: float) -> None:
        current = process_map.get(e)
        incoming = process_map.get(t)
        if isinstance(current, (list, tuple)):
            process_map[e] = list(current) + [incoming]
            return
        if isinstance(current, (str, float, int)) or isinstance(
            incoming, (str, float, int)
        ):
            process_map[e] = _dx_to_str(current) + _dx_to_str(incoming)
            return
        process_map[e] = "NaN"

    def func_6(e: float, t: float, n: float) -> None:
        tv = process_map.get(t)
        nv = process_map.get(n)
        if isinstance(tv, str) and isinstance(nv, str):
            value = f"{tv}.{nv}"
            if value == "window.document.location":
                process_map[e] = "https://auth.openai.com/"
            else:
                process_map[e] = value

    def func_7(e: float, *args: float) -> None:
        target = process_map.get(e)
        values = [process_map.get(arg) for arg in args]
        if isinstance(target, str) and target == "window.Reflect.set":
            if len(values) >= 3:
                obj, key_name, val = values[0], values[1], values[2]
                if isinstance(obj, _OrderedMap):
                    obj.add(str(key_name), val)
        elif callable(target):
            target(*values)

    def func_8(e: float, t: float) -> None:
        process_map[e] = process_map.get(t)

    def func_14(e: float, t: float) -> None:
        process_map[e] = json.loads(process_map.get(t))  # type: ignore[arg-type]

    def func_15(e: float, t: float) -> None:
        process_map[e] = json.dumps(process_map.get(t), separators=(",", ":"))

    def func_17(e: float, t: float, *args: float) -> None:
        call_args = [process_map.get(arg) for arg in args]
        target = process_map.get(t)
        if target == "window.performance.now":
            elapsed_ns = time.time_ns() - int(start_time * 1e9)
            process_map[e] = (elapsed_ns + random.random()) / 1e6
        elif target == "window.Object.create":
            process_map[e] = _OrderedMap()
        elif target == "window.Object.keys":
            if call_args and call_args[0] == "window.localStorage":
                process_map[e] = [
                    "oai-did",
                    "oai-sc",
                    "client-correlated-secret",
                    "oai/apps/capExpiresAt",
                ]
            elif call_args and isinstance(call_args[0], _OrderedMap):
                process_map[e] = list(call_args[0].keys)
            else:
                process_map[e] = []
        elif target == "window.Math.random":
            process_map[e] = random.random()
        elif callable(target):
            process_map[e] = target(*call_args)

    def func_18(e: float) -> None:
        process_map[e] = base64.b64decode(_dx_to_str(process_map.get(e))).decode()

    def func_19(e: float) -> None:
        process_map[e] = base64.b64encode(
            _dx_to_str(process_map.get(e)).encode()
        ).decode()

    def func_20(e: float, t: float, n: float, *args: float) -> None:
        if process_map.get(e) == process_map.get(t):
            target = process_map.get(n)
            if callable(target):
                target(*[process_map.get(arg) for arg in args])

    def func_21(*_: Any) -> None:
        return

    def func_23(e: float, t: float, *args: float) -> None:
        if process_map.get(e) is not None and callable(process_map.get(t)):
            process_map[t](*[process_map.get(a) for a in args])

    def func_24(e: float, t: float, n: float) -> None:
        tv = process_map.get(t)
        nv = process_map.get(n)
        if isinstance(tv, str) and isinstance(nv, str):
            process_map[e] = f"{tv}.{nv}"

    process_map.update(
        {
            1: func_1,
            2: func_2,
            3: func_3,
            5: func_5,
            6: func_6,
            7: func_7,
            8: func_8,
            9: token_list,
            10: "window",
            14: func_14,
            15: func_15,
            16: p,
            17: func_17,
            18: func_18,
            19: func_19,
            20: func_20,
            21: func_21,
            23: func_23,
            24: func_24,
        }
    )

    for token in token_list:
        try:
            if not isinstance(token, (list, tuple)) or not token:
                continue
            fn = process_map.get(token[0])
            if callable(fn):
                fn(*token[1:])
        except Exception:
            continue
    return result


# ── Public API ──────────────────────────────────────────────────────


def build_sentinel_bundle(
    session: Session,
    device_id: str,
    flow: str,
    *,
    user_agent: str = "",
    sec_ch_ua: str = "",
) -> tuple[str, str, str]:
    """Build full sentinel headers for an auth API call.

    Returns:
        (openai-sentinel-token, oai-sc cookie value, openai-sentinel-so-token or "")

    Side effect: writes oai-sc into ``session.cookies`` when non-empty.
    """
    ua = user_agent or DEFAULT_SENTINEL_USER_AGENT
    ch_ua = sec_ch_ua or DEFAULT_SENTINEL_SEC_CH_UA
    generator = SentinelTokenGenerator(device_id, ua)

    # p sent to /req — also XOR key for turnstile/so dx (must be exact)
    req_p = generator.generate_requirements_token()

    resp = session.post(
        SENTINEL_REQ_URL,
        data=json.dumps(
            {
                "p": req_p,
                "id": device_id,
                "flow": flow,
            }
        ),
        headers={
            "Content-Type": "text/plain;charset=UTF-8",
            "Referer": SENTINEL_FRAME_REFERER,
            "Origin": "https://sentinel.openai.com",
            "User-Agent": ua,
            "sec-ch-ua": ch_ua,
            "sec-ch-ua-mobile": "?0",
            "sec-ch-ua-platform": '"Windows"',
            "Accept": "*/*",
            "Sec-Fetch-Dest": "empty",
            "Sec-Fetch-Mode": "cors",
            "Sec-Fetch-Site": "same-origin",
        },
        timeout=20,
        verify=False,
    )

    try:
        data = resp.json() if resp.text else {}
    except Exception:
        data = {}

    if not isinstance(data, dict):
        data = {}

    token = str(data.get("token") or "").strip()
    if resp.status_code != 200 or not token:
        raise RuntimeError(
            f"sentinel_req_failed status={resp.status_code} "
            f"body={(resp.text or '')[:200]}"
        )

    # ── p: proof-of-work ────────────────────────────────────────────
    pow_data = data.get("proofofwork") or {}
    if isinstance(pow_data, dict) and pow_data.get("required") and pow_data.get("seed"):
        p_value = generator.generate_token(
            str(pow_data.get("seed") or ""),
            str(pow_data.get("difficulty") or "0"),
        )
    else:
        p_value = req_p

    # ── t: turnstile.dx ─────────────────────────────────────────────
    t_value = ""
    turnstile = data.get("turnstile") or {}
    if isinstance(turnstile, dict):
        dx = str(turnstile.get("dx") or "").strip()
        if dx:
            t_value = solve_dx(dx, req_p, user_agent=ua)
            if not t_value:
                logger.warning(
                    "turnstile.dx present but solve_dx empty (flow=%s required=%s)",
                    flow,
                    turnstile.get("required"),
                )

    # ── so: so.snapshot_dx / so.dx ──────────────────────────────────
    so_header = ""
    so_info = data.get("so") or {}
    if isinstance(so_info, dict):
        so_dx = str(so_info.get("snapshot_dx") or so_info.get("dx") or "").strip()
        if so_dx:
            so_token = solve_dx(so_dx, req_p, user_agent=ua)
            if so_token:
                so_header = json.dumps(
                    {
                        "so": so_token,
                        "c": token,
                        "id": device_id,
                        "flow": flow,
                    },
                    separators=(",", ":"),
                )
            else:
                logger.warning(
                    "so.dx present but solve_dx empty (flow=%s required=%s)",
                    flow,
                    so_info.get("required"),
                )

    if not so_header:
        raw_so = str(data.get("so_token") or "").strip()
        if raw_so:
            if raw_so.startswith("{"):
                so_header = raw_so
            else:
                so_header = json.dumps(
                    {"so": raw_so, "c": token, "id": device_id, "flow": flow},
                    separators=(",", ":"),
                )

    sentinel_value = json.dumps(
        {
            "p": p_value,
            "t": t_value,
            "c": token,
            "id": device_id,
            "flow": flow,
        },
        separators=(",", ":"),
    )
    oai_sc_value = "0" + token

    try:
        session.cookies.set("oai-sc", oai_sc_value, domain=".openai.com")
        session.cookies.set("oai-sc", oai_sc_value, domain="auth.openai.com")
        session.cookies.set("oai-sc", oai_sc_value, domain="sentinel.openai.com")
    except Exception as e:
        logger.debug("set oai-sc cookie failed: %s", e)

    logger.info(
        "sentinel bundle flow=%s t=%s so=%s sdk=%s",
        flow,
        "ok" if t_value else "empty",
        "ok" if so_header else "none",
        SENTINEL_SDK_VERSION,
    )

    return sentinel_value, oai_sc_value, so_header


def build_sentinel_token(
    session: Session,
    device_id: str,
    flow: str,
    *,
    user_agent: str = "",
    sec_ch_ua: str = "",
) -> tuple[str, str]:
    """Backward-compatible wrapper: returns (openai-sentinel-token, oai-sc).

    Prefer ``build_sentinel_bundle`` for create_account (needs so-token).
    """
    sentinel_value, oai_sc_value, _so = build_sentinel_bundle(
        session,
        device_id,
        flow,
        user_agent=user_agent,
        sec_ch_ua=sec_ch_ua,
    )
    return sentinel_value, oai_sc_value
