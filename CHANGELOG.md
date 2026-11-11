# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- A card redrawn under its `waits` id edits that one card in place and posts nothing new. Each older copy of it loses its buttons and reads Redrawn in place with a link to the card, and no later redraw brings them back. Before, every redraw also posted a fresh copy, which since drive question cards moved to the DM root was a new top-level message in the DM, while the older copies kept working buttons: on Nov 10 between 8:17 and 8:33 PM one card had four live copies at the DM root.
- `slack_edit` with only text refuses a card and names the `slack_dm_status` redraw. Such an edit cleared the card's blocks, leaving its words as plain text with no layout or buttons.

### Added

- A card's `body` line that holds only a markdown image, `![what it shows](/absolute/path.png)` or one drawn from an https url, shows that picture on the card where the line sits. The daemon uploads it to the `[pictures]` bucket as it does an artifact's picture, and a card redrawn in place keeps it without a second upload. `cc-slack card render` reads each such picture and refuses one it cannot read, one that is no PNG, JPEG, GIF or WebP image, one with no caption, and one named by a relative path. An image inside a sentence, a table cell or a code block stays text. Before, a body had no way to show a picture among its lines: on Nov 10 at 6:09 PM a question card about two dashboard layouts showed its screenshots as links only.

### Fixed

- `card-copy` returns `posted: true` and the thread it posted in. Before, every copy it posted read `posted: false` with an empty `thread_ts`, so on Nov 10 at 4:46 PM and 4:51 PM a handler reported two copies of PR cards as failed when both were up. Both were also second copies, since the post with `tap_prs` had put the card's copy in that thread seconds earlier. `card-copy` into a thread that already holds the card's copy now posts nothing and fails naming that copy and saying which posts draw one themselves.
- A post with `tap_prs` asks the card command before it posts, and when a PR it names gets no card it fails with the command's reason and sends nothing. Before, its text line went out first, and a handler read that line as the card. A post that draws a card now names it in `card_ts` and `card_permalink`, and `card-copy` aimed at the post's text line fails naming the card to copy instead.
- A post with `tap_prs` asks the card command again when the PR's newest card has no button to tap, and redraws that card and its copies in place with the buttons it now gets. Before, a card drawn while the PR was not yet marked for his tap stood for it, so later posts only copied that buttonless card and `drive-cards` never listed it: on Nov 10 two posts with `tap_prs` for one PR, at 1:03 PM and 1:39 PM, drew no Approve.
- The permission hook now counts an order the user sent the session in Slack. Each channel line cc-slack marks as the operator's goes to the judge as the user's words, however long ago it arrived, in the session that got it and in any handler it starts. A handler in another session links the message in its brief, and the hook reads it back from Slack and checks the user wrote it in their own DM with the app. An order covers one write, and a Slack link inside it is the only place that write may go. The steering line for an operator message now says that a post it asks for is approved. Before, every channel line was dropped from the user's words: on Nov 10 at 1:37 PM and again at 2:17 PM the hook refused an update the user had asked for in so many words, from the handler that drafted it and from the session holding the DM.
- `slack_card_answer` on a card that was already answered now returns `already_answered: true` beside `answered: false`, with the answer the card shows as `label`. Before, a handler read `answered: false` as "the card is still open": on Nov 10 at 1:00 PM it reported that a card the user had answered in its Other field at 12:45 PM still waited on him.
- A session with a working channel now gets each linked Slack message and card tap once. The tool-use hook had been delivering it again almost every time, because capt-hook runs that hook outside the Claude window and the daemon could not find the window's channel. Channel proof also survives a daemon restart now, so a session that never calls a cc-slack tool after a release keeps single delivery (cc-interact 0.41.1).

### Removed

- The picture-keeping thread in the DM is gone. A card's pictures go only to the `[pictures]` bucket, and a WebP picture is stored as a PNG, since Slack draws only PNG, JPEG and GIF from a link. A card drawn before the bucket keeps drawing the pictures it already shared.

### Added

- A tapped answer on a waiting card shows an Undo button for 10 minutes, on the card and its copies. Undo takes the answer back: the card shows its options again, the answered reaction comes off, and the session gets one `slack.action` with `undo: true`, the question and the undone `label`, to treat as never given. It carries no `options`, so the permission hook never reads it as an answer. An Undo after the 10 minutes is refused on the card and the answer stands. A permission card and a card carrying a PR get no Undo. On Nov 10 at 1:32 AM a stray tap picked the third option on a design card and nothing could take it back.
- `cc-slack dm-relay --text <words> [--under <permalink>]` carries words typed on a drive dashboard into your DM. It posts them as the bot, marked "You, on the dashboard:", under the card or message named by `--under`, or as a new message, and the session watching your DM gets them as your own DM, with the eyes and the owed reply a typed DM gets. Before, a reply, a "Tell Claude what to change" or a new ask typed on the dashboard had no way to reach the drive.
- `cc-slack canvas --title <t> --canvas <id>` replaces a canvas Slack calls full. Slack caps how much one canvas can be rewritten in total and then answers every edit with `canvas_too_large`; on Nov 9 a drive's status tab hit that cap after about 14 hours of redraws and stopped updating. The daemon now deletes that canvas and draws the page on a fresh tab of the same title in your DM, but only when the canvas carries that title and the app made it; any other canvas is refused and kept.
- `slack_withdraw_pr_card` and `cc-slack withdraw-pr-card --pr <url> --reason <text>` withdraw a PR tap card in place: every open card carrying that PR, one PR's or a stack's, is edited to its title, "Withdrawn:" and the reason, then its PR lines, with no Approve or Hold. Its answer on record is the withdrawal, so a late tap records nothing and reaches no session or card command, `drive-cards` no longer lists it, and no redraw brings its buttons back; a later post naming the PR in `tap_prs` draws a fresh card. `card-answer --withdrawn` on a PR card's permalink now does the same. Before, nothing could take down a PR card: on Nov 9 `card-answer` refused all three withdrawals with "no waiting card", so four cards the drive had called off kept live buttons in your DM.
- `slack_thread` and `cc-slack thread` take `since_ts` (`--since-ts`) and return only the messages newer than it. The read-before-reply hook now asks for a read through the thread's newest message with `since_ts` set to the newest ts already read, instead of the whole thread with `limit` 1000; DM threads had grown to 395 messages, each re-read in full before a reply.
- While a session that holds your DM is compacting, each message you send it gets "Got it, back in a minute." in its thread at once, next to the eyes, instead of nothing for the minutes the compaction takes. The plugin's new `PreCompact` hook marks the session and its next start clears the mark; the message reaches the session with `acked` set, so it posts no second ack. On Nov 9 four messages waited about 3.5 minutes unacked while the session holding the DM compacted. The daemon now logs how long after a message was sent its eyes went on.
- A line in your DM with the app that says something waits on you draws the card it means, with its buttons, right under it in the same thread. Link the open card by its permalink, and `send`, `reply` and `dm-status` text copy that card in; a tap on either copy answers both. A line that says something waits on you with no open card linked, no `tap_prs`, no PR link and no open card in its thread is refused before it posts, naming the sentence. `check-post` flags the same sentence when the post links no DM card and no PR.
- A `[pictures]` section names a private S3-compatible bucket for the pictures on cards. With it, a card's pictures upload to the bucket and draw from links signed for 7 days, re-signed on every redraw, so the card is the only message in the DM: no picture-keeping thread and no share. `login` reads the bucket's key from `[ssm] picture_key_id` and `picture_secret`. Without the section, a card that carries a picture is refused before it posts.

