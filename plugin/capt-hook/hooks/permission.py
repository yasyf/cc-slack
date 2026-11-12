from __future__ import annotations

import hashlib
import json
import re
import shlex
import subprocess
import time
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass, replace
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from cc_transcript.models import AttachmentEvent, QueuedCommand, UserEvent
from loguru import logger
from spawnllm import LabelAnswer

from captain_hook import Allow, Ask, Block, Event, HookResult, Input, PreToolUseEvent, Rewrite, T, on
from captain_hook.conditions import UserSaid
from captain_hook.events import BaseHookEvent, ToolHookEvent
from captain_hook.tickets import Context, CustomTicketCondition, Decide, Deny, Evidence, Holds, Proposal, Reach, Rule, Ticket, Tickets, Undecided, UserAnswered, holds, store
from captain_hook.tickets.conditions import found
from captain_hook.tickets.context import CARD_SERVER, RULINGS_TIMEOUT, instruction_words, ruling_evidence, said_evidence, written_at
from captain_hook.tickets.decide import cited
from captain_hook.tickets.declare import UNCHECKED
from captain_hook.tickets.transcript_evidence import EvidenceSession, sessions
from captain_hook.types import And, Or
from captain_hook.util import reqenv

from .common import BIN, check_post
from .writes import DRIVE_SAY, Write, capture, cli_calls, pending, permalink, permissions

CC_SLACK = str(BIN)
GATE = "slack_writes_need_permission"
KIND = "slack.write"
WINDOW = 240
JUDGED_TURNS = 12
SHOWN = 300
LATEST_SHOWN = 3
SHORTCUT_BRIEF = "[cc-slack shortcut brief]"
CHECK_SECONDS = 40
DEADLINE_MARGIN = 5
CHECK_FAILURES = (OSError, LookupError, ValueError, RuntimeError, TypeError, AttributeError, subprocess.SubprocessError)
UNFINISHED = "{gate}: the check did not finish ({cause}), so this write goes ahead unchecked"
GRANT_LABEL = "slack-grant:"
EVERY_THREAD = "/threads"
SOLVED = "/solved"
SOLVED_LINE = "solved-line"
ASKS = re.compile(r"\?|\b(?:please|pls|could you|can you|would you|let (?:me|us) know|try (?:it )?again|retry|confirm)\b", re.I)
REVOKE_LABEL = "slack-revoke:"
NO_STORE = "open git repository"
SCOPE = "recorded-scope"
SCOPE_LINE = "Answer {answer} is recorded as a grant for this channel's top level. It covers a thread only where its own words say threads or replies."
ROOM_OPENED = re.compile(r"Incident opened:\W*<#(C[A-Z0-9]+)")
ROOM_PAGES = "10"
AGENT_POSTER = re.compile(r" \([^()]+\)$")
ASKED = frozenset({"from:owner", "source:askuserquestion"})
KEEP_POSTING = re.compile(r"\b(?:keep|continue|carry on) posting\b|\bwithout asking\b|\bstop asking\b", re.I)
STOP_POSTING = re.compile(r"\b(?:stop|quit|no more) posting\b|\b(?:don['’]?t|do not|never|no longer)\s+(?:(?:keep|continue|carry on)\s+)?post|\bno more posts\b", re.I)
TEXT_VERBS = {"post": "Post this", "edit": "Edit the bot's message to this", "topic": "Set the channel topic to this"}
OTHER_VERBS = {
    "delete": "Delete the bot's Slack message",
    "create": "Create the Slack channel",
    "archive": "Archive the Slack channel",
    "invite": "Invite these people to the Slack channel",
    "join": "Add the cc-slack bot to the Slack channel",
    "mark": "Mark the Slack conversation read",
    "workflow": "Save this Slack workflow",
}
CARD_BODY = 2800
OPENER_SHOWN = 120
LINKED_VERBS = {"post", "edit", "delete", "join"}
SLACK_URL = re.compile(r"https://[\w.-]+\.slack\.com/archives/[^\s<>|)\"]+")
PASTED = re.compile(r"<pasted_content\b.*?</pasted_content>", re.S)
SLACK_ID = re.compile(r"[CDGU][A-Z0-9]{8,}")
BOARD_ANSWER = "board-answer"
HIS_WORDS = frozenset({"prompt", "queued", "plan", "ask", "card", "board", "owner-ledger"})
DM_ORDER = "dm-order"
ASKED_HERE = "the user's own message in the thread this write goes to, read back from Slack"
DM_LINK = re.compile(r"https://[\w.-]+\.slack\.com/archives/(D[A-Z0-9]{8,})/p(\d{10})(\d{6})(?:\?thread_ts=(\d{10}\.\d{6}))?")
SLACK_MESSAGE = "slack.message"
ORDER_LINE = re.compile(rf'<channel source="{re.escape(CARD_SERVER)}"[^>]*\btype="slack\.message"[^>]*>\s*(?P<payload>\{{.*\}})\s*</channel>', re.S)
BOARD_CARD = "board-card:"
BOARD_SESSION = "board-session:"
APPROVED = "Yes"
OWNER_ROW = re.compile(r"\bowner:(m\d+(?:\.\d+)?)\b")
LEDGER_ROW = re.compile(r"^\| (m\d+(?:\.\d+)?) \| [^|]* \| (.*) \| [^|]* \| (?:open|responded|addressed|no-order|due [^|]*) \|$", re.MULTILINE)
DRIVES = Path("long-running") / "drives"
LEDGER = Path("owner-relay") / "owner-ledger.md"
TICKET_FLAGS = {"ticket_id": "ticket", "ticket_tree": "ticket-tree", "ticket_agent": "ticket-agent", "ticket_call": "ticket-call", "ticket_fingerprint": "ticket-fingerprint"}
UNREAD = (
    "A Slack write is checked on its literal values, one write per command. Run one `cc-slack` call per Bash command"
    " with literal `--text`, `--file`, and `--blocks` values and no `--ticket` flags."
)
DRAFTED = (
    "This command writes the `--{flag}` file it sends, and the check reads that file before the command runs, so it would judge"
    " what the file held before. Write the file in its own Bash call first, then send it in the next."
)
PLAN_VERDICTS = frozenset({"delete", "rewrite", "keep"})
UNPLANNED = "This plan cannot run: {reason}. `cc-slack apply` only deletes, rewrites, or keeps the bot's own posts in one DM, so fix the plan and run it again."
REFUSED = (
    "No permission covers this Slack write. Ask the user with `cc-present ask -` and the first card on stdin, or with"
    " `slack_dm_status` and the second card when cc-present is missing, its body opened by a `Why:` line, then retry once they tap Yes."
    "\n```json\n{board}\n```\n```json\n{ask}\n```"
)
RULES = """
    The agent is about to make one Slack write, sent to <destination>. Allow it when the user's own words or
    answers permit a write there, explicitly or by implication: telling the agent to reply, answer, update,
    tell, or explain something to someone there, or approving a post the agent proposed for that place, is
    permission. The words need not quote the text.
    - Permission for one write is not permission for the next thing in that place. Words that permitted an
      earlier write there permit this one only when it carries out the same instruction: the next reply to the
      person the user told the agent to answer, a follow-up on what was asked, or a correction. A new request or
      question to a person, which the agent, another agent, or a lane thought of and the user's words do not ask
      for, is not permitted, however recently the agent wrote there.
    - A question or request the user wrote in this thread asks for its answer there: the acknowledgement and
      the reply that answers it or carries it out, whichever session or handler posts them.
    - A rule of the user's in <evidence> that limits writes in a place, such as one solved post per bug thread
      with no asks and no jargon, holds back each write it limits, even where another item permits writing there.
    - A standing grant in <evidence> permits what its words name, where they name it: a reply in that thread
      about what was reported or asked there, whether an answer, status, news of a fix, or a question about that
      report. A request to a person about anything else, which the agent, another agent, or a lane thought of,
      needs a grant whose words name that purpose.
    - Scolding the agent for not replying to someone, or for not doing something, is an instruction to do it
      now, where that person is waiting: "why did you not respond to him?" permits responding to him, "you
      never shared the doc there, do that" permits sharing it there, and "why haven't you started?" permits
      reporting that it has started. "never" and "not" in such a complaint describe what was left undone;
      they hold nothing back. Asking for the work a post reports permits reporting it.
    - An instruction naming a thing to share or post covers every place it was owed: each thread where it was
      promised or asked for, and each thread or person the user handed over. "etc." widens it to those
      comparable places, never to arbitrary ones.
    - An instruction to post in a place permits every post there that carries it out: an introduction, a
      context message, or one post of several, whatever its wording, attachments, or count. A channel named as the
      place means its top level: words that name a channel, or grant posting or keep posting there, permit no reply
      in one of its threads unless they say threads or replies. A condition on the agent's own work, such as "once
      these are done", orders the work and holds nothing back; only a
      condition the user must meet, such as "after I review it", holds the write.
    - "don't block", "don't wait on me", "stop asking", and words like them are permission to proceed with the
      pending write, not a hold.
    - The user's latest words on the matter decide: a later go-ahead such as "actually go ahead and post"
      overrides an earlier skip, hold, or edit request.
    - A destination the user named by id, name, or link is that place; words about another destination permit
      nothing here unless they widen to this one, as "etc.", "and the others", or "everywhere it was owed" do.
    - Telling the agent to fix, correct, reword, or edit a message it posted, or complaining that its message is
      wrong or makes no sense, permits editing that message, deleting it, and posting the correction in the same
      place. Once the user has told the agent to write somewhere, its edits there need no second ask.
    - Words that hold the write back, withdraw it, or answer a different proposal permit nothing.
"""


@dataclass(frozen=True, slots=True)
class Message:
    ts: str
    user: str
    user_name: str
    text: str
    from_user: bool = False
    from_claude: bool = False
    from_bot: bool = False

    @property
    def human(self) -> bool:
        return bool(self.user) and not self.from_user and not self.from_claude and not self.from_bot

    @property
    def operator(self) -> bool:
        return self.from_user and not self.from_claude and not self.from_bot


@dataclass(frozen=True, slots=True)
class Thread:
    messages: tuple[Message, ...] = ()
    readable: bool = True
    stale: bool = False


def holds_name(text: str, name: str) -> bool:
    return holds(text.casefold(), name.casefold())


def names_person(text: str, name: str) -> bool:
    parts = name.split()
    return any(holds_name(text, form) for form in (name, parts[0], f"{parts[0]} {parts[-1][0]}") if form)


@dataclass(frozen=True, slots=True)
class Conversation:
    channel: str
    thread: str
    peer: str = ""
    peer_name: str = ""
    peer_names: tuple[str, ...] = ()
    ts: str = ""
    channel_wide: bool = False
    linkable: bool = True
    own: bool = False

    @property
    def place(self) -> dict[str, str]:
        return {"channel": self.peer or self.channel, "thread": "" if self.peer else self.thread}

    @property
    def where(self) -> str:
        return f"{self.channel}, thread {self.thread}" if self.thread else self.channel

    @property
    def stamps(self) -> tuple[str, ...]:
        return (self.thread, f"p{self.thread.replace('.', '')}") if self.thread else ()

    def named_in(self, text: str, channel_names: list[str]) -> bool:
        places = (self.channel, self.peer, *channel_names, *self.stamps)
        return any(holds_name(text, place) for place in places if place) or any(names_person(text, name) for name in self.peer_names)

    def linked_in(self, text: str, replies: frozenset[str]) -> bool:
        if not self.linkable:
            return False
        return any(channel == self.channel and (self.channel_wide or thread == self.thread or ts in replies or ts == self.ts) for channel, thread, ts in slack_links(text) if ts)

    def dms_linked_in(self, text: str) -> set[str]:
        if not self.linkable or not self.peer or self.thread:
            return set()
        return {channel for channel, _, ts in slack_links(text) if ts and channel.startswith("D") and channel != self.channel}


def slack_links(text: str) -> list[tuple[str, str, str]]:
    return [permalink(url.rstrip(".,;:!?")) for url in SLACK_URL.findall(PASTED.sub(" ", text))]


class ChannelRead(dict[str, Any]):
    @property
    def names(self) -> list[str]:
        return [f"#{name}" for name in (self["name"], *self.get("previous_names", []))] if self else []


def cc_slack(*args: str) -> dict[str, Any]:
    done = subprocess.run([CC_SLACK, *args], capture_output=True, text=True, timeout=5)
    if done.returncode or not done.stdout:
        raise RuntimeError(f"`cc-slack {args[0]}` exited {done.returncode}: {done.stderr.strip() or 'no output'}")
    return json.loads(done.stdout)


def conversation(write: Write) -> Conversation:
    if write.verb not in ("post", "edit", "delete"):
        return Conversation(write.channel, write.thread, channel_wide=True, linkable=write.verb in LINKED_VERBS)
    peer = cc_slack("dm-peer", "--channel", write.channel)
    name = str(peer.get("peer_name") or "")
    names = tuple(dict.fromkeys(found for found in (name, *peer.get("peer_names", [])) if found))
    return Conversation(write.channel, write.thread or message_thread(write), str(peer.get("peer") or ""), name, names, write.ts, own=bool(peer.get("own")))


def message_thread(write: Write) -> str:
    if not write.ts:
        return ""
    if not (out := capture([CC_SLACK, "thread", "--channel", write.channel, "--thread", write.ts], timeout=5)):
        return ""
    message = next((found for found in json.loads(out)["messages"] if found.get("ts") == write.ts), {})
    parent = str(message.get("thread_ts") or "")
    return "" if parent == write.ts else parent


def read_thread(conv: Conversation) -> Thread:
    if not conv.thread:
        return Thread()
    if not (out := capture([CC_SLACK, "thread", "--channel", conv.channel, "--thread", conv.thread], timeout=5)):
        return Thread(readable=False)
    read = json.loads(out)
    return Thread(
        tuple(
            Message(message["ts"], message.get("user", ""), message.get("user_name", ""), message.get("text", ""), bool(message.get("from_user")), bool(message.get("from_claude")), bool(message.get("from_bot")))
            for message in read["messages"]
        ),
        stale=bool(read.get("stale")),
    )


def conversation_of(ctx: Context) -> Conversation:
    return ctx.fact(Conversation, lambda ctx: conversation(pending(live(ctx))[0]))


def thread_of(ctx: Context) -> Thread:
    return ctx.fact(Thread, lambda ctx: read_thread(conversation_of(ctx)))


def channel_of(ctx: Context) -> ChannelRead:
    def read(ctx: Context) -> ChannelRead:
        conv = conversation_of(ctx)
        if conv.peer:
            return ChannelRead()
        try:
            return ChannelRead(cc_slack("channel-info", "--channel", conv.channel))
        except CHECK_FAILURES:
            return ChannelRead()

    return ctx.fact(ChannelRead, read)


