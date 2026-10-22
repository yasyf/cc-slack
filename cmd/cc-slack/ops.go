package main

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/subject"
)

const (
	opSessionStart daemon.Op = "session-start"
	opLogin        daemon.Op = "login"
	opWhoami       daemon.Op = "whoami"
	opSend         daemon.Op = "send"
	opReply        daemon.Op = "reply"
	opReact        daemon.Op = "react"
	opUnreact      daemon.Op = "unreact"
	opEdit         daemon.Op = "edit"
	opDelete       daemon.Op = "delete"
	opThread       daemon.Op = "thread"
	opHistory      daemon.Op = "history"
	opFile         daemon.Op = "file"
	opChannels     daemon.Op = "channels"
	opUsers        daemon.Op = "users"
	opDMPeer       daemon.Op = "dm-peer"
	opFollow       daemon.Op = "follow"
	opUnfollow     daemon.Op = "unfollow"
	opDMStatus     daemon.Op = "dm-status"
	opRevoke       daemon.Op = "revoke"
	opAgentSubject daemon.Op = "agent-subject"

	opChannelCreate  daemon.Op = "channel-create"
	opChannelArchive daemon.Op = "channel-archive"
	opChannelInvite  daemon.Op = "channel-invite"
	opChannelTopic   daemon.Op = "channel-topic"
	opChannelJoin    daemon.Op = "channel-join"
)

const (
	kindThread  = "thread"
	kindChannel = "channel"
	kindDM      = "dm"

	defaultThreadLimit  = 50
	defaultHistoryLimit = 20
)

type opBody struct {
	ChannelID string            `json:"channel_id"`
	ThreadTS  string            `json:"thread_ts"`
	TS        string            `json:"ts"`
	Text      string            `json:"text"`
	Name      string            `json:"name"`
	Limit     int               `json:"limit"`
	Owner     string            `json:"owner"`
	WatchID   int64             `json:"watch_id"`
	URL       string            `json:"url"`
	Token     string            `json:"token"`
	Email     string            `json:"email"`
	SocketURL string            `json:"socket_url"`
	RepoRoot  string            `json:"repo_root"`
	OrcaPath  string            `json:"orca_path"`
	CaptHook  string            `json:"capt_hook_path"`
	AvatarKey string            `json:"avatar_key_id"`
	AvatarSec string            `json:"avatar_secret"`
	Config    config            `json:"config"`
	Files     []string          `json:"files"`
	Blocks    []json.RawMessage `json:"blocks"`
	Card      *statusCard       `json:"card"`
	Users     []string          `json:"users"`
	Topic     string            `json:"topic"`
	Prefix    string            `json:"prefix"`
	Query     string            `json:"query"`
	FileID    string            `json:"file_id"`
	Out       string            `json:"out"`
	Broadcast bool              `json:"broadcast"`
	NoWatch   bool              `json:"no_watch"`
	Agent     string            `json:"agent"`
	ticketSpend
}

type ticketResult struct {
	TicketID string `json:"ticket_id"`
}

type postResult struct {
	ChannelID  string              `json:"channel_id"`
	TS         string              `json:"ts"`
	ThreadTS   string              `json:"thread_ts"`
	Permalink  string              `json:"permalink"`
	WatchID    int64               `json:"watch_id,omitempty"`
	TicketID   string              `json:"ticket_id,omitempty"`
	Posted     bool                `json:"posted"`
	Watched    bool                `json:"watched"`
	WatchError string              `json:"watch_error,omitempty"`
	Replayed   bool                `json:"replayed,omitempty"`
	Channel    daemon.ChannelState `json:"channel,omitempty"`
}

func (r receipt) result(replayed bool) postResult {
	return postResult{
		ChannelID: r.ChannelID, TS: r.TS, Permalink: r.Permalink, WatchID: r.WatchID, TicketID: r.TicketID,
		Posted: true, Watched: r.WatchID != 0, WatchError: r.WatchError, Replayed: replayed,
	}
}

type channelResult struct {
	ChannelID string `json:"channel_id"`
	Permalink string `json:"permalink"`
	TicketID  string `json:"ticket_id,omitempty"`
	Replayed  bool   `json:"replayed,omitempty"`
}

func (r receipt) channelResult(replayed bool) channelResult {
	return channelResult{ChannelID: r.ChannelID, Permalink: r.Permalink, TicketID: r.TicketID, Replayed: replayed}
}

type threadMessage struct {
	TS         string        `json:"ts"`
	ThreadTS   string        `json:"thread_ts,omitempty"`
	User       string        `json:"user"`
	UserName   string        `json:"user_name"`
	FromUser   bool          `json:"from_user,omitempty"`
	FromClaude bool          `json:"from_claude"`
	FromBot    bool          `json:"from_bot,omitempty"`
	Text       string        `json:"text"`
	Files      []messageFile `json:"files,omitempty"`
}

type messageFile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Mimetype  string `json:"mimetype"`
	Permalink string `json:"permalink"`
}

type threadResult struct {
	ChannelID  string          `json:"channel_id"`
	ThreadTS   string          `json:"thread_ts"`
	Messages   []threadMessage `json:"messages"`
	Truncated  bool            `json:"truncated"`
	Stale      bool            `json:"stale,omitempty"`
	RetryAfter int             `json:"retry_after,omitempty"`
}

type historyMessage struct {
	TS          string        `json:"ts"`
	User        string        `json:"user"`
	UserName    string        `json:"user_name"`
	FromUser    bool          `json:"from_user,omitempty"`
	FromClaude  bool          `json:"from_claude"`
	Text        string        `json:"text"`
	Permalink   string        `json:"permalink"`
	ReplyCount  int           `json:"reply_count,omitempty"`
	LatestReply string        `json:"latest_reply,omitempty"`
	Files       []messageFile `json:"files,omitempty"`
}

type historyResult struct {
	ChannelID string           `json:"channel_id"`
	Messages  []historyMessage `json:"messages"`
}

type channelInfo struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	PreviousNames []string `json:"previous_names,omitempty"`
	Created       int64    `json:"created"`
}

type channelsResult struct {
	Channels []channelInfo `json:"channels"`
}

type userInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	RealName    string `json:"real_name"`
	Email       string `json:"email"`
}

type usersResult struct {
	Users []userInfo `json:"users"`
}

type dmPeerResult struct {
	ChannelID string   `json:"channel_id"`
	Peer      string   `json:"peer"`
	PeerName  string   `json:"peer_name"`
	PeerNames []string `json:"peer_names,omitempty"`
}

type followResult struct {
	WatchID   int64               `json:"watch_id"`
	ChannelID string              `json:"channel_id"`
	Joined    bool                `json:"joined"`
	Cursor    string              `json:"cursor"`
	Channel   daemon.ChannelState `json:"channel,omitempty"`
}

type unfollowResult struct {
	Stopped   bool `json:"stopped"`
	Remaining int  `json:"remaining"`
}

type whoami struct {
	identity
	Username string `json:"username"`
	Socket   string `json:"socket"`
}

