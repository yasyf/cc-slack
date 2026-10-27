package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/cmd"
	"github.com/yasyf/cc-interact/consume"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/event"
)

func TestParsePermalink(t *testing.T) {
	cases := map[string]permalinkRef{
		"https://acme.slack.com/archives/C1/p1800000000000300":                                    {channelID: "C1", ts: "1800000000.000300", threadTS: "1800000000.000300"},
		"https://acme.slack.com/archives/C1/p1800000000000300?thread_ts=1800000000.000100&cid=C1": {channelID: "C1", ts: "1800000000.000300", threadTS: "1800000000.000100"},
		"https://acme.slack.com/archives/G2":                                                      {channelID: "G2"},
		"https://acme.slack.com/archives/D3/p1800000000000300/":                                   {channelID: "D3", ts: "1800000000.000300", threadTS: "1800000000.000300"},
		"https://acme.slack.com/files/U1/F0123FILE01/screenshot.png":                              {fileID: "F0123FILE01"},
	}
	for raw, want := range cases {
		got, err := parsePermalink(raw)
		if err != nil {
			t.Fatalf("parsePermalink(%q): %v", raw, err)
		}
		if got != want {
			t.Errorf("parsePermalink(%q) = %+v, want %+v", raw, got, want)
		}
	}
	for _, bad := range []string{"https://acme.slack.com/team/U1", "https://acme.slack.com/archives/C1/x123", "https://acme.slack.com/files/U1"} {
		if _, err := parsePermalink(bad); err == nil {
			t.Errorf("parsePermalink(%q) accepted a non-message url", bad)
		}
	}
}

func TestPermalinkRoundTrips(t *testing.T) {
	id := identity{TeamURL: "https://acme.slack.com/"}
	link := id.permalink("C1", "1800000000.000300", testThread)
	got, err := parsePermalink(link)
	if err != nil {
		t.Fatal(err)
	}
	if got.channelID != "C1" || got.ts != "1800000000.000300" || got.threadTS != testThread {
		t.Fatalf("round trip = %+v", got)
	}
}

func loggedIn(t *testing.T, l *liveDaemon) {
	t.Helper()
	if who := loginAs(t, l, "yasyf"); who.Username != "yasyf (agent)" || who.DMChannelID != "D"+testUser || who.BotID != "BBOT" {
		t.Fatalf("whoami = %+v", who)
	}
}

func loginAs(t *testing.T, l *liveDaemon, displayName string) whoami {
	t.Helper()
	l.slack.setUser(&slack.User{ID: testUser, Name: "ym", RealName: "Yasyf Mohamedali", Profile: slack.UserProfile{
		Email: "ym@example.com", Image512: "https://avatars/ym.png", DisplayName: displayName,
	}})
	var who whoami
	if err := json.Unmarshal(l.do(opLogin, map[string]string{
		"token": "xoxb-test", "email": "ym@example.com", "socket_url": "ws://127.0.0.1:1/live", "repo_root": "/repo", "capt_hook_path": testCaptHook,
		"avatar_key_id": "r2-key", "avatar_secret": "r2-secret",
	}).Body, &who); err != nil {
		t.Fatal(err)
	}
	return who
}

func TestLoginPostsAsTheHandleWithoutADisplayName(t *testing.T) {
	l := startDaemon(t)
	if who := loginAs(t, l, ""); who.Username != "ym (agent)" {
		t.Fatalf("whoami username = %q, want the handle fallback %q", who.Username, "ym (agent)")
	}
	var who whoami
	if err := json.Unmarshal(l.do(opWhoami, map[string]string{}).Body, &who); err != nil {
		t.Fatal(err)
	}
	if who.Username != "ym (agent)" {
		t.Fatalf("whoami after reload = %q, want %q", who.Username, "ym (agent)")
	}
}

func TestPostsNeverUnfurlLinks(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.post(opSend, map[string]string{"channel_id": testChannel, "text": "card: <https://example.slack.com/archives/C1/p1|card>"})
	if posts := l.slack.sent(); len(posts) != 1 || posts[0].unfurlLinks != "false" {
		t.Fatalf("posts = %+v, want one post with unfurl_links=false so a linked card is never frozen into a stale snapshot", posts)
	}
}

const testDocCardsURL = "https://cards.example.com/doc-cards"

func TestDocsLinksCarryTheCardsTheServiceDraws(t *testing.T) {
	l := startDaemon(t)
	l.slack.setUser(&slack.User{ID: testUser, Name: "ym", Profile: slack.UserProfile{Email: "ym@example.com", DisplayName: "yasyf"}})
	l.do(opLogin, map[string]any{
		"token": "xoxb-test", "email": "ym@example.com", "socket_url": "ws://127.0.0.1:1/live", "repo_root": "/repo", "capt_hook_path": testCaptHook,
		"config": map[string]any{"doc_cards": map[string]string{"link": "https://docs.example.com/", "url": testDocCardsURL}},
	})
	design := "https://docs.example.com/design-docs/ai-oncall/"
	card := slack.WorkObjectEntity{
		URL:           design,
		ExternalRef:   slack.WorkObjectExternalRef{ID: "design-docs/ai-oncall", Type: "doc"},
		EntityType:    slack.EntityTypeContentItem,
		EntityPayload: map[string]any{"attributes": map[string]any{"title": map[string]any{"text": "AI on-call: first-layer triage for Datadog pages"}}},
	}
	l.slack.cards = []slack.WorkObjectEntity{card}
	text := "read <" + design + "|this design-doc link>"
	l.post(opSend, map[string]string{"channel_id": testChannel, "text": text})
	l.post(opSend, map[string]string{"channel_id": testChannel, "text": "no docs here: <https://example.com|x>"})
	l.slack.fail("doc-cards", errors.New("doc-cards answered 502 Bad Gateway"))
	l.post(opSend, map[string]string{"channel_id": testChannel, "text": text})
	posts := l.slack.sent()
	if len(posts) != 3 {
		t.Fatalf("posts = %+v, want three", posts)
	}
	for _, p := range posts[1:] {
		var tag slack.SlackMetadata
		if err := json.Unmarshal([]byte(p.metadata), &tag); err != nil || tag.EventType != postEventType || tag.EventPayload["user_id"] != testUser {
			t.Fatalf("post = %+v, want the author tag in place of cards", p)
		}
	}
	var metadata slack.WorkObjectMetadata
	if err := json.Unmarshal([]byte(posts[0].metadata), &metadata); err != nil {
		t.Fatalf("metadata %q: %v", posts[0].metadata, err)
	}
	if !reflect.DeepEqual(metadata.Entities, []slack.WorkObjectEntity{card}) {
		t.Fatalf("cards = %+v, want the service's card", metadata.Entities)
	}
	asked := testDocCardsURL + " " + testChannel + " " + text
	if want := []string{asked, asked}; !slices.Equal(l.slack.asked, want) {
		t.Fatalf("asked the doc-cards service %q, want %q", l.slack.asked, want)
	}
}

func TestDocsLinksCarryNoCardsWithoutTheService(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.cards = []slack.WorkObjectEntity{{URL: "https://docs.example.com/x/"}}
	l.post(opSend, map[string]string{"channel_id": testChannel, "text": "read <https://docs.example.com/x/|this>"})
	if len(l.slack.asked) != 0 {
		t.Fatalf("asked %q, want no doc-cards call without [doc_cards]", l.slack.asked)
	}
}

type answering func(*http.Request) (*http.Response, error)

func (a answering) Do(req *http.Request) (*http.Response, error) { return a(req) }