def live(ctx: Context) -> ToolHookEvent:
    if not isinstance(ctx.evt, ToolHookEvent):
        raise TypeError("a Slack context built without its conversation needs the tool event")
    return ctx.evt


@dataclass(frozen=True, slots=True)
class Linked:
    def matches(self, text: str, ctx: Context) -> bool:
        conv = conversation_of(ctx)
        replies = frozenset(message.ts for message in thread_of(ctx).messages if message.ts != conv.thread)
        return conv.linked_in(text, replies) or any(cc_slack("dm-peer", "--channel", dm).get("peer") == conv.peer for dm in conv.dms_linked_in(text))


@dataclass(frozen=True, slots=True)
class Named:
    def matches(self, text: str, ctx: Context) -> bool:
        conv = conversation_of(ctx)
        return conv.named_in(text, channel_of(ctx).names)


class Refusal(str):
    pass


@dataclass(frozen=True, slots=True)
class Judged(CustomTicketCondition):
    judge: Decide

    def evidence(self, ctx: Context) -> list[Evidence] | None:
        if not (items := self.judge.items(ctx)):
            return None
        try:
            verdict = self.judge.verdict(ctx, items)
        except Undecided as exc:
            raise TimeoutError(exc.cause) from exc
        if not verdict.allow:
            ctx.facts[Refusal] = Refusal(verdict.explained)
            return None
        return [item for item in cited(items, verdict.relied_on) if item.source != SCOPE] or None


def answers_naming(evt: BaseHookEvent, term: str) -> list[dict[str, Any]]:
    done = subprocess.run(["ccn", "answer", "search", term, "--json", "--limit", "0", "-R", str(evt.cwd or reqenv.cwd())], capture_output=True, text=True, timeout=RULINGS_TIMEOUT, env=reqenv.env_map(), check=False)
    if done.returncode == 0:
        return json.loads(done.stdout)
    if NO_STORE in done.stderr:
        return []
    raise RuntimeError(f"`ccn answer search` exited {done.returncode}: {done.stderr.strip() or 'no output'}")


class StoreReads(dict[str, list[dict[str, Any]]]):
    pass


def live_answers(ctx: Context, term: str) -> list[dict[str, Any]]:
    reads = ctx.fact(StoreReads, lambda _: StoreReads())
    if term not in reads:
        reads[term] = sorted((answer for answer in answers_naming(live(ctx), term) if not answer.get("superseded_by")), key=written_at)
    return reads[term]


def standing(answer: dict[str, Any], channel: str, thread: str) -> bool | None:
    tags = set(answer.get("tags", ()))
    if thread and f"{REVOKE_LABEL}{channel}/{thread}" in tags:
        return False
    if thread and f"{GRANT_LABEL}{channel}/{thread}" in tags:
        return True
    if thread and f"{GRANT_LABEL}{channel}{EVERY_THREAD}" in tags:
        return True
    if any(tag.startswith((f"{GRANT_LABEL}{channel}/", f"{REVOKE_LABEL}{channel}/")) for tag in tags):
        return None
    if f"{REVOKE_LABEL}{channel}" in tags:
        return False
    if f"{GRANT_LABEL}{channel}" in tags:
        return None if thread else True
    if not ASKED <= tags or not holds(f"{answer['title']}\n{answer['body']}", channel):
        return None
    picked = answer["body"].partition("\nOptions:")[0]
    if STOP_POSTING.search(picked):
        return False
    return True if KEEP_POSTING.search(picked) and not thread else None


@dataclass(frozen=True, slots=True)
class Standing:
    answer: dict[str, Any] | None = None
    grants: bool = False


def decided(ctx: Context, channel: str, thread: str) -> Standing:
    found = [
        Standing(answer, grants)
        for answer in live_answers(ctx, channel)
        if (grants := standing(answer, channel, thread)) is not None and (not grants or BOARD_ANSWER not in answer.get("tags", ()) or Named().matches(answer["title"], ctx))
    ]
    return found[-1] if found else Standing()


def announced(conv: Conversation) -> set[str]:
    messages = read_thread(conv).messages
    if not any(message.from_claude and AGENT_POSTER.search(message.user_name) for message in messages):
        return set()
    return {opened[1] for message in messages if message.from_bot and not AGENT_POSTER.search(message.user_name) for opened in ROOM_OPENED.finditer(message.text)}


def incident_room(ctx: Context, room: str) -> Standing:
    granted = {tag.removeprefix(GRANT_LABEL) for answer in live_answers(ctx, GRANT_LABEL) for tag in answer.get("tags", ()) if tag.startswith(GRANT_LABEL) and "/" not in tag}
    for channel in sorted(granted - {room}):
        found = decided(ctx, channel, "")
        if not found.grants:
            continue
        pages = cc_slack("history", "--channel", channel, "--json", "--limit", ROOM_PAGES)["messages"]
        if any(int(page.get("reply_count") or 0) and room in announced(Conversation(channel, page["ts"])) for page in pages):
            return found
    return Standing()


def standing_of(ctx: Context) -> Standing:
    def newest(ctx: Context) -> Standing:
        channel, thread = ctx.place["channel"], ctx.place["thread"]
        found = decided(ctx, channel, thread)
        return found if found.answer is not None or thread else incident_room(ctx, channel)

    return ctx.fact(Standing, newest)


@dataclass(frozen=True, slots=True)
class StandingGrant(CustomTicketCondition):
    def evidence(self, ctx: Context) -> list[Evidence] | None:
        found = standing_of(ctx)
        return [ruling_evidence(found.answer)] if found.answer is not None and found.grants else None


@dataclass(frozen=True, slots=True)
class UnjudgedGrant(CustomTicketCondition):
    def evidence(self, ctx: Context) -> list[Evidence] | None:
        found = standing_of(ctx)
        if found.answer is None or not found.grants or f"{GRANT_LABEL}{ctx.place['channel']}" in found.answer.get("tags", ()):
            return None
        return [ruling_evidence(found.answer)]


def recorded_for(answer: dict[str, Any], channel: str) -> bool:
    return f"{GRANT_LABEL}{channel}" in answer.get("tags", ()) or holds(f"{answer['title']}\n{answer['body']}", channel)


@dataclass(frozen=True, slots=True)
class RecordedWords(CustomTicketCondition):
    def evidence(self, ctx: Context) -> list[Evidence] | None:
        found = standing_of(ctx)
        if found.answer is not None and not found.grants:
            return None
        channel, thread = ctx.place["channel"], ctx.place["thread"]
        answers = [answer for answer in live_answers(ctx, channel) if recorded_for(answer, channel)]
        scopes = [Evidence(id=f"scope-{answer['id'][:7]}", source=SCOPE, quote="", detail=SCOPE_LINE.format(answer=f"ccn:{answer['id'][:7]}")) for answer in answers if thread and standing(answer, channel, "") and standing(answer, channel, thread) is None]
        return [*map(ruling_evidence, answers), *scopes] or None


@dataclass(frozen=True, slots=True)
class NotRevoked(CustomTicketCondition):
    def evidence(self, ctx: Context) -> list[Evidence] | None:
        found = standing_of(ctx)
        return None if found.answer is not None and not found.grants else []


def solved_grants(ctx: Context) -> list[dict[str, Any]]:
    channel = ctx.place["channel"]
    grant = f"{GRANT_LABEL}{channel}{SOLVED}"
    return [answer for answer in live_answers(ctx, channel) if grant in answer.get("tags", ())] if ctx.place["thread"] else []


def plain(text: str) -> bool:
    return not ASKS.search(text) and not check_post().check(text)


@dataclass(frozen=True, slots=True)
class SolvedLine(CustomTicketCondition):
    def evidence(self, ctx: Context) -> list[Evidence] | None:
        write = pending(live(ctx))[0]
        grants = solved_grants(ctx)
        if not grants or write.verb != "post" or write.files or write.fields.get("blocks") or not plain(write.text):
            return None
        read = thread_of(ctx)
        if not read.readable or read.stale or any(message.from_claude for message in read.messages):
            return None
        return [ruling_evidence(grants[-1])]


def written_here(ctx: Context) -> list[Ticket]:
    return [ticket for ticket in store.tickets(KIND, ctx.trees) if ticket.place == dict(ctx.place) and store.live(ticket)]


@dataclass(frozen=True, slots=True)
class Unspent(CustomTicketCondition):
    within: UserSaid | UserAnswered

    def evidence(self, ctx: Context) -> list[Evidence] | None:
        spent = {item.id for ticket in store.tickets(KIND, ctx.trees) for item in ticket.evidence}
        return [item for item in found(self.within, ctx) or () if item.id not in spent] or None


@dataclass(frozen=True, slots=True)
class EarlierWords(CustomTicketCondition):
    def evidence(self, ctx: Context) -> list[Evidence] | None:
        return [item for ticket in written_here(ctx) for item in ticket.evidence if item.source in HIS_WORDS] or None


@dataclass(frozen=True, slots=True)
class OwnPost(CustomTicketCondition):
    def evidence(self, ctx: Context) -> list[Evidence] | None:
        write = pending(live(ctx))[0]
        if write.verb not in ("edit", "delete"):
            return None
        written = written_here(ctx)
        conv = conversation_of(ctx)
        if not written and not conv.own:
            return None
        messages = thread_of(ctx).messages if conv.thread else read_thread(Conversation(conv.channel, write.ts)).messages
        if not any(message.ts == write.ts and message.from_claude for message in messages):
            return None
        if conv.own:
            return [Evidence(id=f"message:{write.ts}", source="message", quote=f"message {write.ts} is the agent's own, in the user's own DM with the app")]
        return [Evidence(id=f"ticket:{written[-1].id}", source="ticket", quote=f"this session wrote in {conv.where} under ticket {written[-1].id} ({written[-1].rule}), and the message is the agent's own")]


@dataclass(frozen=True, slots=True)
class TopLevel(CustomTicketCondition):
    def evidence(self, ctx: Context) -> list[Evidence] | None:
        return None if ctx.place["thread"] else []


def channel_name(channel: dict[str, Any]) -> str:
    formerly = ", ".join(f"#{name}" for name in channel.get("previous_names", []))
    return f"#{channel['name']} ({channel['id']})" + (f", formerly {formerly}" if formerly else "")


@dataclass(frozen=True, slots=True)
class Destination:
    tag: str = "destination"
    required: bool = False

    def content(self, evt: BaseHookEvent) -> str:
        ctx = Context.current(evt)
        conv = conversation_of(ctx)
        if conv.peer:
            names = ", ".join(conv.peer_names) or conv.peer_name
            return f"the direct message conversation with {names} ({conv.peer}); a write here is a message to that person"
        channel = channel_of(ctx)
        place = channel_name(channel) if channel else conv.channel
        if not conv.thread:
            return place
        thread = thread_of(ctx)
        if not thread.readable:
            return f"{place}, thread {conv.thread}"
        opener = next((message for message in thread.messages if message.ts == conv.thread), thread.messages[0])
        opened = f"opened by the user, {opener.user_name}" if opener.operator else f"opened by {opener.user_name}, whose words are never the user's"
        humans = [message for message in thread.messages if message.human and message is not opener][-LATEST_SHOWN:]
        latest = "".join(f"\n{message.user_name}: {message.text[:SHOWN]}" for message in humans)
        return f"{place}, thread {conv.thread}, {opened}: {opener.text[:SHOWN]}" + (f"\nlatest replies from others in the thread, never the user's words:{latest}" if latest else "")


def card_id(write: Write, place: dict[str, str]) -> str:
    digest = hashlib.sha256(json.dumps([write.verb, write.fields, write.users], sort_keys=True, default=str).encode()).hexdigest()[:12]
    return f"slack:{place['channel']}:{place['thread']}:{digest}" if SLACK_ID.fullmatch(place["channel"]) else f"slack-{digest}"


def tool_out(evt: BaseHookEvent, argv: list[str]) -> str | None:
    done = subprocess.run(argv, capture_output=True, text=True, timeout=5, env=reqenv.env_map(), cwd=evt.cwd, check=False)
    return done.stdout if done.returncode == 0 and done.stdout.strip() else None


def board_tap(ctx: Context) -> Evidence | None:
    evt = live(ctx)
    card = card_id(pending(evt)[0], ctx.place)
    rows = sorted((answer for answer in answers_naming(evt, f"{BOARD_CARD}{card}") if f"{BOARD_CARD}{card}" in answer.get("tags", ())), key=written_at)
    session = next((tag.removeprefix(BOARD_SESSION) for tag in rows[-1]["tags"] if tag.startswith(BOARD_SESSION)), None) if rows else None
    if session is None or (out := tool_out(evt, ["cc-present", "outcomes", "--block", card, "--session", session])) is None:
        return None
    shown = json.loads(out)
    drawn = next((block for block in shown["doc"]["blocks"] if block["id"] == card), None)
    if drawn is None:
        return None
    text = "\n".join([drawn.get("title", ""), *(child["md"] for child in drawn.get("children", ()) if child.get("type") == "markdown")])
    picks = [answer["payload"] for answer in shown.get("answers") or () if answer["origin"] == "human" and answer["type"] == "choice.selected" and answer["payload"]["blockId"] == f"{card}-answer"]
    choice = next((child for child in drawn.get("children", ()) if child["id"] == f"{card}-answer"), {})
    labels = {option["id"]: option["label"] for option in choice.get("options", ())}
    if not picks or [labels.get(option) for option in picks[0].get("optionIds") or ()] != [APPROVED] or not Named().matches(text, ctx):
        return None
    return Evidence(id=f"board:{session}:{card}", source="board", quote="Yes", detail=f"board card: {text}")


@dataclass(frozen=True, slots=True)
class BoardTapped(CustomTicketCondition):
    def evidence(self, ctx: Context) -> list[Evidence] | None:
        try:
            found = board_tap(ctx)
        except CHECK_FAILURES:
            return None
        return None if found is None else [found]


def owner_ledger(evt: BaseHookEvent) -> str:
    drives = Path(reqenv.getenv("CLAUDE_CONFIG_DIR") or Path.home() / ".claude") / DRIVES
    if drive := reqenv.getenv("CLAUDE_LONG_RUNNING_DRIVE"):
        paths = [named] if (named := drives / f"{drive}.json").exists() else []
    else:
        paths = sorted(drives.glob("*.json"))
    for path in paths:
        entry = json.loads(path.read_text())
        if drive or evt.session_id in entry["sessions"]:
            return (path.parent / entry["state_dir"] / LEDGER).read_text()
    return ""


@dataclass(frozen=True, slots=True)
class OwnerOrdered(CustomTicketCondition):
    def evidence(self, ctx: Context) -> list[Evidence] | None:
        cited = {row for text in (*(item.quote for item in ctx.said), *(touch.text for touch in ctx.touches)) for row in OWNER_ROW.findall(text)}
        if not cited:
            return None
        try:
            ledger = owner_ledger(live(ctx))
        except CHECK_FAILURES:
            return None
        found = [
            Evidence(id=f"owner:{row}", source="owner-ledger", quote=words.replace("<br>", "\n").replace(r"\|", "|"), detail="the owner's own message, as the communicator's owner ledger holds it")
            for row, words in LEDGER_ROW.findall(ledger)
            if row in cited
        ]
        return found or None


