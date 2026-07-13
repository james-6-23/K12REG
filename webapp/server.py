"""FastAPI web control panel for chatgpt-register-sub2api.

Endpoints are grouped as:
  auth    — simple password login (cookie session)
  config  — read/write/generate config.toml
  files   — list/read/write/upload/delete data files (mailboxes, proxies, …)
  run     — start/stop the pipeline, live log stream (SSE), status
  results — registered accounts + downloadable artifacts

The server operates entirely inside a single "data dir" (``K12_DATA_DIR``),
so it can be run locally against the repo root or in Docker against a mounted
volume without changing behaviour.
"""

from __future__ import annotations

import json
import os
import secrets
import sys
import tomllib
from pathlib import Path
from typing import Any

from fastapi import Cookie, FastAPI, Form, HTTPException, Request, UploadFile
from fastapi.responses import (
    FileResponse,
    HTMLResponse,
    JSONResponse,
    PlainTextResponse,
    StreamingResponse,
)
from fastapi.staticfiles import StaticFiles

# Make the repo modules importable (config.generate_default_config, …).
REPO_ROOT = Path(__file__).resolve().parent.parent
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

import config as app_config  # noqa: E402
from webapp.runner import RunManager  # noqa: E402

# ── Configuration ────────────────────────────────────────────────────

DATA_DIR = Path(os.environ.get("K12_DATA_DIR", str(REPO_ROOT))).resolve()
DATA_DIR.mkdir(parents=True, exist_ok=True)

PASSWORD = os.environ.get("WEB_PASSWORD", "admin")
COOKIE_NAME = "k12_session"
STATIC_DIR = Path(__file__).resolve().parent / "static"

# Files the UI is allowed to read as text / edit inline. Uploads and downloads
# are constrained to DATA_DIR by _safe_path regardless.
TEXT_SUFFIXES = {".toml", ".txt", ".json", ".env", ".log", ".md"}
# Never expose these through the file API (secrets / noise).
HIDDEN_NAMES = {".DS_Store"}

app = FastAPI(title="K12REG Control Panel", docs_url=None, redoc_url=None)
runner = RunManager(DATA_DIR)

# In-memory session store (single-instance server).
_sessions: set[str] = set()


# ── Auth helpers ─────────────────────────────────────────────────────


def _require_auth(session: str | None) -> None:
    if not session or session not in _sessions:
        raise HTTPException(status_code=401, detail="未登录")


def _authed(session: str | None) -> bool:
    return bool(session and session in _sessions)


# ── Path helpers ─────────────────────────────────────────────────────


def _safe_path(name: str) -> Path:
    """Resolve ``name`` to a file directly inside DATA_DIR (no traversal)."""
    name = (name or "").strip()
    if not name or "/" in name or "\\" in name or name.startswith("."):
        # allow dotfiles that are explicitly known (none currently) — reject rest
        if name not in ():
            raise HTTPException(status_code=400, detail="非法文件名")
    p = (DATA_DIR / name).resolve()
    if p.parent != DATA_DIR:
        raise HTTPException(status_code=400, detail="文件必须位于数据目录内")
    return p


def _list_files() -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []
    for p in sorted(DATA_DIR.iterdir()):
        if not p.is_file() or p.name in HIDDEN_NAMES:
            continue
        try:
            st = p.stat()
        except OSError:
            continue
        out.append(
            {
                "name": p.name,
                "size": st.st_size,
                "mtime": st.st_mtime,
                "editable": p.suffix.lower() in TEXT_SUFFIXES,
            }
        )
    return out


# ── Routes: auth ─────────────────────────────────────────────────────


@app.post("/api/login")
async def login(password: str = Form(...)) -> JSONResponse:
    if not secrets.compare_digest(password, PASSWORD):
        raise HTTPException(status_code=401, detail="密码错误")
    token = secrets.token_urlsafe(32)
    _sessions.add(token)
    resp = JSONResponse({"ok": True})
    resp.set_cookie(
        COOKIE_NAME, token, httponly=True, samesite="lax", max_age=7 * 24 * 3600
    )
    return resp