### Changed

- The `slack` skill now says the user's order to post, typed in the session, sent in their DM with the app, or tapped on a card, is the approval for that one post. A session never draws a card asking them to approve it or tells them the hook needs their Yes. A refusal of an ordered post is reported as a hook defect, and the user hears in one line that the post goes out once the fix is installed. An open card asking them to approve it is closed with their order's words. Before, the skill said a DM message could steer work but never approve a write, so on Nov 10 a session drew a Yes or No card for an update the user had ordered in their DM 4 minutes earlier.
- On a channel watch, every message an app posts, Datadog, Sentry, Platy or any other, reaches the session as its first non-empty line, cut to 160 characters, and a mark that the whole message is at its permalink. A person's message still arrives whole, and `slack_thread` still returns every message whole. On Nov 9 and 10, 60 bot messages on one session's channel watch averaged 2,687 characters each.
- A bot's thread reply that repeats its thread's parent, such as a Datadog alert posted under its own summary, reaches a session as its first line, a mark saying it repeats the parent, and its permalink, instead of the whole body again. It applies only when the parent already reached that session and every word of the reply is in it; a person's message always arrives whole. On Nov 10 one flapping monitor put a 4,300-character body into a session twice per alert.
- The `slack` skill is now an 8 KB core plus nine files under `plugin/skills/slack/reference/` that a session reads when it needs them: permission and refusals, tools, receiving, the DM, cards, the shortcut, reactions and conversation, voice, and setup. The core keeps the permission rule, posting, DM steering, which events arrive once and which twice, reactions, post rules, and an index with a trigger for each reference. Before, the skill was one 60 KB file, so a session that reloaded it after a compaction got only its first 19.5 KB.
- Every session that loads cc-slack carries less standing context: the MCP tool list drops from 31,810 to 14,036 bytes and the server instructions from 4,045 to 1,503. The `ticket_*` properties the permission hook writes into a call are no longer advertised in any tool's schema, since an unadvertised property still reaches the daemon, and each tool and property description is cut to what a call needs; card authoring lives in the slack skill's `reference/cards.md`. The Monitor arming steps, `channel-ack` and the dedup rule now come in a tool result whose `channel` is `pending` or `inactive`, the one moment a session acts on them. A test fails when either grows past its ceiling.
- A `slack.action` tap no longer carries the card's `capture`. The capture line still arrives in the tap's directive. A tap's payload had averaged 699 bytes, 256 of them the capture.

### Fixed

- A tapped answer no longer makes the daemon redraw every live card at that moment. The Undo it shows still goes when its ten minutes end: the tap now only moves up when the card loop next wakes. That extra redraw could land between a failed edit of the tapped card and its retry, so the card's own retry and its warning in the thread were lost.
- `cc-slack login` and `reload` from a checkout with its own `.claude/cc-slack.toml` keep every setting in `~/.config/cc-slack/config.toml` that the checkout's file leaves out, such as `[pr_links] card_command`, `[pictures]` with its `[ssm]` keys, and `[links] tailnet`. A key both files set takes the checkout's value. Before, the checkout's file replaced the home file whole: on Nov 10 at 2:52 AM a `login` from a checkout whose file names no card command left the daemon with no PR cards and no pictures bucket.
- A DM line that names what waits on you without naming any one thing, such as "a list of what's still waiting on you, coming in a minute", posts instead of being refused for linking no card. A line that says something waits on you, such as "This waits on your answer.", is still refused without its card. On Nov 10 at 3:31 AM the drive's ack promising that list was refused.
- A post with `tap_prs` that cannot draw a PR card at all now fails after the post goes out, naming the post and the reason, instead of returning OK. The two reasons are a login whose config sets no `[pr_links] card_command`, or whose card program is not on the daemon's PATH, and a channel that draws no PR cards. On Nov 10 a `cc-slack login` at 2:52 AM read a config with no `card_command`, and three `dm-status` calls with `tap_prs` over the next 12 minutes each returned OK with no card.
- A DM status line written in Markdown, such as `**Ready.** It deletes nothing.`, now posts its notification text in Slack's own format too, so `slack_thread` reads it back once instead of twice, once with `**` and once in italics.
- `card-copy` given the link of a copy copies the card that copy came from, with its buttons. Before, it read the copy, whose buttons already point at the original card, counted no answer on it, and refused with "that card has no answer left to tap".
- A window whose channel works gets each message on a watched thread or channel, and each card tap, once, as a channel tag, no longer followed by the same words as a directive in its next tool result. A message the operator wrote now carries `operator: true`. A tap now carries its `capture` line, the `notes_label` to record it under for a permission card, and `poster_ended` when it reached the DM holder because the card's session ended. With those fields the tag says everything its directive did, and the slack skill's `reference/cards.md` says how to act on each. On Nov 10 between 1:49 and 1:57 AM a flapping Datadog monitor's channel messages reached the drive coordinator twice each.
- The slack skill's `reference/cards.md` and `reference/tools.md` again explain the fields whose only description was in the tool schemas #248 shortened: a card's `supersedes`, an ask's `description`, `style` and `confirm`, `progress.label`, `slack_stream`'s task fields and `stop`, `slack_card_answer`'s effects and `settled`, `slack_file`'s default `out`, and `slack_channel_create`'s name rules.
- A tap on a waiting card counts again as your answer to the permission hook, so a Slack write you approved with a tap goes out. Since v0.19.32 a tap carried no `question` or `options`, and capt-hook reads a tap as an answer only when both are there and the label is one of the options, so every tapped approval was refused. A test now holds the Go tap's keys to the fixture the permission hook's tests read.

