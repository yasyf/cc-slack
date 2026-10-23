from __future__ import annotations

import hashlib
import json
import re
import shlex
import subprocess
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass, replace
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from loguru import logger
from spawnllm import LabelAnswer

from captain_hook import Allow, Ask, Block, Event, HookResult, Input, PostToolUseEvent, PreToolUseEvent, Rewrite, T, Warn, on
from captain_hook.conditions import UserSaid
from captain_hook.events import BaseHookEvent, ToolHookEvent
from captain_hook.tickets import Context, CustomTicketCondition, Decide, Deny, Evidence, Holds, Proposal, Reach, Rule, Ticket, Tickets, Undecided, UserAnswered, UserRuled, holds, store
from captain_hook.tickets.context import ccn_answers, instruction_words, ruling_evidence, said_evidence, written_at
from captain_hook.tickets.declare import UNCHECKED
from captain_hook.types import And, Or
from captain_hook.util import reqenv

from .common import BIN
from .writes import Write, capture, cli_calls, pending, permalink, permissions

CC_SLACK = str(BIN)
GATE = "slack_writes_need_permission"
KIND = "slack.write"
WINDOW = 240
JUDGED_TURNS = 12
SHOWN = 300
LATEST_SHOWN = 3
SHORTCUT_BRIEF = "[cc-slack shortcut brief]"
CARRY = "thread-carry"
CHECK_SECONDS = 40
DEADLINE_MARGIN = 5
CHECK_FAILURES = (OSError, LookupError, ValueError, RuntimeError, subprocess.SubprocessError)
UNFINISHED = "{gate}: the check did not finish ({cause}), so this write goes ahead unchecked"
GRANT_LABEL = "slack-grant:"
REVOKE_LABEL = "slack-revoke:"
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
LINKED_VERBS = {"post", "edit", "delete", "join"}
SLACK_URL = re.compile(r"https://[\w.-]+\.slack\.com/archives/[^\s<>|)\"]+")
PASTED = re.compile(r"<pasted_content\b.*?</pasted_content>", re.S)
TICKET_FLAGS = {"ticket_id": "ticket", "ticket_tree": "ticket-tree", "ticket_agent": "ticket-agent", "ticket_call": "ticket-call", "ticket_fingerprint": "ticket-fingerprint"}
UNREAD = (
    "A Slack write is checked on its literal values, one write per command. Run one `cc-slack` call per Bash command"
    " with literal `--text`, `--file`, and `--blocks` values and no `--ticket` flags."
)
REFUSED = (
    "No permission covers this Slack write. Ask the user on a card in your DM status thread: call `slack_dm_status` with"
    " this card, and retry the same call once they tap Yes.\n```json\n{ask}\n```"
)
RULES = """
    The agent is about to make one Slack write, sent to <destination>. Allow it when the user's own words or
    answers permit a write there, explicitly or by implication: telling the agent to reply, answer, update,
    tell, or explain something to someone there, or approving a post the agent proposed for that place, is
    permission; so is permission given earlier in the same thread. The words need not quote the text.
    - Scolding the agent for not replying to someone, or for not doing something, is an instruction to do it
      now, where that person is waiting: "why did you not respond to him?" permits responding to him, "you
      never shared the doc there, do that" permits sharing it there, and "why haven't you started?" permits
      reporting that it has started. "never" and "not" in such a complaint describe what was left undone;
      they hold nothing back. Asking for the work a post reports permits reporting it.
    - An instruction naming a thing to share or post covers every place it was owed: each thread where it was
      promised or asked for, and each thread or person the user handed over. "etc." widens it to those
      comparable places, never to arbitrary ones.
    - An instruction to post in a place permits every post there that carries it out: an introduction, a
      context message, or one post of several, whatever its wording, attachments, or count. A condition on the
      agent's own work, such as "once these are done", orders the work and holds nothing back; only a
      condition the user must meet, such as "after I review it", holds the write.
    - "don't block", "don't wait on me", "stop asking", and words like them are permission to proceed with the
      pending write, not a hold.
    - The user's latest words on the matter decide: a later go-ahead such as "actually go ahead and post"
      overrides an earlier skip, hold, or edit request.
    - A destination the user named by id, name, or link is that place; words about another destination permit
      nothing here unless they widen to this one, as "etc.", "and the others", or "everywhere it was owed" do.
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
        links = [permalink(url.rstrip(".,;:!?")) for url in SLACK_URL.findall(PASTED.sub(" ", text))]
        return any(channel == self.channel and (self.channel_wide or thread == self.thread or ts in replies or ts == self.ts) for channel, thread, ts in links if ts)


class Channels(dict[str, dict[str, Any]]):
    def names(self, channel: str) -> list[str]:
        found = self.get(channel)
        return [] if found is None else [f"#{name}" for name in (found["name"], *found.get("previous_names", []))]


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
    return Conversation(write.channel, write.thread or message_thread(write), str(peer.get("peer") or ""), name, names, write.ts)


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


def channels_of(ctx: Context) -> Channels:
    return ctx.fact(Channels, lambda ctx: Channels((channel["id"], channel) for channel in cc_slack("channels", "--json")["channels"]))


def live(ctx: Context) -> ToolHookEvent:
    if not isinstance(ctx.evt, ToolHookEvent):
        raise TypeError("a Slack context built without its conversation needs the tool event")
    return ctx.evt


@dataclass(frozen=True, slots=True)
class Linked:
    def matches(self, text: str, ctx: Context) -> bool:
        conv = conversation_of(ctx)
        replies = frozenset(message.ts for message in thread_of(ctx).messages if message.ts != conv.thread)
        return conv.linked_in(text, replies)


@dataclass(frozen=True, slots=True)
class Named:
    def matches(self, text: str, ctx: Context) -> bool:
        conv = conversation_of(ctx)
        return conv.named_in(text, channels_of(ctx).names(conv.channel))


@dataclass(frozen=True, slots=True)
class Judged(CustomTicketCondition):
    judge: Decide

    def evidence(self, ctx: Context) -> list[Evidence] | None:
        try:
            return self.judge.evidence(ctx)
        except Undecided as exc:
            raise TimeoutError(exc.cause) from exc


def standing(answer: dict[str, Any], channel: str, thread: str) -> bool | None:
    tags = set(answer.get("tags", ()))
    if thread and f"{REVOKE_LABEL}{channel}/{thread}" in tags:
        return False
    if thread and f"{GRANT_LABEL}{channel}/{thread}" in tags:
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


def standing_of(ctx: Context) -> Standing:
    def newest(ctx: Context) -> Standing:
        channel, thread = ctx.place["channel"], ctx.place["thread"]
        live_answers = sorted((answer for answer in ccn_answers(live(ctx), channel) if not answer.get("superseded_by")), key=written_at)
        decided = [Standing(answer, grants) for answer in live_answers if (grants := standing(answer, channel, thread)) is not None]
        return decided[-1] if decided else Standing()

    return ctx.fact(Standing, newest)


@dataclass(frozen=True, slots=True)
class StandingGrant(CustomTicketCondition):
    def evidence(self, ctx: Context) -> list[Evidence] | None:
        found = standing_of(ctx)
        return [ruling_evidence(found.answer)] if found.answer is not None and found.grants else None


@dataclass(frozen=True, slots=True)
class NotRevoked(CustomTicketCondition):
    def evidence(self, ctx: Context) -> list[Evidence] | None:
        found = standing_of(ctx)
        return None if found.answer is not None and not found.grants else []


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
        channel = channels_of(ctx).get(conv.channel)
        place = conv.channel if channel is None else channel_name(channel)
        if not conv.thread:
            return place
        thread = thread_of(ctx)
        if not thread.readable:
            return f"{place}, thread {conv.thread}"
        opener = next((message for message in thread.messages if message.ts == conv.thread), thread.messages[0])
        humans = [message for message in thread.messages if message.human and message is not opener][-LATEST_SHOWN:]
        latest = "".join(f"\n{message.user_name}: {message.text[:SHOWN]}" for message in humans)
        return f"{place}, thread {conv.thread}, opened by {opener.user_name}, whose words are never the user's: {opener.text[:SHOWN]}" + (f"\nlatest replies from others in the thread, never the user's words:{latest}" if latest else "")


SLACK = Tickets(
    KIND,
    ("channel", "thread"),
    Reach.PLACE,
    rules=(
        Rule("standing-grant", StandingGrant()),
        Rule("user-linked", UserSaid(Linked())),
        Rule("asked-user", Or(UserAnswered(Named(), Holds("text"), approved=True), And(TopLevel(), NotRevoked(), UserRuled("channel")))),
        Rule("decide", Judged(Decide(RULES, contexts=[Destination()], transcript=JUDGED_TURNS, root_transcript=JUDGED_TURNS))),
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


def ask(write: Write, conv: Conversation) -> str:
    if write.verb in TEXT_VERBS:
        title, body = f"{TEXT_VERBS[write.verb]} in {conv.where}?", shown(write)
    else:
        title = f"{OTHER_VERBS[write.verb]} ({conv.where})?" if conv.where else f"{OTHER_VERBS[write.verb]}?"
        body = ", ".join(str(value) for value in (*write.fields.values(), *write.users) if isinstance(value, str) and value)
    waits = "slack-" + hashlib.sha256(json.dumps([write.verb, write.fields, write.users], sort_keys=True, default=str).encode()).hexdigest()[:12]
    card = {"status": "needs_you", "title": title, "body": card_body(body), "options": ["Yes", "No"], "recommended": "Yes", "waits": waits}
    return json.dumps(card, ensure_ascii=False, indent=2)


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


def slack_writes_need_permission(evt: PreToolUseEvent) -> HookResult | None:
    writes = pending(evt)
    if len(writes) > 1 or not writes[0].readable:
        return evt.block(UNREAD)
    write = writes[0]
    conv = Conversation(write.channel, write.thread)
    try:
        with check_deadline():
            conv = conversation(write)
            ctx = context_of(evt, write, conv)
            decided = SLACK.decide(ctx)
    except CHECK_FAILURES as exc:
        return unchecked(evt, write, conv, exc)
    verdict = decided if isinstance(decided, Deny) else decided.ticket
    logger.bind(hook=GATE, agent=evt.agent_id or "main", tool=evt.tool_name, allowed=isinstance(verdict, Ticket), detail=verdict.detail if isinstance(verdict, Deny) else f"{verdict.id} ({verdict.rule})").info("slack write checked")
    if isinstance(verdict, Deny):
        return evt.block(REFUSED.format(ask=ask(write, conv)), system_message=f"{GATE}: {verdict.detail}" if verdict.detail else None)
    if not write.daemon:
        return evt.allow(system_message=f"{GATE}: allowed by ticket `{verdict.id}` ({verdict.rule}).")
    return named_ticket(evt, ctx, verdict)


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


def posted(evt: PostToolUseEvent) -> dict[str, Any] | None:
    return next((found for found in json_objects(evt.tool_response) if found.get("thread_ts") or found.get("ts")), None)


def ticket_behind(evt: PostToolUseEvent, write: Write, result: dict[str, Any]) -> Ticket | None:
    if result.get("ticket_id"):
        return store.load(str(result["ticket_id"]))
    ctx = context_of(evt, write, Conversation(write.channel, write.thread))
    return store.covering(KIND, ctx.trees, ctx.place, ctx.action.fingerprint)


def slack_posts_carry_their_thread(evt: PostToolUseEvent) -> HookResult | None:
    writes = pending(evt)
    if len(writes) != 1 or writes[0].verb != "post" or writes[0].thread or (result := posted(evt)) is None or (ticket := ticket_behind(evt, writes[0], result)) is None:
        return None
    place = {"channel": str(result.get("channel_id") or result.get("channel") or ticket.place["channel"]), "thread": str(result.get("thread_ts") or result["ts"])}
    if ticket.kind != KIND or ticket.rule == UNCHECKED or ticket.place["thread"] or ticket.place["channel"] != place["channel"]:
        return None
    ctx = SLACK.context(evt, Proposal(place, summary=f"replies in {place['channel']}, thread {place['thread']}"))
    if store.covering(KIND, ctx.trees, place, ctx.action.fingerprint) is not None:
        return None
    carried = SLACK.mint(ctx, CARRY, ticket.evidence)
    return evt.context(f"{GATE}: ticket `{carried.id}` carries ticket `{ticket.id}` to the thread this post opened.", system_message=f"{GATE}: ticket {carried.id} covers {place['channel']}, thread {place['thread']}")


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
DANA_ASKED = f'Post this in {FOLLOWERS}, thread {DANA_THREAD}? "{DANA_TEXT}"'
DANA_CARD = f"Post this in {FOLLOWERS}, thread {DANA_THREAD}?"
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
    f"{CC_SLACK} channels": json.dumps({"channels": [{"id": FOLLOWERS, "name": "app-followers"}]}),
    f"{CC_SLACK} thread": json.dumps({"messages": [DANA_OPENED]}),
    "ccn answer search": "[]",
    "cc-present outcomes": "{}",
}
PRIYA_DM = NOT_DM | {f"{CC_SLACK} dm-peer": json.dumps({"peer": PRIYA_USER, "peer_name": "priya"})}
PRIYA_BY_USER = NOT_DM | {f"{CC_SLACK} dm-peer": (1, "dm-peer needs channel_id (or a Slack url)"), f"{CC_SLACK} dm-peer --channel {PRIYA_USER}": PRIYA_DM[f"{CC_SLACK} dm-peer"]}
SAM_USER = "U0SAMOK0001"
SAM_DM = NOT_DM | {f"{CC_SLACK} dm-peer": json.dumps({"peer": SAM_USER, "peer_name": "Sam Okafor", "peer_names": ["Sam Okafor", "sam.okafor"]})}
NONE = "none"
JEV_NONE = {"permits": LabelAnswer(NONE, {NONE: 1.0}, 0.9)}
LLM_SILENT = {"error": TimeoutError("the llm judge timed out")}
LLM_REFUSES = {"reason": "nothing the user said permits this write", "allow": False, "refusal": "Ask the user."}
FIXTURES = Path(__file__).with_name("fixtures")
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
KEPT_IN_CHANNEL = {"id": "f00df00df00df00d", "title": "Where do drive updates go?", "body": f"Keep posting in {FOLLOWERS}", "updated_at": "2026-10-01T00:00:00Z", "tags": ["from:owner", "scope:durable"], "superseded_by": None}
REVOKED = LABELED_GRANT | {"id": "d0d0d0d0d0d0d0d0", "updated_at": "2026-10-17T01:00:00Z", "tags": ["owner-ruling", f"slack-revoke:{FOLLOWERS}"]}


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
    payload = {"type": "slack.action", "watch_id": 7, "ts": "1791950000.000800", "action_ts": action_ts, "label": label, "id": "slack-0a1b2c3d4e5f", "waits": True, "question": question, "options": list(options)}
    tap = f'<channel source="plugin:cc-slack:cc-slack" subject_id="s1" type="slack.action">\n{json.dumps(payload)}\n</channel>'
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


@dataclass(frozen=True, slots=True)
class Case:
    write: dict[str, Any]
    tool: str = "slack_reply"
    command: str | None = None
    transcript: list[dict[str, Any]] | None = None
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


ALLOW = Rewrite(ticket_agent="main")
DENY = Block(pattern="No permission covers this Slack write")


def fixture_stubs(line: dict[str, Any]) -> dict[str, str]:
    commands: dict[str, str] = dict(NOT_DM)
    if line.get("dm"):
        commands[f"{CC_SLACK} dm-peer"] = json.dumps(line["dm"])
    if "channels" in line:
        commands[f"{CC_SLACK} channels"] = json.dumps({"channels": line["channels"]})
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
            case(DIGEST, tool="slack_send", transcript=LANE_BRIEF, root_transcript=owner_answered(DIGEST_ASKED, DIGEST_OPTIONS, DIGEST_OPTIONS[0], id="digest")) >> ALLOW,
            case(DIGEST, tool="slack_send", transcript=LANE_BRIEF, root_transcript=owner_answered(DIGEST_ASKED, DIGEST_OPTIONS, DIGEST_OPTIONS[1], id="digest")) >> DENY,
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
            case(DANA | {"text": "x" * 3000}) >> Block(pattern="… 240 more characters"),
            case(transcript=[T.user("status?")], root_transcript=card_tapped(DANA_CARD, "Yes"), agent_id="a-lane") >> Rewrite(ticket_agent="a-lane"),
            case(transcript=card_tapped(DANA_CARD, "Yes"), root_transcript=[T.user("status?")], agent_id="a-lane") >> DENY,
            case() >> Block(pattern=re.escape(f'"title": "{DANA_CARD}",\n  "body": "{DANA_TEXT}",\n  "options": [\n    "Yes",\n    "No"\n  ],\n  "recommended": "Yes",\n  "waits": "slack-')),
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
            case(tickets=[covering()]) >> Rewrite(ticket_id="0123456789ab"),
            case(DANA | {"text": "another reply"}, tickets=[covering()]) >> Rewrite(ticket_id="0123456789ab"),
            case(tickets=[covering({"channel": FOLLOWERS, "thread": "1791866612.381629"})]) >> DENY,
            case(tickets=[covering({"channel": FOLLOWERS, "thread": ""})]) >> DENY,
            case(tickets=[covering(revoked=datetime(2026, 10, 14, tzinfo=UTC))]) >> DENY,
            case(tickets=[covering(tree="another-tree")]) >> DENY,
            case({"channel_id": FOLLOWERS, "text": "a top-level post", "no_watch": True}, tool="slack_send", tickets=[covering({"channel": FOLLOWERS, "thread": ""})]) >> Rewrite(ticket_id="0123456789ab"),
            case({"channel_id": PRIYA_USER, "thread_ts": "1791900000.000400", "text": "and one more thing"}, commands=PRIYA_DM, tickets=[covering({"channel": PRIYA_USER, "thread": ""})]) >> Rewrite(ticket_id="0123456789ab"),
            case(command=f"{CLI} reply --channel {FOLLOWERS} --thread {DANA_THREAD} --text 'any reply'", tickets=[covering()]) >> Rewrite(pattern="--ticket 0123456789ab --ticket-tree fixture --ticket-agent main"),
            case(command=f"{CLI} reply --channel {FOLLOWERS} --thread {DANA_THREAD} --text 'any reply'") >> DENY,
            case(command=f"{CLI} send --channel {FOLLOWERS} --text 'a post' --ticket 0123456789ab") >> Block(pattern="literal values"),
            case(command=f'{CLI} send --channel {FOLLOWERS} --text "$(cat reply.txt)"') >> Block(pattern="literal values"),
            case(command=f"{CLI} send --channel {FOLLOWERS} --text a && {CLI} reply --channel {FOLLOWERS} --thread 1 --text b") >> Block(pattern="one write per command"),
            case(command=f"{SLACK_POST} send --channel {FOLLOWERS} --thread {DANA_THREAD} --text '{DANA_TEXT}'", tickets=[covering()]) >> Allow(),
            case(command=f"{SLACK_POST} send --channel {FOLLOWERS} --thread {DANA_THREAD} --text '{DANA_TEXT}'") >> DENY,
            case(DANA | {"text": f"_(Alex's Claude)_ {DANA_TEXT}"}, tool="mcp__slack__slack_send_message", tickets=[covering()]) >> Allow(),
            case(DANA | {"text": DANA_TEXT}, tool="mcp__slack__slack_send_message") >> DENY,
            case({"url": DANA_URL}, tool="slack_delete", tickets=[covering({"channel": FOLLOWERS, "thread": ""})]) >> Rewrite(ticket_id="0123456789ab"),
            case({"url": DANA_URL}, tool="slack_delete") >> Block(pattern="Delete the bot's Slack message"),
            case({"channel_id": FOLLOWERS, "ts": BOT_ANSWERED["ts"]}, tool="slack_delete", tickets=[covering()], commands=NOT_DM | thread_read(DANA_OPENED, BOT_ANSWERED | {"thread_ts": DANA_THREAD})) >> Rewrite(ticket_id="0123456789ab"),
            case({"channel_id": FOLLOWERS, "ts": BOT_ANSWERED["ts"]}, tool="slack_delete", tickets=[covering({"channel": FOLLOWERS, "thread": ""})], commands=NOT_DM | thread_read(DANA_OPENED, BOT_ANSWERED | {"thread_ts": DANA_THREAD})) >> Block(pattern="Delete the bot's Slack message"),
            case({"channel_id": FOLLOWERS, "ts": BOT_ANSWERED["ts"]}, tool="slack_delete", tickets=[covering({"channel": FOLLOWERS, "thread": ""})], commands=NOT_DM | {f"{CC_SLACK} thread": ""}) >> Rewrite(ticket_id="0123456789ab"),
            case(PRIYA_EDIT, tool="slack_edit", tickets=[covering({"channel": PRIYA_USER, "thread": ""})], commands=PRIYA_DM | {f"{CC_SLACK} thread": (1, "thread_not_found")}) >> Rewrite(ticket_id="0123456789ab"),
            case(PRIYA_EDIT, tool="slack_edit", commands=PRIYA_DM | {f"{CC_SLACK} thread": (1, "thread_not_found")}) >> Block(pattern="Edit the bot's message to this"),
            case(PRIYA_EDIT, tool="slack_edit", commands=NOT_DM | {f"{CC_SLACK} dm-peer": (1, "ratelimited")}) >> ALLOW,
            case(command=f"{CLI} reply --channel {FOLLOWERS} --thread {DANA_THREAD} --text 'any reply'", commands=NOT_DM | {f"{CC_SLACK} dm-peer": (1, "ratelimited")}) >> Rewrite(pattern="--ticket-agent main"),
            case(DANA | {"text": f"_(Alex's Claude)_ {DANA_TEXT}"}, tool="mcp__slack__slack_send_message", commands=NOT_DM | {f"{CC_SLACK} dm-peer": (1, "ratelimited")}) >> Allow(explicit=True),
            case(**said("let dana know the fix landed, in her thread"), decide={"error": TimeoutError("jev down")}, llm=LLM_SILENT) >> ALLOW,
            case(command=f"{CLI} send --user {PRIYA_USER} --text '{PRIYA_DM_TEXT}'", transcript=owner_answered(PRIYA_ASKED, ["DM priya and ask her to try posting it", "I'll paste it myself"], "DM priya and ask her to try posting it", id="doc-link"), commands=PRIYA_BY_USER) >> Rewrite(pattern="--ticket-agent main"),
            case(command=f"{CLI} send --user {PRIYA_USER} --text '{PRIYA_DM_TEXT}'", commands=PRIYA_BY_USER) >> DENY,
            case({"channel_id": FOLLOWERS}, tool="slack_channel_archive", transcript=owner_answered(f"Archive {FOLLOWERS}, the stale incident channel?", ["Yes", "No"], "Yes", id="archive")) >> ALLOW,
            case({"channel_id": FOLLOWERS}, tool="slack_channel_archive") >> Block(pattern="Archive the Slack channel"),
            case(DANA | {"ts": DANA_THREAD, "name": "eyes"}, tool="slack_react") >> Allow(),
            case({"text": "status"}, tool="slack_dm_status") >> Allow(),
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
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(LABELED_GRANT)) >> ALLOW,
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(LABELED_GRANT, REVOKED)) >> DENY,
            case(DANA | {"text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(LABELED_GRANT) | thread_read(DANA_OPENED)) >> DENY,
            case(DANA | {"text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(KEPT_POSTING) | thread_read(DANA_OPENED)) >> DENY,
            case(DANA | {"text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(LABELED_GRANT, THREAD_GRANT) | thread_read(DANA_OPENED)) >> ALLOW,
            case(DANA | {"text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(THREAD_GRANT) | thread_read(DANA_OPENED)) >> ALLOW,
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
            case(LANDED, tool="slack_send", **said("status?"), commands=ruled(KEPT_IN_CHANNEL)) >> ALLOW,
            case(DANA | {"text": "fix is out"}, tool="slack_reply", **said("status?"), commands=ruled(KEPT_IN_CHANNEL) | thread_read(DANA_OPENED)) >> DENY,
            case(env=DISABLED) >> Allow(),
            case(env={}) >> Allow(),
            case(command=f"{SLACK_POST} send --channel {FOLLOWERS} --text 'a post'", env=DISABLED) >> Allow(),
            *fixture_cases().items(),
        ]
    ),
)(slack_writes_need_permission)


on(
    Event.PostToolUse,
    only_if=[WritesSlack()],
    tests={
        Input(
            env=ENABLED,
            tool=P + "slack_send",
            tool_input={"channel_id": FOLLOWERS, "text": "a top-level post"},
            output=json.dumps({"posted": True, "channel_id": FOLLOWERS, "ts": "1791950000.000100", "thread_ts": "1791950000.000100", "ticket_id": "0123456789ab"}),
            tickets=[covering({"channel": FOLLOWERS, "thread": ""})],
        ): Warn(pattern="carries ticket `0123456789ab` to the thread this post opened", system_message=f"covers {FOLLOWERS}, thread 1791950000.000100"),
        Input(
            env=ENABLED,
            tool=P + "slack_reply",
            tool_input=DANA | {"text": "a reply"},
            output=json.dumps({"posted": True, "channel_id": FOLLOWERS, "ts": "1791950000.000200", "thread_ts": DANA_THREAD, "ticket_id": "0123456789ab"}),
            tickets=[covering()],
        ): Allow(),
        Input(
            env=ENABLED,
            command=f"{CLI} send --channel {FOLLOWERS} --text 'a top-level post' --ticket 0123456789ab",
            output=json.dumps({"posted": True, "channel_id": FOLLOWERS, "ts": "1791950000.000300", "thread_ts": "1791950000.000300", "ticket_id": "0123456789ab"}),
            tickets=[covering({"channel": FOLLOWERS, "thread": ""})],
        ): Warn(pattern="carries ticket `0123456789ab` to the thread this post opened", system_message=f"covers {FOLLOWERS}, thread 1791950000.000300"),
        Input(env=ENABLED, tool=P + "slack_send", tool_input={"channel_id": FOLLOWERS, "text": "a post"}, output="rate limited; nothing was sent", tickets=[covering({"channel": FOLLOWERS, "thread": ""})]): Allow(),
        Input(
            env=ENABLED,
            tool=P + "slack_send",
            tool_input={"channel_id": FOLLOWERS, "text": "a top-level post"},
            output=json.dumps({"posted": True, "channel_id": FOLLOWERS, "ts": "1791950000.000700", "thread_ts": "1791950000.000700", "ticket_id": "0123456789ab"}),
            tickets=[covering({"channel": FOLLOWERS, "thread": ""}, rule=UNCHECKED)],
        ): Allow(),
        Input(
            env=ENABLED,
            command=f"{SLACK_POST} send --channel {FOLLOWERS} --text 'a top-level post'",
            output=json.dumps({"channel_id": FOLLOWERS, "ts": "1791950000.000400", "thread_ts": "1791950000.000400", "permalink": f"https://acme.slack.com/archives/{FOLLOWERS}/p1791950000000400", "username": "Alex (claude)"}),
            tickets=[covering({"channel": FOLLOWERS, "thread": ""})],
        ): Warn(pattern="carries ticket `0123456789ab` to the thread this post opened", system_message=f"covers {FOLLOWERS}, thread 1791950000.000400"),
        Input(
            env=ENABLED,
            tool="mcp__slack__slack_send_message",
            tool_input={"channel_id": FOLLOWERS, "text": "_(Alex's Claude)_ a top-level post"},
            output=json.dumps({"ok": True, "channel": FOLLOWERS, "ts": "1791950000.000500"}),
            tickets=[covering({"channel": FOLLOWERS, "thread": ""})],
        ): Warn(pattern="carries ticket `0123456789ab` to the thread this post opened", system_message=f"covers {FOLLOWERS}, thread 1791950000.000500"),
        Input(env=ENABLED, tool="mcp__slack__slack_send_message", tool_input={"channel_id": FOLLOWERS, "text": "_(Alex's Claude)_ a top-level post"}, output=json.dumps({"ok": True, "channel": FOLLOWERS, "ts": "1791950000.000600"})): Allow(),
        Input(
            env=DISABLED,
            tool=P + "slack_send",
            tool_input={"channel_id": FOLLOWERS, "text": "a top-level post"},
            output=json.dumps({"posted": True, "channel_id": FOLLOWERS, "ts": "1791950000.000800", "thread_ts": "1791950000.000800", "ticket_id": "0123456789ab"}),
            tickets=[covering({"channel": FOLLOWERS, "thread": ""})],
        ): Allow(),
    },
)(slack_posts_carry_their_thread)
