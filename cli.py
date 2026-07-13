"""CLI entry point for chatgpt-register-sub2api.

Subcommands:
  init             Write default config.toml
  register         Register N ChatGPT accounts
  join-workspace   Join registered accounts to K12 workspace
  login-team       Re-login with Team space selection
  export           Export to sub2api JSON
  run              Full pipeline (register → join → login → export)
"""

from __future__ import annotations

import argparse
import logging
import sys
from pathlib import Path

from version import __version__
from config import (
    DEFAULT_CONFIG_FILE,
    generate_default_config,
    load_config,
)
from pipeline import (
    load_accounts,
    run_export,
    run_export_access_tokens,
    run_full_pipeline,
    run_import_access_tokens,
    run_join_workspace,
    run_re_login,
    run_refresh_tokens,
    run_register,
    save_accounts,
)


def setup_logging(
    config: dict,
    verbose: bool = False,
    log_style: str | None = None,
) -> None:
    """Configure board-style logging via ``utils.logger``.

    Display modes (``logging.style`` / ``--log-style`` / env)::

      board  — live table, one row per account (default)
      lines  — classic stream, one line per step
    """
    import warnings

    from utils import logger as plog

    # Local HTTP proxies often trip urllib3 cert warnings and clutter the board.
    warnings.filterwarnings("ignore", message="Unverified HTTPS request")

    log_cfg = config.get("logging", {})
    style = log_style or str(log_cfg.get("style", "") or "").strip() or None
    plog.setup(
        level=str(log_cfg.get("level", "INFO")),
        log_file=str(log_cfg.get("file", "") or "").strip(),
        verbose=verbose,
        style=style,
    )

    config_file = str(config.get("_config_file") or "").strip()
    mode = str((config.get("registration") or {}).get("mode") or "protocol").strip()
    if config_file:
        logging.getLogger(__name__).info(f"Config file={config_file} mode={mode}")


def cmd_init(args) -> int:
    """Write default config.toml."""
    path = Path(args.config) if args.config else DEFAULT_CONFIG_FILE
    try:
        output = generate_default_config(path)
        print(f"Config written to {output}")
        return 0
    except FileExistsError as e:
        print(f"Error: {e}", file=sys.stderr)
        return 1


def cmd_register(args) -> int:
    """Register N ChatGPT accounts."""
    config = load_config(args.config)
    setup_logging(config, args.verbose, log_style=getattr(args, "log_style", None))
    logger = logging.getLogger(__name__)

    config_dir = Path(config.get("_config_dir", "."))
    accounts_file = config_dir / "registered_accounts.json"

    results = run_register(
        config=config,
        accounts_file=accounts_file,
        count=args.count,
    )

    # Board mode already prints a summary card; keep a short list either way.
    print(f"\nRegistered: {len(results)} accounts")
    for acc in results:
        print(f"  {acc['email']}")
    return 0 if results else 1


def cmd_join_workspace(args) -> int:
    """Join registered accounts to K12 workspace."""
    config = load_config(args.config)
    setup_logging(config, args.verbose, log_style=getattr(args, "log_style", None))
    logger = logging.getLogger(__name__)

    # Override workspace IDs from CLI
    if args.workspace_id:
        config.setdefault("workspace", {})["ids"] = args.workspace_id

    config_dir = Path(config.get("_config_dir", "."))
    input_file = Path(args.input) if args.input else config_dir / "registered_accounts.json"
    accounts = load_accounts(input_file)

    if not accounts:
        print(f"No accounts found in {input_file}", file=sys.stderr)
        return 1

    accounts = run_join_workspace(config, accounts)
    save_accounts(input_file, accounts)

    joined = sum(1 for a in accounts if a.get("join_status") == "ok")
    print(f"\nJoined: {joined}/{len(accounts)} accounts")
    return 0


