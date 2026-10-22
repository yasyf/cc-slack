from __future__ import annotations

from captain_hook import BaseHookEvent, Event, on

from . import common


@on(Event.SessionStart, async_=True)
def session_record(evt: BaseHookEvent) -> None:
    common.call_bin(evt, "session-record")
