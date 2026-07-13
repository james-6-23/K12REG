"""Board-style pipeline logging powered by **Rich Live**.

Key steps (REG/JOIN/APPR/K12/IMP) update **in place** on one row per account
instead of printing a new line each time.

Usage::

    from utils import logger as plog
    plog.setup(level="INFO")
    plog.board_start(total=3)
    plog.milestone("REG", "#1/3 user@x", "40s")
    plog.milestone("JOIN", "#1/3 user@x")
    plog.board_stop()
    plog.print_run_summary(summary)

Environment:
  NO_COLOR=1     disable colors
  FORCE_COLOR=1  force colors
"""

from __future__ import annotations

import logging
import os
import re
import sys
import threading
from typing import Any

from rich.console import Console, Group
from rich.live import Live
from rich.logging import RichHandler
from rich.panel import Panel
from rich.table import Table
from rich.text import Text
from rich.theme import Theme

# ── Theme / consoles ─────────────────────────────────────────────────

_THEME = Theme(
    {
        "stage.reg": "bold bright_green",
        "stage.join": "bold cyan",
        "stage.appr": "bold blue",
        "stage.k12": "bold bright_green",
        "stage.check": "bold green",
        "stage.imp": "bold magenta",
        "stage.done": "bold bright_green",
        "stage.fail": "bold red",
        "bullet": "bold bright_green",
        "who": "white",
        "detail": "dim",
        "ok": "bold bright_green",
        "err": "bold red",
        "warn": "yellow",
        "pending": "dim",
        "run": "bold yellow",
    }
)

_force_terminal: bool | None = None
if os.environ.get("NO_COLOR"):
    _force_terminal = False
elif os.environ.get("FORCE_COLOR"):
    _force_terminal = True

_log_console = Console(
    stderr=True, theme=_THEME, highlight=False, force_terminal=_force_terminal
)
_out_console = Console(theme=_THEME, highlight=False, force_terminal=_force_terminal)

_STAGES: dict[str, tuple[str, str]] = {
    "REG": ("REG", "stage.reg"),
    "REGISTER": ("REG", "stage.reg"),
    "JOIN": ("JOIN", "stage.join"),
    "APPR": ("APPR", "stage.appr"),
    "APPROVE": ("APPR", "stage.appr"),
    "K12": ("K12", "stage.k12"),
    "ELEVATE": ("K12", "stage.k12"),
    "CHECK": ("CHK", "stage.check"),
    "IMP": ("IMP", "stage.imp"),
    "IMPORT": ("IMP", "stage.imp"),
    "DONE": ("DONE", "stage.done"),
    "FAIL": ("FAIL", "stage.fail"),
}

# Board column order for per-account live row
_BOARD_STEPS = ("REG", "JOIN", "APPR", "K12", "IMP")

_log = logging.getLogger("pipeline")
_board: "StatusBoard | None" = None
_board_lock = threading.Lock()

# Display style: "board" (live one-row-per-account) | "lines" (classic stream)
_style: str = "board"


def get_style() -> str:
    return _style


def set_style(style: str) -> None:
    """Set log display mode: ``board`` or ``lines``."""
    global _style
    s = str(style or "board").strip().lower()
    if s in ("line", "stream", "classic", "verbose"):
        s = "lines"
    if s in ("live", "table", "panel"):
        s = "board"
    if s not in ("board", "lines"):
        s = "board"
    _style = s


def colors_enabled() -> bool:
    if os.environ.get("NO_COLOR"):
        return False
    if os.environ.get("FORCE_COLOR"):
        return True
    try:
        return sys.stderr.isatty()
    except Exception:
        return False


def get_console() -> Console:
    return _log_console


# ── Helpers ──────────────────────────────────────────────────────────


def short_email(email: str, width: int = 28) -> str:
    email = str(email or "").strip()
    if not email or len(email) <= width:
        return email or "?"
    if "@" in email:
        local, _, domain = email.partition("@")
        keep = max(6, width - len(domain) - 4)
        if keep < len(local):
            local = local[:keep] + "…"
        out = f"{local}@{domain}"
        if len(out) <= width:
            return out
    return email[: width - 1] + "…"


def short_id(value: str, n: int = 8) -> str:
    value = str(value or "").strip()
    if len(value) <= n:
        return value
    return value[:n] + "…"