func (a *app) register(s *daemon.Server) {
	a.channelState = func(hc daemon.HandlerCtx, subjectID string) daemon.ChannelState {
		return s.ChannelState(hc, subjectID, channelConsumer)
	}
	s.Register(opSessionStart, a.handleSessionStart)
	s.Register(opLogin, a.handleLogin)
	s.Register(opWhoami, a.handleWhoami)
	s.Register(opSend, a.handlePost(false))
	s.Register(opReply, a.handlePost(true))
	s.Register(opReact, a.handleReaction(actionReact, "already_reacted", slackAPI.AddReactionContext))
	s.Register(opUnreact, a.handleReaction(actionUnreact, "no_reaction", slackAPI.RemoveReactionContext))
	s.Register(opEdit, a.handleEdit)
	s.Register(opDelete, a.handleDelete)
	s.Register(opThread, a.handleThread)
	s.Register(opHistory, a.handleHistory)
	s.Register(opFile, a.handleFile)
	s.Register(opChannels, a.handleChannels)
	s.Register(opUsers, a.handleUsers)
	s.Register(opDMPeer, a.handleDMPeer)
	s.Register(opFollow, a.handleFollow)
	s.Register(opUnfollow, a.handleUnfollow)
	s.Register(opDMStatus, a.handleDMStatus)
	s.Register(opRevoke, a.handleRevoke)
	s.Register(opAgentSubject, a.handleAgentSubject)
	s.Register(opChannelCreate, a.handleChannel(approveCreate, createChannel))
	s.Register(opChannelArchive, a.handleChannel(approveArchive, a.archiveChannel))
	s.Register(opChannelInvite, a.handleChannel(approveInvite, a.inviteToChannel))
	s.Register(opChannelTopic, a.handleChannel(approveTopic, a.setChannelTopic))
	s.Register(opChannelJoin, a.handleChannel(approveJoin, a.joinChannel))
}

func ok(v any) daemon.Reply {
	body, err := json.Marshal(v)
	if err != nil {
		return fail(err.Error())
	}
	return daemon.Reply{OK: true, Body: body}
}

func fail(msg string) daemon.Reply { return daemon.Reply{OK: false, Error: msg} }

func decodeBody(hc daemon.HandlerCtx) (opBody, error) {
	var b opBody
	if len(hc.Env.Body) == 0 {
		return b, nil
	}
	if err := json.Unmarshal(hc.Env.Body, &b); err != nil {
		return b, fmt.Errorf("bad %s body: %w", hc.Env.Op, err)
	}
	err := b.fillFromPermalink()
	return b, err
}

func (b *opBody) fillFromPermalink() error {
	if b.URL == "" {
		return nil
	}
	ref, err := parsePermalink(b.URL)
	if err != nil {
		return err
	}
	if b.ChannelID == "" {
		b.ChannelID = ref.channelID
	}
	if b.TS == "" {
		b.TS = ref.ts
	}
	if b.ThreadTS == "" {
		b.ThreadTS = ref.threadTS
	}
	if b.FileID == "" {
		b.FileID = ref.fileID
	}
	return nil
}

type permalinkRef struct {
	channelID string
	ts        string
	threadTS  string
	fileID    string
}

func parsePermalink(raw string) (permalinkRef, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return permalinkRef{}, fmt.Errorf("parse Slack url %q: %w", raw, err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "files" {
		return permalinkRef{fileID: parts[2]}, nil
	}
	if len(parts) < 2 || parts[0] != "archives" {
		return permalinkRef{}, fmt.Errorf("not a Slack archives or files url: %q", raw)
	}
	ref := permalinkRef{channelID: parts[1]}
	if len(parts) > 2 {
		digits, found := strings.CutPrefix(parts[2], "p")
		if !found || len(digits) <= 6 {
			return permalinkRef{}, fmt.Errorf("not a Slack message url: %q", raw)
		}
		ref.ts = digits[:len(digits)-6] + "." + digits[len(digits)-6:]
		ref.threadTS = ref.ts
	}
	if thread := u.Query().Get("thread_ts"); thread != "" {
		ref.threadTS = thread
	}
	return ref, nil
}

func (b opBody) messageThread() string {
	if b.ThreadTS == b.TS {
		return ""
	}
	return b.ThreadTS
}

func messageThreadOf(ctx context.Context, api slackAPI, channelID string, b opBody) (string, error) {
	if thread := b.messageThread(); thread != "" {
		return thread, nil
	}
	msgs, _, _, err := api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{ChannelID: channelID, Timestamp: b.TS, Limit: 1})
	if err != nil {
		return "", fmt.Errorf("read the thread of message %s: %w", b.TS, err)
	}
	for _, m := range msgs {
		if m.Timestamp == b.TS && m.ThreadTimestamp != "" && m.ThreadTimestamp != b.TS {
			return m.ThreadTimestamp, nil
		}
	}
	return "", nil
}

func (a *app) handleSessionStart(hc daemon.HandlerCtx) daemon.Reply {
	sub, _, err := hc.Subjects.Start(hc.Ctx, hc.Window, hc.Scope, "", lifecycle, false)
	if err != nil {
		return fail(err.Error())
	}
	return daemon.Reply{OK: true, SubjectID: sub.ID, HTTPPort: hc.HTTPPort}
}

func (a *app) handleLogin(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.Token == "" || b.Email == "" || b.SocketURL == "" || b.RepoRoot == "" || b.CaptHook == "" {
		return fail("login needs a token, an email, a socket_url, a repo_root, and a capt_hook_path")
	}
	if err := b.Config.validate(); err != nil {
		return fail("login: " + err.Error())
	}
	if b.Config.Avatars.enabled() && (b.AvatarKey == "" || b.AvatarSec == "") {
		return fail("login needs an avatar_key_id and an avatar_secret when the config sets [avatars]")
	}
	id, err := resolveIdentity(hc.Ctx, a.newAPI(b.Token), b.Token, b.Email, a.now())
	if err != nil {
		return fail(fmt.Sprintf("login: %s", errorCode(err)))
	}
	id.SocketURL, id.RepoRoot, id.OrcaPath, id.CaptHookPath = b.SocketURL, b.RepoRoot, b.OrcaPath, b.CaptHook
	id.AvatarKeyID, id.AvatarSecret, id.Config = b.AvatarKey, b.AvatarSec, b.Config
	if id.AvatarURL, err = a.avatar(hc.Ctx, id); err != nil {
		return fail(fmt.Sprintf("login: agent avatar: %v", err))
	}
	if err := saveIdentity(hc.Ctx, hc.DB, id); err != nil {
		return fail(err.Error())
	}
	a.poller.reconcileAll()
	a.socket.restart()
	return ok(whoami{identity: id, Username: id.username(), Socket: a.socket.status()})
}

func (a *app) handleWhoami(hc daemon.HandlerCtx) daemon.Reply {
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	return ok(whoami{identity: id, Username: id.username(), Socket: a.socket.status()})
}