def cmd_login_team(args) -> int:
    """Re-login with Team space selection."""
    config = load_config(args.config)
    setup_logging(config, args.verbose, log_style=getattr(args, "log_style", None))
    logger = logging.getLogger(__name__)

    config_dir = Path(config.get("_config_dir", "."))
    input_file = Path(args.input) if args.input else config_dir / "registered_accounts.json"
    accounts = load_accounts(input_file)

    if not accounts:
        print(f"No accounts found in {input_file}", file=sys.stderr)
        return 1

    accounts = run_re_login(config, accounts)
    save_accounts(input_file, accounts)

    team_logged = sum(1 for a in accounts if a.get("team_login_status") == "ok")
    print(f"\nTeam logged: {team_logged}/{len(accounts)} accounts")
    return 0


def cmd_export(args) -> int:
    """Export to sub2api JSON."""
    config = load_config(args.config)
    setup_logging(config, args.verbose, log_style=getattr(args, "log_style", None))
    logger = logging.getLogger(__name__)

    config_dir = Path(config.get("_config_dir", "."))
    input_file = Path(args.input) if args.input else config_dir / "registered_accounts.json"
    accounts = load_accounts(input_file)

    if not accounts:
        print(f"No accounts found in {input_file}", file=sys.stderr)
        return 1

    output_file = Path(args.output) if args.output else None
    json_str = run_export(config, accounts, output_file)

    if args.stdout or not output_file:
        print(json_str)
    else:
        print(f"Exported to {output_file}")

    return 0


def cmd_import(args) -> int:
    """Push access tokens to the external admin API."""
    config = load_config(args.config)
    setup_logging(config, args.verbose, log_style=getattr(args, "log_style", None))

    config_dir = Path(config.get("_config_dir", "."))
    input_file = Path(args.input) if args.input else config_dir / "registered_accounts.json"

    if not input_file.exists():
        print(f"No input file found: {input_file}", file=sys.stderr)
        return 1

    # .txt → one access_token per line; otherwise the accounts JSON store
    if input_file.suffix.lower() == ".txt":
        tokens = [
            ln.strip()
            for ln in input_file.read_text(encoding="utf-8").splitlines()
            if ln.strip()
        ]
        accounts = [{"access_token": t} for t in tokens]
    else:
        accounts = load_accounts(input_file)

    if not accounts:
        print(f"No accounts/tokens found in {input_file}", file=sys.stderr)
        return 1

    summary = run_import_access_tokens(config, accounts)

    print(f"\nImported: {summary.get('ok', 0)}/{summary.get('total', 0)} ok "
          f"(added={summary.get('added', 0)}, updated={summary.get('updated', 0)}, "
          f"duplicate={summary.get('duplicate', 0)}, "
          f"failed={summary.get('failed', 0)}, skipped={summary.get('skipped', 0)})")
    return 0 if summary.get("failed", 0) == 0 and summary.get("ok", 0) > 0 else 1


def cmd_refresh(args) -> int:
    """Refresh tokens + elevate to K12 workspace for existing accounts.

    Use after join/approve when plan is still free, or to re-mint ATs.
    """
    config = load_config(args.config)
    setup_logging(config, args.verbose, log_style=getattr(args, "log_style", None))

    config_dir = Path(config.get("_config_dir", "."))
    input_file = Path(args.input) if args.input else config_dir / "registered_accounts.json"
    accounts = load_accounts(input_file)
    if not accounts:
        print(f"No accounts found in {input_file}", file=sys.stderr)
        return 1

    accounts = run_refresh_tokens(config, accounts)
    save_accounts(input_file, accounts)
    at_path = run_export_access_tokens(config, accounts)

    k12 = sum(1 for a in accounts if str(a.get("plan_type") or "").lower() == "k12")
    print(f"\nRefreshed: {len(accounts)} accounts | plan=k12: {k12}")
    print(f"Access tokens: {at_path}")
    return 0


