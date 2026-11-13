# Status cards and ask cards

## Send status as a card

Send each status update as `slack_dm_status(card)`. Use plain `text` only for
a short conversational answer to the user's DM reply. The CLI takes
`dm-status --card '<json>'` or `--card @<absolute path>`. A card holds:

| Field | Rule |
|---|---|
| `status` | `info`, `progress`, `needs_you`, `incident`, or `done`; sets the emoji; defaults to `needs_you` on a waiting card and `info` otherwise |
| `title` | one line; the card header and the phone notification; defaults to the body's first line |
| `body` | 1-3 sentences of mrkdwn, under the post rules in [voice.md](voice.md) |
| `drive`, `phase` | the long-running drive's name and phase, when there is one; a `waits` card with `drive` is a drive's question and posts at the DM root with no board warning |
| `links` | up to 5 `{label, url}` https buttons: PRs, builds, monitors |
| `options` | up to 4 quick replies of 200 characters or fewer; one over 61 characters or two words draws on its own row with a Pick button; one marked "(Recommended)" becomes the recommended answer |
| `refs` | up to 10 GitHub pull request or Buildkite build urls; the card shows their live CI, queue, and landed state |
| `progress` | `{step, total, label}`, drawn as a progress bar; `label` names what the steps count |
| `started_at`, `eta` | RFC 3339 times; the card shows how long the work has run and when it should finish |
| `asks` | up to 5 `{id, label, description, style, confirm, expires_at}` decisions, each a button: `description` is mrkdwn beside it, `style` is `primary` or `danger`, and `confirm` is the text of the dialog that guards the tap |
| `facts` | up to 10 `{label, value}` pairs, drawn as a grid |
| `hints` | on a card with options, an option's words to read, keyed by the option; each option then lists once beside its own button |
| `prs` | any number of `{url, title, head, changed, review, refresher, surprise, hate_most, plan, show_me, pictures, port, blocks, lands}` pull requests, each drawn with its title, link and live CI, then the lines it has as fields, `changed` first as Changed since your answer, and its Show me on a row of its own; a missing line is left out. The PRs that would take the card past Slack's 50 blocks are named on it, one line each under "In full under Explain more", and drawn whole in the pages Explain more posts. Below the buttons, the first 3 also draw as GitHub Work Objects with Merge and CI tags and Open on GitHub. Its description, read from GitHub, posts in the thread on Explain more. Once every PR reads merged or closed, the card collapses to its title, a line per PR and its outcome, with no buttons |
| `artifacts` | up to 5 `{title, kind, url, image, language, size}` things the reader opens at `url`, such as a diagram on the dashboard, each drawn as a Work Object with its language and size and an Open button when `url` is https, or http on the `[links]` tailnet; any other `url` posts the card with no Work Object and a warning; `image`, an absolute path to a PNG, JPEG, GIF, or WebP, is drawn as a picture on the card, from the `[pictures]` bucket when the config sets one, whether or not its Work Object draws; keep the link in `body` too, for a client that draws no card |
| `replaces` | the Slack permalink of a posted card this one replaces in place: same message, new title and body, no buttons, nothing new posted |
| `supersedes` | with `waits`, up to 5 permalinks of the bot's earlier posts in the user's DM that asked the same question; when the card posts, each is edited to say the question is asked again on this card and link it, and when the card is answered, each shows the question and the answer; refused without `waits`, for a post outside the DM, or for a link that is not a Slack message |
| `rollup` | a drive's `lanes` (`{name, status, detail}`) and `prs` (`{url, label, status}`), on the root card |
| `waits` | a question id; makes the card an ask card the asking lane stops on, as the next section describes |
| `recommended` | the one option you recommend, on an ask card or a reply card; the card shows it first, marked (Recommended). An ask card needs one; on a reply card nothing waits on it |
| `group`, `rows` | a cc-present board's group of pull requests: up to 20 `{id, text, buttons}` rows, or 8 carrying a `pr` shaped as in `prs`, each with up to 4 buttons |
| `key` | an id for a reply card that is neither waiting nor a group: posting a card with the same `key` again edits that card in place |

