package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/store"
	"github.com/yasyf/cc-interact/subject"
)

type fakePost struct {
	channelID   string
	threadTS    string
	text        string
	username    string
	iconURL     string
	ts          string
	broadcast   bool
	unfurlLinks string
	blocks      string
	metadata    string
}

type fakeEdit struct {
	channelID string
	ts        string
	text      string
	blocks    string
}

type fakeSlack struct {
	mu        sync.Mutex
	uploads   map[string][]byte
	downloads map[string][]byte
	complete  [][]slack.FileSummary
	shared    map[string]slack.Share
	files     map[string]slack.File
	pending   int
	auth      slack.AuthTestResponse
	users     map[string]*slack.User
	channels  map[string][]slack.Message
	listed    []slack.Channel
	dms       map[string][]string
	errs      map[string][]error
	calls     map[string]int
	oldest    []string
	posts     []fakePost
	edits     []fakeEdit
	deletes   []fakeEdit
	joined    []string
	created   []string
	archived  []string
	invites   map[string][]string
	topics    map[string]string
	cards     []slack.WorkObjectEntity
	asked     []string
	clock     int64
}

func newFakeSlack() *fakeSlack {
	return &fakeSlack{
		auth:      slack.AuthTestResponse{URL: "https://acme.slack.com/", TeamID: "T1", UserID: "UBOT", BotID: "BBOT"},
		users:     map[string]*slack.User{},
		channels:  map[string][]slack.Message{},
		dms:       map[string][]string{},
		errs:      map[string][]error{},
		calls:     map[string]int{},
		uploads:   map[string][]byte{},
		downloads: map[string][]byte{},
		shared:    map[string]slack.Share{},
		files:     map[string]slack.File{},
		invites:   map[string][]string{},
		topics:    map[string]string{},
		clock:     1_800_000_000,
	}
}

func (f *fakeSlack) call(method string) error {
	f.calls[method]++
	queue := f.errs[method]
	if len(queue) == 0 {
		return nil
	}
	f.errs[method] = queue[1:]
	return queue[0]
}

func (f *fakeSlack) DocCardsContext(_ context.Context, serviceURL, channelID, text string) ([]slack.WorkObjectEntity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, serviceURL+" "+channelID+" "+text)
	if err := f.call("doc-cards"); err != nil {
		return nil, err
	}
	return f.cards, nil
}

func historyOf(channelID string) string { return methodHistory + " " + channelID }

func (f *fakeSlack) fail(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[method] = append(f.errs[method], err)
}

func (f *fakeSlack) repliesOldest() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.oldest)
}

func (f *fakeSlack) setText(channelID, ts, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, m := range f.channels[channelID] {
		if m.Timestamp == ts {
			f.channels[channelID][i].Text = text
		}
	}
}

func (f *fakeSlack) remove(channelID, ts string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels[channelID] = slices.DeleteFunc(f.channels[channelID], func(m slack.Message) bool { return m.Timestamp == ts })
}

func (f *fakeSlack) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *fakeSlack) add(channelID string, m slack.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels[channelID] = append(f.channels[channelID], m)
}

func (f *fakeSlack) AuthTestContext(context.Context) (*slack.AuthTestResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("auth.test"); err != nil {
		return nil, err
	}
	auth := f.auth
	return &auth, nil
}

func (f *fakeSlack) GetUserByEmailContext(_ context.Context, email string) (*slack.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("users.lookupByEmail"); err != nil {
		return nil, err
	}
	for _, u := range f.users {
		if u.Profile.Email == email {
			return u, nil
		}
	}
	return nil, slack.SlackErrorResponse{Err: "users_not_found"}
}

func (f *fakeSlack) GetUsersContext(_ context.Context, _ ...slack.GetUsersOption) ([]slack.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("users.list"); err != nil {
		return nil, err
	}
	var out []slack.User
	for _, u := range f.users {
		out = append(out, *u)
	}
	return out, nil
}

func (f *fakeSlack) GetUserInfoContext(_ context.Context, user string) (*slack.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("users.info"); err != nil {
		return nil, err
	}
	if u, ok := f.users[user]; ok {
		return u, nil
	}
	return nil, slack.SlackErrorResponse{Err: "user_not_found"}
}

func (f *fakeSlack) OpenConversationContext(_ context.Context, params *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("conversations.open"); err != nil {
		return nil, false, false, err
	}
	ch := &slack.Channel{}
	ch.ID = "D" + params.Users[0]
	if len(params.Users) > 1 {
		ch.ID = "G" + strings.Join(params.Users, "")
	}
	f.dms[ch.ID] = slices.Clone(params.Users)
	return ch, false, false, nil
}

