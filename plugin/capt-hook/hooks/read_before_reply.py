from __future__ import annotations

import json
import subprocess
from collections.abc import Iterator
from datetime import UTC, datetime, timedelta
from typing import Any

from pydantic import BaseModel, Field

from captain_hook import Allow, Block, Event, HookResult, Input, PostToolUseEvent, PreToolUseEvent, on
from captain_hook.events import ToolHookEvent

from .common import BIN
from .plain_words import CLI, P, THREAD, THREAD_LINK
from .writes import Write, capture, pending, read_threads

READ_WINDOW = timedelta(minutes=10)
PICK_KEPT = timedelta(days=1)
SEEN_TIMEOUT = 5
PICKUP = P + "slack_pickup"
FALLBACK_READ = "mcp__slack__slack_get_thread"
UNREAD_THREAD = (
    "A thread reply needs this agent's read, in the last 10 minutes, through the thread's newest message, neither truncated, stale, nor piped."
    " Call `slack_thread` with `channel_id` {channel}, `thread_ts` {thread} and `since_ts` set to the newest ts you have read, then retry."
)
REPLY = THREAD | {"text": "On it."}
FULL_READ = json.dumps(THREAD | {"messages": [], "truncated": False})
SEEN = f"{BIN} thread-seen"
PICKED = "1790210300.000001"
UNSEEN = {SEEN: json.dumps({"newest": "1790210400.000001", "delivered": False})}


class ThreadReads(BaseModel):
    at: dict[str, datetime] = Field(default_factory=dict)
    picked: dict[str, datetime] = Field(default_factory=dict)


def read_key(evt: ToolHookEvent, thread: str) -> str:
    return f"{evt.agent_id or 'main'}/{thread}"


def picked_key(evt: ToolHookEvent, channel: str, ts: str) -> str:
    return f"{evt.agent_id or 'main'}/{channel}/{ts}"


def reads(*threads: str, agent: str = "main", age: timedelta = timedelta()) -> ThreadReads:
    return ThreadReads(at={f"{agent}/{thread}": datetime.now(UTC) - age for thread in threads})


def picks(ts: str) -> ThreadReads:
    return ThreadReads(picked={f"main/{THREAD['channel_id']}/{ts}": datetime.now(UTC)})


def seen(newest: str, *, delivered: bool) -> dict[str, str]:
    return {SEEN: json.dumps({"newest": newest, "delivered": delivered})}


def newest_reached(evt: ToolHookEvent, write: Write, picked: dict[str, datetime]) -> bool:
    if not (session := evt._raw.get("session_id")):
        return False
    argv = [str(BIN), "thread-seen", "--session", session, "--channel", write.channel, "--thread", write.thread, "--owner", evt.agent_id or ""]
    try:
        found = json.loads(capture(argv, timeout=SEEN_TIMEOUT) or "{}")
    except (OSError, subprocess.SubprocessError, ValueError):
        return False
    return bool(found.get("delivered")) or picked_key(evt, write.channel, str(found.get("newest"))) in picked


def replies(evt: ToolHookEvent) -> list[Write]:
    return [write for write in pending(evt) if write.verb == "post" and write.readable and write.thread]


def json_objects(value: object) -> Iterator[dict[str, Any]]:
    if isinstance(value, dict):
        yield value
        for inner in value.values():
            yield from json_objects(inner)
    elif isinstance(value, list):
        for inner in value:
            yield from json_objects(inner)
    elif isinstance(value, str) and (start := value.find("{")) >= 0:
        try:
            yield from json_objects(json.loads(value[start : value.rfind("}") + 1]))
        except ValueError:
            return


def read_in_full(evt: PostToolUseEvent) -> bool:
    if evt.tool_name == FALLBACK_READ:
        return True
    read = next((found for found in json_objects(evt.tool_response) if "messages" in found), None)
    return read is not None and not read.get("truncated") and not read.get("stale")


class RepliesInThread:
    def check(self, evt: ToolHookEvent) -> bool:
        return bool(replies(evt))


class ReadsThread:
    def check(self, evt: ToolHookEvent) -> bool:
        return bool(read_threads(evt))