A card's `body` takes markdown or Slack mrkdwn up to 40,000 characters and renders in Block Kit: bold, links, lists, tables, sections split under Slack's 3,000 limit. Every table draws as its own Slack table block, which Slack also draws in the preview under a link to the card. A table past Slack's 100 rows or 10,000 characters goes on in parts of at most 100 rows and 9,000 characters, each opening with the table's header and each in a message of its own, and a header with no row under it draws as one line; Slack refuses a message whose tables together run past 10,000 characters. A table is a markdown pipe table, or columns laid out in a code block: a line at the block's left edge with three or more columns two spaces apart, and two or more lines under it whose words start in those columns, draw as the table; any other line of the block stays code. A PR card's Show me draws the same way, each table as its own table block. A card shows the first 9,000 characters of its body, ending on a whole sentence, above its PRs. The rest stays behind Explain more, which posts it in the card's thread; nothing is cut.

A `body` line holding only a markdown image, `![what it shows](/absolute/path.png)`, draws that picture as an image block where the line sits, from the `[pictures]` bucket. The source is an absolute path or an https url, wrapped in `<>` when the path has spaces, and the file is a PNG, JPEG, GIF, or WebP; a caption is required. An image inside a sentence or a table cell stays text, and `cc-slack card render` refuses a picture it cannot read. Two to ten pictures in a row, with only blank lines between them, draw as one Slack carousel. The pictures sit side by side, each on a card titled with its caption, so number each caption by the option it shows, as in `![1 · Feed first](...)`. A run past ten starts a second carousel, and a picture alone keeps its full-width image block. A PR's `pictures` and a card's other pictures draw the same way.

