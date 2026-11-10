package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/subject"
)

const (
	opSessionStart  daemon.Op = "session-start"
	opLogin         daemon.Op = "login"
	opReload        daemon.Op = "reload"
	opWhoami        daemon.Op = "whoami"
	opSend          daemon.Op = "send"
	opReply         daemon.Op = "reply"
	opReact         daemon.Op = "react"
	opUnreact       daemon.Op = "unreact"
	opPickup        daemon.Op = "pickup"
	opStream        daemon.Op = "stream"
	opEdit          daemon.Op = "edit"
	opDelete        daemon.Op = "delete"
	opThread        daemon.Op = "thread"
	opHistory       daemon.Op = "history"
	opFile          daemon.Op = "file"
	opChannels      daemon.Op = "channels"
	opUsers         daemon.Op = "users"
	opDMPeer        daemon.Op = "dm-peer"
	opChannelInfo   daemon.Op = "channel-info"
	opDMOwed        daemon.Op = "dm-owed"
	opRedrawPRCards daemon.Op = "redraw-pr-cards"
	opFollow        daemon.Op = "follow"
	opHoldDelivery  daemon.Op = "hold-delivery"
	opUnfollow      daemon.Op = "unfollow"
	opFollows       daemon.Op = "follows"
	opThreadSeen    daemon.Op = "thread-seen"
	opDMStatus      daemon.Op = "dm-status"
	opCardAnswer    daemon.Op = "card-answer"
	opCardCopy      daemon.Op = "card-copy"
	opRevoke        daemon.Op = "revoke"
	opAgentSubject  daemon.Op = "agent-subject"

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

	sessionMessageWatches = 500

	defaultThreadLimit  = 50
	defaultHistoryLimit = 20
	historyBudget       = 25 * time.Second
)

type opBody struct {
	StreamPID int               `json:"stream_pid"`
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
	RootOnly  bool              `json:"root_only"`
	Users     []string          `json:"users"`
	Topic     string            `json:"topic"`
	Prefix    string            `json:"prefix"`
	Query     string            `json:"query"`
	FileID    string            `json:"file_id"`
	Out       string            `json:"out"`
	Broadcast bool              `json:"broadcast"`
	NoWatch   bool              `json:"no_watch"`

	Raw       bool     `json:"raw"`
	Agent     string   `json:"agent"`
	Mirrored  bool     `json:"mirrored"`
	Waits     string   `json:"waits"`
	Label     string   `json:"label"`
	Other     bool     `json:"other"`
	Settled   bool     `json:"settled"`
	Withdrawn bool     `json:"withdrawn"`
	Via       string   `json:"via"`
	Surface   string   `json:"surface"`
	About     string   `json:"about"`
	TapPRs    []string `json:"tap_prs"`
	PR        string   `json:"pr"`
	Plan      string   `json:"plan"`
	Tasks     []task   `json:"tasks"`
	Stop      bool     `json:"stop"`
	Done      bool     `json:"done"`
	State     string   `json:"state"`
	Recipient string   `json:"recipient"`
	Verdict   string   `json:"verdict"`
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
	Warning    string              `json:"warning,omitempty"`
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
	TS         string          `json:"ts"`
	ThreadTS   string          `json:"thread_ts,omitempty"`
	User       string          `json:"user"`
	UserName   string          `json:"user_name"`
	FromUser   bool            `json:"from_user,omitempty"`
	FromClaude bool            `json:"from_claude"`
	FromBot    bool            `json:"from_bot,omitempty"`
	Text       string          `json:"text"`
	Files      []messageFile   `json:"files,omitempty"`
	Shares     []sharedMessage `json:"shares,omitempty"`
	Unfurls    []string        `json:"unfurls,omitempty"`
	Buttons    []string        `json:"buttons,omitempty"`

	Raw json.RawMessage `json:"raw,omitempty"`
}

type messageFile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Mimetype  string `json:"mimetype"`
	Permalink string `json:"permalink"`
	Path      string `json:"path,omitempty"`
	PathError string `json:"path_error,omitempty"`
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
	TS          string          `json:"ts"`
	User        string          `json:"user"`
	UserName    string          `json:"user_name"`
	FromUser    bool            `json:"from_user,omitempty"`
	FromClaude  bool            `json:"from_claude"`
	Text        string          `json:"text"`
	Permalink   string          `json:"permalink"`
	ReplyCount  int             `json:"reply_count,omitempty"`
	LatestReply string          `json:"latest_reply,omitempty"`
	Files       []messageFile   `json:"files,omitempty"`
	Shares      []sharedMessage `json:"shares,omitempty"`
	Unfurls     []string        `json:"unfurls,omitempty"`

	Raw json.RawMessage `json:"raw,omitempty"`
}

type historyResult struct {
	ChannelID string           `json:"channel_id"`
	Messages  []historyMessage `json:"messages"`
	Partial   bool             `json:"partial,omitempty"`
}

type channelInfo struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	PreviousNames []string `json:"previous_names,omitempty"`
	Created       int64    `json:"created"`
	ExtShared     bool     `json:"ext_shared,omitempty"`
}

type dmOwedResult struct {
	Replies       []owedReply `json:"replies"`
	HolderSession string      `json:"holder_session"`
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
	Own       bool     `json:"own,omitempty"`
}

type followResult struct {
	WatchID   int64               `json:"watch_id"`
	ChannelID string              `json:"channel_id"`
	Joined    bool                `json:"joined"`
	Cursor    string              `json:"cursor"`
	Channel   daemon.ChannelState `json:"channel,omitempty"`
	HeldBy    string              `json:"held_by,omitempty"`
}

type unfollowResult struct {
	Stopped   bool `json:"stopped"`
	Remaining int  `json:"remaining"`
}

type followedWatch struct {
	WatchID   int64  `json:"watch_id"`
	Kind      string `json:"kind"`
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	Owner     string `json:"owner,omitempty"`
	Created   int64  `json:"created"`
}

type followsResult struct {
	Session string          `json:"session"`
	Watches []followedWatch `json:"watches"`
}

type whoami struct {
	identity
	Username string `json:"username"`
	Socket   string `json:"socket"`
}

func newWhoami(id identity, socket string) whoami {
	id.Config.GitHub, id.Config.Buildkite = id.Config.GitHub.redacted(), id.Config.Buildkite.redacted()
	id.Config.Pictures.Secret = ""
	return whoami{identity: id, Username: id.username(), Socket: socket}
}

