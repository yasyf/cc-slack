package main

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

const (
	profileTTL           = 24 * time.Hour
	profileLookupTimeout = 5 * time.Second
	postEventType        = "cc_slack_post"
	defaultAgent         = "agent"
)

var (
	errNoIdentity = errors.New("cc-slack has no Slack identity on this machine")
	agentName     = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,23}$`)
)

type identity struct {
	UserID       string `json:"user_id"`
	Email        string `json:"email"`
	RealName     string `json:"real_name"`
	FirstName    string `json:"first_name"`
	DisplayName  string `json:"display_name"`
	Handle       string `json:"handle"`
	Image512     string `json:"image_512"`
	AvatarURL    string `json:"avatar_url"`
	AvatarKeyID  string `json:"-"`
	AvatarSecret string `json:"-"`
	TeamID       string `json:"team_id"`
	TeamURL      string `json:"team_url"`
	BotUserID    string `json:"bot_user_id"`
	BotID        string `json:"bot_id"`
	Token        string `json:"-"`
	DMChannelID  string `json:"dm_channel_id"`
	SocketURL    string `json:"socket_url"`
	RepoRoot     string `json:"repo_root"`
	OrcaPath     string `json:"orca_path"`
	CaptHookPath string `json:"capt_hook_path"`
	FetchedAt    int64  `json:"fetched_at"`
	Config       config `json:"config"`
	Agent        string `json:"-"`
}

func (id identity) name() string {
	if id.DisplayName == "" {
		return id.Handle
	}
	return id.DisplayName
}

func (id identity) username() string {
	return fmt.Sprintf("%s (%s)", id.name(), cmp.Or(id.Agent, defaultAgent))
}

func checkAgent(name string) error {
	if name != "" && !agentName.MatchString(name) {
		return fmt.Errorf("agent %q is not a lowercase agent name such as claude, codex, or cursor", name)
	}
	return nil
}

func (id identity) postOptions(text, threadTS string, extra ...slack.MsgOption) []slack.MsgOption {
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionUsername(id.username()),
		slack.MsgOptionIconURL(id.AvatarURL),
		slack.MsgOptionDisableLinkUnfurl(),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	return append(opts, extra...)
}

func (id identity) permalink(channelID, ts, threadTS string) string {
	link := fmt.Sprintf("%sarchives/%s/p%s", id.TeamURL, channelID, strings.ReplaceAll(ts, ".", ""))
	if threadTS != "" && threadTS != ts {
		link += fmt.Sprintf("?thread_ts=%s&cid=%s", threadTS, channelID)
	}
	return link
}

func (id identity) channelLink(channelID string) string {
	return fmt.Sprintf("%sarchives/%s", id.TeamURL, channelID)
}

func firstName(u *slack.User) string {
	if u.Profile.FirstName != "" {
		return u.Profile.FirstName
	}
	name, _, _ := strings.Cut(strings.TrimSpace(u.RealName), " ")
	return name
}

func resolveIdentity(ctx context.Context, api slackAPI, token, email string, now time.Time) (identity, error) {
	auth, err := api.AuthTestContext(ctx)
	if err != nil {
		return identity{}, fmt.Errorf("auth.test: %w", err)
	}
	if auth.BotID == "" {
		return identity{}, errors.New("the token is not a bot token (auth.test returned no bot_id)")
	}
	user, err := api.GetUserByEmailContext(ctx, email)
	if err != nil {
		return identity{}, fmt.Errorf("users.lookupByEmail %s: %w", email, err)
	}
	dm, _, _, err := api.OpenConversationContext(ctx, &slack.OpenConversationParameters{Users: []string{user.ID}})
	if err != nil {
		return identity{}, fmt.Errorf("conversations.open: %w", err)
	}
	return identity{
		UserID:      user.ID,
		Email:       email,
		RealName:    user.RealName,
		FirstName:   firstName(user),
		DisplayName: user.Profile.DisplayName,
		Handle:      user.Name,
		Image512:    user.Profile.Image512,
		TeamID:      auth.TeamID,
		TeamURL:     auth.URL,
		BotUserID:   auth.UserID,
		BotID:       auth.BotID,
		Token:       token,
		DMChannelID: dm.ID,
		FetchedAt:   now.Unix(),
	}, nil
}

func refreshProfile(ctx context.Context, api slackAPI, db *sql.DB, publish avatarPublisher, id identity, now time.Time) (identity, error) {
	if now.Sub(time.Unix(id.FetchedAt, 0)) < profileTTL {
		return id, nil
	}
	id.FetchedAt = now.Unix()
	lookupCtx, cancel := context.WithTimeout(ctx, profileLookupTimeout)
	user, lookupErr := api.GetUserInfoContext(lookupCtx, id.UserID)
	cancel()
	var avatarErr error
	if lookupErr == nil {
		id.RealName, id.FirstName, id.DisplayName, id.Handle = user.RealName, firstName(user), user.Profile.DisplayName, user.Name
		if user.Profile.Image512 != id.Image512 || !strings.Contains(id.AvatarURL, avatarStyle) {
			next := id
			next.Image512 = user.Profile.Image512
			if next.AvatarURL, avatarErr = publish(ctx, next); avatarErr == nil {
				id = next
			}
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE identity SET real_name = ?, first_name = ?, display_name = ?, handle = ?, image_512 = ?, avatar_url = ?, fetched_at = ? WHERE id = 1`,
		id.RealName, id.FirstName, id.DisplayName, id.Handle, id.Image512, id.AvatarURL, id.FetchedAt); err != nil {
		return id, fmt.Errorf("save profile read: %w", err)
	}
	if lookupErr != nil {
		return id, fmt.Errorf("users.info %s, keeping the saved profile for another %s: %w", id.UserID, profileTTL, lookupErr)
	}
	if avatarErr != nil {
		return id, fmt.Errorf("rebuild the agent avatar, keeping the old one for another %s: %w", profileTTL, avatarErr)
	}
	return id, nil
}

