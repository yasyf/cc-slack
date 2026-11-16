package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/daemon"
)

const (
	searchDateLayout = "2006-01-02"
	searchMaxCount   = 100
)

func (a *app) searchMessages(hc daemon.HandlerCtx, b opBody) daemon.Reply {
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionRead, err))
	}
	query, err := a.searchQuery(hc.Ctx, id, b)
	if err != nil {
		return fail(err.Error())
	}
	user, err := a.userAPI(hc.Ctx, id)
	if err != nil {
		return fail(readFailure(err, id))
	}
	params := slack.NewSearchParameters()
	params.Sort, params.SortDirection, params.Count = "timestamp", "desc", min(max(b.Limit, 1), searchMaxCount)
	if b.Limit <= 0 {
		params.Count = defaultHistoryLimit
	}
	ctx, cancel := context.WithTimeout(hc.Ctx, historyBudget)
	defer cancel()
	found, err := user.SearchMessagesContext(ctx, query, params)
	if err != nil && errorCode(err) == "missing_scope" {
		return fail(fmt.Sprintf("searching Slack as you needs your consent to search: %v", a.consentAgain(hc.Ctx, id)))
	}
	if err != nil {
		return fail(fmt.Sprintf("search Slack as you for %q: %s", query, errorCode(err)))
	}
	out := make([]readMessage, 0, len(found.Matches))
	for _, m := range found.Matches {
		link, _ := parsePermalink(m.Permalink)
		out = append(out, readMessage{
			TS: m.Timestamp, ThreadTS: link.threadTS, User: m.User, UserName: "#" + m.Channel.Name + " " + m.Username,
			Text: m.Text, ChannelID: m.Channel.ID,
		})
	}
	return ok(historyResult{Query: query, Total: found.Total, Messages: out})
}

func (a *app) searchQuery(ctx context.Context, id identity, b opBody) (string, error) {
	var terms []string
	if query := strings.TrimSpace(b.Query); query != "" {
		terms = append(terms, query)
	}
	if b.ChannelID != "" {
		channelID, err := conversationID(ctx, a.newAPI(id.Token), id, b.ChannelID)
		if err != nil {
			return "", fmt.Errorf("search in %s: %w", b.ChannelID, err)
		}
		terms = append(terms, "in:<#"+channelID+">")
	}
	if b.From != "" {
		terms = append(terms, "from:"+searchUser(b.From))
	}
	for _, d := range []struct{ modifier, day string }{{"after", b.After}, {"before", b.Before}} {
		if d.day == "" {
			continue
		}
		if _, err := time.Parse(searchDateLayout, d.day); err != nil {
			return "", fmt.Errorf("search %s takes a day as YYYY-MM-DD, not %q", d.modifier, d.day)
		}
		terms = append(terms, d.modifier+":"+d.day)
	}
	return strings.Join(terms, " "), nil
}

func searchUser(from string) string {
	from = strings.TrimPrefix(from, "@")
	if slackUserID.MatchString(from) {
		return "<@" + from + ">"
	}
	return "@" + from
}