func TestDocCardsAskTheServiceWithTheBotToken(t *testing.T) {
	var asked *http.Request
	var body map[string]string
	api := newSlackClient("xoxb-test", slack.APIURL, answering(func(req *http.Request) (*http.Response, error) {
		asked = req
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		answer := `{"entities":[{"url":"https://docs.example.com/design-docs/ai-oncall/","external_ref":{"id":"design-docs/ai-oncall","type":"doc"},"entity_type":"slack#/entities/content_item","entity_payload":{"attributes":{"title":{"text":"AI on-call"}}}}]}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(answer)), Request: req}, nil
	}))
	cards, err := api.DocCardsContext(context.Background(), testDocCardsURL, testChannel, "https://docs.example.com/design-docs/ai-oncall/")
	if err != nil {
		t.Fatal(err)
	}
	if asked.URL.String() != testDocCardsURL || asked.Method != http.MethodPost || asked.Header.Get("Authorization") != "Bearer xoxb-test" ||
		body["channel"] != testChannel || body["text"] != "https://docs.example.com/design-docs/ai-oncall/" {
		t.Fatalf("asked %s %s with %v", asked.Method, asked.URL, body)
	}
	if len(cards) != 1 || cards[0].ExternalRef.ID != "design-docs/ai-oncall" || cards[0].EntityType != slack.EntityTypeContentItem {
		t.Fatalf("cards %+v", cards)
	}
}

func TestOpsRefuseWithoutLoginAndNameTheFallback(t *testing.T) {
	l := startDaemon(t)
	raw, _ := json.Marshal(map[string]string{"channel_id": testChannel, "text": "hi"})
	_, err := invoke(l.ctx, l.deps, testSession, "/any", opSend, raw)
	if err == nil || !strings.Contains(err.Error(), "cc-slack login") || !strings.Contains(err.Error(), mcpSend) {
		t.Fatalf("send before login: %v", err)
	}
}

func TestSendReplyReactAndThread(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]string{"channel_id": testChannel, "text": "PR is up"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	link := "https://acme.slack.com/archives/C1/p" + strings.ReplaceAll(sent.TS, ".", "")
	if sent.Permalink != link {
		t.Fatalf("permalink = %q, want %q", sent.Permalink, link)
	}
	var replied postResult
	if err := json.Unmarshal(l.post(opReply, map[string]string{"url": sent.Permalink, "text": "CI is green", "agent": "claude"}).Body, &replied); err != nil {
		t.Fatal(err)
	}
	if got := l.slack.sent()[0]; got.username != "yasyf (agent)" {
		t.Fatalf("send = %+v, want it as yasyf (agent) without an agent", got)
	}
	if got := l.slack.sent()[1]; got.threadTS != sent.TS || got.username != "yasyf (claude)" {
		t.Fatalf("reply = %+v, want it in the sent message's thread as yasyf (claude)", got)
	}
	l.refused(opReply, l.hooked(opReply, map[string]string{"url": sent.Permalink, "text": "hi", "agent": "Claude Code"}), "lowercase agent name")
	if n := len(l.slack.sent()); n != 2 {
		t.Fatalf("sent %d messages, want the refused agent name to send nothing", n)
	}
	l.do(opReact, map[string]string{"channel_id": testChannel, "ts": sent.TS, "name": ":white_check_mark:"})
	if n := l.slack.callCount("reactions.add"); n != 1 {
		t.Fatalf("reactions.add calls = %d", n)
	}

	var thread struct {
		Messages []threadMessage `json:"messages"`
	}
	if err := json.Unmarshal(l.do(opThread, map[string]string{"url": sent.Permalink}).Body, &thread); err != nil {
		t.Fatal(err)
	}
	if len(thread.Messages) != 2 || !thread.Messages[1].FromClaude || thread.Messages[1].UserName != "yasyf (claude)" ||
		thread.Messages[1].Text != "CI is green" {
		t.Fatalf("thread = %+v", thread.Messages)
	}

	raw, _ := json.Marshal(map[string]string{"channel_id": testChannel, "text": "no thread"})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opReply, raw); err == nil || !strings.Contains(err.Error(), "thread_ts") {
		t.Fatalf("reply without a thread: %v", err)
	}
}

func TestHistoryReturnsTopLevelMessagesNewestFirst(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000100", User: testUser, Text: "oldest"}})
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{
		Timestamp: "1800000000.000200", BotID: "BBOT", Username: "yasyf", Text: "PR is up\nsecond line",
		ReplyCount: 2, LatestReply: "1800000000.000400",
		Files: []slack.File{{ID: "F1", Name: "chart.png", Mimetype: "image/png", Permalink: "https://acme.slack.com/files/U1/F1/chart.png"}},
	}})
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000300", User: testUser, Text: "newest"}})
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000400", ThreadTimestamp: "1800000000.000200", User: testUser, Text: "reply"}})

	var res historyResult
	if err := json.Unmarshal(l.do(opHistory, map[string]any{"channel_id": testChannel, "limit": 2}).Body, &res); err != nil {
		t.Fatal(err)
	}
	want := []historyMessage{
		{TS: "1800000000.000300", User: testUser, UserName: "yasyf", FromUser: true, Text: "newest", Permalink: "https://acme.slack.com/archives/C1/p1800000000000300"},
		{
			TS: "1800000000.000200", UserName: "yasyf", FromClaude: true, Text: "PR is up\nsecond line",
			Permalink: "https://acme.slack.com/archives/C1/p1800000000000200", ReplyCount: 2, LatestReply: "1800000000.000400",
			Files: []messageFile{{ID: "F1", Name: "chart.png", Mimetype: "image/png", Permalink: "https://acme.slack.com/files/U1/F1/chart.png"}},
		},
	}
	if res.ChannelID != testChannel || !reflect.DeepEqual(res.Messages, want) {
		t.Fatalf("history = %+v, want %+v", res, want)
	}

	var buf strings.Builder
	w := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	renderHistory(w, res)
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "2027-01-15 00:00  1800000000.000300") ||
		!strings.Contains(lines[1], "2 replies") || !strings.HasSuffix(lines[1], "PR is up") {
		t.Fatalf("rendered history = %q", buf.String())
	}

	raw, _ := json.Marshal(map[string]string{})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opHistory, raw); err == nil || !strings.Contains(err.Error(), "channel_id") {
		t.Fatalf("history without a channel: %v", err)
	}
	l.slack.fail(historyOf("C9"), slack.SlackErrorResponse{Err: "not_in_channel"})
	raw, _ = json.Marshal(map[string]string{"channel_id": "C9"})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opHistory, raw); err == nil || !strings.Contains(err.Error(), mcpHistory) {
		t.Fatalf("unreadable channel: %v", err)
	}
}

func TestThreadCarriesEachMessagesFiles(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000100", User: testUser, Text: "status"}})
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{
		Timestamp: "1800000000.000200", ThreadTimestamp: "1800000000.000100", BotID: "BBOT", Username: "yasyf", Text: "chart",
		Files: []slack.File{{ID: "F1", Name: "chart.png", Mimetype: "image/png", Permalink: "https://acme.slack.com/files/U1/F1/chart.png"}},
	}})

	var res threadResult
	if err := json.Unmarshal(l.do(opThread, map[string]string{"channel_id": testChannel, "thread_ts": "1800000000.000100"}).Body, &res); err != nil {
		t.Fatal(err)
	}
	want := []messageFile{{ID: "F1", Name: "chart.png", Mimetype: "image/png", Permalink: "https://acme.slack.com/files/U1/F1/chart.png"}}
	if len(res.Messages) != 2 || res.Messages[0].Files != nil || !reflect.DeepEqual(res.Messages[1].Files, want) {
		t.Fatalf("thread = %+v, want only the reply to carry chart.png", res.Messages)
	}
}

func TestABotPostsSharedImageBlockReadsAsItsFile(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.files["F2"] = slack.File{ID: "F2", Name: "sysd.png", Mimetype: "image/png", Permalink: "https://acme.slack.com/files/U1/F2/sysd.png"}
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{
		Timestamp: "1800000000.000500", BotID: "BBOT", Username: "yasyf", Text: "kickoff",
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, "kickoff", false, false), nil, nil),
			&slack.ImageBlock{Type: slack.MBTImage, AltText: "sysd.png", SlackFile: &slack.SlackFileObject{ID: "F2"}},
		}},
	}})

	var res historyResult
	if err := json.Unmarshal(l.do(opHistory, map[string]any{"channel_id": testChannel}).Body, &res); err != nil {
		t.Fatal(err)
	}
	want := []messageFile{{ID: "F2", Name: "sysd.png", Mimetype: "image/png", Permalink: "https://acme.slack.com/files/U1/F2/sysd.png"}}
	if len(res.Messages) != 1 || !reflect.DeepEqual(res.Messages[0].Files, want) {
		t.Fatalf("history = %+v, want the image block's file", res.Messages)
	}
}

func TestChannelsListsUnarchivedChannelsByPrefix(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	conv := func(id, name string, created int64, archived bool, previous ...string) slack.Channel {
		return slack.Channel{GroupConversation: slack.GroupConversation{
			Conversation: slack.Conversation{ID: id, Created: slack.JSONTime(created), PreviousNames: previous}, Name: name, IsArchived: archived,
		}}
	}
	l.slack.list(
		conv("C3", "test-oncall-b", 1_800_000_300, false, "test-oncall-renamed"),
		conv("C1", "general", 1_800_000_100, false),
		conv("C2", "test-oncall-a", 1_800_000_200, false),
		conv("C4", "test-oncall-old", 1_800_000_400, true),
		conv("C5", "test-oncall-c", 1_800_000_500, false),
	)

	var res channelsResult
	if err := json.Unmarshal(l.do(opChannels, map[string]string{"prefix": "test-oncall-"}).Body, &res); err != nil {
		t.Fatal(err)
	}
	want := []channelInfo{
		{ID: "C2", Name: "test-oncall-a", Created: 1_800_000_200},
		{ID: "C3", Name: "test-oncall-b", PreviousNames: []string{"test-oncall-renamed"}, Created: 1_800_000_300},
		{ID: "C5", Name: "test-oncall-c", Created: 1_800_000_500},
	}
	if !reflect.DeepEqual(res.Channels, want) {
		t.Fatalf("channels = %+v, want %+v", res.Channels, want)
	}
	if n := l.slack.callCount("conversations.list"); n != 3 {
		t.Fatalf("conversations.list calls = %d, want 3 pages", n)
	}
	if err := json.Unmarshal(l.do(opChannels, map[string]string{}).Body, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Channels) != 4 || res.Channels[0].Name != "general" {
		t.Fatalf("unfiltered channels = %+v", res.Channels)
	}

	var buf strings.Builder
	renderChannels(&buf, channelsResult{Channels: want[:1]})
	if got := buf.String(); got != "C2\ttest-oncall-a\t2027-01-15 00:03\n" {
		t.Fatalf("rendered channels = %q", got)
	}
}

func TestUsersMatchesHandleDisplayRealNameAndEmailIgnoringCase(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	user := func(id, name, display, real, email string, deleted bool) *slack.User {
		return &slack.User{ID: id, Name: name, RealName: real, Deleted: deleted, Profile: slack.UserProfile{DisplayName: display, Email: email}}
	}
	l.slack.setUser(user("U1", "jrivera", "Josh", "Josh Rivera", "josh@example.com", false))
	l.slack.setUser(user("U2", "amy", "Rivera Fan", "Amy Chen", "amy@example.com", false))
	l.slack.setUser(user("U3", "bob", "", "Bob Smith", "bob.rivera@example.com", false))
	l.slack.setUser(user("U4", "carol", "", "Carol Rivera", "carol@example.com", true))
	l.slack.setUser(user("U5", "dave", "", "Dave Jones", "dave@example.com", false))

	var res usersResult
	if err := json.Unmarshal(l.do(opUsers, map[string]string{"query": "RIVERA"}).Body, &res); err != nil {
		t.Fatal(err)
	}
	want := []userInfo{
		{ID: "U2", Name: "amy", DisplayName: "Rivera Fan", RealName: "Amy Chen", Email: "amy@example.com"},
		{ID: "U3", Name: "bob", RealName: "Bob Smith", Email: "bob.rivera@example.com"},
		{ID: "U1", Name: "jrivera", DisplayName: "Josh", RealName: "Josh Rivera", Email: "josh@example.com"},
	}
	if !slices.Equal(res.Users, want) {
		t.Fatalf("users = %+v, want %+v", res.Users, want)
	}

	raw, _ := json.Marshal(map[string]string{})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opUsers, raw); err == nil || !strings.Contains(err.Error(), "query") {
		t.Fatalf("empty query: %v", err)
	}

	var buf strings.Builder
	renderUsers(&buf, usersResult{Users: want[2:]})
	if got := buf.String(); got != "U1\tjrivera\tJosh Rivera\tjosh@example.com\n" {
		t.Fatalf("rendered users = %q", got)
	}
}

func TestPostsWatchTheirThreadUnlessToldNotTo(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	var sent, replied, quiet postResult
	if err := json.Unmarshal(l.post(opSend, map[string]string{"channel_id": testChannel, "text": "from now on we deploy on merge"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(l.post(opReply, map[string]string{"url": sent.Permalink, "text": "first one is out"}).Body, &replied); err != nil {
		t.Fatal(err)
	}
	if sent.WatchID == 0 || replied.WatchID != sent.WatchID {
		t.Fatalf("send watch %d, reply watch %d, want one watch on the post's thread", sent.WatchID, replied.WatchID)
	}
	var owner, kind, channelID, threadTS, cursor string
	if err := l.srv.DB().QueryRowContext(l.ctx, `SELECT w.owner_agent_id, w.kind, w.channel_id, w.thread_ts, c.oldest_ts FROM watches w
		JOIN cursors c ON c.channel_id = w.channel_id AND c.thread_ts = w.thread_ts WHERE w.id = ? AND w.status = 'active'`,
		sent.WatchID).Scan(&owner, &kind, &channelID, &threadTS, &cursor); err != nil {
		t.Fatal(err)
	}
	if owner != agent.TopLevel || kind != kindThread || channelID != testChannel || threadTS != sent.TS || cursor != sent.TS {
		t.Fatalf("watch = owner %q kind %q %s/%s cursor %s, want a top-level thread watch from the post", owner, kind, channelID, threadTS, cursor)
	}

	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "fyi", "no_watch": true}).Body, &quiet); err != nil {
		t.Fatal(err)
	}
	var watches int
	if err := l.srv.DB().QueryRowContext(l.ctx, `SELECT COUNT(*) FROM watches`).Scan(&watches); err != nil {
		t.Fatal(err)
	}
	if quiet.WatchID != 0 || watches != 1 {
		t.Fatalf("no_watch post armed watch %d (%d watches), want none", quiet.WatchID, watches)
	}

	stranger := l.deps
	stranger.ClaudePID = func() int { return testPID + 1 }
	raw, _ := json.Marshal(l.hooked(opSend, map[string]string{"channel_id": testChannel, "text": "hi"}))
	reply, err := invoke(l.ctx, stranger, "other-session", "/any", opSend, raw)
	if err != nil {
		t.Fatalf("watched send from a window that never ran session-start: %v", err)
	}
	var started postResult
	if err := json.Unmarshal(reply.Body, &started); err != nil {
		t.Fatal(err)
	}
	if started.WatchID == 0 {
		t.Fatalf("send from an unregistered window armed no watch: %+v", started)
	}
	if n := len(l.slack.sent()); n != 4 {
		t.Fatalf("posts = %d, want 4", n)
	}
}

func TestFollowJoinsPublicChannelsAndDedupes(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: testThread, Text: "parent"}})
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000200", ThreadTimestamp: testThread, Text: "earlier"}})

	var first followResult
	if err := json.Unmarshal(l.do(opFollow, map[string]string{"channel_id": testChannel, "thread_ts": testThread}).Body, &first); err != nil {
		t.Fatal(err)
	}
	if !first.Joined || first.Cursor != "1800000000.000200" || len(l.slack.joinedChannels()) != 1 {
		t.Fatalf("follow = %+v joined %v, want a join and the thread's latest ts as cursor", first, l.slack.joinedChannels())
	}
	var again followResult
	if err := json.Unmarshal(l.do(opFollow, map[string]string{"channel_id": testChannel, "thread_ts": testThread}).Body, &again); err != nil {
		t.Fatal(err)
	}
	if again.WatchID != first.WatchID {
		t.Fatalf("second follow made watch %d, want the existing %d", again.WatchID, first.WatchID)
	}

	var channel followResult
	if err := json.Unmarshal(l.do(opFollow, map[string]string{"channel_id": testChannel}).Body, &channel); err != nil {
		t.Fatal(err)
	}
	var stopped unfollowResult
	if err := json.Unmarshal(l.do(opUnfollow, map[string]int64{"watch_id": first.WatchID}).Body, &stopped); err != nil {
		t.Fatal(err)
	}
	if !stopped.Stopped || stopped.Remaining != 1 {
		t.Fatalf("unfollow = %+v, want stopped with the channel watch remaining", stopped)
	}
	if err := json.Unmarshal(l.do(opUnfollow, map[string]int64{"watch_id": channel.WatchID}).Body, &stopped); err != nil {
		t.Fatal(err)
	}
	if !stopped.Stopped || stopped.Remaining != 0 {
		t.Fatalf("last unfollow = %+v, want none remaining", stopped)
	}
}

func TestFollowRejectsUnknownOwnerAndUnreadableChannel(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	raw, _ := json.Marshal(map[string]string{"channel_id": testChannel, "owner": "agent-404"})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opFollow, raw); err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("unknown owner: %v", err)
	}
	l.slack.fail(historyOf("G9"), slack.SlackErrorResponse{Err: "not_in_channel"})
	raw, _ = json.Marshal(map[string]string{"channel_id": "G9"})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opFollow, raw); err == nil || !strings.Contains(err.Error(), mcpHistory) {
		t.Fatalf("unreadable channel: %v", err)
	}
}

func TestResumedWindowWatchesAndAwaitsUnderTheSubjectItsAgentStartedIn(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: testThread, Text: "parent"}})
	resumed := l.deps
	resumed.ClaudePID = func() int { return testPID + 2 }
	call := func(d cmd.Deps, session string, op daemon.Op, body any) (daemon.Reply, error) {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return invoke(l.ctx, d, session, "/any", op, raw)
	}
	must := func(reply daemon.Reply, err error) daemon.Reply {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	mcp := must(call(resumed, "pre-resume-session", opSessionStart, nil))
	must(call(resumed, testSession, opSessionStart, nil))
	must(call(resumed, testSession, daemon.OpAgentStart, map[string]string{"agent_id": "agent-r", "agent_type": waiterAgentType, "session_id": testSession}))
	if mcp.SubjectID == l.subID {
		t.Fatalf("the MCP server's session shares subject %s, want the two subjects a resume leaves", l.subID)
	}

	var watched followResult
	if err := json.Unmarshal(must(call(resumed, "pre-resume-session", opFollow, map[string]string{"channel_id": testChannel, "thread_ts": testThread, "owner": "agent-r"})).Body, &watched); err != nil {
		t.Fatal(err)
	}
	var subjectID string
	if err := l.srv.DB().QueryRowContext(l.ctx, `SELECT subject_id FROM watches WHERE id = ?`, watched.WatchID).Scan(&subjectID); err != nil {
		t.Fatal(err)
	}
	if subjectID != l.subID {
		t.Fatalf("watch subject = %s, want %s, where agent-r started", subjectID, l.subID)
	}
	if got := must(call(resumed, "pre-resume-session", opAgentSubject, map[string]string{"owner": "agent-r"})); got.SubjectID != l.subID || got.HTTPPort != l.port {
		t.Fatalf("await resolves subject %s port %d, want %s port %d", got.SubjectID, got.HTTPPort, l.subID, l.port)
	}

	stranger := l.deps
	stranger.ClaudePID = func() int { return testPID + 3 }
	if _, err := call(stranger, "other-session", opFollow, map[string]string{"channel_id": testChannel, "owner": "agent-r"}); err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("another window's follow for agent-r: %v, want unknown agent", err)
	}

	var stopped unfollowResult
	if err := json.Unmarshal(must(call(resumed, "pre-resume-session", opUnfollow, map[string]int64{"watch_id": watched.WatchID})).Body, &stopped); err != nil {
		t.Fatal(err)
	}
	if !stopped.Stopped {
		t.Fatalf("unfollow = %+v, want agent-r's watch stopped", stopped)
	}
}

func TestReplyBroadcastsToTheChannelOnlyWhenAsked(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]string{"channel_id": testChannel, "text": "PR is up"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	l.post(opReply, map[string]any{"channel_id": testChannel, "thread_ts": sent.TS, "text": "quiet"})
	l.post(opReply, map[string]any{"channel_id": testChannel, "thread_ts": sent.TS, "text": "landed", "broadcast": true})
	if posts := l.slack.sent(); posts[1].broadcast || !posts[2].broadcast || posts[2].threadTS != sent.TS {
		t.Fatalf("posts = %+v, want only the second reply broadcast", posts)
	}

	raw, _ := json.Marshal(map[string]any{"channel_id": testChannel, "text": "top level", "broadcast": true})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opSend, raw); err == nil || !strings.Contains(err.Error(), "broadcast needs thread_ts") {
		t.Fatalf("broadcast without a thread: %v", err)
	}
}

func TestPostAndReactJoinAPublicChannelAndRetryOnce(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	l.slack.fail("chat.postMessage", slack.SlackErrorResponse{Err: "not_in_channel"})
	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]string{"channel_id": testChannel, "text": "PR is up"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	l.slack.fail("reactions.add", slack.SlackErrorResponse{Err: "not_in_channel"})
	l.do(opReact, map[string]string{"channel_id": testChannel, "ts": sent.TS, "name": "eyes"})
	if joined := l.slack.joinedChannels(); len(joined) != 2 || joined[0] != testChannel || joined[1] != testChannel {
		t.Fatalf("joined = %v, want one join of %s per failed call", joined, testChannel)
	}
	if posts, reacts := l.slack.callCount("chat.postMessage"), l.slack.callCount("reactions.add"); posts != 2 || reacts != 2 {
		t.Fatalf("chat.postMessage %d, reactions.add %d, want each retried once", posts, reacts)
	}
}

func TestPostToAHiddenPrivateChannelRefusesWithoutAnAsk(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	l.slack.fail("chat.postMessage", slack.SlackErrorResponse{Err: "channel_not_found"})
	l.slack.fail("conversations.join", slack.SlackErrorResponse{Err: "channel_not_found"})
	body := l.hooked(opSend, map[string]string{"channel_id": testChannel, "text": "PR is up"})
	raw, _ := json.Marshal(body)
	_, err := invoke(l.ctx, l.deps, testSession, "/any", opSend, raw)
	if err == nil || !strings.Contains(err.Error(), "<#"+testChannel+"> is private") || !strings.Contains(err.Error(), errNoUserCredential.Error()) ||
		strings.Contains(err.Error(), mcpSend) || strings.Contains(err.Error(), "/invite") {
		t.Fatalf("send to a hidden private channel: %v", err)
	}
	if n := len(l.slack.sentTo(testChannel)); n != 0 {
		t.Fatalf("posts to %s = %d, want none without a join", testChannel, n)
	}

	l.slack.fail("reactions.add", slack.SlackErrorResponse{Err: "not_in_channel"})
	l.slack.fail("conversations.join", slack.SlackErrorResponse{Err: "method_not_supported_for_channel_type"})
	l.refused(opReact, map[string]string{"channel_id": testChannel, "ts": "1800000000.000100", "name": "eyes"}, "add it with channel-join")
	if asks := l.slack.sentTo("D" + testUser); len(asks) != 0 {
		t.Fatalf("DM posts = %+v, want no /invite ask", asks)
	}

	var res postResult
	if err := json.Unmarshal(l.do(opSend, body).Body, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Posted || len(l.slack.sentTo(testChannel)) != 1 {
		t.Fatalf("send after the invite = %+v, want it posted under the handed-back ticket", res)
	}
}

func TestPostToAPrivateChannelInvitesTheBotAsTheUser(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	user := l.user.as(testUser)

	l.slack.fail("chat.postMessage", slack.SlackErrorResponse{Err: "channel_not_found"})
	l.slack.fail("conversations.join", slack.SlackErrorResponse{Err: "method_not_supported_for_channel_type"})
	var res postResult
	if err := json.Unmarshal(l.post(opSend, map[string]string{"channel_id": testChannel, "text": "PR is up"}).Body, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Posted || len(l.slack.sentTo(testChannel)) != 1 {
		t.Fatalf("send = %+v, want it posted after the invite", res)
	}
	if _, _, invites, _ := user.channelChanges(); !slices.Equal(invites[testChannel], []string{"UBOT"}) {
		t.Fatalf("invites as the user = %v, want the bot invited to %s", invites, testChannel)
	}
	if asks := l.slack.sentTo("D" + testUser); len(asks) != 0 {
		t.Fatalf("DM posts = %+v, want no ask once the bot invited itself", asks)
	}
}

func TestSelfInviteNeedsTheLoggedInUsersCredential(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	user := l.user.as("UOTHER")

	l.slack.fail("chat.postMessage", slack.SlackErrorResponse{Err: "channel_not_found"})
	l.slack.fail("conversations.join", slack.SlackErrorResponse{Err: "channel_not_found"})
	l.refused(opSend, l.hooked(opSend, map[string]string{"channel_id": testChannel, "text": "PR is up"}),
		"inviting it as the user failed (the Slack MCP credential is UOTHER in T1, not "+testUser+" in T1)")
	if n := user.callCount("conversations.invite"); n != 0 {
		t.Fatalf("conversations.invite calls = %d, want none with another user's credential", n)
	}
	if asks := l.slack.sentTo("D" + testUser); len(asks) != 0 {
		t.Fatalf("DM posts = %+v, want no /invite ask", asks)
	}
}

func TestExpiredUserSessionIsReMintedFromChromeBeforeTheInvite(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	stored := l.user.as(testUser)
	stored.fail("auth.test", slack.SlackErrorResponse{Err: "invalid_auth"})
	chrome := l.user.inChrome(testUser)

	l.slack.fail("conversations.join", slack.SlackErrorResponse{Err: "method_not_supported_for_channel_type"})
	joined := decodeChange(t, l.post(opChannelJoin, map[string]any{"channel_id": testChannel}))
	if _, _, invites, _ := chrome.channelChanges(); joined.ChannelID != testChannel || !slices.Equal(invites[testChannel], []string{"UBOT"}) {
		t.Fatalf("join = %+v, invites through Chrome's session %v, want the bot invited to %s", joined, invites, testChannel)
	}
	if n := stored.callCount("conversations.invite"); n != 0 {
		t.Fatalf("conversations.invite calls with the expired session = %d, want none", n)
	}
	if saved := l.user.savedCredentials(); !slices.Equal(saved, []userCredential{{Token: chromeUserToken, Cookie: chromeCookie}}) {
		t.Fatalf("saved = %+v, want Chrome's session stored once", saved)
	}
}

func TestExpiredUserSessionWithChromeSignedOutRefusesInOneLine(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	user := l.user.as(testUser)
	user.fail("auth.test", slack.SlackErrorResponse{Err: "invalid_auth"})

	l.slack.fail("conversations.join", slack.SlackErrorResponse{Err: "method_not_supported_for_channel_type"})
	l.refused(opChannelJoin, l.hooked(opChannelJoin, map[string]any{"channel_id": testChannel}),
		"inviting it as the user failed ("+errChromeSignedOut.Error()+")")
	if n := user.callCount("conversations.invite"); n != 0 {
		t.Fatalf("conversations.invite calls = %d, want none with an expired credential", n)
	}
	if saved := l.user.savedCredentials(); len(saved) != 0 {
		t.Fatalf("saved = %+v, want nothing stored", saved)
	}
	if asks := l.slack.sentTo("D" + testUser); len(asks) != 0 {
		t.Fatalf("DM posts = %+v, want the refusal to send nothing", asks)
	}
}

func TestChromeSessionIsStoredOnlyOnceItPassesAuthTest(t *testing.T) {
	for name, tc := range map[string]struct {
		chromeUser string
		chromeErr  string
		want       string
	}{
		"signed out server-side": {chromeUser: testUser, chromeErr: "invalid_auth", want: errChromeSignedOut.Error()},
		"another user":           {chromeUser: "UOTHER", want: "the Slack MCP credential is UOTHER in T1, not " + testUser + " in T1"},
	} {
		t.Run(name, func(t *testing.T) {
			l := startDaemon(t)
			loggedIn(t, l)
			l.user.as(testUser).fail("auth.test", slack.SlackErrorResponse{Err: "token_revoked"})
			chrome := l.user.inChrome(tc.chromeUser)
			if tc.chromeErr != "" {
				chrome.fail("auth.test", slack.SlackErrorResponse{Err: tc.chromeErr})
			}

			l.slack.fail("conversations.join", slack.SlackErrorResponse{Err: "method_not_supported_for_channel_type"})
			l.refused(opChannelJoin, l.hooked(opChannelJoin, map[string]any{"channel_id": testChannel}), tc.want)
			if n := chrome.callCount("conversations.invite"); n != 0 {
				t.Fatalf("conversations.invite calls = %d, want none", n)
			}
			if saved := l.user.savedCredentials(); len(saved) != 0 {
				t.Fatalf("saved = %+v, want nothing stored", saved)
			}
		})
	}
}

func TestUserSessionFailureOtherThanExpiryKeepsTheStoredSession(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.user.as(testUser).fail("auth.test", slack.SlackErrorResponse{Err: "account_inactive"})
	l.user.inChrome(testUser)

	l.slack.fail("conversations.join", slack.SlackErrorResponse{Err: "method_not_supported_for_channel_type"})
	l.refused(opChannelJoin, l.hooked(opChannelJoin, map[string]any{"channel_id": testChannel}),
		"the user's Slack session credential failed auth.test: account_inactive")
	if saved := l.user.savedCredentials(); len(saved) != 0 {
		t.Fatalf("saved = %+v, want the stored session kept", saved)
	}
}

func TestUnreactJoinsAPublicChannelAndToleratesAMissingReaction(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	l.slack.fail("reactions.remove", slack.SlackErrorResponse{Err: "not_in_channel"})
	l.do(opUnreact, map[string]string{"channel_id": testChannel, "ts": "1800000000.000100", "name": ":eyes:"})
	if joined := l.slack.joinedChannels(); len(joined) != 1 || joined[0] != testChannel {
		t.Fatalf("joined = %v, want one join of %s", joined, testChannel)
	}
	l.slack.fail("reactions.remove", slack.SlackErrorResponse{Err: "no_reaction"})
	l.do(opUnreact, map[string]string{"channel_id": testChannel, "ts": "1800000000.000100", "name": "eyes"})
	if n := l.slack.callCount("reactions.remove"); n != 3 {
		t.Fatalf("reactions.remove calls = %d, want a retry after the join and one more call", n)
	}
}

func TestEditReplacesTheTextOfABotPost(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	text := "Fixed in <https://github.com/acme/app/pull/28352|#28352>"
	l.post(opEdit, map[string]string{"channel_id": testChannel, "ts": "1800000000.000100", "text": text})
	edits := l.slack.edited()
	if len(edits) != 1 || edits[0].channelID != testChannel || edits[0].ts != "1800000000.000100" || edits[0].text != text {
		t.Fatalf("edits = %+v, want one chat.update of %s/1800000000.000100 with the new text", edits, testChannel)
	}
}

func TestSendAndEditOfTextPastTheTextLimitStayOneMessage(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	text := strings.Repeat("• *What's risky:* a batch line with a <https://github.com/acme/app/pull/42829|PR link>\n", 60)
	l.post(opSend, map[string]any{"channel_id": testChannel, "text": text})
	posts := l.slack.sent()
	if len(posts) != 1 || len([]rune(posts[0].text)) > maxTextChars {
		t.Fatalf("posts = %d, text = %d runes, want one post whose text Slack will not split", len(posts), len([]rune(posts[0].text)))
	}
	var blocks slack.Blocks
	if err := json.Unmarshal([]byte(posts[0].blocks), &blocks); err != nil {
		t.Fatal(err)
	}
	if got := messageText(slack.Message{Msg: slack.Msg{Text: posts[0].text, Blocks: blocks}}); got != text {
		t.Fatalf("read back %d runes, want the whole %d-rune text", len([]rune(got)), len([]rune(text)))
	}

	l.post(opEdit, map[string]any{"channel_id": testChannel, "ts": posts[0].ts, "text": text + "edited\n"})
	if edits := l.slack.edited(); len(edits) != 1 || len([]rune(edits[0].text)) > maxTextChars || edits[0].blocks == "[]" {
		t.Fatalf("edits = %+v, want one chat.update under the text limit carrying the text as blocks", edits)
	}
}

func TestEditOfAMessageTheBotDidNotPostNamesNoMCPTool(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	l.slack.fail("chat.update", slack.SlackErrorResponse{Err: "cant_update_message"})
	raw, _ := json.Marshal(l.hooked(opEdit, map[string]string{"channel_id": testChannel, "ts": "1800000000.000100", "text": "fixed"}))
	_, err := invoke(l.ctx, l.deps, testSession, "/any", opEdit, raw)
	if err == nil || !strings.Contains(err.Error(), "cant_update_message") || strings.Contains(err.Error(), "mcp__slack__") {
		t.Fatalf("edit of a foreign message: %v", err)
	}
}

func TestDeleteRemovesABotPost(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	l.post(opDelete, map[string]string{"channel_id": testChannel, "ts": "1800000000.000100"})
	deletes := l.slack.deleted()
	if len(deletes) != 1 || deletes[0].channelID != testChannel || deletes[0].ts != "1800000000.000100" {
		t.Fatalf("deletes = %+v, want one chat.delete of %s/1800000000.000100", deletes, testChannel)
	}
}

func TestDeleteOfAMessageTheBotDidNotPostNamesNoMCPTool(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	l.slack.fail("chat.delete", slack.SlackErrorResponse{Err: "cant_delete_message"})
	raw, _ := json.Marshal(l.hooked(opDelete, map[string]string{"channel_id": testChannel, "ts": "1800000000.000100"}))
	_, err := invoke(l.ctx, l.deps, testSession, "/any", opDelete, raw)
	if err == nil || !strings.Contains(err.Error(), "cant_delete_message") || strings.Contains(err.Error(), "mcp__slack__") {
		t.Fatalf("delete of a foreign message: %v", err)
	}
}

func TestReactInAPrivateChannelNamesChannelJoinNotTheMCP(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	l.slack.fail("reactions.add", slack.SlackErrorResponse{Err: "not_in_channel"})
	l.slack.fail("conversations.join", slack.SlackErrorResponse{Err: "method_not_supported_for_channel_type"})
	user := l.user.as(testUser)
	raw, _ := json.Marshal(map[string]string{"channel_id": testChannel, "ts": "1800000000.000100", "name": "eyes"})
	_, err := invoke(l.ctx, l.deps, testSession, "/any", opReact, raw)
	if err == nil || !strings.Contains(err.Error(), "channel-join") || strings.Contains(err.Error(), mcpReaction) {
		t.Fatalf("react in a private channel: %v", err)
	}
	if n := user.callCount("conversations.invite"); n != 0 {
		t.Fatalf("conversations.invite calls = %d, want a reaction to never invite the bot", n)
	}
	if n := l.slack.callCount("reactions.add"); n != 1 {
		t.Fatalf("reactions.add calls = %d, want no retry without a join", n)
	}
}

func TestDMStatusOpensOneThreadPerSession(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	var first, second map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]string{"text": "starting"}).Body, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(l.do(opDMStatus, map[string]string{"text": "halfway"}).Body, &second); err != nil {
		t.Fatal(err)
	}
	if first["thread_ts"] == "" || second["thread_ts"] != first["thread_ts"] || first["channel_id"] != "D"+testUser {
		t.Fatalf("dm-status threads = %v then %v, want one DM thread", first, second)
	}
	if got := l.slack.sent()[1]; got.threadTS != first["thread_ts"] {
		t.Fatalf("second status = %+v, want it in the session thread", got)
	}
}

func TestDMStatusBindsToAGivenDMMessage(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	var opened, bound, later map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]string{"text": "starting"}).Body, &opened); err != nil {
		t.Fatal(err)
	}
	const dmTS = "1800000050.000000"
	if err := json.Unmarshal(l.do(opDMStatus, map[string]string{"text": "on it", "thread_ts": dmTS}).Body, &bound); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(l.do(opDMStatus, map[string]string{"text": "done"}).Body, &later); err != nil {
		t.Fatal(err)
	}
	if bound["thread_ts"] != dmTS || later["thread_ts"] != dmTS {
		t.Fatalf("dm-status threads = %v then %v, want both on the shortcut DM", bound, later)
	}
	sent := l.slack.sent()
	if sent[1].threadTS != dmTS || sent[2].threadTS != dmTS {
		t.Fatalf("status posts = %+v, want them under %s", sent, dmTS)
	}
	var active []string
	rows, err := l.srv.DB().QueryContext(l.ctx, `SELECT thread_ts FROM watches WHERE kind = 'dm' AND status = 'active'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var ts string
		if err := rows.Scan(&ts); err != nil {
			t.Fatal(err)
		}
		active = append(active, ts)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0] != dmTS || opened["thread_ts"] == dmTS {
		t.Fatalf("active DM watches = %v, want only the shortcut DM thread", active)
	}
	raw := mustJSON(map[string]string{"text": "x", "thread_ts": "not-a-ts"})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, raw); err == nil || !strings.Contains(err.Error(), "not a Slack ts") {
		t.Fatalf("bad thread_ts: %v", err)
	}
}

func TestWritesStartAnUnregisteredWindow(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	stranger := l.deps
	stranger.ClaudePID = func() int { return testPID + 1 }
	raw, _ := json.Marshal(map[string]string{"text": "hi"})
	if _, err := invoke(l.ctx, stranger, "other-session", "/any", opDMStatus, raw); err != nil {
		t.Fatalf("dm-status for a window that never ran session-start: %v", err)
	}
	again, err := invoke(l.ctx, stranger, "other-session", "/any", opSessionStart, nil)
	if err != nil {
		t.Fatal(err)
	}
	var subjects int
	if err := l.srv.DB().QueryRowContext(l.ctx, `SELECT COUNT(*) FROM subjects WHERE id = ?`, again.SubjectID).Scan(&subjects); err != nil {
		t.Fatal(err)
	}
	if again.SubjectID == l.subID || subjects != 1 {
		t.Fatalf("session-start after an auto-start returned subject %q (%d rows), want the auto-started one", again.SubjectID, subjects)
	}
}

func TestOwnerTargetResolvesToTheBotIM(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	im := "D" + testUser

	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]string{"channel_id": testUser, "text": "PR is up"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ChannelID != im || l.slack.sent()[0].channelID != im {
		t.Fatalf("send to %s = %+v posted to %q, want the bot IM %s", testUser, sent, l.slack.sent()[0].channelID, im)
	}
	l.post(opReply, map[string]string{"channel_id": testUser, "thread_ts": sent.TS, "text": "CI is green"})
	if got := l.slack.sent()[1]; got.channelID != im || got.threadTS != sent.TS {
		t.Fatalf("reply = %+v, want it in the IM thread", got)
	}
	l.do(opReact, map[string]string{"channel_id": testUser, "ts": sent.TS, "name": "eyes"})
	var thread struct {
		ChannelID string          `json:"channel_id"`
		Messages  []threadMessage `json:"messages"`
	}
	if err := json.Unmarshal(l.do(opThread, map[string]string{"channel_id": testUser, "thread_ts": sent.TS}).Body, &thread); err != nil {
		t.Fatal(err)
	}
	if thread.ChannelID != im || len(thread.Messages) != 2 {
		t.Fatalf("thread = %+v, want the IM thread with both posts", thread)
	}
	if n := l.slack.callCount("conversations.open"); n != 5 {
		t.Fatalf("conversations.open calls = %d, want login plus one per user-targeted op", n)
	}
}

func TestPersonTargetsResolveToAGroupDMWithTheOwner(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	mpim := "GUOTHER" + testUser

	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]string{"channel_id": "UOTHER", "text": "PR is up"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ChannelID != mpim || l.slack.sent()[0].channelID != mpim {
		t.Fatalf("send to UOTHER = %+v posted to %q, want the group DM %s", sent, l.slack.sent()[0].channelID, mpim)
	}
	l.post(opReply, map[string]string{"channel_id": "UOTHER", "thread_ts": sent.TS, "text": "CI is green"})
	if got := l.slack.sent()[1]; got.channelID != mpim || got.threadTS != sent.TS {
		t.Fatalf("reply = %+v, want it in the group DM thread", got)
	}
	l.do(opReact, map[string]string{"channel_id": "UOTHER", "ts": sent.TS, "name": "eyes"})
	var thread struct {
		ChannelID string          `json:"channel_id"`
		Messages  []threadMessage `json:"messages"`
	}
	if err := json.Unmarshal(l.do(opThread, map[string]string{"channel_id": "UOTHER", "thread_ts": sent.TS}).Body, &thread); err != nil {
		t.Fatal(err)
	}
	if thread.ChannelID != mpim || len(thread.Messages) != 2 {
		t.Fatalf("thread = %+v, want the group DM thread with both posts", thread)
	}
}

func TestDMPeerNamesThePersonADMReaches(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.setUser(&slack.User{ID: "UOTHER", Profile: slack.UserProfile{DisplayName: "anubhav"}})
	mpim := "GUOTHER" + testUser
	l.post(opSend, map[string]string{"channel_id": "UOTHER", "text": "PR is up"})

	owner := []string{"Yasyf Mohamedali", "yasyf", "ym"}
	for channel, want := range map[string]dmPeerResult{
		mpim:           {ChannelID: mpim, Peer: "UOTHER", PeerName: "anubhav", PeerNames: []string{"anubhav"}},
		"UOTHER":       {ChannelID: "UOTHER", Peer: "UOTHER", PeerName: "anubhav", PeerNames: []string{"anubhav"}},
		"D" + testUser: {ChannelID: "D" + testUser, Peer: testUser, PeerName: "yasyf", PeerNames: owner},
		testChannel:    {ChannelID: testChannel},
	} {
		var got dmPeerResult
		if err := json.Unmarshal(l.do(opDMPeer, map[string]string{"channel_id": channel}).Body, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("dm-peer %s = %+v, want %+v", channel, got, want)
		}
	}

	l.slack.fail("conversations.info", slack.SlackErrorResponse{Err: "channel_not_found"})
	var private dmPeerResult
	if err := json.Unmarshal(l.do(opDMPeer, map[string]string{"channel_id": "CPRIVATE"}).Body, &private); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(private, dmPeerResult{ChannelID: "CPRIVATE"}) {
		t.Fatalf("dm-peer of a channel the bot cannot see = %+v, want no peer", private)
	}
}

func TestADMWriteSpendsAgainstThePersonItReaches(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	mpim := "GUOTHER" + testUser
	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]string{"channel_id": "UOTHER", "text": "PR is up"}).Body, &sent); err != nil {
		t.Fatal(err)
	}

	body := map[string]any{"channel_id": mpim, "thread_ts": sent.TS, "text": "CI is green"}
	ticket := l.tickets.mint(fakeTicket{tree: testTree, place: map[string]string{"channel": "UOTHER", "thread": ""}, reach: reachPlace, rule: "asked-owner"})
	l.do(opReply, l.spending(ticket, body))
	if got := l.tickets.lastCall(); !slices.Equal(got[4:8], []string{"--place", "channel=UOTHER", "--place", "thread="}) {
		t.Fatalf("capt-hook argv = %q, want the spend scoped to UOTHER with no thread", got)
	}
	if got := l.slack.sent()[1]; got.channelID != mpim || got.threadTS != sent.TS {
		t.Fatalf("reply = %+v, want it in the group DM thread", got)
	}
}

