package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

const docsPatience = 30 * time.Second

// DocCardsContext asks the configured doc-cards service to draw a post's Work Object cards: Slack's
// crawler cannot read a private docs site, and no app gets link_shared for a bot's own links.
func (c *apiClient) DocCardsContext(ctx context.Context, serviceURL, channelID, text string) ([]slack.WorkObjectEntity, error) {
	ctx, cancel := context.WithTimeout(ctx, docsPatience)
	defer cancel()
	body, err := json.Marshal(map[string]string{"channel": channelID, "text": text})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serviceURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req) //nolint:gosec // G704: the request goes to the doc-cards URL the config names
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", serviceURL, resp.Status)
	}
	var answer struct {
		Entities []slack.WorkObjectEntity `json:"entities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return nil, err
	}
	return answer.Entities, nil
}

func docEntities(ctx context.Context, api slackAPI, logger *log.Logger, cfg docCardsConfig, channelID, text string) []slack.WorkObjectEntity {
	if !cfg.enabled() || !strings.Contains(text, cfg.Link) {
		return nil
	}
	entities, err := api.DocCardsContext(ctx, cfg.URL, channelID, text)
	if err != nil {
		logger.Printf("post to %s goes out without its design-doc cards: %v", channelID, err)
		return nil
	}
	return entities
}

func docCards(ctx context.Context, api slackAPI, logger *log.Logger, cfg docCardsConfig, channelID, text string) []slack.MsgOption {
	return entitiesOption(docEntities(ctx, api, logger, cfg, channelID, text))
}

// Slack keeps one metadata per message, so a post carrying PR cards puts its doc cards among them.
func withDocCards(ctx context.Context, api slackAPI, logger *log.Logger, cfg docCardsConfig, channelID, text string, prs []slack.WorkObjectEntity) []slack.MsgOption {
	if len(prs) == 0 {
		return nil
	}
	return entitiesOption(append(docEntities(ctx, api, logger, cfg, channelID, text), prs...))
}
