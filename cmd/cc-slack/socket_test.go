package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type socketServer struct {
	t       *testing.T
	srv     *httptest.Server
	refuse  bool
	mu      sync.Mutex
	headers []http.Header
	pings   int
	hellos  []string
	conns   chan *websocket.Conn
}

func newSocketServer(t *testing.T, refuse bool) *socketServer {
	t.Helper()
	s := &socketServer{t: t, refuse: refuse, conns: make(chan *websocket.Conn, 8)}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *socketServer) url() string { return "ws" + strings.TrimPrefix(s.srv.URL, "http") + "/live" }

func (s *socketServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.headers = append(s.headers, r.Header.Clone())
	s.mu.Unlock()
	if s.refuse {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.t.Errorf("accept: %v", err)
		return
	}
	s.conns <- conn
	for {
		_, frame, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		s.mu.Lock()
		if string(frame) == pingFrame {
			s.pings++
		} else {
			s.hellos = append(s.hellos, string(frame))
		}
		s.mu.Unlock()
	}
}

func (s *socketServer) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.headers)
}

func (s *socketServer) pingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pings
}

func (s *socketServer) helloFrames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.hellos)
}

func (s *socketServer) header(i int) http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headers[i]
}

type socketRun struct {
	client   *socketClient
	frames   chan string
	mu       sync.Mutex
	connects int
}

func (r *socketRun) connectCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.connects
}

func startSocket(t *testing.T, f *fixture, url string) *socketRun {
	t.Helper()
	f.id.SocketURL = url
	if err := saveIdentity(f.ctx, f.db(), f.id); err != nil {
		t.Fatal(err)
	}
	return startSocketWithoutIdentity(t, f)
}

func startSocketWithoutIdentity(t *testing.T, f *fixture) *socketRun {
	t.Helper()
	r := &socketRun{client: f.app.socket, frames: make(chan string, 8)}
	c := r.client
	c.plane = f.plane
	c.pingEvery = 20 * time.Millisecond
	c.minBackoff = 10 * time.Millisecond
	c.maxBackoff = 40 * time.Millisecond
	c.jitter = func() float64 { return 0 }
	c.log.SetOutput(testWriter{t})
	c.hello = func() ([]byte, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.connects++
		return fmt.Appendf(nil, `{"type":"hello","since":"%d"}`, r.connects), nil
	}
	c.onFrame = func(_ context.Context, id identity, frame []byte) error {
		if id.UserID != testUser {
			t.Errorf("frame handed over for %s", id.UserID)
		}
		r.frames <- string(frame)
		return nil
	}
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return r
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSocketAuthenticatesPingsDeliversAndReconnects(t *testing.T) {
	f := newFixture(t)
	s := newSocketServer(t, false)
	r := startSocket(t, f, s.url())

	first := <-s.conns
	if got := s.header(0).Get("Authorization"); got != "Bearer xoxb-test" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := s.header(0).Get(userHeader); got != testUser {
		t.Fatalf("%s = %q", userHeader, got)
	}
	eventually(t, "connected state", func() bool { return r.client.status() == socketConnected })
	eventually(t, "two pings", func() bool { return s.pingCount() >= 2 })

	if err := first.Write(f.ctx, websocket.MessageText, []byte(`{"event_type":"cc_slack_shortcut","event_payload":{}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-r.frames:
		if got != `{"event_type":"cc_slack_shortcut","event_payload":{}}` {
			t.Fatalf("frame = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("frame never reached the handler")
	}

	_ = first.Close(websocket.StatusGoingAway, "api gateway hard close")
	<-s.conns
	eventually(t, "a hello on each connection", func() bool { return len(s.helloFrames()) == 2 })
	if got := s.helloFrames(); !slices.Equal(got, []string{`{"type":"hello","since":"1"}`, `{"type":"hello","since":"2"}`}) {
		t.Fatalf("hello frames = %q, want one fresh hello per connection", got)
	}
	if got := s.header(1).Get("Authorization"); got != "Bearer xoxb-test" {
		t.Fatalf("reconnect Authorization = %q", got)
	}
}

func TestSocketBacksOffWhenRefused(t *testing.T) {
	f := newFixture(t)
	s := newSocketServer(t, true)
	r := startSocket(t, f, s.url())

	eventually(t, "three refused dials", func() bool { return s.connections() >= 3 })
	if n := r.connectCount(); n != 0 {
		t.Fatalf("said hello %d times without a connection", n)
	}
	eventually(t, "the refused dial in status", func() bool {
		got := r.client.status()
		return strings.HasPrefix(got, "disconnected: ") && strings.Contains(got, "401")
	})
}

func TestSocketRestartDialsTheNewURL(t *testing.T) {
	f := newFixture(t)
	old := newSocketServer(t, false)
	r := startSocket(t, f, old.url())
	<-old.conns
	eventually(t, "first connection", func() bool { return r.connectCount() == 1 })

	next := newSocketServer(t, false)
	f.id.SocketURL = next.url()
	if err := saveIdentity(f.ctx, f.db(), f.id); err != nil {
		t.Fatal(err)
	}
	r.client.restart()
	select {
	case <-next.conns:
	case <-time.After(5 * time.Second):
		t.Fatal("restart never dialed the new URL")
	}
}

func TestSocketConnectsOnceLoginLands(t *testing.T) {
	f := newFixture(t)
	s := newSocketServer(t, false)
	id := f.id
	if _, err := f.db().ExecContext(f.ctx, `DELETE FROM identity`); err != nil {
		t.Fatal(err)
	}
	r := startSocketWithoutIdentity(t, f)
	if got := r.client.status(); got != socketNoIdentity {
		t.Fatalf("status = %q before login, want %q", got, socketNoIdentity)
	}

	id.SocketURL = s.url()
	if err := saveIdentity(f.ctx, f.db(), id); err != nil {
		t.Fatal(err)
	}
	r.client.restart()
	select {
	case <-s.conns:
	case <-time.After(5 * time.Second):
		t.Fatal("login never woke the socket")
	}
}
