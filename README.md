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

`cc-slack login` reads one TOML file, the first of `--config`,
`$CC_SLACK_CONFIG`, the checkout's `.claude/cc-slack.toml`, and
`~/.config/cc-slack/config.toml`. Every section is optional.

```toml
# Read credentials from AWS Systems Manager instead of the environment.
[ssm]
region = "us-west-2"
bot_token = "/slack/bot-token"
socket_url = "/slack/socket-url"
avatar_key_id = "/slack/avatar-key-id"
avatar_secret = "/slack/avatar-secret"

# Badge your avatar and host it in an S3-compatible bucket.
[avatars]
endpoint = "https://<account>.r2.cloudflarestorage.com"
bucket = "public-assets"
public_base = "https://assets.example.com/"

# Attach Work Object cards to links Slack cannot preview.
[doc_cards]
link = "https://docs.example.com/"
url = "https://cards.example.com/doc-cards"
```

A team can check its config in as `.claude/cc-slack.toml`. The daemon keeps
the config it was logged in with, so rerun `cc-slack login` after a change.

## Run a backend

cc-slack reads Slack only when told something changed. A small service beside
your Slack app receives its events and pushes frames to each daemon over a
WebSocket; [PROTOCOL.md](PROTOCOL.md) specifies them, with fixtures under
`cmd/cc-slack/testdata/protocol/`.

Licensed under [PolyForm-Noncommercial-1.0.0](LICENSE).