def channel_texts(source: EvidenceSession) -> list[str]:
    if source.indexed:
        return [
            record["text"]
            for record in source.session.query({"kind": "meta_messages", "contains": SLACK_MESSAGE})
            if (record["kind"] == "user" and record["is_meta"] and record["source_tool_use_id"] is None and record["source_tool_assistant_uuid"] is None) or (record["kind"] == "queued" and record["origin"] == "channel")
        ]
    return [
        event.text if isinstance(event, UserEvent) else event.detail.prompt or ""
        for turn in source.session.turns
        for event in turn.events
        if (isinstance(event, UserEvent) and event.meta.is_meta and event.source_tool_use_id is None and event.source_tool_assistant_uuid is None)
        or (isinstance(event, AttachmentEvent) and isinstance(event.detail, QueuedCommand) and event.detail.origin == "channel")
    ]


def order(channel: str, ts: str, text: str, detail: str) -> Evidence:
    return Evidence(id=f"operator:{channel}/{ts}", source=DM_ORDER, quote=text, said_at=datetime.fromtimestamp(float(ts), UTC), detail=detail)


def delivered_orders(ctx: Context) -> list[Evidence]:
    with sessions(ctx.sources) as read:
        texts = [text for source in read if source.prompts_are_user for text in channel_texts(source)]
    lines = [json.loads(match["payload"]) for text in texts if (match := ORDER_LINE.fullmatch(text.strip()))]
    return [order(line["channel_id"], line["ts"], line["text"], "the user's own Slack message to this session, as cc-slack delivered it") for line in lines if line.get("operator")]


def own_dm(channel: str) -> bool:
    try:
        return bool(cc_slack("dm-peer", "--channel", channel).get("own"))
    except CHECK_FAILURES:
        return False


def linked_orders(ctx: Context, delivered: set[str]) -> list[Evidence]:
    texts = [*(item.quote for item in ctx.said), *(touch.text for touch in ctx.touches)]
    links = {(channel, thread or f"{seconds}.{micros}", f"{seconds}.{micros}") for text in texts for channel, seconds, micros, thread in DM_LINK.findall(text) if f"operator:{channel}/{seconds}.{micros}" not in delivered}
    owned = {channel for channel in {link[0] for link in links} if own_dm(channel)}
    found = []
    for channel, thread in {(channel, thread) for channel, thread, _ in links if channel in owned}:
        try:
            messages = read_thread(Conversation(channel, thread)).messages
        except CHECK_FAILURES:
            continue
        found += [
            order(channel, message.ts, message.text, "the user's own message in their DM with the app, read back from Slack")
            for message in messages
            if (channel, thread, message.ts) in links and message.operator
        ]
    return found


def asked_here(ctx: Context) -> list[Evidence]:
    conv = conversation_of(ctx)
    return [order(conv.channel, message.ts, message.text, ASKED_HERE) for message in thread_of(ctx).messages if message.operator]


def dm_orders(ctx: Context) -> list[Evidence]:
    delivered = delivered_orders(ctx)
    orders = [*delivered, *linked_orders(ctx, {item.id for item in delivered}), *asked_here(ctx)]
    return list({item.id: item for item in orders}.values())


@dataclass(frozen=True, slots=True)
class DmOrdered(CustomTicketCondition):
    def evidence(self, ctx: Context) -> list[Evidence] | None:
        return dm_orders(ctx) or None


SLACK = Tickets(
    KIND,
    ("channel", "thread"),
    Reach.ONCE,
    rules=(
        Rule("standing-grant", And(TopLevel(), UnjudgedGrant())),
        Rule("user-linked", Unspent(UserSaid(Linked()))),
        Rule("asked-user", Or(Unspent(UserAnswered(Named(), Holds("text"), approved=True)), And(NotRevoked(), BoardTapped()))),
        Rule("own-post", OwnPost()),
        Rule(SOLVED_LINE, And(NotRevoked(), SolvedLine())),
        Rule("decide", Judged(Decide(RULES, second_opinion=False, over=(UserSaid(), UserAnswered(approved=False), OwnerOrdered(), DmOrdered(), EarlierWords(), StandingGrant(), RecordedWords()), contexts=[Destination()], transcript=JUDGED_TURNS, root_transcript=JUDGED_TURNS, tool_results=True))),
    ),
    deny="Ask the user.",
    window=WINDOW,
    machine=(SHORTCUT_BRIEF,),
    hook=GATE,
)


def shown(write: Write) -> str:
    return "\n".join([write.text, *(f"attached: {path}" for path in write.files)])


def card_body(text: str) -> str:
    if len(text) <= CARD_BODY:
        return text
    kept = CARD_BODY - 40
    return f"{text[:kept]}\n… {len(text) - kept} more characters"


def clock(ts: str) -> str:
    at = datetime.fromtimestamp(float(ts))
    day = "" if at.date() == datetime.now().date() else f"{at:%b} {at.day} at "
    return day + at.strftime("%I:%M%p").lstrip("0").lower()


@dataclass(frozen=True, slots=True)
class Place:
    name: str
    detail: str = ""


def place_words(ctx: Context) -> Place:
    conv = conversation_of(ctx)
    if conv.peer:
        return Place(f"a direct message to {', '.join(conv.peer_names) or conv.peer_name or conv.peer}")
    channel = channel_of(ctx)
    named = f"#{channel['name']}" if channel else conv.channel
    if not conv.thread:
        return Place(named)
    opener = next((message for message in thread_of(ctx).messages if message.ts == conv.thread), None)
    if opener is None:
        return Place(f"the {named} thread")
    name = f"the {named} thread {opener.user_name} started"
    return Place(name, f"{name} at {clock(conv.thread)}: {' '.join(opener.text.split())[:OPENER_SHOWN]}")


def ask(write: Write, place: Place, card: str, why: str) -> dict[str, Any]:
    if write.verb in TEXT_VERBS:
        title, body = f"{TEXT_VERBS[write.verb]} in {place.name}?", shown(write)
    else:
        title = f"{OTHER_VERBS[write.verb]} ({place.name})?" if place.name else f"{OTHER_VERBS[write.verb]}?"
        body = ", ".join(str(value) for value in (*write.fields.values(), *write.users) if isinstance(value, str) and value)
    body = f"Where: {place.detail}\n\n{body}" if place.detail else body
    body = f"Why: {why}\n\n{body}" if why else body
    return {"status": "needs_you", "title": title, "body": card_body(body), "options": ["Yes", "No"], "recommended": "Yes", "waits": card}


def board_card(card: dict[str, Any]) -> dict[str, Any]:
    context = [{"id": f"{card['waits']}-context", "type": "markdown", "md": card["body"]}] if card["body"] else []
    answers = {"id": f"{card['waits']}-answer", "type": "choice", "options": [{"id": "yes", "label": "Yes", "recommended": True}, {"id": "no", "label": "No"}]}
    return {"id": card["waits"], "type": "card", "title": card["title"], "children": [*context, answers]}


def refused(write: Write, ctx: Context) -> str:
    card = ask(write, place_words(ctx), card_id(write, ctx.place), ctx.facts.get(Refusal, ""))
    return REFUSED.format(board=json.dumps(board_card(card), ensure_ascii=False, indent=2), ask=json.dumps(card, ensure_ascii=False, indent=2))


def context_of(evt: ToolHookEvent, write: Write, conv: Conversation) -> Context:
    ctx = SLACK.context(evt, Proposal(conv.place, write.payload, f"{write.verb} in {conv.where}"))
    ctx.facts[Conversation] = conv
    return ctx


@contextmanager
def check_deadline() -> Iterator[None]:
    left = reqenv.seconds_left()
    with reqenv.deadline_in(CHECK_SECONDS if left is None else min(CHECK_SECONDS, left - DEADLINE_MARGIN)):
        yield


def named_ticket(evt: PreToolUseEvent, ctx: Context, ticket: Ticket) -> HookResult:
    fields = {"ticket_id": ticket.id, "ticket_tree": ticket.tree, "ticket_agent": ctx.agent, "ticket_call": ctx.call, "ticket_fingerprint": ctx.action.fingerprint}
    if evt.tool_name != "Bash":
        return evt.rewrite(dict(evt.input.raw) | fields)
    call = cli_calls(evt)[0][0]
    head, _, tail = evt.input.raw["command"].partition(call.command.raw)
    flags = " ".join(f"--{TICKET_FLAGS[name]} {shlex.quote(value)}" for name, value in fields.items())
    return evt.rewrite_command(f"{head}{call.command.raw} {flags}{tail}")


def unchecked(evt: PreToolUseEvent, write: Write, conv: Conversation, exc: Exception) -> HookResult:
    cause = f"{type(exc).__name__}: {str(exc).partition(chr(10))[0]}"[:SHOWN]
    logger.bind(hook=GATE, cause=cause).opt(exception=True).warning("slack write check failed; the write goes ahead unchecked")
    note = UNFINISHED.format(gate=GATE, cause=cause)
    if not write.daemon:
        return evt.allow(system_message=note)
    ctx = context_of(evt, write, conv)
    return replace(named_ticket(evt, ctx, SLACK.unchecked(ctx, cause)), system_message=note)


def written_files(evt: PreToolUseEvent) -> set[str]:
    redirected = {redirect.target for call in evt.cmd.calls() for redirect in call.redirects if redirect.op.lstrip("&").startswith(">")}
    return redirected | {arg for call in evt.cmd.calls("tee") for arg in call.args if not arg.startswith("-")}


def plan_refusal(path: Path) -> str | None:
    try:
        entries = json.loads(path.read_text())
    except OSError as exc:
        return f"{path.name} cannot be read ({exc.strerror})"
    except ValueError as exc:
        return f"{path.name} does not parse ({str(exc)[:60]})"
    if not isinstance(entries, list) or not all(isinstance(entry, dict) for entry in entries):
        return f"{path.name} is not a JSON list of entries"
    if bad := next((entry for entry in entries if entry.get("verdict") not in PLAN_VERDICTS), None):
        return f"entry {str(bad.get('ts'))[:20]} has verdict {str(bad.get('verdict'))[:20]!r}"
    if len(channels := sorted({permalink(str(entry.get("permalink") or ""))[0] for entry in entries})) != 1:
        return f"it names {len(channels)} channels ({', '.join(channels[:3])})" if channels else "it names no channel"
    if not channels[0].startswith("D"):
        return f"its channel {channels[0]} is not a DM"
    return None


def plan_checked(evt: PreToolUseEvent, write: Write) -> HookResult:
    named = str(write.fields.get("plan") or "")
    if named in written_files(evt):
        return evt.block(DRAFTED.format(flag="plan"))
    path = Path(named).expanduser()
    reason = plan_refusal(path if path.is_absolute() else evt.cwd / path)
    logger.bind(hook=GATE, agent=evt.agent_id or "main", tool=evt.tool_name, allowed=reason is None, detail=reason or named).info("slack plan checked")
    return evt.allow() if reason is None else evt.block(UNPLANNED.format(reason=reason))


def slack_writes_need_permission(evt: PreToolUseEvent) -> HookResult | None:
    writes = pending(evt)
    if len(writes) > 1 or not writes[0].readable:
        return evt.block(UNREAD)
    write = writes[0]
    if write.verb == "apply":
        return plan_checked(evt, write)
    if write.fields.get("text_file") in written_files(evt):
        return evt.block(DRAFTED.format(flag="text-file"))
    conv = Conversation(write.channel, write.thread)
    try:
        with check_deadline():
            conv = conversation(write)
            ctx = context_of(evt, write, conv)
            decided = SLACK.decide(ctx)
            verdict = decided if isinstance(decided, Deny) else decided.ticket
            withdrawn = isinstance(verdict, Ticket) and (found := standing_of(ctx)).answer is not None and not found.grants and written_at(found.answer) > verdict.created
    except CHECK_FAILURES as exc:
        return unchecked(evt, write, conv, exc)
    if withdrawn:
        return evt.block(refused(write, ctx), system_message=f"{GATE}: a revocation newer than ticket {verdict.id} withdraws it.")
    logger.bind(hook=GATE, agent=evt.agent_id or "main", tool=evt.tool_name, allowed=isinstance(verdict, Ticket), detail=verdict.detail if isinstance(verdict, Deny) else f"{verdict.id} ({verdict.rule})").info("slack write checked")
    if isinstance(verdict, Deny):
        return evt.block(refused(write, ctx), system_message=f"{GATE}: {verdict.detail}" if verdict.detail else None)
    if not write.daemon:
        return evt.allow()
    return named_ticket(evt, ctx, verdict)


class WritesSlack:
    def check(self, evt: BaseHookEvent) -> bool:
        return isinstance(evt, ToolHookEvent) and permissions().enabled and bool(pending(evt))


