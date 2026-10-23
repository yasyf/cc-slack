from __future__ import annotations

import json
import re

from captain_hook import CONFIRMED, UNCONFIRMED, Allow, Block, Confirm, Event, HookResult, Input, PreToolUseEvent, on

from .permission import CC_SLACK, Conversation, read_thread
from .plain_words import DISABLED, ENABLED, P, THREAD, WritesSlack, block_text
from .writes import Write, pending

EVIDENCE_ASK = re.compile(
    r"\b(?:send|share|attach|paste|upload|grab|post|forward|provide|give)\b[^.?!\n]{0,40}\b(?:logs?|screenshots?|screen ?(?:recordings?|captures?)|recordings?|versions?|console (?:output|logs?)|har files?)\b"
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
FIXED_CLAIM = re.compile(r"\b(?:fixed|resolved|works? (?:again|now)|working again|should (?:open|work|load))\b", re.IGNORECASE)
PROVEN_USER = re.compile(r"\busr_[A-Za-z0-9]+\b")
PROOF_CITED = re.compile(r"(?<![\w/])#\d{3,}\b|https?://[^\s|>]*(?:sentry\.io|datadoghq\.com)[^\s|>]*")
FIXED_JUDGE = Confirm(
    rule="Block only when a line of the post tells a person their problem is fixed, works, or should open;"
    " a line saying a fix is in progress or not yet verified never blocks."
)
UNPROVEN_FIX = (
    "A fixed line cites the record of that person's case succeeding on the live build, their `usr_` id, and the thread owner's `<@id>`."
    " Add all three to each such line, or say the fix is still being verified."
)
OWNER = "U0DANA00001"
OPENED = json.dumps({"messages": [{"ts": THREAD["thread_ts"], "user": OWNER, "user_name": "Dana", "text": "uploads fail for me"}]})


def posts(evt: PreToolUseEvent) -> list[tuple[Write, str]]:
    found = []
    for write in pending(evt):
        if write.verb in ("post", "edit"):
            blocks = write.fields.get("blocks")
            found.append((write, "\n".join([write.text, *block_text(blocks if isinstance(blocks, list) else [blocks] if blocks else [])])))
    return found


def thread_owner(write: Write) -> str | None:
    thread = read_thread(Conversation(write.channel, write.thread))
    if not thread.readable or not thread.messages:
        return None
    return next((message.user for message in thread.messages if message.ts == write.thread), thread.messages[0].user)


def proven(line: str, owner: str | None) -> bool:
    return bool(PROVEN_USER.search(line)) and bool(PROOF_CITED.search(line)) and (owner is None or owner in line)


def unproven(write: Write, text: str) -> bool:
    claims = [line for line in text.splitlines() if FIXED_CLAIM.search(line)]
    if not claims:
        return False
    owner = thread_owner(write) if write.thread else None
    return not all(proven(line, owner) for line in claims)


def reply(text: str, **extra: object) -> Input:
    return Input(env=ENABLED, tool=P + "slack_reply", tool_input=THREAD | {"text": text}, commands={f"{CC_SLACK} thread": OPENED}, **extra)


@on(
    Event.PreToolUse,
    only_if=[WritesSlack()],
    tests={
        reply("Can you send us your logs from this morning?", decide=CONFIRMED): Block(pattern=r"^Incident comms never asks a person for logs"),
        reply("Which version of the app are you on?", decide=CONFIRMED): Block(),
        reply("Could you provide a screenshot of the error?", decide=CONFIRMED): Block(),
        Input(env=ENABLED, tool=P + "slack_send", tool_input={"channel_id": THREAD["channel_id"], "text": "Can you send your logs?"}, decide=CONFIRMED): Block(),
        reply("Can you send us your logs from this morning?", decide=UNCONFIRMED): Allow(),
        reply("We read the upload errors from telemetry; a fix is building.", decide=CONFIRMED): Allow(),
        Input(env=DISABLED, tool=P + "slack_reply", tool_input=THREAD | {"text": "Can you send us your logs?"}, decide=CONFIRMED): Allow(),
    },
)
def comms_never_asks_for_evidence(evt: PreToolUseEvent) -> HookResult | None:
    asks = [write for write, text in posts(evt) if EVIDENCE_ASK.search(text)]
    return evt.block(NO_EVIDENCE_ASKS, confirm=EVIDENCE_JUDGE) if asks else None


@on(
    Event.PreToolUse,
    only_if=[WritesSlack()],
    tests={
        reply("Dana, this is fixed now.", decide=CONFIRMED): Block(pattern=r"^A fixed line cites the record"),
        reply(f"<@{OWNER}> your uploads are fixed: usr_8f2k1 now succeeds on the live build (#500310).", decide=CONFIRMED): Allow(),
        reply(f"<@{OWNER}> your uploads are fixed: usr_8f2k1 now succeeds on the live build.", decide=CONFIRMED): Block(),
        reply("Your uploads are fixed: usr_8f2k1 now succeeds on the live build (#500310).", decide=CONFIRMED): Block(),
        reply(f"<@{OWNER}> usr_8f2k1 checked (#500310).\nEli, your uploads are fixed too.", decide=CONFIRMED): Block(),
        reply("We've fixed your upload problem.", decide=CONFIRMED): Block(),
        reply("The app should open now.", decide=CONFIRMED): Block(),
        reply("Dana, this is fixed now.", decide=UNCONFIRMED): Allow(),
        reply("A fix for the upload errors is building; we will confirm once it is live.", decide=CONFIRMED): Allow(),
        Input(
            env=ENABLED,
            tool=P + "slack_reply",
            tool_input=THREAD | {"text": "usr_8f2k1 is fixed now (#500310)."},
            commands={f"{CC_SLACK} thread": (1, "cc-slack can't reach the daemon")},
            decide=CONFIRMED,
        ): Allow(),
        Input(env=ENABLED, tool=P + "slack_send", tool_input={"channel_id": THREAD["channel_id"], "text": "The upload errors are fixed."}, decide=CONFIRMED): Block(),
        Input(
            env=ENABLED,
            tool=P + "slack_send",
            tool_input={"channel_id": THREAD["channel_id"], "text": "usr_8f2k1 uploads are fixed: https://acme.sentry.io/issues/42/"},
            decide=CONFIRMED,
        ): Allow(),
        Input(env=DISABLED, tool=P + "slack_reply", tool_input=THREAD | {"text": "Dana, this is fixed now."}, decide=CONFIRMED): Allow(),
    },
)
def fixed_lines_name_the_proven_user(evt: PreToolUseEvent) -> HookResult | None:
    return evt.block(UNPROVEN_FIX, confirm=FIXED_JUDGE) if any(unproven(write, text) for write, text in posts(evt)) else None
