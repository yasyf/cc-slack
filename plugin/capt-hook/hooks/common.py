from __future__ import annotations

import json
import os
import pwd
import sqlite3
from contextlib import closing
from pathlib import Path

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


def call_bin(evt: BaseHookEvent, sub: str, *, timeout: int = 10) -> str | None:
    if daemon_is_down() or not BIN.exists():
        return None
    try:
        return evt.ctx.call_cli(
            [str(BIN), sub],
            input=json.dumps(evt._raw),
            timeout=timeout,
            throw=False,
        )
    except UnicodeDecodeError:
        return None
