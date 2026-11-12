from __future__ import annotations

import importlib.util
import sys
import unittest
from importlib.machinery import SourceFileLoader
from pathlib import Path

SCRIPT = Path(__file__).with_name("check-post")
loader = SourceFileLoader("check_post", str(SCRIPT))
check_post = importlib.util.module_from_spec(importlib.util.spec_from_loader(loader.name, loader))
sys.modules[loader.name] = check_post
loader.exec_module(check_post)


def kinds(text: str) -> list[tuple[str, str]]:
    return [(finding.kind, finding.token) for finding in check_post.check(text)]


class CheckPostTest(unittest.TestCase):
    def test_flags_internal_shorthand_and_unlinked_references(self) -> None:
        text = (
            "Rolled restate to 06ca99cdcd per G158 and R699; brw_01abc and run_9x stuck on pod "
            "01J9ZQ4M7X3K8V2N5P6R7S8T9W. See #27801, build 1399, and https://example.com/x."
        )
        self.assertEqual(
            kinds(text),
            [
                ("commit-sha", "06ca99cdcd"),
                ("ruling-id", "G158"),
                ("ruling-id", "R699"),
                ("internal-id", "brw_01abc"),
                ("internal-id", "run_9x"),
                ("ulid", "01J9ZQ4M7X3K8V2N5P6R7S8T9W"),
                ("bare-number", "#27801"),
                ("bare-number", "build 1399"),
                ("bare-url", "https://example.com/x."),
            ],
        )

    def test_accepts_linked_references_and_mentions(self) -> None:
        text = (
            "Thanks <@U0123ABCDE>! Fixed by <https://github.com/acme/app/pull/27801|#27801> at "
            "<https://github.com/acme/app/commit/06ca99cdcd301593953f4d89bfddeadbdb469c81|06ca99c>, "
            "live with <https://buildkite.com/acme/deploy/builds/1399|deploy #1399>."
        )
        self.assertEqual(kinds(text), [])

    def test_reads_the_words_of_each_link_as_the_reader_sees_them(self) -> None:
        text = (
            "<https://github.com/acme/app/pull/298|a card draws a chart fence> and "
            "<https://github.com/acme/app/pull/299|o329 lands on the lane>; pick <https://acme.slack.com/archives/D0123/p1|option 2>."
        )
        self.assertEqual(
            kinds(text),
            [("drive-term", "fence"), ("drive-term", "o329"), ("drive-term", "lane"), ("option-reference", "option 2")],
        )

    def test_flags_a_bare_slack_message_link_which_unfurls_under_words(self) -> None:
        text = "Alert: https://acme.slack.com/archives/C0123ABCDE/p1731123456789012"
        self.assertEqual(kinds(text), [("bare-url", "https://acme.slack.com/archives/C0123ABCDE/p1731123456789012")])

    def test_accepts_a_slack_message_link_under_words_that_name_it(self) -> None:
        text = "Alert: <https://acme.slack.com/archives/C0123ABCDE/p1731123456789012|the disk alert>"
        self.assertEqual(kinds(text), [])

    def test_accepts_a_bare_sentry_issue_link_so_it_unfurls(self) -> None:
        text = "Issue: https://acme.sentry.io/issues/4512345678/"
        self.assertEqual(kinds(text), [])

    def test_accepts_a_bare_sentry_organization_issue_link_so_it_unfurls(self) -> None:
        text = "Issue: https://sentry.io/organizations/acme/issues/4512345678/"
        self.assertEqual(kinds(text), [])

    def test_accepts_bare_datadog_monitor_incident_and_event_links_so_they_unfurl(self) -> None:
        text = (
            "Monitor: https://app.datadoghq.com/monitors/123456\n"
            "Incident: https://app.datadoghq.com/incidents/42\n"
            "Event: https://app.datadoghq.com/event/event?id=7890"
        )
        self.assertEqual(kinds(text), [])

    def test_flags_a_bare_github_link_beside_an_unfurling_one(self) -> None:
        text = "Fix: https://github.com/acme/app/pull/27801 for https://acme.sentry.io/issues/4512345678/"
        self.assertEqual(kinds(text), [("bare-url", "https://github.com/acme/app/pull/27801")])

    def test_flags_a_link_or_mention_inside_another_links_words_as_nested_never_as_a_bare_url(self) -> None:
        release = (
            "<https://buildkite.com/acme/release/builds/14765|The release carrying "
            "<https://github.com/acme/app/pull/54651|the fix that makes the view route return the read's real error>> "
            "is failing on its `runtime-api-next` image build."
        )
        mention = "<https://github.com/acme/app/pull/27801|the fix <@U0123ABCDE> asked for> merged."
        self.assertEqual(kinds(release), [("nested-link", "<https://buildkite.com/acme/release/builds/14765|The release carrying")])
        self.assertEqual(kinds(mention), [("nested-link", "<https://github.com/acme/app/pull/27801|the fix")])

    def test_accepts_the_inner_link_or_mention_put_after_the_links_words(self) -> None:
        release = (
            "<https://buildkite.com/acme/release/builds/14765|Release 14765>, carrying "
            "<https://github.com/acme/app/pull/54651|the fix that makes the view route return the read's real error>, "
            "is failing on its `runtime-api-next` image build."
        )
        self.assertEqual(kinds(release), [])
        self.assertEqual(kinds("<@U0123ABCDE> asked for <https://github.com/acme/app/pull/27801|the fix>, which merged."), [])

    def test_ignores_ordinary_words_and_versions(self) -> None:
        text = "app 0.162.9 deployed 3 services to R2 storage; the facade and 1,000+ pods are fine; L4 balancer."
        self.assertEqual(kinds(text), [])

    def test_flags_drive_codenames_and_zone_labels(self) -> None:
        text = (
            "Heads-up: D landed today (TS release pipeline gone), so the walker and the landing desk "
            "held the lane behind the fence on api at 1:25 PM PT; parity is next in the drive, ctx_9f2."
        )
        self.assertEqual(
            kinds(text),
            [
                ("codename", "D"),
                ("drive-term", "walker"),
                ("drive-term", "desk"),
                ("drive-term", "lane"),
                ("drive-term", "fence"),
                ("zone-label", "PT"),
                ("drive-term", "parity"),
                ("drive-term", "the drive"),
                ("internal-id", "ctx_9f2"),
            ],
        )

    def test_passes_proper_names_for_code_files_paths_and_alerts(self) -> None:
        text = (
            "The hang is in fsync(), called from read_before_reply.py through handleThread, which waits in epoll_wait; "
            "see plugins/long-running/lib/drive/lanes.py, `the drive lane`, and SandSQL's unproven-replica-lane alert."
        )
        self.assertEqual(kinds(text), [])

    def test_passes_a_letter_that_names_an_option(self) -> None:
        self.assertEqual(kinds("Direction B keeps one lock, and Plan C, Phase D, Step E and option F follow it."), [])

    def test_still_flags_a_lane_name_and_order_and_job_ids(self) -> None:
        self.assertEqual(
            kinds("The rt-slack-gates lane took o308 and j789."),
            [("drive-term", "lane"), ("drive-term", "o308"), ("drive-term", "j789")],
        )

    def test_passes_a_multi_agent_term_the_question_it_answers_used(self) -> None:
        answer = "The drive has two PRs left, and each lane waits on CI."
        self.assertEqual(
            [(finding.kind, finding.token) for finding in check_post.check(answer, ["how far along is the drive?", "which lanes are still running"])],
            [],
        )
        self.assertEqual(
            [(finding.kind, finding.token) for finding in check_post.check(answer, ["what's left in this drive?"])],
            [("drive-term", "lane")],
        )
        self.assertEqual(
            [(finding.kind, finding.token) for finding in check_post.check(answer, ["is the deploy out?"])],
            [("drive-term", "The drive"), ("drive-term", "lane")],
        )
        self.assertEqual(
            [(finding.kind, finding.token) for finding in check_post.check(answer, ["is the driver fixed?", "which laneways are closed"])],
            [("drive-term", "The drive"), ("drive-term", "lane")],
        )

    def test_ignores_letters_inside_words_and_names(self) -> None:
        text = "I asked Walker about the U.S. region, C++ builds, Q&A notes, and a T-shirt; S3 and E2E are fine."
        self.assertEqual(kinds(text), [])

    def test_ignores_hex_segments_of_dotted_cdn_references(self) -> None:
        text = "The edge answered 403 with ref18.cd0e2017.1791517253.2e07a421 and a.cd0e2017 too."
        self.assertEqual(kinds(text), [])

    def test_flags_bare_short_sha_even_at_sentence_end(self) -> None:
        self.assertEqual(
            kinds("Rolled back cd0e2017, then cd0e2018."),
            [("commit-sha", "cd0e2017"), ("commit-sha", "cd0e2018")],
        )

    def test_ignores_hex_runs_joined_to_a_longer_identifier(self) -> None:
        text = (
            "Reach it with `orca terminal switch --terminal term_afce9df1-5316-4b5f-8636-26f9a5ea8391`.\n"
            "Session 3f2b8c1e-9a4d-4e6f-b7a2-0c5d8e9f1a2b is open."
        )
        self.assertEqual(kinds(text), [])

    def test_flags_a_bare_full_or_short_sha_in_prose_or_code(self) -> None:
        self.assertEqual(
            kinds("Fixed at 06ca99cdcd301593953f4d89bfddeadbdb469c81, see `git show 06ca99c`."),
            [("commit-sha", "06ca99cdcd301593953f4d89bfddeadbdb469c81"), ("commit-sha", "06ca99c")],
        )

    def test_flags_a_dense_ask_greeting_a_plain_name_with_a_bare_secret_name(self) -> None:
        text = (
            "Hi Anubhav, Yasyf asked me to ask you for one thing on our litestream fork. Could you create a "
            "fine-grained personal access token with access to only the repository Forge-AI/litestream. Then save "
            "it as a repository secret named FORGE_REBASE_TOKEN. It lets the nightly job rebase our patches onto "
            "upstream. Please do not send the token to anyone, me included."
        )
        self.assertEqual(
            kinds(text),
            [
                ("plain-name", "Hi Anubhav"),
                ("bare-identifier", "FORGE_REBASE_TOKEN"),
            ],
        )

    def test_accepts_the_same_ask_mentioned_split_and_formatted(self) -> None:
        text = (
            "hey <@U0123ABCDE>, Yasyf asked me for one thing on our "
            "<https://github.com/Forge-AI/litestream|litestream fork>: a token for its nightly rebase.\n\n"
            "*What to make:* a fine-grained token for `Forge-AI/litestream` only, with:\n"
            "• Contents: read and write\n"
            "• Workflows: read and write\n\n"
            "*Where it goes:* a repository secret named `FORGE_REBASE_TOKEN`. Please don't send it to anyone.\n\n"
            "Could you set that up?"
        )
        self.assertEqual(kinds(text), [])

    def test_reads_no_identifier_number_or_sentence_inside_code(self) -> None:
        text = (
            "Run `export AWS_PROFILE=dev. Then retry. Then wait. Then go.` once.\n```\none. two. three. four. five.\n```\n"
            "The worker died on `RangeError: memory value #830225 out of bounds`, as in `PR 4031`.\n"
            "```\ndeploy #1399 failed: FORGE_REBASE_TOKEN unset\n```"
        )
        self.assertEqual(kinds(text), [])

    def test_still_flags_a_bare_number_beside_code(self) -> None:
        text = "The worker died on `RangeError: memory value out of bounds` after #830225 and build 1399.\n```echo hi```\nSee #4031."
        self.assertEqual(
            kinds(text),
            [("bare-number", "#830225"), ("bare-number", "build 1399"), ("bare-number", "#4031")],
        )

    def test_flags_an_option_named_by_number_or_position(self) -> None:
        text = (
            "Own process plus a memory cap: as 1, and a script over the cap stops.\n\n"
            "Option 2 is the only one; options 1 and 2 add a process.\n\n"
            "Same as 2 but slower, unlike (1). Pick the first option."
        )
        self.assertEqual(
            kinds(text),
            [
                ("option-reference", "as 1"),
                ("option-reference", "Option 2"),
                ("option-reference", "options 1 and 2"),
                ("option-reference", "Same as 2"),
                ("option-reference", "unlike (1)"),
                ("option-reference", "first option"),
            ],
        )

    def test_passes_an_option_number_the_reader_typed_on_its_own(self) -> None:
        ack = "Checking whether option 1 on that card is the new-tree-only one, and the answer comes in this thread."
        for asked in (["sot hat was asnwet 1 right"], ["1 and then if we kepe getting false rejects move to 3"]):
            with self.subTest(asked=asked):
                self.assertEqual(check_post.check(ack, asked), [])
        self.assertEqual(
            [(finding.kind, finding.token) for finding in check_post.check("Options 1 and 3 both hold.", ["1 and then move to 3"])], []
        )

    def test_still_flags_an_option_number_the_reader_never_typed_alone(self) -> None:
        cases = (
            ("Checking whether option 1 on that card is the new-tree-only one.", []),
            ("Checking whether option 2 on that card is the new-tree-only one.", ["sot hat was asnwet 1 right"]),
            ("Checking whether option 1 on that card is the new-tree-only one.", ["is #1 or v1 or 1.5 or 10 or 1% done"]),
            ("Options 1 and 3 both hold.", ["go with 1"]),
            ("Pick the first option.", ["1"]),
        )
        for text, asked in cases:
            with self.subTest(text=text, asked=asked):
                self.assertEqual([finding.kind for finding in check_post.check(text, asked)], ["option-reference"])

    def test_ignores_counts_steps_versions_and_linked_numbers(self) -> None:
        text = (
            "Merged 1 PR in 2 days, counted as 1 PR, like 2 days. This is step 1 of the migration on 0.162.3619, "
            "as 1.5 times or as 1% of runs, like <https://github.com/acme/app/pull/4031|#4031>."
        )
        self.assertEqual(kinds(text), [])

    def test_flags_a_paragraph_that_lists_what_is_not_proven_under_any_label(self) -> None:
        cases = {
            "*Not proven yet*": "Not proven",
            "*Not seen working yet*": "Not seen working",
            "Not proven: that it ships only those three parts": "Not proven",
            "Not proven yet, and said so.": "Not proven",
            "_Not yet proven_ that the build launches": "Not yet proven",
            "• Unproven: the recording over ssh": "Unproven",
            "1. *Unverified:* the unsigned build": "Unverified",
            "- `Not verified` on a test Mac": "Not verified",
            "NOT CONFIRMED: the release": "NOT CONFIRMED",
            "~Untested~ on Linux": "Untested",
        }
        for line, token in cases.items():
            with self.subTest(line=line):
                self.assertEqual(kinds(line), [("unproven", token)])

    def test_accepts_a_reported_result_and_a_quoted_reply(self) -> None:
        for line in (
            "Proven on a real Mac at 7:27 PM.",
            "Ruled out: a stale build on the test Mac.",
            "> Not proven? Then fix it and tell me when it works.",
        ):
            with self.subTest(line=line):
                self.assertEqual(kinds(line), [])

    def test_flags_a_line_that_reports_no_change(self) -> None:
        cases = {
            "No change since the last update.": "No change since the last update",
            "Nothing new.": "Nothing new",
            "• No updates yet": "No updates yet",
            "Nothing changed since 5:25pm.": "Nothing changed since 5:25pm",
            "Same as before.": "Same as before",
            "No news from the build yet.": "No news from the build yet",
        }
        for line, token in cases.items():
            with self.subTest(line=line):
                self.assertEqual(kinds(line), [("nothing-new", token)])

    def test_passes_a_line_that_says_what_changed(self) -> None:
        for line in (
            "The fix merged; the strip shows live incidents only.",
            "No change to the API; the fix only touches the worker.",
            "No changes from this PR affect the API; the worker now retries failed uploads.",
            "Nothing changed since Tuesday, and the worker now retries failed uploads.",
            "No change since the deploy but the worker now retries failed uploads.",
        ):
            with self.subTest(line=line):
                self.assertEqual(kinds(line), [])

    def test_reads_through_emphasis_markers(self) -> None:
        self.assertEqual(kinds("Thanks *Andrew*, it is live."), [("plain-name", "Thanks *Andrew")])

    def test_reports_line_and_column(self) -> None:
        [finding] = check_post.check("ok\nsee G158")
        self.assertEqual((finding.line, finding.column), (2, 5))


if __name__ == "__main__":
    unittest.main()