func (f *fakeSlack) GetConversationInfoContext(_ context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("conversations.info"); err != nil {
		return nil, err
	}
	ch := &slack.Channel{}
	ch.ID = input.ChannelID
	if users, found := f.dms[input.ChannelID]; found {
		ch.IsIM, ch.IsMpIM = len(users) == 1, len(users) > 1
	}
	return ch, nil
}

func (f *fakeSlack) GetUsersInConversationContext(_ context.Context, params *slack.GetUsersInConversationParameters) ([]string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("conversations.members"); err != nil {
		return nil, "", err
	}
	return append([]string{f.auth.UserID}, f.dms[params.ChannelID]...), "", nil
}

func (f *fakeSlack) JoinConversationContext(_ context.Context, channelID string) (*slack.Channel, string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("conversations.join"); err != nil {
		return nil, "", nil, err
	}
	f.joined = append(f.joined, channelID)
	return &slack.Channel{}, "", nil, nil
}

func (f *fakeSlack) PostMessageContext(_ context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("chat.postMessage"); err != nil {
		return "", "", err
	}
	values, err := sentForm(channelID, options...)
	if err != nil {
		return "", "", err
	}
	f.clock++
	ts := fmt.Sprintf("%d.000000", f.clock)
	p := fakePost{
		channelID: channelID, threadTS: values.Get("thread_ts"), text: values.Get("text"),
		username: values.Get("username"), iconURL: values.Get("icon_url"), ts: ts,
		broadcast: values.Get("reply_broadcast") == "true", unfurlLinks: values.Get("unfurl_links"), blocks: values.Get("blocks"),
		metadata: values.Get("metadata"),
	}
	f.posts = append(f.posts, p)
	var blocks slack.Blocks
	if p.blocks != "" {
		if err := json.Unmarshal([]byte(p.blocks), &blocks); err != nil {
			return "", "", err
		}
	}
	var metadata slack.SlackMetadata
	if p.metadata != "" {
		if err := json.Unmarshal([]byte(p.metadata), &metadata); err != nil {
			return "", "", err
		}
	}
	f.channels[channelID] = append(f.channels[channelID], slack.Message{Msg: slack.Msg{
		Timestamp: ts, ThreadTimestamp: p.threadTS, User: f.auth.UserID, BotID: f.auth.BotID, Username: p.username, Text: p.text,
		Blocks: blocks, Metadata: metadata,
	}})
	return channelID, ts, nil
}

type formCapture struct{ values url.Values }

func (c *formCapture) Do(r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if c.values, err = url.ParseQuery(string(body)); err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: r,
	}, nil
}

func sentForm(channelID string, options ...slack.MsgOption) (url.Values, error) {
	capture := &formCapture{}
	if _, _, err := slack.New("xoxb-fake", slack.OptionHTTPClient(capture)).PostMessageContext(context.Background(), channelID, options...); err != nil {
		return nil, err
	}
	return capture.values, nil
}

func (f *fakeSlack) AddReactionContext(_ context.Context, _ string, _ slack.ItemRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.call("reactions.add")
}

func (f *fakeSlack) UpdateMessageContext(_ context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("chat.update"); err != nil {
		return "", "", "", err
	}
	values, err := sentForm(channelID, options...)
	if err != nil {
		return "", "", "", err
	}
	f.edits = append(f.edits, fakeEdit{channelID: channelID, ts: timestamp, text: values.Get("text"), blocks: values.Get("blocks")})
	return channelID, timestamp, values.Get("text"), nil
}

func (f *fakeSlack) edited() []fakeEdit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeEdit(nil), f.edits...)
}

func (f *fakeSlack) DeleteMessageContext(_ context.Context, channelID, timestamp string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("chat.delete"); err != nil {
		return "", "", err
	}
	f.channels[channelID] = slices.DeleteFunc(f.channels[channelID], func(m slack.Message) bool { return m.Timestamp == timestamp })
	f.deletes = append(f.deletes, fakeEdit{channelID: channelID, ts: timestamp})
	return channelID, timestamp, nil
}

func (f *fakeSlack) deleted() []fakeEdit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeEdit(nil), f.deletes...)
}

func (f *fakeSlack) RemoveReactionContext(_ context.Context, _ string, _ slack.ItemRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.call("reactions.remove")
}

