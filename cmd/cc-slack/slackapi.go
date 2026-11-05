package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
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
	SetAssistantThreadsStatusContext(ctx context.Context, params slack.AssistantThreadsSetStatusParameters) error
	StartStreamContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error)
	AppendStreamContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, error)
	StopStreamContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, error)
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
	RawMessages(ctx context.Context, method, channelID, threadTS string) (map[string]json.RawMessage, error)
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

const (
	slackTries         = 4
	slackFirstBackoff  = 500 * time.Millisecond
	slackMaxBackoff    = 8 * time.Second
	slackMaxRetryAfter = 30 * time.Second
)

var nonIdempotent = map[string]bool{
	"chat.postMessage":             true,
	"chat.postEphemeral":           true,
	"chat.scheduleMessage":         true,
	"chat.meMessage":               true,
	"files.completeUploadExternal": true,
	"conversations.create":         true,
}

type retrying struct{ next doer }

func newSlackClient(token, endpoint string, hc doer) slackAPI {
	hc = retrying{hc}
	return &apiClient{slack.New(token, slack.OptionAPIURL(endpoint), slack.OptionHTTPClient(hc)), token, endpoint, hc}
}

func newSlackAPI(token string) slackAPI { return newSlackClient(token, slack.APIURL, &http.Client{}) }

func (r retrying) Do(req *http.Request) (*http.Response, error) {
	replayable := req.Body == nil || req.GetBody != nil
	idempotent := !nonIdempotent[path.Base(req.URL.Path)]
	backoff := slackFirstBackoff
	for try := 1; ; try++ {
		resp, err := r.next.Do(req) //nolint:gosec // G704: the caller built the request for Slack or the doc-cards URL the config names
		if err == nil && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < http.StatusInternalServerError {
			return resp, nil
		}
		wait, again := retryWait(resp, err, idempotent, backoff)
		if !again || !replayable || try == slackTries || req.Context().Err() != nil {
			return withTries(resp, err, try)
		}
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		if err := pause(req.Context(), wait); err != nil {
			return nil, err
		}
		if req.GetBody != nil {
			if req.Body, err = req.GetBody(); err != nil {
				return nil, err
			}
		}
		backoff = min(2*backoff, slackMaxBackoff)
	}
}

func retryWait(resp *http.Response, err error, idempotent bool, backoff time.Duration) (time.Duration, bool) {
	if err != nil || resp.StatusCode != http.StatusTooManyRequests {
		return backoff, idempotent
	}
	after := resp.Header.Get("Retry-After")
	if after == "" {
		return backoff, true
	}
	seconds, err := strconv.Atoi(after)
	wait := time.Duration(seconds) * time.Second
	return wait, err == nil && wait <= slackMaxRetryAfter
}

func withTries(resp *http.Response, err error, tries int) (*http.Response, error) {
	if tries == 1 {
		return resp, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w after %d tries", err, tries)
	}
	// slack-go, ShareFilesContext and DocCardsContext quote Status in the error they build from a refused response.
	resp.Status += fmt.Sprintf(" after %d tries", tries)
	return resp, nil
}

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

type chatAnswer struct {
	slack.SlackResponse
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	Text    string `json:"text"`
}

func (c *apiClient) PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	body, carded, err := workObjectBody(c.token, channelID, c.endpoint, options...)
	if err != nil {
		return "", "", err
	}
	if !carded {
		return c.Client.PostMessageContext(ctx, channelID, options...)
	}
	answer, err := c.chatJSON(ctx, "chat.postMessage", body)
	return answer.Channel, answer.TS, err
}

func (c *apiClient) UpdateMessageContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error) {
	body, carded, err := workObjectBody(c.token, channelID, c.endpoint, append(options, slack.MsgOptionUpdate(timestamp))...)
	if err != nil {
		return "", "", "", err
	}
	if !carded {
		return c.Client.UpdateMessageContext(ctx, channelID, timestamp, options...)
	}
	answer, err := c.chatJSON(ctx, "chat.update", body)
	return answer.Channel, answer.TS, answer.Text, err
}

