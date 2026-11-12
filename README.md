# cc-slack

**Slack for Claude Code sessions: post as you, watch threads, and take steering
from your DMs.** One daemon per machine holds a shared Slack bot token, posts
as `<your display name> (claude)` with your avatar, and delivers every reply on
a watched thread into the session that is waiting for it.

[![Release](https://img.shields.io/github/v/release/yasyf/cc-slack?sort=semver)](https://github.com/yasyf/cc-slack/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/yasyf/cc-slack/ci.yml?branch=main&label=ci)](https://github.com/yasyf/cc-slack/actions/workflows/ci.yml)
[![License: PolyForm-Noncommercial-1.0.0](https://img.shields.io/badge/License-PolyForm--Noncommercial--1.0.0-blue.svg)](https://github.com/yasyf/cc-slack/blob/main/LICENSE)

## Get started

cc-slack runs on macOS. Install the plugin, which downloads the pinned binary
on first use:

```bash
claude plugin marketplace add yasyf/cc-slack
claude plugin install cc-slack@cc-slack --scope user
```

Or install the binary on its own:

```bash
brew install --cask yasyf/tap/cc-slack
```

Point cc-slack at your Slack app's bot token and backend socket, then log in
from the checkout you work in:

```bash
export CC_SLACK_BOT_TOKEN=xoxb-...
export CC_SLACK_SOCKET_URL=wss://slack-backend.example.com/live
cc-slack login
cc-slack whoami
```

`login` resolves your Slack user from `git config user.email` and needs
[captain-hook](https://github.com/yasyf/captain-hook) on `PATH`.

Driving with an agent? Paste this:

```text
Install the cc-slack plugin (claude plugin marketplace add yasyf/cc-slack; claude plugin install cc-slack@cc-slack --scope user), run cc-slack login, and post a status card to my DM with slack_dm_status.
```

---

## Use cases

### Post in a thread as yourself, through the agent

Without cc-slack, an agent either posts as you through a user token, which
reads as if you typed it, or not at all. With it, `slack_reply` posts as
`Ada Lovelace (claude)` with your avatar badged, so readers see who asked for
the post and that an agent wrote it.

### Hear replies without polling

`slack_send` and `slack_reply` arm a watch on the thread they post in. When
someone answers, the reply arrives in the session as a `slack.message`
channel event, read once on the backend's nudge instead of on a timer.

### Steer a session from your phone

`slack_dm_status` posts a status card to your DM with the app, with Ack,
Pause, and up to four quick replies. A tap or a DM reply reaches the session
as steering, the same as a prompt.

### Open a Slack thread in a fresh workspace

The app's "Open in Claude" message shortcut opens an orca
workspace on your laptop with Claude already reading the thread.

## Configure it

`cc-slack login` reads the first of `--config`, `$CC_SLACK_CONFIG`, the
checkout's `.claude/cc-slack.toml`, and `~/.config/cc-slack/config.toml`.
Every section is optional. Each key the checkout's file leaves out comes from
`~/.config/cc-slack/config.toml`, so settings for your machine alone, such as
a card command, live there and survive a `login` from any checkout.

```toml
# Read credentials from AWS Systems Manager instead of the environment,
# signed in as the named AWS profile (the shell's profile when unset).
[ssm]
profile = "slack-reader"
region = "us-west-2"
bot_token = "/slack/bot-token"
socket_url = "/slack/socket-url"
avatar_key_id = "/slack/avatar-key-id"
avatar_secret = "/slack/avatar-secret"
picture_key_id = "/slack/picture-key-id"
picture_secret = "/slack/picture-secret"

# Badge your avatar and host it in an S3-compatible bucket.
[avatars]
endpoint = "https://<account>.r2.cloudflarestorage.com"
bucket = "public-assets"
public_base = "https://assets.example.com/"

[pictures]
endpoint = "https://<account>.r2.cloudflarestorage.com"
bucket = "card-pictures"

# Attach Work Object cards to links Slack cannot preview.
[doc_cards]
link = "https://docs.example.com/"
url = "https://cards.example.com/doc-cards"

# Let a card link use http on your tailnet; every other link stays https.
[links]
tailnet = "example.ts.net"

# Read pull request state through `ccx vcs gh`, and link #N to a PR in one of these repos.
[github]
token_command = ["gh", "auth", "token"]

[pr_links]
repos = ["acme/app", "acme/lib"]
card_channels = ["C0TEAMROOM"]
card_command = ["lr", "board", "card", "--lane", "pr-cards"]
preview_command = ["lr", "board", "rows", "--drive", "my-drive", "--all", "--json"]
```

Your pick on a question card goes to `card_command` before it reaches the session that posted the card,
as `{"answer": {"waits", "label", "other", "undone", "url", "action_ts", "source"}}`, and so does an Undo of
it, with `undone` true, so the program records your answer once and the session is told to post no record of
it. A program that does not know the card exits non-zero, and the session alone keeps the answer. When that
session has ended, the same request is how the program hears your pick and its Undo: the card closes on your
pick with its Undo button, unless the program exits non-zero and no session holds your DM, which leaves the
card open, saying your pick reached no one.

A release post in a channel a session's feed watches for releases also goes to `card_command` as
`{"release": {"name", "line", "link"}}`, once each time its line changes, `line` being what follows
`RELEASE` in the feed. A drive uses it to reopen an incident's stop the turn a release it named fails
a step. Its output is ignored.

A team can check its config in as `.claude/cc-slack.toml`. The daemon keeps
the config it was logged in with, so run `cc-slack reload` after a change.
Reload hands the daemon the new config and keeps the bot token, socket URL,
and avatar key that `cc-slack login` last read, so it needs no AWS access.
Rerun `cc-slack login` when `[ssm]` names a different parameter, or when
`[avatars]` turns on after a `login` that read no avatar key.

## Run a backend

cc-slack reads Slack only when told something changed. A small service beside
your Slack app receives its events and pushes frames to each daemon over a
WebSocket; [PROTOCOL.md](PROTOCOL.md) specifies them, with fixtures under
`cmd/cc-slack/testdata/protocol/`.

Licensed under [PolyForm-Noncommercial-1.0.0](LICENSE).
