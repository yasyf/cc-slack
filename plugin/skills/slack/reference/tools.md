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
| Replace the text of the bot's own post; a card is redrawn through `slack_dm_status` with its card, since an edit with no `blocks` would leave it plain text and is refused | `slack_edit(channel_id, ts, text, thread_ts?, blocks?)` |
| Delete the bot's own post | `slack_delete(channel_id, ts, thread_ts?)` |
| Delete or rewrite a reviewed batch of the bot's own posts in the user's DM, archiving each first | CLI only: `cc-slack apply --plan <file> [--dry-run]`, below |
| Close a waiting card, or a reply card's options, the user answered somewhere else, such as in a thread reply you relay | `slack_card_answer(url, label, via, waits?, other?, settled?)`; the permalink `url` finds the card, whichever session posted it. A reply card's options wait on nothing, so any answer settles it in place and no session is told. `label` is one of its options, or with `other` the user's own words, and `via` is the permalink of where they answered, which the card links. The card collapses to its title and the answer, the asking session gets the answer's `slack.action` as from a tap, the board card resolves, and every post the card supersedes shows the outcome. When the asking session has ended, the card still shows the answer and no session is told. `settled` closes a question that what happened answered, such as the pull request landing, with no `slack.action`. A card answered before, by a tap, its Other field or another surface, keeps that answer: the call returns `answered: false`, `already_answered: true` and that answer as `label`, and the card already shows it, so it needs no second close. Needs no ticket |
| Read a thread, with each shared message's original author, channel, text, and files under `shares`, each image's local `path`, and each button's label under `buttons`; a rate-limited read fails, saying how many seconds until Slack answers again | `slack_thread(channel_id?, thread_ts, since_ts?, limit?, url?, raw?)`; omit `channel_id` for a thread in your DM; it returns a line per message with its text whole, and `raw` with a message's permalink as `url` returns JSON with that message as Slack sends it, with its blocks and metadata, under `raw` |
| Read a channel's latest top-level messages, newest first, with reply counts, permalinks, and each button's label under `buttons` | `slack_history(channel_id, limit?, url?, raw?)`; a line per message with its permalink and first line of text; `raw` as for `slack_thread` |
| Download a file attachment, such as a screenshot, to a local path you can Read (CLI `cc-slack file <permalink or id> [--out <path>]`) | `slack_file(file_id or url, out?)`; without `out` it writes to the daemon's files directory |
| List unarchived public channels, and private ones the bot is in, by name prefix, with id, name, and created | `slack_channels(prefix?)` |
| Find active users by handle, display name, real name, or email, ignoring case, or the one a user id or exact email names (CLI `cc-slack users <query>`) | `slack_users(query)`; a user id such as `U0123ABCDE` goes to `users.info` and an exact email to `users.lookupByEmail`, so both still resolve when `users.list` fails |
| Watch a thread, or a channel's top-level messages and thread replies | `slack_watch(channel_id, thread_ts?, owner?)` |
| Hear a channel's alerts, releases or posts as one line per event, not each message | CLI only: `cc-slack subscribe --channel <id> --kind alerts,releases,posts [--match <text>] [--owner <agent id>]`; `slack_unwatch` ends it |
| End a watch, or stop hearing one thread, such as a DM thread | `slack_unwatch(watch_id)`, or `slack_unwatch(url)` |
| Update the user's own status DM | `slack_dm_status(text, thread_ts?, files?, blocks?)` |
| Show you picked up a message: eyes plus Slack's working line, together | `slack_pickup(channel_id, ts, thread_ts?)`; `done: true` takes both away (CLI `cc-slack pickup`) |
| Once you decide to do actual work, show separate steps with a spinner and keep the working line | `slack_stream(channel_id, thread_ts, recipient?, plan?, tasks)` returns `ts`; then `slack_stream(channel_id, thread_ts, ts, tasks)` with only the steps that changed, and `stop: true` to end the steps and clear the working line. Each task is `{id, status, title, details?}`: a stable id, `pending`, `in_progress`, `complete`, or `error`, what the step does, and a line under it (CLI `cc-slack stream`, `--task "id\|in_progress\|title"`) |
| Create an approved public channel | `slack_channel_create(name)`; the name is lowercase letters, digits, hyphens, and underscores, at most 80 characters, with no leading `#` |
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
The result reports `posted` and `watched` separately. A repeat of a write
already posted under its once ticket, the same text to the same channel and
thread, returns that receipt with `replayed` set and posts nothing; the same
text to another thread posts.

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
holds stays plain text. When GitHub refuses the read, a reference naming one PR
still links, and a bare number two repos could hold stays plain. Code spans and text already inside `<...>` are left
alone. With `card_command` set, a PR the user has to tap gets its card when the
call lists its URL in `tap_prs` (the CLI's repeatable `--tap-pr` on `dm-status`),
once per PR and head; a PR GitHub does not read still goes to the card command,
which draws its card from what it holds. In the user's DM, a post that says something waits on the
user also draws the card of each PR it links by URL, as if `tap_prs` listed it,
so name a PR the user has nothing to tap in a post that waits on nothing. List a
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
fails and sends nothing, naming each PR with the reason; fix the reason, such as
a login whose config sets no `card_command` or a channel that draws no PR cards,
then send `tap_prs` for that PR alone. Never post a PR card or add a
`refs` entry by hand for a PR your text names. With `preview_command` set, a PR
link in a post to the user's DM also shows a preview under the text saying what
it changes and where it stands, and a line that only repeats a link is dropped,
so name each PR once and never draw its state yourself. With `[buildkite]` set, a build link
there shows its state and message the same way.

A `text` with no `blocks` that holds markdown, such as a `|` table, `**bold**` or
a `[words](url)` link, or runs past 3,000 characters, is drawn in `send`,
`reply` and `edit` the way `dm-status` draws it: each table becomes a Slack
table block, and what one message cannot hold goes on as replies in its
thread, or, for an edit, is refused. The drive's `say` syntax, such as
`{pr:123|words}` or `{card:<id>}`, is refused in every cc-slack text, since
only `say` turns it into a link: post through `say`, or write `<url|words>`.

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
instead of polling `slack_history`. `slack_thread` and `slack_history` read
a conversation the bot is not in, such as the user's DM with someone else, as
the user. The first such read posts a consent card to the user's DM with the
app and is refused with its link: ask the user to tap Allow on the Mac
running cc-slack, then retry the read. To watch a private channel the bot is
not in, add it there with `slack_channel_join` under the user's approval;
never ask the user to `/invite` it.
`slack_send`, `slack_reply`, `slack_edit`, `slack_delete`, `slack_react`,
and `slack_unreact` join a public channel and retry once when the bot is
outside it.

If a private channel excludes the bot, an approved write invites the bot
with the user's own Slack token, the one the consent card grants, and
retries; no ask. The invite carries out only the write its
ticket covers; reactions never invite.

When that invite fails, the refusal is one line naming the cause, such as
the consent card to tap, and cc-slack sends nothing. Report that cause
and retry the same write once it is fixed. Never ask the user to
`/invite` the app, and never post the message through the user-level Slack
MCP instead.

`slack_unwatch` returns `{stopped, remaining}`, where `remaining` counts this
session's top-level watches still active. For a top-level watch, omit
`owner`; an owner subagent supplies its own agent id.

A post or edit draws a Slack message it links only when that message is an open
card the post's thread has not shown yet, so refer to a card by its permalink,
from its line in `cc-slack drive-cards --drive <drive>`, under words that name it, as
`<permalink|the lock card>`: the line reads as words, and Slack draws the card
under it the first time the thread links it. A later link to it in that thread,
and every link to any other Slack message, your own messages included, stays
plain words. A post linking both a card that draws and a link that stays plain
goes out plain, and the card follows as its own reply in the post's thread. A
bare permalink shows the raw URL above the card, and Slack never previews a link
whose label is its own URL; either is refused, as is a copy of its blocks or its
title repeated without the link. Slack skips the preview of every link after a
message's fifth, so such a link shows only its words. A post to your DM with no
`blocks` that links more than five cards that draw, Sentry issues and Datadog
monitors goes out as pages, each cut at the last blank line before
the line holding its sixth link, or at that line's start, so every card draws;
the first page goes where the post goes, each later page goes in the first page's
thread, and the result's `page_permalink` names each page after the first. A link in a table row counts toward none of the five,
since Slack previews no link in a table cell, so a table of cards stays one
message. Put each card on its own line or in a table row: a line linking six or more
is refused, and so is an edit in your DM linking six or more, since an edit
cannot add a page. A post to
your DM that links a Sentry issue or a Datadog monitor asks Slack to unfurl it
too; posts to channels do not. `thread`
and `history` list what Slack unfurled for each message: Slack messages under
`shares`, other links under `unfurls`. A message's `text` holds its own words
alone, so a post copied from it carries none of Slack's previews. A post with `tap_prs` and a drive's
question card put the card's link in the post's thread themselves, under the
card's question.

To read a card before it goes out, run `cc-slack card render --card <file>`. It
prints the card's Block Kit as JSON and posts nothing, so no test card is needed.
