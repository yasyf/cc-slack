package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

type slackAPI interface {
	AuthTestContext(ctx context.Context) (*slack.AuthTestResponse, error)
	GetUserByEmailContext(ctx context.Context, email string) (*slack.User, error)
	GetUserInfoContext(ctx context.Context, user string) (*slack.User, error)
	OpenConversationContext(ctx context.Context, params *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error)
	JoinConversationContext(ctx context.Context, channelID string) (*slack.Channel, string, []string, error)
	PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error)
	AddReactionContext(ctx context.Context, name string, item slack.ItemRef) error
	RemoveReactionContext(ctx context.Context, name string, item slack.ItemRef) error
	UpdateMessageContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error)
	DeleteMessageContext(ctx context.Context, channelID, timestamp string) (string, string, error)
	GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error)
	GetConversationHistoryContext(ctx context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error)
	GetConversationsContext(ctx context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error)
	GetConversationInfoContext(ctx context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error)
	GetUsersInConversationContext(ctx context.Context, params *slack.GetUsersInConversationParameters) ([]string, string, error)
	GetUsersContext(ctx context.Context, options ...slack.GetUsersOption) ([]slack.User, error)
	GetUploadURLExternalContext(ctx context.Context, params slack.GetUploadURLExternalParameters) (*slack.GetUploadURLExternalResponse, error)
	UploadToURL(ctx context.Context, params slack.UploadToURLParameters) error
	CompleteUploadExternalContext(ctx context.Context, params slack.CompleteUploadExternalParameters) (*slack.CompleteUploadExternalResponse, error)
	GetFileInfoContext(ctx context.Context, fileID string, count, page int) (*slack.File, []slack.Comment, *slack.Paging, error)
	GetFileContext(ctx context.Context, downloadURL string, writer io.Writer) error
	CreateConversationContext(ctx context.Context, params slack.CreateConversationParams) (*slack.Channel, error)
	ArchiveConversationContext(ctx context.Context, channelID string) error
	InviteUsersToConversationContext(ctx context.Context, channelID string, users ...string) (*slack.Channel, error)
	SetTopicOfConversationContext(ctx context.Context, channelID, topic string) (*slack.Channel, error)
	ShareFilesContext(ctx context.Context, share fileShare) error
	DocCardsContext(ctx context.Context, serviceURL, channelID, text string) ([]slack.WorkObjectEntity, error)
}

type fileShare struct {
	files     []slack.FileSummary
	channelID string
	threadTS  string
	text      string
	username  string
	iconURL   string
}

type doer interface {
	Do(req *http.Request) (*http.Response, error)
}

type apiClient struct {
	*slack.Client
	token    string
	endpoint string
	http     doer
}

func newSlackClient(token, endpoint string, hc doer) slackAPI {
	return &apiClient{slack.New(token, slack.OptionAPIURL(endpoint), slack.OptionHTTPClient(hc)), token, endpoint, hc}
}

func newSlackAPI(token string) slackAPI { return newSlackClient(token, slack.APIURL, &http.Client{}) }

// ShareFilesContext calls files.completeUploadExternal itself because slack-go sends no username or icon_url,
// which the share message needs to read as the user's agent.
func (c *apiClient) ShareFilesContext(ctx context.Context, share fileShare) error {
	files, err := json.Marshal(share.files)
	if err != nil {
		return err
	}
	form := url.Values{"files": {string(files)}, "channel_id": {share.channelID}, "username": {share.username}, "icon_url": {share.iconURL}}
	if share.threadTS != "" {
		form.Set("thread_ts", share.threadTS)
	}
	if share.text != "" {
		form.Set("initial_comment", share.text)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"files.completeUploadExternal", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req) //nolint:gosec // G704: the request goes to the Slack API endpoint
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		retry, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return &slack.RateLimitedError{RetryAfter: time.Duration(retry) * time.Second}
	}
	if resp.StatusCode != http.StatusOK {
		return slack.StatusCodeError{Code: resp.StatusCode, Status: resp.Status}
	}
	var out slack.SlackResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("files.completeUploadExternal: %s: %w", resp.Status, err)
	}
	if !out.Ok {
		return slack.SlackErrorResponse{Err: out.Error, Errors: out.Errors, ResponseMetadata: out.ResponseMetadata}
	}
	return nil
}