@on(  # capt-hook: no-escape the refusal names the one read call that clears it
    Event.PreToolUse,
    only_if=[RepliesInThread()],
    tests={
        Input(tool=P + "slack_reply", tool_input=REPLY, commands=UNSEEN): Block(pattern=r"`slack_thread` with `channel_id` C0APPTHREAD, `thread_ts` 1790210144\.444179 and `since_ts`"),
        Input(tool=P + "slack_send", tool_input=REPLY, commands=UNSEEN): Block(),
        Input(tool="mcp__slack__slack_send_message", tool_input=REPLY, commands=UNSEEN): Block(),
        Input(command=f"{CLI} reply --url {THREAD_LINK} --text 'on it'", commands=UNSEEN): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"])]): Allow(),
        Input(command=f"{CLI} reply --url {THREAD_LINK} --text 'on it'", state=[reads(THREAD["thread_ts"])]): Allow(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"], age=timedelta(minutes=11))], commands=UNSEEN): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads("1790210000.000001")], commands=UNSEEN): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"])], agent_id="a1", commands=UNSEEN): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"], agent="a1")], agent_id="a1"): Allow(),
        Input(tool=P + "slack_send", tool_input={"channel_id": THREAD["channel_id"], "text": "A new post."}): Allow(),
        Input(tool=P + "slack_edit", tool_input=REPLY | {"ts": "1790210200.000001"}): Allow(),
        Input(tool=P + "slack_dm_status", tool_input=REPLY): Allow(),
        Input(tool=P + "slack_reply", tool_input=REPLY, commands=seen(PICKED, delivered=True)): Allow(),
        Input(tool=P + "slack_reply", tool_input=REPLY, commands=seen(PICKED, delivered=False)): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, commands=seen(PICKED, delivered=False), state=[picks(PICKED)]): Allow(),
        Input(tool=P + "slack_reply", tool_input=REPLY, commands=seen("1790210400.000001", delivered=False), state=[picks(PICKED)]): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, commands={SEEN: (1, "daemon down")}): Block(),
    },
)
def replies_read_their_thread_first(evt: PreToolUseEvent) -> HookResult | None:
    known, now = evt.ctx.state.load(ThreadReads), datetime.now(UTC)
    for write in replies(evt):
        read = known.at.get(read_key(evt, write.thread))
        if (read is None or now - read >= READ_WINDOW) and not newest_reached(evt, write, known.picked):
            return evt.block(UNREAD_THREAD.format(channel=write.channel, thread=write.thread))
    return None


@on(
    Event.PostToolUse,
    only_if=[ReadsThread()],
    tests={
        Input(tool=P + "slack_thread", tool_input=THREAD, output=FULL_READ): Allow(),
        Input(tool=P + "slack_thread", tool_input=THREAD | {"since_ts": PICKED}, output=FULL_READ): Allow(),
        Input(tool=P + "slack_reply", tool_input=REPLY, output=FULL_READ): Allow(),
    },
)
def full_thread_reads_are_recorded(evt: PostToolUseEvent) -> HookResult | None:
    if not read_in_full(evt):
        return None
    with evt.ctx.state[ThreadReads].mutate() as known:
        now = datetime.now(UTC)
        for thread in read_threads(evt):
            known.at[read_key(evt, thread)] = now
    return None


class PicksUp:
    def check(self, evt: ToolHookEvent) -> bool:
        return isinstance(evt, ToolHookEvent) and evt.tool_name == PICKUP


@on(
    Event.PostToolUse,
    only_if=[PicksUp()],
    tests={
        Input(tool=PICKUP, tool_input={"channel_id": THREAD["channel_id"], "ts": PICKED}, output='{"ok":true}'): Allow(),
        Input(tool=PICKUP, tool_input={"channel_id": THREAD["channel_id"], "ts": PICKED, "done": True}, output='{"ok":true}'): Allow(),
    },
)
def pickups_are_recorded(evt: PostToolUseEvent) -> HookResult | None:
    picked = evt.input.raw
    if picked.get("done") or not picked.get("channel_id") or not picked.get("ts"):
        return None
    with evt.ctx.state[ThreadReads].mutate() as known:
        now = datetime.now(UTC)
        known.picked = {key: at for key, at in known.picked.items() if now - at < PICK_KEPT}
        known.picked[picked_key(evt, str(picked["channel_id"]), str(picked["ts"]))] = now
    return None
