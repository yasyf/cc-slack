package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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

func TestAPostDrawsOnlyAnOpenCardItLinks(t *testing.T) {
	message := "https://example.slack.com/archives/C1/p1793818357021259"
	for _, tc := range []struct {
		name, want string
		text       func(card postedCard) string
	}{
		{"an open card under the words that name it", "true", func(c postedCard) string { return "card: <" + c.url() + "|the merge card>" }},
		{"his own message", "false", func(postedCard) string { return "your order: <" + message + "|your words>" }},
		{"a message in a thread", "false", func(postedCard) string { return "see " + message + "?thread_ts=1793818357.021259" }},
		{"any other link", "false", func(postedCard) string { return "see <https://example.com/a|a>" }},
		{"no link", "false", func(postedCard) string { return "plain words" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := startDaemon(t)
			loggedIn(t, l)
			card := postWaiting(t, l, mergeCard())
			l.post(opSend, map[string]string{"channel_id": testChannel, "text": tc.text(card)})
			if posts := l.slack.sentTo(testChannel); len(posts) != 1 || posts[0].unfurlLinks != tc.want {
				t.Fatalf("posts = %+v, want one post with unfurl_links=%s", posts, tc.want)
			}
		})
	}
}

func TestAPostInAThreadWritesALinkToTheMessageItIsUnderAsItsWords(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	var parent, other postResult
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "Which store keeps the tally?"}).Body, &parent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "Which cache holds the runs?"}).Body, &other); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, text, want, unfurl string
	}{
		{"its link under words", "The design job behind <" + parent.Permalink + "|the tally question> has your question", "The design job behind the tally question has your question", "false"},
		{"its bare link", "Answered in " + parent.Permalink + "?thread_ts=" + parent.TS, "Answered in this thread", "false"},
		{"another message's link", "Asked beside <" + other.Permalink + "|the cache question>", "", "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l.post(opReply, map[string]any{"channel_id": testChannel, "thread_ts": parent.TS, "text": tc.text})
			got := lastPost(l)
			if tc.want != "" && got.text != tc.want || tc.want == "" && !strings.Contains(got.text, other.Permalink) || got.unfurlLinks != tc.unfurl {
				t.Fatalf("posted %q with unfurl_links=%q, want %q with unfurl_links=%s", got.text, got.unfurlLinks, cmp.Or(tc.want, "the other message's link"), tc.unfurl)
			}
		})
	}
}

func TestACardDrawsOncePerThreadByALinkSlackHasNotShownThisHour(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	card := postWaiting(t, l, mergeCard())
	link := "<" + card.url() + "|the merge card>"
	var first postResult
	if err := json.Unmarshal(l.post(opSend, map[string]string{"channel_id": testChannel, "text": "first: " + link}).Body, &first); err != nil {
		t.Fatal(err)
	}
	l.post(opReply, map[string]string{"channel_id": testChannel, "thread_ts": first.TS, "text": "again: " + link})
	l.post(opSend, map[string]string{"channel_id": testChannel, "text": "elsewhere: " + link})
	posts := l.slack.sentTo(testChannel)
	if len(posts) != 3 {
		t.Fatalf("posts = %+v, want three", posts)
	}
	drew := func(p fakePost) bool { return p.unfurlLinks == "true" && strings.Contains(p.text, freshLinkParam+"=") }
	if !drew(posts[0]) || drew(posts[1]) || posts[1].unfurlLinks == "true" || !drew(posts[2]) {
		t.Fatalf("posts = %+v, want the card drawn by the first post, plain in its thread after, and drawn again in the next thread", posts)
	}
	if fresh := messageLink.FindString(posts[0].text); fresh == messageLink.FindString(posts[2].text) {
		t.Fatalf("both threads link the card as %q, want a link per post, since Slack draws no link it showed in the conversation within the hour", fresh)
	}
}

func TestOneThreadDrawsEachCardOnceWhateverItsPostsLink(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	titles := []string{"Roll the executor back?", "Raise the executor's memory?", "Page the on-call?"}
	links := make([]string, 0, len(titles))
	for n, title := range titles {
		card := postWaiting(t, l, statusCard{Status: "needs_you", Title: title, Body: "The executor runs out of memory.", Options: []string{"Yes", "No"}, Recommended: "Yes", Waits: fmt.Sprintf("oom:%d", n)})
		links = append(links, "<"+card.url()+fmt.Sprintf("|card %d>", n+1))
	}
	all := strings.Join(links, ", ")
	lines := []string{
		"The executor runs out of memory: " + links[0],
		"Rollback and memory: " + strings.Join(links[:2], ", "),
		"All three wait: " + all,
		"Still waiting: " + all,
		"The rollback is out.",
		"Still waiting: " + all,
		"Memory raised: " + all,
		"Paged: " + all,
		"No new runs fail.",
		"Closing out: " + all,
	}
	before := len(l.slack.sentTo(testDM))
	root := postThread(t, l, lines[0])
	for _, line := range lines[1:] {
		l.do(opDMStatus, map[string]any{"thread_ts": root, "text": line})
	}
	posts := l.slack.sentTo(testDM)[before:]
	said, linked, drawn := 0, 0, map[string]int{}
	for _, p := range posts {
		if !strings.HasPrefix(p.text, "<") {
			said++
			linked += len(messageLink.FindAllString(p.text, -1))
		}
		if p.unfurlLinks != "true" {
			continue
		}
		for _, link := range messageLink.FindAllString(p.text, -1) {
			if strings.Contains(link, freshLinkParam+"=") {
				ref, _ := parsePermalink(link)
				drawn[ref.ts]++
			}
		}
	}
	if said != 10 || linked != 21 || len(posts) != 12 {
		t.Fatalf("posts = %+v, want the 10 posts and 21 card links of the incident thread, and the 2 cards a post linked beside cards the thread showed drawn as their own replies", posts)
	}
	if len(drawn) != 3 || slices.ContainsFunc(slices.Collect(maps.Values(drawn)), func(n int) bool { return n != 1 }) {
		t.Fatalf("drawn = %v, want each of the three cards drawn once in the thread, where it drew 21 copies", drawn)
	}
}

func TestAnAlertLinkInADMCardUnfurls(t *testing.T) {
	for _, tc := range []struct {
		name, link, want string
	}{
		{"a Sentry issue", "https://acme.sentry.io/issues/4821/", "true"},
		{"a Sentry issue under an org path", "https://sentry.io/organizations/acme/issues/4821/", "true"},
		{"a Datadog monitor", "https://app.datadoghq.com/monitors/4821", "true"},
		{"a Sentry page that is no issue", "https://acme.sentry.io/releases/", "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := startDaemon(t)
			loggedIn(t, l)
			l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Incident", Body: "The run timeline fails " + tc.link}})
			posts := l.slack.sent()
			at := slices.IndexFunc(posts, func(p fakePost) bool { return strings.Contains(p.blocks, tc.link) })
			if at < 0 || posts[at].unfurlLinks != tc.want {
				t.Fatalf("posts = %+v, want the card with unfurl_links=%s", posts, tc.want)
			}
		})
	}
}

func TestAnAlertLinkInAChannelDoesNotUnfurl(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.post(opSend, map[string]string{"channel_id": testChannel, "text": "The run timeline fails https://acme.sentry.io/issues/4821/"})
	if posts := l.slack.sent(); len(posts) != 1 || posts[0].unfurlLinks != "false" {
		t.Fatalf("posts = %+v, want one post with unfurl_links=false", posts)
	}
}