func (a *app) handlePost(requireThread bool) daemon.HandlerFunc {
	return func(hc daemon.HandlerCtx) daemon.Reply {
		b, err := decodeBody(hc)
		if err != nil {
			return fail(err.Error())
		}
		extra, err := checkPost(b, hc.Env.Op, requireThread)
		if err != nil {
			return fail(err.Error())
		}
		sub, _, err := hc.Subjects.Start(hc.Ctx, hc.Window, hc.Scope, "", lifecycle, false)
		if err != nil {
			return fail(err.Error())
		}
		id, err := loadIdentity(hc.Ctx, hc.DB)
		if err != nil {
			return fail(fallback(actionWrite, err))
		}
		id.Agent = b.Agent
		api := a.newAPI(id.Token)
		channelID, err := conversationID(hc.Ctx, api, id, b.ChannelID)
		if err != nil {
			return fail(fallback(actionWrite, err))
		}
		w := write{action: approvePost, named: b.ChannelID, channelID: channelID, threadTS: b.ThreadTS, text: b.Text}
		ticket, replay, err := a.authorize(hc, api, id, b.ticketSpend, w)
		if err != nil {
			return fail(err.Error())
		}
		if replay != nil {
			res := replay.result(true)
			posted, err := parsePermalink(res.Permalink)
			if err != nil {
				return fail(err.Error())
			}
			res.ThreadTS = posted.threadTS
			return ok(a.reportChannel(hc, sub.ID, res))
		}
		blocks, err := checkMessage(b)
		if err != nil {
			return fail(a.handBack(hc.Ctx, hc.DB, id, b.ticketSpend, ticket, false, err.Error()))
		}
		if err := a.claimReceipt(hc.Ctx, hc.DB, ticket, w); err != nil {
			return fail(err.Error())
		}
		var res postResult
		err = a.writeAndRetry(hc.Ctx, api, id, channelID, func() (err error) {
			res, err = postAs(hc.Ctx, api, hc.DB, a.avatar, a.poller.log, a.now(), id, channelID, b.ThreadTS, b.Text, blocks, b.Files, extra...)
			return err
		})
		if err != nil {
			return fail(a.slackFailed(hc, id, b.ticketSpend, ticket, actionWrite, err))
		}
		res.Posted = true
		res.ThreadTS = cmp.Or(b.ThreadTS, res.TS)
		a.wrote(hc.Ctx, res.ChannelID, res.ThreadTS)
		res.TicketID = ticket.ID
		r := receipt{TicketID: ticket.ID, ChannelID: res.ChannelID, TS: res.TS, Permalink: res.Permalink}
		if ticket.once() {
			if err := a.saveReceipt(hc.Ctx, hc.DB, r); err != nil {
				return fail(fmt.Sprintf("posted %s but could not record its receipt, so do not resend: %v", res.Permalink, err))
			}
		}
		if b.NoWatch {
			return ok(res)
		}
		if r.WatchID, err = a.watchPostThread(hc.Ctx, hc.DB, sub.ID, res.ChannelID, res.ThreadTS, res.TS); err != nil {
			r.WatchError = err.Error()
		}
		res.WatchID, res.Watched, res.WatchError = r.WatchID, r.WatchID != 0, r.WatchError
		if ticket.once() {
			if err := a.attachWatchReceipt(hc.Ctx, hc.DB, r); err != nil {
				res.WatchError = err.Error()
			}
		}
		return ok(a.reportChannel(hc, sub.ID, res))
	}
}

func checkPost(b opBody, op daemon.Op, requireThread bool) ([]slack.MsgOption, error) {
	if b.ChannelID == "" || (b.Text == "" && len(b.Files) == 0) {
		return nil, fmt.Errorf("%s needs channel_id and text or files (or a Slack url)", op)
	}
	if requireThread && b.ThreadTS == "" {
		return nil, errors.New("reply needs thread_ts (or a Slack message url)")
	}
	if !b.Broadcast {
		return nil, nil
	}
	if b.ThreadTS == "" {
		return nil, errors.New("broadcast needs thread_ts (or a Slack message url)")
	}
	return []slack.MsgOption{slack.MsgOptionBroadcast()}, nil
}

func (a *app) reportChannel(hc daemon.HandlerCtx, subjectID string, res postResult) postResult {
	if res.Watched {
		res.Channel = a.channelState(hc, subjectID)
	}
	return res
}

func (a *app) followChannel(hc daemon.HandlerCtx, subjectID, owner string) daemon.ChannelState {
	if owner != agent.TopLevel {
		return ""
	}
	return a.channelState(hc, subjectID)
}

func (a *app) watchPostThread(ctx context.Context, db *sql.DB, subjectID, channelID, threadTS, postTS string) (int64, error) {
	if existing, _, found, err := activeWatch(ctx, db, subjectID, agent.TopLevel, channelID, threadTS); err != nil || found {
		return existing, err
	}
	watchID, _, err := insertWatch(ctx, db, subjectID, agent.TopLevel, kindThread, channelID, threadTS, postTS, a.now())
	if err != nil {
		return 0, err
	}
	a.poller.nudge(cursorKey{channelID, threadTS})
	return watchID, nil
}

func (a *app) post(ctx context.Context, db *sql.DB, id identity, channelID, threadTS, text string, blocks []slack.Block, files []string) (postResult, error) {
	res, err := postAs(ctx, a.newAPI(id.Token), db, a.avatar, a.poller.log, a.now(), id, channelID, threadTS, text, blocks, files)
	if err == nil && threadTS != "" {
		a.wrote(ctx, res.ChannelID, threadTS)
	}
	return res, err
}

func (a *app) wrote(ctx context.Context, channelID, threadTS string) {
	if err := a.cache.invalidate(ctx, channelID, threadTS); err != nil {
		a.poller.log.Printf("%v", err)
	}
}

func postAs(ctx context.Context, api slackAPI, db *sql.DB, publish avatarPublisher, logger *log.Logger, now time.Time, id identity, channelID, threadTS, text string, blocks []slack.Block, files []string, extra ...slack.MsgOption) (postResult, error) {
	id, err := refreshProfile(ctx, api, db, publish, id, now)
	if err != nil {
		logger.Printf("refresh profile: %v", err)
	}
	ch, ts, err := postMessage(ctx, api, db, logger, id, channelID, threadTS, text, blocks, files, extra...)
	if err != nil {
		return postResult{}, err
	}
	return postResult{ChannelID: ch, TS: ts, Permalink: id.permalink(ch, ts, threadTS)}, nil
}

func postMessage(ctx context.Context, api slackAPI, db *sql.DB, logger *log.Logger, id identity, channelID, threadTS, text string, blocks []slack.Block, files []string, extra ...slack.MsgOption) (string, string, error) {
	metadata := docCards(ctx, api, logger, id.Config.DocCards, channelID, text)
	if metadata == nil {
		metadata = []slack.MsgOption{id.authorMetadata()}
	}
	extra = append(metadata, extra...)
	ch, ts, err := publish(ctx, api, id, channelID, threadTS, text, blocks, files, extra...)
	if err != nil {
		return "", "", err
	}
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO posts(channel_id, ts) VALUES (?, ?)`, ch, ts); err != nil {
		logger.Printf("record post %s/%s, the poller may route it back: %v", ch, ts, err)
	}
	return ch, ts, nil
}

func ownPost(ctx context.Context, db *sql.DB, channelID, ts string) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM posts WHERE channel_id = ? AND ts = ?`, channelID, ts).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check own post %s/%s: %w", channelID, ts, err)
	}
	return true, nil
}

func publish(ctx context.Context, api slackAPI, id identity, channelID, threadTS, text string, blocks []slack.Block, files []string, extra ...slack.MsgOption) (string, string, error) {
	if len(files) == 0 {
		opts := id.postOptions(text, threadTS, extra...)
		if len(blocks) > 0 {
			opts = append(opts, slack.MsgOptionBlocks(blocks...))
		}
		return api.PostMessageContext(ctx, channelID, opts...)
	}
	images, err := allImages(files)
	if err != nil {
		return "", "", err
	}
	if images {
		return postImages(ctx, api, id, channelID, threadTS, text, blocks, files, extra...)
	}
	return shareFiles(ctx, api, id, channelID, threadTS, text, files)
}