def tag(index: int, total: int, email: str = "") -> str:
    """Build ``#i/n full@email`` for board binding / log grepping.

    Prefer the full address so the live board never re-binds a truncated name.
    """
    who = str(email or "").strip()
    base = f"#{index}/{total}"
    return f"{base} {who}".rstrip()


def strip_ansi(text: str) -> str:
    return re.sub(r"\x1b\[[0-9;]*m", "", text or "")


def dim(text: str) -> str:
    return str(text)


def green(text: str) -> str:
    return str(text)


def red(text: str) -> str:
    return str(text)


def yellow(text: str) -> str:
    return str(text)


def cyan(text: str) -> str:
    return str(text)


def format_info(msg: str) -> str:
    return str(msg)


def _stage_meta(stage: str) -> tuple[str, str]:
    key = str(stage or "").strip().upper()
    return _STAGES.get(key, (key[:4], "who"))


def _normalize_stage(stage: str) -> str:
    """Map stage names onto live-board columns."""
    key = str(stage or "").strip().upper()
    mapping = {
        "REG": "REG",
        "REGISTER": "REG",
        "JOIN": "JOIN",
        "APPR": "APPR",
        "APPROVE": "APPR",
        "K12": "K12",
        "ELEVATE": "K12",
        "CHECK": "K12",
        "CHK": "K12",
        "IMP": "IMP",
        "IMPORT": "IMP",
    }
    return mapping.get(key, key)


def parse_who(who: str) -> tuple[int | None, int | None, str]:
    """Parse ``#2/3 email@x`` → (2, 3, email)."""
    who = str(who or "").strip()
    m = re.match(r"#(\d+)\s*/\s*(\d+)(?:\s+(.*))?$", who)
    if not m:
        # [Task N] style from browser registrar
        m2 = re.match(r"\[Task\s+(\d+)\](?:\s+(.*))?$", who, re.I)
        if m2:
            return int(m2.group(1)), None, (m2.group(2) or "").strip()
        return None, None, who
    return int(m.group(1)), int(m.group(2)), (m.group(3) or "").strip()


def format_milestone(stage: str, who: str = "", detail: str = "") -> str:
    label, _ = _stage_meta(stage)
    parts = ["●", label.strip()]
    if who:
        parts.append(str(who))
    if detail:
        parts.append(str(detail))
    return "  ".join(parts)


def format_fail(stage: str, who: str = "", detail: str = "") -> str:
    label, _ = _stage_meta(stage)
    parts = ["✗", label.strip()]
    if who:
        parts.append(str(who))
    if detail:
        parts.append(str(detail))
    return "  ".join(parts)


# ── Live status board ────────────────────────────────────────────────


