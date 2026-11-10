# Set up the session and fall back

## Set up the session

Invoke the binary as `"${CLAUDE_PLUGIN_ROOT}/bin/cc-slack"`. That launcher
runs the release this plugin version pins, downloading it on first use;
`SessionStart` registers this session through the same launcher. A build
older than the running daemon hands each command to the daemon's own
executable, so it never needs a reconnect. A send, reply, watch, or
`dm-status` from a window that `SessionStart` never registered registers it
on the spot.

Install the plugin at user scope (`claude plugin install cc-slack@cc-slack
--scope user`) so lanes mount the bot tools at startup.

On first use, run `"${CLAUDE_PLUGIN_ROOT}/bin/cc-slack" login`, then
`"${CLAUDE_PLUGIN_ROOT}/bin/cc-slack" whoami`. The `login` command reads the
cc-slack config (`--config`, then `$CC_SLACK_CONFIG`, then the checkout's
`.claude/cc-slack.toml`, then `~/.config/cc-slack/config.toml`), takes the
shared bot token and socket URL from the config's AWS Systems Manager (SSM)
parameters, signed in as the AWS profile `[ssm] profile` names or else the
shell's, or from `CC_SLACK_BOT_TOKEN` and `CC_SLACK_SOCKET_URL`, and
resolves the user's Slack identity by git email. Check the returned
identity before sending. `capt-hook` must be on PATH; `login` records its
absolute path for the daemon. After a config change, run
`"${CLAUDE_PLUGIN_ROOT}/bin/cc-slack" reload`: it hands the daemon the new
config and keeps the token and socket URL the last `login` read, so it needs
no AWS access. Rerun `login` only when `[ssm]` names a new parameter. When the store schema changes, every command asks for `login`,
which archives the old `state.db`; re-run `login` and re-arm watches.

For the "Open in Claude" shortcut, run `"${CLAUDE_PLUGIN_ROOT}/bin/cc-slack" login`
from the main checkout registered in `orca repo list --json`, or pass
`--repo <checkout>` with that exact `path`. Do not use a Claude or orca
worktree. Set the workspace base once so shortcuts branch from a fresh
default branch:

```bash
orca repo set-base-ref --repo path:<checkout> --ref origin/main
```

Run `"${CLAUDE_PLUGIN_ROOT}/bin/cc-slack" setup-channels` to check the
first-run offer. It consults the setup marker file and managed settings.
When `offer` is true, use `AskUserQuestion` to offer channel approval. Explain
that it requires an admin prompt and adds `cc-slack@cc-slack` to the
machine-wide allowlist.

On acceptance, run
`"${CLAUDE_PLUGIN_ROOT}/bin/cc-slack" setup-channels --apply`; on decline,
run `"${CLAUDE_PLUGIN_ROOT}/bin/cc-slack" setup-channels --decline`. These
commands record the decision in the marker file. When `offer` is false,
skip the question. Setup arms no Monitor; a session arms one only when
it subscribes.

## Fall back when the bot cannot act

Use the matching tool below for reads the bot cannot make or writes in a
personal 1:1 DM it cannot join. A private-channel write the bot cannot
invite itself to waits for the user's invite.
If the user declines, leave the write unsent. Prefix every MCP post with
`_(<Firstname>'s Claude)_` and quote that exact text in the approval question.

| Failure | Next step |
|---|---|
| Post or reply fails | `mcp__slack__slack_send_message` |
| Thread read fails | `mcp__slack__slack_get_thread` |
| Channel history read fails | `mcp__slack__slack_conversations_history` |
| Reaction fails | `mcp__slack__slack_add_reaction` |
| Private channel excludes the bot and the self-invite failed | Report the cause the refusal names, then retry the same write once it is fixed; never ask the user to `/invite` the app |
| Personal 1:1 DM excludes the bot | Use the matching Slack MCP tool above |
| No identity | Run `"${CLAUDE_PLUGIN_ROOT}/bin/cc-slack" login` and `whoami` |
| The daemon runs a newer build and its executable is unreadable | Run `/mcp` and reconnect cc-slack, or start a new session |
