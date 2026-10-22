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

    def test_reports_line_and_column(self) -> None:
        [finding] = check_post.check("ok\nsee G158")
        self.assertEqual((finding.line, finding.column), (2, 5))


if __name__ == "__main__":
    unittest.main()