func (f *fakeSlack) GetConversationRepliesContext(_ context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(methodReplies); err != nil {
		return nil, false, "", err
	}
	f.oldest = append(f.oldest, params.Oldest)
	var out []slack.Message
	for _, m := range f.channels[params.ChannelID] {
		if !params.IncludeAllMetadata {
			m.Metadata = slack.SlackMetadata{}
		}
		if m.Timestamp == params.Timestamp {
			out = append([]slack.Message{m}, out...)
			continue
		}
		if m.ThreadTimestamp == params.Timestamp && (params.Oldest == "" || tsLess(params.Oldest, m.Timestamp) ||
			params.Inclusive && params.Oldest == m.Timestamp) {
			out = append(out, m)
		}
	}
	if params.Limit == 0 || len(out) <= params.Limit {
		return out, false, "", nil
	}
	start, _ := strconv.Atoi(params.Cursor)
	end := min(start+params.Limit, len(out))
	if end == len(out) {
		return out[start:end], false, "", nil
	}
	return out[start:end], true, strconv.Itoa(end), nil
}

func (f *fakeSlack) GetConversationHistoryContext(_ context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(methodHistory); err != nil {
		return nil, err
	}
	if err := f.call(historyOf(params.ChannelID)); err != nil {
		return nil, err
	}
	resp := &slack.GetConversationHistoryResponse{}
	msgs := f.channels[params.ChannelID]
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		top := m.ThreadTimestamp == "" || m.ThreadTimestamp == m.Timestamp
		if top && (params.Oldest == "" || tsLess(params.Oldest, m.Timestamp)) {
			resp.Messages = append(resp.Messages, m)
		}
		if params.Limit > 0 && len(resp.Messages) == params.Limit {
			break
		}
	}
	return resp, nil
}

const fakeListPage = 2

func (f *fakeSlack) GetConversationsContext(_ context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("conversations.list"); err != nil {
		return nil, "", err
	}
	if !params.ExcludeArchived || !slices.Equal(params.Types, []string{"public_channel", "private_channel"}) {
		return nil, "", fmt.Errorf("conversations.list params = %+v, want unarchived public and private channels", params)
	}
	start, _ := strconv.Atoi(params.Cursor)
	end := min(start+fakeListPage, len(f.listed))
	var out []slack.Channel
	for _, c := range f.listed[start:end] {
		if !c.IsArchived {
			out = append(out, c)
		}
	}
	if end == len(f.listed) {
		return out, "", nil
	}
	return out, strconv.Itoa(end), nil
}

func (f *fakeSlack) list(channels ...slack.Channel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = append(f.listed, channels...)
}

func (f *fakeSlack) GetUploadURLExternalContext(_ context.Context, params slack.GetUploadURLExternalParameters) (*slack.GetUploadURLExternalResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("files.getUploadURLExternal"); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("F%d", len(f.uploads)+1)
	f.uploads[id] = nil
	return &slack.GetUploadURLExternalResponse{UploadURL: "https://files.slack.test/upload/" + id, FileID: id}, nil
}

func (f *fakeSlack) UploadToURL(_ context.Context, params slack.UploadToURLParameters) error {
	body, err := io.ReadAll(params.Reader)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("upload"); err != nil {
		return err
	}
	f.uploads[strings.TrimPrefix(params.UploadURL, "https://files.slack.test/upload/")] = body
	return nil
}

func (f *fakeSlack) CompleteUploadExternalContext(_ context.Context, params slack.CompleteUploadExternalParameters) (*slack.CompleteUploadExternalResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("files.completeUploadExternal"); err != nil {
		return nil, err
	}
	if params.Channel != "" || len(params.Channels) > 0 {
		return nil, fmt.Errorf("completeUploadExternal shared the files into %q", params.Channel)
	}
	f.complete = append(f.complete, params.Files)
	return &slack.CompleteUploadExternalResponse{Files: params.Files}, nil
}

