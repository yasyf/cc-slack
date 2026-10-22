# cc-slack backend protocol

Version 1.

The cc-slack daemon reads Slack only when a backend tells it something changed.
The backend is a service you run beside your Slack app: it receives the app's
Events API and interactivity requests, and pushes small frames to every
connected daemon over a WebSocket. This page is the contract between the two.
Any backend that speaks it works; cc-slack ships none.

The fixtures under `cmd/cc-slack/testdata/protocol/` are the normative
examples, and `cmd/cc-slack/protocol_test.go` decodes each one with the
daemon's own types. A backend that keeps a copy of the fixtures and tests its
encoder against them stays compatible.

## Connection

The daemon dials the `socket_url` it read at `cc-slack login` and sends two
headers on the upgrade request:

| Header | Value |
|---|---|
| `Authorization` | `Bearer <bot token>`, the same token the daemon posts with |
| `x-cc-slack-user` | The logged-in engineer's Slack user ID, such as `U0123ABCDE` |

The backend authenticates the bearer token and routes frames for a user to
every connection that named that user. Frames are UTF-8 text messages.

The daemon sends the text frame `ping` every 60 seconds; the backend answers
`pong` or nothing. A dropped connection reconnects with jittered backoff from
1 s to 60 s.

## Sequence numbers

Every pushed event carries a `seq`, a string that sorts by arrival:

```
<receive time in Unix milliseconds, zero-padded to 15 digits>-<random tiebreak>
```

For example, `001800000001000-a1b2c3`. The backend logs each event frame under
its `seq` for at least two hours before it pushes the frame, so a reconnecting
daemon can replay what it missed.

## Daemon to backend

### `hello`

The daemon's first frame on every connection.

```json
{"type":"hello","since":"001800000000000-7f3a9c"}
```

`since` is the newest `seq` the daemon has fully processed. The daemon omits
it when it has none, or when the saved `seq` is older than 1 h 45 min, the
log's retention less a 15-minute margin; it then rereads every watch instead
of replaying (`hello.json`).

On a hello with `since`, the backend pushes every logged event frame from five
minutes before `since` onward, one at a time in log order, then answers
`caught_up`. On a hello without `since` it answers `caught_up` at once.

## Backend to daemon

### `caught_up`

```json
{"type":"caught_up","seq":"001800000005000-0b12de"}
```

Ends the replay a `hello` started. `seq` is the later of the backend's current
time as a `seq` and the newest `seq` the replay pushed, so the daemon's cursor
advances even when nothing was logged. Any frame with a non-empty `type` ends the replay the same way, so a
backend may add typed frames in a later version without stalling older
daemons.

### Event frames

Every other frame is an event push:

```json
{"event_type":"<type>","event_payload":{...}}
```

The daemon ignores an `event_type` it does not know.

#### `cc_slack_message`

A new message in a channel, private channel, or DM the bot belongs to. The
backend sends coordinates, never the message text; the daemon reads the
message itself through `conversations.replies` or `conversations.history`,
and only for a thread or channel a live watch covers.

| Field | Required | Meaning |
|---|---|---|
| `channel` | yes | Conversation ID |
| `ts` | yes | The message's timestamp |
| `thread_ts` | no | The thread's root timestamp, for a reply |
| `subtype` | no | Slack's message subtype; the daemon routes `""`, `bot_message`, `thread_broadcast`, `file_share`, and `me_message` |
| `seq` | yes | The frame's sequence number |

Fixture: `message.json`.

#### `cc_slack_action`

The engineer tapped a button on a status card the daemon posted in their DM
with the app. The backend acknowledges Slack's interactivity request itself
and forwards the tap.

| Field | Required | Meaning |
|---|---|---|
| `user` | yes | Who tapped |
| `channel` | yes | The DM the card is in |
| `message_ts` | yes | The card's timestamp |
| `thread_ts` | yes | The status thread's root timestamp |
| `action_id` | yes | `cc-slack-card:ack`, `cc-slack-card:pause`, or `cc-slack-card:opt:<n>` |
| `value` | for options | The option's label |
| `action_ts` | yes | Slack's action timestamp, used to deduplicate taps |
| `seq` | yes | The frame's sequence number |

The daemon drops a tap by anyone but the logged-in engineer, or outside their
DM with the app. Fixture: `action.json`.

#### `cc_slack_shortcut`

The engineer ran the app's message shortcut ("Open in Claude"). The backend
posts a DM to the engineer that carries the payload as message metadata with
`event_type` `cc_slack_shortcut` (`shortcut_metadata.json`), then pushes the
same payload with the DM's timestamp added as `dm_ts` (`shortcut.json`).
The daemon reads the app's DM on reconnect, so a shortcut whose push it
missed still opens.

| Field | Required | Meaning |
|---|---|---|
| `user` | yes | Who ran the shortcut |
| `channel` | yes | The source message's conversation |
| `channel_name` | no | Its name, used to name the workspace |
| `ts` | yes | The source message's timestamp |
| `thread_ts` | no | Its thread's root timestamp |
| `response_url` | yes | Slack's `https://hooks.slack.com/...` response URL |
| `dm_ts` | in the push | The DM's timestamp; the daemon takes it from the DM itself when it reads the metadata |

The backend may send more fields, such as `permalink` and `invoked_at`; the
daemon ignores them. It refuses an invocation older than one hour by `dm_ts`.

## Message metadata the daemon writes

Posts the daemon makes carry Slack message metadata with `event_type`
`cc_slack_post` and `event_payload` `{"user_id": "<engineer>"}`, so a backend
or another tool can tie a bot post to the engineer whose agent wrote it.
Posts with doc cards carry Work Object metadata instead.

## Versioning

This document describes version 1. A change that an older daemon or backend
would misread gets a new version number and a section on what changed;
additive changes, such as a new typed frame or a new optional field, do not.
