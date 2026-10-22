package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	helloType      = "hello"
	caughtUpType   = "caught_up"
	nudgeRetention = 2 * time.Hour
	replayMargin   = 15 * time.Minute
)

type helloFrame struct {
	Type  string `json:"type"`
	Since string `json:"since,omitempty"`
}

func replayPath() string { return filepath.Join(appPaths().StateDir(), "nudge-seq") }

type replayCursor struct {
	path string

	mu       sync.Mutex
	saved    string
	received string
	held     bool
}

func loadReplayCursor(path string) (*replayCursor, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read nudge seq: %w", err)
	}
	seq := strings.TrimSpace(string(data))
	return &replayCursor{path: path, saved: seq, received: seq}, nil
}

func (c *replayCursor) hello(now time.Time) helloFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held = true
	millis, _, _ := strings.Cut(c.saved, "-")
	at, err := strconv.ParseInt(millis, 10, 64)
	if err != nil || now.Sub(time.UnixMilli(at)) > nudgeRetention-replayMargin {
		return helloFrame{Type: helloType}
	}
	return helloFrame{Type: helloType, Since: c.saved}
}

func (c *replayCursor) nudged(seq string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.held && seq > c.received {
		c.received = seq
	}
}

func (c *replayCursor) caughtUp(seq string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held = false
	if seq > c.received {
		c.received = seq
	}
}

func (c *replayCursor) latest() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.received
}

func (c *replayCursor) save(seq string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if seq <= c.saved {
		return nil
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(seq), 0o600); err != nil {
		return fmt.Errorf("write nudge seq: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return fmt.Errorf("save nudge seq: %w", err)
	}
	c.saved = seq
	return nil
}

func (a *app) hello() ([]byte, error) {
	frame := a.replay.hello(a.now())
	if frame.Since == "" {
		a.poller.reconcileAll()
	}
	return json.Marshal(frame)
}
