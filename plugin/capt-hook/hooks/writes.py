from __future__ import annotations

import subprocess
import tomllib
from dataclasses import dataclass, replace
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
DRIVE_SAY = "mcp__plugin_long-running_drive__say"
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
STREAM_VERBS = {"stream": "post"}
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
    "apply": "apply",
}
MESSAGE_VERBS = frozenset({"post", "edit", "delete"})
READ_TOOLS = frozenset({"slack_thread", "slack_get_thread"})
READ_VERBS = {"thread": "read"}
THREADED_VERBS = MESSAGE_VERBS | set(READ_VERBS.values())
VALUED = ("agent", "channel", "thread", "ts", "url", "text", "text-file", "file", "blocks", "name", "user", "topic", "session", "cwd", "limit", "since-ts", "plan", "task", "recipient")
HELP = Option("help", ("--help", "-h"), bool)
CLI = CommandSchema(
    "cc-slack",
    operands=(Operand("verb"),),
    options=(
        *(Option(name, (f"--{name}",)) for name in VALUED),
        *(Option(name, (f"--{name}",), bool) for name in ("broadcast", "no-watch", "stop", "dry-run")),
        HELP,
    ),
)
FIELDS = {"channel": "channel_id", "thread": "thread_ts", "file": "files", "user": "users", "text-file": "text_file", "task": "tasks"}


@dataclass(frozen=True, slots=True)
class ExtraCLI:
    schema: CommandSchema
    verbs: dict[str, str]


@dataclass(frozen=True, slots=True)
class Permissions:
    enabled: bool = False
    clis: tuple[ExtraCLI, ...] = ()
    proof_channels: tuple[str, ...] = ()


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
    return Permissions(bool(table.get("enabled")), tuple(extra_cli(cli) for cli in table.get("clis", ())), tuple(table.get("proof_channels", ())))


def permissions() -> Permissions:
    project = resolve_project_dir()
    path = Path(project) / CONFIG if project else None
    if path is None or not path.is_file():
        return Permissions()
    return parsed(path, path.stat().st_mtime_ns)


def task_words(task: dict[str, str] | str) -> list[str]:
    if isinstance(task, dict):
        return [task["title"], task.get("details", "")]
    return task.split("|", 3)[2:]


def stream_words(fields: dict[str, Any]) -> str:
    words = [word for task in fields.get("tasks") or [] for word in task_words(task)]
    return "\n".join(word for word in [str(fields.get("plan") or ""), *words] if word)


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
    stream: bool = False

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
        thread = str(self.fields.get("thread_ts") or self.link[1]) if self.verb in THREADED_VERBS else ""
        return "" if thread == self.ts else thread

    @property
    def text(self) -> str:
        if self.verb not in ("post", "edit", "topic"):
            return ""
        if self.stream:
            return stream_words(self.fields)
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
        payload = {"verb": self.verb, "channel": self.channel, "thread": self.thread, "text": text, "files": self.files, "broadcast": bool(self.fields.get("broadcast")), "ts": self.ts}
        return payload | ({"blocks": self.fields["blocks"]} if self.fields.get("blocks") else {}) | ({"users": self.users} if self.users else {}) | ({"stream": True} if self.stream else {})


def bound_calls(evt: ToolHookEvent, schema: CommandSchema, verbs: dict[str, str], *, daemon: bool) -> list[tuple[Call, Write]]:
    found = []
    for call in evt.cmd.calls(schema.program) if evt.tool_name == "Bash" else ():
        bound = schema.bind(call)
        values = {name: list(given) if name in ("file", "user", "task") else given[-1] for name, given in bound.values.items() if given}
        if (verb := verbs.get(str(values.pop("verb", "")))) is not None and not values.get("help"):
            fields = {FIELDS.get(name, name): value for name, value in values.items()}
            if verb in MESSAGE_VERBS and (users := fields.pop("users", None)):
                fields.setdefault("channel_id", users[-1])
            found.append((call, Write(verb, fields, daemon=daemon, readable=bound.complete)))
    return found


def cli_calls(evt: ToolHookEvent) -> list[tuple[Call, Write]]:
    streams = [(call, replace(write, stream=True)) for call, write in bound_calls(evt, CLI, STREAM_VERBS, daemon=True) if not write.fields.get("ts")]
    return [*bound_calls(evt, CLI, CLI_VERBS, daemon=True), *streams]


def pending(evt: ToolHookEvent) -> list[Write]:
    tool, raw = (evt.tool_name or "").rsplit("__", 1)[-1], evt.input.raw
    if evt.tool_name == DRIVE_SAY:
        return [Write("post", dict(raw), daemon=True)] if raw.get("channel_id") and not raw.get("dry_run") else []
    if tool == "slack_stream":
        return [] if raw.get("ts") else [Write("post", dict(raw), daemon=True, stream=True)]
    if tool in TOOL_VERBS:
        return [Write(TOOL_VERBS[tool], dict(raw), daemon=tool not in USER_TOOLS)]
    extra = [write for cli in permissions().clis for _, write in bound_calls(evt, cli.schema, cli.verbs, daemon=False)]
    return [*(write for _, write in cli_calls(evt)), *extra]


def read_threads(evt: ToolHookEvent) -> list[str]:
    tool = (evt.tool_name or "").rsplit("__", 1)[-1]
    reads = [Write("read", dict(evt.input.raw), daemon=True)] if tool in READ_TOOLS else [write for _, write in bound_calls(evt, CLI, READ_VERBS, daemon=True) if write.readable]
    return [read.thread for read in reads if read.thread]
