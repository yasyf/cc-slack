package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/yasyf/cc-interact/daemon"
)

const slowOp = 2 * time.Second

type traceCtxKey struct{}

type callSpan struct {
	calls int
	took  time.Duration
}

type callTrace struct {
	mu    sync.Mutex
	spans map[string]*callSpan
	order []string
}

func (t *callTrace) add(name string, took time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	span, seen := t.spans[name]
	if !seen {
		span = &callSpan{}
		t.spans[name] = span
		t.order = append(t.order, name)
	}
	span.calls++
	span.took += took
}

func (t *callTrace) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.order) == 0 {
		return "no outbound calls"
	}
	parts := make([]string, 0, len(t.order))
	for _, name := range t.order {
		span := t.spans[name]
		parts = append(parts, fmt.Sprintf("%s x%d %s", name, span.calls, span.took.Round(10*time.Millisecond)))
	}
	return strings.Join(parts, ", ")
}

func traced(ctx context.Context, name string, took time.Duration) {
	if t, ok := ctx.Value(traceCtxKey{}).(*callTrace); ok {
		t.add(name, took)
	}
}

func timedOp(op daemon.Op, h daemon.HandlerFunc) daemon.HandlerFunc {
	return func(hc daemon.HandlerCtx) daemon.Reply {
		trace := &callTrace{spans: map[string]*callSpan{}}
		hc.Ctx = context.WithValue(hc.Ctx, traceCtxKey{}, trace)
		began := time.Now()
		reply := h(hc)
		if took := time.Since(began); took >= slowOp {
			log.Printf("slow op %s: %s, ok=%t; %s", op, took.Round(10*time.Millisecond), reply.OK, trace)
		}
		return reply
	}
}

type timedDoer struct{ next doer }

func (d timedDoer) Do(req *http.Request) (*http.Response, error) {
	began := time.Now()
	resp, err := d.next.Do(req) //nolint:gosec // G704: the caller built the request for Slack or the doc-cards URL the config names
	name := req.URL.Host + " " + path.Base(req.URL.Path)
	if strings.HasSuffix(req.URL.Host, "slack.com") {
		name = path.Base(req.URL.Path)
	}
	traced(req.Context(), name, time.Since(began))
	return resp, err
}