func TestSlackKeepsAFileNameOnACardOrAPostAsWrittenAndABareURLStaysALink(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	const fix = "<https://github.com/acme/app/pull/7|the fix>"
	l.do(opDMStatus, map[string]any{"card": statusCard{
		Status: "needs_you", Title: "Which job writes page maps?", Waits: "maps",
		Body:    "The subcommand lives in `main.rs`; " + fix + " ships it. Its run: https://example.com/runs/1.",
		Options: []string{"Session pass", "New CronJob"}, Recommended: "Session pass",
		Hints: map[string]string{"New CronJob": "Builds the subcommand in `sandsql-journal`'s `main.rs`."},
	}})
	l.post(opSend, map[string]string{"channel_id": testChannel, "text": "Fixed in `indexing.rs`, see https://example.com/runs/1."})
	cards, posts := l.slack.sentTo(testDM), l.slack.sentTo(testChannel)
	if len(cards) != 1 || len(posts) != 1 {
		t.Fatalf("cards = %+v, posts = %+v, want one of each", cards, posts)
	}
	var blocks []any
	if err := json.Unmarshal([]byte(cards[0].blocks), &blocks); err != nil {
		t.Fatal(err)
	}
	objects := mrkdwnObjectsIn(blocks)
	drawn := make([]string, 0, len(objects))
	for _, text := range objects {
		if text["verbatim"] != true {
			t.Errorf("mrkdwn %q goes out with verbatim %v, so Slack links the file names in it", text["text"], text["verbatim"])
		}
		said, _ := text["text"].(string)
		drawn = append(drawn, said)
	}
	all := strings.Join(drawn, "\n")
	for _, want := range []string{"lives in `main.rs`;", "`sandsql-journal`'s `main.rs`.", fix, "Its run: <https://example.com/runs/1>."} {
		if !strings.Contains(all, want) {
			t.Errorf("card text %q lacks %q", all, want)
		}
	}
	if want := "Fixed in `indexing.rs`, see <https://example.com/runs/1>."; posts[0].text != want {
		t.Errorf("post text = %q, want %q", posts[0].text, want)
	}
	if cards[0].parse != "none" || posts[0].parse != "none" {
		t.Errorf("parse = %q on the card and %q on the post, want none on both so Slack links nothing in their text", cards[0].parse, posts[0].parse)
	}
}

func mrkdwnObjectsIn(node any) []map[string]any {
	var found []map[string]any
	switch n := node.(type) {
	case map[string]any:
		if n["type"] == "mrkdwn" {
			found = append(found, n)
		}
		for _, v := range n {
			found = append(found, mrkdwnObjectsIn(v)...)
		}
	case []any:
		for _, v := range n {
			found = append(found, mrkdwnObjectsIn(v)...)
		}
	}
	return found
}

func TestAThreadListsTheLinksSlackUnfurledApartFromSharedMessages(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	issue := "https://acme.sentry.io/issues/4821/"
	l.slack.add(originChan, slack.Message{Msg: slack.Msg{Timestamp: originTS, User: "U0JOSH", Text: originalText}})
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{
		Timestamp: "1800000000.000100", User: testUser, Text: sharerText,
		Attachments: []slack.Attachment{{FromURL: originLink, Text: originalText}, {FromURL: issue, Title: "TypeError"}, {Title: "no link"}},
	}})
	var res threadResult
	if err := json.Unmarshal(l.do(opThread, map[string]string{"channel_id": testChannel, "thread_ts": "1800000000.000100"}).Body, &res); err != nil {
		t.Fatal(err)
	}
	if got := res.Messages[0].Unfurls; !reflect.DeepEqual(got, []string{issue}) {
		t.Fatalf("unfurls = %v, want only the Sentry issue", got)
	}
	if len(res.Messages[0].Shares) != 1 {
		t.Fatalf("shares = %+v, want the one shared message", res.Messages[0].Shares)
	}
}

func TestAStatusCardLinkingOneOfHisOpenCardsUnfurlsIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	link := postWaiting(t, l, mergeCard()).url()
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Status", Body: "Waiting on " + link}})
	posts := l.slack.sent()
	at := slices.IndexFunc(posts, func(p fakePost) bool { return strings.Contains(p.blocks, link) })
	if at < 0 || posts[at].unfurlLinks != "true" {
		t.Fatalf("posts = %+v, want the status card keeping its link and Slack drawing the card under it", posts)
	}
	if slices.ContainsFunc(posts, func(p fakePost) bool { return p.attachments != "" }) {
		t.Fatalf("posts = %+v, want no hand-built preview", posts)
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

func TestOpsRefuseWithoutLoginAndNameTheLogin(t *testing.T) {
	l := startDaemon(t)
	raw, _ := json.Marshal(map[string]string{"channel_id": testChannel, "text": "hi"})
	_, err := invoke(l.ctx, l.deps, testSession, "/any", opSend, raw)
	if err == nil || !strings.Contains(err.Error(), "cc-slack login") {
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
		Messages []readMessage `json:"messages"`
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

var chart = slack.File{
	ID: "F1", Name: "chart.png", Mimetype: "image/png", Permalink: "https://acme.slack.com/files/U1/F1/chart.png",
	URLPrivateDownload: "https://files.slack.com/files-pri/T1-F1/download/chart.png", Size: 4,
}

func chartFile() messageFile {
	return messageFile{ID: "F1", Name: "chart.png", Mimetype: "image/png", Permalink: chart.Permalink, Path: filepath.Join(filesDir(), "F1.png")}
}

func TestHistoryReturnsTopLevelMessagesNewestFirst(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.serveFile(chart, []byte("\x89PNG"))
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000100", User: testUser, Text: "oldest"}})
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{
		Timestamp: "1800000000.000200", BotID: "BBOT", Username: "yasyf", Text: "PR is up\nsecond line",
		ReplyCount: 2, LatestReply: "1800000000.000400",
		Files: []slack.File{chart},
	}})
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000300", User: testUser, Text: "newest"}})
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000400", ThreadTimestamp: "1800000000.000200", User: testUser, Text: "reply"}})

	var res historyResult
	if err := json.Unmarshal(l.do(opHistory, map[string]any{"channel_id": testChannel, "limit": 2}).Body, &res); err != nil {
		t.Fatal(err)
	}
	want := []readMessage{
		{TS: "1800000000.000300", User: testUser, UserName: "yasyf", FromUser: true, Text: "newest", Permalink: "https://acme.slack.com/archives/C1/p1800000000000300"},
		{
			TS: "1800000000.000200", UserName: "yasyf", FromClaude: true, FromBot: true, Text: "PR is up\nsecond line",
			Permalink: "https://acme.slack.com/archives/C1/p1800000000000200", ReplyCount: 2, LatestReply: "1800000000.000400",
			Files: []messageFile{chartFile()},
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
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opHistory, raw); err == nil || !strings.Contains(err.Error(), "not_in_channel") || strings.Contains(err.Error(), "mcp__slack__") {
		t.Fatalf("unreadable channel: %v", err)
	}
}

func TestThreadCarriesEachMessagesFiles(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.serveFile(chart, []byte("\x89PNG"))
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000100", User: testUser, Text: "status"}})
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{
		Timestamp: "1800000000.000200", ThreadTimestamp: "1800000000.000100", BotID: "BBOT", Username: "yasyf", Text: "chart",
		Files: []slack.File{chart},
	}})

	var res threadResult
	if err := json.Unmarshal(l.do(opThread, map[string]string{"channel_id": testChannel, "thread_ts": "1800000000.000100"}).Body, &res); err != nil {
		t.Fatal(err)
	}
	want := []messageFile{chartFile()}
	if len(res.Messages) != 2 || res.Messages[0].Files != nil || !reflect.DeepEqual(res.Messages[1].Files, want) {
		t.Fatalf("thread = %+v, want only the reply to carry chart.png", res.Messages)
	}
	if got, err := os.ReadFile(want[0].Path); err != nil || string(got) != "\x89PNG" {
		t.Fatalf("downloaded %q, %v", got, err)
	}

	if err := json.Unmarshal(l.do(opThread, map[string]string{"channel_id": testChannel, "thread_ts": "1800000000.000100"}).Body, &res); err != nil {
		t.Fatal(err)
	}
	if n := l.slack.callCount("files.download"); n != 1 {
		t.Fatalf("downloads = %d, want a second read to reuse the file", n)
	}
}