func (f *fakeSlack) ShareFilesContext(_ context.Context, share fileShare) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("files.completeUploadExternal"); err != nil {
		return err
	}
	f.clock++
	ts := fmt.Sprintf("%d.000000", f.clock)
	p := fakePost{channelID: share.channelID, threadTS: share.threadTS, text: share.text, username: share.username, iconURL: share.iconURL, ts: ts}
	f.posts = append(f.posts, p)
	f.channels[share.channelID] = append(f.channels[share.channelID], slack.Message{Msg: slack.Msg{
		Timestamp: ts, ThreadTimestamp: p.threadTS, User: f.auth.UserID, BotID: f.auth.BotID, Username: p.username, Text: p.text,
	}})
	f.complete = append(f.complete, share.files)
	for _, file := range share.files {
		f.shared[file.ID] = slack.Share{Public: map[string][]slack.ShareFileInfo{share.channelID: {{Ts: ts, ThreadTs: share.threadTS}}}}
	}
	return nil
}

func (f *fakeSlack) GetFileInfoContext(_ context.Context, fileID string, _, _ int) (*slack.File, []slack.Comment, *slack.Paging, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("files.info"); err != nil {
		return nil, nil, nil, err
	}
	known := f.files[fileID]
	file := &slack.File{
		ID: fileID, Name: known.Name, Mimetype: known.Mimetype, Permalink: known.Permalink, Shares: f.shared[fileID],
		URLPrivateDownload: known.URLPrivateDownload, Size: known.Size,
	}
	if f.pending > 0 {
		f.pending--
		return file, nil, nil, nil
	}
	file.OriginalW = 640
	return file, nil, nil, nil
}

func (f *fakeSlack) serveFile(file slack.File, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[file.ID] = file
	f.downloads[file.URLPrivateDownload] = body
}

func (f *fakeSlack) GetFileContext(_ context.Context, downloadURL string, w io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("files.download"); err != nil {
		return err
	}
	_, err := w.Write(f.downloads[downloadURL])
	return err
}

func (f *fakeSlack) CreateConversationContext(_ context.Context, params slack.CreateConversationParams) (*slack.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("conversations.create"); err != nil {
		return nil, err
	}
	if params.IsPrivate {
		return nil, fmt.Errorf("conversations.create made %q private", params.ChannelName)
	}
	f.created = append(f.created, params.ChannelName)
	ch := &slack.Channel{}
	ch.ID, ch.Name = fmt.Sprintf("CNEW%d", len(f.created)), params.ChannelName
	return ch, nil
}

func (f *fakeSlack) ArchiveConversationContext(_ context.Context, channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("conversations.archive"); err != nil {
		return err
	}
	f.archived = append(f.archived, channelID)
	return nil
}

func (f *fakeSlack) InviteUsersToConversationContext(_ context.Context, channelID string, users ...string) (*slack.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("conversations.invite"); err != nil {
		return nil, err
	}
	f.invites[channelID] = append(f.invites[channelID], users...)
	ch := &slack.Channel{}
	ch.ID = channelID
	return ch, nil
}

func (f *fakeSlack) SetTopicOfConversationContext(_ context.Context, channelID, topic string) (*slack.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("conversations.setTopic"); err != nil {
		return nil, err
	}
	f.topics[channelID] = topic
	ch := &slack.Channel{}
	ch.ID = channelID
	return ch, nil
}

func (f *fakeSlack) channelChanges() ([]string, []string, map[string][]string, map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.created), slices.Clone(f.archived), maps.Clone(f.invites), maps.Clone(f.topics)
}

func (f *fakeSlack) completed() [][]slack.FileSummary {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]slack.FileSummary(nil), f.complete...)
}

type directed struct {
	subjectID string
	agentID   string
	origin    string
	text      string
}

type fakePlane struct {
	st         *store.Store
	events     []event.Event
	directives []directed
	appendErr  error
	directErr  error
}

func (f *fakePlane) DB() *sql.DB { return f.st.DB() }

func (f *fakePlane) Append(ctx context.Context, e *event.Event) (int64, error) {
	if f.appendErr != nil {
		return 0, f.appendErr
	}
	seq, err := f.st.AppendEvent(ctx, e)
	if err != nil {
		return 0, err
	}
	f.events = append(f.events, *e)
	return seq, nil
}

func (f *fakePlane) Direct(ctx context.Context, subjectID, agentID, origin, text string) (agent.Directive, error) {
	if f.directErr != nil {
		return agent.Directive{}, f.directErr
	}
	d, _, err := f.st.EnqueueDirective(ctx, subjectID, agentID, origin, text, time.Now())
	if err != nil {
		return agent.Directive{}, err
	}
	f.directives = append(f.directives, directed{subjectID: subjectID, agentID: agentID, origin: origin, text: text})
	return d, nil
}

