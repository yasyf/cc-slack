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
NO_READ = (
    "This agent has not read the thread, and its newest message is neither one the agent got nor one it posted."
    " Call `slack_thread` with `channel_id` {channel} and `thread_ts` {thread}, then retry."
)
OLD_READ = (
    "This agent's last whole read of the thread is {minutes} minutes old{behind}."
    " Call `slack_thread` with `channel_id` {channel}, `thread_ts` {thread} and `since_ts` {since}, then retry."
)
TRUNCATED_READ = (
    "This agent's last read of the thread was truncated, so it did not reach every message."
    " Call `slack_thread` with `channel_id` {channel}, `thread_ts` {thread} and `since_ts` {since},"
    " and while a read is truncated, again with `since_ts` set to the last `ts` it returned, then retry."
)
CUT_READ = (
    "This agent's last read of the thread was truncated, and it has no whole read to start from."
    " Call `slack_thread` with `channel_id` {channel}, `thread_ts` {thread} and a `limit` above the thread's length, then retry."
)
STALE_READ = (
    "This agent's last read of the thread was a stale copy served while Slack rate-limited the bot."
    " Call `slack_thread` with `channel_id` {channel} and `thread_ts` {thread} again once its `retry_after` passes, then retry."
)
REPLY = THREAD | {"text": "On it."}
FULL_READ = json.dumps(THREAD | {"messages": [], "truncated": False})
SEEN = f"{BIN} thread-seen"
PICKED = "1790210300.000001"
POSTED = "1790210350.000001"
NEWER = "1790210400.000001"
UNSEEN = {SEEN: json.dumps({"newest": NEWER, "delivered": False})}
POST_RESULT = json.dumps(THREAD | {"ts": POSTED, "posted": True, "watched": True})


class ThreadRead(BaseModel):
    at: datetime | None = None
    through: str = ""
    gap: str = ""


class ThreadReads(BaseModel):
    reads: dict[str, ThreadRead] = Field(default_factory=dict)
    picked: dict[str, datetime] = Field(default_factory=dict)
    posted: dict[str, datetime] = Field(default_factory=dict)


def read_key(evt: ToolHookEvent, thread: str) -> str:
    return f"{evt.agent_id or 'main'}/{thread}"


def message_key(evt: ToolHookEvent, channel: str, ts: str) -> str:
    return f"{evt.agent_id or 'main'}/{channel}/{ts}"


def reads(*threads: str, agent: str = "main", age: timedelta = timedelta(), through: str = "", gap: str = "") -> ThreadReads:
    return ThreadReads(reads={f"{agent}/{thread}": ThreadRead(at=datetime.now(UTC) - age, through=through, gap=gap) for thread in threads})


def gapped(gap: str) -> ThreadReads:
    return ThreadReads(reads={f"main/{THREAD['thread_ts']}": ThreadRead(gap=gap)})


def picks(ts: str) -> ThreadReads:
    return ThreadReads(picked={f"main/{THREAD['channel_id']}/{ts}": datetime.now(UTC)})


def posts(ts: str, agent: str = "main") -> ThreadReads:
    return ThreadReads(posted={f"{agent}/{THREAD['channel_id']}/{ts}": datetime.now(UTC)})


def seen(newest: str, *, delivered: bool, after: int = 0) -> dict[str, str]:
    return {SEEN: json.dumps({"newest": newest, "delivered": delivered, "after": after})}


def thread_seen(evt: ToolHookEvent, write: Write, since: str) -> dict[str, Any]:
    if not (session := evt._raw.get("session_id")):
        return {}
    argv = [str(BIN), "thread-seen", "--session", session, "--channel", write.channel, "--thread", write.thread, "--owner", evt.agent_id or "", "--since", since]
    try:
        return json.loads(capture(argv, timeout=SEEN_TIMEOUT) or "{}")
    except (OSError, subprocess.SubprocessError, ValueError):
        return {}


def newest_reached(evt: ToolHookEvent, write: Write, known: ThreadReads, found: dict[str, Any]) -> bool:
    newest = message_key(evt, write.channel, str(found.get("newest")))
    return bool(found.get("delivered")) or newest in known.picked or newest in known.posted


def refusal(write: Write, read: ThreadRead, now: datetime, after: int) -> str:
    where = {"channel": write.channel, "thread": write.thread, "since": read.through}
    if read.gap == "stale":
        return STALE_READ.format(**where)
    if read.gap == "truncated":
        return (TRUNCATED_READ if read.through else CUT_READ).format(**where)
    if read.at is None:
        return NO_READ.format(**where)
    behind = f" and {after} message{'s' * (after != 1)} behind the thread's newest" if after else ""
    return OLD_READ.format(**where | {"since": read.through or write.thread}, minutes=int((now - read.at) / timedelta(minutes=1)), behind=behind)


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


def thread_read(evt: PostToolUseEvent) -> dict[str, Any] | None:
    if evt.tool_name == FALLBACK_READ:
        return {}
    return next((found for found in json_objects(evt.tool_response) if "messages" in found), None)