func TestFollowWatchesDMsWithoutJoining(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	var dm followResult
	if err := json.Unmarshal(l.do(opFollow, map[string]string{"channel_id": "D" + testUser}).Body, &dm); err != nil {
		t.Fatal(err)
	}
	var user followResult
	if err := json.Unmarshal(l.do(opFollow, map[string]string{"channel_id": "UOTHER"}).Body, &user); err != nil {
		t.Fatal(err)
	}
	if dm.Joined || user.Joined || l.slack.callCount("conversations.join") != 0 {
		t.Fatalf("follow joined a DM: dm %+v user %+v joins %d", dm, user, l.slack.callCount("conversations.join"))
	}
	if dm.ChannelID != "D"+testUser || user.ChannelID != "GUOTHER"+testUser {
		t.Fatalf("follow channels = %q and %q, want the IM and group DM ids", dm.ChannelID, user.ChannelID)
	}
	var watched string
	if err := l.srv.DB().QueryRowContext(l.ctx, `SELECT channel_id FROM watches WHERE id = ?`, user.WatchID).Scan(&watched); err != nil {
		t.Fatal(err)
	}
	if watched != "GUOTHER"+testUser {
		t.Fatalf("watch on UOTHER stored channel %q, want the group DM", watched)
	}
}

func TestUpdateMessageDropsLinkPreviews(t *testing.T) {
	t.Parallel()
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		form = r.PostForm
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1800000000.000100","text":"same text"}`))
	}))
	defer srv.Close()
	if err := updateMessage(context.Background(), newSlackClient("xoxb-test", srv.URL+"/", srv.Client()), "C1", "1800000000.000100", "same text", nil); err != nil {
		t.Fatal(err)
	}
	if form.Get("text") != "same text" || form.Get("blocks") != "[]" || form.Get("attachments") != "[]" || form.Get("unfurl_links") != "false" {
		t.Fatalf("chat.update form = %v, want the text with blocks, attachments and link unfurls cleared", form)
	}
}