A ```` ```chart ```` fence in a `body` or a PR's Show me draws as Slack's own chart, a `data_visualization` block, from a Vega-Lite spec with its data inline in `data.values`. A `bar`, `line`, or `area` mark plots its quantitative field against the field on the other axis, one series per `color` value; an `arc` mark draws a pie of its `theta` field by its `color` field. The chart's title is the spec's `title`, or `<value field> by <category field>`, and each axis is titled with its encoding's `title`, or its field when the encoding names none. Slack holds 2 charts per message, and a post with a third is refused. A chart's title runs up to 50 characters and each label up to 20, with up to 12 series or slices and up to 20 categories, each series giving one value for every category. `cc-slack card render` refuses a chart past those limits, and a fence that is no such spec, naming why.

A card posts as its own message at the DM root, or under `thread_ts` when a directive names a thread. A drive's question card, a `waits` card that sets `drive`, always posts at the DM root, where the user expects an answer to be asked: with `thread_ts` it posts there and links it in that thread under its question, and Slack draws the card under that link. A `thread_ts` that is no thread in your DM, such as a thread in another channel, gets no link and a `NO CARD LINK` warning, since Slack posts a reply under a thread it cannot find at the DM root, where the link would draw the card a second time; any other `slack_dm_status` call naming such a thread is refused and sends nothing. `root_only`
(`dm-status --root-only`) keeps one live root card per session, for a script
that keeps a drive's rollup current: the first call posts it, and each later
call redraws it, keeping the last progress, times, facts, refs, links, and
rollup any call set, every open ask, and a timeline of the last updates. The
daemon tags each card with the session's short id, repo, branch, and
worktree.

An ask's `id` is stable: reusing it updates the same ask, and an answered
ask stays answered, so a script can resend its asks every pass. `confirm`
guards the tap with a dialog, and a tap after `expires_at` is dropped. The
tap arrives as a `slack.action` whose payload carries the ask's `id`: act on
its `label` as operator steering, since a later card may have superseded it.
A tap whose payload sets `poster_ended` reaches you because you hold the DM and
the session that posted the card has ended: route it to the lane it is for. An
Undo of that tap reaches you the same way, as the `undo: true` payload below, and
so does an Explain more tap, which the card answers with "An explanation follows
in this thread" once you hold it.

A card with a `group` id is one group of rows on a cc-present board.
cc-present posts it; you never write one by hand. Posting the same `group`
again edits its card in place, unless the card gains a row its earlier card
did not carry: then it posts as a new message at the bottom of the thread,
so the user is notified, and the earlier card is deleted. A group with no
rows is posted only to edit an earlier card. A tap on a row's button goes to the board as that
row's verdict through `cc-present slack-sync decided`, not to the session,
and the row shows its answer. When the board refuses the tap, the row says
why, and the next push asks it again.

Each reply card carries the `options` plus fixed Ack and Pause buttons. The
card lists every option in full, numbered, above its buttons. When any option
runs past two words, each button shows only its number. A bare number the user
replies in the card's thread counts as a tap on that button. A
tap arrives once, as a `slack.action` tag or a directive, steering this
session the way a DM reply does. Nothing waits on these taps. A tap on
an older card may be stale: its `ts` is when that card was posted. Ack means the user saw the card and needs nothing. Pause means start
no new lanes, applies, deploys, or PRs. Let in-flight work settle, then post
a `needs_you` card naming what is paused and what resuming would start.

A question you keep working past, such as a design choice you go ahead on
with your recommended answer, is a reply card with `options` and
`recommended` and no `waits`. The card shows the recommended option first,
marked (Recommended), keeps its Ack and Pause, and says nothing about a
stopped lane. A tap on another option arrives as a `slack.action` like any
reply card's, so change course when it comes. When the user answers it in
words instead, close it with `slack_card_answer` on its permalink, which
shows their answer in place of its buttons.

## Ask the user with an ask card

An ask card is how a lane asks the user a question during execution. With
cc-present installed, every question starts on the cc-present board: put
each question card there with `cc-present update-block` or `push`, and
cc-present posts it here as an ask card by itself, with the card's images.
`cc-present ask <card>` does the same for a single card. A `waits` card posted with
`slack_dm_status` or `dm-status` is still posted, but its reply carries a
`warning` saying so. A long-running drive asks in Slack by design: a `waits`
card that sets `drive` is a drive's question, posts with no warning, and
still lands on the board. The rest of this section describes the Slack card those
calls post, and how to post one directly when cc-present is not installed. Set
`waits` to a question id that leads with the asking lane's name, such as
`step-3:merge`. Write the question verbatim as the `title`, the context
in `body`, the answers as `options`, and the answer you recommend as
`recommended`. The card shows the recommended answer first, marked
(Recommended), then the other answers, numbered in that order, then fixed Other and Explain more
buttons, with no Ack or Pause. It tells the user the lane is stopped until
they answer, except a card carrying `prs`, which says only what its buttons
do, and it takes no `asks`.

Every ask card has a recommendation and an Other. A card without
`recommended` is refused. Never write (Recommended) into a label or add
an Other option: the card draws both.

The card draws the recommended option first and numbers the rest after
it, so the numbers you had in mind are not the ones the user sees. Write
each option in full words that stand alone. A card with options is
refused when its title, body, an option, or a hint points at an option by
number or position, such as "same as 1", "options 1 and 2", "like #2", or
"the first option", and the refusal quotes the words.

Other is the user's own answer. A tap on Other opens a Slack modal with the
question and a text box, and what they type there answers the card. A copy of
the card posted before `card-copy` was removed carries Other too, and an answer
written from it answers the card.

Any other reply the user writes in a card's thread, a waiting question card
included, is their message: the daemon puts eyes and the working line on it
and delivers it as a `slack.message` whose `thread_card` names the card's
`waits` id, and the card keeps its buttons. Only their tap or your
`slack_card_answer` answers the card. When their words pick an option or
answer the question, call `slack_card_answer` with them; when they ask
something, answer it in that thread and leave the card waiting. A card
closed on their words that answered nothing waits again after
`cc-slack card-reopen --card <its permalink> --reply <their reply's permalink>`,
which refuses unless the card shows that reply's words as its answer.
Without `--reply` it takes back a button answer while the card shows Undo,
as a tap on Undo does; a drive dashboard's Undo calls it.

A card that reuses the `waits` id of a card still waiting edits that one card
in place, its buttons kept, and posts nothing, whether or not its words change.
When its words change, each older copy of it loses its buttons and reads one
line, Redrawn in place, linking the card, so only the card asks. When they stay
the same, each copy that still has buttons is drawn again from the card, Other
included. A drive's question finds the card its `waits` id holds anywhere in
your DM, whichever session posted it, so a redraw from a later handler edits
that card the same way. When two cards wait under one drive `waits` id, the
redraw keeps the newest and each older one reads Asked again with new words,
linking it.
`text` that repeats the
card's `body` is dropped, so the body reads once. A redraw is always this
`slack_dm_status` call with the card: `slack_edit` with only text refuses a
card, since that edit would replace its layout and buttons with plain text.
Reusing the id of a card the user answered is refused; ask again under a new id. A card cc-present mirrors (`--mirrored`) that changes only the words of a card the user answered, such as its title, body, refs, hints or PR text, redraws that card in place: the user's answer stays and no button comes back. A mirrored card whose `replaces` names a post cc-slack no longer remembers is refused when that post is a card with no answer left to tap. A withdrawn card, such as one whose PR closed, asked again under its id comes back in place: the same card and thread, its buttons back and the withdrawn line gone. A withdrawn PR tap card comes back the same way when the card command draws it again under its waits id, through a post naming its PR in `tap_prs`, `redraw-pr-cards --pr` or a new head of its PR; drawn under its waits id with a PR `changed` line it did not carry, it asks on a fresh card instead and the withdrawn one says `Replaced:` with that line; drawn with no waits id or another, it stays withdrawn. A mirrored resend of a withdrawn card that changes only its words keeps it withdrawn, with the new words and no buttons.