def reached(read: dict[str, Any]) -> list[str]:
    return [str(message["ts"]) for message in read.get("messages") or [] if isinstance(message, dict) and message.get("ts")]


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
        Input(tool=P + "slack_reply", tool_input=REPLY, commands=UNSEEN): Block(pattern=r"has not read the thread.*`slack_thread` with `channel_id` C0APPTHREAD and `thread_ts` 1790210144\.444179, then retry"),
        Input(tool=P + "slack_send", tool_input=REPLY, commands=UNSEEN): Block(),
        Input(tool="mcp__slack__slack_send_message", tool_input=REPLY, commands=UNSEEN): Block(),
        Input(command=f"{CLI} reply --url {THREAD_LINK} --text 'on it'", commands=UNSEEN): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"])]): Allow(),
        Input(command=f"{CLI} reply --url {THREAD_LINK} --text 'on it'", state=[reads(THREAD["thread_ts"])]): Allow(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"], age=timedelta(minutes=11))], commands=UNSEEN): Block(pattern=r"1\d minutes old\. .*`since_ts` 1790210144\.444179,"),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"], age=timedelta(minutes=12), through=PICKED)], commands=seen(NEWER, delivered=False, after=3)): Block(
            pattern=r"1\d minutes old and 3 messages behind the thread's newest\. .*`since_ts` 1790210300\.000001,"
        ),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"], age=timedelta(minutes=12), through=PICKED)], commands=seen(NEWER, delivered=False, after=1)): Block(pattern=r"and 1 message behind"),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[gapped("truncated")], commands=UNSEEN): Block(pattern=r"was truncated, and it has no whole read.*a `limit` above"),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"], age=timedelta(minutes=11), through=PICKED, gap="truncated")], commands=UNSEEN): Block(
            pattern=r"was truncated, so it did not reach every message.*`since_ts` 1790210300\.000001,.*the last `ts` it returned"
        ),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[gapped("stale")], commands=UNSEEN): Block(pattern=r"stale copy.*`retry_after`"),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"], age=timedelta(minutes=2), gap="truncated")], commands=UNSEEN): Allow(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[picks(PICKED), posts(POSTED)], commands=seen(POSTED, delivered=False)): Allow(),
        Input(command=f"{CLI} reply --url {THREAD_LINK} --text 'and done'", state=[posts(POSTED)], commands=seen(POSTED, delivered=False)): Allow(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[picks(PICKED), posts(POSTED)], commands=seen(NEWER, delivered=False)): Block(pattern=r"has not read the thread"),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[posts(POSTED, agent="a1")], commands=seen(POSTED, delivered=False)): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads("1790210000.000001")], commands=UNSEEN): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"])], agent_id="a1", commands=UNSEEN): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, state=[reads(THREAD["thread_ts"], agent="a1")], agent_id="a1"): Allow(),
        Input(tool=P + "slack_send", tool_input={"channel_id": THREAD["channel_id"], "text": "A new post."}): Allow(),
        Input(tool=P + "slack_edit", tool_input=REPLY | {"ts": "1790210200.000001"}): Allow(),
        Input(tool=P + "slack_dm_status", tool_input=REPLY): Allow(),
        Input(tool=P + "slack_reply", tool_input=REPLY, commands=seen(PICKED, delivered=True)): Allow(),
        Input(tool=P + "slack_reply", tool_input=REPLY, commands=seen(PICKED, delivered=False)): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, commands=seen(PICKED, delivered=False), state=[picks(PICKED)]): Allow(),
        Input(tool=P + "slack_reply", tool_input=REPLY, commands=seen(NEWER, delivered=False), state=[picks(PICKED)]): Block(),
        Input(tool=P + "slack_reply", tool_input=REPLY, commands={SEEN: (1, "daemon down")}): Block(),
    },
)
def replies_read_their_thread_first(evt: PreToolUseEvent) -> HookResult | None:
    known, now = evt.ctx.state.load(ThreadReads), datetime.now(UTC)
    for write in replies(evt):
        read = known.reads.get(read_key(evt, write.thread), ThreadRead())
        if read.at is not None and now - read.at < READ_WINDOW:
            continue
        found = thread_seen(evt, write, read.through)
        if not newest_reached(evt, write, known, found):
            return evt.block(refusal(write, read, now, int(found.get("after") or 0)))
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
def thread_reads_are_recorded(evt: PostToolUseEvent) -> HookResult | None:
    if (read := thread_read(evt)) is None:
        return None
    gap = "truncated" if read.get("truncated") else "stale" if read.get("stale") else ""
    with evt.ctx.state[ThreadReads].mutate() as known:
        now = datetime.now(UTC)
        for thread in read_threads(evt):
            last = known.reads.get(read_key(evt, thread), ThreadRead())
            update = {"gap": gap} if gap else {"at": now, "through": max([last.through, *reached(read)]), "gap": ""}
            known.reads[read_key(evt, thread)] = last.model_copy(update=update)
    return None


@on(
    Event.PostToolUse,
    only_if=[RepliesInThread()],
    tests={
        Input(tool=P + "slack_reply", tool_input=REPLY, output=POST_RESULT): Allow(),
        Input(command=f"{CLI} reply --url {THREAD_LINK} --text 'on it'", output=POST_RESULT): Allow(),
        Input(tool=P + "slack_reply", tool_input=REPLY, output='{"ticket_id":"t1"}'): Allow(),
    },
)
def own_replies_are_recorded(evt: PostToolUseEvent) -> HookResult | None:
    if (posted := next((found for found in json_objects(evt.tool_response) if found.get("posted") and found.get("ts")), None)) is None:
        return None
    with evt.ctx.state[ThreadReads].mutate() as known:
        now = datetime.now(UTC)
        known.posted = {key: at for key, at in known.posted.items() if now - at < PICK_KEPT}
        known.posted[message_key(evt, str(posted["channel_id"]), str(posted["ts"]))] = now
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
        known.picked[message_key(evt, str(picked["channel_id"]), str(picked["ts"]))] = now
    return None