func TestMessageTextAppendsAttachmentsAndBlocks(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  slack.Msg
		want string
	}{
		{"text only", slack.Msg{Text: "hello"}, "hello"},
		{
			"datadog alert",
			slack.Msg{Attachments: []slack.Attachment{{
				Title: "[Triggered] SandSQL replica lane stale", Text: "lane out of routing table",
				TitleLink: "https://app.datadoghq.com/monitors/315028131",
			}}},
			"[Triggered] SandSQL replica lane stale\nlane out of routing table\nhttps://app.datadoghq.com/monitors/315028131",
		},
		{"fallback only", slack.Msg{Text: "see below", Attachments: []slack.Attachment{{Fallback: "Sentry: TypeError"}}}, "see below\nSentry: TypeError"},
		{
			"long text as sections",
			slack.Msg{Text: "first half…", Blocks: slack.Blocks{BlockSet: []slack.Block{
				slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, "first half, ", false, false), nil, nil),
				slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, "second half", false, false), nil, nil),
			}}},
			"first half, second half",
		},
		{
			"card whose text ends in an ellipsis",
			slack.Msg{Text: "Deploying…", Blocks: slack.Blocks{BlockSet: []slack.Block{
				slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, "api is rolling out", false, false), nil, nil),
			}}},
			"Deploying…\napi is rolling out",
		},
		{
			"sentry alert in blocks",
			slack.Msg{Text: "[api] Error: Not authorized", Blocks: slack.Blocks{BlockSet: []slack.Block{
				slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, "<https://acme.sentry.io/issues/7815544165/|*Error*>", false, false), nil, nil),
				slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, "Short ID: API-1NH9", false, false)),
				slack.NewActionBlock("", slack.NewButtonBlockElement("resolve", "resolve", slack.NewTextBlockObject(slack.PlainTextType, "Resolve", false, false)),
					slack.NewButtonBlockElement("", "", slack.NewTextBlockObject(slack.PlainTextType, "View", false, false)).WithURL("https://acme.sentry.io/issues/7815544165/")),
			}}},
			"[api] Error: Not authorized\n<https://acme.sentry.io/issues/7815544165/|*Error*>\nShort ID: API-1NH9",
		},
		{
			"attachment fields and blocks",
			slack.Msg{Attachments: []slack.Attachment{{
				Fallback: "[Triggered] lane stale",
				Fields:   []slack.AttachmentField{{Title: "Env", Value: "prod"}},
				Blocks: slack.Blocks{BlockSet: []slack.Block{
					slack.NewHeaderBlock(slack.NewTextBlockObject(slack.PlainTextType, "lane stale", false, false)),
					slack.NewSectionBlock(nil, []*slack.TextBlockObject{slack.NewTextBlockObject(slack.MarkdownType, "*Team* aig", false, false)},
						slack.NewAccessory(slack.NewButtonBlockElement("", "", slack.NewTextBlockObject(slack.PlainTextType, "Open", false, false)).WithURL("https://app.datadoghq.com/monitors/1"))),
				}},
			}}},
			"[Triggered] lane stale\nEnv: prod\n*Team* aig\nhttps://app.datadoghq.com/monitors/1",
		},
	} {
		if got := messageText(slack.Message{Msg: tc.msg}); got != tc.want {
			t.Errorf("%s: messageText = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSubscriptionsReportTheChannelRouteAndProbeIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	follow := func(body map[string]string) followResult {
		var res followResult
		if err := json.Unmarshal(l.do(opFollow, body).Body, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	if got := follow(map[string]string{"channel_id": testChannel}).Channel; got != daemon.ChannelInactive {
		t.Fatalf("follow with no channel stream = %q, want inactive", got)
	}

	ctx, cancel := context.WithTimeout(l.ctx, 20*time.Second)
	defer cancel()
	probed := make(chan struct{})
	go func() {
		_ = consume.ConsumeEvents(ctx, consume.StreamSource{
			Port: l.port, SubjectID: l.subID, Consumer: channelConsumer, ClaudePID: testPID,
			ExcludeOrigin: event.OriginAgent, Paths: appPaths(),
		}, func(_ int64, data string) (bool, error) {
			if eventType(data) != daemon.ProbeEventType {
				return false, nil
			}
			close(probed)
			return true, nil
		})
	}()
	for follow(map[string]string{"channel_id": testChannel}).Channel != daemon.ChannelPending {
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-probed:
	case <-ctx.Done():
		t.Fatal("a pending follow never probed the attached channel")
	}

	l.do(daemon.OpChannelAck, nil)
	l.startAgent("agent-owner")
	if got := follow(map[string]string{"channel_id": testChannel, "owner": "agent-owner"}).Channel; got != "" {
		t.Fatalf("owner follow reported channel %q, want none", got)
	}
	var quiet postResult
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "fyi", "no_watch": true}).Body, &quiet); err != nil {
		t.Fatal(err)
	}
	if quiet.Channel != "" {
		t.Fatalf("no_watch post reported channel %q, want none", quiet.Channel)
	}
}

