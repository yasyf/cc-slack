# Send, react, and read

Always prefer cc-slack for posts and reactions so Claude is visibly acting.
Writes join public channels automatically. In a private channel the user
belongs to, an approved write invites the bot as the user and retries;
otherwise, the write is refused with the cause, and cc-slack sends nothing.
The shared bot posts as `<display name> (<agent>)` with the user's avatar
and no message prefix. Pass `agent` on `slack_send`, `slack_reply`, and
`slack_dm_status`, or `--agent` on the CLI verbs, naming the agent writing:
`claude` from Claude Code, `codex` from Codex, `cursor` from Cursor. A post
without it reads `<display name> (agent)`. The user-level Slack Model Context Protocol (MCP) posts
as the user. When the session's project opts in with `[permissions]
enabled = true` in its `.claude/cc-slack.toml`, protected writes on both
surfaces need explicit user permission.

The tool names below have the full prefix
`mcp__plugin_cc-slack_cc-slack__`. Every tool also accepts a permalink `url`.

| Task | Tool and arguments |
|---|---|
| Post an approved message | `slack_send(channel_id, text, thread_ts?, files?, blocks?, broadcast?, no_watch?)` |
| Reply with approved text | `slack_reply(channel_id, thread_ts, text, files?, blocks?, broadcast?, no_watch?)` |
| Revoke a captain-hook ticket | `slack_revoke(ticket_id)` |
| Add a reaction | `slack_react(channel_id, ts, name)` |
| Remove the bot's reaction | `slack_unreact(channel_id, ts, name)` |
| Replace the text of the bot's own post | `slack_edit(channel_id, ts, text, thread_ts?, blocks?)` |
| Delete the bot's own post | `slack_delete(channel_id, ts, thread_ts?)` |
| Delete or rewrite a reviewed batch of the bot's own posts in the user's DM, archiving each first | CLI only: `cc-slack apply --plan <file> [--dry-run]`, below |
| Close a waiting card the user answered somewhere else, such as in a thread reply you relay | `slack_card_answer(url, label, via, waits?, other?, settled?)`; the permalink `url` finds the card, whichever session posted it |
| Read a thread, with each shared message's original author, channel, text, and files under `shares` and each image's local `path`; a rate-limited read returns the cached copy with `stale: true` and `retry_after` | `slack_thread(channel_id, thread_ts, limit?)` |
| Read a channel's latest top-level messages, newest first, with reply counts and permalinks | `slack_history(channel_id, limit?)` |
| Download a file attachment, such as a screenshot, to a local path you can Read (CLI `cc-slack file <permalink or id> [--out <path>]`) | `slack_file(file_id or url, out?)` |
| List unarchived public channels, and private ones the bot is in, by name prefix, with id, name, and created | `slack_channels(prefix?)` |
| Find active users by handle, display name, real name, or email, ignoring case (CLI `cc-slack users <query>`) | `slack_users(query)` |
| Watch a thread, or a channel's top-level messages and thread replies | `slack_watch(channel_id, thread_ts?, owner?)` |
| End a watch | `slack_unwatch(watch_id)` |
| Update the user's own status DM | `slack_dm_status(text, thread_ts?, files?, blocks?)` |
| Show you picked up a message: eyes plus Slack's working line, together | `slack_pickup(channel_id, ts, thread_ts?)`; `done: true` takes both away (CLI `cc-slack pickup`) |
| Once you decide to do actual work, show separate steps with a spinner and keep the working line | `slack_stream(channel_id, thread_ts, recipient?, plan?, tasks)` returns `ts`; then `slack_stream(channel_id, thread_ts, ts, tasks)` per step and `stop: true` to end (CLI `cc-slack stream`, `--task "id\|in_progress\|title"`) |
| Create an approved public channel | `slack_channel_create(name)` |
| Archive an approved channel | `slack_channel_archive(channel_id)` |
| Invite approved people to a channel | `slack_channel_invite(channel_id, users)` |
| Set an approved channel topic | `slack_channel_topic(channel_id, topic)` |
| Add the bot to an approved channel, private ones included | `slack_channel_join(channel_id)` |

`cc-slack apply --plan <file>` cleans up the user's DM in one command. The
plan is a JSON list of entries with `ts`, `thread_ts`, `permalink`, `verdict`
(`delete`, `rewrite`, or `keep`), and `new_text` for a rewrite, all in the
user's DM with the bot. The permission hook passes it with no ticket when
the plan parses and names only those verdicts in one DM. The daemon refuses
any entry that is not the bot's own post in that DM and logs it, and the
run goes on. Before each change it appends the message, as Slack returns
it, to `<plan>.archive.jsonl` beside the plan, with the plan's extension
dropped, and it records each entry done in `<plan>.done.jsonl`, so a rerun
skips those. It waits out Slack's `Retry-After`, paces itself under the
delete and update rate limits, and ends with one line: deleted, rewritten,
skipped, refused, failed. Run `--dry-run` first: it reads and checks every
entry and changes nothing.

Every `slack_send` and `slack_reply` arms a top-level watch on the post's
thread and returns its `watch_id`, so replies to your own post reach this
session like any watched message. A reply from the user arrives as
operator steering, the same as a prompt. Pass `no_watch: true`, or `--no-watch` to the CLI, only for a
one-shot post or when an owner subagent watches the thread itself.

When the config sets `[doc_cards]`, a link under its `link` prefix in
`text` gets a Work Object card. `slack_send`, `slack_reply`, and
`slack_dm_status` ask the doc-cards service for the cards, so a private docs
site that Slack's crawler cannot read still previews, and Slack sends no
`link_shared` event for links an app posts. A link inside `blocks` alone
gets no card, and neither does a Slack MCP fallback post.

With `[github]` in the config, the daemon links every pull request the text
names, so write `#123`, `owner/repo#123`, or the PR URL and never build the
link yourself. Each becomes `<url|#123>`. A bare `#123` resolves against
this session's repo, then `repos` under `[pr_links]`; a number none of them
holds stays plain text. Code spans and text already inside `<...>` are left
alone. A link never draws a card or a button. With `card_command` set, a PR the
user has to tap gets its card only when the call lists its URL in `tap_prs` (the
CLI's repeatable `--tap-pr` on `dm-status`), once per PR and head. List a
stack's PRs in one call: they draw one card, and an older card of any of them
is edited to say it was replaced. When a PR no longer waits on the user's tap,
`slack_withdraw_pr_card(pr, reason)` takes down every open card carrying it, a
stack's included, in place: Withdrawn and the reason replace its buttons, and a
late tap records nothing. A tap on it,
or on a pick card whose `prs` names that one PR at that head with the same
answers, closes every other such card and counts once. A tap on a waiting card
whose `prs` names a PR, with no PR card for it, also goes to the card command,
which records it when the PR is its own. A reply carrying its own
picks lists the PR in the card's `prs` instead of `tap_prs`. In the user's DM the
card opens at the root in its own thread, wherever the post sits; a post lands in
a DM thread only when its call names that thread. In a listed channel the card
goes in the post's thread. When an open PR in `tap_prs` draws no card, the call
fails after the post went out: the error names that post, so never post it again,
and each PR with the reason; fix the reason, such as a PR body missing What you'll
hate most, then send `tap_prs` for that PR alone. Never post a PR card or add a
`refs` entry by hand for a PR your text names. With `preview_command` set, a PR
link in a post to the user's DM also shows a preview under the text saying what
it changes and where it stands, and a line that only repeats a link is dropped,
so name each PR once and never draw its state yourself. With `[buildkite]` set, a build link
there shows its state and message the same way.

`files` attaches files by absolute path, up to 1 GB each; the CLI verbs
`send`, `reply`, and `dm-status` take a repeatable `--file <path>`. When
every file is a PNG, JPEG, GIF, or WebP image, the images appear under the text in
the same `<display name> (<agent>)` message, a moment after it posts, and the
files are shared into its thread so everyone in the conversation can open them.
Any other file, such as a log,
Markdown, CSV, JSON, or PDF, goes out as a Slack file share that carries the
text as its comment, still as `<display name> (<agent>)`, and a broadcast reply
cannot carry one.

`blocks` sends a raw Block Kit array as the message body, for a layout plain
mrkdwn cannot draw; the CLI verbs take `--blocks <json>` or
`--blocks @<absolute path>`. `text` stays required and becomes the
notification and fallback text. Blocks combine with image files, which follow
them, but not with other files.

The CLI verbs `history --channel <id>` and `channels --prefix <p>` print
one line per message or channel, with times in Pacific; `--json` prints the
result the tools return.

A `channel_id` that names a person, `U…` or `W…`, resolves to a group DM
with that person, the user, and the app, so both people see every message.
Read the thread before composing a reply; reads and watches need no write
approval. `slack_watch` joins public channels when needed and returns
`{watch_id, joined, cursor}`; retain the watch id for cleanup. A channel
watch delivers every new top-level message and every new reply in any of
the channel's threads, each read once on Slack's push, so watch a channel
instead of polling `slack_history`. To read or watch a private channel the
bot is not in, add it there with `slack_channel_join` under the user's
approval; never ask the user to `/invite` it.
`slack_send`, `slack_reply`, `slack_edit`, `slack_delete`, `slack_react`,
and `slack_unreact` join a public channel and retry once when the bot is
outside it.

If a private channel excludes the bot, an approved write invites the bot
with the user's own Slack credential, the one the user-level Slack MCP
holds, and retries; no ask. The invite carries out only the write its
ticket covers; reactions never invite.

When that invite fails, the refusal is one line naming the cause, such as an
expired Slack MCP credential, and cc-slack sends nothing. Report that cause
and retry the same write once it is fixed. Never ask the user to
`/invite` the app, and never post the message through the user-level Slack
MCP instead.

`slack_unwatch` returns `{stopped, remaining}`, where `remaining` counts this
session's top-level watches still active. For a top-level watch, omit
`owner`; an owner subagent supplies its own agent id.

A post that links a Slack message shows Slack's own preview of it, so post the
permalink and never draw a preview of a card by hand. A post to your DM that
links a Sentry issue or a Datadog monitor asks Slack to unfurl it too; posts to
channels do not. `thread` and `history` list what Slack unfurled for each message:
Slack messages under `shares`, other links under `unfurls`. Slack's preview has no
working buttons, so when a post needs a tap on an open card beside it, run
`cc-slack card-copy --url <permalink> --thread <ts>`. A tap on that copy answers
the card.

To read a card before it goes out, run `cc-slack card render --card <file>`. It
prints the card's Block Kit as JSON and posts nothing, so no test card is needed.