const (
	testSubject = "0123456789abcdef0123456789abcdef"
	testSession = "sess-1"
	testPID     = 4242
	testUser    = "U0OWNER01"
	testChannel = "C1"
	testThread  = "1800000000.000100"
)

type fixture struct {
	t         *testing.T
	ctx       context.Context
	slack     *fakeSlack
	plane     *fakePlane
	poller    *poller
	app       *app
	cache     *readCache
	orca      *fakeOrca
	responses *fakeResponses
	now       time.Time
	alive     map[int]bool
	id        identity
}

func agentAvatar(_ context.Context, id identity) (string, error) {
	return id.Image512 + "?agent" + avatarStyle, nil
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"), storeSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{
		t: t, ctx: ctx, slack: newFakeSlack(), plane: &fakePlane{st: st},
		now: time.Unix(1_800_000_100, 0), alive: map[int]bool{testPID: true},
		orca: newFakeOrca(), responses: &fakeResponses{},
	}
	f.slack.users[testUser] = &slack.User{ID: testUser, RealName: "Yasyf Mohamedali", Profile: slack.UserProfile{
		FirstName: "Yasyf", Email: "ym@example.com", Image512: "https://avatars/ym.png", DisplayName: "yasyf",
	}}
	f.slack.users["UOTHER"] = &slack.User{ID: "UOTHER", Profile: slack.UserProfile{DisplayName: "teammate"}}
	f.cache = openTestCache(t)
	f.cache.now = func() time.Time { return f.now }
	f.app = newApp(func(string) slackAPI { return f.slack }, func(pid int) bool { return f.alive[pid] }, f.cache, openTestReplay(t))
	f.app.now = func() time.Time { return f.now }
	f.app.avatar = agentAvatar
	f.poller = f.app.poller
	f.poller.plane = f.plane
	f.poller.now = func() time.Time { return f.now }
	f.poller.log.SetOutput(testWriter{t})
	sh := f.app.shortcuts
	sh.plane = f.plane
	sh.avatar = agentAvatar
	sh.spawn = func(fn func(context.Context)) { fn(f.ctx) }
	sh.orca = f.orca.run
	sh.http = &http.Client{Transport: f.responses}
	sh.now = func() time.Time { return f.now }
	sh.readyWait = 200 * time.Millisecond
	sh.readyPoll = 10 * time.Millisecond
	sh.createTimeout = 50 * time.Millisecond
	sh.settleWait = 200 * time.Millisecond
	sh.settlePoll = time.Millisecond
	sh.log.SetOutput(testWriter{t})
	if _, err := store.NewSubjectStore(st.DB()).Create(ctx, testSubject, "", testSession, scopeName, testPID, statusOpen); err != nil {
		t.Fatal(err)
	}
	f.id, err = resolveIdentity(ctx, f.slack, "xoxb-test", "ym@example.com", time.Unix(f.slack.clock, 0))
	if err != nil {
		t.Fatal(err)
	}
	f.id.SocketURL, f.id.RepoRoot, f.id.OrcaPath = "wss://socket.example/live", testRepo, "/usr/local/bin/orca"
	f.id.AvatarURL, _ = agentAvatar(ctx, f.id)
	if err := saveIdentity(ctx, st.DB(), f.id); err != nil {
		t.Fatal(err)
	}
	f.poller.reconcileAll()
	return f
}