const (
	pageLimit     = 200
	maxPages      = 5
	listPageLimit = 1000
)

func fetchReplies(ctx context.Context, api slackAPI, channelID, threadTS, oldest string) ([]slack.Message, error) {
	var out []slack.Message
	cursor := ""
	for range maxPages {
		msgs, more, next, err := api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
			ChannelID: channelID, Timestamp: threadTS, Oldest: oldest, Cursor: cursor, Limit: pageLimit, IncludeAllMetadata: true,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, msgs...)
		if !more || next == "" {
			return sortByTS(out), nil
		}
		cursor = next
	}
	return nil, fmt.Errorf("thread %s/%s runs past %d messages, so its latest message cannot be verified", channelID, threadTS, maxPages*pageLimit)
}

func fetchHistory(ctx context.Context, api slackAPI, channelID, oldest string, limit int) ([]slack.Message, error) {
	var out []slack.Message
	cursor := ""
	for range maxPages {
		resp, err := api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
			ChannelID: channelID, Oldest: oldest, Cursor: cursor, Limit: limit, IncludeAllMetadata: true,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.Messages...)
		if !resp.HasMore || resp.ResponseMetaData.NextCursor == "" || limit < pageLimit {
			break
		}
		cursor = resp.ResponseMetaData.NextCursor
	}
	return sortByTS(out), nil
}

func fetchRecentTopLevel(ctx context.Context, api slackAPI, channelID string, limit int) ([]slack.Message, error) {
	var out []slack.Message
	cursor := ""
	for range maxPages {
		resp, err := api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
			ChannelID: channelID, Cursor: cursor, Limit: min(limit, pageLimit),
		})
		if err != nil {
			return nil, err
		}
		for _, m := range resp.Messages {
			if m.ThreadTimestamp == "" || m.ThreadTimestamp == m.Timestamp {
				out = append(out, m)
			}
		}
		if len(out) >= limit || !resp.HasMore || resp.ResponseMetaData.NextCursor == "" {
			break
		}
		cursor = resp.ResponseMetaData.NextCursor
	}
	out = sortByTS(out)
	slices.Reverse(out)
	return out[:min(limit, len(out))], nil
}

func fetchChannels(ctx context.Context, api slackAPI) ([]slack.Channel, error) {
	var out []slack.Channel
	cursor := ""
	for {
		channels, next, err := api.GetConversationsContext(ctx, &slack.GetConversationsParameters{
			Cursor: cursor, ExcludeArchived: true, Limit: listPageLimit, Types: []string{"public_channel", "private_channel"},
		})
		if err != nil {
			return nil, err
		}
		out = append(out, channels...)
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

func sortByTS(msgs []slack.Message) []slack.Message {
	sort.SliceStable(msgs, func(i, j int) bool { return tsLess(msgs[i].Timestamp, msgs[j].Timestamp) })
	return msgs
}

func tsLess(a, b string) bool {
	as, au := splitTS(a)
	bs, bu := splitTS(b)
	if as != bs {
		return as < bs
	}
	return au < bu
}

func splitTS(ts string) (int64, int64) {
	sec, micro, _ := strings.Cut(ts, ".")
	s, _ := strconv.ParseInt(sec, 10, 64)
	u, _ := strconv.ParseInt((micro + "000000")[:6], 10, 64)
	return s, u
}

func formatTS(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

func errorCode(err error) string {
	var rate *slack.RateLimitedError
	if errors.As(err, &rate) {
		return "ratelimited"
	}
	var resp slack.SlackErrorResponse
	if errors.As(err, &resp) {
		return resp.Err
	}
	if errors.Is(err, errNoIdentity) {
		return "not_logged_in"
	}
	return err.Error()
}

var fatalWatchCodes = map[string]bool{
	"not_in_channel":    true,
	"channel_not_found": true,
	"missing_scope":     true,
	"thread_not_found":  true,
	"is_archived":       true,
	"invalid_auth":      true,
	"account_inactive":  true,
	"token_revoked":     true,
}