func saveIdentity(ctx context.Context, db *sql.DB, id identity) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin identity tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	cfg, err := json.Marshal(id.Config)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO identity
		(id, user_id, email, real_name, first_name, display_name, handle, image_512, avatar_url, avatar_key_id, avatar_secret, team_id, team_url,
		 bot_user_id, bot_id, token, dm_channel_id, socket_url, repo_root, orca_path, capt_hook_path, fetched_at, config)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.UserID, id.Email, id.RealName, id.FirstName, id.DisplayName, id.Handle, id.Image512, id.AvatarURL, id.AvatarKeyID, id.AvatarSecret,
		id.TeamID, id.TeamURL, id.BotUserID, id.BotID, id.Token, id.DMChannelID, id.SocketURL, id.RepoRoot, id.OrcaPath, id.CaptHookPath,
		id.FetchedAt, string(cfg)); err != nil {
		return fmt.Errorf("save identity: %w", err)
	}
	fetched := time.Unix(id.FetchedAt, 0)
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO cursors(channel_id, thread_ts, oldest_ts, next_poll_at) VALUES (?, '', ?, ?)`,
		id.DMChannelID, formatTS(fetched), fetched.UnixMilli()); err != nil {
		return fmt.Errorf("claim DM cursor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit identity: %w", err)
	}
	return nil
}

func loadIdentity(ctx context.Context, db *sql.DB) (identity, error) {
	var (
		id  identity
		cfg string
	)
	err := db.QueryRowContext(ctx, `SELECT user_id, email, real_name, first_name, display_name, handle, image_512, avatar_url, avatar_key_id,
		avatar_secret, team_id, team_url, bot_user_id, bot_id, token, dm_channel_id, socket_url, repo_root, orca_path, capt_hook_path, fetched_at,
		config FROM identity WHERE id = 1`).Scan(
		&id.UserID, &id.Email, &id.RealName, &id.FirstName, &id.DisplayName, &id.Handle, &id.Image512, &id.AvatarURL, &id.AvatarKeyID,
		&id.AvatarSecret, &id.TeamID, &id.TeamURL, &id.BotUserID, &id.BotID, &id.Token, &id.DMChannelID, &id.SocketURL, &id.RepoRoot,
		&id.OrcaPath, &id.CaptHookPath, &id.FetchedAt, &cfg)
	if errors.Is(err, sql.ErrNoRows) {
		return identity{}, fmt.Errorf("%w: cc-slack %s read no identity from %s", errNoIdentity, appVersion, statePath())
	}
	if err != nil {
		return identity{}, fmt.Errorf("load identity: %w", err)
	}
	if err := json.Unmarshal([]byte(cfg), &id.Config); err != nil {
		return identity{}, fmt.Errorf("decode the identity's config: %w", err)
	}
	return id, nil
}

func (id identity) authorMetadata() slack.MsgOption {
	return slack.MsgOptionMetadata(slack.SlackMetadata{EventType: postEventType, EventPayload: map[string]any{"user_id": id.UserID}})
}

func (id identity) authored(m slack.Message) bool {
	return id.fromClaude(m) && m.Metadata.EventType == postEventType && m.Metadata.EventPayload["user_id"] == id.UserID
}
