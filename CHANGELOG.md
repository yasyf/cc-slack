# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- A PR tap card, and a status card that names or carries one, now shows the PR as the GitHub card ai-oncall draws, in the same message below Approve and Hold: `#N` and its title, Pull request, a Merge tag (Ready for your tap, Open, Draft, Queued, Merged or Closed), a CI tag, and Open on GitHub, which opens the PR's GitHub page. A message shows at most 3 such cards; the rest of its PRs stay in its blocks.
- Every card that names a PR carries an Open on GitHub button that opens the PR, beside Approve, Hold, Other and Explain more. A card naming several PRs has one button per PR, labeled with its number. A copy of a PR card carried in a status message keeps the button.
- A post to your DM that links a PR ready for your tap now shows that PR once, as its card with Approve and Hold, in the same message or its thread, as if the PR had been passed with `--tap-pr`. Before, its "What this links to" preview listed the PR as "Ready for your tap" with nothing to tap. The preview now lists only PRs that draw no card. When no card command is set, so no card can be drawn, a ready PR's preview says "Not approved yet".
- A post that links one of your DM messages, such as a card, no longer unfurls it. Slack drew the link as a "Shared from" copy with no buttons, often of a card already answered or withdrawn. A link to a message outside your DM still unfurls.

- A status card's artifact now shows its language and size on its Work Object, so you can tell a 3-line file from a 400-turn transcript before opening it. A diagram shows neither and draws as before.

### Fixed

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