The lane that asks stops on the card. It starts no dependent work, takes
no default, and never proceeds on a provisional pick. The answer reaches
the top-level session as a `slack.action` whose payload carries the `id`,
`waits: true`, the `question`, the card's `options`, the chosen `label`, and
`other: true` for a written-in answer, and the `capture` line, plus
`notes_label` for a permission card and `via` for an answer given on a board.
When the `card_command` took a tapped answer before the session got it, the
payload carries `recorded: true` and no `capture` or `notes_label`: that
program's record already reaches the lane that asked, so the session posts no
record or capture of it. Otherwise the top-level session
hands the answer to the lane the id names, by `SendMessage`, and records the
`capture` line in cc-notes as the user's answer, with the `notes_label` label
when the payload has one; `slack-grant:<channel>/threads` grants replies in every thread of a channel. `slack-grant:<channel>/solved` passes the first post in each thread of that channel, and `slack-grant:solved` the first post in each thread of every channel, that is plain text with no question, ask or jargon, with no card, and leaves every later post in that thread to the other rules, so each fixed bug gets one solved line. Jev judges every other post in such a thread. It reads the grant's own limit of one plain post without asks or jargon, and the user's other words for that thread, such as a question they asked in it or an order to answer someone there. A bare `slack-grant:<channel>` answer, or any live answer naming the channel, passes nothing alone: the judge reads its words for that post, and a thread reply needs them to cover threads or replies. A Claude lane parks on its mailbox
wait until that message arrives. Only the first answer counts; a later
tap is dropped.

For 10 minutes a tapped answer shows an Undo button on the card and its
copies, except on a permission card or a card that carries a PR. Undo
takes the answer back: the card shows its options again with no answer
on record, and the session gets one `slack.action` with `undo: true`, the
`id`, the `question` and the undone `label`, and no `options`, so nothing
reads it as an answer. Treat that answer as never given: tell the
lane to stop or redirect what it started on it, supersede anything you
recorded from it, such as its cc-notes answer, and wait for the next
answer, which arrives as an ordinary tap. With `recorded: true` the
`card_command` already posted the undo, so post no record of it. An Undo tapped after the 10
minutes is refused on the card and the answer stands.

An Explain more tap arrives as a `slack.action` with the label `Explain
more` and the question id. It answers nothing, and the lane stays
stopped. For a card on a cc-present board, reply with
`cc-present reply --block <question id> --body <answer>`, which answers on
the board and under the card. Otherwise reply with `slack_dm_status` text
and `about` set to the question id. Either way the reply lands in the card's
own thread, so it stays beside the question, and calls the card this card with
no link, since a post in a thread that links the message the thread is under
draws that message again under the post; cc-slack writes such a link as its
words. Pass `about` on any other reply
about a waiting card too. A drive's question is found by its `waits` id anywhere
in your DM, open or answered, whichever session asked it. Give the question in plain words and
what each answer does, and keep waiting for the answer.

A card about a pull request lists it in `refs`. The card links each one as
`<url|repo PR N>` with its live state.

### On a cc-present board too

With `cc-present` on PATH, each board write posts its simple question cards
here: one single-select choice of up to 4 answers, an approval, or an input,
images included. A card already posted in the thread is edited in place when
it changes. `cc-present send` posts any the board still holds unsent. An ask
card posted here directly also lands on this session's
cc-present board as a card with the same question, answers, and
recommendation.
The user answers on whichever surface is in front of them. Whichever
answer lands first wins. The other surface shows it, and the session gets
one `slack.action` capture either way. A board answer's capture names the
board, and its `via` carries the board's URL. `cc-slack card-answer` is
the verb cc-present runs for a board answer; a session never needs it.