class StatusBoard:
    """One row per account; cells flip · → … → ✓ / ✗ in place."""

    def __init__(
        self,
        total: int,
        console: Console | None = None,
        *,
        mode: str = "",
    ) -> None:
        self.total = max(1, int(total))
        self.mode = str(mode or "").strip().lower()
        self.console = console or _log_console
        self._lock = threading.Lock()
        self._rows: dict[int, dict[str, Any]] = {}
        for i in range(1, self.total + 1):
            self._rows[i] = {
                "email": "",
                "steps": {s: "pending" for s in _BOARD_STEPS},  # pending|run|ok|fail
                "note": "waiting",
            }
        self._live: Live | None = None
        self._started = False

    def start(self) -> None:
        if self._started:
            return
        self._sync_console_width()
        self._live = Live(
            self.render(),
            console=self.console,
            refresh_per_second=8,
            transient=False,
            vertical_overflow="visible",
        )
        self._live.start()
        self._started = True

    def _sync_console_width(self) -> None:
        """Align Rich console width with the real terminal (avoid 80-col trap).

        Rich only honors ``_width`` when ``_height`` is also set (see
        ``Console.size``); otherwise it falls back to the tty / 80 columns.
        """
        try:
            import shutil

            cols, rows = shutil.get_terminal_size(fallback=(0, 0))
        except Exception:
            cols, rows = 0, 0
        try:
            env_cols = int(os.environ.get("COLUMNS") or 0)
        except Exception:
            env_cols = 0
        target = max(cols, env_cols, 0)

        try:
            cur_w = int(getattr(self.console, "_width", None) or 0)
        except Exception:
            cur_w = 0
        try:
            # Prefer the larger of current / terminal so we never shrink.
            final_w = max(cur_w, target) if max(cur_w, target) >= 60 else cur_w
            if final_w >= 60:
                self.console._width = final_w  # type: ignore[attr-defined]
            # Height must be non-None or Rich ignores _width entirely.
            if getattr(self.console, "_height", None) is None:
                self.console._height = (  # type: ignore[attr-defined]
                    rows if rows and rows > 0 else 40
                )
        except Exception:
            pass

    def stop(self) -> None:
        if not self._started or self._live is None:
            return
        try:
            self._live.update(self.render())
            self._live.stop()
        except Exception:
            try:
                self._live.stop()
            except Exception:
                pass
        self._started = False
        self._live = None

    def _cell(self, state: str) -> Text:
        if state == "ok":
            return Text("✓", style="bold bright_green")
        if state == "fail":
            return Text("✗", style="bold red")
        if state == "run":
            return Text("…", style="run")
        return Text("·", style="pending")

    def _term_width(self) -> int:
        """Width used for layout — always match what Rich will actually paint."""
        self._sync_console_width()
        try:
            w = int(getattr(self.console, "width", 0) or 0)
            if w >= 40:
                return w
        except Exception:
            pass
        return 100

    def _note_style(self, note: str) -> str:
        low = note.lower()
        if "fail" in low or note.startswith("✗"):
            return "err"
        if note in ("done", "added") or note.endswith("✓"):
            return "ok"
        if (
            note.endswith("…")
            or note.endswith("...")
            or " pass " in note
            or low.startswith("wait ")
            or "waiting" in low
            or low.startswith("create ")
        ):
            return "run"
        return "detail"

    def render(self) -> Panel:
        """Left columns with comfortable spacing; status exactly fills the rest.

        Important: status column width == text clip width (no ratio stretch),
        otherwise Rich grows the cell while we still ellipsize early → blank band.
        """
        with self._lock:
            items = list(self._rows.items())

        term_w = self._term_width()
        # Panel border (│ + │ = 2) + panel horizontal padding (1+1 = 2)
        usable = max(56, term_w - 4)
        protocol = self.mode == "protocol"

        max_email = 0
        for _, row in items:
            max_email = max(max_email, len(str(row.get("email") or "").strip()))

        # Comfortable gutters without crushing narrow terminals.
        col_pad = 2 if usable >= 110 else 1  # each side of a cell
        idx_w = 3
        step_w = 5 if usable >= 110 else 4
        n_cols = 2 + len(_BOARD_STEPS) + 1  # # email steps… status
        pad_total = n_cols * (col_pad * 2)

        # Prefer a wide status: email is secondary and may ellipsize.
        email_cap = 28 if protocol else 40
        email_floor = 14
        # Give status ~55%+ of the row on typical wide terminals.
        status_floor = max(40, (usable * 55) // 100)
        fixed_no_email = (
            idx_w + step_w * len(_BOARD_STEPS) + pad_total
        )

        email_w = max(email_floor, min(max_email or email_floor, email_cap))
        if fixed_no_email + email_w + status_floor > usable:
            email_w = max(email_floor, usable - fixed_no_email - status_floor)
        email_w = max(email_floor, min(email_w, email_cap))

        # Exact remaining width — status owns everything left of the right border.
        status_w = max(12, usable - fixed_no_email - email_w)

        table = Table(
            show_header=True,
            header_style="bold",
            border_style="dim",
            expand=False,  # fixed widths; do not re-distribute and leave dead space
            box=None,
            width=usable,
            padding=(0, col_pad),
            collapse_padding=False,
            pad_edge=True,
        )
        table.add_column(
            "#",
            style="dim",
            width=idx_w,
            min_width=idx_w,
            max_width=idx_w,
            justify="right",
            no_wrap=True,
        )
        table.add_column(
            "email",
            width=email_w,
            min_width=email_w,
            max_width=email_w,
            no_wrap=True,
            overflow="ellipsis",
        )
        for step in _BOARD_STEPS:
            table.add_column(
                step,
                width=step_w,
                min_width=step_w,
                max_width=step_w,
                justify="center",
                no_wrap=True,
            )
        table.add_column(
            "status",
            width=status_w,
            min_width=status_w,
            max_width=status_w,
            overflow="ellipsis",
            no_wrap=True,
        )

        for idx, row in items:
            raw_email = str(row.get("email") or "").strip()
            email_disp = raw_email if raw_email else "…"
            email_style = "who" if raw_email else "pending"
            cells = [self._cell(row["steps"].get(s, "pending")) for s in _BOARD_STEPS]
            note = str(row.get("note") or "").replace("\n", " ").replace("\r", " ")
            style = self._note_style(note)
            # Clip to the real status width so text runs to the panel edge.
            note_txt = Text(note, style=style, no_wrap=True)
            note_txt.truncate(max(status_w, 8), overflow="ellipsis")
            table.add_row(
                str(idx),
                Text(email_disp, style=email_style, no_wrap=True, overflow="ellipsis"),
                *cells,
                note_txt,
            )

        if self.mode == "protocol":
            mode_tag = "[bold magenta]protocol[/]"
        elif self.mode == "browser":
            mode_tag = "[bold yellow]browser[/]"
        elif self.mode:
            mode_tag = f"[bold]{self.mode}[/]"
        else:
            mode_tag = ""
        if mode_tag:
            title = (
                f"[bold]pipeline[/]  [dim]{self.total} accounts ·[/] "
                f"{mode_tag} [dim]· live[/]"
            )
        else:
            title = f"[bold]pipeline[/]  [dim]{self.total} accounts · live[/]"
        return Panel(
            table,
            title=title,
            border_style="cyan",
            padding=(0, 1),
            expand=False,
            width=term_w,
        )
    def _refresh(self) -> None:
        if self._live is not None:
            try:
                self._live.update(self.render())
            except Exception:
                pass

    @staticmethod
    def _prefer_email(current: str, incoming: str) -> str:
        """Keep a full address; never overwrite with a truncated short form."""
        cur = str(current or "").strip()
        inc = str(incoming or "").strip()
        if not inc:
            return cur
        if not cur:
            return inc
        # Prefer longer (full) address; skip short_email-style replacements.
        if "…" in inc or "..." in inc:
            return cur
        if len(inc) >= len(cur):
            return inc
        # Incoming is shorter but may be full if current was wrong — only keep
        # current when it already looks complete (has @ and no ellipsis).
        if "@" in cur and "…" not in cur and "..." not in cur:
            return cur
        return inc

    def set_email(self, index: int, email: str, note: str = "") -> None:
        with self._lock:
            row = self._rows.get(index)
            if not row:
                return
            if email:
                row["email"] = self._prefer_email(row.get("email") or "", email)
            if note:
                row["note"] = note
            elif email and row.get("note") in ("", "waiting"):
                row["note"] = "queued"
        self._refresh()

    def set_status(self, index: int, note: str, *, email: str = "") -> None:
        """Update only the long status column (in-place)."""
        note = str(note or "").strip()
        with self._lock:
            row = self._rows.get(index)
            if not row:
                return
            if email:
                row["email"] = self._prefer_email(row.get("email") or "", email)
            if note:
                row["note"] = note
        self._refresh()

    def mark(
        self,
        index: int,
        stage: str,
        *,
        ok: bool = True,
        email: str = "",
        note: str = "",
    ) -> None:
        col = _normalize_stage(stage)
        with self._lock:
            row = self._rows.get(index)
            if not row:
                # Dynamic grow if total was wrong
                self._rows[index] = {
                    "email": email or "",
                    "steps": {s: "pending" for s in _BOARD_STEPS},
                    "note": "",
                }
                row = self._rows[index]
                self.total = max(self.total, index)
            if email:
                row["email"] = self._prefer_email(row.get("email") or "", email)
            if col in row["steps"]:
                row["steps"][col] = "ok" if ok else "fail"
                # Mark next pending step as running (visual cue)
                if ok:
                    try:
                        i = _BOARD_STEPS.index(col)
                        if i + 1 < len(_BOARD_STEPS):
                            nxt = _BOARD_STEPS[i + 1]
                            if row["steps"][nxt] == "pending":
                                row["steps"][nxt] = "run"
                    except ValueError:
                        pass
            if note:
                row["note"] = note
            elif ok and col == "IMP":
                row["note"] = "done"
            elif ok:
                row["note"] = {
                    "REG": "joining…",
                    "JOIN": "approving…",
                    "APPR": "elevating…",
                    "K12": "importing…",
                    "IMP": "done",
                }.get(col, row.get("note") or "")
            else:
                row["note"] = note or f"{col} failed"
        self._refresh()

    def mark_running(self, index: int, stage: str, email: str = "", note: str = "") -> None:
        col = _normalize_stage(stage)
        with self._lock:
            row = self._rows.get(index)
            if not row:
                return
            if email:
                row["email"] = self._prefer_email(row.get("email") or "", email)
            if col in row["steps"] and row["steps"][col] == "pending":
                row["steps"][col] = "run"
            if note:
                row["note"] = note
        self._refresh()


def board_start(total: int, *, mode: str = "") -> None:
    """Start the live multi-account status board (no-op in ``lines`` style).

    ``mode`` is shown in the panel title (``browser`` / ``protocol``).
    """
    global _board
    with _board_lock:
        if _board is not None:
            try:
                _board.stop()
            except Exception:
                pass
            _board = None

        # lines mode: no live board — each milestone prints a new line
        if _style == "lines":
            _board = None
            return

        _board = StatusBoard(total, console=_log_console, mode=mode)
        # Only use Live when stderr is a real terminal
        if colors_enabled() and _log_console.is_terminal:
            _board.start()
        else:
            # Fallback: no live (CI / piped) — milestones become normal lines
            _board = StatusBoard(total, console=_log_console, mode=mode)


def board_bind(index: int, email: str, *, note: str = "queued") -> None:
    """Set email on a board row as soon as the mailbox is known (before REG)."""
    email = str(email or "").strip()
    if not email:
        return
    with _board_lock:
        b = _board
    if b is None:
        # lines mode: optional quiet bind (no spam)
        return
    b.set_email(index, email, note=note)
    # Show REG as running so the row is visibly active.
    b.mark_running(index, "REG", email=email, note=note or "registering…")


def board_status(index: int, note: str, *, email: str = "") -> None:
    """Update the long status text for one account (in-place, no new log line)."""
    with _board_lock:
        b = _board
    if b is None:
        return
    b.set_status(index, note, email=email)


def board_stop() -> None:
    """Stop the live board.

    When Rich Live was active with ``transient=False``, the last frame is
    already left on screen — do **not** print again (would duplicate the table).
    Only print a final snapshot for non-TTY / non-Live fallbacks.
    """
    global _board
    with _board_lock:
        if _board is not None:
            # Capture before stop() clears _live / _started.
            was_live = bool(_board._started and _board._live is not None)
            try:
                _board.stop()
            except Exception:
                pass
            if not was_live:
                try:
                    _log_console.print(_board.render())
                except Exception:
                    pass
            _board = None


def board_active() -> bool:
    return _board is not None


# ── Logging API ──────────────────────────────────────────────────────


def get_logger(name: str = "pipeline") -> logging.Logger:
    return logging.getLogger(name)


def milestone(
    stage: str,
    who: str = "",
    detail: str = "",
    *,
    logger: logging.Logger | None = None,
) -> None:
    """Update live board row (or print one line if board is off)."""
    idx, _total, email = parse_who(who)
    col = _normalize_stage(stage)
    log = logger or _log

    with _board_lock:
        b = _board

    if b is not None and idx is not None and col in _BOARD_STEPS:
        b.mark(idx, col, ok=True, email=email, note=detail or "")
        # Also keep a compact plain log for files / non-TTY
        if not (colors_enabled() and _log_console.is_terminal):
            log.info(format_milestone(stage, who, detail))
        return

    # DONE or no board / unparsed who → normal log line
    if stage.upper() in ("DONE", "FAIL") or b is None or idx is None:
        text = _milestone_text(stage, who, detail)
        log.info(format_milestone(stage, who, detail), extra={"rich_renderable": text})
        return

    log.info(format_milestone(stage, who, detail))


def fail(
    stage: str,
    who: str = "",
    detail: str = "",
    *,
    logger: logging.Logger | None = None,
) -> None:
    idx, _total, email = parse_who(who)
    col = _normalize_stage(stage)
    log = logger or _log

    with _board_lock:
        b = _board

    if b is not None and idx is not None and col in _BOARD_STEPS:
        b.mark(idx, col, ok=False, email=email, note=detail or f"{col} failed")
        if not (colors_enabled() and _log_console.is_terminal):
            log.warning(format_fail(stage, who, detail))
        return

    text = _fail_text(stage, who, detail)
    log.warning(format_fail(stage, who, detail), extra={"rich_renderable": text})


def _milestone_text(stage: str, who: str = "", detail: str = "") -> Text:
    label, style = _stage_meta(stage)
    t = Text()
    t.append("● ", style="bullet")
    t.append(f"{label}", style=style)
    t.append("  ")
    if who:
        t.append(str(who), style="who")
    if detail:
        if who:
            t.append("  ")
        t.append(str(detail), style="detail")
    return t


def _fail_text(stage: str, who: str = "", detail: str = "") -> Text:
    label, _ = _stage_meta(stage)
    t = Text()
    t.append("✗ ", style="err")
    t.append(f"{label}", style="stage.fail")
    t.append("  ")
    if who:
        t.append(str(who), style="who")
    if detail:
        if who:
            t.append("  ")
        t.append(str(detail), style="detail")
    return t


def info(msg: str, *, logger: logging.Logger | None = None) -> None:
    (logger or _log).info(str(msg))


def warn(msg: str, *, logger: logging.Logger | None = None) -> None:
    (logger or _log).warning(str(msg))


def error(msg: str, *, logger: logging.Logger | None = None) -> None:
    (logger or _log).error(str(msg))


# ── Rich handler ─────────────────────────────────────────────────────


class BoardRichHandler(RichHandler):
    def emit(self, record: logging.LogRecord) -> None:
        # While live board is running, skip non-milestone chatter on console
        # (still goes to file handlers on root). In ``lines`` style, show all.
        renderable = getattr(record, "rich_renderable", None)
        with _board_lock:
            live_on = (
                _style == "board"
                and _board is not None
                and _board._started
                and colors_enabled()
                and _log_console.is_terminal
            )
        if live_on and renderable is None:
            # Drop noisy INFO to keep the board clean; keep WARNING+
            if record.levelno < logging.WARNING:
                return
        if renderable is not None:
            try:
                from datetime import datetime

                ts = datetime.fromtimestamp(record.created).strftime("%H:%M:%S")
                line = Text()
                line.append(f"{ts}  ", style="dim")
                if isinstance(renderable, Text):
                    line.append_text(renderable)
                else:
                    line.append(str(renderable))
                self.console.print(line)
                return
            except Exception:
                pass
        super().emit(record)


def setup(
    level: str | int = "INFO",
    log_file: str = "",
    *,
    verbose: bool = False,
    style: str | None = None,
) -> None:
    """Configure logging.

    ``style``: ``board`` (live table, default) | ``lines`` (classic stream).
    Also: env ``PIPELINE_LOG_STYLE`` / ``LOG_STYLE``.
    """
    global _log_console, _out_console, _force_terminal

    if os.environ.get("NO_COLOR"):
        _force_terminal = False
    elif os.environ.get("FORCE_COLOR"):
        _force_terminal = True
    else:
        _force_terminal = None

    env_style = (
        os.environ.get("PIPELINE_LOG_STYLE")
        or os.environ.get("LOG_STYLE")
        or ""
    ).strip()
    set_style(style if style is not None else (env_style or "board"))

    if isinstance(level, str):
        level_name = "DEBUG" if verbose else level.upper()
        level_val = getattr(logging, level_name, logging.INFO)
    else:
        level_val = level

    console = Console(
        stderr=True,
        theme=_THEME,
        highlight=False,
        force_terminal=_force_terminal,
    )
    _log_console = console
    _out_console = Console(
        theme=_THEME, highlight=False, force_terminal=_force_terminal
    )

    root = logging.getLogger()
    root.handlers.clear()
    root.setLevel(level_val)

    handler = BoardRichHandler(
        console=console,
        show_time=True,
        show_level=(_style == "lines"),
        show_path=False,
        rich_tracebacks=True,
        tracebacks_show_locals=False,
        markup=False,
        log_time_format="[%X]",
        omit_repeated_times=False,
    )
    handler.setLevel(level_val)
    handler.setFormatter(logging.Formatter("%(message)s"))
    root.addHandler(handler)

    path = str(log_file or "").strip()
    if path:
        fh = logging.FileHandler(path, encoding="utf-8")
        fh.setLevel(level_val)
        fh.setFormatter(
            logging.Formatter(
                "%(asctime)s [%(levelname)-5s] %(message)s",
                datefmt="%H:%M:%S",
            )
        )
        root.addHandler(fh)


# ── Summary ──────────────────────────────────────────────────────────


def summary_panel(summary: dict[str, Any]) -> Panel:
    reg = summary.get("registered", 0)
    join = summary.get("joined", 0)
    k12 = summary.get("refreshed", 0)
    elapsed = summary.get("elapsed_seconds", 0)
    avg = summary.get("avg_seconds", 0)
    interrupted = bool(summary.get("interrupted"))
    imp = summary.get("import") or {}
    imp_ok = imp.get("ok", 0)
    imp_total = imp.get("total", reg)
    imp_fail = imp.get("failed", 0)

    table = Table.grid(padding=(0, 2))
    table.add_column(style="bold dim", justify="right")
    table.add_column()
    table.add_row("registered", f"[bold]{reg}[/]")
    table.add_row("joined", f"[bold]{join}[/]")
    table.add_row("k12", f"[bold bright_green]{k12}[/]")
    imp_txt = f"{imp_ok}/{imp_total} ok"
    if imp_fail:
        imp_txt += f"  [red]failed={imp_fail}[/]"
    table.add_row("import", imp_txt)
    table.add_row("elapsed", f"{elapsed}s  (avg {avg}s/account)")
    af = summary.get("accounts_file")
    atf = summary.get("access_tokens_file")
    if af:
        table.add_row("accounts", str(af))
    if atf:
        table.add_row("tokens", str(atf))

    title = "[err]INTERRUPTED[/]" if interrupted else "[ok]COMPLETE[/]"
    border = "red" if interrupted else "bright_green"
    return Panel(table, title=title, border_style=border, padding=(0, 1))


def accounts_table(rows: list[dict[str, Any]]) -> Table:
    table = Table(
        title="per-account",
        show_header=True,
        header_style="bold",
        border_style="dim",
        expand=False,
    )
    table.add_column("#", style="dim", justify="right", width=3)
    table.add_column("email", min_width=22, overflow="ellipsis")
    table.add_column("join", width=6, justify="center")
    table.add_column("plan", width=8, justify="center")
    table.add_column("token", width=5, justify="center")
    table.add_column("import", width=6, justify="center")

    for row in sorted(rows, key=lambda r: r.get("index", 0)):
        join = str(row.get("join") or "-")
        plan = str(row.get("plan") or "-")
        token_ok = bool(row.get("has_token"))
        imported = row.get("imported")
        join_cell = Text("✓", style="green") if join == "ok" else Text(join[:6], style="yellow")
        plan_cell = (
            Text(plan, style="bold bright_green")
            if plan.lower() == "k12"
            else Text(plan, style="dim")
        )
        tok_cell = Text("✓", style="green") if token_ok else Text("✗", style="red")
        if imported is True:
            imp_cell = Text("✓", style="green")
        elif imported is False:
            imp_cell = Text("✗", style="red")
        else:
            imp_cell = Text("·", style="dim")
        table.add_row(
            str(row.get("index", "?")),
            short_email(str(row.get("email") or ""), 28),
            join_cell,
            plan_cell,
            tok_cell,
            imp_cell,
        )
    return table


def print_run_summary(summary: dict[str, Any]) -> None:
    board_stop()  # freeze live board before final card
    _out_console.print()
    _out_console.print(summary_panel(summary))
    rows = summary.get("per_account") or []
    if rows:
        _out_console.print()
        _out_console.print(accounts_table(rows))
    _out_console.print()


def summary_box(summary: dict[str, Any]) -> str:
    with _out_console.capture() as cap:
        _out_console.print(summary_panel(summary))
    return cap.get()


def format_account_row(row: dict[str, Any]) -> str:
    join = str(row.get("join") or "-")
    plan = str(row.get("plan") or "-")
    token_ok = bool(row.get("has_token"))
    imported = row.get("imported")
    join_s = "join✓" if join == "ok" else f"join={join}"
    tok_s = "token✓" if token_ok else "token✗"
    if imported is True:
        imp_s = " import✓"
    elif imported is False:
        imp_s = " import✗"
    else:
        imp_s = ""
    return (
        f"  #{row.get('index', '?'):<3} "
        f"{short_email(str(row.get('email') or ''), 28):<30} "
        f"{join_s:<8} plan={plan:<6} {tok_s}{imp_s}"
    )
