# Receive watched messages and wait for replies

## Receive watched messages

A session receives Slack messages only for its own subscriptions: its
top-level `slack_watch` calls and its `slack_dm_status` thread. The daemon
delivers each message to the session that subscribed and to no other.
Shortcut hand-offs never reach a session; the daemon opens their workspaces
itself. Arm no Monitor at session start.

Whenever this session goes from no subscriptions to one, through a
successful `slack_watch` without an `owner`, a `slack_send` or `slack_reply`
without `no_watch`, or a `slack_dm_status`, read the `channel` field of that
result:

- `active`: channel tags already reach this session. Arm no Monitor.
- `pending` or `inactive`: arm Monitor with `persistent: true` and a
  description naming that subscription, such as
  `cc-slack: thread <channel_id>/<thread_ts>` or `cc-slack: DM status`,
  running:

  ```bash
  "${CLAUDE_PLUGIN_ROOT}/bin/cc-slack" watch --until-proven --session "$CLAUDE_CODE_SESSION_ID"
  ```

A `pending` result also sends a `<channel source="cc-slack" type="channel.probe">`
tag, which is a delivery check and needs no reply. On every `channel.probe` tag,
and on the first other `<channel source="cc-slack">` tag while the Monitor runs,
run:

```bash
"${CLAUDE_PLUGIN_ROOT}/bin/cc-slack" channel-ack --session "$CLAUDE_CODE_SESSION_ID"
```

The Monitor then prints `{"type":"channel.proven"}` and exits by itself, so
don't re-arm it. Tags carry the session from then on. One Monitor carries every
subscription of the session, and later subscriptions reuse it while it runs.
A session without channels never proves delivery, so its Monitor keeps running.
Stop it with `TaskStop` when `slack_unwatch` returns `remaining: 0`, which
can't happen while the session holds a `slack_dm_status` thread, since that
thread stays subscribed for the session's life. `watch` prints the session's
`slack.message`, `slack.watch_error`, and `agent.relay` events that arrive
after it starts, never earlier ones; `--lifecycle` adds agent lifecycle events
for debugging. `slack_watch` registers the Slack conversation to poll. The CLI
registration verbs are `follow --owner <agent_id>` and `unfollow`.

A `<channel source="cc-slack" type="slack.message">` tag carries
`{type, watch_id, kind, channel_id, thread_ts, ts, user, user_name,
from_claude, text, permalink}`. Monitor lines carry the same event data,
each within the 500 characters a Monitor event holds: a longer line drops a
card's `options`, then its `question` and the thread owner's title, then the
`permalink`, and last cuts its own words and sets `cut`. A tap's `id` is never cut.
A bot's thread reply that repeats its parent, such as an alert under its own
summary, carries only its first line and a mark saying so in `text`; read the
whole message at its `permalink` or with `slack_thread` when you need it.
Top-level sessions also receive each thread or channel message, and each
tap, as an injected steering directive at their next tool call. A DM
message, a watch error, and a `slack.owner_stopped` event arrive once: as a
channel tag when this window's channel is proven, else as a directive.
Deduplicate all deliveries by
`(channel_id, ts)` before acting, including deliveries across watches.

`slack.message@<agent_id>` belongs only to that owner subagent and arrives
through its mailbox, never as a channel tag to the parent.

On `agent.relay`, use `SendMessage` to give the named agent a wake only: identify yourself
and say it has pending directives to collect on its next tool call or
`await`. Include no directive content. Repeated relay wakes are safe.
A stopped agent is no reason to skip the wake: `SendMessage` resumes it.
`slack.owner_stopped` names a stopped agent and the watches it still owns.
Resume it with `SendMessage`, or, when you stopped it on purpose, end each
watch with `slack_unwatch(watch_id)` and arm it again with `slack_watch`
under the agent that took over its work.
On `slack.watch_error`, report the watch and error, then follow the named
fallback. End a finished watch with `slack_unwatch(watch_id)`.

## Wait for a reply or triage a burst

The top-level session uses Monitor or tags and never calls `await`. Keep
working while messages arrive. When a task must block on a reply, dispatch
Agent with `subagent_type: "cc-slack:slack-waiter"` and
`run_in_background: true`. Pass one thread reference, already-seen message
keys, and the stop condition. It returns the first inbound batch as its
final answer; dispatch another waiter to keep listening.

Include optional outbound text verbatim when the user asked for it or a
standing grant covers its thread; the hook checks the waiter's write like any
other. The waiter sends the text before arming the watch and messages back
the posted `ts`. The waiter learns its own id from its
greeting, owns the watch, and parks on
`mcp__plugin_cc-slack_cc-slack__await(agent_id)` in the default 60-second
windows, so a `SendMessage` with newly approved text for its thread
is posted within a minute. Send such text to the running waiter instead
of waiting for its batch. `await` honors an explicit `timeout_seconds` up
to a day, for an owner agent with nothing to hear from its parent; a
`SendMessage` reaches an agent parked that way only when the window ends.

For a burst, dispatch `cc-slack:slack-triage` in the background with the JSON
envelope defined in `../../../agents/slack-triage.md`: thread references and
seen timestamps, parent id, and task guidance. It reads each thread once
and returns `reply`, `act`, or `none` with a reason. Classification is not
permission to send; every resulting write needs a ticket like any other.