P = "mcp__plugin_cc-slack_cc-slack__"
CLI = "cc-slack"
SLACK_POST = "slack-post"
SLACK_POST_CALL = re.compile(r"(?:^|[\s/])slack-post\s")
FOLLOWERS = "C0APPFOLLOW"
PETS = "C0PETS00001"
DANA_THREAD = "1791859635.513139"
DANA = {"channel_id": FOLLOWERS, "thread_ts": DANA_THREAD, "no_watch": True}
DANA_TEXT = "Good catch Dana! The SQL viewer now stops this before it runs and tells you why, plus the deleted_at soft-delete fix: https://github.com/acme/app/pull/36126"
DANA_URL = f"https://acme.slack.com/archives/{FOLLOWERS}/p{DANA_THREAD.replace('.', '')}"
LINKED_URL = f"https://acme.slack.com/archives/{FOLLOWERS}/p1791900000000300?thread_ts={DANA_THREAD}&cid={FOLLOWERS}"
DANA_ASKED = f'Post this in {FOLLOWERS}, thread {DANA_THREAD}? "{DANA_TEXT}"'
DANA_CARD = "Post this in the #app-followers thread Dana started?"
DANA_OPENED = {"ts": DANA_THREAD, "user": "U0DANA00001", "user_name": "Dana", "text": "the SQL viewer ran a delete with no soft-delete filter"}
DANA_FOLLOWED_UP = {"ts": "1791900000.000100", "user": "U0DANA00001", "user_name": "Dana", "text": "any update on this?"}
BOT_ANSWERED = {"ts": "1791900000.000200", "user": "U0BOT000001", "user_name": "Alex (claude)", "text": "fix is out", "from_claude": True}
PRIYA_USER = "U0PRIYA0001"
PRIYA_ASKED = "Should I DM priya to ask her to try posting the doc link again, or will you paste it yourself?"
PRIYA_DM_TEXT = "Hey Priya, could you try posting the design-doc link in #docs-test again?"
PRIYA_EDIT = {"channel_id": "C0RELEASES1", "ts": "1792445493.848219", "text": "#38054 and #38070 now queue together"}
DIGEST_ASKED = f"Post the held digest in {FOLLOWERS} now, and keep posting one update per landing and release from here on?"
DIGEST_OPTIONS = ["Yes, post and keep posting", "Hold"]
DIGEST = {"channel_id": FOLLOWERS, "text": "Catching up: 3 releases and about 70 merged PRs", "no_watch": True}
LANE_BRIEF = [T.user(f"comms-desk: post the held digest in {FOLLOWERS}")]
PROMPTED_AT = datetime(2026, 10, 13, 10, 55, 46, tzinfo=UTC)
NOT_DM = {
    f"{CC_SLACK} dm-peer": json.dumps({"peer": "", "peer_name": ""}),
    f"{CC_SLACK} channel-info": (1, "channel_not_found"),
    f"{CC_SLACK} channel-info --channel {FOLLOWERS}": json.dumps({"id": FOLLOWERS, "name": "app-followers"}),
    f"{CC_SLACK} thread": json.dumps({"messages": [DANA_OPENED]}),
    f"{CC_SLACK} history": json.dumps({"channel_id": FOLLOWERS, "messages": []}),
    "ccn answer search": "[]",
    "cc-present outcomes": "{}",
}
OWN_DM = NOT_DM | {f"{CC_SLACK} dm-peer": json.dumps({"peer": "U0OWNER0001", "peer_name": "Wren", "own": True})}
OWN_DM_EDIT = {"channel_id": "D0OWNDM0001", "ts": BOT_ANSWERED["ts"], "text": "fix is out, edited"}
PRIYA_DM = NOT_DM | {f"{CC_SLACK} dm-peer": json.dumps({"peer": PRIYA_USER, "peer_name": "priya"})}
PRIYA_BY_USER = NOT_DM | {f"{CC_SLACK} dm-peer": (1, "dm-peer needs channel_id (or a Slack url)"), f"{CC_SLACK} dm-peer --channel {PRIYA_USER}": PRIYA_DM[f"{CC_SLACK} dm-peer"]}
SAM_USER = "U0SAMOK0001"
SAM_DM = NOT_DM | {f"{CC_SLACK} dm-peer": json.dumps({"peer": SAM_USER, "peer_name": "Sam Okafor", "peer_names": ["Sam Okafor", "sam.okafor"]})}
HIS_DM_WITH_PRIYA = "D0PRIYADM01"
PRIYA_REPORTED = f"respond to this asap https://acme.slack.com/archives/{HIS_DM_WITH_PRIYA}/p1791900000000400"
PRIYA_FIXED = {"channel_id": PRIYA_USER, "text": "Hey Priya, the fix is live: https://github.com/acme/app/pull/36126"}
PRIYA_GROUP_DM = PRIYA_BY_USER | {f"{CC_SLACK} dm-peer --channel {HIS_DM_WITH_PRIYA}": PRIYA_DM[f"{CC_SLACK} dm-peer"]}
NONE = "none"
JEV_NONE = {"permits": LabelAnswer(NONE, {NONE: 1.0}, 0.9)}
LLM_SILENT = {"error": TimeoutError("the llm judge timed out")}
LLM_REFUSES = {"reason": "nothing the user said permits this write", "allow": False, "refusal": "Ask the user."}
FIXTURES = Path(__file__).with_name("fixtures")
LINKED_LONG_AGO = FIXTURES / "linked_long_ago.jsonl"
LINKED_LONG_AGO_BY_OTHERS = FIXTURES / "linked_long_ago_by_others.jsonl"
ENABLED = {"CLAUDE_PROJECT_DIR": str(FIXTURES / "enabled")}
DISABLED = {"CLAUDE_PROJECT_DIR": str(FIXTURES / "disabled")}
LANDED = {"channel_id": FOLLOWERS, "text": "#38538 landed: Slack writes now run on tickets", "no_watch": True}
KEEP_POSTING_OPTIONS = "Options: Yes, post and keep posting (Recommended) | Yes, digest only | No"
KEPT_POSTING = {
    "id": "6cc14f23f6759d9f",
    "title": f"Post the held digest in {FOLLOWERS} now, and keep posting one update per landing from here on?",
    "body": f"Yes, post and keep posting (Recommended)\n{KEEP_POSTING_OPTIONS}",
    "updated_at": "2026-10-16T20:25:40Z",
    "tags": ["from:owner", "header:Slack grant", "scope:ephemeral", "source:askuserquestion"],
    "superseded_by": None,
}
LABELED_GRANT = {
    "id": "c7409d5f94d250d9",
    "title": "May comms-desk post per-landing updates in the drive channel?",
    "body": "Owner, verbatim: 'no one told you to stop posting to the channel'",
    "updated_at": "2026-10-16T20:16:11Z",
    "tags": ["owner-ruling", "scope:durable", f"slack-grant:{FOLLOWERS}"],
    "superseded_by": None,
}
THREAD_GRANT = LABELED_GRANT | {
    "id": "e1e1e1e1e1e1e1e1",
    "title": f"May the drive reply in {FOLLOWERS} thread {DANA_THREAD}, where a user reported the outage?",
    "tags": ["owner-ruling", "scope:durable", f"slack-grant:{FOLLOWERS}/{DANA_THREAD}"],
}
EVERY_THREAD_GRANT = LABELED_GRANT | {
    "id": "e3e3e3e3e3e3e3e3",
    "title": "May bug-thread replies go out with no approval per thread?",
    "tags": ["scope:durable", f"slack-grant:{FOLLOWERS}{EVERY_THREAD}"],
}
SOLVED_GRANT = LABELED_GRANT | {
    "id": "e4e4e4e4e4e4e4e4",
    "title": "Does the one solved line in a bug's thread need his permission each time?",
    "body": "No: every bug can have one solved post that is simple, with no asks and no jargon",
    "tags": ["scope:durable", f"slack-grant:{FOLLOWERS}{SOLVED}"],
}
SOLVED_TEXT = "This is fixed in version 0.162.3623. Your existing chats open again, with their drafts intact."
KEPT_IN_CHANNEL = {"id": "f00df00df00df00d", "title": "Where do drive updates go?", "body": f"Keep posting in {FOLLOWERS}", "updated_at": "2026-10-01T00:00:00Z", "tags": ["from:owner", "scope:durable"], "superseded_by": None}
REVOKED = LABELED_GRANT | {"id": "d0d0d0d0d0d0d0d0", "updated_at": "2026-10-17T01:00:00Z", "tags": ["owner-ruling", f"slack-revoke:{FOLLOWERS}"]}
ROOM = "C0ROOM00001"
ROOM_POST = {"channel_id": ROOM, "text": "Mechanism: the monitor reads a snapshot taken before runs are assigned; the fix is releasing", "no_watch": True}
PAGES_HISTORY = {f"{CC_SLACK} history": json.dumps({"channel_id": FOLLOWERS, "messages": [{"ts": "1793580376.381769", "user_name": "Wadsworth", "from_bot": True, "text": "Run queue backlog not draining", "reply_count": 6}]})}


DRIVE_ANSWERED = {"ts": "1793580741.495599", "user": "", "user_name": "Yasyf (agent)", "from_claude": True, "from_bot": True, "text": "Mechanism (4:52 PM): this reads as a signal defect, not a stuck queue"}


def room_opened(room: str, *, from_bot: bool = True, by: str = "Wadsworth", answered: bool = True) -> dict[str, str]:
    opened = {"ts": "1793581646.621179", "user": "", "user_name": by, "from_claude": from_bot, "from_bot": from_bot, "text": f":mega: *Incident opened: <#{room}>*\nThe chart, incident doc and every update are there"}
    return thread_read(*([DRIVE_ANSWERED] if answered else []), opened)


def jev(approval: str) -> dict[str, object]:
    return {"permits": LabelAnswer(approval, {approval: 0.97, NONE: 0.03}, 0.9)}


def ruled(*answers: dict[str, Any]) -> dict[str, str]:
    return NOT_DM | {"ccn answer search": json.dumps(list(answers))}


def thread_read(*messages: dict[str, Any], stale: bool = False) -> dict[str, str]:
    return {f"{CC_SLACK} thread": json.dumps({"messages": list(messages), "stale": stale})}


def owner_answered(question: str, options: list[str] | dict[str, str], label: str, *, notes: str = "", id: str | None = None, described: dict[str, str] | None = None) -> list[dict[str, Any]]:
    previews = options if isinstance(options, dict) else dict.fromkeys(options, "")
    listed = [{"label": option, "description": (described or {}).get(option, "")} | ({"preview": preview} if preview else {}) for option, preview in previews.items()]
    questions = [{"question": question, "header": "Slack", "multiSelect": False, "options": listed}]
    call = T.tool("AskUserQuestion", id=id, questions=questions)
    annotation = ({"notes": notes} if notes else {}) | ({"preview": previews[label]} if previews.get(label) else {})
    annotations = {question: annotation} if annotation else {}
    result = T.result(f'User has answered your questions: "{question}"="{label}".', of=call)
    return [T.assistant(call), T.user(result, toolUseResult={"questions": questions, "answers": {question: label}, "annotations": annotations})]


def card_tapped(question: str, label: str, options: tuple[str, ...] = ("Yes", "No"), action_ts: str = "1791950000.000900") -> list[dict[str, Any]]:
    payload = json.loads((FIXTURES / "slack_action.json").read_text()) | {"action_ts": action_ts, "label": label, "question": question, "options": list(options)}
    tap = f'<channel source="plugin:cc-slack:cc-slack" subject_id="s1" type="slack.action">\n{json.dumps(payload, separators=(",", ":"))}\n</channel>'
    return [T.assistant("asked on the card"), T.user(tap, isMeta=True, origin={"kind": "channel", "server": "plugin:cc-slack:cc-slack"})]


def touched(tool: str, **fields: Any) -> list[dict[str, Any]]:
    call = T.tool(P + tool, **fields)
    return [T.assistant(call), T.user(T.result(json.dumps({"posted": True, "channel_id": fields.get("channel_id"), "thread_ts": fields.get("thread_ts")}), of=call))]


def said(*prompts: str, agent_id: str | None = None, at: datetime = PROMPTED_AT) -> dict[str, Any]:
    turns = [turn for prompt in prompts for turn in (T.user(prompt, timestamp=at.isoformat()), T.assistant("checking"))]
    return {"agent_id": agent_id, "transcript": turns if agent_id is None else [T.user("status?")], "root_transcript": turns if agent_id else None}


def covering(place: dict[str, str] | None = None, *, rule: str = "asked-user", revoked: datetime | None = None, tree: str = "fixture") -> Ticket:
    return Ticket(
        id="0123456789ab",
        kind=KIND,
        tree=tree,
        place=place or {"channel": FOLLOWERS, "thread": DANA_THREAD},
        reach=Reach.PLACE,
        rule=rule,
        author="test",
        created=datetime(2026, 10, 13, tzinfo=UTC),
        revoked=revoked,
    )


def posted_once(place: dict[str, str] | None = None, evidence: tuple[Evidence, ...] = ()) -> Ticket:
    return covering(place, rule=UNCHECKED).model_copy(update={"reach": Reach.ONCE, "fingerprint": "0" * 16, "evidence": list(evidence)})


def retrying(write: dict[str, Any], *, verb: str = "post", place: dict[str, str] | None = None) -> Ticket:
    written = Write(verb, write, daemon=True)
    at = place or {"channel": written.channel, "thread": written.thread}
    return covering(at).model_copy(update={"reach": Reach.ONCE, "fingerprint": Proposal(at, written.payload).fingerprint})


@dataclass(frozen=True, slots=True)
class Case:
    write: dict[str, Any]
    tool: str = "slack_reply"
    command: str | None = None
    transcript: list[dict[str, Any]] | Path | None = None
    root_transcript: list[dict[str, Any]] | None = None
    agent_id: str | None = None
    commands: dict[str, str | tuple[int, str]] | None = None
    tickets: list[Ticket] | None = None
    decide: dict[str, object] | None = None
    llm: dict[str, Any] | None = None
    env: dict[str, str] | None = None

    def __rshift__(self, expected: Allow | Ask | Block | Rewrite) -> tuple[Input, Allow | Ask | Block | Rewrite]:
        tool = None if self.command else (self.tool if "__" in self.tool else P + self.tool)
        return (
            Input(
                tool=tool,
                tool_input=None if self.command else self.write,
                command=self.command,
                transcript=self.transcript,
                root_transcript=self.root_transcript,
                agent_id=self.agent_id,
                commands=NOT_DM if self.commands is None else self.commands,
                tickets=self.tickets,
                decide=JEV_NONE if self.decide is None else self.decide,
                llm=LLM_REFUSES if self.llm is None else self.llm,
                env=ENABLED if self.env is None else self.env,
            ),
            expected,
        )


def case(write: dict[str, Any] | None = None, **fields: Any) -> Case:
    return Case(write or DANA | {"text": DANA_TEXT}, **fields)


