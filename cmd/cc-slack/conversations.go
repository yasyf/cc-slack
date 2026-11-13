package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/yasyf/cc-interact/store"
)

var conversationsSchema = store.Schema{DDL: `
CREATE TABLE conversations (
  channel_id TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
`}

type conversations struct {
	st  *store.Store
	mu  sync.Mutex
	ids map[string]bool
}

type conversationWatches struct {
	channelID string
	watches   []watch
}

func conversationsPath() string { return filepath.Join(appPaths().StateDir(), "conversations.db") }

func openConversations(ctx context.Context, path string) (*conversations, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("conversations dir: %w", err)
	}
	st, err := store.Open(ctx, path, conversationsSchema, store.WithUnsupportedSchema(store.ArchiveUnsupportedSchema))
	if err != nil {
		return nil, fmt.Errorf("open conversations %s: %w", path, err)
	}
	rows, err := st.DB().QueryContext(ctx, `SELECT channel_id FROM conversations`)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("load conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	c := &conversations{st: st, ids: map[string]bool{}}
	for rows.Next() {
		var channelID string
		if err := rows.Scan(&channelID); err != nil {
			_ = st.Close()
			return nil, fmt.Errorf("scan a conversation: %w", err)
		}
		c.ids[channelID] = true
	}
	if err := rows.Err(); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("iterate conversations: %w", err)
	}
	return c, nil
}

func (c *conversations) Close() error { return c.st.Close() }

func (c *conversations) add(ctx context.Context, channelID, name string, at time.Time) error {
	if _, err := c.st.DB().ExecContext(ctx, `INSERT INTO conversations(channel_id, name, created_at) VALUES (?, ?, ?)
		ON CONFLICT(channel_id) DO UPDATE SET name = excluded.name`, channelID, name, at.UnixMilli()); err != nil {
		return fmt.Errorf("keep conversation %s: %w", channelID, err)
	}
	c.mu.Lock()
	c.ids[channelID] = true
	c.mu.Unlock()
	return nil
}

func (c *conversations) find(ctx context.Context, name string) (string, bool, error) {
	var channelID string
	err := c.st.DB().QueryRowContext(ctx, `SELECT channel_id FROM conversations WHERE name = ?`, name).Scan(&channelID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("find conversation %s: %w", name, err)
	}
	return channelID, true, nil
}

func (c *conversations) has(channelID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ids[channelID]
}

func (c *conversations) is(id identity, channelID string) bool {
	return channelID == id.DMChannelID || c.has(channelID)
}

func (c *conversations) of(id identity, channelID string) identity {
	if c.has(channelID) {
		return id.in(channelID)
	}
	return id
}

type notConversationError struct {
	channelID, dmChannelID string
}

func (e notConversationError) Error() string {
	return fmt.Sprintf("%s is not one of your conversations with the app: your DM %s, or a private channel `cc-slack channel-create --private` made", e.channelID, e.dmChannelID)
}

func (a *app) conversation(ctx context.Context, db *sql.DB, channelID string) (identity, error) {
	id, err := loadIdentity(ctx, db)
	if err != nil {
		return identity{}, err
	}
	if channelID == "" || a.convs.is(id, channelID) {
		return a.convs.of(id, channelID), nil
	}
	return identity{}, notConversationError{channelID: channelID, dmChannelID: id.DMChannelID}
}

func (c *conversations) all(id identity) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{id.DMChannelID}, slices.Sorted(maps.Keys(c.ids))...)
}

func (c *conversations) split(id identity, watches []watch) []conversationWatches {
	channels := c.all(id)
	split := make([]conversationWatches, len(channels))
	at := make(map[string]int, len(channels))
	for i, channelID := range channels {
		split[i].channelID, at[channelID] = channelID, i
	}
	for _, w := range watches {
		i, own := at[w.ChannelID]
		if !own {
			i = 0
		}
		split[i].watches = append(split[i].watches, w)
	}
	return split
}