- A window whose channel works gets each DM message, watch error and stopped-owner notice once: as a channel tag, no longer followed by the same words as a directive in its next tool result. Each such directive now names the event it carries (cc-interact v0.41.0), so the hook skips it once the window's proven channel has pushed the event, and a channel withholds an event the hook already delivered. A window without a proven channel still gets each one as a directive. Messages on watched threads and channels, and card taps, still arrive both ways: only their directive says whether the operator wrote the message, or what the tapped ask was and the capture to record.
- One post naming a stack in `tap_prs` draws one card for the stack, even when one of its PRs already has an open card. The daemon now asks the card command for every PR the post names, the ones with an open card included, so the program groups the whole stack; it skips the ask only when one open card already carries all of them. A drawn card whose PRs differ from an older card that carries some of them posts fresh, and the older card is edited in place to "Replaced: now one card for its stack of N PRs", a link to the new card, and the answer it got if he answered it, and is never redrawn or picked again. A card drawn for exactly the PRs of a posted card still redraws it in place. Before, a PR with an open card was left out of the ask, so the program grouped the rest without it: on Nov 9 a post naming a stack of four, one of them with an open card from 4:42 PM, left three cards for one stack.
- A PR you answered, then rewritten so your answer no longer stands, gets a fresh card with Approve and Hold, and its old card says it was replaced. When the card command draws the PR waiting on your tap again with a `changed` line, the next post naming it in `tap_prs` posts the fresh card, opened as a first card is, with that line first as Changed since your answer and a working copy beside the post; the old card is edited in place to "Replaced:", that line, a link to the fresh card, and the answer you gave the old version, and is never redrawn again. Before, the old card was redrawn with the new title and still read your old answer with no buttons, a post in its thread drew nothing, and `drive-cards` listed nothing to tap: on Nov 9 a PR held at 5:25 PM and rewritten to do the opposite left nothing to tap.
- `check-post` flags a line that leads with what is not proven yet, under any label: "Not proven yet", "Not yet proven", "Unproven", "Not verified", "Unverified", "Not confirmed", "Not seen working yet" or "Untested", in any case, as a paragraph, a bullet or a bold label, and outside a `>` quote. Its fix says to delete the paragraph and put a job on the unknown, so the reader gets its result or nothing. It had no such rule, so on Nov 9 a status post whose bold label read "Not proven yet" passed. Every rule now reads through `*bold*`, `_italic_` and `~strike~` markers, so a greeting to a bold name such as "Thanks *Andrew*" is flagged too, and the plain-words hook no longer fails on a finding it has no wording for, such as a plain-name greeting.
- `check-post` no longer reads a hex run joined to a longer identifier by `_` or `-` as a raw commit sha, such as a UUID or the Orca terminal handle `term_afce9df1-5316-4b5f-8636-26f9a5ea8391`, whose last group it flagged. On Nov 9 a post went out without its two `orca terminal switch` commands because of it. A standalone sha of 7 to 40 characters, in prose or in a code span, is still flagged.
- `dm-owed` no longer lists what owes you nothing: a tap a card recorded as its answer, your reply a card recorded as its answer (any reply in a card's thread up to its answer), or a DM message read again after the bot already replied under it. On Nov 9 it listed eleven such entries, eight tap echoes, one "1+2" a card showed as its answer and two messages answered at 9:35 AM, so the list never emptied.
- A window that gets a new channel no longer has every Slack message and tap since its last channel pushed into it. The channel now starts at the session's newest event, since hook directives already delivered what arrived while no channel was attached. On Nov 9 at 5:08 PM a rebuilt window's channel started from a cursor three days old and pushed 492 old messages and taps in two minutes. A channel that drops and reconnects inside one process still catches up on its gap.
- A waiting card sent with its body as both `text` and `card.body` draws the body once, so resending the same card with its words corrected that way edits nothing and posts nothing. An open card whose words really change is still edited in place, its copies follow, and one fresh copy is posted where the card sits.
- In a channel whose bug threads carry the one-solved-line grant, a thread the bot has already posted in gets no second post from the judge unless one of the owner's recorded answers names that thread. The judge had passed a second fix line on a ruling that only asked for a status list of every bug.
- In a channel whose bug threads carry the one-solved-line grant, a thread post that asks the reporter something or carries jargon no longer reaches the judge, which had passed a "could you try it and let me know" line on an unrelated status ruling. Only the owner's own approval of that post, a card tap, his answer or his link, lets it out.
- A window whose channel comes back after a while no longer gets old Slack messages and card taps from watches that have since ended. The channel resumes from its saved position, and the daemon now holds back any message or tap whose watch is no longer active for that session, so only the watches `cc-slack follows` lists deliver. On Nov 9 a retired window's channel reconnected from a Nov 6 position and replayed about 300 messages and taps from stopped watches into that terminal.
- A card's pictures show once, inside the card. They were shared into the card's own thread so Slack would let you open them, which drew every picture a second time under the card. They are now shared into one thread in your DM with the app that holds every card's pictures, posted once the first time a card carries one.

### Changed

- A PR card gives each PR its own section: a divider, then the PR's title and link with its own Open button, then a Refresher for someone who knows the codebase but has not read this code lately, the Biggest surprise, what you'll hate most, and its Show me with that PR's pictures inline under it. A card's body, such as a stack's own summary, leads the card above the PR sections. "Where it stands" is left out while CI is green and the PR is open, and "Waits on" while it would only say it lands once you approve. The card reads `refresher`, `surprise` and per-PR `pictures` where it read `eli5` and `change`; the Open on GitHub buttons in the bottom row are gone.
- An answered PR card folds to its title, your pick, and one line per PR with its link and state.
- A waiting card takes image files with `dm-status`, from any sender, and shows them inside the card; before, only a waiting card cc-present sent could.

### Added

- A bug thread gets its one solved line without a card. Once the owner's grant is recorded as a `slack-grant:<channel>/solved` answer, the first post in each thread of that channel passes when it is plain text with no question, ask or jargon; a second post in that thread, a post with an ask or jargon, a top-level post, and a post in any other channel still need his permission.
- `cc-slack canvas --title <t> [--canvas <id>]` draws the markdown on stdin as a canvas tabbed in your DM with the app, and redraws it in place on later calls, so a drive's status page lives as a tab at the top of that DM. `cc-slack drive-cards --drive <name>` lists the drive's open question cards and the PR cards still waiting on your tap, with links, for the drive's queue.
- A PR card can carry a whole stack: a card whose `prs` lists several PRs counts as the card of each, so `tap_prs` naming the stack draws it once with no NO APPROVE warning for the PRs it carries, and a new head or state on any of them redraws it in place. Before, a status card that carried one card for three PRs warned that two drew no card, and only the first PR's changes redrew it.
- A PR card row shows its ELI5 first under an ELI5 label, then What changes for you, What you'll hate most (`hate_most`), Where it stands, Waits on and Differs from the plan, with Show me on a full-width row of its own below. Risk is gone from the card: the owner found its threat-model bullets unhelpful, and What you'll hate most takes its place.
- The daemon puts eyes and Slack's working line, reading "Picked up…", on your DM message itself the moment it hands that message to a session, with no model call in between. Before, both waited for the session to reach a `slack_pickup` call, often minutes later while it finished other steps. A message no session receives, such as one the daemon answers with a list of sessions, gets neither, and a message a session already had gets no second pair. A session's own `slack_pickup` now only changes what the line says, and `done` still takes both away.
- `cc-slack apply --plan <file> [--dry-run]` deletes or rewrites the bot's own posts in your DM with it, entry by entry from a reviewed plan, in one command. Before, the permission hook let one `cc-slack` write through for each Bash call, so cleaning up 600 posts took 600 tool calls. The daemon refuses any entry outside that DM or not posted by the bot, and the run goes on. Each message is appended, as Slack returned it, to `<plan>.archive.jsonl` before it changes. The run waits out Slack's `Retry-After` and paces itself under the delete and update rate limits, and a rerun skips every entry `<plan>.done.jsonl` lists. It ends with one line: deleted, rewritten, skipped, refused, failed. `--dry-run` reads and checks every entry and changes nothing. The permission hook passes the command with no ticket when the plan parses and names only delete, rewrite, or keep entries in one DM, and refuses it with the reason otherwise.
- `cc-slack reload` hands the daemon the current cc-slack config and keeps the bot token, socket URL and avatar key from the last `login`, so a config change such as a new `card_command` needs no AWS access. Before, only `login` took a config change, and it read every SSM parameter again.
- `[ssm] profile` names the AWS profile `login` signs in with to read its SSM parameters. Before, `login` used whatever profile the shell had, and a read that failed under the wrong role named neither the profile it used nor a way to pick one; the error now names both.
- A PR card's Risk line is now ELI5, a one- or two-line plain explainer of the change from the reviewer bot, drawn in Risk's place beside What changes, Against the plan and Blocks landing. The card command and a `pr` block send it as `eli5`.
- An operator reply in a DM thread under a live card, or under a copy of one ("Answer it here:"), now carries `thread_card` in its `slack.message` payload: the card's `waits` id, the copied card's for a copy. A session that reads his replies can route them to the lane that drew the card; before, a reply under a copy named no card at all.
- `cc-slack follows [--session <id>]` lists a session's active Slack watches, channel follows first, from stored state with no Slack call. Before, nothing showed which channels a session followed: `cc-slack status --session` printed only a watcher count.
- `cc-slack check-post <file|->` runs the plugin's post checker with its findings, output and exit status. A session's PATH keeps the plugin `bin/` it started with, so a bare `check-post` was missing in any session older than the plugin version that shipped it; `cc-slack` always reaches the newest installed version.
- A PR tap card, and a status card that names or carries one, now shows the PR as the GitHub card ai-oncall draws, in the same message below Approve and Hold: `#N` and its title, Pull request, a Merge tag (Ready for your tap, Open, Draft, Queued, Merged or Closed), a CI tag, and Open on GitHub, which opens the PR's GitHub page. A message shows at most 3 such cards; the rest of its PRs stay in its blocks.
- Every card that names a PR carries an Open on GitHub button that opens the PR, beside Approve, Hold, Other and Explain more. A card naming several PRs has one button per PR, labeled with its number. A copy of a PR card carried in a status message keeps the button.
- A post to your DM that links a PR ready for your tap now shows that PR once, as its card with Approve and Hold, in the same message or its thread, as if the PR had been passed with `--tap-pr`. Before, its "What this links to" preview listed the PR as "Ready for your tap" with nothing to tap. The preview now lists only PRs that draw no card. When no card command is set, so no card can be drawn, a ready PR's preview says "Not approved yet".
- A post that links one of your DM messages, such as a card, no longer unfurls it. Slack drew the link as a "Shared from" copy with no buttons, often of a card already answered or withdrawn. A link to a message outside your DM still unfurls.

- A status card's artifact now shows its language and size on its Work Object, so you can tell a 3-line file from a 400-turn transcript before opening it. A diagram shows neither and draws as before.

### Fixed

- A tap on a waiting card that names a PR, with no PR card drawn for it, now also reaches the card command, which records it on the drive's PR. Before, a PR card posted with `dm-status --card` instead of `tap_prs` sent his Approve only to the session holding his DM, so the drive's PR row still read no tap and landing refused it.
- `slack_card_answer` settles a card whose posting session has ended, such as one an earlier drive session posted: the card shows the answer linked to `via` in place of its buttons, and no session is told. Before, it refused with `no session on <host> posted that message`, so a card he answered in its thread kept its buttons.
- `slack_card_answer` and `cc-slack card-answer` find the waiting card by its permalink, whichever session posted it, so a session can settle a card an earlier drive session posted. Before, the call also had to give the card's exact `waits` id and refused a board's id such as `rmw8sh2p2` for a card posted as `card-rmw8sh2p2`, so a card he answered in its thread kept its live buttons. `waits` is now optional.
- A thread reply no longer needs a full `slack_thread` read first when the thread's newest message, as the daemon last read it, is one the daemon delivered to the replying agent or one the agent called `slack_pickup` on. Before, the read-before-reply hook refused every ack until a full read, so each acknowledgment of your message waited on one. A reply into a thread with a newer message the agent never got still needs the full read. The hook asks the daemon through the new hidden `cc-slack thread-seen`.
- `slack_pickup` (`cc-slack pickup`) fails, naming Slack's error, when Slack refuses the working line, where it used to answer `ok` with a `line_error` beside it that sessions read past. The eyes stay on. A refusal while clearing the line with `done` still answers `ok` with `line_error`.
- `cc-slack drive-cards` lists a waiting status card that carries PR rows and names no drive as a PR card, so a PR card posted by hand with `dm-status --card` no longer shows in a drive's queue as "no Slack card yet".
- A `dm-status`, `send` or `reply` call whose `tap_prs` names an open PR that draws no card now fails, naming the post it already sent so the caller does not post it again, and each PR with the reason. Before, it answered ok with no card and no log line, as on Nov 9, when the drive's card command matched `forge-ai/monorepo` against `Forge-AI/monorepo` and answered no cards. The card command can now answer `skipped` with a reason per PR: a first draw fails with that reason, and a redraw leaves the card as drawn and logs it.
- A question card that names its drive, a `waits` card with `drive` set, posts with no warning. A long-running drive asks the owner in Slack by design, and every one of its questions came back with "a question goes on the cc-present board first", as if the drive had skipped a step. A question card without `drive` still carries the warning when cc-present is installed, and both still land on the board.
- `cc-slack check-post` accepts a bare link to a Slack message, a Sentry issue, or a Datadog monitor, incident or event, so an incident post can carry the alert and its issue as links Slack unfurls. Before, it flagged every bare URL with "wrap it as <url|short label>", and a wrapped link shows no preview. Any other bare URL, such as a GitHub link, is still flagged.
- A reply card with no `waits` can carry `recommended`, or an option ending in "(Recommended)": it draws that option first, marked (Recommended), keeps Ack and Pause, and says nothing about a stopped lane, so a question the lane works past can still show its recommendation. Before, `dm-status` refused it with "recommended marks a waiting card's answer; set waits too", and setting `waits` made the card say the lane was stopped when it was not.
- `cc-slack check-post` run from another plugin's process, such as `lr reply`, finds its own checker. The child inherited that plugin's `BINRUN_PLUGIN_ROOT`, built a path that did not exist, and every check refused with a fork/exec error. A `BINRUN_PLUGIN_ROOT` that holds no `skills/slack/scripts/check-post` is now passed over for the newest installed cc-slack.
- A card he answered keeps its words current. cc-present may send a board card again under the `waits` id of a card he answered with only its words changed, such as its title, body, refs, hints or a PR's text. `dm-status --mirrored` then redraws that message in place with the new words, his answer, and no buttons. A withdrawn card resent that way keeps its withdrawn line and mark; only a resend that changes the question brings its buttons back. Before, it refused the card with "ask again under a new waits id", so an answered card kept a stale title. A change to its options, their order, the recommended answer, its PRs or their heads, its pictures, or the posts it supersedes is still refused, and so is a card a session posts without `--mirrored`.
- A board card resent over a post cc-slack no longer remembers, because its session let go of the thread or 48 hours passed, is refused with "ask again under a new waits id" when that post is a card with no answer left to tap. Before, the resend redrew the answered post as an open question with its buttons back.
- An `edit` of a card's post keeps its words. cc-slack stops redrawing that post from the card it stored, so a later redraw, such as one for a PR that changed state, no longer brings back the words the edit replaced. A tap or Explain more on it is drawn on the post as edited, and a card `dm-status` puts on the post again takes it back.
- A withdrawn waiting card asked again under its `waits` id comes back in place: the same message and thread, its buttons back, its withdrawn line and mark gone. Before, `dm-status` refused it with "ask again under a new waits id" as though he had answered it, so a PR that closed and reopened got a second card for the same pick. A card he answered is still refused.
- A copy of a PR card shown in your thread opens with "Answer it here:" only when it carries a button to tap. A copy of a card with nothing to answer, such as a PR that is tapped or has landed, opens with the card's own title, and a redraw that adds or removes the card's buttons switches the opening line to match.
- `cc-slack check-post` run through Homebrew's `cc-slack`, or any way but the plugin's launcher, finds the newest installed plugin's checker. Before, it refused with "BINRUN_PLUGIN_ROOT is unset", and Homebrew's `cc-slack` comes first on most PATHs.
- A header over Slack's 150 characters is cut at its last whole word with an ellipsis, wherever a post or edit carries one, where `card-copy` used to fail with "Slack would refuse block 1 (header)". Slack reads an emoji in a header back as its `:name:`, so a copied header could run past 150 though the card had fit.
- A tap on a card copy whose answer is on record now redraws that copy to its answer and drops its buttons, even when another machine posted the copy. Before, such a copy kept Approve and Hold, and the bot posted "Your tap did not change this message" under it, so the card read as still asking.
- A tap on a card whose session has ended now also reaches the session holding your DM, as a reply it owes, so a tap with no ask id, such as Ack or an option, is heard too. The card closes naming who heard it.
- A tap whose answer was recorded by a daemon that was replaced before delivering it is delivered when the next daemon replays it, where it used to read as already answered and reach no one.
- A card withdrawn because another message replaced it says `Replaced:` and links that message, where it used to say `Withdrawn:`.
- A question card with picks that asks about a PR is the only message about that PR: no information card without buttons is posted beside it.
- A tap on any card now changes the message you tapped. A tap the daemon used to drop in its log, because the session that posted the card has ended or the card now asks something else, keeps the card open and says your pick reached no one, on the card and on the copy you tapped. With a `[pr_links] card_command`, an answer to a card whose session has ended goes to that command as `{"unheard": {...}}`, and once it prints `{"heard": true}` the card and its copies close on your pick. A row on a group card shows your pick before the board answers. After every tap the daemon reads the message back from Slack, and if it did not change, it says so in the thread and says whether the tap was taken.
- A Bash command that writes a `--text-file` and sends it in the same call is refused with that cause and the fix: write the file in its own call, then send. The permission check reads the file before the command runs, so it judged whatever the file held before, and refused a permitted reply as "No permission covers this Slack write".
- A withdrawn card keeps its whole reason, up to 2,500 characters, where it used to cut it at 200 mid-sentence.
- A status card at the root of your DM that names PRs waiting on your tap now carries each PR's tap card inside itself, with Approve and Hold, so you tap them in the message you were sent. A tap there answers the PR's own card, and either one shows the answer. Before, the post's thread held only a link to the card, and Slack's preview of that link draws buttons that do nothing. A text post at the DM root gets a working copy of the card in its thread instead of that link. `dm-status` now waits for the cards, reads the status card back from Slack, and its warning starts with NO APPROVE when the message holds no button to answer a card it lists or a listed PR drew no card. A status card replaced in place with `--tap-pr` carries its cards too.

### Changed

- A reply under a post or card from a session of the DM holder's own drive now reaches the holder, which owes it, and not the posting session. A session belongs to a drive through the `drive` on the cards it posted, and two drive names are one drive when a session posted under both, so an earlier driver session's threads come with a renamed drive. Before, the Nov 3 rule (a reply in another live session's card thread reaches only that session) also kept the drive's own earlier sessions' threads from the driver, which then followed each thread by hand. Threads of sessions on other drives keep that rule.
- The skill now says when an approval card for a post you did not ask for may be drawn: only when the person must act or a fix you approved is live. Your No ends that note for good, an "ask again when X" draws nothing until X is done and shown done, and a card pulled before you answered is never drawn again under any id. Before, it said to ask once, which a worker read as allowing a fresh card each time a build changed.
- A message Claude picks up in a public channel now shows the two-step pickup, eyes with Slack's working line and then the steps once work starts, the same as in your DM with the app. Before, the skill kept public channels to eyes alone until a first channel was named, so a channel saw no sign that work had started.
- A post to your DM that links an open PR still in CI now shows its card with Approve and Hold right away, the same as a PR ready for your tap. Before, it drew a preview with nothing to tap, and in a thread reply that preview sat next to the PR's Approve card. Your tap is stored at that head, and the PR still lands only after its checks, review and grades pass. A held PR keeps its preview.
- A pickup in a public thread shows in two steps, in place of the wrench. `slack_pickup` (`cc-slack pickup`) adds eyes and Slack's working line together and nothing else. `slack_stream` (`cc-slack stream`), called only once Claude decides to do actual work, adds separate steps with a spinner and keeps the working line up. A message Claude looks at and does not work on never gets the steps. The steps work outside your DM with the app, the first call needing a ticket like a reply. Removed: `cc-slack thread-status` and `stream --mode`; the working line's words are now fixed, and the steps are always drawn separately.

### Fixed

- A card artifact's picture draws on the card as an image, and its link alone makes the Work Object. An artifact whose link Slack cannot open, an http address off the `[links]` tailnet, posts the card with its picture and no Work Object, and the result warns why, in place of refusing the card. Every diagram card from a dashboard at a Tailscale IP was refused before.
- A tap redraws its card without waiting out a Slack rate limit the background card refresh ran into. When Slack limits `chat.update`, the refresh stops at once instead of sleeping while it holds a thread's cards, and redraws the threads it skipped once the limit passes. The daemon log names each Slack retry with its method and wait, and each tap with the time from tap to redraw.
- A daemon restart that replays a PR-card tap already drawn no longer draws it again.
- A picture on a card or a message loads for the person reading it. Pictures were uploaded private to the bot and named by an image block, so only the bot could be sure of reading them. The message now posts first, its files are shared into its thread, and the message then shows them. A card sent again with the same pictures keeps them and shares nothing new.

### Changed

- A settled card collapses to its title and one outcome line, on every route: a tap, an answer from elsewhere, a settle, a withdrawal, and a card the live store has forgotten. A PR card keeps a link to its PR, a withdrawal naming another Slack post links it, and an answered group row keeps only its PR's headline.

### Fixed

- A post naming a PR always draws its card, with or without a question: a row with nothing to approve shows where it stands and no button, and a card follows its row on the same post at every change, a new head included.
- A reply in a thread naming a PR carries a working copy of its card under the reply, redrawn with the card.
- A paged plain DM post, an edit, and a card answer's written-in reason draw no HTML comment.
- A tap redraws its card within about a second, whatever GitHub, the card command or Slack are doing. Card redraws write to Slack outside the lock every card shares, taps run beside the socket instead of on it, one queue per card, and a PR card draws his answer before the card command applies it.
- A post no longer reports a failure after Slack took it: the PR lookups that draw its PR cards run after the reply, not on the caller's clock.
- A restart redraws only the cards whose PRs or builds moved while it was down. Their last known states are kept in `live-refs.json` beside `live-cards.json`, so an install no longer rewrites every tracked card.
- A redrawn card stays in the thread it was posted in. It used to move to the DM root and delete the thread's copy.
- No card field draws a sender's HTML comment, in a card, its options, a PR's lines, a row or a plain post.
- A PR card draws What changes, Against the plan and Risk whole again, in place of the preview from #130.
- A pick's label may run to 200 characters and draws whole on its own row with a short Pick button, in place of a limit of 61.
- A working copy of a card no longer carries an Open the card button.

### Added

- `cc-slack card render --card <file>` prints the Block Kit a status card draws, without posting, so a card's render can be read instead of asked about.

### Changed

- A card with a PR stops at its first screen: the PR's description posts in the card's thread on Explain more, and stays behind the PR link. A card no longer posts a "What this links to:" preview of a PR it already draws.
- A link to a PR in a post no longer draws an Approve card. It keeps its link and ledger preview, and a card with buttons is drawn only for a PR the call lists in `tap_prs` (`dm-status --tap-pr`). Callers that relied on a link to draw the card need `tap_prs`.

### Fixed

- A message carrying a PR Work Object keeps its blocks and attachments. Since 0.6.2 such a card or reply reached Slack as its clipped text alone: raw markdown, cut off, no buttons.
- `card-copy` on a message that drew as text says so, instead of reading "no answer left to tap".
- `card-copy` posts the same card: its header, every paragraph and its live buttons, as blocks instead of an attachment clipped to 600 characters. Images stay on the card, one tap away.
- Every line `cc-slack watch` prints fits the 500 characters a Claude Code Monitor event holds, so a Monitor gets each tap, message, watch error and stopped owner as whole JSON instead of cut mid-line. A longer line drops a card's capture and options, then its question and the thread owner's title, then the permalink, and last cuts its own words and sets `cut`; a tap's id is never cut. `--whole` prints lines as the daemon sent them, for a program reading the stream.
- A Markdown link naming a PR, such as `[#41](https://github.com/acme/app/pull/41)`, posts as one link. Linking used to rewrite both its text and its URL, so the post showed brackets and two links.
- A waiting card with `replaces` now takes the place of that message, buttons included, and stays there on later edits. It posts nothing new. If the edit fails, the error comes back and nothing is posted.
- A PR at one head now shows as one open offer. A tap on its card, or on a pick card whose `prs` names that one PR at that head with the same answers, closes every other such card, cards already open included, and the ledger takes the answer once.
- A thread post under a standing grant is judged against the grant's words, so a grant for replies in a thread or a channel's threads covers what was reported there, and no longer carries a request to a person about anything else. Top-level posts and direct messages under a grant naming that channel or person still pass without a judge.
- A PR card in the user's DM opens at the root in its own thread, instead of following the post that named the PR into whatever thread that post sits in, where the user never saw it. The session whose post named the PR keeps the card's thread watched, a PR at one head gets one card across the DM, and a new head gets a new card once the old one is answered. Listed channels keep the card in the post's thread.
- The Slack permission hook fails open again: a check that cannot finish lets the write through with a warning instead of refusing it.
- Permission for one Slack write no longer covers later writes in that thread or DM. A ticket now covers one write and its exact retry, and each other write is checked again: standing grants, an approval or link not yet used, or the judge reading the user's words, including the words behind earlier tickets there. A follow-up carrying out the same order and a correction still pass; a new ask of a person gets a card. An approved top-level post no longer carries its approval to the thread it opens.

### Added

- `cc-slack watch --only-delivery` makes the stream the only path for the session's Slack messages while it runs, with no channel tag or directive for them.
- A standing grant can cover every thread in a channel: the label `slack-grant:<channel>/threads` lets replies in any thread there go out with no per-thread approval, while a new top-level post still needs one.

### Changed

- A card's footer is one quiet line, the drive or repo, the phase and the time. The branch, worktree and session no longer show.
- A card lists an option with words to read once, beside its own Pick button, in place of a numbered list above a row of numbered buttons.

### Added

- `slack_dm_status` with `thread_ts` posts a status card under that thread, where it used to post every card at the DM root. A directive for a DM message written in a thread names that thread and tells the session to pass it, so a reply lands where the user wrote.
- A status card's `prs` field draws each pull request the same way: its link with live CI, what it changes, what differed from the plan, its risk and what blocks it as fields, and its description read from GitHub. A missing line is left out, and a failed GitHub read posts the card without its description.
- A card's `replaces` field redraws a posted card in place with a new title and body and no buttons, and `hints` carries an option's words to read.
- A card body renders markdown in Block Kit, bold, links, lists and a table block, up to 40,000 characters, splitting under Slack's section limit. A card body has no length limit now, except a root card's. A status card shows its first 400 characters and a waiting card its first 1,500; the rest stays behind Explain more, which posts it in the card's thread.
- The permission hook honours the user's tap on a permission card drawn by another session: `asked-user` finds the write's own card through its board-watch answer row and confirms the tap in that board's recorded interaction. The card as drawn must name the write's destination, a later `slack-revoke:` answer for the place wins, and a ccn row alone, or a lookup that fails, grants nothing.
- The `decide` judge reads the drive's owner ledger: a record in the session tree that cites `owner:m<row>` gives the judge that row's words, read by exact id from the communicator's `owner-relay/owner-ledger.md` in the drive's state dir, never the citing record's own quote.
- A permission card's id names its place, `slack:<channel>:<thread>:<digest>`, and an approving tap on one tells the session to record its capture with the label `slack-grant:<channel>/<thread>`, or `slack-grant:<channel>` for a top-level post or DM.
- A `board-answer` row carrying a `slack-grant:` label counts as a standing grant only when the card title it recorded names the write's destination, so a card whose id and text disagree grants nothing.
- A `slack-revoke:` answer newer than a live ticket withdraws that ticket, so a revocation stops later writes at the place. A No tapped on a permission card records `slack-revoke:` for its place, read from the tapped option's shown label, and a `slack:` card id gets the same plain-words check as a `slack-` one.
- The session holding the DM learns whose card an operator's thread reply answers: the `slack.message` payload carries `thread_owner` with the posting session's id, the card's id and title, and whether that session has ended, and the directive names them.
- A resumed session gets its Slack watches back with no re-arming: the `session-start` hook brings back what the restart orphaned, says so in the session's context, and catches up on anything sent while it was down. A DM another session took meanwhile stays with that session.
- `cc-slack card-answer --withdrawn --label "<reason>"` closes a waiting card whose question was withdrawn. The card keeps its title and shows `Withdrawn: <reason>` in place of its buttons. No session receives a tap, and later answers are refused.
- An answered, settled, or withdrawn waiting card gets a reaction: `white_check_mark` for an answer, `x` for a withdrawal.

### Fixed

- A GitHub token command or PR card command that `login` pinned keeps working after its tool moves: the daemon finds it on its own `PATH`. A removed `gh` shim used to cost every card its live CI, PR title and description until the next `login`.
- A reply in a card's thread reaches only the session that posted the card. The session holding the DM used to get every thread's replies and owe them in `cc-slack dm-owed`, so another drive's relay acted on an order that was not its own. The holder gets the thread back once the posting session ends.
- A card's options are no longer cut off. Every card with options lists them in full, numbered, above its buttons. When any option runs past two words, each button shows only its number, and (Recommended) still marks the recommended one. A tap still answers with the full option. A bare number replied in the card's thread answers the card the way its button would. Any other reply stays an ordinary reply.
- A Slack permission card says in words where the post goes: the channel by `#name`, a direct message by the person's name, and a thread by who started it, when, and its opening words, above the text to be posted. The asking agent opens the body with a `Why:` line. A permission card whose title or body still shows a channel id, thread ts, hash, or lane name is refused before it reaches Slack or the board, with what to write instead.
- A session posting under an old thread no longer replays that thread. Opening a DM thread reset its shared cursor to the thread's root, so every reply came back as new operator steering, for the session and for anyone following the thread. A cursor now stays where it is.
- One Slack message reaches a session once, keyed by its own timestamp. A new watch on the same thread used to pass the delivery check.
- A resumed session's channel carries Slack tags again (cc-interact 0.40.2).
- `cc-slack history` with a large `--limit` prints what it read before its deadline, marked `partial`, instead of failing with no output.
- A bare `#N` no longer links to whichever candidate repo GitHub finds first. With `card_command` set, the daemon asks the program for `{"numbers": [...]}` and links a bare number only to the one PR it names, then draws that PR's card; without it, a bare number links only when exactly one candidate repo holds it. A number held by none or by two stays plain text. `owner/repo#N` and PR URLs link as before.
- A Slack call answered 429 or 5xx, or whose connection drops, tries again, up to four tries in all, instead of failing on the first. Posts, file shares, and channel creates retry only a 429, so a retry never posts twice. A 5xx or dropped connection that outlasts its retries says how many tries it took.
- A `+1` reaction from the bot on your DM message answers it: `cc-slack dm-owed` stops listing it, and only that message. `eyes` and other reactions still leave it owed.
- `cc-slack login` no longer fails when the program `card_command` names is missing from this machine. PR cards stay off there and PR links still work, so a shared config can name a tool only some people have.
- Only one session holds your DM with the bot. A second session asking to follow it gets `held_by` with the holder's session id and no watch, so your replies stop reaching an unrelated session. `cc-slack dm-owed` names the holder as `holder_session`, so only that session owes the replies.
- Deleting a card forgets the replies owed on it, since nothing can answer in a thread that is gone.
- The daemon no longer posts anything of its own when you reply in your DM. The "Got it" lines are gone; your reply is still delivered and still listed by `cc-slack dm-owed` until someone answers it.
- A Slack write made while capt-hook is being reinstalled waits up to 30 seconds for it to come back instead of failing. If it stays missing, the error says capt-hook is missing rather than blaming the ticket.

### Changed

- With `proof_channels` set, the incident-comms checks also run in every channel shared with another Slack workspace, so a new customer channel is covered without a config change. `cc-slack channels --json` reports `ext_shared`.

### Added

- A waiting card takes `supersedes`, the permalinks of the bot's earlier posts in your DM that asked the same question, such as a plain-text pick. Each one is edited to point to the card when it posts, and again with the outcome when the card is answered on any surface, so an old pick no longer sits open after it was asked again or answered.
- `slack_card_answer` answers a waiting card with an answer given somewhere else, such as a thread reply a lane relays. The card shows the answer linked to where it was given, the asking session gets its `slack.action`, and the posts the card supersedes show the outcome, with no ticket needed.
- A `slack_dm_status` card with a `key` edits the session's earlier card with that key in place instead of posting a second one, so a caller can redraw a status card it posted. A root-only, waiting, or group card takes no key, since each is redrawn by its own id.
- `slack_send`, `slack_reply`, `slack_edit`, and `slack_dm_status` turn every pull request reference in the text into a Slack link labeled `#N`: a GitHub PR URL, `owner/repo#N`, or a bare `#N`. A bare number resolves against the session's own repo, then each entry of `repos` under `[pr_links]`. Text already linked, code spans, and code blocks stay as written, and so does a number no listed repo holds. Each post reads GitHub once however many PRs it names, and a failed read sends the text as written and logs why.
- With `card_command` under `[pr_links]`, a post in your DM, or in a channel `card_channels` lists, gets a card in its thread for each open PR it links that the program draws a card for, once per thread. `lr board card` draws the merge row with Merge and Hold. A merged or closed PR, or one the program leaves out, stays a link. Merge or Hold goes back to the program, and the card shows the answer.
- A status card with `refs` and nothing else offers no Ack or Pause.
- `cc-slack dm-owed` lists your DM replies no bot or agent has answered in their thread yet, from stored state with no Slack call. A reply is kept even when no live session watches its thread, so one on a thread a finished session posted is not lost.

### Changed

- A write refused for want of a ticket now says why one is often missing: a cc-slack call inside a script, a function or a pipe the permission hook cannot read gets no ticket, so run it as its own command or through its `slack_*` tool.
- A session holds your DM by watching its channel: every reply you write in any DM thread reaches it as well as any session watching that thread, your top-level DMs go to it without the session list, and other bots' messages stay out of it.
- The two incident-comms checks, no asking a person for evidence and no unproven "fixed" line, run only in channels whose name matches `proof_channels` under `[permissions]` in `.claude/cc-slack.toml`, and never in a DM or a thread we posted. A project with no list runs neither.
- Every `slack_dm_status` update and every mirrored board card posts as its own message at the root of your DM, with its own thread for your replies and taps, instead of inside one status thread per session. A reply or tap on a message reaches the session that posted it, up to the newest 500 messages per session. An edit of the agent's own message in this DM needs no second ask. Reconnecting reads the DM history instead of polling every thread.

### Fixed

- `cc-slack login` accepts `proof_channels` under `[permissions]` instead of refusing it as an unknown key.
- A waiting card cc-present sends can carry images, shown on the card above its buttons and kept when a tap redraws it. It can also have no recommended answer, and with no answers at all its write-in button reads Answer. A card posted by hand still needs its answers and a recommendation, and still takes no files.
- The incident-comms hook no longer blocks a release note that says a tool is fixed in a named version, such as `Fixed in ccx v0.91.61`. It still blocks telling a person their own problem is fixed without their `usr_` id, the thread owner's mention and a link to their case succeeding.
- A cc-present group card that gains a row its earlier card did not carry posts as a new message at the bottom of the DM thread and deletes the earlier card, instead of editing a card posted days earlier far up the thread, where a new merge question reached the user with no notification.

### Added

- A card link may be an http url whose host is on the tailnet named by `[links] tailnet` in the config, such as a page served on a tailnet machine. Every other card link, and every rollup PR link, still needs https.
- A reply into a thread is refused unless this agent read that whole thread in the last 10 minutes. The check covers `slack_reply`, `slack_send` with a thread, the CLI, and `mcp__slack__slack_send_message`. The refusal names the `slack_thread` call to make. A truncated or stale read does not count, and each subagent reads for itself.
- `slack_thread` and `slack_history` list each shared message under `shares`, with its original author, channel, text, and files, and name that author in the message text. Each image comes back with a local `path` you can Read, so a screenshot in a thread or a share no longer needs its own `slack_file` call.
- A channel ai-oncall opened for a page the drive answered takes the standing grant of the channel the page is in. The Slack permission hook reads the newest page threads of each granted channel for ai-oncall's `Incident opened: <#channel>` post in a thread an agent replied in. An agent's own post naming a channel grants nothing, and a revoke on the page channel withdraws the room too.

### Changed

- The Homebrew cask strips quarantine with a `postflight_steps` stanza instead of the Ruby `postflight` block Homebrew 7 deprecated, so `brew upgrade` no longer warns.
- Any process running as your user can now connect to the cc-slack daemon's socket. The daemon no longer checks a caller's code signature or user ID. Each command names its build by the version string instead of SHA-256-hashing the executable, and it does not run `launchctl print` when the daemon already serves this release. A dirty `task build` stamps its version with the build time, so each local build replaces the running daemon. The first command after upgrading stops the old daemon with SIGTERM, because the old daemon speaks an older wire protocol.

### Removed

- `--rebuild`. The daemon is now replaced exactly when its version differs, and a dirty local build always gets a new version, so the flag has nothing left to force.

### Fixed

- A session whose cc-slack process started before the daemon moved to wire protocol 3 can read and post again without `/mcp` reconnect. The daemon now answers a protocol-2 build's health probe, so every build before 0.4.3 sees the newer release and hand each command to its executable. Before, every call from such a build failed with `unsupported protocol version: got 3, want 2`.
- When a later release's daemon speaks a newer wire protocol, this build hands each command to that daemon's executable instead of failing. It finds the daemon's process through its socket and reads the release from `--version`. Builds before 0.4.3 failed every call with `unsupported protocol version` against the 0.4.3 daemon, and need `/mcp` reconnect to recover. From 0.4.3, an older build's ensure would have stopped the newer daemon with SIGTERM and started itself in its place.
- A message whose text opens with the Slack MCP fallback marker `_(<name>'s Claude)_` is an agent's own post, so watches mark it `from_claude` and the status thread no longer turns it into operator steering. Such a post goes out under the engineer's own Slack user, so a DM watch used to deliver it back to the session as a DM from the operator.
- `thread`, `history`, and watched messages print the text and link buttons a message carries in its blocks, and each attachment's fields and blocks. An alert app such as Sentry that posts its issue link only in blocks now shows that link in place of its one-line summary.

### Added

- `cc-slack card-answer --settled --other --label "<outcome>"` closes a waiting card that events already settled, such as a merge card whose pull request landed. The card shows the outcome, linked to `--via`, in place of its buttons. No session receives a tap or capture, and later answers are refused.

## [0.3.0] - 2026-10-26

### Added

- A dm-status card with a `group` id and `rows` asks a cc-present board's rows on Slack, each row with its own buttons. Posting the same `group` again edits its card in place, and an empty group is posted only to edit an earlier card. A tap on a row is sent to the board with `cc-present slack-sync decided` and recorded there as the row's verdict, not sent to the session. The row then shows its answer, or why the board refused it, and a refused row asks again on the next push.

## [0.2.5] - 2026-10-26

### Fixed

- `send` and `reply` post text past 4,000 characters as one message. Slack split such a post into several messages and returned only the last one's `ts`, so an edit of the post hit the wrong message. The text now goes out as section blocks with a short notification text, and `thread` and `history` read the whole text back.

## [0.2.4] - 2026-10-26

### Fixed

- `edit` replaces a message with text past 4,000 characters, the most `chat.update` takes, by rendering the text as section blocks and sending the first 4,000 characters as its notification text. A replacement as long as a `send` posted no longer fails with `msg_too_long`.

## [0.2.3] - 2026-10-23

### Changed

- The plain-words hook warns at most once per agent, in one line, and no longer tells the agent to run `check-post` first.
- An allowed Slack write, and the ticket a top-level post carries to its own thread, no longer print a system message. The permission decision is unchanged.

## [0.2.2] - 2026-10-23

### Changed

- With cc-present installed, a `dm-status` card that waits on an answer and did not come from cc-present is still posted, and the reply carries a `warning`: a question goes on the cc-present board first and reaches Slack through `cc-present send`.
- The permission hook's refusal prints the approval question as a cc-present board card for `cc-present ask -`, beside the Slack card for sessions without cc-present.
- An Explain more tap tells the session to answer with `cc-present reply` when the card is on a cc-present board, so the answer lands on both surfaces.

## [0.2.1] - 2026-10-23

### Changed

- One `dm-status` call posts a card from whatever it is given: text beside a card becomes its body, a missing status becomes `needs_you` on a waiting card and `info` otherwise, a missing title becomes the body's first line, and an option marked "(Recommended)" becomes the recommended answer. A card that still fails names every problem at once.
- An option label runs to 61 characters, so it fits a 75-character Slack button with "(Recommended)" after it.

## [0.2.0] - 2026-10-23

### Added
- A `waits` card names its recommended answer in `recommended`. The card shows that answer first as "<label> (Recommended)" in the primary style.
- Every `waits` card carries a built-in Other button beside Explain more. The backend opens a modal on the tap and pushes the typed text as a `cc-slack-card:wrote:<id>` action, which answers the card; the `slack.action` carries `other: true`. A bare Other tap answers nothing.
- `cc-slack card-answer --waits <id> --card <permalink>` answers a waiting card from another surface, such as a cc-present board. It fires the same `slack.action` capture a tap fires, naming the surface in `via`, and refuses a late answer by naming the one that won and whether it was written in.
- With `cc-present` on PATH, a posted `waits` card is added to the session's cc-present board, with each GitHub pull request in its `refs` as a live PR card, and an answer on Slack is sent there too. `dm-status --mirrored` posts a card the board already holds without telling the board again.
- `dm-status` returns the posted card's `ts` and `permalink`.
- `slack_dm_status` and `dm-status --about <waits id>` open a text reply with a Slack link back to that waiting card, including one another app posted in the session's status thread with an Explain more button for that id.

### Changed
- A `waits` card without `recommended`, with "(Recommended)" in a label, or with an Other option is refused.
- A card that reuses the `waits` id of a card still waiting edits that card in place instead of posting a second one. Reusing the id of an answered card is refused.
- The permission hook's approval card recommends Yes.
- The Explain more directive tells the session to reply with `about`, so the reply opens with a Slack link back to the card.
- A pull request in a card's `refs` is labeled "<repo> PR <number>".

## [0.1.10] - 2026-10-23

### Changed
- The permission hook asks about a Slack write the user did not request on an ask card in the DM status thread, never through `AskUserQuestion`. Its refusal prints the `slack_dm_status` card: the title names the place, the body carries the draft, and the answers are Yes and No. A tap on Yes approves the write.
- The plugin requires captain-hook 12.104.0 or later, which reads a tap on a cc-slack question card as the user's answer.
- A bare `slack-grant:<channel>` answer, or an asked answer to keep posting in a channel, now covers top-level posts there only. A reply in one of the channel's threads needs that thread's own `slack-grant:<channel>/<thread ts>` grant or another permission.
- The permission hook's owed-update rule is gone. A thread whose latest message is a person's, in a channel the session touched, no longer permits a reply there; a reply needs that thread's grant, a ticket, or the user's word. The daemon's thread read before an owed-update reply goes with it, and sends, replies, edits, and deletes no longer get the longer timeout that read needed.

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

[Unreleased]: https://github.com/yasyf/cc-slack/compare/v0.2.5...HEAD
[0.2.5]: https://github.com/yasyf/cc-slack/compare/v0.2.4...v0.2.5
[0.2.4]: https://github.com/yasyf/cc-slack/compare/v0.2.3...v0.2.4
[0.2.3]: https://github.com/yasyf/cc-slack/compare/v0.2.2...v0.2.3
[0.2.2]: https://github.com/yasyf/cc-slack/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/yasyf/cc-slack/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/yasyf/cc-slack/compare/v0.1.10...v0.2.0
[0.1.10]: https://github.com/yasyf/cc-slack/compare/v0.1.9...v0.1.10
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
