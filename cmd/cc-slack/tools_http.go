package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yasyf/cc-interact/channel"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/procs"
	"golang.org/x/sys/unix"
)

const (
	toolsAddr      = "127.0.0.1:7378"
	toolsPIDHeader = "X-Claude-Pid"
	toolsEnvHeader = "X-Cc-Slack-Env-"
	toolsBodyBytes = 4 << 20
	toolsBindRetry = time.Second
	toolsStageWait = 10 * time.Second
)

type origin struct {
	env []string
	dir string
}

type window struct {
	pid     int
	session string
	cwd     string
	from    origin
}

type registryEntry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

func serveTools(ctx context.Context, s *daemon.Server) {
	logger := log.New(os.Stderr, "["+binaryName+" tools] ", log.LstdFlags)
	var ln net.Listener
	for {
		var err error
		if ln, err = (&net.ListenConfig{}).Listen(ctx, "tcp", toolsAddr); err == nil {
			break
		}
		logger.Printf("bind %s: %v; retrying", toolsAddr, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(toolsBindRetry):
		}
	}
	srv := &http.Server{
		Handler:           toolsHandler(s),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		logger.Printf("serve %s: %v", toolsAddr, err)
	}
}

func toolsHandler(s *daemon.Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(w, binaryName+" tools answer no browser", http.StatusForbidden)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, toolsBodyBytes))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		var line bytes.Buffer
		if err := errors.Join(json.Unmarshal(body, &msg), json.Compact(&line, body)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var tools []channel.Tool
		if msg.Method == "tools/call" {
			win, err := callerWindow(r)
			if err != nil {
				_, _ = w.Write(mustJSON(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
					"isError": true, "content": []any{map[string]any{"type": "text", "text": err.Error()}},
				}}))
				return
			}
			tools = sessionTools(hostedCaller(s, win), win.session)
		} else {
			tools = sessionTools(nil, "")
		}
		line.WriteByte('\n')
		var out bytes.Buffer
		srv := channel.NewServer(channel.ServerInfo{Name: binaryName, Version: appVersion}, tools)
		if err := srv.Serve(r.Context(), &line, &out); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		reply, err := rpcReply(out.Bytes(), msg.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(reply)
	})
}

func rpcReply(out, id json.RawMessage) ([]byte, error) {
	for line := range bytes.Lines(out) {
		var msg struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(line, &msg) == nil && bytes.Equal(msg.ID, id) {
			return line, nil
		}
	}
	return nil, fmt.Errorf("no reply to request %s", id)
}

func hostedCaller(s *daemon.Server, win window) caller {
	return func(ctx context.Context, op daemon.Op, body json.RawMessage) (daemon.Reply, error) {
		ctx, cancel := tapCallContext(ctx, op, body)
		defer cancel()
		if slices.Contains(attachmentWrites, op) {
			staged, cleanup, err := stageWithin(ctx, body)
			if err != nil {
				return daemon.Reply{}, err
			}
			defer cleanup()
			if body, _, err = keyedBodies(staged, time.Now()); err != nil {
				return daemon.Reply{}, err
			}
		}
		reply := s.Dispatch(ctx, daemon.Envelope{Op: op, Session: win.session, ClaudePID: win.pid, Scope: win.cwd, Body: body})
		if !reply.OK {
			return reply, errors.New(reply.Error)
		}
		afterPost(ctx, win.from, win.session, op, body, reply.Body)
		return reply, nil
	}
}

func callerWindow(r *http.Request) (window, error) {
	pid, err := strconv.Atoi(r.Header.Get(toolsPIDHeader))
	if err != nil {
		return window{}, fmt.Errorf("%s: %w", toolsPIDHeader, err)
	}
	env := os.Environ()
	configDir := ""
	for name, values := range r.Header {
		if variable, ok := strings.CutPrefix(name, toolsEnvHeader); ok && len(values) > 0 {
			variable = strings.ToUpper(variable)
			env = append(env, variable+"="+values[0])
			if variable == "CLAUDE_CONFIG_DIR" {
				configDir = values[0]
			}
		}
	}
	claude := procs.FindAncestor(pid, procs.IsClaude)
	entry, err := registeredWindow(configDir, claude)
	if err != nil {
		return window{}, fmt.Errorf("%s tools for pid %d: %w", binaryName, pid, err)
	}
	return window{pid: claude, session: entry.SessionID, cwd: entry.Cwd, from: origin{env: env, dir: entry.Cwd}}, nil
}

func registeredWindow(configDir string, pid int) (registryEntry, error) {
	if pid == 0 {
		return registryEntry{}, errors.New("no Claude Code process among its ancestors")
	}
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return registryEntry{}, err
		}
		configDir = filepath.Join(home, ".claude")
	}
	path := filepath.Join(configDir, "sessions", strconv.Itoa(pid)+".json")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path holds only a numeric pid under the Claude config dir.
	if err != nil {
		return registryEntry{}, fmt.Errorf("no Claude Code session registry entry: %w", err)
	}
	var entry registryEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return registryEntry{}, fmt.Errorf("decode the Claude Code session registry entry %s: %w", path, err)
	}
	if entry.PID != pid || entry.SessionID == "" || unix.Kill(pid, 0) != nil {
		return registryEntry{}, fmt.Errorf("claude pid %d is not a live window of this user", pid)
	}
	return entry, nil
}

type stagedBody struct {
	body    json.RawMessage
	cleanup func()
	err     error
}

func stageWithin(ctx context.Context, body json.RawMessage) (json.RawMessage, func(), error) {
	done := make(chan stagedBody, 1)
	go func() {
		staged, cleanup, err := stageAttachments(body)
		done <- stagedBody{staged, cleanup, err}
	}()
	select {
	case s := <-done:
		return s.body, s.cleanup, s.err
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-time.After(toolsStageWait):
		return nil, nil, fmt.Errorf("the %s daemon could not read an attachment within %s: a launchd daemon has no macOS privacy grant for folders such as ~/Documents, so copy the file outside them and attach the copy", binaryName, toolsStageWait)
	}
}