func (a *app) register(s *daemon.Server) {
	a.channelState = func(hc daemon.HandlerCtx, subjectID string) daemon.ChannelState {
		return s.ChannelState(hc, subjectID, channelConsumer)
	}
	s.Register(opSessionStart, a.handleSessionStart)
	s.Register(opLogin, a.handleLogin)
	s.Register(opReload, a.handleReload)
	s.Register(opWhoami, a.handleWhoami)
	s.Register(opSend, a.handlePost(false))
	s.Register(opReply, a.handlePost(true))
	s.Register(opReact, a.handleReaction(actionReact, "already_reacted", slackAPI.AddReactionContext))
	s.Register(opUnreact, a.handleReaction(actionUnreact, "no_reaction", slackAPI.RemoveReactionContext))
	s.Register(opPickup, a.handlePickup)
	s.Register(opStream, a.handleStream)
	s.Register(opEdit, a.handleEdit)
	s.Register(opDelete, a.handleDelete)
	s.Register(opApplyRead, a.handleApplyRead)
	s.Register(opApplyChange, a.handleApplyChange)
	s.Register(opThread, a.handleThread)
	s.Register(opHistory, a.handleHistory)
	s.Register(opFile, a.handleFile)
	s.Register(opChannels, a.handleChannels)
	s.Register(opUsers, a.handleUsers)
	s.Register(opDMPeer, a.handleDMPeer)
	s.Register(opChannelInfo, a.handleChannelInfo)
	s.Register(opDMOwed, a.handleDMOwed)
	s.Register(opDriveCards, a.handleDriveCards)
	s.Register(opCanvas, a.handleCanvas)
	s.Register(opRedrawPRCards, a.handleRedrawPRCards)
	s.Register(opFollow, a.handleFollow)
	s.Register(opHoldDelivery, a.handleHoldDelivery)
	s.Register(opUnfollow, a.handleUnfollow)
	s.Register(opFollows, a.handleFollows)
	s.Register(opThreadSeen, a.handleThreadSeen)
	s.Register(opDMStatus, a.handleDMStatus)
	s.Register(opCardAnswer, a.handleCardAnswer)
	s.Register(opCardCopy, a.handleCardCopy)
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

func okUnlessUndrawn(v any, undrawn error) daemon.Reply {
	if undrawn != nil {
		return fail(undrawn.Error())
	}
	return ok(v)
}

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

type sessionStartResult struct {
	WatchesBack int64 `json:"watches_back"`
}

func (a *app) handleSessionStart(hc daemon.HandlerCtx) daemon.Reply {
	sub, resumed, err := hc.Subjects.Start(hc.Ctx, hc.Window, hc.Scope, "", lifecycle, false)
	if err != nil {
		return fail(err.Error())
	}
	var res sessionStartResult
	if resumed {
		if res.WatchesBack, err = a.watchesBack(hc.Ctx, hc.DB, sub.ID); err != nil {
			return fail(err.Error())
		}
	}
	reply := ok(res)
	reply.SubjectID, reply.HTTPPort = sub.ID, hc.HTTPPort
	return reply
}

func (a *app) watchesBack(ctx context.Context, db *sql.DB, subjectID string) (int64, error) {
	id, err := loadIdentity(ctx, db)
	if errors.Is(err, errNoIdentity) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	back, err := reviveWatches(ctx, db, subjectID, id.DMChannelID)
	if err != nil || back == 0 {
		return 0, err
	}
	a.poller.reconcileAll()
	return back, nil
}

func reviveWatches(ctx context.Context, db *sql.DB, subjectID, dmChannelID string) (int64, error) {
	res, err := db.ExecContext(ctx, `UPDATE watches SET status = 'active' WHERE id IN (
		SELECT MAX(o.id) FROM watches o
		WHERE o.subject_id = ? AND o.owner_agent_id = ? AND o.status = 'orphaned'
			AND NOT EXISTS (SELECT 1 FROM watches a WHERE a.subject_id = o.subject_id AND a.owner_agent_id = o.owner_agent_id
				AND a.channel_id = o.channel_id AND a.thread_ts = o.thread_ts AND a.status = 'active')
			AND NOT (o.kind = 'channel' AND o.channel_id = ? AND EXISTS (SELECT 1 FROM watches h
				WHERE h.channel_id = o.channel_id AND h.thread_ts = '' AND h.kind = 'channel' AND h.status = 'active'))
		GROUP BY o.channel_id, o.thread_ts)`, subjectID, agent.TopLevel, dmChannelID)
	if err != nil {
		return 0, fmt.Errorf("bring back watches: %w", err)
	}
	return res.RowsAffected()
}

func (a *app) handleLogin(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.Token == "" || b.Email == "" || b.RepoRoot == "" || b.CaptHook == "" {
		return fail("login needs a token, an email, a repo_root, and a capt_hook_path")
	}
	if err := b.Config.validate(); err != nil {
		return fail("login: " + err.Error())
	}
	if b.Config.Avatars.enabled() && (b.AvatarKey == "" || b.AvatarSec == "") {
		return fail("login needs an avatar_key_id and an avatar_secret when the config sets [avatars]")
	}
	if b.Config.Pictures.enabled() && (b.Config.Pictures.KeyID == "" || b.Config.Pictures.Secret == "") {
		return fail("login needs the picture bucket's key when the config sets [pictures]")
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
	a.cards.usePictures(id.Config.Pictures)
	a.poller.reconcileAll()
	a.socket.restart()
	return ok(newWhoami(id, a.socket.status()))
}

func (a *app) handleReload(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if err := b.Config.validate(); err != nil {
		return fail("reload: " + err.Error())
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	if b.Config.Avatars.enabled() && (id.AvatarKeyID == "" || id.AvatarSecret == "") {
		return fail("reload: the config sets [avatars], and the last login read no avatar key: run `cc-slack login`")
	}
	b.Config.Pictures.KeyID, b.Config.Pictures.Secret = id.Config.Pictures.KeyID, id.Config.Pictures.Secret
	if b.Config.Pictures.enabled() && b.Config.Pictures.KeyID == "" {
		return fail("reload: the config sets [pictures], and the last login read no picture key: run `cc-slack login`")
	}
	avatarsChanged := b.Config.Avatars != id.Config.Avatars
	id.Config = b.Config
	if avatarsChanged {
		if id.AvatarURL, err = a.avatar(hc.Ctx, id); err != nil {
			return fail(fmt.Sprintf("reload: agent avatar: %v", err))
		}
	}
	if err := saveIdentity(hc.Ctx, hc.DB, id); err != nil {
		return fail(err.Error())
	}
	a.cards.usePictures(id.Config.Pictures)
	a.poller.reconcileAll()
	a.socket.restart()
	return ok(newWhoami(id, a.socket.status()))
}

func (a *app) handleWhoami(hc daemon.HandlerCtx) daemon.Reply {
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	return ok(newWhoami(id, a.socket.status()))
}

func (a *app) handlePost(requireThread bool) daemon.HandlerFunc {
	return func(hc daemon.HandlerCtx) daemon.Reply {
		b, err := decodeBody(hc)
		if err != nil {
			return fail(err.Error())
		}
		hc.Ctx = withPostWarnings(hc.Ctx)
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
		texts, _ := a.linkPRs(hc.Ctx, id, hc.Env.Scope, b.Text)
		text, previews, ready := a.linkPreviews(hc.Ctx, id, channelID, texts[0], nil)
		asked := slices.Clone(b.TapPRs)
		b.TapPRs = append(b.TapPRs, ready...)
		if len(previews) > 0 {
			extra = append(extra, slack.MsgOptionAttachments(previews...))
		}
		var res postResult
		err = a.writeAndRetry(hc.Ctx, api, id, channelID, func() (err error) {
			res, err = postAs(hc.Ctx, api, hc.DB, a.avatar, a.poller.log, a.now(), id, channelID, b.ThreadTS, text, blocks, b.Files, extra...)
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
		undrawn := a.attachTapPRs(hc, sub, id, res.ChannelID, res.ThreadTS, res.TS, b, asked)
		res.Warning = postWarningsIn(hc.Ctx)
		if b.NoWatch {
			return okUnlessUndrawn(res, undrawn)
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
		return okUnlessUndrawn(a.reportChannel(hc, sub.ID, res), undrawn)
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

func (a *app) post(ctx context.Context, db *sql.DB, id identity, channelID, threadTS, text string, blocks []slack.Block, files []string, extra ...slack.MsgOption) (postResult, error) {
	blocks = fitHeaders(blocks)
	if err := checkBlocks(blocks); err != nil {
		return postResult{}, err
	}
	res, err := postAs(ctx, a.newAPI(id.Token), db, a.avatar, a.poller.log, a.now(), id, channelID, threadTS, text, blocks, files, extra...)
	if err == nil && threadTS != "" {
		a.wrote(ctx, res.ChannelID, threadTS)
		if res.ChannelID == id.DMChannelID {
			if err := a.owed.answered(ctx, threadTS, res.TS); err != nil {
				a.poller.log.Printf("%v", err)
			}
		}
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
	ch, ts, err := postMessage(ctx, api, db, logger, id, channelID, threadTS, withoutHidden(text), blocks, files, extra...)
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
	record := postRecorder(ctx, db, logger)
	ch, ts, err := publish(ctx, api, record, id, channelID, threadTS, text, blocks, files, extra...)
	if err != nil {
		return "", "", err
	}
	record(ch, ts)
	return ch, ts, nil
}

func postRecorder(ctx context.Context, db *sql.DB, logger *log.Logger) func(channelID, ts string) {
	return func(channelID, ts string) {
		if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO posts(channel_id, ts) VALUES (?, ?)`, channelID, ts); err != nil {
			logger.Printf("record post %s/%s, the poller may route it back: %v", channelID, ts, err)
		}
	}
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

func publish(ctx context.Context, api slackAPI, record func(channelID, ts string), id identity, channelID, threadTS, text string, blocks []slack.Block, files []string, extra ...slack.MsgOption) (string, string, error) {
	if len(files) == 0 {
		text, blocks = fitText(text, blocks)
		opts := id.postOptions(text, threadTS, extra...)
		if len(blocks) > 0 {
			opts = append(opts, slack.MsgOptionBlocks(blocks...))
		}
		if linksBeyondDM(id.DMChannelID, text, blocks) || (channelID == id.DMChannelID && linksTo(text, blocks, alertLink)) {
			opts = append(opts, slack.MsgOptionEnableLinkUnfurl())
		}
		return api.PostMessageContext(ctx, channelID, opts...)
	}
	images, err := allImages(files)
	if err != nil {
		return "", "", err
	}
	if images {
		return postImages(ctx, api, record, id, channelID, threadTS, text, blocks, files, extra...)
	}
	return shareFiles(ctx, api, id, channelID, threadTS, text, files)
}

func linksTo(text string, blocks []slack.Block, link *regexp.Regexp) bool {
	return link.MatchString(text) || slices.ContainsFunc(blockTexts(slack.Blocks{BlockSet: blocks}), link.MatchString)
}

// Slack draws a linked message as a share with no buttons, so a link to one of
// his DM cards stays a link and its answers reach him through the card's copy.
func linksBeyondDM(dm, text string, blocks []slack.Block) bool {
	return slices.ContainsFunc(append([]string{text}, blockTexts(slack.Blocks{BlockSet: blocks})...), func(s string) bool {
		return slices.ContainsFunc(messageLink.FindAllString(s, -1), func(link string) bool {
			ref, err := parsePermalink(link)
			return err == nil && ref.channelID != dm
		})
	})
}

func checkMessage(b opBody) ([]slack.Block, error) {
	if len(b.Blocks) > 0 && b.Text == "" {
		return nil, errors.New("blocks need text too: Slack shows the text in notifications and where blocks cannot render")
	}
	if err := checkAgent(b.Agent); err != nil {
		return nil, err
	}
	for _, raw := range b.TapPRs {
		if pr, ok := parseRef(raw); !ok || pr.Kind != refPR {
			return nil, fmt.Errorf("tap_prs takes pull request URLs, not %q", raw)
		}
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
	return a.changeOwnMessage(hc, b, approveEdit, actionEdit, func(ctx context.Context, api slackAPI, id identity, channelID string) error {
		texts, _ := a.linkPRs(ctx, id, hc.Env.Scope, withoutHidden(b.Text))
		return a.cards.editByHand(channelID, b.TS, func() error {
			return updateMessage(ctx, api, channelID, b.TS, texts[0], blocks)
		})
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
	return a.changeOwnMessage(hc, b, approveDelete, actionDelete, func(ctx context.Context, api slackAPI, _ identity, channelID string) error {
		if _, _, err := api.DeleteMessageContext(ctx, channelID, b.TS); err != nil {
			return err
		}
		return a.owed.forget(ctx, b.TS)
	})
}

func (a *app) changeOwnMessage(hc daemon.HandlerCtx, b opBody, verb string, act action, change func(context.Context, slackAPI, identity, string) error) daemon.Reply {
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
		return change(hc.Ctx, api, id, channelID)
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

func updateMessage(ctx context.Context, api slackAPI, channelID, ts, text string, blocks []slack.Block, extra ...slack.MsgOption) error {
	blocks = fitHeaders(blocks)
	if err := checkBlocks(blocks); err != nil {
		return err
	}
	text, blocks = fitText(text, blocks)
	_, _, _, err := api.UpdateMessageContext(ctx, channelID, ts, append([]slack.MsgOption{
		slack.MsgOptionText(text, false), slack.MsgOptionBlocks(blocks...), slack.MsgOptionAttachments([]slack.Attachment{}...), slack.MsgOptionDisableLinkUnfurl()}, extra...)...)
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
		if act == actionReact && answeringReactions[name] && channelID == id.DMChannelID {
			if err := a.owed.reacted(hc.Ctx, b.TS); err != nil {
				return fail(err.Error())
			}
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
	var raw map[string]json.RawMessage
	if b.Raw {
		if raw, err = api.RawMessages(hc.Ctx, "conversations.replies", channelID, b.ThreadTS); err != nil {
			return fail(fallback(actionRead, err))
		}
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
			Shares: a.sharedMessages(hc.Ctx, api, m), Unfurls: linkUnfurls(m), Buttons: messageButtons(m), Raw: raw[m.Timestamp],
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
	ctx, cancel := context.WithTimeout(hc.Ctx, historyBudget)
	defer cancel()
	msgs, partial, err := fetchRecentTopLevel(ctx, api, channelID, limit)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	var raw map[string]json.RawMessage
	if b.Raw {
		if raw, err = api.RawMessages(ctx, "conversations.history", channelID, ""); err != nil {
			return fail(fallback(actionRead, err))
		}
	}
	out := make([]historyMessage, 0, len(msgs))
	for _, m := range msgs {
		files, err := messageFiles(ctx, api, m)
		if errors.Is(err, context.DeadlineExceeded) {
			partial = true
			break
		}
		if err != nil {
			return fail(fallback(actionRead, err))
		}
		out = append(out, historyMessage{
			TS: m.Timestamp, User: m.User, UserName: a.cache.name(ctx, api, m), FromUser: m.User == id.UserID,
			FromClaude: id.fromClaude(m), Text: messageText(m),
			Permalink: id.permalink(channelID, m.Timestamp, ""), ReplyCount: m.ReplyCount, LatestReply: m.LatestReply,
			Files: files, Shares: a.sharedMessages(ctx, api, m), Unfurls: linkUnfurls(m), Raw: raw[m.Timestamp],
		})
	}
	return ok(historyResult{ChannelID: channelID, Messages: out, Partial: partial})
}

func messageFiles(ctx context.Context, api slackAPI, m slack.Message) ([]messageFile, error) {
	var out []messageFile
	seen := map[string]bool{}
	for _, f := range m.Files {
		seen[f.ID] = true
		out = append(out, attachedFile(ctx, api, &f))
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
		out = append(out, attachedFile(ctx, api, f))
	}
	return out, nil
}

func messageText(m slack.Message) string {
	parts := append([]string{sectionedText(m)}, blockTexts(m.Blocks)...)
	for _, at := range m.Attachments {
		if ref, shared := shareRef(at); shared {
			parts = append(parts, fmt.Sprintf("Shared from %s in <#%s>: %s", shareAuthor(at), ref.channelID, cmp.Or(at.Text, at.Fallback)))
			continue
		}
		parts = append(parts, at.Pretext, at.Title, at.Text, at.TitleLink)
		if at.Title == "" && at.Text == "" {
			parts = append(parts, at.Fallback)
		}
		for _, f := range at.Fields {
			parts = append(parts, strings.TrimPrefix(f.Title+": "+f.Value, ": "))
		}
		parts = append(parts, blockTexts(at.Blocks)...)
		for _, action := range at.Actions {
			parts = append(parts, action.URL)
		}
	}
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) == "" || slices.ContainsFunc(out, func(o string) bool { return strings.Contains(o, p) }) {
			continue
		}
		out = append(slices.DeleteFunc(out, func(o string) bool { return strings.Contains(p, o) }), p)
	}
	return strings.Join(out, "\n")
}

func messageButtons(m slack.Message) []string {
	var labels []string
	add := func(element any) {
		if button, ok := element.(*slack.ButtonBlockElement); ok && button.Text != nil {
			labels = append(labels, button.Text.Text)
		}
	}
	for _, block := range m.Blocks.BlockSet {
		switch b := block.(type) {
		case *slack.ActionBlock:
			if b.Elements != nil {
				for _, element := range b.Elements.ElementSet {
					add(element)
				}
			}
		case *slack.SectionBlock:
			if b.Accessory != nil && b.Accessory.ButtonElement != nil {
				add(b.Accessory.ButtonElement)
			}
		}
	}
	return labels
}

func blockTexts(blocks slack.Blocks) []string {
	var out []string
	text := func(t *slack.TextBlockObject) {
		if t != nil {
			out = append(out, t.Text)
		}
	}
	button := func(e slack.BlockElement) {
		if b, ok := e.(*slack.ButtonBlockElement); ok && b.URL != "" {
			out = append(out, b.URL)
		}
	}
	for _, block := range blocks.BlockSet {
		switch b := block.(type) {
		case *slack.HeaderBlock:
			text(b.Text)
		case *slack.SectionBlock:
			text(b.Text)
			for _, f := range b.Fields {
				text(f)
			}
			if b.Accessory != nil && b.Accessory.ButtonElement != nil {
				button(b.Accessory.ButtonElement)
			}
		case *slack.ContextBlock:
			for _, e := range b.ContextElements.Elements {
				if t, ok := e.(*slack.TextBlockObject); ok {
					text(t)
				}
			}
		case *slack.ActionBlock:
			if b.Elements != nil {
				for _, e := range b.Elements.ElementSet {
					button(e)
				}
			}
		}
	}
	return out
}

func sectionedText(m slack.Message) string {
	prefix, short := strings.CutSuffix(m.Text, "…")
	if !short {
		return m.Text
	}
	var full strings.Builder
	for _, block := range m.Blocks.BlockSet {
		if section, ok := block.(*slack.SectionBlock); ok && section.Text != nil {
			full.WriteString(section.Text.Text)
		}
	}
	if full.Len() <= len(prefix) || !strings.HasPrefix(full.String(), prefix) {
		return m.Text
	}
	return full.String()
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
			out = append(out, channelInfo{ID: c.ID, Name: c.Name, PreviousNames: c.PreviousNames, Created: int64(c.Created), ExtShared: c.IsExtShared})
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

func (a *app) handleRedrawPRCards(hc daemon.HandlerCtx) daemon.Reply {
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(err.Error())
	}
	if len(id.Config.PRLinks.CardCommand) == 0 {
		return fail("redraw-pr-cards needs a [pr_links] card_command on this machine")
	}
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	posted := a.cards.postedPRCards()
	var listing []cardRef
	if b.PR != "" {
		only, found := parseRef(b.PR)
		if !found {
			return fail(fmt.Sprintf("pr takes a pull request URL, not %q", b.PR))
		}
		posted = slices.DeleteFunc(posted, func(card postedPRCard) bool { return !card.carries(only) })
		listing = a.cards.cardsListing(only)
	}
	go func(ctx context.Context) {
		for _, card := range posted {
			a.recardPR(ctx, id, card.card, card.prs[0])
		}
		for _, card := range listing {
			a.poller.redrawWithFreshPRLines(ctx, id, card)
			if err := a.redrawCopies(ctx, id, card); err != nil {
				a.poller.log.Printf("card %s: its copies keep the lines they were drawn with: %v", card.ts, err)
			}
		}
	}(context.WithoutCancel(hc.Ctx))
	return ok(map[string]int{"redrawing": len(posted) + len(listing)})
}

func (a *app) handleHoldDelivery(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	sub, found, err := hc.Subjects.Find(hc.Ctx, hc.Window, hc.Scope)
	if err != nil {
		return fail(err.Error())
	}
	if !found {
		return fail("no cc-slack session for this Claude window, so it has no messages to hold")
	}
	a.poller.holdDelivery(sub.ID, b.StreamPID)
	return ok(map[string]any{"held": sub.ID})
}

func (a *app) handleDMOwed(hc daemon.HandlerCtx) daemon.Reply {
	replies, err := a.owed.list(hc.Ctx)
	if err != nil {
		return fail(err.Error())
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(err.Error())
	}
	holder, err := dmHolder(hc.Ctx, hc.DB, id.DMChannelID)
	if err != nil {
		return fail(err.Error())
	}
	mates, err := loadDriveMates(hc.Ctx, hc.DB, a.cards, id.DMChannelID)
	if err != nil {
		return fail(err.Error())
	}
	others, err := othersCardThreads(hc.Ctx, hc.DB, id.DMChannelID, holder, mates)
	if err != nil {
		return fail(err.Error())
	}
	return ok(dmOwedResult{Replies: slices.DeleteFunc(replies, func(r owedReply) bool { return others[r.ThreadTS] }), HolderSession: holder.session})
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
	return ok(dmPeerResult{ChannelID: b.ChannelID, Peer: peer, PeerName: name, PeerNames: peerNames(hc.Ctx, api, peer, name), Own: b.ChannelID == id.DMChannelID})
}

func (a *app) handleChannelInfo(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.ChannelID == "" {
		return fail("channel-info needs channel_id")
	}
	if held, found := a.channels.Load(b.ChannelID); found {
		return ok(held)
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	c, err := a.newAPI(id.Token).GetConversationInfoContext(hc.Ctx, &slack.GetConversationInfoInput{ChannelID: b.ChannelID})
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	info := channelInfo{ID: c.ID, Name: c.Name, PreviousNames: c.PreviousNames, Created: int64(c.Created), ExtShared: c.IsExtShared}
	a.channels.Store(b.ChannelID, info)
	return ok(info)
}

func peerNames(ctx context.Context, api slackAPI, peer, name string) []string {
	names := []string{name}
	if u, err := api.GetUserInfoContext(ctx, peer); err == nil {
		names = append(names, u.RealName, u.Profile.DisplayName, u.Name)
	}
	return slices.Compact(slices.Sorted(slices.Values(slices.DeleteFunc(names, func(n string) bool { return n == "" }))))
}

var fallbackMarker = regexp.MustCompile(`^\s*_\([^)\n]+['’]s Claude\)_`)

func (id identity) fromClaude(m slack.Message) bool {
	return id.botPosted(m) || fallbackMarker.MatchString(m.Text)
}

func (id identity) botPosted(m slack.Message) bool {
	return (id.BotID != "" && m.BotID == id.BotID) || (id.BotUserID != "" && m.User == id.BotUserID)
}

func (id identity) fromOperator(m slack.Message) bool {
	return m.User == id.UserID && !id.fromClaude(m)
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
	if kind == kindChannel && channelID == id.DMChannelID {
		holder, err := dmHolder(hc.Ctx, hc.DB, channelID)
		if err != nil {
			return fail(err.Error())
		}
		if holder.subject != "" && holder.subject != subjectID {
			return ok(followResult{ChannelID: channelID, HeldBy: holder.session})
		}
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

func claimCursor(ctx context.Context, tx *sql.Tx, channelID, threadTS, oldest string, now time.Time, keep bool) error {
	query := `INSERT INTO cursors(channel_id, thread_ts, oldest_ts, next_poll_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(channel_id, thread_ts) DO UPDATE SET oldest_ts = excluded.oldest_ts, next_poll_at = excluded.next_poll_at, backoff_s = 0`
	if keep {
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

func (a *app) handleFollows(hc daemon.HandlerCtx) daemon.Reply {
	sub, found, err := hc.Subjects.Find(hc.Ctx, hc.Window, hc.Scope)
	if err != nil {
		return fail(err.Error())
	}
	if !found {
		return fail("no cc-slack session for this Claude window, so it follows nothing")
	}
	rows, err := hc.DB.QueryContext(hc.Ctx, `SELECT id, kind, channel_id, thread_ts, owner_agent_id, created_at FROM watches
		WHERE subject_id = ? AND status = 'active' ORDER BY kind, id`, sub.ID)
	if err != nil {
		return fail(fmt.Sprintf("list watches: %v", err))
	}
	defer func() { _ = rows.Close() }()
	res := followsResult{Session: sub.SessionID, Watches: []followedWatch{}}
	for rows.Next() {
		var w followedWatch
		if err := rows.Scan(&w.WatchID, &w.Kind, &w.ChannelID, &w.ThreadTS, &w.Owner, &w.Created); err != nil {
			return fail(fmt.Sprintf("list watches: %v", err))
		}
		res.Watches = append(res.Watches, w)
	}
	if err := rows.Err(); err != nil {
		return fail(fmt.Sprintf("list watches: %v", err))
	}
	return ok(res)
}

func (a *app) handleDMStatus(hc daemon.HandlerCtx) daemon.Reply {
	hc.Ctx = withPostWarnings(hc.Ctx)
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.Card != nil && !b.RootOnly {
		b.Card.fill(b.Text)
		b.Text = ""
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionWrite, err))
	}
	if err := checkDMStatus(b, a.now(), id); err != nil {
		return fail(err.Error())
	}
	blocks, err := checkMessage(b)
	if err != nil {
		return fail(err.Error())
	}
	sub, _, err := hc.Subjects.Start(hc.Ctx, hc.Window, hc.Scope, "", lifecycle, false)
	if err != nil {
		return fail(err.Error())
	}
	id.Agent = b.Agent
	pictures, err := a.uploadCardPictures(hc.Ctx, id, b)
	if err != nil {
		return fail(fallback(actionWrite, err))
	}
	if b.Card != nil {
		b.Card.dropUnopenable(hc.Ctx, id.Config.Links.Tailnet)
	}
	msg, later := a.dmMessageOf(hc, id, b, blocks)
	asked := slices.Clone(b.TapPRs)
	b.TapPRs = append(b.TapPRs, msg.tapPRs...)
	if b.Card != nil && b.Card.Replaces != "" && b.Card.Waits == "" {
		return a.replaceCardReply(hc, sub, id, b, asked, msg.tag, pictures)
	}
	if reply, answered := a.rewordAnswered(hc, sub, id, b); answered {
		return reply
	}
	under, err := a.dmThreadOf(hc, sub, id, b)
	if err != nil {
		return fail(err.Error())
	}
	thread, ts, posted, err := a.dmStatus(hc.Ctx, hc.DB, sub, id, under, msg)
	if errors.Is(err, errAskedAgain) {
		return fail(err.Error())
	}
	if err != nil && !posted {
		return fail(fallback(actionWrite, err))
	}
	if err != nil {
		return fail(err.Error())
	}
	if err := a.postPages(hc, sub, id, thread, ts, msg.text, later); err != nil {
		return fail(err.Error())
	}
	notePostWarning(hc.Ctx, a.linesCarried(hc.Ctx, id, cardRef{id.DMChannelID, thread, ts}, a.cards.continued(id.DMChannelID, thread), b.Card))
	undrawn := a.attachTapPRs(hc, sub, id, id.DMChannelID, thread, ts, b, asked)
	a.showCardPictures(hc.Ctx, hc.DB, id, b.Card, pictures, thread, ts)
	return okUnlessUndrawn(a.dmStatusReply(hc, sub.ID, id, thread, ts, b), undrawn)
}

func (a *app) replaceCardReply(hc daemon.HandlerCtx, sub subject.Subject, id identity, b opBody, asked []string, tag cardTag, pictures []slack.FileSummary) daemon.Reply {
	card := *b.Card
	if len(pictures) > 0 {
		card, _ = a.shareCardPictures(hc.Ctx, hc.DB, id, card, pictures)
	}
	thread, ts, err := a.replaceCard(hc.Ctx, id, card, tag, b.TapPRs)
	if err != nil {
		return fail(fallback(actionWrite, err))
	}
	undrawn := a.attachTapPRs(hc, sub, id, id.DMChannelID, thread, ts, b, asked)
	return okUnlessUndrawn(a.dmStatusReply(hc, sub.ID, id, thread, ts, b), undrawn)
}

func (a *app) rewordAnswered(hc daemon.HandlerCtx, sub subject.Subject, id identity, b opBody) (daemon.Reply, bool) {
	if b.Card == nil || !b.Mirrored || b.Card.Waits == "" {
		return daemon.Reply{}, false
	}
	threads, err := sessionThreads(hc.Ctx, hc.DB, sub.ID, id.DMChannelID)
	if err != nil {
		return fail(err.Error()), true
	}
	if !a.cards.holds(id.DMChannelID, threads, *b.Card) {
		return a.refuseClosedPost(hc.Ctx, id, *b.Card)
	}
	card, reworded := a.cards.reword(id.DMChannelID, threads, *b.Card)
	if !reworded {
		return daemon.Reply{}, false
	}
	if _, err := a.cards.redraw(hc.Ctx, a.newAPI(id.Token), card.channel, card.thread, card.ts, a.now()); err != nil {
		return fail(fmt.Sprintf("edit answered card %q in place: %v", b.Card.Waits, err)), true
	}
	return ok(a.dmStatusReply(hc, sub.ID, id, card.thread, card.ts, b)), true
}

func (a *app) refuseClosedPost(ctx context.Context, id identity, card statusCard) (daemon.Reply, bool) {
	if card.Replaces == "" {
		return daemon.Reply{}, false
	}
	ref, _ := parsePermalink(card.Replaces)
	post, err := messageAt(ctx, a.newAPI(id.Token), id.DMChannelID, ref.threadTS, ref.ts)
	if err != nil {
		return fail(fmt.Sprintf("read the post waiting card %q replaces: %v", card.Waits, err)), true
	}
	if drawn, answers := copiedCard(post.Blocks.BlockSet, ref); drawn == nil || answers > 0 {
		return daemon.Reply{}, false
	}
	return fail(fmt.Errorf("waiting card %q replaces %s, a card with no answer left to tap; %w", card.Waits, card.Replaces, errAskedAgain).Error()), true
}

func (a *app) dmThreadOf(hc daemon.HandlerCtx, sub subject.Subject, id identity, b opBody) (string, error) {
	if b.About == "" {
		return b.ThreadTS, nil
	}
	if b.Card != nil || b.Text == "" {
		return "", errors.New("about puts a text reply in a waiting card's thread; send text, not a card")
	}
	return a.cardThread(hc.Ctx, hc.DB, sub.ID, id, b.About)
}

func (a *app) dmMessageOf(hc daemon.HandlerCtx, id identity, b opBody, blocks []slack.Block) (dmMessage, [][]slack.Block) {
	texts, _ := a.linkPRs(hc.Ctx, id, hc.Env.Scope, withoutHidden(b.Text))
	msg := dmMessage{text: texts[0], blocks: blocks, files: b.Files, card: b.Card, rootOnly: b.RootOnly}
	if b.Card == nil {
		msg.text, msg.previews, msg.tapPRs = a.linkPreviews(hc.Ctx, id, id.DMChannelID, texts[0], nil)
		if len(blocks)+len(b.Files) > 0 || !mdMarks.MatchString(msg.text) && runes(msg.text) <= cardMaxSectionLen {
			return msg, nil
		}
		pages := bodyPages(msg.text, cardMaxPageBlock, cardMaxVisible, true)
		msg.text, msg.blocks = clipRunes(msg.text, cardMaxSectionLen), pages[0]
		return msg, pages[1:]
	}
	texts, _ = a.linkPRs(hc.Ctx, id, hc.Env.Scope, b.Card.Body, b.Card.Title)
	b.Card.Body, msg.previews, msg.tapPRs = a.linkPreviews(hc.Ctx, id, id.DMChannelID, texts[0], b.Card.refIDs())
	msg.tag = gitCardTag(hc.Ctx, hc.Env.Session, hc.Env.Scope)
	a.describeCardPRs(hc.Ctx, id, b.Card)
	return msg, nil
}

func (a *app) postPages(hc daemon.HandlerCtx, sub subject.Subject, id identity, thread, ts, text string, pages [][]slack.Block) error {
	for _, page := range pages {
		if _, _, _, err := a.dmStatus(hc.Ctx, hc.DB, sub, id, thread, dmMessage{text: text, blocks: page}); err != nil {
			return fmt.Errorf("posted the start of this reply at %s, but its next page failed: %w", id.permalink(id.DMChannelID, ts, thread), err)
		}
	}
	return nil
}

func (a *app) replaceCard(ctx context.Context, id identity, card statusCard, tag cardTag, taps []string) (string, string, error) {
	ref, err := parsePermalink(card.Replaces)
	if err != nil {
		return "", "", err
	}
	thread := cmp.Or(ref.threadTS, ref.ts)
	card.Replaces = ""
	api := a.newAPI(id.Token)
	now := a.now()
	tapped := map[string]bool{}
	for _, pr := range taps {
		if found, ok := parseRef(pr); ok {
			tapped[found.key()] = true
		}
	}
	a.cards.keepCarried(id.DMChannelID, thread, tapped, now)
	if a.cards.markReply(id.DMChannelID, thread, ref.ts, func(m *liveMessage) {
		m.Card, m.Tapped, m.Hint, m.Final, m.Unfolded, m.Edited = card, "", "", true, false, false
	}) {
		_, err = a.cards.redraw(ctx, api, id.DMChannelID, thread, ref.ts, now)
	} else {
		v := cardView{tag: tag, at: now, now: now, final: true, pictures: id.Config.Pictures}
		err = updateMessage(ctx, api, id.DMChannelID, ref.ts, card.text(), card.replyBlocks(v), card.entities(card.prEntities(v)...)...)
	}
	if err != nil {
		return "", "", fmt.Errorf("replace card %s in place: %w", ref.ts, err)
	}
	if err := api.RemoveReactionContext(ctx, cardWithdrawnReaction, slack.ItemRef{Channel: id.DMChannelID, Timestamp: ref.ts}); err != nil && !strings.Contains(err.Error(), "no_reaction") {
		a.poller.log.Printf("card %s replaced, but its withdrawn mark stays: %v", ref.ts, err)
	}
	return thread, ref.ts, nil
}

func (a *app) uploadCardPictures(ctx context.Context, id identity, b opBody) ([]slack.FileSummary, error) {
	if b.Card == nil {
		return nil, nil
	}
	images, err := allImages(b.Files)
	if err != nil {
		return nil, err
	}
	if !images {
		return nil, errors.New("a card attaches PNG, JPEG, GIF, or WebP images only")
	}
	var drawn []string
	for _, artifact := range b.Card.Artifacts {
		if artifact.Image != "" {
			drawn = append(drawn, artifact.Image)
		}
	}
	if images, err = allImages(drawn); err != nil {
		return nil, err
	}
	if !images {
		return nil, errors.New("an artifact's image is a PNG, JPEG, GIF, or WebP")
	}
	pictures := slices.Concat(b.Files, drawn)
	if b.Card.ImagesHash, err = hashFiles(pictures); err != nil {
		return nil, err
	}
	if shown := a.cards.shownImages(id.DMChannelID, *b.Card); shown != nil {
		b.Card.Images = shown
		return nil, nil
	}
	if bucket := id.Config.Pictures; bucket.enabled() {
		b.Card.Images, err = bucket.upload(ctx, id.TeamID, pictures)
		return nil, err
	}
	return uploadFiles(ctx, a.newAPI(id.Token), pictures)
}

func hashFiles(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", nil
	}
	sum := sha256.New()
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		file := sha256.Sum256(body)
		sum.Write(file[:])
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func (a *app) shareCardPictures(ctx context.Context, db *sql.DB, id identity, card statusCard, files []slack.FileSummary) (statusCard, bool) {
	shelf, err := a.pictureShelf(ctx, db, id)
	if err == nil {
		err = shareProcessed(ctx, a.newAPI(id.Token), postRecorder(ctx, db, a.poller.log), id, id.DMChannelID, shelf, files)
	}
	if err != nil {
		notePostWarning(ctx, "the card posted without its pictures: "+err.Error())
		return card, false
	}
	card.Images = sharedImages(files)
	return card, true
}

func (a *app) pictureShelf(ctx context.Context, db *sql.DB, id identity) (string, error) {
	a.cards.shelfMu.Lock()
	defer a.cards.shelfMu.Unlock()
	if ts := a.cards.shelf(id.DMChannelID); ts != "" {
		return ts, nil
	}
	res, err := a.post(ctx, db, id, id.DMChannelID, "", cardShelfText, nil, nil)
	if err != nil {
		return "", fmt.Errorf("post the thread that keeps card pictures: %w", err)
	}
	a.cards.keepShelf(id.DMChannelID, res.TS)
	return res.TS, nil
}

func (a *app) showCardPictures(ctx context.Context, db *sql.DB, id identity, card *statusCard, files []slack.FileSummary, thread, ts string) {
	if len(files) == 0 || thread == "" {
		return
	}
	shown, shared := a.shareCardPictures(ctx, db, id, *card, files)
	if !shared {
		return
	}
	at := cmp.Or(ts, thread)
	if !a.cards.showPictures(id.DMChannelID, thread, at, shown) {
		notePostWarning(ctx, "the card posted without its pictures: it left the live cards before they were shared")
		return
	}
	api := a.newAPI(id.Token)
	if err := whileProcessing(ctx, func() error {
		_, err := a.cards.redraw(ctx, api, id.DMChannelID, thread, at, a.now())
		return err
	}); err != nil {
		notePostWarning(ctx, "the card posted without its pictures: "+err.Error())
	}
}

func (a *app) dmStatusReply(hc daemon.HandlerCtx, subjectID string, id identity, thread, ts string, b opBody) map[string]string {
	res := map[string]string{"channel_id": id.DMChannelID, "thread_ts": thread, "channel": string(a.channelState(hc, subjectID))}
	if ts != "" {
		res["ts"], res["permalink"] = ts, id.permalink(id.DMChannelID, ts, thread)
	}
	if handPostedQuestion(b) {
		res["warning"] = questionOnBoard
	}
	if refusal := postWarningsIn(hc.Ctx); refusal != "" {
		res["warning"] = strings.TrimSpace(res["warning"] + " " + refusal)
	}
	return res
}

func (a *app) handleCardAnswer(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	switch {
	case strings.TrimSpace(b.Label) == "":
		return fail("card-answer needs the answer's label, or a write-in with other")
	case b.URL == "":
		return fail("card-answer needs url, the Slack card's permalink")
	case b.Via == "":
		return fail("card-answer needs via, the url of the surface the answer came from")
	case b.Withdrawn && (b.Settled || b.Other):
		return fail("card-answer withdrawn takes the reason as its label, without settled or other")
	case b.Settled && !b.Other:
		return fail("card-answer settled takes a written-in outcome with other")
	case b.Surface != "" && b.Surface != surfaceDashboard:
		return fail("card-answer surface is dashboard, or empty for a cc-present board")
	}
	label := b.Label
	if b.Other || b.Withdrawn {
		label = strings.TrimSpace(withoutHidden(label))
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(err.Error())
	}
	res, err := a.poller.answerElsewhere(hc.Ctx, id, b.Waits, b.URL, label, b.Other, b.Settled, b.Withdrawn, b.Via, b.Surface)
	if err != nil {
		return fail(err.Error())
	}
	return ok(res)
}

func checkDMStatus(b opBody, now time.Time, id identity) error {
	tailnet := id.Config.Links.Tailnet
	switch {
	case b.Card != nil && (b.Text != "" || len(b.Blocks) > 0 || (len(b.Files) > 0 && b.Card.Waits == "")):
		return errors.New("dm-status takes a card, or blocks and files, never both; only a waiting card carries images")
	case b.Card == nil && b.Text == "" && len(b.Files) == 0:
		return errors.New("dm-status needs a card, text, or files")
	case b.RootOnly && (b.Card == nil || b.ThreadTS != ""):
		return errors.New("dm-status root_only redraws the session's root card; it takes a card and no thread_ts")
	case b.ThreadTS != "" && !slackTSPattern.MatchString(b.ThreadTS):
		return fmt.Errorf("dm-status thread_ts %q is not a Slack ts", b.ThreadTS)
	case b.Card != nil && !b.Card.supersedesIn(id.DMChannelID):
		return errors.New("a card supersedes only the bot's earlier posts in your DM with the app")
	case b.Card != nil && b.Mirrored:
		return b.Card.checkMirrored(now, b.RootOnly, tailnet)
	case b.Card != nil:
		return b.Card.check(now, b.RootOnly, tailnet)
	}
	return nil
}

var errAskedAgain = errors.New("ask again under a new waits id")

type dmMessage struct {
	text     string
	blocks   []slack.Block
	files    []string
	card     *statusCard
	tag      cardTag
	rootOnly bool
	previews []slack.Attachment
	tapPRs   []string
}

func (a *app) dmStatus(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, under string, msg dmMessage) (string, string, bool, error) {
	if under != "" {
		if err := a.bindSessionThread(ctx, db, sub, id, under); err != nil {
			return "", "", false, err
		}
	}
	if msg.card != nil {
		thread, ts, posted, err := a.cardStatus(ctx, db, sub, id, under, msg)
		if err == nil && posted && len(msg.previews) > 0 {
			if _, err := a.post(ctx, db, id, id.DMChannelID, thread, linkPreviewsLead, nil, nil, slack.MsgOptionAttachments(msg.previews...)); err != nil {
				a.poller.log.Printf("previews under card %s: %v", ts, err)
			}
		}
		return thread, ts, posted, err
	}
	res, err := a.post(ctx, db, id, id.DMChannelID, under, msg.text, msg.blocks, msg.files, slack.MsgOptionAttachments(msg.previews...))
	if err != nil {
		return "", "", false, err
	}
	if under != "" {
		return under, res.TS, true, nil
	}
	if err := a.bindSessionThread(ctx, db, sub, id, res.TS); err != nil {
		return "", res.TS, true, err
	}
	return res.TS, res.TS, true, nil
}

func (a *app) cardStatus(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, under string, msg dmMessage) (string, string, bool, error) {
	thread, ts, posted, again, err := a.placeStatusCard(ctx, db, sub, id, under, msg)
	if err == nil && again {
		if err := a.askAgain(ctx, db, id, cardRef{id.DMChannelID, thread, ts}); err != nil {
			a.poller.log.Printf("card %s redrawn in place, but not posted again beside it: %v", ts, err)
		}
	}
	return thread, ts, posted, err
}

func (a *app) placeStatusCard(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, under string, msg dmMessage) (string, string, bool, bool, error) {
	cards := a.cards
	cards.mu.Lock()
	defer cards.mu.Unlock()
	threads, err := sessionThreads(ctx, db, sub.ID, id.DMChannelID)
	if err != nil {
		return "", "", false, false, err
	}
	if msg.rootOnly {
		thread, posted, err := a.rootCard(ctx, db, sub, id, threads, msg)
		return thread, "", posted, false, err
	}
	card := *msg.card
	replaces := ""
	if card.Replaces != "" {
		ref, err := parsePermalink(card.Replaces)
		if err != nil {
			return "", "", false, false, err
		}
		under, replaces, card.Replaces = cmp.Or(ref.threadTS, ref.ts), ref.ts, ""
	}
	place, err := cards.placeFor(id.DMChannelID, under, replaces, threads, card)
	if err != nil {
		return "", "", false, false, err
	}
	if place.nothing {
		return "", "", false, false, nil
	}
	t, prior, asked, superseded := place.thread, place.prior, place.asked, place.superseded
	again := place.redraws(card)
	t.apply(card, msg.tag, a.now(), false)
	placed, err := a.placeCard(ctx, db, id, t, asked)
	if err != nil {
		return "", "", false, false, err
	}
	if place.revived && asked != nil {
		if err := a.newAPI(id.Token).RemoveReactionContext(ctx, cardWithdrawnReaction, slack.ItemRef{Channel: id.DMChannelID, Timestamp: asked.TS}); err != nil && !strings.Contains(err.Error(), "no_reaction") {
			a.poller.log.Printf("card %s asked again in place, but its withdrawn mark stays: %v", asked.TS, err)
		}
	}
	t.keep(card, placed)
	if superseded != nil {
		cards.forget(prior, superseded.TS)
	}
	cards.commit(t)
	if superseded != nil {
		if _, _, err := a.newAPI(id.Token).DeleteMessageContext(ctx, id.DMChannelID, superseded.TS); err != nil {
			a.poller.log.Printf("card %s/%s superseded by %s not deleted: %v", id.DMChannelID, superseded.TS, placed.TS, err)
		}
	}
	if asked == nil {
		a.poller.noteSuperseded(ctx, id, card.Supersedes, "↪ *"+mrkdwnEscapes.Replace(card.Title)+"* is asked again as a card with buttons: "+
			slackLink(id.permalink(id.DMChannelID, placed.TS, t.Thread), "answer it there"))
	}
	if prior != nil && asked != nil {
		return t.Thread, placed.TS, true, again, nil
	}
	if err := a.bindSessionThread(ctx, db, sub, id, t.Thread); err != nil {
		return "", placed.TS, true, false, err
	}
	return t.Thread, placed.TS, true, again, nil
}

type cardPlace struct {
	thread            *liveThread
	prior             *liveThread
	asked, superseded *liveMessage
	nothing, revived  bool
}

func (p cardPlace) redraws(card statusCard) bool {
	return p.asked != nil && card.Waits != "" && (p.revived || p.asked.Card.Waits != "" && !sameCard(p.asked.Card, card))
}

func (l *liveCards) placeFor(channel, under, replaces string, threads []string, card statusCard) (cardPlace, error) {
	place := cardPlace{thread: l.draft(channel, under), prior: l.holding(channel, threads, card)}
	if place.prior != nil {
		answer, done := place.prior.Answers[card.Waits]
		if card.Waits != "" && done && !answer.Withdrawn {
			return place, fmt.Errorf("waiting card %q was answered with %q at %s; %w",
				card.Waits, answer.Label, answer.At.In(pacific).Format(cardTimeLayout), errAskedAgain)
		}
		place.revived = card.Waits != "" && done
		place.asked = place.prior.kept(card)
	}
	switch {
	case place.asked != nil && card.gainsRows(place.asked.Card):
		place.superseded, place.asked = place.asked, nil
		place.thread.Answers = maps.Clone(place.prior.Answers)
	case place.asked != nil:
		place.thread = l.draft(place.prior.Channel, place.prior.Thread)
	case card.Group != "" && len(card.Rows) == 0:
		place.nothing = true
	}
	if place.asked == nil && place.superseded == nil && replaces != "" {
		place.asked = &liveMessage{TS: replaces}
	}
	if place.revived {
		delete(place.thread.Answers, card.Waits)
	}
	return place, nil
}

func (a *app) placeCard(ctx context.Context, db *sql.DB, id identity, t *liveThread, asked *liveMessage) (*liveMessage, error) {
	placed := t.Reply
	text, blocks := t.replyMessage(t.Reply, a.now(), a.cards.canvas())
	prs := placed.Card.prEntities(t.view(placed, a.now(), a.cards.canvas()))
	carrier := asked != nil && asked.TS == t.Thread
	var more [][]slack.Block
	if carrier {
		var carried []slack.WorkObjectEntity
		blocks, more, carried = a.cards.withCarried(t, blocks, a.now())
		prs = append(prs, carried...)
	}
	entities := placed.Card.entities(prs...)
	write := func(do func() error) error {
		if !placed.Card.hostsFiles() {
			return do()
		}
		return whileProcessing(ctx, do)
	}
	if asked != nil {
		placed.TS = asked.TS
		if err := write(func() error {
			return updateMessage(ctx, a.newAPI(id.Token), id.DMChannelID, asked.TS, text, blocks, entities...)
		}); err != nil {
			return nil, fmt.Errorf("edit card %q in place: %w", cmp.Or(placed.Card.Waits, placed.Card.Group, placed.Card.Key), err)
		}
		if !carrier {
			return placed, nil
		}
		continued, err := a.cards.continueCarried(ctx, a.newAPI(id.Token), id.DMChannelID, t.Thread, t.Continued, more)
		t.Continued = continued
		if err != nil {
			t.Stale = true
			a.poller.log.Printf("card %s/%s: the cards it carries past Slack's %d blocks did not all reach its thread, so the next redraw tries again: %v", id.DMChannelID, asked.TS, cardMaxBlocks, err)
		}
		return placed, nil
	}
	var res postResult
	if err := write(func() (err error) {
		res, err = a.post(ctx, db, id, id.DMChannelID, t.Thread, text, blocks, nil, entities...)
		return err
	}); err != nil {
		return nil, err
	}
	placed.TS, t.Thread = res.TS, cmp.Or(t.Thread, res.TS)
	return placed, nil
}

func (a *app) rootCard(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, threads []string, msg dmMessage) (string, bool, error) {
	cards := a.cards
	now := a.now()
	prior := cards.rooted(id.DMChannelID, threads)
	if prior == nil {
		t := &liveThread{Channel: id.DMChannelID}
		t.apply(*msg.card, msg.tag, now, true)
		text, blocks := t.rootMessage(now, cards.canvas())
		res, err := a.post(ctx, db, id, id.DMChannelID, "", text, blocks, nil, msg.card.entities()...)
		if err != nil {
			return "", false, err
		}
		t.Thread, t.Root.TS = res.TS, res.TS
		cards.commit(t)
		if err := a.bindSessionThread(ctx, db, sub, id, t.Thread); err != nil {
			return "", true, err
		}
		return t.Thread, true, nil
	}
	t := cards.draft(prior.Channel, prior.Thread)
	t.apply(*msg.card, msg.tag, now, true)
	text, blocks := t.rootMessage(now, cards.canvas())
	if err := updateMessage(ctx, a.newAPI(id.Token), id.DMChannelID, t.Thread, text, blocks, msg.card.entities()...); err != nil {
		return "", false, fmt.Errorf("redraw status card root %s/%s: %w", id.DMChannelID, t.Thread, err)
	}
	cards.commit(t)
	return t.Thread, true, nil
}

func (a *app) bindSessionThread(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, thread string) error {
	if err := a.openSessionThread(ctx, db, sub, id.DMChannelID, thread); err != nil {
		return err
	}
	a.poller.nudge(cursorKey{id.DMChannelID, thread}, cursorKey{id.DMChannelID, ""})
	return nil
}

func (a *app) cardThread(ctx context.Context, db *sql.DB, subjectID string, id identity, waits string) (string, error) {
	threads, err := sessionThreads(ctx, db, subjectID, id.DMChannelID)
	if err != nil {
		return "", err
	}
	thread, _, found := a.cards.waiting(id.DMChannelID, waits, func(thread, _ string) bool { return slices.Contains(threads, thread) })
	if !found {
		return "", fmt.Errorf("no waiting card %q in your DM to reply under", waits)
	}
	return thread, nil
}

func sessionThreads(ctx context.Context, db *sql.DB, subjectID, dmChannelID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT thread_ts FROM watches WHERE subject_id = ? AND kind = 'dm' AND channel_id = ? AND status = 'active'
		ORDER BY id DESC`, subjectID, dmChannelID)
	if err != nil {
		return nil, fmt.Errorf("find session threads: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var threads []string
	for rows.Next() {
		var thread string
		if err := rows.Scan(&thread); err != nil {
			return nil, fmt.Errorf("scan session thread: %w", err)
		}
		threads = append(threads, thread)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session threads: %w", err)
	}
	return threads, nil
}

func (a *app) openSessionThread(ctx context.Context, db *sql.DB, sub subject.Subject, dmChannelID, threadTS string) error {
	now := a.now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin session thread tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	open, err := countActive(ctx, tx, `SELECT COUNT(*) FROM watches WHERE subject_id = ? AND kind = 'dm' AND channel_id = ? AND thread_ts = ? AND status = 'active'`,
		sub.ID, dmChannelID, threadTS)
	if err != nil || open {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO cursors(channel_id, thread_ts, oldest_ts, next_poll_at) VALUES (?, '', ?, ?)
		ON CONFLICT(channel_id, thread_ts) DO UPDATE SET next_poll_at = MIN(next_poll_at, excluded.next_poll_at)`,
		dmChannelID, threadTS, now.UnixMilli()); err != nil {
		return fmt.Errorf("claim DM cursor: %w", err)
	}
	if err := claimCursor(ctx, tx, dmChannelID, threadTS, threadTS, now, true); err != nil {
		return err
	}
	if _, err := addWatch(ctx, tx, sub.ID, agent.TopLevel, kindDM, dmChannelID, threadTS, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE watches SET status = 'stopped' WHERE subject_id = ? AND kind = 'dm' AND status = 'active' AND id NOT IN
		(SELECT id FROM watches WHERE subject_id = ? AND kind = 'dm' AND status = 'active' ORDER BY id DESC LIMIT ?)`,
		sub.ID, sub.ID, sessionMessageWatches); err != nil {
		return fmt.Errorf("release oldest session thread: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session thread: %w", err)
	}
	return nil
}