func TestThreadReportsAnImageItCannotDownloadWithoutFailingTheRead(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.serveFile(chart, []byte("\x89PNG"))
	l.slack.fail("files.download", slack.SlackErrorResponse{Err: "file_not_found"})
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000100", User: testUser, Text: "status", Files: []slack.File{chart}}})

	var res threadResult
	if err := json.Unmarshal(l.do(opThread, map[string]string{"channel_id": testChannel, "thread_ts": "1800000000.000100"}).Body, &res); err != nil {
		t.Fatal(err)
	}
	file := res.Messages[0].Files[0]
	if file.Path != "" || !strings.Contains(file.PathError, "file_not_found") {
		t.Fatalf("file = %+v, want no path and the download failure", file)
	}
}

const (
	sharerText   = "I wonder if we have logs for this?"
	originalText = "iris crashed and now seeing this"
	originChan   = "C0ORIGIN"
	originTS     = "1799999000.000100"
	originLink   = "https://acme.slack.com/archives/C0ORIGIN/p1799999000000100"
)

func addSharedThread(l *liveDaemon) {
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{
		Timestamp: "1800000000.000100", User: testUser, Text: sharerText,
		Attachments: []slack.Attachment{{FromURL: originLink, AuthorID: "U0JOSH", AuthorName: "Josh", Text: originalText}},
	}})
}

func TestAThreadShowsASharedMessageAsItsOriginalAuthorsReport(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.serveFile(chart, []byte("\x89PNG"))
	l.slack.add(originChan, slack.Message{Msg: slack.Msg{Timestamp: originTS, Username: "Josh Weiner", User: "U0JOSH", Text: originalText, Files: []slack.File{chart}}})
	addSharedThread(l)
	l.do(opThread, map[string]string{"channel_id": originChan, "thread_ts": originTS})

	var res threadResult
	if err := json.Unmarshal(l.do(opThread, map[string]string{"channel_id": testChannel, "thread_ts": "1800000000.000100"}).Body, &res); err != nil {
		t.Fatal(err)
	}
	if n := l.slack.callCount(methodReplies); n != 2 {
		t.Fatalf("replies calls = %d, want one per thread read: the share answers from the cached thread it came from", n)
	}
	want := []sharedMessage{{
		Permalink: originLink, ChannelID: originChan, TS: originTS, User: "U0JOSH", UserName: "Josh Weiner", Text: originalText, Files: []messageFile{chartFile()},
	}}
	if !reflect.DeepEqual(res.Messages[0].Shares, want) {
		t.Fatalf("shares = %+v, want %+v", res.Messages[0].Shares, want)
	}
	if got, err := os.ReadFile(want[0].Files[0].Path); err != nil || string(got) != "\x89PNG" {
		t.Fatalf("screenshot %q, %v", got, err)
	}
	if text := res.Messages[0].Text; text != sharerText {
		t.Fatalf("text = %q, want the sharer's own words, the shared message under shares alone", text)
	}
}

func TestARepostOfAReadMessageCarriesOnlyItsOwnWords(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	lock := postWaiting(t, l, mergeCard())
	image := postWaiting(t, l, statusCard{Status: "needs_you", Title: "Approve the image.sh fix?", Body: "CI is green.", Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: "step-4:image"})
	linked := "https://example.slack.com/archives/" + testDM + "/p1800000400000100"
	words := "Catch-up, page 8:\n<" + linked + "|your card on the clock>"
	page := slack.Message{Msg: slack.Msg{
		Timestamp: "1800000500.000100", BotID: "B0CLAUDE", Text: words,
		Attachments: []slack.Attachment{{FromURL: linked, AuthorName: "PlatClaude", Text: "Approve the clock?\n<" + lock.url() + ">\n<" + image.url() + ">"}},
	}}
	l.slack.add(testDM, page)

	var res threadResult
	if err := json.Unmarshal(l.do(opThread, map[string]string{"thread_ts": page.Timestamp}).Body, &res); err != nil {
		t.Fatal(err)
	}
	read := res.Messages[0]
	if read.Text != words || len(read.Shares) != 1 || !strings.Contains(read.Shares[0].Text, image.url()) {
		t.Fatalf("read = %+v, want its own words as text and Slack's preview of the card it links under shares", read)
	}
	l.refused(opReply, map[string]string{"channel_id": testUser, "thread_ts": lock.thread, "text": messageText(page)}, cardLinkNamed)
	l.post(opReply, map[string]string{"channel_id": testUser, "thread_ts": lock.thread, "text": read.Text})
}

func TestACardLinkedIntoAThreadReadsItsBodyOnceAndItsShowMeTable(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	card := statusCard{
		Status: "needs_you", Title: "Tap the uploader fix now?", Options: []string{"Tap now", "Hold"}, Recommended: "Tap now",
		Body: "[The fix](https://github.com/acme/app/pull/7) makes `uploader.resume` start from the highest position. I recommend tapping.\n\n*Show me*\nBefore and after one `uploader.sync`.\n\n| — | Before | After |\n|---|---|---|\n| Uploader position | 0, retries every 300 s | 32 |\n| Replica ends at | 32 | 49 |",
	}
	l.slack.add(originChan, slack.Message{Msg: slack.Msg{Timestamp: originTS, BotID: "B0CLAUDE", Text: card.text(), Blocks: slack.Blocks{BlockSet: card.replyBlocks(view())}}})
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{
		Timestamp: "1800000000.000100", BotID: "B0CLAUDE", Text: slackLink(originLink, card.Title),
		Attachments: []slack.Attachment{{FromURL: originLink, AuthorName: "PlatClaude", Text: card.text()}},
	}})
	l.do(opThread, map[string]string{"channel_id": originChan, "thread_ts": originTS})

	var res threadResult
	if err := json.Unmarshal(l.do(opThread, map[string]string{"channel_id": testChannel, "thread_ts": "1800000000.000100"}).Body, &res); err != nil {
		t.Fatal(err)
	}
	shared := res.Messages[0].Shares[0].Text
	body := "<https://github.com/acme/app/pull/7|The fix> makes `uploader.resume` start from the highest position. I recommend tapping."
	if strings.Count(shared, body) != 1 || strings.Count(shared, card.Title) != 1 {
		t.Fatalf("shared card reads %q, want its title and body once each", shared)
	}
	if !strings.Contains(shared, "[table block]\n| *—* | *Before* | *After* |\n| --- | --- | --- |\n| Uploader position | 0, retries every 300 s | 32 |\n| Replica ends at | 32 | 49 |") {
		t.Fatalf("shared card reads %q, want its Show me table row by row", shared)
	}
}

func TestHistoryResolvesASharedMessage(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(originChan, slack.Message{Msg: slack.Msg{Timestamp: originTS, Username: "Josh Weiner", User: "U0JOSH", Text: originalText}})
	addSharedThread(l)
	l.do(opThread, map[string]string{"channel_id": originChan, "thread_ts": originTS})

	var res historyResult
	if err := json.Unmarshal(l.do(opHistory, map[string]any{"channel_id": testChannel}).Body, &res); err != nil {
		t.Fatal(err)
	}
	if shares := res.Messages[0].Shares; len(shares) != 1 || shares[0].UserName != "Josh Weiner" || shares[0].ChannelID != originChan {
		t.Fatalf("shares = %+v, want Josh's message", shares)
	}
}

