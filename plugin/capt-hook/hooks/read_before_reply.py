from __future__ import annotations

import json
from collections.abc import Iterator
from datetime import UTC, datetime, timedelta
from typing import Any

from pydantic import BaseModel, Field

from captain_hook import Allow, Block, Event, HookResult, Input, PostToolUseEvent, PreToolUseEvent, on
from captain_hook.events import ToolHookEvent

from .plain_words import CLI, P, THREAD, THREAD_LINK
from .writes import Write, pending, read_threads

READ_WINDOW = timedelta(minutes=10)
FALLBACK_READ = "mcp__slack__slack_get_thread"
UNREAD_THREAD = (
    "A thread reply needs a full read of that thread by this agent in the last 10 minutes; a read cut by a pipe or a redirect does not count."
    " Call `slack_thread` with `channel_id` {channel}, `thread_ts` {thread} and `limit` 1000, or run `cc-slack thread` bare, then retry."
)
REPLY = THREAD | {"text": "On it."}
FULL_READ = json.dumps(THREAD | {"messages": [], "truncated": False})


class ThreadReads(BaseModel):
    at: dict[str, datetime] = Field(default_factory=dict)


def read_key(evt: ToolHookEvent, thread: str) -> str:
    return f"{evt.agent_id or 'main'}/{thread}"


def reads(*threads: str, agent: str = "main", age: timedelta = timedelta()) -> ThreadReads:
    return ThreadReads(at={f"{agent}/{thread}": datetime.now(UTC) - age for thread in threads})


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
        Input(tool=P + "slack_reply", tool_input=REPLY): Block(pattern=r"`slack_thread` with `channel_id` C0APPTHREAD, `thread_ts` 1790210144\.444179"),
        Input(tool=P + "slack_send", tool_input=REPLY): Block(),
        Input(tool="mcp__slack__slack_send_message", tool_input=REPLY): Block(),
        Input(command=f"{CLI} reply --url {THREAD_LINK} --text 'on it'"): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"])]): Allow(),
        Input(command=f"{CLI} reply --url {THREAD_LINK} --text 'on it'", state=[reads(THREAD["thread_ts"])]): Allow(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"], age=timedelta(minutes=11))]): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads("1790210000.000001")]): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"])], agent_id="a1"): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"], agent="a1")], agent_id="a1"): Allow(),
        Input(tool=P + "slack_send", tool_input={"channel_id": THREAD["channel_id"], "text": "A new post."}): Allow(),
        Input(tool=P + "slack_edit", tool_input=REPLY | {"ts": "1790210200.000001"}): Allow(),
        Input(tool=P + "slack_dm_status", tool_input=REPLY): Allow(),
    },
)
def replies_read_their_thread_first(evt: PreToolUseEvent) -> HookResult | None:
    known, now = evt.ctx.state.load(ThreadReads).at, datetime.now(UTC)
    for write in replies(evt):
        read = known.get(read_key(evt, write.thread))
        if read is None or now - read >= READ_WINDOW:
            return evt.block(UNREAD_THREAD.format(channel=write.channel, thread=write.thread))
    return None


@on(
    Event.PostToolUse,
    only_if=[ReadsThread()],
    tests={
        Input(tool=P + "slack_thread", tool_input=THREAD, output=FULL_READ): Allow(),
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