@app.post("/api/logout")
async def logout(session: str | None = Cookie(default=None, alias=COOKIE_NAME)):
    if session:
        _sessions.discard(session)
    resp = JSONResponse({"ok": True})
    resp.delete_cookie(COOKIE_NAME)
    return resp


@app.get("/api/me")
async def me(session: str | None = Cookie(default=None, alias=COOKIE_NAME)):
    return {"authed": _authed(session), "data_dir": str(DATA_DIR)}


# ── Routes: config ───────────────────────────────────────────────────


@app.get("/api/config", response_class=PlainTextResponse)
async def get_config(session: str | None = Cookie(default=None, alias=COOKIE_NAME)):
    _require_auth(session)
    cfg = DATA_DIR / "config.toml"
    if not cfg.exists():
        return PlainTextResponse("", status_code=200)
    return PlainTextResponse(cfg.read_text(encoding="utf-8"))


@app.put("/api/config")
async def put_config(
    request: Request,
    session: str | None = Cookie(default=None, alias=COOKIE_NAME),
):
    _require_auth(session)
    text = (await request.body()).decode("utf-8")
    try:
        tomllib.loads(text)
    except Exception as e:
        raise HTTPException(status_code=400, detail=f"TOML 解析失败: {e}")
    (DATA_DIR / "config.toml").write_text(text, encoding="utf-8")
    return {"ok": True}


@app.post("/api/config/default", response_class=PlainTextResponse)
async def config_default(session: str | None = Cookie(default=None, alias=COOKIE_NAME)):
    _require_auth(session)
    # Return the packaged default template without overwriting an existing file.
    return PlainTextResponse(app_config.DEFAULT_CONFIG_TOML)


# ── Routes: files ────────────────────────────────────────────────────


@app.get("/api/files")
async def files(session: str | None = Cookie(default=None, alias=COOKIE_NAME)):
    _require_auth(session)
    return {"files": _list_files()}


@app.get("/api/file", response_class=PlainTextResponse)
async def read_file(
    name: str, session: str | None = Cookie(default=None, alias=COOKIE_NAME)
):
    _require_auth(session)
    p = _safe_path(name)
    if not p.exists():
        raise HTTPException(status_code=404, detail="文件不存在")
    if p.suffix.lower() not in TEXT_SUFFIXES:
        raise HTTPException(status_code=415, detail="非文本文件，请使用下载")
    return PlainTextResponse(p.read_text(encoding="utf-8", errors="replace"))


@app.put("/api/file")
async def write_file(
    request: Request,
    name: str,
    session: str | None = Cookie(default=None, alias=COOKIE_NAME),
):
    _require_auth(session)
    p = _safe_path(name)
    text = (await request.body()).decode("utf-8")
    p.write_text(text, encoding="utf-8")
    return {"ok": True}


@app.post("/api/upload")
async def upload(
    file: UploadFile,
    name: str | None = Form(default=None),
    session: str | None = Cookie(default=None, alias=COOKIE_NAME),
):
    _require_auth(session)
    target = name or file.filename or "upload.bin"
    p = _safe_path(target)
    data = await file.read()
    p.write_bytes(data)
    return {"ok": True, "name": p.name, "size": len(data)}


@app.delete("/api/file")
async def delete_file(
    name: str, session: str | None = Cookie(default=None, alias=COOKIE_NAME)
):
    _require_auth(session)
    p = _safe_path(name)
    if p.exists():
        p.unlink()
    return {"ok": True}


@app.get("/api/download")
async def download(
    name: str, session: str | None = Cookie(default=None, alias=COOKIE_NAME)
):
    _require_auth(session)
    p = _safe_path(name)
    if not p.exists():
        raise HTTPException(status_code=404, detail="文件不存在")
    return FileResponse(p, filename=p.name)