func checkMessage(b opBody) ([]slack.Block, error) {
	if len(b.Blocks) > 0 && b.Text == "" {
		return nil, errors.New("blocks need text too: Slack shows the text in notifications and where blocks cannot render")
	}
	if err := checkAgent(b.Agent); err != nil {
		return nil, err
	}
	blocks, err := parseBlocks(b.Blocks)
	if err != nil {
		return nil, err
	}
	return blocks, checkAttachments(b.Text, blocks, b.Files, b.Broadcast)
}

func (a *app) handleEdit(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.ChannelID == "" || b.TS == "" || b.Text == "" {
		return fail("edit needs channel_id, ts, and text (or a Slack url and text)")
	}
	blocks, err := checkMessage(b)
	if err != nil {
		return fail(err.Error())
	}
	return a.changeOwnMessage(hc, b, approveEdit, actionEdit, func(ctx context.Context, api slackAPI, channelID string) error {
		return updateMessage(ctx, api, channelID, b.TS, b.Text, blocks)
	})
}

func (a *app) handleDelete(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.ChannelID == "" || b.TS == "" || b.Text != "" || len(b.Blocks) > 0 {
		return fail("delete needs channel_id and ts (or a Slack url), and no text or blocks")
	}
	return a.changeOwnMessage(hc, b, approveDelete, actionDelete, func(ctx context.Context, api slackAPI, channelID string) error {
		_, _, err := api.DeleteMessageContext(ctx, channelID, b.TS)
		return err
	})
}

func (a *app) changeOwnMessage(hc daemon.HandlerCtx, b opBody, verb string, act action, change func(context.Context, slackAPI, string) error) daemon.Reply {
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(act, err))
	}
	api := a.newAPI(id.Token)
	channelID, err := conversationID(hc.Ctx, api, id, b.ChannelID)
	if err != nil {
		return fail(fallback(act, err))
	}
	thread, err := messageThreadOf(hc.Ctx, api, channelID, b)
	if err != nil {
		return fail(fallback(act, err))
	}
	w := write{action: verb, named: b.ChannelID, channelID: channelID, threadTS: thread, text: b.Text}
	ticket, replay, err := a.authorize(hc, api, id, b.ticketSpend, w)
	if err != nil {
		return fail(err.Error())
	}
	if replay != nil {
		return ok(replay.result(true))
	}
	if err := a.claimReceipt(hc.Ctx, hc.DB, ticket, w); err != nil {
		return fail(err.Error())
	}
	err = a.writeAndRetry(hc.Ctx, api, id, channelID, func() error {
		return change(hc.Ctx, api, channelID)
	})
	if err != nil {
		return fail(a.slackFailed(hc, id, b.ticketSpend, ticket, act, err))
	}
	touched := thread
	if touched == "" {
		touched = b.TS
	}
	a.wrote(hc.Ctx, channelID, touched)
	r := receipt{TicketID: ticket.ID, ChannelID: channelID, TS: b.TS, Permalink: id.permalink(channelID, b.TS, thread)}
	if !ticket.once() {
		return ok(r.result(false))
	}
	if err := a.saveReceipt(hc.Ctx, hc.DB, r); err != nil {
		return fail(fmt.Sprintf("made %s of %s but could not record its receipt, so do not resend: %v", w.target(), r.Permalink, err))
	}
	return ok(r.result(false))
}

func channelWrite(action string, b opBody) (write, error) {
	w := write{action: action, named: b.ChannelID}
	switch action {
	case approveCreate:
		if b.Name == "" {
			return w, errors.New("channel-create needs name")
		}
		w.named, w.text = b.Name, b.Name
		return w, nil
	case approveInvite:
		if len(b.Users) == 0 {
			return w, errors.New("channel-invite needs users")
		}
		w.users = slices.Sorted(slices.Values(b.Users))
	case approveTopic:
		if b.Topic == "" {
			return w, errors.New("channel-topic needs topic")
		}
		w.text = b.Topic
	}
	if b.ChannelID == "" {
		return w, fmt.Errorf("channel-%s needs channel_id (or a Slack url)", action)
	}
	return w, nil
}

type channelCall func(ctx context.Context, api slackAPI, id identity, w write) (string, error)

func (a *app) handleChannel(action string, call channelCall) daemon.HandlerFunc {
	return func(hc daemon.HandlerCtx) daemon.Reply {
		b, err := decodeBody(hc)
		if err != nil {
			return fail(err.Error())
		}
		w, err := channelWrite(action, b)
		if err != nil {
			return fail(err.Error())
		}
		id, err := loadIdentity(hc.Ctx, hc.DB)
		if err != nil {
			return fail(fallback(actionChannel, err))
		}
		api := a.newAPI(id.Token)
		if w.channelID, err = conversationID(hc.Ctx, api, id, b.ChannelID); err != nil {
			return fail(fallback(actionChannel, err))
		}
		ticket, replay, err := a.authorize(hc, api, id, b.ticketSpend, w)
		if err != nil {
			return fail(err.Error())
		}
		if replay != nil {
			return ok(replay.channelResult(true))
		}
		if err := a.claimReceipt(hc.Ctx, hc.DB, ticket, w); err != nil {
			return fail(err.Error())
		}
		channelID, err := call(hc.Ctx, api, id, w)
		if err != nil {
			return fail(a.slackFailed(hc, id, b.ticketSpend, ticket, actionChannel, err))
		}
		r := receipt{TicketID: ticket.ID, ChannelID: channelID, TS: formatTS(a.now()), Permalink: id.channelLink(channelID)}
		if !ticket.once() {
			return ok(r.channelResult(false))
		}
		if err := a.saveReceipt(hc.Ctx, hc.DB, r); err != nil {
			return fail(fmt.Sprintf("made %s but could not record its receipt, so do not resend: %v", w.target(), err))
		}
		return ok(r.channelResult(false))
	}
}

func createChannel(ctx context.Context, api slackAPI, _ identity, w write) (string, error) {
	ch, err := api.CreateConversationContext(ctx, slack.CreateConversationParams{ChannelName: w.text})
	if err != nil {
		return "", err
	}
	return ch.ID, nil
}

func (a *app) archiveChannel(ctx context.Context, api slackAPI, id identity, w write) (string, error) {
	err := a.writeAndRetry(ctx, api, id, w.channelID, func() error { return api.ArchiveConversationContext(ctx, w.channelID) })
	if err != nil && errorCode(err) != "already_archived" {
		return "", err
	}
	return w.channelID, nil
}

func (a *app) inviteToChannel(ctx context.Context, api slackAPI, id identity, w write) (string, error) {
	err := a.writeAndRetry(ctx, api, id, w.channelID, func() error {
		_, err := api.InviteUsersToConversationContext(ctx, w.channelID, w.users...)
		return err
	})
	if err != nil && errorCode(err) != "already_in_channel" {
		return "", err
	}
	return w.channelID, nil
}