def cmd_run(args) -> int:
    """Run the full pipeline (default when no subcommand is given).

    Everything (account count, threads, workspace IDs, import target …)
    comes from the config file. The optional CLI flags below just override.
    """
    config = load_config(args.config)
    setup_logging(
        config,
        getattr(args, "verbose", False),
        log_style=getattr(args, "log_style", None),
    )

    # Optional CLI overrides (all default to config values when absent)
    workspace_id = getattr(args, "workspace_id", None)
    if workspace_id:
        config.setdefault("workspace", {})["ids"] = workspace_id

    from utils import logger as plog

    try:
        summary = run_full_pipeline(
            config=config,
            count=getattr(args, "count", None),
            output_file=getattr(args, "output", None),
            accounts_file=getattr(args, "accounts", None),
        )
    except KeyboardInterrupt:
        try:
            plog.board_stop()
        except Exception:
            pass
        print(
            "\nInterrupted (Ctrl+C). Partial results kept in "
            "registered_accounts.json / access_token.txt",
            file=sys.stderr,
        )
        # Avoid hanging on Playwright / thread-pool atexit joins.
        try:
            from pipeline import _flush_and_force_exit

            _flush_and_force_exit(130)
        except Exception:
            import os

            os._exit(130)

    plog.print_run_summary(summary)

    if summary.get("interrupted"):
        print(
            "Interrupted — partial results saved. Exiting "
            "(orphan browsers may close with the process).",
            file=sys.stderr,
        )
        # force_exit: skip non-daemon thread joins at interpreter shutdown
        if summary.get("force_exit", True):
            try:
                from pipeline import _flush_and_force_exit

                _flush_and_force_exit(130)
            except Exception:
                import os

                os._exit(130)
        return 130
    return 0 if summary.get("registered", 0) > 0 else 1


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(
        prog="chatgpt-register",
        description="ChatGPT 账号注册 + K12 母号加入 + 导出/导入 access token"
                    "（无子命令时直接按 config.toml 运行完整流水线）",
    )
    parser.add_argument(
        "--version", action="version", version=f"%(prog)s {__version__}"
    )
    parser.add_argument("--config", "-c", default=None, help="Config file path (default: config.toml)")
    parser.add_argument("--verbose", "-v", action="store_true", help="Verbose output")
    # Full-pipeline overrides usable on the bare command (e.g. `chatgpt-register -n 3`).
    parser.add_argument("--count", "-n", type=int, default=None, help="Number of accounts (override registration.total)")
    parser.add_argument("--workspace-id", action="append", default=None, help="Workspace UUID (repeatable)")
    parser.add_argument("--output", "-o", default=None, help="Output sub2api JSON file")
    parser.add_argument("--accounts", default=None, help="Accounts store JSON file")
    parser.add_argument(
        "--log-style",
        choices=("board", "lines"),
        default=None,
        help="Log display: board=live table (default), lines=classic stream",
    )
    sub = parser.add_subparsers(dest="command", help="Available commands (optional)")

    # Subcommand flags that also exist on the parent MUST use SUPPRESS.
    # Otherwise `chatgpt-register -c foo.toml register` is clobbered to
    # config=None by the subparser default, and load_config falls back to
    # config.toml — silent wrong-config bug.

    # ── init ──
    p_init = sub.add_parser("init", help="Write default config.toml")
    p_init.add_argument(
        "--config", "-c", default=argparse.SUPPRESS, help="Config file path"
    )
    p_init.set_defaults(func=cmd_init)

    # ── register ──
    p_reg = sub.add_parser("register", help="Register ChatGPT accounts")
    p_reg.add_argument(
        "--config", "-c", default=argparse.SUPPRESS, help="Config file path"
    )
    p_reg.add_argument(
        "--verbose",
        "-v",
        action="store_true",
        default=argparse.SUPPRESS,
        help="Verbose output",
    )
    p_reg.add_argument(
        "--count",
        "-n",
        type=int,
        default=argparse.SUPPRESS,
        help="Number of accounts",
    )
    p_reg.add_argument(
        "--log-style",
        choices=("board", "lines"),
        default=argparse.SUPPRESS,
        help="Log display: board=live table, lines=classic stream",
    )
    p_reg.set_defaults(func=cmd_register)

    # ── join-workspace ──
    p_join = sub.add_parser("join-workspace", help="Join workspace")
    p_join.add_argument(
        "--config", "-c", default=argparse.SUPPRESS, help="Config file path"
    )
    p_join.add_argument(
        "--verbose",
        "-v",
        action="store_true",
        default=argparse.SUPPRESS,
        help="Verbose output",
    )
    p_join.add_argument(
        "--workspace-id",
        action="append",
        default=argparse.SUPPRESS,
        help="Workspace UUID (repeatable)",
    )
    p_join.add_argument("--input", "-i", default=None, help="Input accounts JSON")
    p_join.set_defaults(func=cmd_join_workspace)

    # ── login-team ──
    p_login = sub.add_parser("login-team", help="Re-login with Team space")
    p_login.add_argument(
        "--config", "-c", default=argparse.SUPPRESS, help="Config file path"
    )
    p_login.add_argument(
        "--verbose",
        "-v",
        action="store_true",
        default=argparse.SUPPRESS,
        help="Verbose output",
    )
    p_login.add_argument("--input", "-i", default=None, help="Input accounts JSON")
    p_login.set_defaults(func=cmd_login_team)

    # ── export ──
    p_export = sub.add_parser("export", help="Export sub2api JSON")
    p_export.add_argument(
        "--config", "-c", default=argparse.SUPPRESS, help="Config file path"
    )
    p_export.add_argument(
        "--verbose",
        "-v",
        action="store_true",
        default=argparse.SUPPRESS,
        help="Verbose output",
    )
    p_export.add_argument(
        "--output", "-o", default=argparse.SUPPRESS, help="Output file path"
    )
    p_export.add_argument("--input", "-i", default=None, help="Input accounts JSON")
    p_export.add_argument("--stdout", action="store_true", help="Print to stdout")
    p_export.set_defaults(func=cmd_export)

    # ── import-at ──
    p_import = sub.add_parser("import-at", help="Push access tokens to admin API")
    p_import.add_argument(
        "--config", "-c", default=argparse.SUPPRESS, help="Config file path"
    )
    p_import.add_argument(
        "--verbose",
        "-v",
        action="store_true",
        default=argparse.SUPPRESS,
        help="Verbose output",
    )
    p_import.add_argument(
        "--input", "-i", default=None, help="Accounts JSON or tokens .txt"
    )
    p_import.set_defaults(func=cmd_import)

    # ── refresh ──
    p_ref = sub.add_parser(
        "refresh",
        help="Refresh tokens + elevate to K12 (for already registered accounts)",
    )
    p_ref.add_argument(
        "--config", "-c", default=argparse.SUPPRESS, help="Config file path"
    )
    p_ref.add_argument(
        "--verbose",
        "-v",
        action="store_true",
        default=argparse.SUPPRESS,
        help="Verbose output",
    )
    p_ref.add_argument("--input", "-i", default=None, help="Input accounts JSON")
    p_ref.set_defaults(func=cmd_refresh)

    # ── run ──
    # These mirror the top-level pipeline overrides. Using SUPPRESS means a flag
    # absent after `run` won't clobber a value already given before it.
    p_run = sub.add_parser("run", help="Full pipeline (same as bare command)")
    p_run.add_argument(
        "--config", "-c", default=argparse.SUPPRESS, help="Config file path"
    )
    p_run.add_argument(
        "--verbose",
        "-v",
        action="store_true",
        default=argparse.SUPPRESS,
        help="Verbose output",
    )
    p_run.add_argument(
        "--count",
        "-n",
        type=int,
        default=argparse.SUPPRESS,
        help="Number of accounts",
    )
    p_run.add_argument(
        "--workspace-id",
        action="append",
        default=argparse.SUPPRESS,
        help="Workspace UUID (repeatable)",
    )
    p_run.add_argument(
        "--output", "-o", default=argparse.SUPPRESS, help="Output sub2api JSON file"
    )
    p_run.add_argument(
        "--accounts", default=argparse.SUPPRESS, help="Accounts store JSON file"
    )
    p_run.add_argument(
        "--log-style",
        choices=("board", "lines"),
        default=argparse.SUPPRESS,
        help="Log display: board=live table, lines=classic stream",
    )
    p_run.set_defaults(func=cmd_run)

    args = parser.parse_args(argv)
    if not args.command:
        # Bare `chatgpt-register` → run the full pipeline from config.toml
        args.func = cmd_run

    sys.exit(args.func(args) or 0)


if __name__ == "__main__":
    main()
