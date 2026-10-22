package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/channel"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/procs"
	"github.com/yasyf/cc-interact/store"
	"github.com/yasyf/cc-interact/subject"
)

const channelConsumer = "channel"

var lifecycle = subject.Lifecycle{Initial: statusOpen, Closed: statusClosed}

type app struct {
	newAPI    func(token string) slackAPI
	user      userSession
	cache     *readCache
	replay    *replayCursor
	poller    *poller
	cards     *liveCards
	shortcuts *shortcutHandler
	socket    *socketClient
	captHook  commandRunner
	claudeCwd func(ctx context.Context, pid int) (string, error)
	avatar    avatarPublisher
	now       func() time.Time

	channelState func(hc daemon.HandlerCtx, subjectID string) daemon.ChannelState

	peers sync.Map
}

func jitter() float64 { return rand.Float64()*2 - 1 } //nolint:gosec // G404: schedule jitter, not a secret

func newApp(newAPI func(token string) slackAPI, alive func(pid int) bool, cache *readCache, replay *replayCursor) *app {
	avatars := newAvatarStore(&http.Client{Timeout: 30 * time.Second})
	shortcuts := &shortcutHandler{
		newAPI:        newAPI,
		avatar:        avatars.publish,
		orca:          execOrca,
		http:          &http.Client{Timeout: 10 * time.Second},
		now:           time.Now,
		readyWait:     orcaReadyWait,
		readyPoll:     orcaReadyPoll,
		createTimeout: orcaCreateTimeout,
		settleWait:    orcaSettleWait,
		settlePoll:    orcaSettlePoll,
		log:           log.New(os.Stderr, "["+binaryName+" shortcut] ", log.LstdFlags),
		inflight:      map[shortcutKey]bool{},
	}
	cards := &liveCards{
		path:    liveCardsPath(),
		client:  refClient{http: &http.Client{Timeout: 30 * time.Second}, githubURL: githubAPI, buildkiteURL: buildkiteAPI, now: time.Now},
		log:     log.New(os.Stderr, "["+binaryName+" cards] ", log.LstdFlags),
		now:     time.Now,
		newAPI:  newAPI,
		wakeCh:  make(chan struct{}, 1),
		threads: map[string]*liveThread{},
		refs:    map[string]refState{},
		hosts:   map[string]*refHost{},
	}
	p := &poller{
		newAPI:    newAPI,
		alive:     alive,
		now:       time.Now,
		cache:     cache,
		replay:    replay,
		shortcuts: shortcuts,
		cards:     cards,
		wakeCh:    make(chan struct{}, 1),
		paused:    map[string]time.Time{},
		pending:   map[cursorKey]uint64{},
		since:     map[cursorKey]string{},
		log:       log.New(os.Stderr, "["+binaryName+" poller] ", log.LstdFlags),
	}
	a := &app{
		newAPI:    newAPI,
		user:      mcpUserSession(),
		cache:     cache,
		replay:    replay,
		captHook:  execCommand,
		claudeCwd: claudeCwd,
		avatar:    avatars.publish,
		now:       time.Now,
		poller:    p,
		cards:     cards,
		shortcuts: shortcuts,
		socket: &socketClient{
			onFrame:    socketFrames(shortcuts, p, replay),
			pingEvery:  socketPingEvery,
			minBackoff: socketMinBackoff,
			maxBackoff: socketMaxBackoff,
			jitter:     jitter,
			wakeCh:     make(chan struct{}, 1),
			log:        log.New(os.Stderr, "["+binaryName+" socket] ", log.LstdFlags),
			state:      socketNoIdentity,
		},
	}
	a.socket.hello = a.hello
	return a
}

const waiterAgentType = "cc-slack:slack-waiter"

func agentGreeting(info agent.Info) string {
	if info.AgentType != waiterAgentType {
		return ""
	}
	return fmt.Sprintf("You are agent %s; owner=%s scopes slack_watch to you, and await with your agent id parks for directives.",
		info.AgentID, info.AgentID)
}

func subscribe(_ subject.Subject, info agent.Info) []string {
	return []string{ownedType(eventMessage, info.AgentID), ownedType(eventWatchError, info.AgentID)}
}

func (a *app) buildServer() (*daemon.Server, error) {
	c := channel.Connectivity{}
	spec, err := appSpec()
	if err != nil {
		return nil, err
	}
	s, err := daemon.New(daemon.Config{
		AppName:           binaryName,
		Paths:             appPaths(),
		Daemon:            spec,
		RuntimeBuild:      appVersion,
		ActiveStatuses:    []string{statusOpen},
		ScopeResolve:      func(context.Context, string) string { return scopeName },
		PresenceEventType: c.Type(),
		OnPresenceChange:  c.OnPresenceChange,
		BootReconcile: func(ctx context.Context, s *daemon.Server) error {
			if err := c.BootReconcile(ctx, s); err != nil {
				return err
			}
			a.poller.plane = s
			a.cards.plane = s
			if err := a.cards.load(); err != nil {
				return err
			}
			a.shortcuts.plane = s
			a.shortcuts.spawn = s.Background
			a.socket.plane = s
			s.Background(a.poller.run)
			s.Background(a.socket.run)
			s.Background(a.cards.run)
			return nil
		},
		AgentGreeting: agentGreeting,
		Subscribe:     subscribe,
		MuteConsumer:  channelConsumer,
		StoreSchema:   storeSchema,
	})
	if err != nil {
		return nil, err
	}
	a.register(s)
	return s, nil
}

func serve(ctx context.Context) error {
	cache, err := openReadCache(ctx, cachePath())
	if err != nil {
		return err
	}
	defer func() { _ = cache.Close() }()
	replay, err := loadReplayCursor(replayPath())
	if err != nil {
		return err
	}
	s, err := newApp(cachedSlackAPI(), procs.LiveClaude, cache, replay).buildServer()
	if err != nil {
		return err
	}
	err = s.Serve(ctx)
	if errors.Is(err, store.ErrUnsupportedSchema) {
		log.Printf("%s predates this build's schema; leaving it for `cc-slack login` to archive: %v", statePath(), err)
		return nil
	}
	return err
}

func cachedSlackAPI() func(token string) slackAPI {
	var mu sync.Mutex
	clients := map[string]slackAPI{}
	return func(token string) slackAPI {
		mu.Lock()
		defer mu.Unlock()
		if c, ok := clients[token]; ok {
			return c
		}
		c := newSlackAPI(token)
		clients[token] = c
		return c
	}
}