func (a *app) setChannelTopic(ctx context.Context, api slackAPI, id identity, w write) (string, error) {
	err := a.writeAndRetry(ctx, api, id, w.channelID, func() error {
		_, err := api.SetTopicOfConversationContext(ctx, w.channelID, w.text)
		return err
	})
	if err != nil {
		return "", err
	}
	return w.channelID, nil
}

func (a *app) joinChannel(ctx context.Context, api slackAPI, id identity, w write) (string, error) {
	_, _, _, err := api.JoinConversationContext(ctx, w.channelID)
	if err == nil {
		return w.channelID, nil
	}
	if code := errorCode(err); code != "channel_not_found" && code != "method_not_supported_for_channel_type" {
		return "", err
	}
	if err := a.inviteBot(ctx, id, w.channelID); err != nil {
		return "", inviteNeededError{channelID: w.channelID, inviteErr: err}
	}
	a.poller.log.Printf("invited the bot to %s as %s", w.channelID, id.UserID)
	return w.channelID, nil
}

func (a *app) handleRevoke(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.TicketID == "" {
		return fail("revoke needs ticket_id")
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionWrite, err))
	}
	if _, err := a.captHook(hc.Ctx, id.CaptHookPath, "ticket", "revoke", b.TicketID); err != nil {
		return fail(fmt.Sprintf("revoke ticket %s: %v", b.TicketID, err))
	}
	return ok(ticketResult{TicketID: b.TicketID})
}

func updateMessage(ctx context.Context, api slackAPI, channelID, ts, text string, blocks []slack.Block) error {
	_, _, _, err := api.UpdateMessageContext(ctx, channelID, ts,
		slack.MsgOptionText(text, false), slack.MsgOptionBlocks(blocks...), slack.MsgOptionAttachments([]slack.Attachment{}...), slack.MsgOptionDisableLinkUnfurl())
	return err
}

func (a *app) handleReaction(act action, settled string, apply func(slackAPI, context.Context, string, slack.ItemRef) error) daemon.HandlerFunc {
	return func(hc daemon.HandlerCtx) daemon.Reply {
		b, err := decodeBody(hc)
		if err != nil {
			return fail(err.Error())
		}
		name := strings.Trim(b.Name, ":")
		if b.ChannelID == "" || b.TS == "" || name == "" {
			return fail(fmt.Sprintf("%s needs channel_id, ts, and name (or a Slack url and name)", hc.Env.Op))
		}
		id, err := loadIdentity(hc.Ctx, hc.DB)
		if err != nil {
			return fail(fallback(act, err))
		}
		api := a.newAPI(id.Token)
		channelID, err := conversationID(hc.Ctx, api, id, b.ChannelID)
		if err != nil {
			return fail(fallback(act, err))
		}
		err = a.joinAndRetry(hc.Ctx, api, channelID, func() error {
			return apply(api, hc.Ctx, name, slack.ItemRef{Channel: channelID, Timestamp: b.TS})
		})
		if err != nil && errorCode(err) != settled {
			return fail(fallback(act, err))
		}
		return ok(map[string]bool{"ok": true})
	}
}

