package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	GetFileInfoContext(ctx context.Context, fileID string, count, page int) (*slack.File, []slack.Comment, *slack.Paging, error)
	GetFileContext(ctx context.Context, downloadURL string, writer io.Writer) error
	CreateConversationContext(ctx context.Context, params slack.CreateConversationParams) (*slack.Channel, error)
	ArchiveConversationContext(ctx context.Context, channelID string) error
	InviteUsersToConversationContext(ctx context.Context, channelID string, users ...string) (*slack.Channel, error)
	SetTopicOfConversationContext(ctx context.Context, channelID, topic string) (*slack.Channel, error)
	TabCanvasContext(ctx context.Context, channelID, title string, page slack.DocumentContent) (string, error)
	EditCanvasContext(ctx context.Context, params slack.EditCanvasParams) error
	DeleteCanvasContext(ctx context.Context, canvasID string) error
	ShareFilesContext(ctx context.Context, share fileShare) error
	DocCardsContext(ctx context.Context, serviceURL, channelID, text string) ([]slack.WorkObjectEntity, error)
	RawMessages(ctx context.Context, method, channelID, threadTS, oldest string) (map[string]json.RawMessage, error)
	RawMessage(ctx context.Context, channelID, threadTS, ts string) (json.RawMessage, error)
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

type rateWaitKey struct{}

func withoutRateWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, rateWaitKey{}, true)
}

