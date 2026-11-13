package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yasyf/cc-interact/store"
)

var errLiveCardsStore = errors.New("live card store")

var liveCardsSchema = store.Schema{DDL: `
CREATE TABLE live_card_threads (
  key TEXT PRIMARY KEY,
  payload BLOB NOT NULL
);
CREATE TABLE live_card_refs (
  key TEXT PRIMARY KEY,
  payload BLOB NOT NULL
);
CREATE TABLE live_card_import (
  id INTEGER PRIMARY KEY CHECK (id = 1)
);
`}

type liveTable struct {
	name   string
	read   string
	remove string
	write  string
}

var liveThreadRows = liveTable{
	name:   "live_card_threads",
	read:   "SELECT key, payload FROM live_card_threads",
	remove: "DELETE FROM live_card_threads WHERE key = ?",
	write:  "INSERT INTO live_card_threads(key, payload) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET payload = excluded.payload",
}

var liveRefRows = liveTable{
	name:   "live_card_refs",
	read:   "SELECT key, payload FROM live_card_refs",
	remove: "DELETE FROM live_card_refs WHERE key = ?",
	write:  "INSERT INTO live_card_refs(key, payload) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET payload = excluded.payload",
}

func (l *liveCards) load() (err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.st != nil {
		if err := l.st.Close(); err != nil {
			return fmt.Errorf("close live card store: %w", err)
		}
		l.st = nil
	}
	st, err := store.Open(context.Background(), l.path, liveCardsSchema)
	if err != nil {
		return fmt.Errorf("%w %s: %w", errLiveCardsStore, l.path, err)
	}
	defer func() {
		if err != nil {
			_ = st.Close()
			l.st = nil
		}
	}()
	l.st = st
	if err := l.importJSON(); err != nil {
		return err
	}
	threads := map[string]*liveThread{}
	if err := readLiveRows(st.DB(), liveThreadRows, func(key string, data []byte) error {
		var t liveThread
		if err := json.Unmarshal(data, &t); err != nil {
			return err
		}
		if threadKey(t.Channel, t.Thread) != key {
			return fmt.Errorf("thread key %q does not match payload", key)
		}
		threads[key] = &t
		return nil
	}); err != nil {
		return err
	}
	refs := map[string]refState{}
	if err := readLiveRows(st.DB(), liveRefRows, func(key string, data []byte) error {
		var ref refState
		if err := json.Unmarshal(data, &ref); err != nil {
			return err
		}
		refs[key] = ref
		return nil
	}); err != nil {
		return err
	}
	l.threads, l.refs = threads, refs
	l.dirtyThreads, l.dirtyRefs = map[string]bool{}, map[string]bool{}
	for _, key := range l.prune() {
		l.dirtyThreads[key] = true
	}
	return l.persist()
}

func readLiveRows(db *sql.DB, table liveTable, decode func(string, []byte) error) error {
	rows, err := db.QueryContext(context.Background(), table.read)
	if err != nil {
		return fmt.Errorf("read %s: %w", table.name, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		var data []byte
		if err := rows.Scan(&key, &data); err != nil {
			return fmt.Errorf("read %s row: %w", table.name, err)
		}
		if err := decode(key, data); err != nil {
			return fmt.Errorf("decode %s %s: %w", table.name, key, err)
		}
	}
	return rows.Err()
}

func (l *liveCards) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.st == nil {
		return nil
	}
	err := l.st.Close()
	l.st = nil
	return err
}

func (l *liveCards) importJSON() error {
	var done int
	if err := l.st.DB().QueryRowContext(context.Background(), "SELECT count(*) FROM live_card_import").Scan(&done); err != nil {
		return fmt.Errorf("read live card import marker: %w", err)
	}
	paths := []string{filepath.Join(filepath.Dir(l.path), "live-cards.json"), filepath.Join(filepath.Dir(l.path), "live-refs.json")}
	if done == 0 {
		if err := l.importLegacyRows(paths); err != nil {
			return err
		}
	}
	return retireLegacyLiveFiles(paths)
}

func (l *liveCards) importLegacyRows(paths []string) error {
	var populated int
	if err := l.st.DB().QueryRowContext(context.Background(), "SELECT (SELECT count(*) FROM live_card_threads) + (SELECT count(*) FROM live_card_refs)").Scan(&populated); err != nil {
		return fmt.Errorf("inspect live card import target: %w", err)
	}
	if populated != 0 {
		return errors.New("live card store has rows without an import marker")
	}
	var threads []*liveThread
	var refs map[string]refState
	for i, target := range []any{&threads, &refs} {
		data, err := os.ReadFile(paths[i])
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read legacy live card state %s: %w", paths[i], err)
		}
		if err := json.Unmarshal(data, target); err != nil {
			return fmt.Errorf("decode legacy live card state %s: %w", paths[i], err)
		}
	}
	tx, err := l.st.DB().BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("begin live card import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, t := range threads {
		if t == nil {
			return errors.New("legacy live card state contains a null thread")
		}
		if err := writeLiveRow(tx, liveThreadRows, threadKey(t.Channel, t.Thread), t, true); err != nil {
			return err
		}
	}
	for key, ref := range refs {
		if err := writeLiveRow(tx, liveRefRows, key, ref, true); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(context.Background(), "INSERT INTO live_card_import(id) VALUES (1)"); err != nil {
		return fmt.Errorf("mark live card import: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit live card import: %w", err)
	}
	return nil
}

func retireLegacyLiveFiles(paths []string) error {
	for _, path := range paths {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect legacy live card state %s: %w", path, err)
		}
		backup := path + ".imported"
		if _, err := os.Stat(backup); err == nil {
			return fmt.Errorf("retire legacy live card state: backup already exists: %s", backup)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect legacy live card backup %s: %w", backup, err)
		}
		if err := os.Rename(path, backup); err != nil {
			return fmt.Errorf("retire legacy live card state %s: %w", path, err)
		}
	}
	return nil
}

func writeLiveRow(tx *sql.Tx, table liveTable, key string, value any, exists bool) error {
	if !exists {
		if _, err := tx.ExecContext(context.Background(), table.remove, key); err != nil {
			return fmt.Errorf("delete %s %s: %w", table.name, key, err)
		}
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s %s: %w", table.name, key, err)
	}
	if _, err := tx.ExecContext(context.Background(), table.write, key, data); err != nil {
		return fmt.Errorf("write %s %s: %w", table.name, key, err)
	}
	return nil
}

func (l *liveCards) save(threads, refs []string) {
	for _, key := range threads {
		l.dirtyThreads[key] = true
	}
	for _, key := range refs {
		l.dirtyRefs[key] = true
	}
	if err := l.persist(); err != nil {
		l.log.Printf("save live cards: %v", err)
	}
}

func (l *liveCards) persist() error {
	if len(l.dirtyThreads)+len(l.dirtyRefs) == 0 {
		return nil
	}
	tx, err := l.st.DB().BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("begin live card update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for key := range l.dirtyThreads {
		t, exists := l.threads[key]
		if err := writeLiveRow(tx, liveThreadRows, key, t, exists); err != nil {
			return err
		}
	}
	for key := range l.dirtyRefs {
		ref, exists := l.refs[key]
		if err := writeLiveRow(tx, liveRefRows, key, ref, exists); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit live card update: %w", err)
	}
	clear(l.dirtyThreads)
	clear(l.dirtyRefs)
	return nil
}
