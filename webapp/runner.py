"""Pipeline run manager — launches ``cli.py`` as a subprocess and streams logs.

Running the pipeline out-of-process keeps the web server responsive, lets us
stop a run cleanly (SIGINT → the pipeline saves partial results), and gives us
a single stdout/stderr stream to fan out to browser clients over SSE.
"""

from __future__ import annotations

import os
import queue
import re
import signal
import subprocess
import sys
import threading
import time
from collections import deque
from pathlib import Path
from typing import Any

# Strip ANSI escapes just in case (we also force NO_COLOR on the child).
_ANSI_RE = re.compile(r"\x1b\[[0-9;]*[mGKHF]")

# Repo root (where cli.py lives) — the code, not the data.
REPO_ROOT = Path(__file__).resolve().parent.parent


class RunManager:
    """Owns at most one live pipeline subprocess and its log buffer."""

    def __init__(self, data_dir: Path, max_lines: int = 5000) -> None:
        self.data_dir = data_dir
        self._proc: subprocess.Popen[str] | None = None
        self._lock = threading.Lock()
        self._buffer: deque[str] = deque(maxlen=max_lines)
        self._subscribers: set[queue.Queue[str | None]] = set()
        self._reader: threading.Thread | None = None
        self._started_at: float | None = None
        self._last_cmd: list[str] = []
        self._exit_code: int | None = None

    # ── State ────────────────────────────────────────────────────────

    def is_running(self) -> bool:
        with self._lock:
            return self._proc is not None and self._proc.poll() is None

    def status(self) -> dict[str, Any]:
        with self._lock:
            running = self._proc is not None and self._proc.poll() is None
            return {
                "running": running,
                "pid": self._proc.pid if self._proc else None,
                "started_at": self._started_at,
                "elapsed": (time.time() - self._started_at)
                if (running and self._started_at)
                else None,
                "command": " ".join(self._last_cmd),
                "exit_code": self._exit_code,
                "buffered_lines": len(self._buffer),
            }

    def snapshot(self) -> list[str]:
        with self._lock:
            return list(self._buffer)

    # ── Log fan-out ──────────────────────────────────────────────────

    def subscribe(self) -> "queue.Queue[str | None]":
        q: queue.Queue[str | None] = queue.Queue(maxsize=10000)
        with self._lock:
            self._subscribers.add(q)
        return q

    def unsubscribe(self, q: "queue.Queue[str | None]") -> None:
        with self._lock:
            self._subscribers.discard(q)

    def _emit(self, line: str) -> None:
        with self._lock:
            self._buffer.append(line)
            subs = list(self._subscribers)
        for q in subs:
            try:
                q.put_nowait(line)
            except queue.Full:
                pass

    # ── Lifecycle ────────────────────────────────────────────────────

    def start(self, *, count: int | None = None, extra_args: list[str] | None = None) -> None:
        with self._lock:
            if self._proc is not None and self._proc.poll() is None:
                raise RuntimeError("pipeline already running")

        cfg = self.data_dir / "config.toml"
        if not cfg.exists():
            raise FileNotFoundError("config.toml not found — save a config first")

        cmd = [
            sys.executable,
            "-u",
            str(REPO_ROOT / "cli.py"),
            "-c",
            str(cfg),
            "run",
            # Web can't render the Rich live board; force the streaming style.
            "--log-style",
            "lines",
        ]
        if count is not None:
            cmd += ["-n", str(int(count))]
        if extra_args:
            cmd += list(extra_args)

        env = dict(os.environ)
        env["NO_COLOR"] = "1"
        env["PYTHONUNBUFFERED"] = "1"
        env["PYTHONIOENCODING"] = "utf-8"

        proc = subprocess.Popen(
            cmd,
            cwd=str(self.data_dir),
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            env=env,
            text=True,
            encoding="utf-8",
            errors="replace",
            bufsize=1,
            start_new_session=True,  # own process group → clean signal delivery
        )

        with self._lock:
            self._proc = proc
            self._started_at = time.time()
            self._last_cmd = cmd
            self._exit_code = None
            self._buffer.clear()

        self._emit(f"▶ 启动流水线: {' '.join(cmd[2:])}")
        self._reader = threading.Thread(target=self._pump, args=(proc,), daemon=True)
        self._reader.start()

    def _pump(self, proc: subprocess.Popen[str]) -> None:
        assert proc.stdout is not None
        for raw in proc.stdout:
            line = _ANSI_RE.sub("", raw.rstrip("\n"))
            self._emit(line)
        code = proc.wait()
        with self._lock:
            self._exit_code = code
        self._emit(f"■ 流水线结束 (exit={code})")
        # Signal end-of-stream to any live subscribers.
        with self._lock:
            subs = list(self._subscribers)
        for q in subs:
            try:
                q.put_nowait(None)
            except queue.Full:
                pass

    def stop(self, force: bool = False) -> bool:
        with self._lock:
            proc = self._proc
        if proc is None or proc.poll() is not None:
            return False
        sig = signal.SIGKILL if force else signal.SIGINT
        try:
            # Signal the whole group; the pipeline traps SIGINT and saves partial.
            os.killpg(os.getpgid(proc.pid), sig)
        except ProcessLookupError:
            return False
        except Exception:
            try:
                proc.send_signal(sig)
            except Exception:
                return False
        self._emit("⏹ 收到停止请求" + ("（强制）" if force else "（优雅，正在保存部分结果）"))
        return True
