"""Configuration loader for chatgpt-register.

Loads a single TOML file (default ``config.toml``).
Reading uses the stdlib ``tomllib`` (Python 3.11+).
"""

from __future__ import annotations

import copy
import logging
import tomllib
from pathlib import Path
from typing import Any

logger = logging.getLogger(__name__)

DEFAULT_CONFIG_FILE = Path("config.toml")

DEFAULT_CONFIG: dict[str, Any] = {
    "mail": {
        "providers": [
            {
                "type": "outlook_token",
                "enable": True,
                "label": "Outlook Pool",
                "mode": "graph",
                "mailboxes_file": "outlook.txt",
            },
        ],
        "request_timeout": 30,
        "wait_timeout": 30,
        "wait_interval": 2,
    },
    "proxy": {
        "url": "",
        "proxies_file": "",
        "default_protocol": "socks5",
        "flaresolverr_url": "",
    },
    "registration": {
        "mode": "protocol",
        "threads": 1,
        "total": 10,
        "allow_high_browser_concurrency": False,
        # reg | full | full_success — see config.toml
        "pipeline_gate": "reg",
    },
    "workspace": {
        "enabled": True,
        "ids": [],
        "route": "request",
        "max_retries": 3,
        "retry_backoff_ms": 5000,
        "approve_requests": True,
        "approve_max_attempts": 6,
        "manager_session_file": "session.json",
        "elevate_timeout_s": 90,
        "elevate_max_passes": 2,
        "elevate_tls_retries": 2,
        "elevate_request_timeout": 20,
    },
    "sub2api": {
        "enabled": False,
        "output_file": "sub2api_bundle.json",
    },
    "import_api": {
        "enabled": False,
        "url": "http://127.0.0.1:2003",
        "admin_key": "<admin_secret>",
        "require_k12": True,
    },
    "logging": {
        "level": "INFO",
        "style": "board",
        "file": "",
    },
}


DEFAULT_CONFIG_TOML = """\
# ============================================
# 协议注册配置 (curl_cffi + Sentinel)
# ============================================
#   uv run chatgpt-register -n 3
#   uv run chatgpt-register register -n 1
# ============================================

[mail]
request_timeout = 30
wait_timeout = 45
wait_interval = 1.5

[[mail.providers]]
type = "outlook_token"
enable = true
label = "Outlook Pool"
mode = "graph"
imap_host = "outlook.office365.com"
message_limit = 10
mailboxes_file = "outlook.txt"
graph_scope = "https://graph.microsoft.com/.default"
alias_count = 5

[proxy]
url = ""
proxies_file = "proxies.txt"
default_protocol = "socks5"
flaresolverr_url = ""

[registration]
mode = "protocol"
threads = 3
total = 1

[workspace]
enabled = true
ids = []
route = "request"
max_retries = 3
retry_backoff_ms = 5000
approve_requests = true
approve_max_attempts = 12
manager_session_file = "session.json"

[sub2api]
enabled = false
output_file = "sub2api_bundle.json"

[import_api]
enabled = false
url = "http://127.0.0.1:2003"
admin_key = "<admin_secret>"

[logging]
level = "INFO"
style = "board"
file = ""
"""


def _read_toml(path: Path) -> dict[str, Any]:
    if not path.exists():
        return {}
    try:
        with open(path, "rb") as f:
            raw = tomllib.load(f)
        return raw if isinstance(raw, dict) else {}
    except Exception as e:
        raise ValueError(f"Failed to parse {path}: {e}") from e


def _deep_merge(base: dict, overlay: dict) -> None:
    """Merge overlay into base in-place, recursively."""
    for key, value in overlay.items():
        if key in base and isinstance(base[key], dict) and isinstance(value, dict):
            _deep_merge(base[key], value)
        else:
            base[key] = value


def load_config(path: str | Path | None = None) -> dict[str, Any]:
    """Load config.toml (or ``-c path``).

    Returns:
        Merged config dict with meta keys ``_config_dir``, ``_config_file``.
    """
    config_file = Path(path) if path else DEFAULT_CONFIG_FILE
    config_file = config_file.resolve() if config_file.exists() else config_file
    base_dir = (
        config_file.parent.resolve()
        if config_file.exists()
        else Path.cwd().resolve()
    )

    config: dict[str, Any] = copy.deepcopy(DEFAULT_CONFIG)
    entry_raw = _read_toml(config_file) if config_file.exists() else {}
    if entry_raw:
        _deep_merge(config, entry_raw)

    config["_config_dir"] = str(base_dir)
    config["_config_file"] = str(config_file if config_file.exists() else config_file)

    mode = str((config.get("registration") or {}).get("mode") or "protocol")
    logger.debug(
        "config loaded file=%s mode=%s",
        config.get("_config_file"),
        mode,
    )
    return config


def generate_default_config(path: str | Path = DEFAULT_CONFIG_FILE) -> Path:
    """Write the default config.toml to disk."""
    output = Path(path).resolve()
    if output.exists():
        raise FileExistsError(f"{output} already exists — not overwriting")
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(DEFAULT_CONFIG_TOML, encoding="utf-8")
    return output


def get_output_dir(config: dict[str, Any], cli_output_dir: str = "") -> Path:
    """Determine the output directory for data files.

    Priority: CLI arg > config _config_dir > cwd
    """
    if cli_output_dir:
        return Path(cli_output_dir).resolve()
    config_dir = config.get("_config_dir", "")
    if config_dir:
        return Path(config_dir)
    return Path.cwd()