func TestAThreadLeavesAShareWhoseThreadIsNotCachedAsItsLinkWithNoSlackRead(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(originChan, slack.Message{Msg: slack.Msg{Timestamp: originTS, Username: "Josh Weiner", User: "U0JOSH", Text: originalText}})
	addSharedThread(l)

	var res threadResult
	if err := json.Unmarshal(l.do(opThread, map[string]string{"channel_id": testChannel, "thread_ts": "1800000000.000100"}).Body, &res); err != nil {
		t.Fatal(err)
	}
	want := []sharedMessage{{Permalink: originLink, ChannelID: originChan, TS: originTS, User: "U0JOSH", UserName: "Josh", Text: originalText}}
	if !reflect.DeepEqual(res.Messages[0].Shares, want) {
		t.Fatalf("shares = %+v, want %+v: the link and what Slack attached to it", res.Messages[0].Shares, want)
	}
	if n := l.slack.callCount(methodReplies); n != 1 {
		t.Fatalf("replies calls = %d, want 1: a share never reads its own thread from Slack", n)
	}
}

func TestOnlyASlackMessageLinkIsAShare(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    slack.Attachment
		share bool
	}{
		{"a shared message", slack.Attachment{FromURL: originLink}, true},
		{"a shared reply", slack.Attachment{FromURL: originLink + "?thread_ts=1799998000.000100"}, true},
		{"a link unfurl", slack.Attachment{FromURL: "https://github.com/acme/app/pull/1"}, false},
		{"archives on another host", slack.Attachment{FromURL: "https://lists.example.com/archives/C0ORIGIN/p1799999000000100"}, false},
		{"a channel link", slack.Attachment{FromURL: "https://acme.slack.com/archives/C0ORIGIN"}, false},
		{"an alert", slack.Attachment{Title: "[Triggered] lane stale"}, false},
	} {
		if _, share := shareRef(tc.at); share != tc.share {
			t.Errorf("%s: share = %v, want %v", tc.name, share, tc.share)
		}
	}
}

func TestABotPostsSharedImageBlockReadsAsItsFile(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.serveFile(slack.File{
		ID: "F2", Name: "sysd.png", Mimetype: "image/png", Permalink: "https://acme.slack.com/files/U1/F2/sysd.png",
		URLPrivateDownload: "https://files.slack.com/files-pri/T1-F2/download/sysd.png", Size: 4,
	}, []byte("\x89PNG"))
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
	want := []messageFile{{ID: "F2", Name: "sysd.png", Mimetype: "image/png", Permalink: "https://acme.slack.com/files/U1/F2/sysd.png", Path: filepath.Join(filesDir(), "F2.png")}}
	if len(res.Messages) != 1 || !reflect.DeepEqual(res.Messages[0].Files, want) {
		t.Fatalf("history = %+v, want the image block's file", res.Messages)
	}
}

func TestChannelsReportsAChannelSharedWithAnotherWorkspace(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	shared := slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C1", IsExtShared: true}, Name: "northwind-team"}}
	internal := slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C2"}, Name: "platform-eng"}}
	l.slack.list(shared, internal)

	var res channelsResult
	if err := json.Unmarshal(l.do(opChannels, map[string]string{}).Body, &res); err != nil {
		t.Fatal(err)
	}
	want := []channelInfo{{ID: "C1", Name: "northwind-team", ExtShared: true}, {ID: "C2", Name: "platform-eng"}}
	if !reflect.DeepEqual(res.Channels, want) {
		t.Fatalf("channels = %+v, want %+v", res.Channels, want)
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

func TestUsersFindsAnIDOrExactEmailWhileUsersListFails(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	ada := &slack.User{ID: "U0123ABCDE", Name: "alovelace", RealName: "Ada Lovelace", Profile: slack.UserProfile{DisplayName: "Ada", Email: "ada@example.com"}}
	l.slack.setUser(ada)
	l.slack.setUser(&slack.User{ID: "U0GONE0001", Name: "cbabbage", RealName: "Charles Babbage", Deleted: true, Profile: slack.UserProfile{Email: "charles@example.com"}})
	fatal := slack.SlackErrorResponse{Err: "fatal_error"}
	users := func(query string) []userInfo {
		t.Helper()
		var res usersResult
		if err := json.Unmarshal(l.do(opUsers, map[string]string{"query": query}).Body, &res); err != nil {
			t.Fatal(err)
		}
		return res.Users
	}
	refusal := func(query string) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"query": query})
		_, err := invoke(l.ctx, l.deps, testSession, "/any", opUsers, raw)
		if err == nil {
			t.Fatalf("users %q succeeded, want a refusal", query)
		}
		return err.Error()
	}

	l.slack.fail("users.list", fatal)
	got := refusal("lovelace")
	for _, want := range []string{"Slack's users.list answered fatal_error", "users.info", "users.lookupByEmail"} {
		if !strings.Contains(got, want) {
			t.Errorf("users.list refusal = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "conversation") {
		t.Errorf("users.list refusal = %q, want no word of reading a conversation", got)
	}

	l.slack.fail("users.list", fatal)
	l.slack.fail("users.list", fatal)
	want := []userInfo{{ID: "U0123ABCDE", Name: "alovelace", DisplayName: "Ada", RealName: "Ada Lovelace", Email: "ada@example.com"}}
	if got := users("U0123ABCDE"); !slices.Equal(got, want) {
		t.Fatalf("users by id = %+v, want %+v", got, want)
	}
	if got := users("ada@example.com"); !slices.Equal(got, want) {
		t.Fatalf("users by email = %+v, want %+v", got, want)
	}
	for _, query := range []string{"U0NOBODY01", "nobody@example.com", "U0GONE0001", "charles@example.com"} {
		if got := users(query); len(got) != 0 {
			t.Fatalf("users %q = %+v, want none", query, got)
		}
	}
	if n := l.slack.callCount("users.list"); n != 1 {
		t.Fatalf("users.list calls = %d, want only the name query's", n)
	}

	l.slack.fail("users.info", fatal)
	if got := refusal("U0123ABCDE"); !strings.Contains(got, "Slack's users.info answered fatal_error") {
		t.Fatalf("users.info refusal = %q", got)
	}
	l.slack.fail("users.lookupByEmail", fatal)
	if got := refusal("ada@example.com"); !strings.Contains(got, "Slack's users.lookupByEmail answered fatal_error") {
		t.Fatalf("users.lookupByEmail refusal = %q", got)
	}
}

func TestChannelsNamesConversationsListWhenItFails(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.fail("conversations.list", slack.SlackErrorResponse{Err: "fatal_error"})
	raw, _ := json.Marshal(map[string]string{})
	_, err := invoke(l.ctx, l.deps, testSession, "/any", opChannels, raw)
	if err == nil || !strings.Contains(err.Error(), "Slack's conversations.list answered fatal_error") {
		t.Fatalf("conversations.list refusal = %v", err)
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

func TestUnfollowStopsAThreadByURLOrByChannelAndThread(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: testThread, Text: "parent"}})
	l.do(opFollow, map[string]string{"channel_id": testChannel, "thread_ts": testThread})
	var first, second map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]string{"text": "starting"}).Body, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(l.do(opDMStatus, map[string]string{"text": "halfway"}).Body, &second); err != nil {
		t.Fatal(err)
	}
	unfollow := func(body map[string]string) unfollowResult {
		t.Helper()
		var res unfollowResult
		if err := json.Unmarshal(l.do(opUnfollow, body).Body, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}

	if got := unfollow(map[string]string{"channel_id": testChannel, "thread_ts": testThread}); !got.Stopped || got.Remaining != 1 {
		t.Fatalf("unfollow by channel and thread = %+v, want the thread watch stopped and the DM subscription left", got)
	}
	if got := unfollow(map[string]string{"url": first["permalink"]}); !got.Stopped || got.Remaining != 1 {
		t.Fatalf("unfollow by url = %+v, want the DM thread let go and the subscription left", got)
	}
	if active := activeDMThreads(t, l); !slices.Equal(active, []string{second["ts"]}) {
		t.Fatalf("active DM threads = %v, want only the message still held", active)
	}
	if got := unfollow(map[string]string{"url": first["permalink"]}); got.Stopped {
		t.Fatalf("second unfollow by url = %+v, want nothing left to stop", got)
	}
}

