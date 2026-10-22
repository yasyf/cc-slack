package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"time"

	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/store"
)

var storeSchema = store.Schema{DDL: `
CREATE TABLE identity (
  id            INTEGER PRIMARY KEY CHECK (id = 1),
  user_id       TEXT NOT NULL,
  email         TEXT NOT NULL,
  real_name     TEXT NOT NULL,
  first_name    TEXT NOT NULL,
  display_name  TEXT NOT NULL,
  handle        TEXT NOT NULL,
  image_512     TEXT NOT NULL,
  avatar_url    TEXT NOT NULL,
  avatar_key_id TEXT NOT NULL,
  avatar_secret TEXT NOT NULL,
  team_id       TEXT NOT NULL,
  team_url      TEXT NOT NULL,
  bot_user_id   TEXT NOT NULL,
  bot_id        TEXT NOT NULL,
  token         TEXT NOT NULL,
  dm_channel_id TEXT NOT NULL,
  socket_url    TEXT NOT NULL,
  repo_root     TEXT NOT NULL,
  orca_path     TEXT NOT NULL,
  capt_hook_path TEXT NOT NULL,
  fetched_at    INTEGER NOT NULL,
  config        TEXT NOT NULL
);
CREATE TABLE watches (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  subject_id     TEXT NOT NULL REFERENCES subjects(id),
  owner_agent_id TEXT NOT NULL,
  kind           TEXT NOT NULL CHECK (kind IN ('thread', 'channel', 'dm')),
  channel_id     TEXT NOT NULL,
  thread_ts      TEXT NOT NULL,
  status         TEXT NOT NULL CHECK (status IN ('active', 'stopped', 'orphaned', 'error')),
  error          TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL
);
CREATE INDEX idx_watches_status ON watches(status, channel_id, thread_ts);
CREATE TABLE cursors (
  channel_id       TEXT NOT NULL,
  thread_ts        TEXT NOT NULL,
  oldest_ts        TEXT NOT NULL,
  next_poll_at     INTEGER NOT NULL,
  backoff_s        INTEGER NOT NULL DEFAULT 0,
  last_activity_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (channel_id, thread_ts)
);
CREATE TABLE shortcuts (
  channel_id    TEXT NOT NULL,
  message_ts    TEXT NOT NULL,
  dm_ts         TEXT NOT NULL,
  status        TEXT NOT NULL CHECK (status IN ('queued', 'opened', 'failed')),
  worktree_id   TEXT NOT NULL DEFAULT '',
  worktree_path TEXT NOT NULL DEFAULT '',
  error         TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  PRIMARY KEY (channel_id, message_ts)
);
CREATE TABLE posts (
  channel_id TEXT NOT NULL,
  ts         TEXT NOT NULL,
  PRIMARY KEY (channel_id, ts)
);
CREATE TABLE receipts (
  ticket_id   TEXT PRIMARY KEY,
  channel_id  TEXT NOT NULL DEFAULT '',
  ts          TEXT NOT NULL DEFAULT '',
  permalink   TEXT NOT NULL DEFAULT '',
  watch_id    INTEGER NOT NULL DEFAULT 0,
  watch_error TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL
);
`}

var (
	processStarted  = time.Now()
	errStaleProcess = errors.New("this cc-slack process is older than its state")
)

func statePath() string { return store.Path(appPaths()) }

func archiveStaleState(ctx context.Context) error {
	st, err := store.Open(ctx, statePath(), storeSchema, store.WithUnsupportedSchema(store.ArchiveUnsupportedSchema))
	if err != nil {
		return err
	}
	return st.Close()
}

func requireCurrentState(ensure func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		path := statePath()
		var info syscall.Stat_t
		err := syscall.Stat(path, &info)
		if errors.Is(err, fs.ErrNotExist) {
			return ensure(ctx)
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		st, err := store.Open(ctx, path, storeSchema)
		if errors.Is(err, store.ErrUnsupportedSchema) {
			build, release, herr := incumbentBinary(ctx)
			if herr != nil {
				return herr
			}
			if build != "" {
				return fmt.Errorf("%w: daemon release %s wrote %s in a schema this build cannot read", daemon.ErrIncumbentNewer, release, path)
			}
			if born := time.Unix(info.Birthtimespec.Unix()); born.After(processStarted) {
				return fmt.Errorf("%w: another cc-slack build created %s at %s, after this cc-slack %s process started at %s; restart the session to pick up the current build",
					errStaleProcess, path, born.Format(time.RFC3339), appVersion, processStarted.Format(time.RFC3339))
			}
			return fmt.Errorf("%w: %s predates the schema cc-slack %s reads, and `cc-slack login` archives it", errNoIdentity, path, appVersion)
		}
		if err != nil {
			return err
		}
		if err := st.Close(); err != nil {
			return err
		}
		return ensure(ctx)
	}
}
