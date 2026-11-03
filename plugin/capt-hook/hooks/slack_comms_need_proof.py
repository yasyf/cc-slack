from __future__ import annotations

import json
import re
from fnmatch import fnmatchcase

from captain_hook import CONFIRMED, UNCONFIRMED, Allow, Block, Confirm, Event, HookResult, Input, PreToolUseEvent, on

from .permission import CC_SLACK, Conversation, Message, read_thread
from .plain_words import DISABLED, ENABLED, P, THREAD, WritesSlack, block_text
from .writes import Write, capture, pending, permissions

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
RELEASE_NOTE = re.compile(r"\bv?\d+\.\d+\.\d+\b")
FIXED_JUDGE = Confirm(
    rule="Block only when a line of the post tells a person their problem is fixed, works, or should open;"
    " a line saying a fix is in progress or not yet verified, or a release note naming a version to upgrade to, never blocks."
)
UNPROVEN_FIX = (
    "Telling someone their problem is fixed needs their `usr_` id, the thread owner's `<@id>` and a link to their case succeeding on the live build"
    " on that line, so add all three or say the fix is still being verified."
)
OWNER = "U0DANA00001"
OPENED = json.dumps({"messages": [{"ts": THREAD["thread_ts"], "user": OWNER, "user_name": "Dana", "text": "uploads fail for me"}]})
OURS = json.dumps({"messages": [{"ts": THREAD["thread_ts"], "user": "", "user_name": "Dana (claude)", "from_claude": True, "text": "Which fix should ship?"}]})
OUTAGE = "C0OUTAGE"
TEAM = "C0TEAMROOM"
DM = "D0DANA0001"
CHANNELS = json.dumps({"channels": [{"id": THREAD["channel_id"], "name": "acc-dana"}, {"id": OUTAGE, "name": "outage"}, {"id": TEAM, "name": "platform-eng"}]})


def posts(evt: PreToolUseEvent) -> list[tuple[Write, str]]:
    found = []
    for write in pending(evt):
        if write.verb in ("post", "edit"):
            blocks = write.fields.get("blocks")
            found.append((write, "\n".join([write.text, *block_text(blocks if isinstance(blocks, list) else [blocks] if blocks else [])])))
    return found


def in_proof_channel(write: Write) -> bool:
    if not (out := capture([CC_SLACK, "channels", "--json"], timeout=5)):
        return False
    names = [channel["name"] for channel in json.loads(out)["channels"] if channel["id"] == write.channel]
    return any(fnmatchcase(name, pattern) for name in names for pattern in permissions().proof_channels)


def thread_root(write: Write) -> Message | None:
    messages = read_thread(Conversation(write.channel, write.thread)).messages
    return next((message for message in messages if message.ts == write.thread), messages[0]) if messages else None


def posted_by_us(root: Message | None) -> bool:
    return root is not None and root.from_claude


def proven(line: str, owner: str | None) -> bool:
    return bool(PROVEN_USER.search(line)) and bool(PROOF_CITED.search(line)) and (owner is None or owner in line)


def unproven(write: Write, text: str) -> bool:
    claims = [line for line in text.splitlines() if FIXED_CLAIM.search(line)]
    if not claims or RELEASE_NOTE.search(text) or not in_proof_channel(write):
        return False
    root = thread_root(write)
    return not posted_by_us(root) and not all(proven(line, root.user if root else None) for line in claims)


def reply(text: str, channel: str = THREAD["channel_id"], root: str | tuple[int, str] = OPENED, **extra: object) -> Input:
    return Input(
        env=ENABLED,
        tool=P + "slack_reply",
        tool_input=THREAD | {"channel_id": channel, "text": text},
        commands={f"{CC_SLACK} channels": CHANNELS, f"{CC_SLACK} thread": root},
        **extra,
    )


def send(text: str, channel: str = THREAD["channel_id"]) -> Input:
    return Input(env=ENABLED, tool=P + "slack_send", tool_input={"channel_id": channel, "text": text}, commands={f"{CC_SLACK} channels": CHANNELS}, decide=CONFIRMED)