func openTestReplay(t *testing.T) *replayCursor {
	t.Helper()
	r, err := loadReplayCursor(filepath.Join(t.TempDir(), "nudge-seq"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func openTestCache(t *testing.T) *readCache {
	t.Helper()
	c, err := openReadCache(context.Background(), filepath.Join(t.TempDir(), "read-cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	c.log.SetOutput(testWriter{t})
	return c
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

func (f *fixture) db() *sql.DB { return f.plane.st.DB() }

func (f *fixture) watch(owner, oldest string) int64 {
	f.t.Helper()
	id, _, err := insertWatch(f.ctx, f.db(), testSubject, owner, kindThread, testChannel, testThread, oldest, f.now)
	if err != nil {
		f.t.Fatal(err)
	}
	f.nudgeThread()
	return id
}

func (f *fixture) nudgeThread() {
	f.poller.nudged(messageNudge{Channel: testChannel, TS: formatTS(f.now), ThreadTS: testThread})
}

func (f *fixture) tick() time.Time {
	f.t.Helper()
	next, err := f.poller.tick(f.ctx)
	if err != nil {
		f.t.Fatalf("tick: %v", err)
	}
	return next
}

func (f *fixture) cursor() cursorRow {
	f.t.Helper()
	c, err := loadCursor(f.ctx, f.db(), cursorKey{testChannel, testThread})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) watchStatus(id int64) string {
	f.t.Helper()
	var status string
	if err := f.db().QueryRowContext(f.ctx, `SELECT status FROM watches WHERE id = ?`, id).Scan(&status); err != nil {
		f.t.Fatal(err)
	}
	return status
}

func reply(ts, text string) slack.Message {
	return slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: testThread, User: "UOTHER", Text: text}}
}

func storeSubjects(f *fixture) subject.Store { return store.NewSubjectStore(f.db()) }

func subjectFor(t *testing.T, f *fixture) subject.Subject {
	t.Helper()
	sub, err := storeSubjects(f).Get(f.ctx, testSubject)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func registerAgent(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := f.plane.st.RegisterAgent(f.ctx, agent.Info{
		SubjectID: testSubject, AgentID: "agent-1", AgentType: waiterAgentType, SessionID: testSession,
		Status: agent.StatusRunning, StartedAt: f.now,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeSlack) sent() []fakePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePost(nil), f.posts...)
}

const (
	storedUserToken = "xoxc-stored"
	chromeUserToken = "xoxc-chrome"
	chromeCookie    = "xoxd-chrome%2F1"
)

type fakeUserCredential struct {
	mu     sync.Mutex
	stored *userCredential
	slacks map[string]*fakeSlack
	chrome string
	saved  []userCredential
}

func newFakeUserCredential() *fakeUserCredential {
	return &fakeUserCredential{slacks: map[string]*fakeSlack{}, chrome: `{"cookies": [], "origins": []}`}
}

func (c *fakeUserCredential) session() userSession {
	return userSession{read: c.read, save: c.save, cookiesync: c.cookiesync, api: c.api}
}

func (c *fakeUserCredential) read(context.Context) (userCredential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stored == nil {
		return userCredential{}, errNoUserCredential
	}
	return *c.stored, nil
}

func (c *fakeUserCredential) save(_ context.Context, cred userCredential) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saved = append(c.saved, cred)
	return nil
}

func (c *fakeUserCredential) cookiesync(_ context.Context, bin string, args ...string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if bin != cookiesyncBinary || !slices.Equal(args, []string{"cookies", slackWebOrigin, "--browser", "chrome", "--format", "playwright"}) {
		return nil, fmt.Errorf("unexpected command %s %v", bin, args)
	}
	return []byte(c.chrome), nil
}

func (c *fakeUserCredential) api(cred userCredential) slackAPI {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.slacks[cred.Token]
}

func (c *fakeUserCredential) addSession(token, userID string) *fakeSlack {
	f := newFakeSlack()
	f.auth = slack.AuthTestResponse{URL: "https://acme.slack.com/", TeamID: "T1", UserID: userID}
	c.slacks[token] = f
	return f
}

func (c *fakeUserCredential) as(userID string) *fakeSlack {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stored = &userCredential{Token: storedUserToken, Cookie: "xoxd-stored"}
	return c.addSession(storedUserToken, userID)
}

func (c *fakeUserCredential) inChrome(userID string) *fakeSlack {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chrome = chromeStorageJSON("T1", chromeUserToken, chromeCookie)
	return c.addSession(chromeUserToken, userID)
}

func (c *fakeUserCredential) savedCredentials() []userCredential {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.saved)
}

func chromeStorageJSON(teamID, token, cookie string) string {
	config, err := json.Marshal(map[string]any{"teams": map[string]any{teamID: map[string]string{"token": token}}})
	if err != nil {
		panic(err)
	}
	state, err := json.Marshal(map[string]any{
		"cookies": []map[string]string{
			{"name": "b", "domain": slackCookieDomain, "value": "other"},
			{"name": slackSessionCookie, "domain": slackCookieDomain, "value": cookie},
		},
		"origins": []map[string]any{{"origin": slackWebOrigin, "localStorage": []map[string]string{{"name": slackLocalConfig, "value": string(config)}}}},
	})
	if err != nil {
		panic(err)
	}
	return string(state)
}

func (f *fakeSlack) sentTo(channelID string) []fakePost {
	return slices.DeleteFunc(f.sent(), func(p fakePost) bool { return p.channelID != channelID })
}

func (f *fakeSlack) joinedChannels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.joined...)
}

func (f *fakeSlack) setUser(u *slack.User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[u.ID] = u
}