NEW_ASK = DANA | {"text": "Could you send the raw log from last night?"}
TOP_LEVEL = {"channel_id": FOLLOWERS, "text": "a top-level post", "no_watch": True}
PRIYA_THREAD = {"channel_id": PRIYA_USER, "thread_ts": "1791900000.000400", "text": "and one more thing"}
LINKED_WORDS = said_evidence(f"respond to this asap {DANA_URL}", PROMPTED_AT, "prompt")
TAPPED = Evidence(id="ask:dana#0", source="ask", quote="Yes", detail=DANA_ASKED)
ORDERED = Evidence(id="words:keepanswering", source="prompt", quote="keep answering dana in her thread")
TOUCHED = Evidence(id="touch:0", source="touch", quote="an earlier call in the thread")
DANA_CARD_ID = card_id(Write("post", DANA | {"text": DANA_TEXT}, daemon=True), {"channel": FOLLOWERS, "thread": DANA_THREAD})
BOARD_SESSION_ID = "7c1f00e2-board"
BOARD_ROW = {
    "id": "b0a4d0b0a4d0b0a4",
    "title": DANA_CARD,
    "body": f'picked "Yes"\nBoard question: {DANA_CARD}',
    "updated_at": "2026-11-03T05:44:00Z",
    "tags": ["from:owner", "board-answer", f"board-session:{BOARD_SESSION_ID}", f"board-card:{DANA_CARD_ID}", f"board-block:{DANA_CARD_ID}-answer"],
    "superseded_by": None,
}
BOARD_GRANT = BOARD_ROW | {"tags": [*BOARD_ROW["tags"], f"slack-grant:{FOLLOWERS}/{DANA_THREAD}"]}
GRANT_PERMITS = jev(ruling_evidence(EVERY_THREAD_GRANT).id)
PAGE_GRANT = LABELED_GRANT | {
    "id": "ea25ea25ea25ea25",
    "title": f"May the drive reply in page threads ({FOLLOWERS}) and {PETS} during an incident?",
    "body": "While an incident is open the drive replies in page threads about it. Top-level posts still need his word.",
    "tags": ["scope:durable", f"slack-grant:{FOLLOWERS}", f"slack-grant:{PETS}"],
}
PAGE_GRANT_PERMITS = jev(ruling_evidence(PAGE_GRANT).id)
LABELED_GRANT_PERMITS = jev(ruling_evidence(LABELED_GRANT).id)
PAGE_REPLY = f"{CLI} reply --channel {FOLLOWERS} --thread {DANA_THREAD} --text-file {FIXTURES / 'page_reply.txt'} --agent claude 2>&1 | head -4 | cut -c1-260"
PAGE_REPLY_READ = thread_read(DANA_OPENED)
OWNER_ORDER = LABELED_GRANT | {
    "id": "27d127d127d127d1",
    "title": "When do we tell Dana about fixes to her bugs?",
    "body": f"Standing grant: whenever a release carrying a fix for her bug goes live, post to her in {FOLLOWERS}, in the thread where she reported it.",
    "tags": ["owner:m12", "scope:durable", "slack"],
}
OWNER_ORDER_PERMITS = jev(ruling_evidence(OWNER_ORDER).id)
THREAD_GRANT_PERMITS = jev(ruling_evidence(THREAD_GRANT).id)
THREAD_REVOKED = THREAD_GRANT | {"id": "e2e2e2e2e2e2e2e2", "updated_at": "2026-11-03T06:00:00Z", "tags": ["owner-ruling", f"slack-revoke:{FOLLOWERS}/{DANA_THREAD}"]}
RELAYED = 'Orders: owner:m12. Owner, verbatim: "let dana know the fix landed, in her thread"'
CLAIMED = 'Orders: owner:m12. Owner, verbatim: "post the fix in every thread you like"'
UNLEDGERED = 'Orders: owner:m99. Owner, verbatim: "post the fix in dana\'s thread"'
LEDGER_CITED = {"reason": "the owner's ledger row asks for this reply", "allow": True, "relied_on": ["owner:m12"]}
LEDGER_ENV = ENABLED | {"CLAUDE_CONFIG_DIR": str(FIXTURES / "config"), "CLAUDE_LONG_RUNNING_DRIVE": "brook"}
OWNER_DM = "D0OWNDM0001"
ORDER_TS = f"{time.time() - 60:.6f}"
STALE_TS = f"{time.time() - 3600:.6f}"
ORDER_TEXT = "did the fix for dana's bug land? tell dana"
ORDER_ID = f"operator:{OWNER_DM}/{ORDER_TS}"
ORDER = Evidence(id=ORDER_ID, source=DM_ORDER, quote=ORDER_TEXT)
ASKED_TS = "1791900000.000150"
ASKED_TEXT = "did the fix for her bug ship?"
HIS_QUESTION = {"ts": ASKED_TS, "user": "U0OWNER0001", "user_name": "Wren", "text": ASKED_TEXT, "from_user": True}
ASKED_ID = f"operator:{FOLLOWERS}/{ASKED_TS}"
SHIPPED = DANA | {"text": "It shipped this morning, and her chats open again."}


def order_url(ts: str = ORDER_TS) -> str:
    return f"https://acme.slack.com/archives/{OWNER_DM}/p{ts.replace('.', '')}"


def owner_dm(text: str = ORDER_TEXT, *, ts: str = ORDER_TS, own: bool = True, from_user: bool = True) -> dict[str, str | tuple[int, str]]:
    peer = {"peer": "U0OWNER0001", "peer_name": "Wren", "own": own}
    message = {"ts": ts, "user": "U0OWNER0001", "user_name": "Wren", "text": text, "from_user": from_user}
    return NOT_DM | {f"{CC_SLACK} dm-peer --channel {OWNER_DM}": json.dumps(peer), f"{CC_SLACK} thread --channel {OWNER_DM}": json.dumps({"messages": [message]})}


def dm_arrived(ts: str = ORDER_TS, *, text: str = ORDER_TEXT, operator: bool = True, channel: str = OWNER_DM, thread: str | None = None) -> list[dict[str, Any]]:
    payload = {"type": "slack.message", "kind": "channel", "channel_id": channel, "thread_ts": thread or ts, "ts": ts, "user": "U0OWNER0001", "user_name": "Wren", "from_claude": False, "text": text, "permalink": order_url(ts), "operator": operator}
    tag = f'<channel source="plugin:cc-slack:cc-slack" subject_id="s1" type="slack.message">\n{json.dumps(payload, separators=(",", ":"))}\n</channel>'
    return [T.user(tag, isMeta=True, origin={"kind": "channel", "server": "plugin:cc-slack:cc-slack"}), T.assistant("on it")]


def briefed(ts: str = ORDER_TS) -> dict[str, Any]:
    call = T.tool("Agent", description="tell dana", prompt=f'Message (his words): "{ORDER_TEXT}" Permalink: {order_url(ts)}')
    return {"transcript": [T.user(f"tell dana the fix landed, {order_url(ts)}")], "root_transcript": [T.user("status?"), T.assistant(call), T.user(T.result("launched", of=call))], "agent_id": "a-lane"}


def board_tapped(*picked: str, title: str = DANA_CARD, yes: str = "Yes") -> dict[str, str]:
    choice = {"id": f"{DANA_CARD_ID}-answer", "type": "choice", "options": [{"id": "yes", "label": yes}, {"id": "no", "label": "No"}]}
    drawn = {"id": DANA_CARD_ID, "type": "card", "title": title, "children": [{"id": f"{DANA_CARD_ID}-context", "type": "markdown", "md": DANA_TEXT}, choice]}
    answers = [{"origin": "human", "type": "choice.selected", "seq": 9, "payload": {"blockId": f"{DANA_CARD_ID}-answer", "optionIds": list(picked)}}]
    return ruled(BOARD_ROW) | {"cc-present outcomes --block": json.dumps({"doc": {"blocks": [drawn]}, "answers": answers})}


def relayed(text: str) -> list[dict[str, Any]]:
    call = T.tool("Bash", command="cci tail")
    return [T.user("status?"), T.assistant(call), T.user(T.result(text, of=call))]


SUBMITTED_BOARD = {"since": "last-submit", "through": 4222, "interactions": [{"seq": 4214, "type": "triage.decided", "blockId": "merge-prs", "at": "2026-10-30T15:24:19-07:00", "payload": {"verdicts": {"45401": {"verdict": "approved", "head": "e34ab03"}}}}]}
ALLOW = Rewrite(ticket_agent="main")
DENY = Block(pattern="No permission covers this Slack write")


def fixture_stubs(line: dict[str, Any]) -> dict[str, str | tuple[int, str]]:
    commands: dict[str, str | tuple[int, str]] = dict(NOT_DM)
    if line.get("dm"):
        commands[f"{CC_SLACK} dm-peer"] = json.dumps(line["dm"])
    if "channels" in line:
        del commands[f"{CC_SLACK} channel-info --channel {FOLLOWERS}"]
        commands |= {f"{CC_SLACK} channel-info --channel {channel['id']}": json.dumps(channel) for channel in line["channels"]}
    if "thread" in line:
        commands[f"{CC_SLACK} thread"] = json.dumps({"messages": line["thread"]})
    if "board" in line:
        commands["cc-present outcomes"] = json.dumps(line["board"])
    return commands


def fixture_cites(line: dict[str, Any]) -> str | None:
    cites = line.get("cites")
    if isinstance(cites, str) and cites.startswith("said:"):
        item = line["said"][int(cites.partition(":")[2])]
        return said_evidence(instruction_words(item["text"])[0], datetime.fromisoformat(item["at"]), "prompt").id
    return cites


def fixture_expected(line: dict[str, Any], *, daemon: bool, command: bool) -> Allow | Ask | Block | Rewrite:
    agent = line.get("agent") or "main"
    match line["expect"]:
        case "deny":
            return DENY
        case "silent":
            return Ask()
        case "unreadable":
            return Block(pattern="literal values")
    if not daemon:
        return Allow(explicit=True)
    if "ticket" in line:
        return Rewrite(pattern="--ticket 0123456789ab") if command else Rewrite(ticket_id="0123456789ab")
    return Rewrite(pattern=f"--ticket-agent {agent}") if command else Rewrite(ticket_agent=agent)


def fixture_case(line: dict[str, Any]) -> list[tuple[Input, Allow | Ask | Block | Rewrite]]:
    prompts = [T.user(item["text"], timestamp=item["at"]) for item in line.get("said", [])]
    answers = [turn for item in line.get("answered", []) for turn in owner_answered(item["question"], item["options"], item["picked"], notes=item.get("notes", ""), id=item["id"], described=item.get("described"))]
    touches = [turn for item in line.get("touched", []) for turn in touched(item["tool"], **item["input"])]
    words = [*answers, *prompts]
    transcript, root = ([*words, *touches], None) if line.get("agent") is None else ([T.user("status?"), *touches], words)
    cites = fixture_cites(line)
    judged = {"decide": jev(cites), "llm": {"reason": "the fixture cites it", "allow": True, "relied_on": [cites]}} if cites else {}
    tickets = [covering(line["ticket"])] if "ticket" in line else None
    fields = {"transcript": transcript, "root_transcript": root, "agent_id": line.get("agent"), "commands": fixture_stubs(line), "tickets": tickets, **judged}
    found = []
    if "tool" in line:
        found.append(case(line["input"], tool=line["tool"], **fields) >> fixture_expected(line, daemon=not line["tool"].startswith("mcp__slack__"), command=False))
    if "command" in line:
        found.append(case(command=line["command"], **fields) >> fixture_expected(line, daemon=SLACK_POST_CALL.search(line["command"]) is None, command=True))
    return found


def fixture_cases() -> dict[Input, Allow | Ask | Block | Rewrite]:
    return dict(found for line in (FIXTURES / "slack_denials.ndjson").read_text().splitlines() if line.strip() for found in fixture_case(json.loads(line)))


