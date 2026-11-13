from __future__ import annotations

import importlib.util
import json
import os
import pwd
import re
import sqlite3
import subprocess
import sys
from contextlib import closing
from functools import cache
from importlib.machinery import SourceFileLoader
from pathlib import Path
from types import ModuleType

from captain_hook import BaseHookEvent
from captain_hook.util import reqenv

__capt_hook_skip__ = True

PLUGIN_ROOT = Path(__file__).resolve().parents[2]
BIN = PLUGIN_ROOT / "bin" / "cc-slack"
SERVICE_LABEL = "com.yasyf.cc-slack"
APP_DIR = Path(".local", "state", "cc-slack")
SCOPE = "cc-slack"
PENDING_DIRECTIVE = (
    "SELECT EXISTS(SELECT 1 FROM directives JOIN subjects ON subjects.id = directives.subject_id "
    "WHERE directives.agent_id = ? AND directives.delivered_at IS NULL AND subjects.scope = ?)"
)
SUBJECT_IN_SCOPE = "SELECT EXISTS(SELECT 1 FROM subjects WHERE scope = ?)"
RECORD_NAME_ESCAPES = str.maketrans({"%": "%25", "/": "%2F"})
PLUGIN_VERSION = re.compile(r"\d+(?:\.\d+)*")


def real_home() -> Path:
    return Path(os.environ.get("DAEMONKIT_HOME") or pwd.getpwuid(os.getuid()).pw_dir)


def daemon_socket() -> Path:
    return real_home() / ".daemonkit" / "a" / SERVICE_LABEL / "daemon.sock"


def state_db() -> Path:
    return real_home() / APP_DIR / "cc-interact-v1" / "state.db"


def daemon_is_down() -> bool:
    socket = daemon_socket()
    return socket.parent.is_dir() and not socket.exists()


def plane_may_match(query: str, *params: str) -> bool:
    try:
        with closing(sqlite3.connect(f"{state_db().as_uri()}?mode=ro", uri=True, timeout=0)) as conn:
            return bool(conn.execute(query, params).fetchone()[0])
    except sqlite3.DatabaseError:
        return True


def directive_pending(evt: BaseHookEvent) -> bool:
    return plane_may_match(PENDING_DIRECTIVE, evt._raw.get("agent_id") or "", SCOPE)


def subject_in_scope() -> bool:
    return plane_may_match(SUBJECT_IN_SCOPE, SCOPE)


def plugin_version(root: Path) -> tuple[int, ...] | None:
    return tuple(map(int, root.name.split("."))) if PLUGIN_VERSION.fullmatch(root.name) else None


def newer_plugin_exists() -> bool:
    if (current := plugin_version(PLUGIN_ROOT)) is None:
        return False
    return any(
        (version := plugin_version(sibling)) is not None
        and version > current
        and not (sibling / ".orphaned_at").exists()
        and os.access(sibling / "bin" / BIN.name, os.X_OK)
        for sibling in PLUGIN_ROOT.parent.iterdir()
    )


def recorded_artifact() -> str | None:
    if reqenv.getenv("BINRUN_BIN") or newer_plugin_exists():
        return None
    descriptor = BIN.with_name(f"{BIN.name}.binrun")
    record = real_home() / ".daemonkit" / "binrun" / "resolved" / str(descriptor).translate(RECORD_NAME_ESCAPES)
    try:
        if int(record.stat().st_mtime) <= int(descriptor.stat().st_mtime):
            return None
        target = record.read_text().strip()
    except (OSError, UnicodeDecodeError):
        return None
    return target if os.access(target, os.X_OK) else None


def call_bin(evt: BaseHookEvent, sub: str, *, timeout: int = 10) -> str | None:
    if daemon_is_down() or not BIN.exists():
        return None
    payload = json.dumps(evt._raw)
    try:
        if artifact := recorded_artifact():
            try:
                return evt.ctx.call_cli(
                    [artifact, sub],
                    input=payload,
                    timeout=timeout,
                    env={"BINRUN_PLUGIN_ROOT": str(PLUGIN_ROOT)},
                )
            except OSError:
                pass
            except subprocess.SubprocessError:
                return None
        return evt.ctx.call_cli([str(BIN), sub], input=payload, timeout=timeout, throw=False)
    except UnicodeDecodeError:
        return None


@cache
def check_post() -> ModuleType:
    loader = SourceFileLoader("check_post", str(PLUGIN_ROOT / "skills" / "slack" / "scripts" / "check-post"))
    module = importlib.util.module_from_spec(importlib.util.spec_from_loader(loader.name, loader))
    sys.modules[loader.name] = module
    loader.exec_module(module)
    return module
