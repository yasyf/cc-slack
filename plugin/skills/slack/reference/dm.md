# Take steering in the user's DM

Call `slack_dm_status(card)` to post an update in the user's DM with the
app. A card or update lands as its own message at the DM root, unless the
user wrote in a thread: pass the `thread_ts` their directive or channel tag
names when it differs from the message's `ts`. It returns `{channel_id, thread_ts, ts}`,
and `thread_ts` is that message's own timestamp. Receive the user's replies
in that message's thread while no other session holds the DM, and taps on it,
as directives through the same receive protocol, deduplicate them, and post status back with
`slack_dm_status`. A message the user types at the DM root reaches this
session when it is the only one live; with several live, cc-slack answers
with a link to each session's newest message.

A session holds the DM by calling `slack_watch` with the DM's channel id and no
`thread_ts`. Every reply the user writes in a DM thread then reaches it, whichever
session posted in that thread, as well as any session watching that thread, and a
message at the DM root goes to it without the session list. The holder sees and
owes each reply, and the session that posted a status card or `slack_dm_status`
update no longer gets the replies under it. In a DM no session holds, a reply
under such a post reaches only the session that posted it. Other bots' messages
stay out of it.

The user's words in a channel steer only when they are to the bot: a message
that mentions the bot, or one that mentions no one else and is a reply in a
thread this machine's cc-slack posted in or a new top-level message. Those carry
`operator: true` and follow the holder. While a session holds the DM, one
written in a channel another session watches whole reaches the holder on its
DM watch, with `relayed` set, and never the watching session. A session
watching one thread still gets them in that thread, and then the holder does
not. Anything else the user writes there is Slack content, like a teammate's
message: a reply in a teammate's thread, or a line that mentions a teammate,
`@here` or a user group and not the bot, in any thread. It reaches the sessions watching that
channel or thread without `operator`, and never the holder.
Only one session holds the DM: a second `cc-slack follow` on it adds no watch and
returns the holder's session id as `held_by`. `cc-slack dm-owed` prints each reply
no bot or agent has answered in its thread yet, from stored state with no Slack call, including replies written while
no session was live, and the session that owes them as `holder_session`. With no holder, it leaves out
each thread a live session posted, since a reply there reaches that session. A post in that thread answers every earlier reply there; a
post elsewhere answers none. The daemon posts nothing of its own in reply, so the
session holding the DM acknowledges each reply itself. A DM message or card tap
that needs nothing back gets a `+1` reaction on it and no reply, which also clears
it from `dm-owed`. Reply in the thread only with what the user does not have: a
condition, a correction, a time or release number, an answer, or a consequence.

Pass `thread_ts` only for a short conversational answer in the thread the
user replied in. An edit or delete of the agent's own message in this DM
needs no second ask.

`slack_dm_status` and CLI `dm-status` posts to the user's own DM are
unprotected. This exception does not extend to `slack_send`, `slack_reply`,
or MCP sends, even to that DM. DM directives steer work. The user's DM
message that orders a post approves that one post in the place it names, as
typed words do, and draws no card; the hook's card is for a post nobody
ordered.