// workObjectBody is the JSON a message carrying Work Objects posts as, the shape ai-oncall's cards draw from:
// event_type and event_payload beside entities, which slack-go's form-encoded metadata cannot carry.
func workObjectBody(token, channelID, endpoint string, options ...slack.MsgOption) (map[string]any, bool, error) {
	_, values, err := slack.UnsafeApplyMsgOptions(token, channelID, endpoint, options...)
	if err != nil {
		return nil, false, err
	}
	var metadata struct {
		Entities []json.RawMessage `json:"entities"`
	}
	raw := values.Get("metadata")
	if raw == "" {
		return nil, false, nil
	}
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return nil, false, err
	}
	if len(metadata.Entities) == 0 {
		return nil, false, nil
	}
	body := map[string]any{"metadata": map[string]any{
		"event_type": postEventType, "event_payload": map[string]any{"source": "cc-slack"}, "entities": metadata.Entities,
	}}
	for key, value := range values {
		switch {
		case key == "token" || key == "metadata":
		case key == "blocks" || key == "attachments":
			body[key] = json.RawMessage(value[0])
		case value[0] == "true" || value[0] == "false":
			body[key] = value[0] == "true"
		default:
			body[key] = value[0]
		}
	}
	return body, true, nil
}

func (c *apiClient) chatJSON(ctx context.Context, method string, body map[string]any) (chatAnswer, error) {
	var answer chatAnswer
	encoded, err := json.Marshal(body)
	if err != nil {
		return answer, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+method, bytes.NewReader(encoded))
	if err != nil {
		return answer, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req) //nolint:gosec // G704: the request goes to the Slack API endpoint
	if err != nil {
		return answer, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		retry, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return answer, &slack.RateLimitedError{RetryAfter: time.Duration(retry) * time.Second}
	}
	if resp.StatusCode != http.StatusOK {
		return answer, slack.StatusCodeError{Code: resp.StatusCode, Status: resp.Status}
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return answer, fmt.Errorf("%s: %s: %w", method, resp.Status, err)
	}
	if !answer.Ok {
		return answer, slack.SlackErrorResponse{Err: answer.Error, Errors: answer.Errors, ResponseMetadata: answer.ResponseMetadata}
	}
	return answer, nil
}

// RawMessages reads messages as Slack sends them, by timestamp, since slack-go's Message drops what it has no field for.
func (c *apiClient) RawMessages(ctx context.Context, method, channelID, threadTS string) (map[string]json.RawMessage, error) {
	query := url.Values{"channel": {channelID}, "include_all_metadata": {"true"}, "limit": {"200"}}
	if threadTS != "" {
		query.Set("ts", threadTS)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+method+"?"+query.Encode(), http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req) //nolint:gosec // G704: the request goes to the Slack API endpoint
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var answer struct {
		slack.SlackResponse
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return nil, fmt.Errorf("%s: %s: %w", method, resp.Status, err)
	}
	if !answer.Ok {
		return nil, slack.SlackErrorResponse{Err: answer.Error, Errors: answer.Errors, ResponseMetadata: answer.ResponseMetadata}
	}
	found := make(map[string]json.RawMessage, len(answer.Messages))
	for _, raw := range answer.Messages {
		var message struct {
			TS string `json:"ts"`
		}
		if err := json.Unmarshal(raw, &message); err != nil {
			return nil, err
		}
		found[message.TS] = raw
	}
	return found, nil
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

func fetchRecentTopLevel(ctx context.Context, api slackAPI, channelID string, limit int) ([]slack.Message, bool, error) {
	var out []slack.Message
	cursor := ""
	partial := false
	for range maxPages {
		resp, err := api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
			ChannelID: channelID, Cursor: cursor, Limit: min(limit, pageLimit),
		})
		if err != nil {
			var rate *slack.RateLimitedError
			if len(out) == 0 || (!errors.Is(err, context.DeadlineExceeded) && !errors.As(err, &rate)) {
				return nil, false, err
			}
			partial = true
			break
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
	return out[:min(limit, len(out))], partial, nil
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
