package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yasyf/cc-interact/procs"
	"github.com/yasyf/cc-interact/store"
)

var holdsSchema = store.Schema{DDL: `
CREATE TABLE delivery_holds (
  subject_id TEXT PRIMARY KEY,
  pid        INTEGER NOT NULL,
  started_at INTEGER NOT NULL
);
CREATE TABLE owner_messages (
  channel_id     TEXT NOT NULL,
  ts             TEXT NOT NULL,
  message        TEXT NOT NULL,
  picked_at      INTEGER NOT NULL,
  holder         TEXT NOT NULL,
  transcript     TEXT NOT NULL,
  transcript_at  INTEGER NOT NULL,
  confirmed_at   INTEGER NOT NULL DEFAULT 0,
  redelivered    INTEGER NOT NULL DEFAULT 0,
  redelivered_at INTEGER NOT NULL DEFAULT 0,
  reacted_at     INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (channel_id, ts)
);
CREATE TABLE held_acks (
  channel_id TEXT NOT NULL,
  ts         TEXT NOT NULL,
  due_at     INTEGER NOT NULL,
  held       TEXT NOT NULL,
  PRIMARY KEY (channel_id, ts)
);
CREATE TABLE compactions (
  subject_id TEXT PRIMARY KEY,
  transcript TEXT NOT NULL,
  offset     INTEGER NOT NULL
);
`}

type deliveryHolds struct {
	st *store.Store
}

func deliveryHoldsPath() string { return filepath.Join(appPaths().StateDir(), "delivery-holds.db") }

func openDeliveryHolds(ctx context.Context, path string) (*deliveryHolds, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("delivery holds dir: %w", err)
	}
	st, err := store.Open(ctx, path, holdsSchema, store.WithUnsupportedSchema(store.ArchiveUnsupportedSchema))
	if err != nil {
		return nil, fmt.Errorf("open delivery holds %s: %w", path, err)
	}
	return &deliveryHolds{st: st}, nil
}

func (h *deliveryHolds) Close() error { return h.st.Close() }

func (h *deliveryHolds) hold(ctx context.Context, subjectID string, pid int) error {
	started, err := procs.StartTime(pid)
	if err != nil {
		return fmt.Errorf("hold delivery for stream %d: %w", pid, err)
	}
	if _, err := h.st.DB().ExecContext(ctx, `INSERT INTO delivery_holds(subject_id, pid, started_at) VALUES (?, ?, ?)
ON CONFLICT(subject_id) DO UPDATE SET pid = excluded.pid, started_at = excluded.started_at`, subjectID, pid, started); err != nil {
		return fmt.Errorf("hold delivery for %s: %w", subjectID, err)
	}
	return nil
}

func (h *deliveryHolds) streamed(ctx context.Context, subjectID string) (bool, error) {
	var (
		pid     int
		started int64
	)
	err := h.st.DB().QueryRowContext(ctx, `SELECT pid, started_at FROM delivery_holds WHERE subject_id = ?`, subjectID).Scan(&pid, &started)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the delivery hold for %s: %w", subjectID, err)
	}
	now, err := procs.StartTime(pid)
	return err == nil && now == started, nil
}
