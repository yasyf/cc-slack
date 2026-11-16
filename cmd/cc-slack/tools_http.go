package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	toolUseMeta    = "claudecode/toolUseId"
)

var metaSeen sync.Once

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
		Handler:           toolsHandler(s, logger),
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

func toolsHandler(s *daemon.Server, logger *log.Logger) http.Handler {
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
			if body, err = ticketed(body, logger); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			line.Reset()
			if err := json.Compact(&line, body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
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

func ticketsDir() (string, error) {
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, binaryName, "tickets"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", binaryName, "tickets"), nil
}

func toolUseID(meta map[string]json.RawMessage) (string, error) {
	var use string
	raw, ok := meta[toolUseMeta]
	if !ok {
		return "", nil
	}
	return use, json.Unmarshal(raw, &use)
}

func ticketed(body []byte, logger *log.Logger) ([]byte, error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	var params struct {
		Name      string                     `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
		Meta      map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(req["params"], &params); err != nil {
		return nil, err
	}
	use, err := toolUseID(params.Meta)
	if err != nil {
		return nil, err
	}
	metaSeen.Do(func() {
		logger.Printf("tools/call _meta keys %v, %s %q", slices.Sorted(maps.Keys(params.Meta)), toolUseMeta, use)
	})
	if use == "" || strings.ContainsAny(use, "/\\") || params.Name == "slack_revoke" {
		return body, nil
	}
	dir, err := ticketsDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, use+".json")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the name is a tool use id with no path separator, under cc-slack's own state.
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if params.Arguments == nil {
		params.Arguments = map[string]json.RawMessage{}
	}
	delete(params.Arguments, "ticket_unhooked")
	_, typed := params.Arguments["ticket_id"]
	switch {
	case err == nil:
		_ = os.Remove(path)
		for name := range params.Arguments {
			if strings.HasPrefix(name, "ticket_") {
				delete(params.Arguments, name)
			}
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("ticket file %s: %w", path, err)
		}
		delete(fields, "session")
		maps.Copy(params.Arguments, fields)
	case !typed:
		if params.Arguments["ticket_unhooked"], err = json.Marshal(params.Name + " " + use); err != nil {
			return nil, err
		}
	}
	var whole map[string]json.RawMessage
	if err := json.Unmarshal(req["params"], &whole); err != nil {
		return nil, err
	}
	if whole["arguments"], err = json.Marshal(params.Arguments); err != nil {
		return nil, err
	}
	if req["params"], err = json.Marshal(whole); err != nil {
		return nil, err
	}
	return json.Marshal(req)
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
