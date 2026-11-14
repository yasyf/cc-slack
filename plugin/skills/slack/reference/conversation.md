# React and keep the conversation going

## React before you reply

On any message you have read and are acting on but have not answered, add
`eyes` at once. A message you pick up shows in two steps, in every channel and
DM, so its readers can see whether it is being read or worked on:

1. When you first look at it, call `slack_pickup`. It adds `eyes` and Slack's
   working line under the thread, together and nothing else. For a reply in
   a thread it reads the thread from Slack when you leave out `thread_ts`,
   since Slack refuses the line under a reply's own ts. Your operator's
   message in your DM with the app already has both: the daemon puts them on
   as it hands you the message, so `slack_pickup` there only sets what the line
   says. When your session itself is compacting and the message is not in
   your transcript 8 seconds after it arrived, the daemon also posts
   `Got it, back in a minute.` in that message's thread and sends you
   `slack.message.acked` with `acked` set to that post's ts: post no ack of
   your own for it. If you decide not to work on it, call `slack_pickup` with
   `done: true`, which takes both away. Such a message never gets step two.
2. Only once you decide to do actual work, call `slack_stream` with
   `recipient` set to the person who asked. It adds each step as its own line
   with a spinner and keeps the working line up. Its first call posts in the
   thread, so it needs a ticket like a reply. Update it as each step starts and
   ends, and stop it when the work ends. Slack ended an unfinished stream by
   itself 6 to 11 minutes after it started, seen once: when an update fails
   because the stream has ended, start a new one with the remaining steps.

When Slack refuses the working line, `slack_pickup` fails with Slack's error
and the eyes stay on; a `line_error` in a `slack_stream` result means the steps
landed without the line. Carry on, and report it to your lane's owner.

When the ask is done, add `white_check_mark` with `slack_react`, then call
`slack_pickup` with `done: true`.
For a message that needs only acknowledgment and no reply, use `pray`, or `+1`
in the user's own DM with the bot. Everywhere else, use only these three:

| Emoji | Name to pass | When |
|---|---|---|
| 👀 | `eyes` | Read, acting on, not yet answered; on a pickup, `slack_pickup` adds it |
| ✅ | `white_check_mark` | Ask done |
| 🙏 | `pray` | Acknowledgment only; no reply needed |

Put 👀 and ✅ on the specific message that addressed you or asked you to act,
usually the thread reply that mentioned you. Never react on the thread root
by default; react there only when the root itself is the ask. If someone's
thread root is just ":catjam:" and a later reply mentions you and asks you
to look "as well if interested", put the reactions on that reply, not the
root.

Pass names without colons to `slack_react(channel_id, ts, name)` and
`slack_unreact(channel_id, ts, name)`, or CLI `react` and `unreact` with
`--channel <id> --ts <ts> --name <name>`. Never react to a `from_claude`
message or use a reaction to replace a reply you owe.

## Keep the conversation going

Post to a person unprompted only when they must act or the fix they reported
is live. In a bug report's thread that is one post, after the fix ships and is
checked, never before: one or two sentences of plain English saying what is
fixed, with no jargon and no asks, so never "please retry", "please confirm"
or "let us know". Your own tests, progress, and "a follow-up" are never posts.
An ask card that offers to ask a reporter something is a defect: the fix is
proven before the post, never by them. Fix a mistake or a gap in it by editing
it. This narrows what a lane asks to
send and changes no permission rule. Questions they ask are not limited:
write every post so a person can answer it, and answer each reply as it lands.

- React `eyes` the moment you look at a message with `slack_pickup`, and call
  `slack_stream` when work starts, as above.
- Answer every question in a watched thread the moment it lands, in that
  thread, before you return to other work. When the answer needs work, say
  in one line what you are doing, then post the answer when you have it.
- Reply the way a person chats: one to three sentences, no labels, no
  restated context.
- Mention each person you address as `<@U…>`, link every build, PR, and
  reference, and state times with no zone label, as [Write a
  post](voice.md) says.
- When you need something, end with one direct question to one mentioned
  person, answerable in a word where possible. Never bury the ask
  mid-paragraph.
- A message telling its reader something waits on them links the card. In your
  DM with the app, link the open card's permalink from its line in `cc-slack drive-cards --drive <drive>`
  under words that name it, as `<permalink|the lock card>`, and Slack draws the
  card under the line the first time its thread links it; the post copies nothing and shows no raw URL, so the card
  stays one message. A PR's tap card comes from `tap_prs`, or from linking the
  PR's URL in a DM post that says something waits on you. A DM line that says
  something waits on you, with no open card linked, no PR linked or in `tap_prs`
  and no open card in its thread, is refused before it posts, and so is one
  linking a PR that draws no card. Put each card on its own line, since Slack
  draws five a message and cc-slack pages a longer DM post between lines. When a card was
  removed or went stale, say so in the same breath and name its replacement.
- Keep answering as the thread continues. Once the user has told you to
  answer someone there, each new question gets its reply without asking again.

```text
Them: <@U0BOT> is the replica caught up yet?
Wrong: no reply for 40 minutes, then "Status update: replication lag on the replica is now 0s as of 9:41pm following the storage target switch at 9:02pm; monitors green; backfill pending the edge allowlist."
Right: 👀 at once, then within a minute "Yes, caught up as of 9:41pm. <@U0123ABCD>, any word from your edge team on the allowlist?"
```
