package main

import (
	"context"
	"database/sql"
	"slices"

	"github.com/slack-go/slack"
)

const takenKept = 512

type pushedMessage struct {
	key cursorKey
	m   slack.Message
}

func nudgedKey(n messageNudge) cursorKey {
	if n.ThreadTS != "" && n.ThreadTS != n.TS {
		return cursorKey{n.Channel, n.ThreadTS}
	}
	return cursorKey{n.Channel, ""}
}

func (p *poller) takeAllPushed(ctx context.Context, db *sql.DB, api slackAPI, id identity, groups []cursorGroup) {
	p.mu.Lock()
	pushed := p.pushed
	p.pushed = nil
	p.mu.Unlock()
	for _, pm := range pushed {
		i := slices.IndexFunc(groups, func(g cursorGroup) bool { return g.key == pm.key })
		at := id.at(pm.key.channelID)
		if i < 0 || pm.key.channelID != at.DMChannelID || !at.fromOperator(pm.m) {
			continue
		}
		if err := p.take(ctx, db, api, at, groups[i], pm.m); err != nil {
			p.log.Printf("pushed %s/%s: route it from the nudge: %v; the thread read routes it instead", pm.key.channelID, pm.m.Timestamp, err)
			continue
		}
		p.markTaken(pm.key.channelID, pm.m.Timestamp)
		p.log.Printf("pushed %s/%s: routed from the nudge with no Slack read", pm.key.channelID, pm.m.Timestamp)
	}
}

func (p *poller) markTaken(channelID, ts string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.taken == nil || len(p.taken) >= takenKept {
		p.taken = map[string]bool{}
	}
	p.taken[channelID+"/"+ts] = true
}

func (p *poller) tookPushed(channelID, ts string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.taken[channelID+"/"+ts]
}