# ── Routes: run ──────────────────────────────────────────────────────


@app.get("/api/run/status")
async def run_status(session: str | None = Cookie(default=None, alias=COOKIE_NAME)):
    _require_auth(session)
    return runner.status()


@app.post("/api/run/start")
async def run_start(
    request: Request,
    session: str | None = Cookie(default=None, alias=COOKIE_NAME),
):
    _require_auth(session)
    body: dict[str, Any] = {}
    try:
        body = await request.json()
    except Exception:
        body = {}
    count = body.get("count")
    count = int(count) if count not in (None, "", 0) else None
    try:
        runner.start(count=count)
    except Exception as e:
        raise HTTPException(status_code=409, detail=str(e))
    return runner.status()


@app.post("/api/run/stop")
async def run_stop(
    request: Request,
    session: str | None = Cookie(default=None, alias=COOKIE_NAME),
):
    _require_auth(session)
    body: dict[str, Any] = {}
    try:
        body = await request.json()
    except Exception:
        body = {}
    stopped = runner.stop(force=bool(body.get("force")))
    return {"stopped": stopped, **runner.status()}


@app.get("/api/run/logs/snapshot")
async def logs_snapshot(session: str | None = Cookie(default=None, alias=COOKIE_NAME)):
    _require_auth(session)
    return {"lines": runner.snapshot()}


@app.get("/api/run/logs")
async def logs_stream(session: str | None = Cookie(default=None, alias=COOKIE_NAME)):
    _require_auth(session)

    def gen():
        # Replay the current buffer first so a fresh client sees history.
        for line in runner.snapshot():
            yield _sse(line)
        q = runner.subscribe()
        try:
            while True:
                item = q.get()
                if item is None:
                    yield _sse("", event="end")
                    # keep the stream open for the next run
                    continue
                yield _sse(item)
        finally:
            runner.unsubscribe(q)

    return StreamingResponse(gen(), media_type="text/event-stream")


def _sse(data: str, event: str | None = None) -> str:
    out = ""
    if event:
        out += f"event: {event}\n"
    # Encode as JSON so newlines / colons are safe inside one SSE frame.
    out += f"data: {json.dumps(data, ensure_ascii=False)}\n\n"
    return out


# ── Routes: results ──────────────────────────────────────────────────


@app.get("/api/accounts")
async def accounts(session: str | None = Cookie(default=None, alias=COOKIE_NAME)):
    _require_auth(session)
    p = DATA_DIR / "registered_accounts.json"
    if not p.exists():
        return {"accounts": [], "total": 0}
    try:
        data = json.loads(p.read_text(encoding="utf-8"))
    except Exception:
        return {"accounts": [], "total": 0}
    rows = data if isinstance(data, list) else []

    def summarize(a: dict[str, Any]) -> dict[str, Any]:
        return {
            "email": a.get("email"),
            "plan_type": a.get("plan_type"),
            "join_status": a.get("join_status"),
            "approve_status": a.get("approve_status"),
            "elevate_status": a.get("elevate_status"),
            "import_status": a.get("import_status"),
            "chatgpt_account_id": a.get("chatgpt_account_id"),
            "has_access_token": bool(a.get("access_token")),
            "has_refresh_token": bool(a.get("refresh_token")),
        }

    return {"accounts": [summarize(a) for a in rows], "total": len(rows)}


# ── Static frontend ──────────────────────────────────────────────────


@app.get("/", response_class=HTMLResponse)
async def index():
    idx = STATIC_DIR / "index.html"
    return HTMLResponse(idx.read_text(encoding="utf-8"))


@app.get("/healthz", response_class=PlainTextResponse)
async def healthz():
    return "ok"


if STATIC_DIR.exists():
    app.mount("/static", StaticFiles(directory=str(STATIC_DIR)), name="static")