@on(
    Event.PreToolUse,
    only_if=[WritesSlack()],
    tests={
        reply("Can you send us your logs from this morning?", decide=CONFIRMED): Block(pattern=r"^Incident comms never asks a person for logs"),
        reply("Which version of the app are you on?", decide=CONFIRMED): Block(),
        reply("Could you provide a screenshot of the error?", decide=CONFIRMED): Block(),
        send("Can you send your logs?"): Block(),
        send("Can you send your logs?", OUTAGE): Block(),
        send("Can you send your logs?", DM): Allow(),
        send("Can you send your logs?", TEAM): Allow(),
        reply("Can you send your logs?", root=OURS, decide=CONFIRMED): Allow(),
        reply("Can you send us your logs from this morning?", decide=UNCONFIRMED): Allow(),
        reply("We read the upload errors from telemetry; a fix is building.", decide=CONFIRMED): Allow(),
        Input(env=DISABLED, tool=P + "slack_reply", tool_input=THREAD | {"text": "Can you send us your logs?"}, decide=CONFIRMED): Allow(),
    },
)
def comms_never_asks_for_evidence(evt: PreToolUseEvent) -> HookResult | None:
    asks = [write for write, text in posts(evt) if EVIDENCE_ASK.search(text) and in_proof_channel(write) and not posted_by_us(thread_root(write))]
    return evt.block(NO_EVIDENCE_ASKS, confirm=EVIDENCE_JUDGE) if asks else None


@on(
    Event.PreToolUse,
    only_if=[WritesSlack()],
    tests={
        reply("Dana, this is fixed now.", decide=CONFIRMED): Block(pattern=r"^Telling someone their problem is fixed"),
        reply("Your run is fixed now.", decide=CONFIRMED): Block(),
        reply("Fixed in ccx v0.91.61 (`brew upgrade ccx`). Ship and stack submit no longer start an SSO sign-in.", decide=CONFIRMED): Allow(),
        reply(f"<@{OWNER}> your uploads are fixed: usr_8f2k1 now succeeds on the live build (#500310).", decide=CONFIRMED): Allow(),
        reply(f"<@{OWNER}> your uploads are fixed: usr_8f2k1 now succeeds on the live build.", decide=CONFIRMED): Block(),
        reply("Your uploads are fixed: usr_8f2k1 now succeeds on the live build (#500310).", decide=CONFIRMED): Block(),
        reply(f"<@{OWNER}> usr_8f2k1 checked (#500310).\nEli, your uploads are fixed too.", decide=CONFIRMED): Block(),
        reply("We've fixed your upload problem.", decide=CONFIRMED): Block(),
        reply("The app should open now.", decide=CONFIRMED): Block(),
        reply("Dana, this is fixed now.", decide=UNCONFIRMED): Allow(),
        reply("A fix for the upload errors is building; we will confirm once it is live.", decide=CONFIRMED): Allow(),
        reply("usr_8f2k1 is fixed now (#500310).", root=(1, "cc-slack can't reach the daemon"), decide=CONFIRMED): Allow(),
        send("The upload errors are fixed."): Block(),
        send("usr_8f2k1 uploads are fixed: https://acme.sentry.io/issues/42/"): Allow(),
        reply("Dana, this is fixed now.", OUTAGE, decide=CONFIRMED): Block(),
        send("The upload errors are fixed.", OUTAGE): Block(),
        reply("Dana, this is fixed now.", DM, decide=CONFIRMED): Allow(),
        send("The upload errors are fixed.", DM): Allow(),
        reply("Dana, this is fixed now.", TEAM, decide=CONFIRMED): Allow(),
        reply("Dana, this is fixed now.", OUTAGE, root=OURS, decide=CONFIRMED): Allow(),
        Input(
            env=ENABLED,
            tool=P + "slack_send",
            tool_input={"channel_id": OUTAGE, "text": "The upload errors are fixed."},
            commands={f"{CC_SLACK} channels": (1, "cc-slack can't reach the daemon")},
            decide=CONFIRMED,
        ): Allow(),
        Input(env=DISABLED, tool=P + "slack_reply", tool_input=THREAD | {"text": "Dana, this is fixed now."}, decide=CONFIRMED): Allow(),
    },
)
def fixed_lines_name_the_proven_user(evt: PreToolUseEvent) -> HookResult | None:
    return evt.block(UNPROVEN_FIX, confirm=FIXED_JUDGE) if any(unproven(write, text) for write, text in posts(evt)) else None