func TestFollowsListsOnlyTheSessionsActiveWatches(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: testThread, Text: "parent"}})
	follow := func(d cmd.Deps, session string, body map[string]string) followResult {
		raw, _ := json.Marshal(body)
		reply, err := invoke(l.ctx, d, session, "/any", opFollow, raw)
		if err != nil {
			t.Fatal(err)
		}
		var res followResult
		if err := json.Unmarshal(reply.Body, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	follows := func(d cmd.Deps, session string) followsResult {
		reply, err := invoke(l.ctx, d, session, "/any", opFollows, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		var res followsResult
		if err := json.Unmarshal(reply.Body, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	stranger := l.deps
	stranger.ClaudePID = func() int { return testPID + 3 }

	thread := follow(l.deps, testSession, map[string]string{"channel_id": testChannel, "thread_ts": testThread})
	channel := follow(l.deps, testSession, map[string]string{"channel_id": testChannel})
	theirs := follow(stranger, "other-session", map[string]string{"channel_id": testChannel})
	l.do(opUnfollow, map[string]int64{"watch_id": thread.WatchID})

	mine := follows(l.deps, testSession)
	if mine.Session != testSession || len(mine.Watches) != 1 || mine.Watches[0].WatchID != channel.WatchID || mine.Watches[0].Kind != kindChannel || mine.Watches[0].ChannelID != testChannel {
		t.Fatalf("follows = %+v, want only the active channel watch %d", mine, channel.WatchID)
	}
	other := follows(l.deps, "other-session")
	if other.Session != "other-session" || len(other.Watches) != 1 || other.Watches[0].WatchID != theirs.WatchID {
		t.Fatalf("follows --session other-session = %+v, want only its watch %d", other, theirs.WatchID)
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
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opFollow, raw); err == nil || !strings.Contains(err.Error(), "slack_history") {
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
	if err == nil || !strings.Contains(err.Error(), "<#"+testChannel+"> is private") || !strings.Contains(err.Error(), errNoClientID.Error()) ||
		strings.Contains(err.Error(), "/invite") {
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

func TestPostSlackRefusesTheBotIsSentAgainAsTheUser(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	user := l.user.as(testUser)

	l.slack.fail("chat.postMessage", slack.SlackErrorResponse{Err: "restricted_action"})
	body := l.hooked(opSend, map[string]string{"channel_id": testChannel, "text": "deploy is out"})
	var res postResult
	if err := json.Unmarshal(l.do(opSend, body).Body, &res); err != nil {
		t.Fatal(err)
	}
	posts := user.sentTo(testChannel)
	if !res.Posted || !res.AsUser || len(posts) != 1 || posts[0].text != "_(Yasyf's Agent)_ deploy is out" || len(l.slack.sentTo(testChannel)) != 0 {
		t.Fatalf("send = %+v, posts as the user %+v, want one marked post as the user and none as the bot", res, posts)
	}
	if spends := l.tickets.committed(field(body, "ticket_id")); len(spends) != 1 {
		t.Fatalf("committed spends = %+v, want the post as the user to spend the ticket", spends)
	}
}

func TestReactionSlackRefusesTheBotIsAddedAgainAsTheUser(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	user := l.user.as(testUser)

	l.slack.fail("reactions.add", slack.SlackErrorResponse{Err: "restricted_action"})
	l.do(opReact, map[string]string{"channel_id": testChannel, "ts": "1800000000.000100", "name": "eyes"})
	if got := user.reactedTo("1800000000.000100"); !slices.Equal(got, []string{"eyes"}) || len(l.slack.reactedTo("1800000000.000100")) != 0 {
		t.Fatalf("reactions as the user = %v, want eyes added as the user", got)
	}
}

func TestUserTokenWithoutWriteScopesAsksForConsentAgain(t *testing.T) {
	l := startDaemon(t)
	loggedInWithOAuth(t, l)
	user := l.user.as(testUser)

	l.slack.fail("chat.postMessage", slack.SlackErrorResponse{Err: "restricted_action"})
	user.fail("chat.postMessage", slack.SlackErrorResponse{Err: "missing_scope"})
	body := l.hooked(opSend, map[string]string{"channel_id": testChannel, "text": "deploy is out"})
	raw, _ := json.Marshal(body)
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opSend, raw); err == nil || !strings.Contains(err.Error(), "tap Allow on the card") || !strings.Contains(err.Error(), "still covers it") {
		t.Fatalf("send with a read-only user token = %v, want a consent card and the ticket handed back", err)
	}
	if scopes := consentLink(t, l.slack).Query().Get("user_scope"); !strings.Contains(scopes, "chat:write") || !strings.Contains(scopes, "reactions:write") {
		t.Fatalf("consent scopes %q, want chat:write and reactions:write", scopes)
	}
	if _, ok := l.user.stored(); !ok {
		t.Fatal("the read-only token was dropped before the new consent")
	}
}

func TestSelfInviteNeedsTheLoggedInUsersCredential(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	user := l.user.as("UOTHER")

	l.slack.fail("chat.postMessage", slack.SlackErrorResponse{Err: "channel_not_found"})
	l.slack.fail("conversations.join", slack.SlackErrorResponse{Err: "channel_not_found"})
	l.refused(opSend, l.hooked(opSend, map[string]string{"channel_id": testChannel, "text": "PR is up"}),
		"inviting it as the user failed (the Slack user token is UOTHER in T1, not "+testUser+" in T1)")
	if n := user.callCount("conversations.invite"); n != 0 {
		t.Fatalf("conversations.invite calls = %d, want none with another user's credential", n)
	}
	if asks := l.slack.sentTo("D" + testUser); len(asks) != 0 {
		t.Fatalf("DM posts = %+v, want no /invite ask", asks)
	}
}

func TestPostToAPersonsDMTheBotCannotJoinNamesTheirGroupDM(t *testing.T) {
	for name, tc := range map[string]struct {
		credential bool
		want       string
	}{
		"other member read as the user": {true, "D0THEIRDM is a 1:1 DM the app cannot join, your DM with <@U0REPORTER>; " +
			"post to them as usual with channel_id U0REPORTER, which opens the group DM of that person, you and the app"},
		"no user token": {false, "reading its other member as you failed (" + errNoClientID.Error()},
	} {
		t.Run(name, func(t *testing.T) {
			l := startDaemon(t)
			loggedIn(t, l)
			fields := asBody(map[string]string{"channel_id": "D0THEIRDM", "thread_ts": "1800000000.000100", "text": "Fixed: the deploy runs again."})
			place := placeOf(opReply, fields)
			if tc.credential {
				l.user.as(testUser).setDM("D0THEIRDM", "U0REPORTER")
				place = map[string]string{"channel": "U0REPORTER", "thread": ""}
			}
			l.slack.fail("chat.postMessage", slack.SlackErrorResponse{Err: "channel_not_found"})
			body := l.spending(l.tickets.mint(fakeTicket{tree: testTree, place: place, reach: reachOnce, rule: "asked-owner", fingerprint: fingerprintOf(fields)}), fields)
			raw, _ := json.Marshal(body)
			_, err := invoke(l.ctx, l.deps, testSession, "/any", opReply, raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("reply in D0THEIRDM = %v, want the group DM route %q", err, tc.want)
			}
		})
	}
}

func TestExpiringUserTokenIsRenewedBeforeTheInvite(t *testing.T) {
	l := startDaemon(t)
	loggedInWithOAuth(t, l)
	l.user.store(userToken{UserID: testUser, TeamID: "T1", AccessToken: storedUserToken, RefreshToken: "xoxe-1", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	stale := l.user.grant(storedUserToken, testUser)
	renewed := l.user.grant("xoxe.xoxp-renewed", testUser)
	l.user.answer = oauthAccess{OK: true, oauthToken: oauthToken{AccessToken: "xoxe.xoxp-renewed", RefreshToken: "xoxe-2", ExpiresIn: 43200}}

	l.slack.fail("conversations.join", slack.SlackErrorResponse{Err: "method_not_supported_for_channel_type"})
	decodeChange(t, l.post(opChannelJoin, map[string]any{"channel_id": testChannel}))
	if _, _, invites, _ := renewed.channelChanges(); !slices.Equal(invites[testChannel], []string{"UBOT"}) || stale.callCount("conversations.invite") != 0 {
		t.Fatalf("invites with the renewed token %v, want the bot invited to %s with it alone", invites, testChannel)
	}
	want := url.Values{"client_id": {testClientID}, "grant_type": {"refresh_token"}, "refresh_token": {"xoxe-1"}}
	if grants := l.user.sentGrants(); len(grants) != 1 || !reflect.DeepEqual(grants[0], want) {
		t.Fatalf("grants = %v, want one refresh %v", grants, want)
	}
	if tok, _ := l.user.stored(); tok.AccessToken != "xoxe.xoxp-renewed" || tok.RefreshToken != "xoxe-2" || tok.UserID != testUser {
		t.Fatalf("stored = %+v, want the renewed token and its new refresh token", tok)
	}
}

func TestRevokedUserTokenAsksForConsentOnce(t *testing.T) {
	l := startDaemon(t)
	loggedInWithOAuth(t, l)
	l.user.as(testUser).fail("auth.test", slack.SlackErrorResponse{Err: "token_revoked"})

	l.slack.fail("conversations.join", slack.SlackErrorResponse{Err: "method_not_supported_for_channel_type"})
	l.refused(opChannelJoin, l.hooked(opChannelJoin, map[string]any{"channel_id": testChannel}), "tap Allow on the card")
	if _, ok := l.user.stored(); ok {
		t.Fatal("the revoked token is still stored")
	}
	l.slack.fail("conversations.join", slack.SlackErrorResponse{Err: "method_not_supported_for_channel_type"})
	l.refused(opChannelJoin, l.hooked(opChannelJoin, map[string]any{"channel_id": testChannel}), "tap Allow on the card")
	if cards := l.slack.sentTo("D" + testUser); len(cards) != 1 {
		t.Fatalf("DM posts = %+v, want one consent card for both refusals", cards)
	}
}

func TestConsentTradesTheCodeForTheUsersTokenAndReadsTheirDM(t *testing.T) {
	l := startDaemon(t)
	loggedInWithOAuth(t, l)
	l.slack.fail(repliesOf("D0THEIRDM", testThread), slack.SlackErrorResponse{Err: "channel_not_found"})
	l.refused(opThread, map[string]string{"channel_id": "D0THEIRDM", "thread_ts": testThread}, "tap Allow on the card")
	link := consentLink(t, l.slack)
	if got := link.Query(); got.Get("client_id") != testClientID || got.Get("redirect_uri") != consentRedirect || got.Get("code_challenge_method") != "S256" ||
		!strings.Contains(got.Get("user_scope"), "im:history") || got.Get("team") != "T1" {
		t.Fatalf("consent link %s, want PlatClaude's client, the localhost redirect, S256 PKCE and im:history", link)
	}

	user := l.user.grant("xoxe.xoxp-1", testUser)
	user.add("D0THEIRDM", slack.Message{Msg: slack.Msg{Timestamp: testThread, User: "UOTHER", Text: "can you look at the deploy?"}})
	l.user.answer = oauthAccess{OK: true, AuthedUser: oauthToken{ID: testUser, AccessToken: "xoxe.xoxp-1", RefreshToken: "xoxe-1", ExpiresIn: 43200}}
	l.user.answer.Team.ID = "T1"
	if code := redirect(l.user, "code=c1&state="+link.Query().Get("state")); code != http.StatusOK {
		t.Fatalf("redirect answered %d, want 200", code)
	}
	grant := l.user.sentGrants()[0]
	challenge := sha256.Sum256([]byte(grant.Get("code_verifier")))
	if grant.Get("code") != "c1" || grant.Get("redirect_uri") != consentRedirect || grant.Has("client_secret") ||
		base64.RawURLEncoding.EncodeToString(challenge[:]) != link.Query().Get("code_challenge") {
		t.Fatalf("grant = %v, want the code traded with the verifier behind the link's challenge and no secret", grant)
	}
	if edits := l.slack.edited(); len(edits) != 1 || !strings.HasPrefix(edits[0].text, "Allowed") {
		t.Fatalf("card edits = %+v, want the card marked Allowed", edits)
	}
	l.slack.fail(repliesOf("D0THEIRDM", testThread), slack.SlackErrorResponse{Err: "channel_not_found"})
	var res threadResult
	if err := json.Unmarshal(l.do(opThread, map[string]string{"channel_id": "D0THEIRDM", "thread_ts": testThread}).Body, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || res.Messages[0].Text != "can you look at the deploy?" {
		t.Fatalf("thread = %+v, want the DM read as the user", res)
	}
}

func TestConsentRefusesAnotherAccountAndAForeignState(t *testing.T) {
	l := startDaemon(t)
	loggedInWithOAuth(t, l)
	l.slack.fail(repliesOf("D0THEIRDM", testThread), slack.SlackErrorResponse{Err: "channel_not_found"})
	l.refused(opThread, map[string]string{"channel_id": "D0THEIRDM", "thread_ts": testThread}, "tap Allow on the card")
	state := consentLink(t, l.slack).Query().Get("state")

	if code := redirect(l.user, "code=c1&state=forged"); code != http.StatusBadRequest || len(l.user.sentGrants()) != 0 {
		t.Fatalf("forged state answered %d after %d grants, want 400 and no trade", code, len(l.user.sentGrants()))
	}
	l.user.answer = oauthAccess{OK: true, AuthedUser: oauthToken{ID: "UOTHER", AccessToken: "xoxe.xoxp-other"}}
	l.user.answer.Team.ID = "T1"
	if code := redirect(l.user, "code=c1&state="+state); code != http.StatusForbidden {
		t.Fatalf("another account's consent answered %d, want 403", code)
	}
	if _, ok := l.user.stored(); ok {
		t.Fatal("stored another account's token")
	}
	if edits := l.slack.edited(); len(edits) != 1 || !strings.Contains(edits[0].text, "UOTHER") {
		t.Fatalf("card edits = %+v, want the card to name the other account", edits)
	}
}

func TestUserTokenFailureOtherThanRevocationKeepsTheStoredToken(t *testing.T) {
	l := startDaemon(t)
	loggedInWithOAuth(t, l)
	l.user.as(testUser).fail("auth.test", slack.SlackErrorResponse{Err: "account_inactive"})

	l.slack.fail("conversations.join", slack.SlackErrorResponse{Err: "method_not_supported_for_channel_type"})
	l.refused(opChannelJoin, l.hooked(opChannelJoin, map[string]any{"channel_id": testChannel}),
		"the Slack user token failed auth.test: account_inactive")
	if _, ok := l.user.stored(); !ok {
		t.Fatal("the stored token was dropped")
	}
	if cards := l.slack.sentTo("D" + testUser); len(cards) != 0 {
		t.Fatalf("DM posts = %+v, want no consent card", cards)
	}
}

const testClientID = "5050.9090"

func loggedInWithOAuth(t *testing.T, l *liveDaemon) {
	t.Helper()
	l.slack.setUser(&slack.User{ID: testUser, Name: "ym", RealName: "Yasyf Mohamedali", Profile: slack.UserProfile{
		Email: "ym@example.com", Image512: "https://avatars/ym.png", DisplayName: "yasyf",
	}})
	l.do(opLogin, map[string]any{
		"token": "xoxb-test", "email": "ym@example.com", "socket_url": "ws://127.0.0.1:1/live", "repo_root": "/repo", "capt_hook_path": testCaptHook,
		"avatar_key_id": "r2-key", "avatar_secret": "r2-secret",
		"config": map[string]any{"oauth": map[string]string{"client_id": testClientID}},
	})
}

func consentLink(t *testing.T, s *fakeSlack) *url.URL {
	t.Helper()
	cards := s.sentTo("D" + testUser)
	if len(cards) != 1 {
		t.Fatalf("DM posts = %+v, want one consent card", cards)
	}
	m := regexp.MustCompile(`<(` + regexp.QuoteMeta(slackAuthorizeURL) + `\?[^|]+)\|`).FindStringSubmatch(cards[0].text)
	if m == nil {
		t.Fatalf("consent card %q carries no authorize link", cards[0].text)
	}
	link, err := url.Parse(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return link
}

func redirect(u *fakeUserToken, query string) int {
	rec := httptest.NewRecorder()
	u.consent.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, consentPath+"?"+query, http.NoBody))
	return rec.Code
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

	var lines strings.Builder
	for i := range 60 {
		fmt.Fprintf(&lines, "• *What's risky %d:* a batch line with a <https://github.com/acme/app/pull/42829|PR link>\n", i+1)
	}
	text := lines.String()
	l.post(opSend, map[string]any{"channel_id": testChannel, "text": text})
	posts := l.slack.sent()
	if len(posts) != 1 || len([]rune(posts[0].text)) > maxTextChars {
		t.Fatalf("posts = %d, text = %d runes, want one post whose text Slack will not split", len(posts), len([]rune(posts[0].text)))
	}
	var blocks slack.Blocks
	if err := json.Unmarshal([]byte(posts[0].blocks), &blocks); err != nil {
		t.Fatal(err)
	}
	if got := messageText(slack.Message{Msg: slack.Msg{Text: posts[0].text, Blocks: blocks}}); got != strings.TrimSpace(text) {
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
	if err == nil || !strings.Contains(err.Error(), "channel-join") {
		t.Fatalf("react in a private channel: %v", err)
	}
	if n := user.callCount("conversations.invite"); n != 0 {
		t.Fatalf("conversations.invite calls = %d, want a reaction to never invite the bot", n)
	}
	if n := l.slack.callCount("reactions.add"); n != 1 {
		t.Fatalf("reactions.add calls = %d, want no retry without a join", n)
	}
}

func TestDMStatusPostsEachUpdateAtTheRootWithItsOwnThread(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	var first, second map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]string{"text": "starting"}).Body, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(l.do(opDMStatus, map[string]string{"text": "halfway"}).Body, &second); err != nil {
		t.Fatal(err)
	}
	sent := l.slack.sent()
	if sent[0].threadTS != "" || sent[1].threadTS != "" || first["thread_ts"] != sent[0].ts || second["thread_ts"] != sent[1].ts || first["channel_id"] != "D"+testUser {
		t.Fatalf("dm-status posts = %+v, results %v then %v, want each at the DM root", sent, first, second)
	}
	if active := activeDMThreads(t, l); !slices.Equal(active, []string{sent[0].ts, sent[1].ts}) {
		t.Fatalf("active DM threads = %v, want one per message", active)
	}
	if n := dmSubscriptionCount(t, l); n != 1 {
		t.Fatalf("DM watch rows = %d, want the one subscription", n)
	}
}

func TestAShortDMLineHoldingAChartFenceDrawsItAsSlacksChart(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.do(opDMStatus, map[string]string{"text": "Posts against alerts today.\n" + barChart})
	sent := l.slack.sent()
	if len(sent) != 1 || !strings.Contains(sent[0].blocks, `"type":"data_visualization"`) || strings.Contains(sent[0].blocks, "vega-lite") {
		t.Fatalf("dm-status posts = %+v, want the chart fence drawn as Slack's chart, not its spec as code", sent)
	}
}

func dmSubscriptionCount(t *testing.T, l *liveDaemon) int {
	t.Helper()
	var n int
	if err := l.srv.DB().QueryRowContext(l.ctx, `SELECT COUNT(*) FROM watches WHERE kind = 'dm'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func activeDMThreads(t *testing.T, l *liveDaemon) []string {
	t.Helper()
	rows, err := l.srv.DB().QueryContext(l.ctx, `SELECT subject_id FROM watches WHERE kind = 'dm' AND status = 'active' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var subscribed []string
	for rows.Next() {
		var subjectID string
		if err := rows.Scan(&subjectID); err != nil {
			t.Fatal(err)
		}
		subscribed = append(subscribed, subjectID)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	active, err := l.app.poller.threads.sessionThreads(l.ctx, "D"+testUser, subscribed...)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(active)
	return active
}

func TestDMStatusRepliesUnderAGivenDMMessageOnlyWhenAsked(t *testing.T) {
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
	sent := l.slack.sent()
	if bound["thread_ts"] != dmTS || sent[1].threadTS != dmTS {
		t.Fatalf("bound reply = %v, post %+v, want it under %s", bound, sent[1], dmTS)
	}
	if sent[2].threadTS != "" || later["thread_ts"] != sent[2].ts {
		t.Fatalf("later status = %v, post %+v, want it back at the DM root", later, sent[2])
	}
	if active := activeDMThreads(t, l); !slices.Equal(active, []string{opened["thread_ts"], dmTS, later["thread_ts"]}) {
		t.Fatalf("active DM watches = %v, want each message and the given one", active)
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
		ChannelID string        `json:"channel_id"`
		Messages  []readMessage `json:"messages"`
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
		ChannelID string        `json:"channel_id"`
		Messages  []readMessage `json:"messages"`
	}
	if err := json.Unmarshal(l.do(opThread, map[string]string{"channel_id": "UOTHER", "thread_ts": sent.TS}).Body, &thread); err != nil {
		t.Fatal(err)
	}
	if thread.ChannelID != mpim || len(thread.Messages) != 2 {
		t.Fatalf("thread = %+v, want the group DM thread with both posts", thread)
	}
}

func TestChannelInfoReadsOneChannelOnceAndHoldsIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	want := channelInfo{ID: testChannel, Name: strings.ToLower(testChannel)}
	read := func() channelInfo {
		t.Helper()
		var got channelInfo
		if err := json.Unmarshal(l.do(opChannelInfo, map[string]string{"channel_id": testChannel}).Body, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("channel-info = %+v, want %+v", got, want)
	}

	l.slack.fail("conversations.info", slack.SlackErrorResponse{Err: "ratelimited"})
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("a held channel read again = %+v, want %+v", got, want)
	}
	l.refused(opChannelInfo, map[string]string{"channel_id": "COTHER"}, "ratelimited")
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
		"D" + testUser: {ChannelID: "D" + testUser, Peer: testUser, PeerName: "yasyf", PeerNames: owner, Own: true},
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

	outside := func() (got dmPeerResult) {
		if err := json.Unmarshal(l.do(opDMPeer, map[string]string{"channel_id": "D0THEIRDM"}).Body, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := outside(); !reflect.DeepEqual(got, dmPeerResult{ChannelID: "D0THEIRDM"}) {
		t.Fatalf("dm-peer of a DM the bot cannot see, with no user credential = %+v, want no peer", got)
	}
	l.user.as(testUser).setDM("D0THEIRDM", "UOTHER")
	if got, want := outside(), (dmPeerResult{ChannelID: "D0THEIRDM", Peer: "UOTHER", PeerName: "anubhav", PeerNames: []string{"anubhav"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("dm-peer of a DM the bot cannot see, read as the user = %+v, want %+v", got, want)
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
			"a share names its author",
			slack.Msg{Text: "seeing this?", Attachments: []slack.Attachment{{FromURL: originLink, AuthorName: "Josh", Text: originalText}}},
			"seeing this?\nShared from Josh in <#C0ORIGIN>: " + originalText,
		},
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
			"fallback text the block body repeats with its link",
			slack.Msg{Text: ":x: Deploy cloudrunner-next on AIG failed.", Blocks: slack.Blocks{BlockSet: []slack.Block{
				slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, ":x: Deploy cloudrunner-next on AIG failed. <https://buildkite.com/forge/release/builds/13589|Open the job>\nNext: `tools/ci release resume nascent-brossa`", false, false), nil, nil),
			}}},
			":x: Deploy cloudrunner-next on AIG failed. <https://buildkite.com/forge/release/builds/13589|Open the job>\nNext: `tools/ci release resume nascent-brossa`",
		},
		{
			"a card posted before its text was its title alone, with its data table",
			slack.Msg{Text: ":raising_hand: Tap now? — The fix lands it.", Blocks: slack.Blocks{BlockSet: []slack.Block{
				slack.NewHeaderBlock(slack.NewTextBlockObject(slack.PlainTextType, ":raising_hand: Tap now?", true, false)),
				slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, "The fix lands it.\n*Show me*\nBefore and after one sync.", false, false), nil, nil),
				slack.NewDataTableBlock("— · Before · After").AddRow(slack.NewDataTableRawTextCell("—"), slack.NewDataTableRawTextCell("Before"), slack.NewDataTableRawTextCell("After")).
					AddRow(slack.NewDataTableRawTextCell("Replica ends at"), slack.NewDataTableRawNumberCell(32).WithText("32"), slack.NewDataTableRichTextCell(slack.NewRichTextSection(slack.NewRichTextSectionLinkElement("https://acme.dev/49", "49", nil)))),
			}}},
			":raising_hand: Tap now?\nThe fix lands it.\n*Show me*\nBefore and after one sync.\n[table block]\n| — | Before | After |\n| --- | --- | --- |\n| Replica ends at | 32 | <https://acme.dev/49|49> |",
		},
		{
			"a notification text keeps the piece its blocks leave out",
			slack.Msg{Text: "Deploy failed — see the job", Blocks: slack.Blocks{BlockSet: []slack.Block{
				slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, "see the job <https://buildkite.com/forge/release/builds/13589|13589>", false, false), nil, nil),
			}}},
			"Deploy failed\nsee the job <https://buildkite.com/forge/release/builds/13589|13589>",
		},
		{
			"a pasted table with an empty cell",
			slack.Msg{Blocks: slack.Blocks{BlockSet: []slack.Block{
				slack.NewTableBlock("").AddRow(slack.NewTableRawTextCell("lane"), slack.NewTableRawTextCell("owner")).AddRow(slack.NewTableRawTextCell("replica"), nil),
			}}},
			"[table block]\n| lane | owner |\n| --- | --- |\n| replica |  |",
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

type slowSecondPage struct {
	slackAPI
	pages int
	limit bool
}

func (s *slowSecondPage) GetConversationHistoryContext(ctx context.Context, _ *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	s.pages++
	if s.pages > 1 && s.limit {
		return nil, &slack.RateLimitedError{RetryAfter: time.Minute}
	}
	if s.pages > 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	resp := &slack.GetConversationHistoryResponse{HasMore: true}
	resp.ResponseMetaData.NextCursor = "next"
	resp.Messages = []slack.Message{{Msg: slack.Msg{Timestamp: "1800000002.000000"}}, {Msg: slack.Msg{Timestamp: "1800000001.000000"}}}
	return resp, nil
}

func TestADeepHistoryReadKeepsWhatItHasAtItsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	msgs, partial, err := fetchRecentTopLevel(ctx, &slowSecondPage{}, testChannel, 400)
	if err != nil {
		t.Fatal(err)
	}
	if !partial || len(msgs) != 2 || msgs[0].Timestamp != "1800000002.000000" {
		t.Fatalf("partial = %v, msgs = %+v, want the first page's two messages newest first and marked partial", partial, msgs)
	}
}

func TestADeepHistoryReadKeepsWhatItHasWhenSlackRateLimitsIt(t *testing.T) {
	msgs, partial, err := fetchRecentTopLevel(context.Background(), &slowSecondPage{limit: true}, testChannel, 400)
	if err != nil {
		t.Fatal(err)
	}
	if !partial || len(msgs) != 2 {
		t.Fatalf("partial = %v, msgs = %+v, want the first page marked partial", partial, msgs)
	}
}

func TestAHistoryReadWithNothingAtItsDeadlineFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := fetchRecentTopLevel(ctx, &slowSecondPage{pages: 1}, testChannel, 400); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
}

func TestPartialHistorySaysSo(t *testing.T) {
	var out bytes.Buffer
	renderHistory(&out, historyResult{Partial: true})
	if !strings.Contains(out.String(), "partial") {
		t.Fatalf("rendered %q, want the partial mark", out.String())
	}
}

func TestAThreadReadWithNoChannelReadsYourDMAndAGuessedOneNamesIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	var posted map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]string{"text": "release is rolling out"}).Body, &posted); err != nil {
		t.Fatal(err)
	}
	dm := posted["channel_id"]
	l.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: "1800000090.000000", ThreadTimestamp: posted["ts"], User: testUser, Text: "how far along?"}})

	var res threadResult
	if err := json.Unmarshal(l.do(opThread, map[string]string{"thread_ts": posted["ts"], "since_ts": posted["ts"]}).Body, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || res.Messages[0].Text != "how far along?" {
		t.Fatalf("thread with no channel = %+v, want the reply in your DM", res.Messages)
	}

	l.slack.fail(repliesOf("D0", posted["ts"]), slack.SlackErrorResponse{Err: "channel_not_found"})
	raw, err := json.Marshal(map[string]string{"channel_id": "D0", "thread_ts": posted["ts"]})
	if err != nil {
		t.Fatal(err)
	}
	_, err = invoke(l.ctx, l.deps, testSession, "/any/cwd", opThread, raw)
	if err == nil || !strings.Contains(err.Error(), "channel_not_found") || !strings.Contains(err.Error(), "your DM with the app is "+dm) {
		t.Fatalf("guessed DM read = %v, want channel_not_found naming %s", err, dm)
	}
}

func TestAboutBesideACardSaysToDropAbout(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	card := map[string]any{"status": "needs_you", "title": "Which database?", "waits": "pick-db", "options": []string{"Postgres", "SQLite"}, "recommended": "Postgres"}
	l.refused(opDMStatus, map[string]any{"about": "pick-db", "card": card}, "a card names its card by card.waits, so drop about")
}
