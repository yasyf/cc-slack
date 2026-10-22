package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	socketPingEvery  = 60 * time.Second
	socketMinBackoff = time.Second
	socketMaxBackoff = 60 * time.Second
	userHeader       = "X-Cc-Slack-User"
	pingFrame        = "ping"
	pongFrame        = "pong"

	socketNoIdentity = "not logged in"
	socketConnecting = "connecting"
	socketConnected  = "connected"
)

type socketClient struct {
	plane      plane
	onFrame    func(ctx context.Context, id identity, frame []byte) error
	hello      func() ([]byte, error)
	pingEvery  time.Duration
	minBackoff time.Duration
	maxBackoff time.Duration
	jitter     func() float64
	wakeCh     chan struct{}
	log        *log.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	state  string
}

func (c *socketClient) restart() {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()
	select {
	case c.wakeCh <- struct{}{}:
	default:
	}
}

func (c *socketClient) status() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *socketClient) setState(state string) {
	c.mu.Lock()
	c.state = state
	c.mu.Unlock()
}

func (c *socketClient) run(ctx context.Context) {
	backoff := c.minBackoff
	for {
		id, err := loadIdentity(ctx, c.plane.DB())
		var wait time.Duration
		switch {
		case errors.Is(err, errNoIdentity):
			c.setState(socketNoIdentity)
		case err != nil:
			c.log.Printf("socket: %v", err)
			wait = backoff
		default:
			connected, err := c.connect(ctx, id)
			if ctx.Err() != nil {
				return
			}
			if connected {
				backoff = c.minBackoff
			}
			c.setState("disconnected: " + err.Error())
			wait = time.Duration(float64(backoff) * (1 + jitterFraction*c.jitter()))
			backoff = min(2*backoff, c.maxBackoff)
			c.log.Printf("socket: disconnected: %v; reconnecting in %s", err, wait.Round(time.Millisecond))
		}
		if !c.sleep(ctx, wait) {
			return
		}
	}
}

func (c *socketClient) sleep(ctx context.Context, wait time.Duration) bool {
	var due <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		due = timer.C
	}
	select {
	case <-ctx.Done():
		return false
	case <-c.wakeCh:
	case <-due:
	}
	return true
}

func (c *socketClient) connect(parent context.Context, id identity) (bool, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	c.mu.Lock()
	c.cancel = cancel
	c.state = socketConnecting
	c.mu.Unlock()
	conn, resp, err := websocket.Dial(ctx, id.SocketURL, &websocket.DialOptions{HTTPHeader: http.Header{
		"Authorization": {"Bearer " + id.Token},
		userHeader:      {id.UserID},
	}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	c.setState(socketConnected)
	c.log.Printf("socket: connected as %s", id.UserID)
	var pinger sync.WaitGroup
	pinger.Go(func() { c.ping(ctx, cancel, conn) })
	defer func() {
		cancel()
		pinger.Wait()
	}()
	hello, err := c.hello()
	if err != nil {
		return true, fmt.Errorf("hello: %w", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		return true, fmt.Errorf("hello: %w", err)
	}
	for {
		_, frame, err := conn.Read(ctx)
		if err != nil {
			return true, fmt.Errorf("read: %w", err)
		}
		if err := c.onFrame(ctx, id, frame); err != nil {
			c.log.Printf("socket frame: %v", err)
		}
	}
}

func (c *socketClient) ping(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn) {
	ticker := time.NewTicker(c.pingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := conn.Write(ctx, websocket.MessageText, []byte(pingFrame)); err != nil {
				c.log.Printf("socket ping: %v", err)
				cancel()
				return
			}
		}
	}
}
