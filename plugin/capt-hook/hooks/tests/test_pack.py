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
from pathlib import Path

from captain_hook import Action, Event, app
from captain_hook.loader import discover_pack
from captain_hook.packs.manager import pack_module_name
from captain_hook.testing.helpers import mock_event

HOOKS = Path(__file__).resolve().parents[1]
PLUGIN_ROOT = Path(__file__).resolve().parents[3]


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
        self.assertEqual(len(app._state.hooks), 10)
        for module in ("agent_plane", "common", "slack_comms_need_proof", "session", "permission", "plain_words", "writes"):
            self.assertIn(f"{self.package}.{module}", sys.modules)

        expected = {
            "agent_start": (Event.SubagentStart, False, False),
            "agent_inject": (Event.PreToolUse, False, True),
            "agent_stop": (Event.SubagentStop, False, False),
            "agent_report": (Event.PostToolUse, True, True),
            "session_record": (Event.SessionStart, True, True),
            "slack_writes_need_permission": (Event.PreToolUse, False, True),
            "slack_posts_carry_their_thread": (Event.PostToolUse, False, True),
            "slack_posts_use_plain_words": (Event.PreToolUse, False, True),
            "comms_never_asks_for_evidence": (Event.PreToolUse, False, True),
            "fixed_lines_name_the_proven_user": (Event.PreToolUse, False, True),
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


if __name__ == "__main__":
    unittest.main()