on(
    Event.PreToolUse,
    only_if=[WritesSlack()],
    mandatory=True,
    tests=dict(
        [
            case(**said(f"respond to this asap {DANA_URL}")) >> ALLOW,
            case(**said(f"respond to this asap {DANA_URL}.", "status?", "and the release?")) >> ALLOW,
            case(**said(f"respond to this asap {DANA_URL}", agent_id="a-lane")) >> Rewrite(ticket_agent="a-lane"),
            case(tool=DRIVE_SAY, **said(f"respond to this asap {DANA_URL}")) >> ALLOW,
            case(tool=DRIVE_SAY, transcript=LINKED_LONG_AGO_BY_OTHERS) >> DENY,
            case(PRIYA_FIXED, tool=DRIVE_SAY, **said(PRIYA_REPORTED), commands=PRIYA_GROUP_DM) >> ALLOW,
            case(PRIYA_FIXED, tool=DRIVE_SAY, **said(PRIYA_REPORTED), commands=PRIYA_GROUP_DM | {f"{CC_SLACK} dm-peer --channel {HIS_DM_WITH_PRIYA}": SAM_DM[f"{CC_SLACK} dm-peer"]}) >> DENY,
            case(DIGEST, tool="slack_send", transcript=LANE_BRIEF, root_transcript=owner_answered(DIGEST_ASKED, DIGEST_OPTIONS, DIGEST_OPTIONS[0], id="digest")) >> ALLOW,
            case(DIGEST, tool="slack_send", transcript=LANE_BRIEF, root_transcript=owner_answered(DIGEST_ASKED, DIGEST_OPTIONS, DIGEST_OPTIONS[1], id="digest")) >> DENY,
            case(transcript=LINKED_LONG_AGO) >> ALLOW,
            case({"url": LINKED_URL, "text": DANA_TEXT, "no_watch": True}, transcript=LINKED_LONG_AGO) >> ALLOW,
            case(transcript=LINKED_LONG_AGO_BY_OTHERS) >> DENY,
            case(**said(f'<pasted_content id="a1">{DANA_URL}</pasted_content> what is this')) >> DENY,
            case(**said(f'<channel source="cc-slack" type="slack.message">{DANA_URL}</channel>')) >> DENY,
            case(**said(f"respond to this asap {DANA_URL.replace(FOLLOWERS, PETS)}")) >> DENY,
            case(DANA | {"text": "on it", "broadcast": True}, **said(f"respond to this asap {DANA_URL}")) >> ALLOW,
            case({"channel_id": FOLLOWERS, "text": "on it", "no_watch": True}, tool="slack_send", **said(f"respond to this asap {DANA_URL}")) >> DENY,
            case(transcript=owner_answered(DANA_ASKED, ["Yes", "No"], "Yes", id="dana")) >> ALLOW,
            case(transcript=owner_answered(DANA_ASKED, ["Yes", "No"], "1, and keep it short", id="dana")) >> ALLOW,
            case(transcript=owner_answered(DANA_ASKED, ["Yes", "No"], "No", id="dana")) >> DENY,
            case(transcript=owner_answered(DANA_ASKED.replace(DANA_THREAD, "1791866612.381629"), ["Yes", "No"], "Yes", id="other")) >> ALLOW,
            case(transcript=owner_answered(f'Post this in {PETS}? "another reply"', ["Yes", "No"], "Yes", id="other")) >> DENY,
            case(transcript=owner_answered(DANA_ASKED, ["Yes", "No"], "No", id="dana"), commands=NOT_DM | thread_read(DANA_OPENED, DANA_FOLLOWED_UP)) >> DENY,
            case(transcript=card_tapped(DANA_CARD, "Yes")) >> ALLOW,
            case(transcript=card_tapped(DANA_CARD, "No")) >> DENY,
            case(transcript=card_tapped(DANA_CARD, "No", options=("No", "Yes"))) >> DENY,
            case(transcript=card_tapped(f"Post this in {PETS}?", "Yes")) >> DENY,
            case(DANA | {"text": "x" * 3000}) >> Block(pattern=r"x\\n… \d+ more characters"),
            case(transcript=[T.user("status?")], root_transcript=card_tapped(DANA_CARD, "Yes"), agent_id="a-lane") >> Rewrite(ticket_agent="a-lane"),
            case(transcript=card_tapped(DANA_CARD, "Yes"), root_transcript=[T.user("status?")], agent_id="a-lane") >> DENY,
            case() >> Block(
                pattern=re.escape(f'"title": "{DANA_CARD}",\n  "body": "Where: the #app-followers thread Dana started at ')
                + r"(?:\w+ \d+ at )?\d{1,2}:\d{2}[ap]m: "
                + re.escape(f'{DANA_OPENED["text"]}\\n\\n{DANA_TEXT}",\n  "options": [\n    "Yes",\n    "No"\n  ],\n  "recommended": "Yes",\n  "waits": "{DANA_CARD_ID}"')
            ),
            case(commands=NOT_DM | {f"{CC_SLACK} channel-info --channel {FOLLOWERS}": (1, "ratelimited")}) >> Block(pattern=re.escape(f'"title": "Post this in the {FOLLOWERS} thread Dana started?"')),
            case(commands=NOT_DM | {f"{CC_SLACK} thread": (1, "thread_not_found")}) >> Block(pattern=re.escape('"title": "Post this in the #app-followers thread?"')),
            case({"channel_id": PRIYA_USER, "text": PRIYA_DM_TEXT}, tool="slack_send", commands=PRIYA_DM) >> Block(pattern=re.escape('"title": "Post this in a direct message to priya?",\n  "body": "Hey Priya')),
            case() >> Block(pattern=re.escape(f'then retry once they tap Yes.\n```json\n{{\n  "id": "{DANA_CARD_ID}",\n  "type": "card",\n  "title": "{DANA_CARD}",\n  "children": [\n    {{\n      "id": "{DANA_CARD_ID}-context"')),
            case(**said(f"answer this reply too {DANA_URL.replace(DANA_THREAD.replace('.', ''), DANA_FOLLOWED_UP['ts'].replace('.', ''))}"), commands=NOT_DM | thread_read(DANA_OPENED, DANA_FOLLOWED_UP)) >> ALLOW,
            case({"channel_id": FOLLOWERS}, tool="slack_channel_join", **said(f"join this and read it {DANA_URL}")) >> ALLOW,
            case({"channel_id": FOLLOWERS}, tool="slack_channel_archive", **said(f"this one is done {DANA_URL}")) >> DENY,
            case(transcript=owner_answered("Post this in #App-Followers?", ["Yes", "No"], "Yes", id="named")) >> ALLOW,
            case(transcript=owner_answered("Post this in #pets?", ["Yes", "No"], "Yes", id="named")) >> DENY,
            case(transcript=owner_answered(f'Post this reply? "{DANA_TEXT}"', ["Yes", "No"], "Yes", id="quoted")) >> ALLOW,
            case(DANA | {"text": "another reply"}, transcript=owner_answered(f'Post this reply? "{DANA_TEXT}"', ["Yes", "No"], "Yes", id="quoted")) >> DENY,
            case({"channel_id": SAM_USER, "text": "Hi Sam, could you grant the app Actions: Read?"}, tool="slack_send", transcript=owner_answered("Who should I ask about the app permission?", ["DM sam O on slack", "Leave it"], "DM sam O on slack", id="sam"), commands=SAM_DM) >> ALLOW,
            case({"channel_id": SAM_USER, "text": "Hi Sam, could you grant the app Actions: Read?"}, tool="slack_send", transcript=owner_answered("Who should I ask about the app permission?", ["DM sam O on slack", "Leave it"], "DM sam O on slack", id="sam"), commands=PRIYA_DM) >> DENY,
            case({"channel_id": SAM_USER, "text": "Hi Sam, could you grant the app Actions: Read?"}, tool="slack_send", transcript=owner_answered("Ask Sam about the app permission?", ["Yes", "No"], "Yes", id="sam"), commands=SAM_DM) >> ALLOW,
            case({"channel_id": SAM_USER, "text": "_(Alex's Claude)_ Hi Sam, could you grant the app Actions: Read?"}, tool="mcp__slack__slack_send_message", transcript=owner_answered("Ask Sam about the app permission?", ["Yes", "No"], "Yes", id="sam"), commands=SAM_DM) >> Allow(explicit=True),
            case({"channel_id": FOLLOWERS, "ts": BOT_ANSWERED["ts"], "text": "fix is out, edited"}, tool="slack_edit", **said(f"fix the wording here {DANA_URL.replace(DANA_THREAD.replace('.', ''), BOT_ANSWERED['ts'].replace('.', ''))}")) >> ALLOW,
            case(commands=NOT_DM | {"ccn answer search": json.dumps([{"id": "0207568abcdef", "title": "Reply in app-followers?", "body": f"Reply in {FOLLOWERS} whenever asked.", "updated_at": "2026-10-01T00:00:00+00:00", "tags": []}])}) >> DENY,
            case(DANA | {"text": "a different reply, later"}, transcript=owner_answered(DANA_ASKED, ["Yes", "No"], "Yes", id="dana")) >> ALLOW,
            case(transcript=touched("slack_reply", channel_id=FOLLOWERS, thread_ts=DANA_THREAD, text="fix is out"), commands=NOT_DM | thread_read(DANA_OPENED, DANA_FOLLOWED_UP)) >> DENY,
            case(transcript=touched("slack_thread", channel_id=FOLLOWERS, thread_ts=DANA_THREAD), commands=NOT_DM | thread_read(DANA_OPENED, DANA_FOLLOWED_UP)) >> DENY,
            case(transcript=[T.user("post the milestone"), *touched("slack_send", channel_id=FOLLOWERS, text="milestone")], root_transcript=[T.user("status?")], agent_id="a-lane", commands=NOT_DM | thread_read(DANA_OPENED, DANA_FOLLOWED_UP)) >> DENY,
            case(
                {"channel_id": PRIYA_USER, "text": PRIYA_DM_TEXT},
                tool="slack_send",
                transcript=owner_answered(PRIYA_ASKED, ["DM priya and ask her to try posting it", "I'll paste it myself"], "DM priya and ask her to try posting it", id="doc-link"),
                commands=PRIYA_DM,
            )
            >> ALLOW,
            case(
                {"channel_id": PRIYA_USER, "text": PRIYA_DM_TEXT},
                tool="slack_send",
                transcript=owner_answered(PRIYA_ASKED, ["DM priya and ask her to try posting it", "I'll paste it myself"], "I'll paste it myself", id="doc-link"),
                commands=PRIYA_DM,
            )
            >> DENY,
            case(**said("let dana know the fix landed, in her thread"), decide=jev(said_evidence("let dana know the fix landed, in her thread", PROMPTED_AT, "prompt").id)) >> ALLOW,
            case(**said("let dana know the fix landed, in her thread"), llm={"reason": "the user asked for this reply", "allow": True, "relied_on": [said_evidence("let dana know the fix landed, in her thread", PROMPTED_AT, "prompt").id]}) >> ALLOW,
            case(**said("let dana know the fix landed, in her thread"), llm={"reason": "the user named another thread", "allow": False, "refusal": "The user asked for a reply elsewhere."}) >> DENY,
            case(**said("let dana know the fix landed, in her thread"), decide={"error": TimeoutError("jev down")}) >> DENY,
            case(**said("status?")) >> DENY,
            case(tickets=[retrying(DANA | {"text": DANA_TEXT})]) >> Rewrite(ticket_id="0123456789ab"),
            case(NEW_ASK, tickets=[posted_once()]) >> DENY,
            case(NEW_ASK, **said(f"respond to this asap {DANA_URL}"), tickets=[posted_once(evidence=(LINKED_WORDS,))]) >> DENY,
            case(NEW_ASK, transcript=owner_answered(DANA_ASKED, ["Yes", "No"], "Yes", id="dana"), tickets=[posted_once(evidence=(TAPPED,))]) >> DENY,
            case(NEW_ASK, transcript=owner_answered(DANA_ASKED, ["Yes", "No"], "Yes", id="dana"), tickets=[posted_once({"channel": FOLLOWERS, "thread": "1791866612.381629"}, evidence=(TAPPED,))]) >> DENY,
            case(NEW_ASK, transcript=owner_answered(DANA_ASKED, ["Yes", "No"], "Yes", id="dana"), tickets=[posted_once(evidence=(TAPPED,))], llm={"reason": "the earlier yes", "allow": True, "relied_on": [TAPPED.id]}) >> ALLOW,
            case(NEW_ASK, **said("status?"), tickets=[posted_once(evidence=(ORDERED,))], llm={"reason": "the same order", "allow": True, "relied_on": [ORDERED.id]}) >> ALLOW,
            case(NEW_ASK, **said("status?"), tickets=[posted_once(evidence=(ORDERED,))], decide=jev(ORDERED.id)) >> ALLOW,
            case(NEW_ASK, **said("status?"), tickets=[posted_once(evidence=(TOUCHED,))], llm={"reason": "an earlier call", "allow": True, "relied_on": [TOUCHED.id]}) >> DENY,
            case(NEW_ASK, **said("status?"), tickets=[posted_once(evidence=(ORDERED,)).model_copy(update={"revoked": datetime(2026, 10, 14, tzinfo=UTC)})], llm={"reason": "the same order", "allow": True, "relied_on": [ORDERED.id]}) >> DENY,
            case(NEW_ASK, **said("keep answering dana in her thread"), decide=jev(said_evidence("keep answering dana in her thread", PROMPTED_AT, "prompt").id), tickets=[posted_once()]) >> ALLOW,
            case(NEW_ASK, **said("that reply was wrong, post a correction"), decide=jev(said_evidence("that reply was wrong, post a correction", PROMPTED_AT, "prompt").id), tickets=[posted_once()]) >> ALLOW,
            case(NEW_ASK, **said("status?"), commands=ruled(THREAD_GRANT) | thread_read(DANA_OPENED), tickets=[posted_once()]) >> DENY,
            case(NEW_ASK, **said("status?"), commands=ruled(EVERY_THREAD_GRANT) | thread_read(DANA_OPENED), tickets=[posted_once()]) >> DENY,
            case(NEW_ASK, **said("status?"), commands=ruled(EVERY_THREAD_GRANT) | thread_read(DANA_OPENED)) >> DENY,
            case(NEW_ASK, tool="mcp__slack__slack_send_message", **said("status?"), commands=ruled(EVERY_THREAD_GRANT) | thread_read(DANA_OPENED)) >> DENY,
            case(command=f"{CLI} reply --channel {FOLLOWERS} --thread {DANA_THREAD} --text 'Could you send the raw log?'", **said("status?"), commands=ruled(EVERY_THREAD_GRANT) | thread_read(DANA_OPENED)) >> DENY,
            case(DANA | {"text": SOLVED_TEXT}, **said("status?"), commands=ruled(SOLVED_GRANT) | thread_read(DANA_OPENED)) >> ALLOW,
            case(DANA | {"text": "This is fixed now, and your chats open again."}, **said("status?"), commands=ruled(SOLVED_GRANT) | thread_read(DANA_OPENED, BOT_ANSWERED)) >> DENY,
            case(DANA | {"text": SOLVED_TEXT}, **said("status?"), commands=ruled(SOLVED_GRANT) | thread_read(DANA_OPENED, stale=True)) >> DENY,
            case(DANA | {"text": f"{SOLVED_TEXT} Does it work for you now?"}, **said("status?"), commands=ruled(SOLVED_GRANT) | thread_read(DANA_OPENED)) >> DENY,
            case(DANA | {"text": f"{SOLVED_TEXT} Please retry and let me know."}, **said("status?"), commands=ruled(SOLVED_GRANT) | thread_read(DANA_OPENED)) >> DENY,
            case(DANA | {"text": "Fixed: the walker lane landed behind the fence."}, **said("status?"), commands=ruled(SOLVED_GRANT) | thread_read(DANA_OPENED)) >> DENY,
            case(DANA | {"text": SOLVED_TEXT}, **said("status?"), commands=ruled(SOLVED_GRANT | {"tags": ["scope:durable", f"slack-grant:{PETS}{SOLVED}"]}) | thread_read(DANA_OPENED)) >> DENY,
            case(DANA | {"text": SOLVED_TEXT}, **said("status?"), commands=ruled(SOLVED_GRANT, THREAD_REVOKED) | thread_read(DANA_OPENED)) >> DENY,
            case(TOP_LEVEL | {"text": SOLVED_TEXT}, tool="slack_send", **said("status?"), commands=ruled(SOLVED_GRANT)) >> DENY,
            case(DANA | {"text": "Fixed: the walker lane landed behind the fence."}, **said("status?"), commands=ruled(THREAD_GRANT, SOLVED_GRANT) | thread_read(DANA_OPENED, BOT_ANSWERED), decide=THREAD_GRANT_PERMITS) >> ALLOW,
            case(DANA | {"text": "fix is out"}, **said("status?"), commands=ruled(THREAD_GRANT, SOLVED_GRANT) | thread_read(DANA_OPENED, BOT_ANSWERED), decide=THREAD_GRANT_PERMITS) >> ALLOW,
            case(transcript=card_tapped(DANA_CARD, "Yes"), commands=ruled(SOLVED_GRANT)) >> ALLOW,
            case(command=f"{SLACK_POST} send --channel {FOLLOWERS} --thread {DANA_THREAD} --text 'Could you send the raw log?'", **said("status?"), commands=ruled(EVERY_THREAD_GRANT) | thread_read(DANA_OPENED)) >> DENY,
            case(NEW_ASK, **said("status?"), commands=ruled(THREAD_GRANT) | thread_read(DANA_OPENED), decide=THREAD_GRANT_PERMITS) >> ALLOW,
            case(DANA | {"text": "fix is out"}, **said("status?"), commands=ruled(EVERY_THREAD_GRANT) | thread_read(DANA_OPENED), decide={"error": TimeoutError("jev down")}, llm=LLM_SILENT) >> ALLOW,
            case(PRIYA_THREAD, **said("status?"), commands=PRIYA_DM | {"ccn answer search": json.dumps([LABELED_GRANT | {"tags": [f"slack-grant:{PRIYA_USER}"]}])}, decide=LABELED_GRANT_PERMITS) >> ALLOW,
            case(PRIYA_THREAD, **said("status?"), commands=PRIYA_DM | {"ccn answer search": json.dumps([LABELED_GRANT | {"tags": [f"slack-grant:{PRIYA_USER}"]}])}) >> DENY,
            case(NEW_ASK, transcript=owner_answered(DANA_ASKED, ["Yes", "No"], "Yes", id="dana"), tickets=[posted_once(evidence=(TAPPED,))], decide={"error": TimeoutError("jev down")}, llm=LLM_SILENT) >> ALLOW,
            case(NEW_ASK, **said("status?"), commands=NOT_DM | {f"{CC_SLACK} dm-peer": (1, "ratelimited")}, tickets=[posted_once()]) >> ALLOW,
            case(tickets=[covering({"channel": FOLLOWERS, "thread": "1791866612.381629"})]) >> DENY,
            case(tickets=[covering({"channel": FOLLOWERS, "thread": ""})]) >> DENY,
            case(tickets=[covering(revoked=datetime(2026, 10, 14, tzinfo=UTC))]) >> DENY,
            case(tickets=[covering(tree="another-tree")]) >> DENY,
            case(TOP_LEVEL, tool="slack_send", tickets=[retrying(TOP_LEVEL)]) >> Rewrite(ticket_id="0123456789ab"),
            case(TOP_LEVEL | {"text": "another top-level post"}, tool="slack_send", tickets=[retrying(TOP_LEVEL)]) >> DENY,
            case(PRIYA_THREAD, commands=PRIYA_DM, tickets=[retrying(PRIYA_THREAD, place={"channel": PRIYA_USER, "thread": ""})]) >> Rewrite(ticket_id="0123456789ab"),
            case(PRIYA_THREAD | {"text": "and a new ask"}, commands=PRIYA_DM, tickets=[retrying(PRIYA_THREAD, place={"channel": PRIYA_USER, "thread": ""})]) >> DENY,
            case(PRIYA_THREAD | {"thread_ts": "1791900000.000500"}, commands=PRIYA_DM, tickets=[retrying(PRIYA_THREAD, place={"channel": PRIYA_USER, "thread": ""})]) >> DENY,
            case({"channel_id": PRIYA_USER, "text": PRIYA_THREAD["text"]}, tool="slack_send", commands=PRIYA_DM, tickets=[retrying(PRIYA_THREAD, place={"channel": PRIYA_USER, "thread": ""})]) >> DENY,
            case(command=f"{CLI} reply --user {PRIYA_USER} --thread {PRIYA_THREAD['thread_ts']} --text '{PRIYA_THREAD['text']}'", commands=PRIYA_DM, tickets=[retrying(PRIYA_THREAD, place={"channel": PRIYA_USER, "thread": ""})]) >> Rewrite(pattern="--ticket 0123456789ab"),
            case(command=f"{CLI} reply --user {PRIYA_USER} --thread 1791900000.000500 --text '{PRIYA_THREAD['text']}'", commands=PRIYA_DM, tickets=[retrying(PRIYA_THREAD, place={"channel": PRIYA_USER, "thread": ""})]) >> DENY,
            case(command=f"{CLI} reply --channel {FOLLOWERS} --thread {DANA_THREAD} --text 'any reply'", tickets=[retrying({"channel_id": FOLLOWERS, "thread_ts": DANA_THREAD, "text": "any reply"})]) >> Rewrite(pattern="--ticket 0123456789ab --ticket-tree fixture --ticket-agent main"),
            case(command=f"{CLI} reply --channel {FOLLOWERS} --thread {DANA_THREAD} --text 'any reply'") >> DENY,
            case(command=f"{CLI} reply --agent claude --channel {FOLLOWERS} --thread {DANA_THREAD} --text 'any reply'", tickets=[retrying({"channel_id": FOLLOWERS, "thread_ts": DANA_THREAD, "text": "any reply"})]) >> Rewrite(pattern="--ticket 0123456789ab"),
            case(command=f"{CLI} send --channel {FOLLOWERS} --text 'a post' --ticket 0123456789ab") >> Block(pattern="literal values"),
            case(command=f'{CLI} send --channel {FOLLOWERS} --text "$(cat reply.txt)"') >> Block(pattern="literal values"),
            case(command=f"{CLI} send --channel {FOLLOWERS} --text a && {CLI} reply --channel {FOLLOWERS} --thread 1 --text b") >> Block(pattern="one write per command"),
            case(command=f"{CLI} apply --plan {FIXTURES / 'plan_dm.json'}") >> Allow(explicit=True),
            case(command=f"{CLI} apply --plan {FIXTURES / 'plan_dm.json'} --dry-run 2>&1 | tail -3") >> Allow(explicit=True),
            case(command=f"{CLI} apply --plan {FIXTURES / 'plan_two_channels.json'}") >> Block(pattern=r"names 2 channels \(C0APPFOLLOW, D0OWNDM0001\)"),
            case(command=f"{CLI} apply --plan {FIXTURES / 'plan_post.json'}") >> Block(pattern="entry 1791900000.000300 has verdict 'post'"),
            case(command=f"{CLI} apply --plan {FIXTURES / 'plan_channel.json'}") >> Block(pattern="its channel C0APPFOLLOW is not a DM"),
            case(command=f"{CLI} apply --plan {FIXTURES / 'missing_plan.json'}") >> Block(pattern=r"missing_plan.json cannot be read \(No such file or directory\)"),
            case(command=f"{CLI} apply --plan {FIXTURES / 'page_reply.txt'}") >> Block(pattern=r"page_reply.txt does not parse \(Expecting value"),
            case(command=f"{CLI} apply --plan {FIXTURES / 'plan_dm.json'} && {CLI} send --channel {FOLLOWERS} --text a") >> Block(pattern="one write per command"),
            case(command=f"{CLI} apply --plan \"$(ls {FIXTURES}/plan_*.json | head -1)\"") >> Block(pattern="literal values"),
            case(command=f"cat > {FIXTURES / 'plan_dm.json'} <<'EOF'\n[]\nEOF\n{CLI} apply --plan {FIXTURES / 'plan_dm.json'}") >> Block(pattern="writes the `--plan` file it sends"),
            case(command=f"cat > {FIXTURES / 'page_reply.txt'} <<'EOF'\nthe new draft\nEOF\n{PAGE_REPLY}", **said("status?"), commands=ruled(PAGE_GRANT) | PAGE_REPLY_READ, decide=PAGE_GRANT_PERMITS) >> Block(pattern="in its own Bash call first"),
            case(command=f"printf 'the new draft' | tee {FIXTURES / 'page_reply.txt'} && {PAGE_REPLY}", **said("status?"), commands=ruled(PAGE_GRANT) | PAGE_REPLY_READ, decide=PAGE_GRANT_PERMITS) >> Block(pattern="in its own Bash call first"),
            case(command=f"{SLACK_POST} send --channel {FOLLOWERS} --thread {DANA_THREAD} --text '{DANA_TEXT}'", tickets=[retrying(DANA | {"text": DANA_TEXT})]) >> Allow(explicit=True),
            case(command=f"{SLACK_POST} send --channel {FOLLOWERS} --thread {DANA_THREAD} --text '{DANA_TEXT}'") >> DENY,
            case(DANA | {"text": f"_(Alex's Claude)_ {DANA_TEXT}"}, tool="mcp__slack__slack_send_message", tickets=[retrying(DANA | {"text": f"_(Alex's Claude)_ {DANA_TEXT}"})]) >> Allow(explicit=True),
            case(DANA | {"text": DANA_TEXT}, tool="mcp__slack__slack_send_message") >> DENY,
            case({"url": DANA_URL}, tool="slack_delete", tickets=[retrying({"url": DANA_URL}, verb="delete", place={"channel": FOLLOWERS, "thread": ""})]) >> Rewrite(ticket_id="0123456789ab"),
            case({"url": DANA_URL}, tool="slack_delete") >> Block(pattern="Delete the bot's Slack message"),
            case({"channel_id": FOLLOWERS, "ts": BOT_ANSWERED["ts"]}, tool="slack_delete", tickets=[retrying({"channel_id": FOLLOWERS, "ts": BOT_ANSWERED["ts"]}, verb="delete", place={"channel": FOLLOWERS, "thread": DANA_THREAD})], commands=NOT_DM | thread_read(DANA_OPENED, BOT_ANSWERED | {"thread_ts": DANA_THREAD})) >> Rewrite(ticket_id="0123456789ab"),
            case({"channel_id": FOLLOWERS, "ts": BOT_ANSWERED["ts"]}, tool="slack_delete", tickets=[posted_once({"channel": FOLLOWERS, "thread": ""})], commands=NOT_DM | thread_read(DANA_OPENED, BOT_ANSWERED | {"thread_ts": DANA_THREAD})) >> Block(pattern="Delete the bot's Slack message"),
            case({"channel_id": FOLLOWERS, "ts": BOT_ANSWERED["ts"]}, tool="slack_delete", tickets=[retrying({"channel_id": FOLLOWERS, "ts": BOT_ANSWERED["ts"]}, verb="delete", place={"channel": FOLLOWERS, "thread": ""})], commands=NOT_DM | {f"{CC_SLACK} thread": ""}) >> Rewrite(ticket_id="0123456789ab"),
            case(PRIYA_EDIT, tool="slack_edit", tickets=[retrying(PRIYA_EDIT, verb="edit", place={"channel": PRIYA_USER, "thread": ""})], commands=PRIYA_DM | {f"{CC_SLACK} thread": (1, "thread_not_found")}) >> Rewrite(ticket_id="0123456789ab"),
            case(PRIYA_EDIT, tool="slack_edit", commands=PRIYA_DM | {f"{CC_SLACK} thread": (1, "thread_not_found")}) >> Block(pattern="Edit the bot's message to this"),
            case({"channel_id": FOLLOWERS, "ts": BOT_ANSWERED["ts"], "text": "fix is out, edited"}, tool="slack_edit", tickets=[posted_once()], commands=NOT_DM | thread_read(DANA_OPENED, BOT_ANSWERED | {"thread_ts": DANA_THREAD})) >> ALLOW,
            case({"channel_id": FOLLOWERS, "ts": BOT_ANSWERED["ts"]}, tool="slack_delete", tickets=[posted_once()], commands=NOT_DM | thread_read(DANA_OPENED, BOT_ANSWERED | {"thread_ts": DANA_THREAD})) >> ALLOW,
            case({"channel_id": FOLLOWERS, "ts": DANA_FOLLOWED_UP["ts"], "text": "edited"}, tool="slack_edit", tickets=[posted_once()], commands=NOT_DM | thread_read(DANA_OPENED, DANA_FOLLOWED_UP | {"thread_ts": DANA_THREAD})) >> Block(pattern="Edit the bot's message to this"),
            case({"channel_id": FOLLOWERS, "ts": BOT_ANSWERED["ts"], "text": "fix is out, edited"}, tool="slack_edit", tickets=[posted_once({"channel": FOLLOWERS, "thread": "1791000000.000001"})], commands=NOT_DM | thread_read(DANA_OPENED, BOT_ANSWERED | {"thread_ts": DANA_THREAD})) >> Block(pattern="Edit the bot's message to this"),
            case(tickets=[posted_once()], commands=NOT_DM | thread_read(DANA_OPENED, BOT_ANSWERED | {"thread_ts": DANA_THREAD})) >> DENY,
            case(OWN_DM_EDIT, tool="slack_edit", commands=OWN_DM | thread_read(BOT_ANSWERED)) >> ALLOW,
            case({"channel_id": OWN_DM_EDIT["channel_id"], "ts": BOT_ANSWERED["ts"]}, tool="slack_delete", commands=OWN_DM | thread_read(BOT_ANSWERED)) >> ALLOW,
            case(OWN_DM_EDIT, tool="slack_edit", commands=OWN_DM | thread_read(BOT_ANSWERED | {"from_claude": False})) >> Block(pattern="Edit the bot's message to this"),
            case(OWN_DM_EDIT, tool="slack_edit", commands=PRIYA_DM | thread_read(BOT_ANSWERED)) >> Block(pattern="Edit the bot's message to this"),
            case(PRIYA_EDIT, tool="slack_edit", commands=NOT_DM | {f"{CC_SLACK} dm-peer": (1, "ratelimited")}) >> ALLOW,
            case(command=f"{CLI} reply --channel {FOLLOWERS} --thread {DANA_THREAD} --text 'any reply'", commands=NOT_DM | {f"{CC_SLACK} dm-peer": (1, "ratelimited")}) >> Rewrite(pattern="--ticket-agent main"),
            case(DANA | {"text": f"_(Alex's Claude)_ {DANA_TEXT}"}, tool="mcp__slack__slack_send_message", commands=NOT_DM | {f"{CC_SLACK} dm-peer": (1, "ratelimited")}) >> Allow(explicit=True, system_message="goes ahead unchecked"),
            case(**said("let dana know the fix landed, in her thread"), decide={"error": TimeoutError("jev down")}, llm=LLM_SILENT) >> ALLOW,
            case(command=f"{CLI} send --user {PRIYA_USER} --text '{PRIYA_DM_TEXT}'", transcript=owner_answered(PRIYA_ASKED, ["DM priya and ask her to try posting it", "I'll paste it myself"], "DM priya and ask her to try posting it", id="doc-link"), commands=PRIYA_BY_USER) >> Rewrite(pattern="--ticket-agent main"),
            case(command=f"{CLI} send --user {PRIYA_USER} --text '{PRIYA_DM_TEXT}'", commands=PRIYA_BY_USER) >> DENY,
            case({"channel_id": FOLLOWERS}, tool="slack_channel_archive", transcript=owner_answered(f"Archive {FOLLOWERS}, the stale incident channel?", ["Yes", "No"], "Yes", id="archive")) >> ALLOW,
            case({"channel_id": FOLLOWERS}, tool="slack_channel_archive") >> Block(pattern="Archive the Slack channel"),
            case(DANA | {"ts": DANA_THREAD, "name": "eyes"}, tool="slack_react") >> Allow(),
            case(DANA | {"ts": DANA_THREAD}, tool="slack_pickup") >> Allow(),
            case(DANA | {"recipient": "U0DANA00001", "plan": "Looking at this"}, tool="slack_stream", tickets=[retrying(DANA | {"text": "Looking at this"})]) >> DENY,
            case(DANA | {"ts": "1791900000.000500", "tasks": [{"id": "1", "status": "complete", "title": "Reading the thread"}]}, tool="slack_stream") >> Allow(),
            case(DANA | {"recipient": "U0DANA00001", "plan": "Looking at this", "tasks": [{"id": "1", "status": "in_progress", "title": "Reading the thread"}]}, tool="slack_stream") >> DENY,
            case({"text": "status"}, tool="slack_dm_status") >> Allow(),
            case({"text": "status", "thread_ts": DANA_THREAD}, tool=DRIVE_SAY) >> Allow(),
            case(DANA | {"text": DANA_TEXT, "dry_run": True}, tool=DRIVE_SAY) >> Allow(),
            case({"query": "release"}, tool="mcp__slack__slack_search_messages") >> Allow(),
            case({"ticket_id": "0123456789ab"}, tool="slack_revoke") >> Allow(),
            case(commands=NOT_DM | {f"{CC_SLACK} dm-peer": "cc-slack can't reach the daemon"}) >> ALLOW,
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(KEPT_POSTING)) >> ALLOW,
            case(command=f"{CLI} send --channel {FOLLOWERS} --text 'another landing'", **said("status?"), commands=ruled(KEPT_POSTING)) >> Rewrite(pattern="--ticket-agent main"),
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(KEPT_POSTING | {"body": f"No\n{KEEP_POSTING_OPTIONS}"})) >> DENY,
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(KEPT_POSTING | {"body": f"Don't keep posting there\n{KEEP_POSTING_OPTIONS}"}, LABELED_GRANT)) >> DENY,
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(KEPT_POSTING | {"superseded_by": "0a0a0a0a"})) >> DENY,
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(KEPT_POSTING | {"title": KEPT_POSTING["title"].replace(FOLLOWERS, PETS)})) >> DENY,
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(KEPT_POSTING | {"tags": ["scope:ephemeral"]})) >> DENY,
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(LABELED_GRANT), decide=LABELED_GRANT_PERMITS) >> ALLOW,
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(LABELED_GRANT)) >> DENY,
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(PAGE_GRANT)) >> DENY,
            case(DANA | {"text": "fix is out"}, **said("status?"), commands=ruled(LABELED_GRANT) | PAGE_REPLY_READ, decide=jev(f"scope-{LABELED_GRANT['id'][:7]}")) >> DENY,
            case(DANA | {"text": "fix is out"}, **said("status?"), commands=ruled(LABELED_GRANT) | PAGE_REPLY_READ, decide=LABELED_GRANT_PERMITS) >> ALLOW,
            case(command=PAGE_REPLY, **said("status?"), commands=ruled(PAGE_GRANT) | PAGE_REPLY_READ | {f"{CC_SLACK} channel-info --channel {FOLLOWERS}": (1, "timed out")}, decide=PAGE_GRANT_PERMITS) >> Rewrite(pattern="--ticket-agent main"),
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(LABELED_GRANT, REVOKED)) >> DENY,
            case(ROOM_POST, tool="slack_send", **said("status?"), commands=ruled(LABELED_GRANT) | PAGES_HISTORY | room_opened(ROOM)) >> ALLOW,
            case(ROOM_POST, tool="slack_send", **said("status?"), commands=ruled(LABELED_GRANT) | PAGES_HISTORY | room_opened("C0OTHER0001")) >> DENY,
            case(ROOM_POST, tool="slack_send", **said("status?"), commands=ruled(LABELED_GRANT) | PAGES_HISTORY | room_opened(ROOM, from_bot=False)) >> DENY,
            case(ROOM_POST, tool="slack_send", **said("status?"), commands=ruled(LABELED_GRANT, REVOKED) | PAGES_HISTORY | room_opened(ROOM)) >> DENY,
            case(ROOM_POST, tool="slack_send", **said("status?"), commands=ruled(LABELED_GRANT) | PAGES_HISTORY | room_opened(ROOM, answered=False)) >> DENY,
            case(ROOM_POST, tool="slack_send", **said("status?"), commands=ruled(LABELED_GRANT) | PAGES_HISTORY | room_opened(ROOM, by="Alex (claude)")) >> DENY,
            case(DANA | {"text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(LABELED_GRANT) | thread_read(DANA_OPENED)) >> DENY,
            case(DANA | {"text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(KEPT_POSTING) | thread_read(DANA_OPENED)) >> DENY,
            case(DANA | {"channel_id": ROOM, "text": "fix is out"}, **said("status?"), commands=ruled(LABELED_GRANT) | PAGE_REPLY_READ | {"ccn answer search slack-grant:": (1, "a thread write never sweeps the grants")}) >> DENY,
            case(command=PAGE_REPLY, **said("status?"), commands=ruled(PAGE_GRANT) | PAGE_REPLY_READ, decide=PAGE_GRANT_PERMITS) >> Rewrite(pattern="--ticket-agent main"),
            case(command=PAGE_REPLY, **said("status?"), commands=ruled(PAGE_GRANT) | PAGE_REPLY_READ) >> DENY,
            case(DANA | {"channel_id": ROOM, "text": "fix is out"}, **said("status?"), commands=ruled(PAGE_GRANT) | PAGE_REPLY_READ, decide=PAGE_GRANT_PERMITS) >> DENY,
            case(command=PAGE_REPLY, **said("status?"), commands=ruled(PAGE_GRANT | {"superseded_by": "0a0a0a0a"}) | PAGE_REPLY_READ, decide=PAGE_GRANT_PERMITS) >> DENY,
            case(command=PAGE_REPLY, **said("status?"), commands=ruled(THREAD_REVOKED | {"updated_at": "2026-10-01T00:00:00Z"}, PAGE_GRANT) | PAGE_REPLY_READ, decide=PAGE_GRANT_PERMITS) >> DENY,
            case(DANA | {"text": "fix is out"}, **said("status?"), commands=ruled(OWNER_ORDER) | PAGE_REPLY_READ, decide=OWNER_ORDER_PERMITS) >> ALLOW,
            case(DANA | {"text": "fix is out"}, **said("status?"), commands=ruled(OWNER_ORDER | {"superseded_by": "0a0a0a0a"}) | PAGE_REPLY_READ, decide=OWNER_ORDER_PERMITS) >> DENY,
            case(DANA | {"text": "fix is out"}, **said("status?"), commands=ruled(OWNER_ORDER | {"body": OWNER_ORDER["body"].replace(FOLLOWERS, PETS)}) | PAGE_REPLY_READ, decide=OWNER_ORDER_PERMITS) >> DENY,
            case(DANA | {"text": "fix is out"}, **said("status?"), commands=ruled(OWNER_ORDER | {"tags": ["scope:durable", "slack"]}) | PAGE_REPLY_READ, decide=OWNER_ORDER_PERMITS) >> ALLOW,
            case(DANA | {"text": "fix is out"}, **said("status?"), commands=NOT_DM | PAGE_REPLY_READ | {"ccn answer search": (1, "database is locked")}) >> ALLOW,
            case(DANA | {"text": "fix is out"}, **said("status?"), commands=NOT_DM | PAGE_REPLY_READ | {"ccn answer search": (1, "error: open git repository at /scratch: not a git repository")}) >> DENY,
            case(DANA | {"text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(LABELED_GRANT, THREAD_GRANT) | thread_read(DANA_OPENED), decide=THREAD_GRANT_PERMITS) >> ALLOW,
            case(DANA | {"text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(THREAD_GRANT) | thread_read(DANA_OPENED), decide=THREAD_GRANT_PERMITS) >> ALLOW,
            case(DANA | {"text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(EVERY_THREAD_GRANT) | thread_read(DANA_OPENED), decide=GRANT_PERMITS) >> ALLOW,
            case(DANA | {"thread_ts": "1791866612.381629", "text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(EVERY_THREAD_GRANT) | thread_read(DANA_OPENED), decide=GRANT_PERMITS) >> ALLOW,
            case(DANA | {"channel_id": ROOM, "text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(EVERY_THREAD_GRANT) | thread_read(DANA_OPENED)) >> DENY,
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(EVERY_THREAD_GRANT)) >> DENY,
            case(DANA | {"thread_ts": "1791866612.381629", "text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(THREAD_GRANT) | thread_read(DANA_OPENED)) >> DENY,
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(THREAD_GRANT)) >> DENY,
            case(
                LANDED,
                tool="slack_send",
                **said("status?"),
                commands=ruled(THREAD_GRANT | {"body": f"Yes, post and keep posting (Recommended)\n{KEEP_POSTING_OPTIONS}", "tags": [*ASKED, f"slack-grant:{FOLLOWERS}/{DANA_THREAD}"]}),
            ) >> DENY,
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(KEPT_POSTING | {"body": f"Don't continue posting\n{KEEP_POSTING_OPTIONS}"})) >> DENY,
            case(LANDED, tool="slack_send", commands=ruled(REVOKED | {"body": f"Stop posting in {FOLLOWERS}", "updated_at": "2026-10-01T00:00:00Z"})) >> DENY,
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(KEPT_IN_CHANNEL), decide=jev(ruling_evidence(KEPT_IN_CHANNEL).id)) >> ALLOW,
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(KEPT_IN_CHANNEL)) >> DENY,
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(KEPT_IN_CHANNEL, REVOKED), decide=jev(ruling_evidence(KEPT_IN_CHANNEL).id)) >> DENY,
            case(DANA | {"text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(KEPT_IN_CHANNEL) | thread_read(DANA_OPENED)) >> DENY,
            case(DANA | {"text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(KEPT_IN_CHANNEL) | thread_read(DANA_OPENED), decide=jev(ruling_evidence(KEPT_IN_CHANNEL).id)) >> ALLOW,
            case(DANA | {"text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(KEPT_POSTING | {"body": f"Don't keep posting there\n{KEEP_POSTING_OPTIONS}"}) | thread_read(DANA_OPENED), decide=jev(ruling_evidence(KEPT_POSTING | {"body": f"Don't keep posting there\n{KEEP_POSTING_OPTIONS}"}).id)) >> DENY,
            case(**said("post it i already told u to"), commands=NOT_DM | {"cc-present outcomes": json.dumps(SUBMITTED_BOARD)}) >> ALLOW,
            case(**said("status?"), commands=board_tapped("yes")) >> ALLOW,
            case(**said("status?"), commands=board_tapped("no")) >> DENY,
            case(DANA | {"text": "another reply"}, **said("status?"), commands=board_tapped("yes")) >> DENY,
            case(**said("status?"), commands=ruled(BOARD_ROW)) >> DENY,
            case(**said("status?"), commands=board_tapped("yes", title=f"Post this in {PETS}?")) >> DENY,
            case(**said("status?"), commands=board_tapped("yes", yes="No")) >> DENY,
            case(DANA | {"text": "a later reply"}, **said("status?"), commands=ruled(BOARD_GRANT, BOARD_GRANT | {"id": "b1b1b1b1", "updated_at": "2026-11-03T06:10:00Z", "tags": [*BOARD_ROW["tags"], f"slack-revoke:{FOLLOWERS}/{DANA_THREAD}"]}) | thread_read(DANA_OPENED)) >> DENY,
            case(tickets=[retrying(DANA | {"text": DANA_TEXT})], commands=ruled(THREAD_REVOKED)) >> DENY,
            case(tickets=[retrying(DANA | {"text": DANA_TEXT})], commands=ruled(THREAD_REVOKED | {"updated_at": "2026-10-01T00:00:00Z"})) >> Rewrite(ticket_id="0123456789ab"),
            case(DANA | {"text": "a later reply"}, **said("status?"), commands=ruled(BOARD_GRANT) | thread_read(DANA_OPENED), decide=jev(ruling_evidence(BOARD_GRANT).id)) >> ALLOW,
            case(DANA | {"text": "a later reply"}, **said("status?"), commands=ruled(BOARD_GRANT | {"title": f"Post this in {PETS}?"}) | thread_read(DANA_OPENED)) >> DENY,
            case(**said("status?"), commands=board_tapped("yes") | {"ccn answer search": json.dumps([BOARD_ROW, THREAD_REVOKED])}) >> DENY,
            case(**said("status?"), commands=board_tapped("yes") | {"cc-present outcomes --block": "not json"}) >> DENY,
            case(**said("status?"), commands=board_tapped("yes") | {"cc-present outcomes --block": (1, "the cc-present daemon is down")}) >> DENY,
            case(transcript=relayed(RELAYED), env=LEDGER_ENV, llm=LEDGER_CITED) >> ALLOW,
            case(transcript=relayed(RELAYED), llm=LEDGER_CITED) >> DENY,
            case(transcript=relayed(RELAYED), env=LEDGER_ENV | {"CLAUDE_LONG_RUNNING_DRIVE": "gone"}, llm=LEDGER_CITED) >> DENY,
            case(transcript=relayed(RELAYED), env=LEDGER_ENV | {"CLAUDE_LONG_RUNNING_DRIVE": "unregistered"}, llm=LEDGER_CITED) >> DENY,
            case(transcript=relayed(UNLEDGERED), env=LEDGER_ENV, llm={"reason": "the record cites a row", "allow": True, "relied_on": ["owner:m99"]}) >> DENY,
            case(transcript=relayed(CLAIMED), env=LEDGER_ENV, llm={"reason": "the record quotes a grant", "allow": True, "relied_on": [said_evidence("post the fix in every thread you like", PROMPTED_AT, "prompt").id]}) >> DENY,
            case(transcript=dm_arrived(), decide=jev(ORDER_ID)) >> ALLOW,
            case(transcript=dm_arrived(STALE_TS), decide=jev(f"operator:{OWNER_DM}/{STALE_TS}")) >> ALLOW,
            case(transcript=dm_arrived(operator=False), decide=jev(ORDER_ID)) >> DENY,
            case(**briefed(), commands=owner_dm(), decide=jev(ORDER_ID)) >> Rewrite(ticket_agent="a-lane"),
            case(**briefed(), decide=jev(ORDER_ID)) >> DENY,
            case(**briefed(), commands=owner_dm(ts="1700000000.000100"), decide=jev(ORDER_ID)) >> DENY,
            case(**briefed(), commands=owner_dm(own=False), decide=jev(ORDER_ID)) >> DENY,
            case(**briefed(), commands=owner_dm(from_user=False), decide=jev(ORDER_ID)) >> DENY,
            case(**briefed(STALE_TS), commands=owner_dm(ts=STALE_TS), decide=jev(f"operator:{OWNER_DM}/{STALE_TS}")) >> Rewrite(ticket_agent="a-lane"),
            case(transcript=dm_arrived(), tickets=[posted_once(evidence=(ORDER,))], decide=jev(ORDER_ID)) >> ALLOW,
            case(SHIPPED, transcript=[T.user("status?")], root_transcript=dm_arrived(), agent_id="a-lane", tickets=[posted_once(evidence=(ORDER,))], commands=ruled(SOLVED_GRANT) | thread_read(DANA_OPENED, BOT_ANSWERED), decide=jev(ORDER_ID)) >> Rewrite(ticket_agent="a-lane"),
            case(transcript=dm_arrived()) >> DENY,
            case(transcript=dm_arrived(text=f"tell her here {DANA_URL}"), decide=jev(ORDER_ID)) >> ALLOW,
            case(SHIPPED, **said("status?", agent_id="a-lane"), commands=ruled(SOLVED_GRANT) | thread_read(DANA_OPENED, HIS_QUESTION, BOT_ANSWERED), decide=jev(ASKED_ID)) >> Rewrite(ticket_agent="a-lane"),
            case(SHIPPED, **said("status?", agent_id="a-lane"), commands=ruled(SOLVED_GRANT) | thread_read(DANA_OPENED, HIS_QUESTION | {"from_user": False}, BOT_ANSWERED), decide=jev(ASKED_ID)) >> DENY,
            case(SHIPPED, **said("status?"), commands=ruled(SOLVED_GRANT) | thread_read(DANA_OPENED, BOT_ANSWERED), llm={"reason": "a second post", "allow": False, "refusal": "The user allows one solved post in this thread, and the bot already posted."}) >> Block(pattern="Why: The user allows one solved post in this thread"),
            case({"channel_id": PRIYA_USER, "text": PRIYA_DM_TEXT}, tool="slack_send", commands=PRIYA_DM) >> Block(pattern=r'"waits": "slack:U0PRIYA0001::[0-9a-f]{12}"'),
            case(LANDED, tool="slack_send", **said("status?")) >> Block(pattern=rf'"waits": "slack:{FOLLOWERS}::[0-9a-f]{{12}}"'),
            case(env=DISABLED) >> Allow(),
            case(env={}) >> Allow(),
            case(command=f"{SLACK_POST} send --channel {FOLLOWERS} --text 'a post'", env=DISABLED) >> Allow(),
            *fixture_cases().items(),
        ]
    ),
)(slack_writes_need_permission)
