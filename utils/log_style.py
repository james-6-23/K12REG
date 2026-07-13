"""Deprecated — use ``from utils import logger`` (Rich-powered) instead."""

from __future__ import annotations

from utils.logger import (  # noqa: F401
    cyan as step,
    format_milestone,
    green,
    red as err,
    yellow as warn,
)


def ok(msg: str) -> str:
    from utils.logger import colors_enabled, green as _g

    text = f"✓ {msg}"
    return _g(text) if colors_enabled() else text


def milestone(tag: str, what: str) -> str:
    return format_milestone("DONE", tag, what)