func newSlackClient(token, endpoint string, hc doer) slackAPI {
	hc = retrying{timedDoer{hc}}
	return &apiClient{slack.New(token, slack.OptionAPIURL(endpoint), slack.OptionHTTPClient(requestPoster{token, endpoint, workObjectPoster{token, hc}})), token, endpoint, hc}
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
		limited := err == nil && resp.StatusCode == http.StatusTooManyRequests
		if !again || !replayable || try == slackTries || req.Context().Err() != nil || limited && req.Context().Value(rateWaitKey{}) != nil {
			return withTries(resp, err, try)
		}
		cause := fmt.Sprint(err)
		if err == nil {
			cause = resp.Status
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		log.Printf("slack %s: %s; try %d of %d in %s", path.Base(req.URL.Path), cause, try+1, slackTries, wait) //nolint:gosec // G706: the line carries Slack's own status, for the daemon log only
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

// workObjectPoster turns slack-go's finished chat form into the JSON a message carrying Work Objects posts as, the shape
// ai-oncall's cards draw from: event_type and event_payload beside entities, which slack-go's form-encoded metadata cannot carry.
// It reads the form on the wire because slack-go holds a message's blocks and attachments privately until it sends.
type workObjectPoster struct {
	token string
	next  doer
}

func (p workObjectPoster) Do(req *http.Request) (*http.Response, error) {
	if method := path.Base(req.URL.Path); method != "chat.postMessage" && method != "chat.update" {
		return p.next.Do(req) //nolint:gosec // G704: the caller built the request for Slack or the doc-cards URL the config names
	}
	form, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	body, carded, err := workObjectBody(form)
	if err != nil {
		return nil, err
	}
	if !carded {
		req.Body = io.NopCloser(bytes.NewReader(form))
		return p.next.Do(req) //nolint:gosec // G704: the caller built the request for Slack or the doc-cards URL the config names
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	posted, err := http.NewRequestWithContext(req.Context(), http.MethodPost, req.URL.String(), bytes.NewReader(encoded)) //nolint:gosec // G704: the request goes to the Slack API endpoint
	if err != nil {
		return nil, err
	}
	posted.Header.Set("Content-Type", "application/json; charset=utf-8")
	posted.Header.Set("Authorization", "Bearer "+p.token)
	answer, err := p.next.Do(posted) //nolint:gosec // G704: the request goes to the Slack API endpoint
	if err != nil {
		return nil, err
	}
	said, err := io.ReadAll(answer.Body)
	if err != nil {
		return nil, err
	}
	answer.Body = io.NopCloser(bytes.NewReader(said))
	echo, refusal := workObjectEcho(said)
	notePostWarning(req.Context(), refusal)
	log.Printf("work object %s %s: %s", path.Base(req.URL.Path), body["channel"], echo) //nolint:gosec // G706: the line carries Slack's own answer, for the daemon log only
	return answer, nil
}

type postWarningsKey struct{}

type postWarnings struct {
	mu   sync.Mutex
	said []string
}

func withPostWarnings(ctx context.Context) context.Context {
	return context.WithValue(ctx, postWarningsKey{}, &postWarnings{})
}

func notePostWarning(ctx context.Context, refusal string) {
	refusals, ok := ctx.Value(postWarningsKey{}).(*postWarnings)
	if !ok || refusal == "" {
		return
	}
	refusals.mu.Lock()
	defer refusals.mu.Unlock()
	refusals.said = append(refusals.said, refusal)
}

func postWarningsIn(ctx context.Context) string {
	refusals, ok := ctx.Value(postWarningsKey{}).(*postWarnings)
	if !ok {
		return ""
	}
	refusals.mu.Lock()
	defer refusals.mu.Unlock()
	return strings.Join(refusals.said, "; ")
}

func workObjectEcho(said []byte) (string, string) {
	var echo struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Warning  string `json:"warning"`
		Metadata struct {
			Warnings []string `json:"warnings"`
			Messages []string `json:"messages"`
		} `json:"response_metadata"`
		Message struct {
			Attachments []struct {
				FromURL string `json:"from_url"`
			} `json:"attachments"`
			Metadata json.RawMessage `json:"metadata"`
		} `json:"message"`
	}
	if err := json.Unmarshal(said, &echo); err != nil {
		return "unreadable answer: " + err.Error(), ""
	}
	refusal := ""
	if strings.Contains(echo.Warning, "metadata") {
		refusal = "Slack dropped this post's card: " + cmp.Or(strings.Join(echo.Metadata.Messages, " "), echo.Warning)
	}
	return fmt.Sprintf("ok=%t error=%q warning=%q warnings=%v messages=%v attachments=%v metadata=%s",
		echo.OK, echo.Error, echo.Warning, echo.Metadata.Warnings, echo.Metadata.Messages, echo.Message.Attachments, echo.Message.Metadata), refusal
}

func workObjectBody(form []byte) (map[string]any, bool, error) {
	values, err := url.ParseQuery(string(form))
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
	body := map[string]any{"metadata": json.RawMessage(raw)}
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

// RawMessages reads every message from oldest on as Slack sends them, by timestamp, since slack-go's Message drops what it has no field for.
func (c *apiClient) RawMessages(ctx context.Context, method, channelID, threadTS, oldest string) (map[string]json.RawMessage, error) {
	query := url.Values{"channel": {channelID}, "oldest": {oldest}, "inclusive": {"true"}, "include_all_metadata": {"true"}, "limit": {"200"}}
	if threadTS != "" {
		query.Set("ts", threadTS)
	}
	return c.rawMessages(ctx, method, query)
}

// RawMessage reads the one message ts in the thread threadTS as Slack sends it, or errMessageGone when the thread no longer holds it.
func (c *apiClient) RawMessage(ctx context.Context, channelID, threadTS, ts string) (json.RawMessage, error) {
	found, err := c.rawMessages(ctx, methodReplies, url.Values{
		"channel": {channelID}, "ts": {threadTS}, "oldest": {ts}, "latest": {ts}, "inclusive": {"true"},
		"include_all_metadata": {"true"}, "limit": {strconv.Itoa(pageLimit)},
	})
	if err != nil {
		return nil, err
	}
	raw, ok := found[ts]
	if !ok {
		return nil, errMessageGone
	}
	return raw, nil
}

func (c *apiClient) rawMessages(ctx context.Context, method string, query url.Values) (map[string]json.RawMessage, error) {
	found := map[string]json.RawMessage{}
	for {
		messages, cursor, err := slackMessages(ctx, c.http, c.token, c.endpoint+method, query)
		if err != nil {
			return nil, err
		}
		for _, raw := range messages {
			var message struct {
				TS string `json:"ts"`
			}
			if err := json.Unmarshal(raw, &message); err != nil {
				return nil, err
			}
			found[message.TS] = raw
		}
		if cursor == "" {
			return found, nil
		}
		query.Set("cursor", cursor)
	}
}

func slackMessages(ctx context.Context, hc doer, token, method string, query url.Values) ([]json.RawMessage, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, method+"?"+query.Encode(), http.NoBody)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req) //nolint:gosec // G704: the request goes to the Slack API endpoint
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		retry, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return nil, "", &slack.RateLimitedError{RetryAfter: time.Duration(retry) * time.Second}
	}
	var answer struct {
		slack.SlackResponse
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return nil, "", fmt.Errorf("%s: %s: %w", path.Base(method), resp.Status, err)
	}
	if !answer.Ok {
		return nil, "", slack.SlackErrorResponse{Err: answer.Error, Errors: answer.Errors, ResponseMetadata: answer.ResponseMetadata}
	}
	return answer.Messages, answer.ResponseMetadata.Cursor, nil
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
			ChannelID: channelID, Cursor: cursor, Limit: min(limit, pageLimit), IncludeAllMetadata: true,
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
