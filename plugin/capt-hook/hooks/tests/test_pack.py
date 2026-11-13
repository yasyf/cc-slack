# /// script
# dependencies = ["capt-hook>=12.104.0"]
# ///

from __future__ import annotations

import json
import os
import sqlite3
import sys
import tempfile
import unittest
from contextlib import closing
from datetime import UTC, datetime, timedelta
from pathlib import Path

from captain_hook import Action, Event, app
from captain_hook.loader import discover_pack
from captain_hook.packs.manager import pack_module_name
from captain_hook.session import SessionStore
from captain_hook.testing.helpers import mock_event, mock_tool_event

HOOKS = Path(__file__).resolve().parents[1]
PLUGIN_ROOT = Path(__file__).resolve().parents[3]
TOOLS = "mcp__plugin_cc-slack_cc-slack__"
THREAD = {"channel_id": "C0APPTHREAD", "thread_ts": "1790210144.444179"}


class PackTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        app.reset()
        name = PLUGIN_ROOT.name
        discover_pack(name, HOOKS)
        cls.package = f"captain_hook._packs.{pack_module_name(name)}"
        cls.common = sys.modules[f"{cls.package}.common"]
        cls.agent_plane = sys.modules[f"{cls.package}.agent_plane"]
        cls.session = sys.modules[f"{cls.package}.session"]
        cls.read_guard = sys.modules[f"{cls.package}.read_before_reply"]
        cls.original_bin = cls.common.BIN

    @classmethod
    def tearDownClass(cls) -> None:
        cls.common.BIN = cls.original_bin
        app.reset()

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.home = Path(self.tmp.name) / "home"
        self.home.mkdir()
        self.saved_home = os.environ.get("DAEMONKIT_HOME")
        os.environ["DAEMONKIT_HOME"] = str(self.home)

    def tearDown(self) -> None:
        self.common.BIN = self.original_bin
        if self.saved_home is None:
            os.environ.pop("DAEMONKIT_HOME", None)
        else:
            os.environ["DAEMONKIT_HOME"] = self.saved_home
        self.tmp.cleanup()

    def plane(self, *, scope: str = "cc-slack", pending_for: str | None = None) -> None:
        db = self.home / ".local" / "state" / "cc-slack" / "cc-interact-v1" / "state.db"
        db.parent.mkdir(parents=True)
        with closing(sqlite3.connect(db)) as conn, conn:
            conn.execute("CREATE TABLE subjects (id TEXT PRIMARY KEY, scope TEXT)")
            conn.execute("CREATE TABLE directives (subject_id TEXT, agent_id TEXT, delivered_at TEXT)")
            conn.execute("INSERT INTO subjects VALUES ('s1', ?)", (scope,))
            if pending_for is not None:
                conn.execute("INSERT INTO directives VALUES ('s1', ?, NULL)", (pending_for,))

    def stub(
        self,
        *,
        stdout: str = "",
        stdout_bytes: bytes | None = None,
        stderr: str = "",
        returncode: int = 0,
    ) -> Path:
        root = Path(self.tmp.name)
        record = root / "call.json"
        binary = root / "stub"
        output = stdout.encode() if stdout_bytes is None else stdout_bytes
        binary.write_text(
            "#!/usr/bin/env python3\n"
            "import json, pathlib, sys\n"
            f"pathlib.Path({str(record)!r}).write_text(json.dumps({{'argv': sys.argv[1:], 'stdin': sys.stdin.read()}}))\n"
            f"sys.stdout.buffer.write({output!r})\n"
            f"sys.stderr.write({stderr!r})\n"
            f"raise SystemExit({returncode})\n"
        )
        binary.chmod(0o755)
        self.common.BIN = binary
        return record

    def test_discover_pack_loads_every_module_and_hook(self) -> None:
        self.assertEqual(app._state.load_errors, [])
        self.assertEqual(len(app._state.hooks), 13)
        for module in ("agent_plane", "common", "slack_comms_need_proof", "session", "permission", "plain_words", "read_before_reply", "writes"):
            self.assertIn(f"{self.package}.{module}", sys.modules)

        expected = {
            "agent_start": (Event.SubagentStart, False, False),
            "agent_inject": (Event.PreToolUse, False, True),
            "agent_stop": (Event.SubagentStop, False, False),
            "agent_report": (Event.PostToolUse, True, True),
            "session_record": (Event.SessionStart, True, True),
            "slack_writes_need_permission": (Event.PreToolUse, False, True),
            "slack_posts_use_plain_words": (Event.PreToolUse, False, True),
            "comms_never_asks_for_evidence": (Event.PreToolUse, False, True),
            "fixed_lines_name_the_proven_user": (Event.PreToolUse, False, True),
            "replies_read_their_thread_first": (Event.PreToolUse, False, True),
            "thread_reads_are_recorded": (Event.PostToolUse, False, True),
            "own_replies_are_recorded": (Event.PostToolUse, False, True),
            "pickups_are_recorded": (Event.PostToolUse, False, True),
        }
        actual = {
            hook.name: (hook.spec.events, hook.spec.async_, hook.spec.skip_planning_agents)
            for hook in app._state.hooks
        }
        self.assertEqual(actual, expected)

    def test_agent_inject_emits_verbatim_context_without_approval(self) -> None:
        text = (
            "Directives from the demo steering channel (operator-authorized):\n"
            "- [operator #17] inspect the exact payload\n"
            "Act on each directive once, then continue your task."
        )
        envelope = json.dumps(
            {"hookSpecificOutput": {"hookEventName": "PreToolUse", "additionalContext": text}}
        )
        evt = mock_event(Event.PreToolUse, tool="Bash", command="go test ./...")
        self.stub(stdout=envelope + "\n")
        result = self.agent_plane.agent_inject(evt)
        self.assertEqual(result.action, Action.warn)
        self.assertEqual(result.message, text)
        self.assertFalse(result.approve)

    def test_call_bin_execs_the_artifact_binrun_recorded(self) -> None:
        cases = (
            ("fresh", True, "", "artifact"),
            ("stale", False, "", "launcher"),
            ("pinned-runner", True, "/opt/dev/binrun", "launcher"),
            ("unspawnable", True, "", "launcher"),
        )
        for label, fresh, runner, expected in cases:
            with self.subTest(label):
                artifact = self.stub(stdout="artifact").with_name("stub")
                if label == "unspawnable":
                    artifact.write_text("#!/nonexistent/interpreter\n")
                plugin = Path(self.tmp.name) / label / "plugin"
                (plugin / "bin").mkdir(parents=True)
                launcher = plugin / "bin" / "cc-slack"
                launcher.write_text("#!/bin/sh\necho launcher\n")
                launcher.chmod(0o755)
                descriptor = plugin / "bin" / "cc-slack.binrun"
                descriptor.write_text("{}")
                name = str(descriptor).replace("%", "%25").replace("/", "%2F")
                record = self.home / ".daemonkit" / "binrun" / "resolved" / name
                record.parent.mkdir(parents=True, exist_ok=True)
                record.write_text(f"{artifact}\n")
                stamp = descriptor.stat().st_mtime + (60 if fresh else -60)
                os.utime(record, (stamp, stamp))
                self.common.BIN = launcher
                os.environ["BINRUN_BIN"] = runner
                self.addCleanup(os.environ.pop, "BINRUN_BIN", None)
                evt = mock_event(Event.PreToolUse, tool="Bash", command="go test ./...")
                self.assertEqual((self.common.call_bin(evt, "agent-inject") or "").strip(), expected)

    def test_agent_inject_rejects_invalid_envelopes(self) -> None:
        evt = mock_event(Event.PreToolUse, tool="Bash", command="go test ./...")
        for label, envelope in (
            (
                "wrong-event",
                {"hookSpecificOutput": {"hookEventName": "PostToolUse", "additionalContext": "text"}},
            ),
            ("missing-event", {"hookSpecificOutput": {"additionalContext": "text"}}),
            (
                "empty-context",
                {"hookSpecificOutput": {"hookEventName": "PreToolUse", "additionalContext": ""}},
            ),
        ):
            with self.subTest(case=label):
                self.stub(stdout=json.dumps(envelope))
                self.assertIsNone(self.agent_plane.agent_inject(evt))

    def test_agent_stop_translates_block_decision(self) -> None:
        evt = mock_event(Event.SubagentStop, agent_type="Explore", agent_id="a1")
        self.stub(stdout='{"decision":"block","reason":"finish the requested check"}\n')
        result = self.agent_plane.agent_stop(evt)
        self.assertEqual(result.action, Action.block)
        self.assertEqual(result.message, "finish the requested check")

    def test_parsing_hooks_fail_open(self) -> None:
        inject_evt = mock_event(Event.PreToolUse, tool="Bash", command="ls")
        stop_evt = mock_event(Event.SubagentStop, agent_type="worker", agent_id="a1")
        for label, stdout, returncode in (
            ("garbage", "not-json", 0),
            ("non-zero", "", 1),
            ("empty", "", 0),
        ):
            with self.subTest(hook="agent-inject", case=label):
                self.stub(stdout=stdout, returncode=returncode)
                self.assertIsNone(self.agent_plane.agent_inject(inject_evt))
            with self.subTest(hook="agent-stop", case=label):
                self.stub(stdout=stdout, returncode=returncode)
                self.assertIsNone(self.agent_plane.agent_stop(stop_evt))

    def test_side_effect_hooks_forward_raw_payload_and_return_none(self) -> None:
        cases = (
            (self.agent_plane.agent_start, "agent-start", mock_event(Event.SubagentStart, agent_id="a1")),
            (self.agent_plane.agent_report, "agent-report", mock_event(Event.PostToolUse, tool="Task", prompt="inspect")),
            (self.session.session_record, "session-record", mock_event(Event.SessionStart, source="startup")),
        )
        for hook, subcommand, evt in cases:
            with self.subTest(subcommand=subcommand):
                evt._raw["contract_marker"] = subcommand
                record = self.stub(stdout="ignored output")
                self.assertIsNone(hook(evt))
                call = json.loads(record.read_text())
                self.assertEqual(call["argv"], [subcommand])
                self.assertEqual(json.loads(call["stdin"]), evt._raw)

    def test_invalid_utf8_output_fails_open_for_every_binary_hook(self) -> None:
        cases = (
            (self.agent_plane.agent_start, mock_event(Event.SubagentStart, agent_id="a1")),
            (self.agent_plane.agent_inject, mock_event(Event.PreToolUse, tool="Bash", command="ls")),
            (self.agent_plane.agent_stop, mock_event(Event.SubagentStop, agent_type="worker", agent_id="a1")),
            (self.agent_plane.agent_report, mock_event(Event.PostToolUse, tool="Task", prompt="inspect")),
            (self.session.session_record, mock_event(Event.SessionStart, source="startup")),
        )
        for hook, evt in cases:
            with self.subTest(hook=hook.__name__):
                self.stub(stdout_bytes=b"\xff")
                self.assertIsNone(hook(evt))

    def test_agent_inject_skips_the_binary_without_a_pending_directive(self) -> None:
        self.plane(pending_for="someone-else")
        record = self.stub(stdout="unused")
        evt = mock_event(Event.PreToolUse, tool="Bash", command="ls")
        evt._raw["agent_id"] = "a1"
        self.assertIsNone(self.agent_plane.agent_inject(evt))
        self.assertFalse(record.exists())

    def test_agent_inject_calls_the_binary_for_a_pending_directive(self) -> None:
        self.plane(pending_for="a1")
        record = self.stub(stdout="")
        evt = mock_event(Event.PreToolUse, tool="Bash", command="ls")
        evt._raw["agent_id"] = "a1"
        self.assertIsNone(self.agent_plane.agent_inject(evt))
        self.assertEqual(json.loads(record.read_text())["argv"], ["agent-inject"])

    def test_pending_directive_counts_only_the_cc_slack_scope(self) -> None:
        self.plane(scope="/some/repo", pending_for="a1")
        record = self.stub(stdout="unused")
        evt = mock_event(Event.PreToolUse, tool="Bash", command="ls")
        evt._raw["agent_id"] = "a1"
        self.assertIsNone(self.agent_plane.agent_inject(evt))
        self.assertFalse(record.exists())

    def test_stop_and_report_skip_the_binary_without_a_subject(self) -> None:
        self.plane(scope="/some/repo")
        record = self.stub(stdout='{"decision":"block","reason":"unused"}')
        self.assertIsNone(self.agent_plane.agent_stop(mock_event(Event.SubagentStop, agent_type="w", agent_id="a1")))
        self.assertIsNone(self.agent_plane.agent_report(mock_event(Event.PostToolUse, tool="Task", prompt="x")))
        self.assertFalse(record.exists())

    def test_a_downed_daemon_skips_the_binary(self) -> None:
        (self.home / ".daemonkit" / "a" / "com.yasyf.cc-slack").mkdir(parents=True)
        record = self.stub(stdout="unused")
        self.assertIsNone(self.session.session_record(mock_event(Event.SessionStart, source="startup")))
        self.assertFalse(record.exists())

    def read(self, session: Path, tool: str = TOOLS + "slack_thread", agent_id: str | None = None, **result: object) -> None:
        output = json.dumps({"channel_id": THREAD["channel_id"], "thread_ts": THREAD["thread_ts"], "messages": [], **result})
        self.read_guard.thread_reads_are_recorded(mock_tool_event(tool, event=Event.PostToolUse, tool_input=THREAD, output=output, agent_id=agent_id, session_dir=session))

    def refused(self, session: Path, agent_id: str | None = None) -> bool:
        evt = mock_tool_event(TOOLS + "slack_reply", tool_input=THREAD | {"text": "On it."}, agent_id=agent_id, session_dir=session)
        result = self.read_guard.replies_read_their_thread_first(evt)
        return result is not None and result.action == Action.block

    def test_a_reply_without_a_read_is_refused_and_names_the_read(self) -> None:
        evt = mock_tool_event(TOOLS + "slack_reply", tool_input=THREAD | {"text": "On it."}, session_dir=Path(self.tmp.name) / "session")
        result = self.read_guard.replies_read_their_thread_first(evt)
        self.assertEqual(result.action, Action.block)
        self.assertIn(f"`slack_thread` with `channel_id` {THREAD['channel_id']} and `thread_ts` {THREAD['thread_ts']}", result.message)
        self.assertNotIn("1000", result.message)

    def test_a_reply_after_a_full_read_is_allowed(self) -> None:
        session = Path(self.tmp.name) / "session"
        self.read(session)
        self.assertFalse(self.refused(session))

    def test_a_read_older_than_ten_minutes_is_refused(self) -> None:
        session = Path(self.tmp.name) / "session"
        self.read(session)
        slot = SessionStore(session)[self.read_guard.ThreadReads]
        reads = slot.get()
        reads.reads = {key: read.model_copy(update={"at": datetime.now(UTC) - timedelta(minutes=10, seconds=1)}) for key, read in reads.reads.items()}
        slot.set(reads)
        self.assertTrue(self.refused(session))

    def test_a_truncated_read_is_not_a_full_read(self) -> None:
        session = Path(self.tmp.name) / "truncated"
        self.read(session, truncated=True)
        self.assertTrue(self.refused(session))

    def test_a_read_of_only_what_is_new_counts_unless_it_is_cut(self) -> None:
        since = THREAD | {"since_ts": "1790210300.000001"}
        whole = Path(self.tmp.name) / "whole"
        self.read_guard.thread_reads_are_recorded(mock_tool_event(TOOLS + "slack_thread", event=Event.PostToolUse, tool_input=since, output=json.dumps(since | {"messages": []}), session_dir=whole))
        self.assertFalse(self.refused(whole))
        cut = Path(self.tmp.name) / "cut"
        self.read_guard.thread_reads_are_recorded(mock_tool_event(TOOLS + "slack_thread", event=Event.PostToolUse, tool_input=since, output=json.dumps(since | {"messages": [], "truncated": True}), session_dir=cut))
        self.assertTrue(self.refused(cut))

    def test_a_reply_records_its_ts_for_the_agent_that_posted_it(self) -> None:
        session = Path(self.tmp.name) / "session"
        posted = json.dumps(THREAD | {"ts": "1790210350.000001", "posted": True})
        self.read_guard.own_replies_are_recorded(mock_tool_event(TOOLS + "slack_reply", event=Event.PostToolUse, tool_input=THREAD | {"text": "On it."}, output=posted, agent_id="a1", session_dir=session))
        self.read_guard.own_replies_are_recorded(mock_tool_event(TOOLS + "slack_reply", event=Event.PostToolUse, tool_input=THREAD | {"text": "On it."}, output='{"ticket_id":"t1"}', session_dir=session))
        self.assertEqual(list(SessionStore(session)[self.read_guard.ThreadReads].get().posted), [f"a1/{THREAD['channel_id']}/1790210350.000001"])

    def test_a_truncated_read_keeps_the_whole_read_before_it(self) -> None:
        session = Path(self.tmp.name) / "session"
        self.read(session, messages=[{"ts": "1790210300.000001"}])
        self.read(session, truncated=True, messages=[{"ts": "1790210400.000001"}])
        read = SessionStore(session)[self.read_guard.ThreadReads].get().reads[f"main/{THREAD['thread_ts']}"]
        self.assertEqual((read.through, read.gap), ("1790210300.000001", "truncated"))
        self.assertFalse(self.refused(session))

    def test_a_read_clears_only_the_agent_that_read(self) -> None:
        session = Path(self.tmp.name) / "session"
        self.read(session, agent_id="a1")
        self.assertTrue(self.refused(session))
        self.assertFalse(self.refused(session, agent_id="a1"))

    def test_the_cli_and_the_user_fallback_reads_count(self) -> None:
        cli = Path(self.tmp.name) / "cli"
        command = f"cc-slack thread --channel {THREAD['channel_id']} --thread {THREAD['thread_ts']} --since-ts 1790210300.000001"
        self.read_guard.thread_reads_are_recorded(mock_tool_event(event=Event.PostToolUse, command=command, output=json.dumps({"messages": []}), session_dir=cli))
        self.assertFalse(self.refused(cli))
        fallback = Path(self.tmp.name) / "fallback"
        self.read_guard.thread_reads_are_recorded(mock_tool_event("mcp__slack__slack_get_thread", event=Event.PostToolUse, tool_input=THREAD, output="a formatted thread", session_dir=fallback))
        self.assertFalse(self.refused(fallback))


if __name__ == "__main__":
    unittest.main()
