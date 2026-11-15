package main

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/slack-go/slack"
	spawnllm "github.com/yasyf/spawnllm/go"
)

type addressee int

const (
	addressedToPeople addressee = iota
	addressedUnclear
	addressedToAgents
)

const (
	toAgentsAt     = 0.7
	toPeopleAt     = 0.3
	linesAround    = 8
	lineRunes      = 500
	messageRunes   = 2000
	addressTimeout = 5 * time.Second
	ownAgents      = "the operator's agents, posting as the bot"
	whereThread    = "a reply in a thread"
	whereTop       = "a new message at the top of the channel"
)

var addressRules = []string{
	"The operator runs AI agents that read this channel and post in it as the bot; teammates run their own agents, which post as the same bot.",
	"The operator's message is written to their agents when it names @bot, or replies to a post the operator's agents made with a question, request or instruction for them, such as asking why, asking them to rerun, check, fix or explain something.",
	"The operator's message is written to people when it replies to, jokes with, praises, thanks or plans with a teammate, or answers a teammate's question, even in a thread where the agents posted.",
}

var addressQuestions = map[string]spawnllm.Question{
	"to_agents": spawnllm.Binary{
		Instructions: "Following `rules`, did the operator write `message` to their agents rather than to the people in `around`?",
		Yes:          "Written to the operator's agents: it names @bot, answers or directs a post of the operator's agents, or asks the agents to act",
		No:           "Written to people: a reply, joke, praise, thanks or plan for a teammate, or talk between people about the work",
	},
}

type addressState struct {
	Rules   []string `json:"rules"`
	Where   string   `json:"where"`
	Message string   `json:"message"`
	Around  []spoken `json:"around,omitempty"`
}

type spoken struct {
	From string `json:"from"`
	Text string `json:"text"`
}

func (a addressee) String() string {
	return [...]string{"content", "unclear", "steers"}[a]
}

func (p *poller) addressee(ctx context.Context, db *sql.DB, api slackAPI, id identity, channelID string, m slack.Message) (addressee, string, error) {
	if !id.fromOperator(m) {
		return addressedToPeople, "not the operator's message", nil
	}
	if channelID == id.DMChannelID {
		return addressedToAgents, "the operator's DM", nil
	}
	state, err := p.addressState(ctx, db, api, id, channelID, m)
	if err != nil {
		return addressedToPeople, "", err
	}
	to, why := jevVerdict(p.decide(ctx, state, addressQuestions, spawnllm.DecideOpts{Timeout: addressTimeout}))
	return to, why, nil
}

func jevVerdict(decision spawnllm.Decision, failed error) (addressee, string) {
	if failed != nil {
		return addressedUnclear, "Jev failed: " + failed.Error()
	}
	answer, answered := decision.Answers["to_agents"].(spawnllm.BinaryAnswer)
	if !answered {
		return addressedUnclear, "Jev declined to answer"
	}
	why := fmt.Sprintf("Jev puts it at %.2f written to the bot", answer.PYes)
	switch {
	case answer.PYes >= toAgentsAt:
		return addressedToAgents, why
	case answer.PYes <= toPeopleAt:
		return addressedToPeople, why
	default:
		return addressedUnclear, why
	}
}

func (p *poller) addressState(ctx context.Context, db *sql.DB, api slackAPI, id identity, channelID string, m slack.Message) (addressState, error) {
	state := addressState{Rules: addressRules, Where: whereThread, Message: clipRunes(p.spokenText(ctx, api, id, m), messageRunes)}
	if operatorRoot(id, m) {
		state.Where = whereTop
	}
	around, err := p.before(ctx, api, id, channelID, m)
	if err != nil {
		return addressState{}, fmt.Errorf("read the messages before %s/%s: %w", channelID, m.Timestamp, err)
	}
	if len(around) > linesAround+1 {
		around = slices.Concat(around[:1], around[len(around)-linesAround:])
	}
	for _, o := range around {
		from, err := p.speaker(ctx, db, api, id, channelID, o)
		if err != nil {
			return addressState{}, err
		}
		state.Around = append(state.Around, spoken{From: from, Text: clipRunes(p.spokenText(ctx, api, id, o), lineRunes)})
	}
	return state, nil
}

func (p *poller) before(ctx context.Context, api slackAPI, id identity, channelID string, m slack.Message) ([]slack.Message, error) {
	if !operatorRoot(id, m) {
		replies, err := p.cache.replies(ctx, api, channelID, m.ThreadTimestamp)
		return slices.DeleteFunc(slices.Clone(replies), func(o slack.Message) bool { return !tsLess(o.Timestamp, m.Timestamp) }), err
	}
	resp, err := api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
		ChannelID: channelID, Latest: m.Timestamp, Limit: linesAround, IncludeAllMetadata: true,
	})
	if err != nil {
		return nil, err
	}
	return sortByTS(resp.Messages), nil
}

func (p *poller) speaker(ctx context.Context, db *sql.DB, api slackAPI, id identity, channelID string, m slack.Message) (string, error) {
	switch {
	case id.botPosted(m):
		own, err := ownPost(ctx, db, channelID, m.Timestamp)
		if err != nil {
			return "", err
		}
		if own {
			return ownAgents, nil
		}
		return "a teammate's agents, posting as the bot", nil
	case id.fromClaude(m):
		return "the operator's agents, posting as the operator", nil
	case m.BotID != "":
		return "the app " + p.cache.name(ctx, api, m), nil
	case m.User == id.UserID:
		return "the operator", nil
	default:
		return p.cache.name(ctx, api, m) + ", a teammate", nil
	}
}

var slackMention = regexp.MustCompile(`<(@[UW][A-Z0-9]+|![^<>|]+)(?:\|([^<>]*))?>`)

func (p *poller) spokenText(ctx context.Context, api slackAPI, id identity, m slack.Message) string {
	return slackMention.ReplaceAllStringFunc(messageText(m), func(mention string) string {
		parts := slackMention.FindStringSubmatch(mention)
		switch target, label := parts[1], parts[2]; {
		case target == "@"+id.BotUserID:
			return "@bot"
		case target == "@"+id.UserID:
			return "@operator"
		case strings.HasPrefix(target, "@"):
			return "@" + p.cache.name(ctx, api, slack.Message{Msg: slack.Msg{User: target[1:]}})
		case label != "":
			return "@" + strings.TrimPrefix(label, "@")
		default:
			return "@" + strings.TrimPrefix(target, "!")
		}
	})
}

type addressReading struct {
	TS  string `json:"ts"`
	To  string `json:"to"`
	Why string `json:"why"`
}

func (a *app) readAddressed(ctx context.Context, db *sql.DB, id identity, channelID, threadTS, ts string) (*addressReading, error) {
	api := a.newAPI(id.Token)
	msgs, err := a.cache.repliesHolding(ctx, api, channelID, threadTS, ts)
	if err != nil {
		return nil, err
	}
	at := slices.IndexFunc(msgs, func(m slack.Message) bool { return m.Timestamp == ts })
	if at < 0 {
		return nil, fmt.Errorf("thread %s/%s holds no message %s", channelID, threadTS, ts)
	}
	to, why, err := a.poller.addressee(ctx, db, api, id, channelID, msgs[at])
	if err != nil {
		return nil, err
	}
	return &addressReading{TS: ts, To: to.String(), Why: why}, nil
}