func TestPostsAndEditsCarryBlocksWithTheirText(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	card := []map[string]any{
		{"type": "header", "text": map[string]any{"type": "plain_text", "text": "Deploy done"}},
		{"type": "section", "block_id": "status", "text": map[string]any{"type": "mrkdwn", "text": "api is on the new build"}},
	}
	l.post(opSend, map[string]any{"channel_id": testChannel, "text": "Deploy done", "blocks": card})
	want, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	if posts := l.slack.sent(); len(posts) != 1 || posts[0].text != "Deploy done" || posts[0].blocks != string(want) {
		t.Fatalf("posts = %+v, want the text and the blocks verbatim", posts)
	}

	l.post(opEdit, map[string]any{"channel_id": testChannel, "ts": "1800000000.000100", "text": "Deploy done", "blocks": card})
	if edits := l.slack.edited(); len(edits) != 1 || edits[0].blocks != string(want) {
		t.Fatalf("edits = %+v, want the blocks verbatim", edits)
	}

	l.refused(opSend, l.hooked(opSend, map[string]any{"channel_id": testChannel, "files": []string{"/tmp/x.png"}, "blocks": card}), "blocks need text")
	l.refused(opSend, l.hooked(opSend, map[string]any{"channel_id": testChannel, "text": "x", "blocks": []map[string]any{{"text": "untyped"}}}), "not a Block Kit block")
}