func (a *app) handleThread(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.ChannelID == "" || b.ThreadTS == "" {
		return fail("thread needs channel_id and thread_ts (or a Slack url)")
	}
	limit := b.Limit
	if limit <= 0 {
		limit = defaultThreadLimit
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	api := a.newAPI(id.Token)
	channelID, err := conversationID(hc.Ctx, api, id, b.ChannelID)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	read, err := a.cache.replies(hc.Ctx, api, channelID, b.ThreadTS)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	msgs := read.msgs
	truncated := len(msgs) > limit
	if truncated {
		msgs = msgs[len(msgs)-limit:]
	}
	out := make([]threadMessage, 0, len(msgs))
	for _, m := range msgs {
		files, err := messageFiles(hc.Ctx, api, m)
		if err != nil {
			return fail(fallback(actionRead, err))
		}
		out = append(out, threadMessage{
			TS: m.Timestamp, ThreadTS: m.ThreadTimestamp, User: m.User, UserName: a.cache.name(hc.Ctx, api, m),
			FromUser: m.User == id.UserID, FromClaude: id.fromClaude(m), FromBot: m.BotID != "", Text: messageText(m), Files: files,
		})
	}
	res := threadResult{ChannelID: channelID, ThreadTS: b.ThreadTS, Messages: out, Truncated: truncated, Stale: read.stale}
	if read.stale {
		res.RetryAfter = int(math.Ceil(read.retryAfter.Seconds()))
	}
	return ok(res)
}

func (a *app) handleHistory(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.ChannelID == "" {
		return fail("history needs channel_id (or a Slack url)")
	}
	limit := b.Limit
	if limit <= 0 {
		limit = defaultHistoryLimit
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	api := a.newAPI(id.Token)
	channelID, err := conversationID(hc.Ctx, api, id, b.ChannelID)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	msgs, err := fetchRecentTopLevel(hc.Ctx, api, channelID, limit)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	out := make([]historyMessage, 0, len(msgs))
	for _, m := range msgs {
		files, err := messageFiles(hc.Ctx, api, m)
		if err != nil {
			return fail(fallback(actionRead, err))
		}
		out = append(out, historyMessage{
			TS: m.Timestamp, User: m.User, UserName: a.cache.name(hc.Ctx, api, m), FromUser: m.User == id.UserID,
			FromClaude: id.fromClaude(m), Text: messageText(m),
			Permalink: id.permalink(channelID, m.Timestamp, ""), ReplyCount: m.ReplyCount, LatestReply: m.LatestReply,
			Files: files,
		})
	}
	return ok(historyResult{ChannelID: channelID, Messages: out})
}

func messageFiles(ctx context.Context, api slackAPI, m slack.Message) ([]messageFile, error) {
	var out []messageFile
	seen := map[string]bool{}
	for _, f := range m.Files {
		seen[f.ID] = true
		out = append(out, messageFile{ID: f.ID, Name: f.Name, Mimetype: f.Mimetype, Permalink: f.Permalink})
	}
	for _, block := range m.Blocks.BlockSet {
		image, ok := block.(*slack.ImageBlock)
		if !ok || image.SlackFile == nil || image.SlackFile.ID == "" || seen[image.SlackFile.ID] {
			continue
		}
		seen[image.SlackFile.ID] = true
		f, _, _, err := api.GetFileInfoContext(ctx, image.SlackFile.ID, 0, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, messageFile{ID: f.ID, Name: f.Name, Mimetype: f.Mimetype, Permalink: f.Permalink})
	}
	return out, nil
}

func messageText(m slack.Message) string {
	parts := []string{m.Text}
	for _, at := range m.Attachments {
		fields := []string{at.Pretext, at.Title, at.Text, at.TitleLink}
		if at.Title == "" && at.Text == "" {
			fields = []string{at.Pretext, at.Fallback}
		}
		parts = append(parts, fields...)
	}
	parts = slices.DeleteFunc(parts, func(p string) bool { return strings.TrimSpace(p) == "" })
	return strings.Join(parts, "\n")
}

func (a *app) handleChannels(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	channels, err := fetchChannels(hc.Ctx, a.newAPI(id.Token))
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	out := []channelInfo{}
	for _, c := range channels {
		if strings.HasPrefix(c.Name, b.Prefix) {
			out = append(out, channelInfo{ID: c.ID, Name: c.Name, PreviousNames: c.PreviousNames, Created: int64(c.Created)})
		}
	}
	slices.SortFunc(out, func(x, y channelInfo) int { return strings.Compare(x.Name, y.Name) })
	return ok(channelsResult{Channels: out})
}

func (a *app) handleUsers(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.Query == "" {
		return fail("users needs a query")
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	users, err := a.newAPI(id.Token).GetUsersContext(hc.Ctx, slack.GetUsersOptionLimit(listPageLimit))
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	query := strings.ToLower(b.Query)
	out := []userInfo{}
	for _, u := range users {
		info := userInfo{ID: u.ID, Name: u.Name, DisplayName: u.Profile.DisplayName, RealName: u.RealName, Email: u.Profile.Email}
		if !u.Deleted && slices.ContainsFunc([]string{info.Name, info.DisplayName, info.RealName, info.Email}, func(f string) bool {
			return strings.Contains(strings.ToLower(f), query)
		}) {
			out = append(out, info)
		}
	}
	slices.SortFunc(out, func(x, y userInfo) int { return strings.Compare(x.Name, y.Name) })
	return ok(usersResult{Users: out})
}

func (a *app) handleDMPeer(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.ChannelID == "" {
		return fail("dm-peer needs channel_id (or a Slack url)")
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	api := a.newAPI(id.Token)
	peer, err := a.dmPeer(hc.Ctx, api, id, b.ChannelID)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	if peer == "" {
		return ok(dmPeerResult{ChannelID: b.ChannelID})
	}
	name := a.cache.name(hc.Ctx, api, slack.Message{Msg: slack.Msg{User: peer}})
	return ok(dmPeerResult{ChannelID: b.ChannelID, Peer: peer, PeerName: name, PeerNames: peerNames(hc.Ctx, api, peer, name)})
}

func peerNames(ctx context.Context, api slackAPI, peer, name string) []string {
	names := []string{name}
	if u, err := api.GetUserInfoContext(ctx, peer); err == nil {
		names = append(names, u.RealName, u.Profile.DisplayName, u.Name)
	}
	return slices.Compact(slices.Sorted(slices.Values(slices.DeleteFunc(names, func(n string) bool { return n == "" }))))
}

func (id identity) fromClaude(m slack.Message) bool {
	return (id.BotID != "" && m.BotID == id.BotID) || (id.BotUserID != "" && m.User == id.BotUserID)
}

func (a *app) handleFollow(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.ChannelID == "" {
		return fail("follow needs channel_id (or a Slack url)")
	}
	sub, _, err := hc.Subjects.Start(hc.Ctx, hc.Window, hc.Scope, "", lifecycle, false)
	if err != nil {
		return fail(err.Error())
	}
	subjectID := sub.ID
	if b.Owner != agent.TopLevel {
		if subjectID, err = agentSubject(hc, b.Owner); err != nil {
			return fail(err.Error())
		}
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionWatch, err))
	}
	kind := kindChannel
	if b.ThreadTS != "" {
		kind = kindThread
	}
	api := a.newAPI(id.Token)
	channelID, err := conversationID(hc.Ctx, api, id, b.ChannelID)
	if err != nil {
		return fail(fallback(actionWatch, err))
	}
	if existing, oldest, found, err := activeWatch(hc.Ctx, hc.DB, subjectID, b.Owner, channelID, b.ThreadTS); err != nil {
		return fail(err.Error())
	} else if found {
		return ok(followResult{WatchID: existing, ChannelID: channelID, Cursor: oldest, Channel: a.followChannel(hc, subjectID, b.Owner)})
	}
	joined, err := joinPublic(hc.Ctx, api, channelID)
	if err != nil {
		return fail(fallback(actionWatch, err))
	}
	latest, err := a.latestTS(hc.Ctx, api, channelID, b.ThreadTS)
	if err != nil {
		return fail(fallback(actionWatch, err))
	}
	watchID, cursor, err := insertWatch(hc.Ctx, hc.DB, subjectID, b.Owner, kind, channelID, b.ThreadTS, latest, a.now())
	if err != nil {
		return fail(err.Error())
	}
	a.poller.nudge(cursorKey{channelID, b.ThreadTS})
	return ok(followResult{WatchID: watchID, ChannelID: channelID, Joined: joined, Cursor: cursor, Channel: a.followChannel(hc, subjectID, b.Owner)})
}

func (a *app) handleAgentSubject(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	subjectID, err := agentSubject(hc, b.Owner)
	if err != nil {
		return fail(err.Error())
	}
	return daemon.Reply{OK: true, SubjectID: subjectID, HTTPPort: hc.HTTPPort}
}

func agentSubject(hc daemon.HandlerCtx, agentID string) (string, error) {
	var subjectID string
	err := hc.DB.QueryRowContext(hc.Ctx, `SELECT a.subject_id FROM agents a JOIN subjects s ON s.id = a.subject_id
		WHERE a.agent_id = ? AND s.scope = ? AND (s.session_id = ? OR (s.claude_pid <> 0 AND s.claude_pid = ?))
		ORDER BY a.started_at DESC LIMIT 1`,
		agentID, hc.Scope, hc.Window.Session, hc.Window.ClaudePID).Scan(&subjectID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("unknown agent %q in this Claude window: pass the agent id a cc-slack:slack-waiter's greeting named", agentID)
	}
	if err != nil {
		return "", fmt.Errorf("look up agent %s: %w", agentID, err)
	}
	return subjectID, nil
}

func conversationID(ctx context.Context, api slackAPI, id identity, channelID string) (string, error) {
	if !strings.HasPrefix(channelID, "U") && !strings.HasPrefix(channelID, "W") {
		return channelID, nil
	}
	users := []string{channelID}
	if channelID != id.UserID {
		users = append(users, id.UserID)
	}
	dm, _, _, err := api.OpenConversationContext(ctx, &slack.OpenConversationParameters{Users: users})
	if err != nil {
		return "", err
	}
	return dm.ID, nil
}

type peerKey struct {
	userID    string
	channelID string
}

func (a *app) dmPeer(ctx context.Context, api slackAPI, id identity, channelID string) (string, error) {
	key := peerKey{id.UserID, channelID}
	if cached, ok := a.peers.Load(key); ok {
		peer, _ := cached.(string)
		return peer, nil
	}
	peer, err := lookupDMPeer(ctx, api, id, channelID)
	if err == nil {
		a.peers.Store(key, peer)
	}
	return peer, err
}

func lookupDMPeer(ctx context.Context, api slackAPI, id identity, channelID string) (string, error) {
	if strings.HasPrefix(channelID, "U") || strings.HasPrefix(channelID, "W") {
		return channelID, nil
	}
	info, err := api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channelID})
	if err != nil {
		if errorCode(err) == "channel_not_found" {
			return "", nil
		}
		return "", err
	}
	if !info.IsIM && !info.IsMpIM {
		return "", nil
	}
	var members []string
	cursor := ""
	for {
		page, next, err := api.GetUsersInConversationContext(ctx, &slack.GetUsersInConversationParameters{ChannelID: channelID, Cursor: cursor})
		if err != nil {
			return "", err
		}
		members = append(members, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	others := slices.DeleteFunc(members, func(member string) bool { return member == id.BotUserID || member == id.UserID })
	switch len(others) {
	case 0:
		return id.UserID, nil
	case 1:
		return others[0], nil
	}
	return "", nil
}

func joinPublic(ctx context.Context, api slackAPI, channelID string) (bool, error) {
	joined, err := join(ctx, api, channelID)
	if err != nil && errorCode(err) == "method_not_supported_for_channel_type" {
		return false, nil
	}
	return joined, err
}

func join(ctx context.Context, api slackAPI, channelID string) (bool, error) {
	if !strings.HasPrefix(channelID, "C") {
		return false, nil
	}
	_, warning, _, err := api.JoinConversationContext(ctx, channelID)
	if err != nil {
		return false, err
	}
	return warning != "already_in_channel", nil
}

type inviteNeededError struct {
	channelID string
	inviteErr error
}

func (e inviteNeededError) Error() string {
	if e.inviteErr != nil {
		return fmt.Sprintf("<#%s> is private and the bot is not in it, and inviting it as the user failed (%v)", e.channelID, e.inviteErr)
	}
	return fmt.Sprintf("<#%s> is private and the bot is not in it: add it with channel-join under the user's approval, then retry", e.channelID)
}

func (a *app) joinAndRetry(ctx context.Context, api slackAPI, channelID string, call func() error) error {
	err := call()
	if err == nil {
		return nil
	}
	if code := errorCode(err); code != "not_in_channel" && code != "channel_not_found" {
		return err
	}
	joined, joinErr := join(ctx, api, channelID)
	if joinErr != nil {
		switch errorCode(joinErr) {
		case "channel_not_found", "method_not_supported_for_channel_type":
			return inviteNeededError{channelID: channelID}
		}
		return joinErr
	}
	if !joined {
		return err
	}
	a.poller.log.Printf("joined %s to write there", channelID)
	return call()
}

func (a *app) writeAndRetry(ctx context.Context, api slackAPI, id identity, channelID string, call func() error) error {
	err := a.joinAndRetry(ctx, api, channelID, call)
	var need inviteNeededError
	if !errors.As(err, &need) {
		return err
	}
	if need.inviteErr = a.inviteBot(ctx, id, channelID); need.inviteErr != nil {
		return need
	}
	a.poller.log.Printf("invited the bot to %s as %s to write there", channelID, id.UserID)
	return call()
}

func (a *app) inviteBot(ctx context.Context, id identity, channelID string) error {
	user, err := a.userAPI(ctx, id)
	if err != nil {
		return err
	}
	if _, err := user.InviteUsersToConversationContext(ctx, channelID, id.BotUserID); err != nil && errorCode(err) != "already_in_channel" {
		return fmt.Errorf("conversations.invite: %w", err)
	}
	return nil
}

func (a *app) userAPI(ctx context.Context, id identity) (slackAPI, error) {
	cred, err := a.user.read(ctx)
	if err != nil {
		return nil, err
	}
	user := a.user.api(cred)
	switch err := checkUserSession(ctx, user, id); {
	case err == nil:
		return user, nil
	case !staleSession(err):
		return nil, err
	}
	if cred, err = chromeUserCredential(ctx, a.user.cookiesync, id.TeamID); err != nil {
		return nil, err
	}
	user = a.user.api(cred)
	switch err := checkUserSession(ctx, user, id); {
	case err == nil:
	case staleSession(err):
		return nil, errChromeSignedOut
	default:
		return nil, err
	}
	if err := a.user.save(ctx, cred); err != nil {
		return nil, fmt.Errorf("save the Slack session re-minted from Chrome: %w", err)
	}
	a.poller.log.Printf("re-minted %s's Slack session from Chrome", id.UserID)
	return user, nil
}

func checkUserSession(ctx context.Context, user slackAPI, id identity) error {
	auth, err := user.AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("the user's Slack session credential failed auth.test: %w", err)
	}
	if auth.UserID != id.UserID || auth.TeamID != id.TeamID {
		return fmt.Errorf("the Slack MCP credential is %s in %s, not %s in %s", auth.UserID, auth.TeamID, id.UserID, id.TeamID)
	}
	return nil
}

func (a *app) latestTS(ctx context.Context, api slackAPI, channelID, threadTS string) (string, error) {
	var msgs []slack.Message
	if threadTS != "" {
		read, err := a.cache.replies(ctx, api, channelID, threadTS)
		if err != nil {
			return "", err
		}
		msgs = read.msgs
	} else {
		var err error
		if msgs, err = fetchHistory(ctx, api, channelID, "", 1); err != nil {
			return "", err
		}
	}
	if len(msgs) == 0 {
		return formatTS(a.now()), nil
	}
	return msgs[len(msgs)-1].Timestamp, nil
}

func activeWatch(ctx context.Context, db *sql.DB, subjectID, owner, channelID, threadTS string) (int64, string, bool, error) {
	var (
		id     int64
		oldest string
	)
	err := db.QueryRowContext(ctx, `SELECT w.id, c.oldest_ts FROM watches w
		JOIN cursors c ON c.channel_id = w.channel_id AND c.thread_ts = w.thread_ts
		WHERE w.subject_id = ? AND w.owner_agent_id = ? AND w.channel_id = ? AND w.thread_ts = ? AND w.status = 'active'`,
		subjectID, owner, channelID, threadTS).Scan(&id, &oldest)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("find watch: %w", err)
	}
	return id, oldest, true, nil
}

