from __future__ import annotations

import json
import re

from captain_hook import CONFIRMED, UNCONFIRMED, Allow, Block, Confirm, Event, HookResult, Input, PreToolUseEvent, on

from .permission import CC_SLACK, Conversation, read_thread
from .plain_words import DISABLED, ENABLED, P, THREAD, WritesSlack, posted_text
from .writes import Write, pending

EVIDENCE_ASK = re.compile(
    r"\b(?:send|share|attach|paste|upload|grab|post|forward)\b[^.?!\n]{0,40}\b(?:logs?|screenshots?|screen ?recordings?|versions?|console output)\b"
    r"|\b(?:what|which) (?:app |build |client )?version\b|\bversion (?:are|is) you\b",
    re.IGNORECASE,
)
EVIDENCE_JUDGE = Confirm(
    rule="Block only when the post asks a person to provide logs, a version, a screenshot or a recording;"
    " a post that reports what telemetry showed, or thanks someone for what they already sent, never blocks."
)
NO_EVIDENCE_ASKS = (
    "Incident comms never asks a person for logs, versions or screenshots; the evidence lane reads them from telemetry."
    " Drop the ask and post what is known."
)
FIXED_CLAIM = re.compile(
    r"\b(?:(?:is|are|was|been|now|should be|got) (?:fixed|resolved)|fixed (?:it|this|that|now)|works? (?:again|now)|working again)\b",
    re.IGNORECASE,
)
PROVEN_USER = re.compile(r"\busr_[A-Za-z0-9]+\b")
FIXED_JUDGE = Confirm(
    rule="Block only when the post tells a person their problem is fixed or resolved;"
    " a post saying a fix is in progress, or reporting on a different person's case, never blocks."
)
UNPROVEN_FIX = (
    "A fixed line names the user id the fix was proven for and tags the thread owner."
    " Add the `usr_` id whose failing case now succeeds on the live build, and `<@owner>`, or say the fix is still being verified."
)
OWNER = "U0DANA00001"
OPENED = json.dumps({"messages": [{"ts": THREAD["thread_ts"], "user": OWNER, "user_name": "Dana", "text": "uploads fail for me"}]})


def replies(evt: PreToolUseEvent) -> list[Write]:
    return [write for write in pending(evt) if write.verb in ("post", "edit") and write.thread]


def thread_owner(write: Write) -> str | None:
    thread = read_thread(Conversation(write.channel, write.thread))
    if not thread.readable or not thread.messages:
        return None
    return next((message.user for message in thread.messages if message.ts == write.thread), thread.messages[0].user)


def proven(write: Write, text: str) -> bool:
    return bool(PROVEN_USER.search(text)) and ((owner := thread_owner(write)) is None or owner in text)


def reply(text: str, **extra: object) -> Input:
    return Input(env=ENABLED, tool=P + "slack_reply", tool_input=THREAD | {"text": text}, commands={f"{CC_SLACK} thread": OPENED}, **extra)


@on(
    Event.PreToolUse,
    only_if=[WritesSlack()],
    tests={
        reply("Can you send us your logs from this morning?", decide=CONFIRMED): Block(pattern=r"^Incident comms never asks a person for logs"),
        reply("Which version of the app are you on?", decide=CONFIRMED): Block(),
        reply("Can you send us your logs from this morning?", decide=UNCONFIRMED): Allow(),
        reply("We read the upload errors from telemetry; a fix is building.", decide=CONFIRMED): Allow(),
        Input(env=DISABLED, tool=P + "slack_reply", tool_input=THREAD | {"text": "Can you send us your logs?"}, decide=CONFIRMED): Allow(),
    },
)
def comms_never_asks_for_evidence(evt: PreToolUseEvent) -> HookResult | None:
    asks = [write for write in replies(evt) if EVIDENCE_ASK.search(posted_text(write.fields))]
    return evt.block(NO_EVIDENCE_ASKS, confirm=EVIDENCE_JUDGE) if asks else None


@on(
    Event.PreToolUse,
    only_if=[WritesSlack()],
    tests={
        reply("Dana, this is fixed now.", decide=CONFIRMED): Block(pattern=r"^A fixed line names the user id"),
        reply(f"<@{OWNER}> your uploads are fixed: usr_8f2k1 now succeeds on the live build.", decide=CONFIRMED): Allow(),
        reply("Your uploads are fixed: usr_8f2k1 now succeeds on the live build.", decide=CONFIRMED): Block(),
        reply("Dana, this is fixed now.", decide=UNCONFIRMED): Allow(),
        reply("A fix for the upload errors is building; we will confirm once it is live.", decide=CONFIRMED): Allow(),
        Input(
            env=ENABLED,
            tool=P + "slack_reply",
            tool_input=THREAD | {"text": "usr_8f2k1 is fixed now."},
            commands={f"{CC_SLACK} thread": (1, "cc-slack can't reach the daemon")},
            decide=CONFIRMED,
        ): Allow(),
        Input(env=ENABLED, tool=P + "slack_send", tool_input={"channel_id": THREAD["channel_id"], "text": "The upload errors are fixed."}, decide=CONFIRMED): Allow(),
        Input(env=DISABLED, tool=P + "slack_reply", tool_input=THREAD | {"text": "Dana, this is fixed now."}, decide=CONFIRMED): Allow(),
    },
)
def fixed_lines_name_the_proven_user(evt: PreToolUseEvent) -> HookResult | None:
    claims = [(write, text) for write in replies(evt) if FIXED_CLAIM.search(text := posted_text(write.fields))]
    return evt.block(UNPROVEN_FIX, confirm=FIXED_JUDGE) if any(not proven(write, text) for write, text in claims) else None
