from __future__ import annotations

import importlib.util
import json
import os
import pwd
import sqlite3
import sys
from contextlib import closing
from functools import cache
from importlib.machinery import SourceFileLoader
from pathlib import Path
from types import ModuleType

from captain_hook import BaseHookEvent

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


def recorded_artifact() -> str | None:
    descriptor = BIN.with_name(f"{BIN.name}.binrun")
    record = real_home() / ".daemonkit" / "binrun" / "resolved" / str(descriptor).translate(RECORD_NAME_ESCAPES)
    try:
        if int(record.stat().st_mtime) <= int(descriptor.stat().st_mtime):
            return None
        target = record.read_text().strip()
    except OSError:
        return None
    return target if os.access(target, os.X_OK) else None


def call_bin(evt: BaseHookEvent, sub: str, *, timeout: int = 10) -> str | None:
    if daemon_is_down() or not BIN.exists():
        return None
    artifact = recorded_artifact()
    try:
        return evt.ctx.call_cli(
            [artifact or str(BIN), sub],
            input=json.dumps(evt._raw),
            timeout=timeout,
            env={"BINRUN_PLUGIN_ROOT": str(BIN.parent.parent)} if artifact else None,
            throw=False,
        )
    except UnicodeDecodeError:
        return None


@cache
def check_post() -> ModuleType:
    loader = SourceFileLoader("check_post", str(PLUGIN_ROOT / "skills" / "slack" / "scripts" / "check-post"))
    module = importlib.util.module_from_spec(importlib.util.spec_from_loader(loader.name, loader))
    sys.modules[loader.name] = module
    loader.exec_module(module)
    return module