func insertWatch(ctx context.Context, db *sql.DB, subjectID, owner, kind, channelID, threadTS, oldest string, now time.Time) (int64, string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", fmt.Errorf("begin watch tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	inUse, err := countActive(ctx, tx, `SELECT COUNT(*) FROM watches WHERE channel_id = ? AND thread_ts = ? AND status = 'active'`, channelID, threadTS)
	if err != nil {
		return 0, "", err
	}
	if err := claimCursor(ctx, tx, channelID, threadTS, oldest, now, inUse); err != nil {
		return 0, "", err
	}
	watchID, err := addWatch(ctx, tx, subjectID, owner, kind, channelID, threadTS, now)
	if err != nil {
		return 0, "", err
	}
	var cursor string
	if err := tx.QueryRowContext(ctx, `SELECT oldest_ts FROM cursors WHERE channel_id = ? AND thread_ts = ?`, channelID, threadTS).Scan(&cursor); err != nil {
		return 0, "", fmt.Errorf("read cursor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, "", fmt.Errorf("commit watch: %w", err)
	}
	return watchID, cursor, nil
}

func addWatch(ctx context.Context, tx *sql.Tx, subjectID, owner, kind, channelID, threadTS string, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO watches(subject_id, owner_agent_id, kind, channel_id, thread_ts, status, created_at)
		VALUES (?, ?, ?, ?, ?, 'active', ?)`, subjectID, owner, kind, channelID, threadTS, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("insert watch: %w", err)
	}
	watchID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert watch id: %w", err)
	}
	return watchID, nil
}

func countActive(ctx context.Context, tx *sql.Tx, query string, args ...any) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return false, fmt.Errorf("count active watches: %w", err)
	}
	return n > 0, nil
}

func claimCursor(ctx context.Context, tx *sql.Tx, channelID, threadTS, oldest string, now time.Time, inUse bool) error {
	query := `INSERT INTO cursors(channel_id, thread_ts, oldest_ts, next_poll_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(channel_id, thread_ts) DO UPDATE SET oldest_ts = excluded.oldest_ts, next_poll_at = excluded.next_poll_at, backoff_s = 0`
	if inUse {
		query = `INSERT OR IGNORE INTO cursors(channel_id, thread_ts, oldest_ts, next_poll_at) VALUES (?, ?, ?, ?)`
	}
	if _, err := tx.ExecContext(ctx, query, channelID, threadTS, oldest, now.UnixMilli()); err != nil {
		return fmt.Errorf("claim cursor: %w", err)
	}
	return nil
}

func (a *app) handleUnfollow(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	sub, found, err := hc.Subjects.Find(hc.Ctx, hc.Window, hc.Scope)
	if err != nil {
		return fail(err.Error())
	}
	if !found {
		return fail("no cc-slack session for this Claude window, so it holds no watch to stop")
	}
	res, err := hc.DB.ExecContext(hc.Ctx, `UPDATE watches SET status = 'stopped' WHERE id = ? AND status = 'active' AND subject_id IN
		(SELECT id FROM subjects WHERE scope = ? AND (id = ? OR (claude_pid <> 0 AND claude_pid = ?)))`,
		b.WatchID, hc.Scope, sub.ID, hc.Window.ClaudePID)
	if err != nil {
		return fail(fmt.Sprintf("stop watch: %v", err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fail(fmt.Sprintf("stop watch: %v", err))
	}
	var remaining int
	if err := hc.DB.QueryRowContext(hc.Ctx, `SELECT COUNT(*) FROM watches WHERE subject_id = ? AND owner_agent_id = ? AND status = 'active'`,
		sub.ID, agent.TopLevel).Scan(&remaining); err != nil {
		return fail(fmt.Sprintf("count watches: %v", err))
	}
	return ok(unfollowResult{Stopped: n > 0, Remaining: remaining})
}

func (a *app) handleDMStatus(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.Card != nil && (b.Text != "" || len(b.Blocks) > 0 || len(b.Files) > 0) {
		return fail("dm-status takes a card or text, blocks, and files, never both")
	}
	if b.Card == nil && b.Text == "" && len(b.Files) == 0 {
		return fail("dm-status needs a card, text, or files")
	}
	if b.Card != nil {
		if err := b.Card.check(); err != nil {
			return fail(err.Error())
		}
	}
	blocks, err := checkMessage(b)
	if err != nil {
		return fail(err.Error())
	}
	if b.ThreadTS != "" && !slackTSPattern.MatchString(b.ThreadTS) {
		return fail(fmt.Sprintf("dm-status thread_ts %q is not a Slack ts", b.ThreadTS))
	}
	sub, _, err := hc.Subjects.Start(hc.Ctx, hc.Window, hc.Scope, "", lifecycle, false)
	if err != nil {
		return fail(err.Error())
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionWrite, err))
	}
	id.Agent = b.Agent
	msg := dmMessage{text: b.Text, blocks: blocks, files: b.Files}
	if b.Card != nil {
		msg = a.cardMessage(*b.Card, gitCardTag(hc.Ctx, hc.Env.Session, hc.Env.Scope))
	}
	thread, posted, err := a.dmStatus(hc.Ctx, hc.DB, sub, id, b.ThreadTS, msg)
	if err != nil && !posted {
		return fail(fallback(actionWrite, err))
	}
	if err != nil {
		return fail(err.Error())
	}
	return ok(map[string]string{"channel_id": id.DMChannelID, "thread_ts": thread, "channel": string(a.channelState(hc, sub.ID))})
}

type dmMessage struct {
	text   string
	blocks []slack.Block
	files  []string
	root   []slack.Block
}

func (a *app) cardMessage(card statusCard, tag cardTag) dmMessage {
	now := a.now()
	return dmMessage{text: card.text(), blocks: card.replyBlocks(tag, now), root: card.rootBlocks(tag, now)}
}

func (a *app) dmStatus(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, threadTS string, msg dmMessage) (string, bool, error) {
	thread, found, err := sessionThread(ctx, db, sub.ID, id.DMChannelID)
	if err != nil {
		return "", false, err
	}
	if threadTS != "" && threadTS != thread {
		thread, found = threadTS, false
	}
	if thread == "" && msg.root != nil {
		root, err := a.post(ctx, db, id, id.DMChannelID, "", msg.text, msg.root, nil)
		if err != nil {
			return "", false, err
		}
		thread = root.TS
		if err := a.bindSessionThread(ctx, db, sub, id, thread); err != nil {
			return "", true, err
		}
		found = true
	} else if msg.root != nil {
		if err := a.refreshRoot(ctx, db, id, thread, msg); err != nil {
			a.poller.log.Printf("status card root %s/%s not refreshed: %v", id.DMChannelID, thread, err)
		}
	}
	res, err := a.post(ctx, db, id, id.DMChannelID, thread, msg.text, msg.blocks, msg.files)
	if err != nil {
		return "", false, err
	}
	if found {
		return thread, true, nil
	}
	if thread == "" {
		thread = res.TS
	}
	if err := a.bindSessionThread(ctx, db, sub, id, thread); err != nil {
		return "", true, err
	}
	return thread, true, nil
}

func (a *app) bindSessionThread(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, thread string) error {
	if err := a.openSessionThread(ctx, db, sub, id.DMChannelID, thread); err != nil {
		return err
	}
	a.poller.nudge(cursorKey{id.DMChannelID, thread}, cursorKey{id.DMChannelID, ""})
	return nil
}

func (a *app) refreshRoot(ctx context.Context, db *sql.DB, id identity, thread string, msg dmMessage) error {
	own, err := ownPost(ctx, db, id.DMChannelID, thread)
	if err != nil || !own {
		return err
	}
	return updateMessage(ctx, a.newAPI(id.Token), id.DMChannelID, thread, msg.text, msg.root)
}

func sessionThread(ctx context.Context, db *sql.DB, subjectID, dmChannelID string) (string, bool, error) {
	var thread string
	err := db.QueryRowContext(ctx, `SELECT thread_ts FROM watches WHERE subject_id = ? AND kind = 'dm' AND channel_id = ? AND status = 'active'
		ORDER BY id DESC LIMIT 1`, subjectID, dmChannelID).Scan(&thread)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("find session thread: %w", err)
	}
	return thread, true, nil
}

func (a *app) openSessionThread(ctx context.Context, db *sql.DB, sub subject.Subject, dmChannelID, threadTS string) error {
	now := a.now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin session thread tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE watches SET status = 'stopped' WHERE subject_id = ? AND kind = 'dm' AND status = 'active'`,
		sub.ID); err != nil {
		return fmt.Errorf("release previous session thread: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO cursors(channel_id, thread_ts, oldest_ts, next_poll_at) VALUES (?, '', ?, ?)
		ON CONFLICT(channel_id, thread_ts) DO UPDATE SET next_poll_at = MIN(next_poll_at, excluded.next_poll_at)`,
		dmChannelID, threadTS, now.UnixMilli()); err != nil {
		return fmt.Errorf("claim DM cursor: %w", err)
	}
	if err := claimCursor(ctx, tx, dmChannelID, threadTS, threadTS, now, false); err != nil {
		return err
	}
	if _, err := addWatch(ctx, tx, sub.ID, agent.TopLevel, kindDM, dmChannelID, threadTS, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session thread: %w", err)
	}
	return nil
}
