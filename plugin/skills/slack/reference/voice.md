# Write a post

Prepare every `send`, `reply`, `dm-status`, and MCP fallback send in this
order.

1. Write for someone with no context of your conversation: they must
   understand it from the message alone. Name the PR, build, service,
   environment, and time.
2. Apply this section's shape, link, and mention rules below. Write every
   PR and issue as `<url|#N>` and every person as `<@U…>` in the first
   draft.
3. When the post narrates a sequence of events, attach a timeline image as
   "Show a timeline as an image" says.
4. Write the text to a temp file and run `cc-slack check-post <tmpfile>`,
   or `cc-slack check-post -` for stdin. It runs the newest installed
   plugin's checker, which a bare `check-post` on an older session's PATH may
   not reach. Fix every line it prints; it exits 1 on a finding. It flags raw commit
   shas, but not a hex run joined to a longer id such as a UUID or a
   `term_` handle, `G`/`R`/`L` ids, `brw_`/`run_`/`exec_`/`ctx_` ids, ULIDs, bare `#N`
   and build or PR numbers, bare URLs other than a Slack message, Sentry
   issue, or Datadog monitor, incident, or event link, which stay bare so
   Slack unfurls them, one-letter codenames such as "D",
   multi-agent terms such as lane, desk, walker, parity, and fence, zone
   labels such as `PT` or `17:35Z`, an option named by number or position
   such as "same as 1" or "the first option", a greeting to a plain first name such as
   "Hi Andrew" where a mention belongs, an unbackticked `SNAKE_CASE`
   identifier, a paragraph past three sentences, a line outside a `>` quote
   that leads with what is not proven yet under any label, such as "Not
   proven yet", "Not seen working yet", "Unverified" or "Untested", and a
   sentence that says something waits on the reader in a post that links no
   card in your DM with the app and no PR. Delete a not-proven paragraph and
   put a job on the unknown: the reader gets its result, or nothing. It reads
   through `*bold*`, `_italic_` and `~strike~` markers, ignores anything
   inside a `<url|label>` link or `<@U…>` mention, and reads no identifier
   or sentence inside backticks or a code fence. Then check what the script
   cannot detect:

   - Would someone who only reads this thread understand every noun?
   - Is everything mentioned linked?
   - Does it claim only finished, published work?
   - Does it fit the shape rule below?

5. Quote that text verbatim in any approval question and post it verbatim.

Omit file paths, constants, selector names, and internal mechanics.

Fit the shape to the post. A reply in a conversation is one to three
sentences, about 50 words at most. A context post, one that brings someone
into a situation they were not following, uses the labeled paragraphs in
"Give context in labeled paragraphs". Do not restate context the thread
already has. Omit "will confirm when…" and "updates to follow" trailers: no
such post follows, as [Keep the conversation going](conversation.md) says.

Talk like a human teammate. Where it fits, open with a thank-you or
greeting. Address people directly by mention, use full sentences and
contractions, and write the way you talk at their desk. Never send a wall
of text: no paragraph runs past three sentences, and a post with more than
one idea breaks into paragraphs with a blank line between them.

Wrap identifiers in backticks, including role names, IAM actions, ARNs,
hostnames, commands, and env and stack names. Leave product names plain.
Link commits, including PR heads, as `<url|label>`. Never write a bare or
backticked SHA.

State every clock time in the team's local time with no timezone label:
`8pm tonight`, `5:25pm`, or `Sept 30 at 8pm`. Convert UTC times from logs,
CI, and git before writing.

Write every post for the person reading the thread, in plain words. Never
use internal shorthand: tracking ids, agent lane names, raw commit shas, or
object ULIDs. Name the work, never its codename: "the new release
pipeline", not "D". Gloss code nouns in one plain phrase, or leave them
out.

```text
Before (heads-up, teammate's thread): Heads-up: D landed today (old release pipeline gone; #345 changed image/build pairs), so rebase onto main first.
After: Heads up, the <https://github.com/acme/app/pull/345|release pipeline change> merged today and changed how images are built, so rebase onto main before you start.
```

Greet or address a person, in a post or a DM, with a real mention,
`<@U…>`, which Slack renders as their @handle: `hey <@U0123ABCD>`, never
`hey Andrew`. Resolve the user id with `slack_users` before writing; a
first name in plain text where a mention belongs is a defect.

Link every build, PR, issue, deploy, alert, monitor, dashboard, run,
commit, runbook, and doc you mention as `<url|label>`. A mention without a
link is a defect.

```text
PR: <https://github.com/<org>/<repo>/pull/N|#N>
Build: <https://buildkite.com/<org>/<pipeline>/builds/N|deploy #N>
Commit: <https://github.com/<org>/<repo>/commit/<sha>|<7-char sha>>
Doc: <url|short title>
```

A bare `#N`, SHA, or build number is a defect. `slack_send`, `slack_reply`, and
`slack_dm_status` accept Slack mrkdwn as-is. Escape only `&`, `<`, and `>`
in literal text; never escape link syntax.

A finished-work post names only merged or published work, with its final
link. Never announce a draft, pending PR, or "works once X merges" caveat.

```text
Before (all-clear): All clear: the api restored at 21:50 UTC (~12 min out) by redeploying the release it was serving. Cause: a stale release pointer redeployed over a newer DB schema. Retro to follow.
After: All clear: the api is back as of 2:50pm (~12 min down); cause and follow-ups in the <https://docs.example.com/retros/stale-release/|retro>.
```

## Give context in labeled paragraphs

A context post, such as a heads-up, a handoff, an incident summary, or an
ask that needs background, uses Slack bold labels, each on its own line.
Under each label goes one short paragraph of one to three sentences that
holds one idea:

- `*What happened*`
- `*Where we are now*`
- `*What <@U…> needs to do*`, naming the person who has to act

Leave a blank line between sections and drop a label with nothing under
it. Open with a one-line greeting when you address someone, and end on the
ask.

```text
Hi <@U0123ABCD>, thanks for jumping on this with us.

*What happened*
At 8:40pm your CDN started refusing our reads from the backup bucket, so replication paused. Timeline attached.

*Where we are now*
Replication has been running again since 9:02pm, into our own bucket, so no data is at risk.

*What <@U0123ABCD> needs to do*
Could you ask your edge team to allow our requests? Once they do, we'll move back and backfill.
```

## Show a timeline as an image

When a post narrates a sequence of events, three or more timed steps, draw
them as a timeline PNG and attach it to that same post. The text keeps its
labeled paragraphs and names only the times that matter; the image carries
the full sequence.

1. Render a clean horizontal timeline, dark text on white, 1600x600, with
   large labels alternating above and below the line: each event as a time
   with no zone label and one plain-words line in the post's vocabulary.
   Write it outside the worktree.
2. View the PNG with Read and check every time and label. Regenerate on any
   mismatch.
3. Attach it with `files: ["<absolute png path>"]` on the same
   `slack_send` or `slack_reply`, or `--file <path>` on the CLI, so the
   image appears under the text in one message. The quoted text in an
   approval question ends with `attached: <absolute png path>`.
