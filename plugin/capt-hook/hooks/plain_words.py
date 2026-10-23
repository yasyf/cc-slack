from __future__ import annotations

import importlib.util
import sys
from functools import cache
from importlib.machinery import SourceFileLoader
from pathlib import Path
from types import ModuleType
from typing import Any

from captain_hook import Allow, Event, HookResult, Input, PreToolUseEvent, Warn, on

from .common import PLUGIN_ROOT
from .writes import pending, permissions

PLAIN_WORDS = {
    "commit-sha": "raw commit hashes",
    "codename": "codenames",
    "drive-term": "drive terms",
    "ruling-id": "inbox ids",
    "internal-id": "internal ids",
    "zone-label": "zone labels",
    "ulid": "ULIDs",
    "bare-number": "unlinked numbers",
    "bare-url": "bare URLs",
}
PLAIN_WORDS_WARNING = "Slack readers outside this session cannot decode {found}; write the next post from the facts."
LINK_KINDS = frozenset({"bare-number", "bare-url"})
LINK_WARNING = "Slack readers cannot follow {found}; link them as <url|label> in the next post."
BLOCK_STRUCTURE = frozenset({"type", "block_id", "action_id", "style", "value", "id"})
P = "mcp__plugin_cc-slack_cc-slack__"
CLI = "cc-slack"
THREAD = {"channel_id": "C0APPTHREAD", "thread_ts": "1790210144.444179"}
THREAD_LINK = "https://acme.slack.com/archives/C0APPTHREAD/p1790210144444179"
FIXTURES = Path(__file__).with_name("fixtures")
ENABLED = {"CLAUDE_PROJECT_DIR": str(FIXTURES / "enabled")}
DISABLED = {"CLAUDE_PROJECT_DIR": str(FIXTURES / "disabled")}
JARGON = THREAD | {"text": "Heads-up: D landed today, so the walker is behind the fence on api until 1:45 PM PT."}
PLAIN = THREAD | {"text": "Thanks <@U0DANA00001>, the <https://github.com/acme/app/pull/28345|new release pipeline> merged today, so rebase onto dev first."}
CARD_BLOCKS = [
    {"type": "header", "text": {"type": "plain_text", "text": "Edge fix shipped"}},
    {"type": "section", "text": {"type": "mrkdwn", "text": "Errors are back to baseline."}},
]


@cache
def check_post() -> ModuleType:
    loader = SourceFileLoader("check_post", str(PLUGIN_ROOT / "skills" / "slack" / "scripts" / "check-post"))
    module = importlib.util.module_from_spec(importlib.util.spec_from_loader(loader.name, loader))
    sys.modules[loader.name] = module
    loader.exec_module(module)
    return module


def block_text(node: object) -> list[str]:
    match node:
        case str():
            return [node]
        case list():
            return [line for item in node for line in block_text(item)]
        case dict():
            return [line for key, value in node.items() if key not in BLOCK_STRUCTURE for line in block_text(value)]
    return []


def posted_text(fields: dict[str, Any]) -> str:
    blocks = fields.get("blocks")
    return "\n".join([str(fields.get("text") or ""), *block_text(blocks if isinstance(blocks, list) else [])])


class WritesSlack:
    def check(self, evt: PreToolUseEvent) -> bool:
        return permissions().enabled and any(write.verb in ("post", "edit") for write in pending(evt))


@on(
    Event.PreToolUse,
    only_if=[WritesSlack()],
    max_fires=1,
    tests={
        Input(env=ENABLED, tool=P + "slack_reply", tool_input=JARGON): Warn(pattern="decode codenames, drive terms, zone labels"),
        Input(env=ENABLED, tool="mcp__slack__slack_send_message", tool_input=THREAD | {"text": "R885 is lifted at 17:35Z."}): Warn(pattern="decode inbox ids, zone labels"),
        Input(env=ENABLED, tool=P + "slack_edit", tool_input=JARGON | {"ts": THREAD["thread_ts"]}): Warn(pattern="zone labels"),
        Input(env=ENABLED, command=f"{CLI} reply --url {THREAD_LINK} --text 'the merge-walker lane is on it'"): Warn(pattern="decode drive terms;"),
        Input(env=ENABLED, tool=P + "slack_reply", tool_input=PLAIN): Allow(),
        Input(env=ENABLED, tool=P + "slack_reply", tool_input=PLAIN | {"blocks": [{"type": "section", "text": {"type": "mrkdwn", "text": JARGON["text"]}}]}): Warn(
            pattern="decode codenames, drive terms, zone labels"
        ),
        Input(env=ENABLED, tool=P + "slack_reply", tool_input=PLAIN | {"blocks": CARD_BLOCKS}): Allow(),
        Input(env=ENABLED, tool=P + "slack_reply", tool_input=THREAD | {"text": "Fixed in #31364, see https://example.com/run"}): Warn(pattern="cannot follow unlinked numbers, bare URLs"),
        Input(env=ENABLED, command=f"{CLI} reply --url {THREAD_LINK} --text '{PLAIN['text']}'"): Allow(),
        Input(env=ENABLED, command=f"slack-post send --url {THREAD_LINK} --text 'the merge-walker lane is on it'"): Warn(pattern="decode drive terms;"),
        Input(env=ENABLED, tool=P + "slack_dm_status", tool_input={"text": "D is queued behind the walker"}): Allow(),
        Input(env=DISABLED, tool=P + "slack_reply", tool_input=JARGON): Allow(),
    },
)
def slack_posts_use_plain_words(evt: PreToolUseEvent) -> HookResult | None:
    findings = [finding.kind for write in pending(evt) if write.verb in ("post", "edit") for finding in check_post().check(posted_text(write.fields))]
    if found := dict.fromkeys(PLAIN_WORDS[kind] for kind in findings if kind not in LINK_KINDS):
        return evt.warn(PLAIN_WORDS_WARNING.format(found=", ".join(found)))
    if unlinked := dict.fromkeys(PLAIN_WORDS[kind] for kind in findings):
        return evt.warn(LINK_WARNING.format(found=", ".join(unlinked)))
    return None
