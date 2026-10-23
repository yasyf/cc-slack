from __future__ import annotations

import subprocess
import tomllib
from dataclasses import dataclass
from functools import cache
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, urlparse

from captain_hook import CommandSchema, Operand
from captain_hook.cmd import Call
from captain_hook.command_schema import Option
from captain_hook.events import ToolHookEvent
from captain_hook.util.paths import resolve_project_dir

__capt_hook_skip__ = True

CONFIG = Path(".claude", "cc-slack.toml")
TOOL_VERBS = {
    "slack_send": "post",
    "slack_reply": "post",
    "slack_send_message": "post",
    "slack_edit": "edit",
    "slack_delete": "delete",
    "slack_channel_create": "create",
    "slack_channel_archive": "archive",
    "slack_channel_invite": "invite",
    "slack_channel_topic": "topic",
    "slack_channel_join": "join",
    "slack_conversations_mark": "mark",
    "slack_workflow_save": "workflow",
}
USER_TOOLS = frozenset({"slack_send_message", "slack_conversations_mark", "slack_workflow_save"})
CLI_VERBS = {
    "send": "post",
    "reply": "post",
    "edit": "edit",
    "delete": "delete",
    "channel-create": "create",
    "channel-archive": "archive",
    "channel-invite": "invite",
    "channel-topic": "topic",
    "channel-join": "join",
}
MESSAGE_VERBS = frozenset({"post", "edit", "delete"})
VALUED = ("channel", "thread", "ts", "url", "text", "text-file", "file", "blocks", "name", "user", "topic", "session", "cwd")
HELP = Option("help", ("--help", "-h"), bool)
CLI = CommandSchema(
    "cc-slack",
    operands=(Operand("verb"),),
    options=(
        *(Option(name, (f"--{name}",)) for name in VALUED),
        *(Option(name, (f"--{name}",), bool) for name in ("broadcast", "no-watch")),
        HELP,
    ),
)
FIELDS = {"channel": "channel_id", "thread": "thread_ts", "file": "files", "user": "users", "text-file": "text_file"}


@dataclass(frozen=True, slots=True)
class ExtraCLI:
    schema: CommandSchema
    verbs: dict[str, str]


@dataclass(frozen=True, slots=True)
class Permissions:
    enabled: bool = False
    clis: tuple[ExtraCLI, ...] = ()


def extra_cli(table: dict[str, Any]) -> ExtraCLI:
    options = (
        *(Option(name, (f"--{name}",)) for name in table.get("options", ())),
        *(Option(name, (f"--{name}",), bool) for name in table.get("flags", ())),
        HELP,
    )
    return ExtraCLI(CommandSchema(table["program"], operands=(Operand("verb"),), options=options), dict(table["verbs"]))


@cache
def parsed(path: Path, modified: int) -> Permissions:
    table = tomllib.loads(path.read_text()).get("permissions", {})
    return Permissions(bool(table.get("enabled")), tuple(extra_cli(cli) for cli in table.get("clis", ())))


def permissions() -> Permissions:
    project = resolve_project_dir()
    path = Path(project) / CONFIG if project else None
    if path is None or not path.is_file():
        return Permissions()
    return parsed(path, path.stat().st_mtime_ns)


def capture(argv: list[str], timeout: float) -> str | None:
    done = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
    return done.stdout if done.returncode == 0 else None


def permalink(url: str) -> tuple[str, str, str]:
    parsed_url = urlparse(url)
    parts = parsed_url.path.strip("/").split("/")
    channel = parts[1] if len(parts) > 1 and parts[0] == "archives" else ""
    stamp = parts[2][1:] if len(parts) > 2 and parts[2].startswith("p") else ""
    ts = f"{stamp[:-6]}.{stamp[-6:]}" if len(stamp) > 6 else ""
    return channel, parse_qs(parsed_url.query).get("thread_ts", [ts])[0], ts


@dataclass(frozen=True, slots=True)
class Write:
    verb: str
    fields: dict[str, Any]
    daemon: bool
    readable: bool = True

    @property
    def link(self) -> tuple[str, str, str]:
        return permalink(str(self.fields.get("url") or ""))

    @property
    def channel(self) -> str:
        if self.verb == "create":
            return str(self.fields.get("name") or "")
        return str(self.fields.get("channel_id") or self.link[0])

    @property
    def ts(self) -> str:
        return str(self.fields.get("ts") or self.link[2]) if self.verb in ("edit", "delete") else ""

    @property
    def thread(self) -> str:
        thread = str(self.fields.get("thread_ts") or self.link[1]) if self.verb in MESSAGE_VERBS else ""
        return "" if thread == self.ts else thread

    @property
    def text(self) -> str:
        if self.verb not in ("post", "edit", "topic"):
            return ""
        if self.verb != "topic" and (path := self.fields.get("text_file")):
            return Path(str(path)).expanduser().read_text()
        return str(self.fields.get("topic" if self.verb == "topic" else "text") or "")

    @property
    def files(self) -> list[str]:
        return [str(path) for path in self.fields.get("files") or []]

    @property
    def users(self) -> list[str]:
        return sorted(str(user) for user in self.fields.get("users") or [])

    @property
    def payload(self) -> dict[str, Any]:
        text = self.channel if self.verb == "create" else self.text
        payload = {"verb": self.verb, "text": text, "files": self.files, "broadcast": bool(self.fields.get("broadcast")), "ts": self.ts}
        return payload | ({"blocks": self.fields["blocks"]} if self.fields.get("blocks") else {}) | ({"users": self.users} if self.users else {})


def bound_calls(evt: ToolHookEvent, schema: CommandSchema, verbs: dict[str, str], *, daemon: bool) -> list[tuple[Call, Write]]:
    found = []
    for call in evt.cmd.calls(schema.program) if evt.tool_name == "Bash" else ():
        bound = schema.bind(call)
        values = {name: list(given) if name in ("file", "user") else given[-1] for name, given in bound.values.items() if given}
        if (verb := verbs.get(str(values.pop("verb", "")))) is not None and not values.get("help"):
            fields = {FIELDS.get(name, name): value for name, value in values.items()}
            if verb in MESSAGE_VERBS and (users := fields.pop("users", None)):
                fields.setdefault("channel_id", users[-1])
            found.append((call, Write(verb, fields, daemon=daemon, readable=bound.complete)))
    return found


def cli_calls(evt: ToolHookEvent) -> list[tuple[Call, Write]]:
    return bound_calls(evt, CLI, CLI_VERBS, daemon=True)


def pending(evt: ToolHookEvent) -> list[Write]:
    tool, raw = (evt.tool_name or "").rsplit("__", 1)[-1], evt.input.raw
    if tool in TOOL_VERBS:
        return [Write(TOOL_VERBS[tool], dict(raw), daemon=tool not in USER_TOOLS)]
    extra = [write for cli in permissions().clis for _, write in bound_calls(evt, cli.schema, cli.verbs, daemon=False)]
    return [*(write for _, write in cli_calls(evt)), *extra]
