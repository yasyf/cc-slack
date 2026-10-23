# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed
- The permission hook asks about a Slack write the user did not request on an ask card in the DM status thread, never through `AskUserQuestion`. Its refusal prints the `slack_dm_status` card: the title names the place, the body carries the draft, and the answers are Yes and No. A tap on Yes approves the write.

### Added
- Each answer button on a `waits` card carries the card's options, and its `slack.action` payload carries them as `options`, so a reader can tell which answer the label is.

## [0.1.9] - 2026-10-23

### Added
- A `slack-grant:<channel>/<thread>` answer label grants writes in that one thread only, and `slack-revoke:<channel>/<thread>` withdraws it; an answer carrying a thread label for a channel no longer covers the rest of that channel.
- Two comms checks in the capt-hook pack run on every send, reply and edit wherever `[permissions]` is on: a post never asks a person for logs, versions, screenshots or recordings, and each line telling a person their problem is fixed cites the record of their case succeeding on the live build, their `usr_` id, and the thread owner.

## [0.1.8] - 2026-10-22

### Added
- The capt-hook pack carries the Slack write permission hook, its thread carry, and the plain-words warning, with scrubbed replay fixtures. A project opts in with `[permissions] enabled = true` in `.claude/cc-slack.toml`; `[[permissions.clis]]` names other posting CLIs the hook checks.

### Changed
- The daemon requires a ticket from sessions whose project sets `[permissions] enabled = true`, in place of the `permission_hook` file check. The `permission_hook` config key is gone.
- The plugin requires captain-hook 12.101.15 or later.

## [0.1.7] - 2026-10-22

### Added
- Ask cards carry a fixed Explain more button. Its tap reaches the session as `slack.action` with that label and the question id, asks for a fuller explanation in the thread, and leaves the question open.

### Changed
- A tap redraws the tapped card before the thread's root card.

## [0.1.6] - 2026-10-22

### Added
- `waits` on status cards: an ask card whose options are its only buttons, which says the asking lane is stopped until the tap, and whose `slack.action` carries the question id, the question, the answer, and a capture line for cc-notes.

## [0.1.5] - 2026-10-22

### Added
- Status cards carry `refs`, GitHub pull request and Buildkite build links whose live CI, queue, and landed state the daemon polls and redraws, plus `progress`, `started_at`, `eta`, `facts`, and a drive `rollup` of lanes and PRs.
- `asks` on status cards: buttons with stable ids, an optional confirm dialog and expiry, whose tap reaches the session as `slack.action` carrying the id.
- The root card keeps the last progress, facts, refs, links, and rollup any card set, lists every open ask, and shows a timeline of the last 5 updates.
- `dm-status --root-only` (`root_only`) redraws the root card without posting a reply.
- Optional `[github]` and `[buildkite]` config sections, each taking `token_env` or `token_command`.

## [0.1.4] - 2026-10-22

### Fixed
- Attachments in folders macOS protects from the daemon, such as `~/Documents`, post again. The CLI and channel server hand the daemon temporary copies, where before the daemon's open blocked until the request timed out.

## [0.1.3] - 2026-10-22

### Fixed
- The Homebrew cask strips the quarantine flag from the binary before its first run.

## [0.1.2] - 2026-10-22

### Fixed
- When a subagent stops while it still owns active watches, the top-level session gets a `slack.owner_stopped` event and a directive listing those watches and how to resume or retire them.
- Each release tag now carries its own binrun descriptor, so installing the plugin at tag N runs cc-slack N.

## [0.1.1] - 2026-10-22

### Fixed
- The tagged plugin now carries its binrun descriptor, so installing the plugin at a release tag provisions the binary.

## [0.1.0] - 2026-10-21

### Added
- The cc-slack daemon, CLI, and channel server, with the Claude Code plugin and its marketplace.
- A TOML config for credentials (SSM or environment), badged avatars, doc-card previews, and the permission hook name.
- PROTOCOL.md, the backend wire contract, with fixtures the daemon's types are tested against.

[Unreleased]: https://github.com/yasyf/cc-slack/compare/v0.1.9...HEAD
[0.1.9]: https://github.com/yasyf/cc-slack/compare/v0.1.8...v0.1.9
[0.1.8]: https://github.com/yasyf/cc-slack/compare/v0.1.7...v0.1.8
[0.1.7]: https://github.com/yasyf/cc-slack/compare/v0.1.6...v0.1.7
[0.1.6]: https://github.com/yasyf/cc-slack/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/yasyf/cc-slack/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/yasyf/cc-slack/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/yasyf/cc-slack/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/yasyf/cc-slack/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/yasyf/cc-slack/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/yasyf/cc-slack/releases/tag/v0.1.0
