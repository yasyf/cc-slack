package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

const (
	mcpTokenFile       = ".slack-mcp-tokens.json" //nolint:gosec // G101: a file name, not a credential
	mcpKeychainService = "slack-mcp-server"

	cookiesyncBinary   = "cookiesync"
	slackWebOrigin     = "https://app.slack.com"
	slackLocalConfig   = "localConfig_v2"
	slackSessionCookie = "d"
	slackCookieDomain  = ".slack.com"
)

var (
	errNoUserCredential = fmt.Errorf("no Slack MCP credential in ~/%s or the %s Keychain item", mcpTokenFile, mcpKeychainService)
	errChromeSignedOut  = fmt.Errorf("the user's Slack session expired and Chrome is signed out of Slack: sign in at %s in Chrome, then retry", slackWebOrigin)
)

func staleSession(err error) bool {
	return slices.Contains([]string{"invalid_auth", "token_expired", "token_revoked"}, errorCode(err))
}

type userCredential struct {
	Token  string `json:"SLACK_TOKEN"`
	Cookie string `json:"SLACK_COOKIE"`
}

type userSession struct {
	read       func(ctx context.Context) (userCredential, error)
	save       func(ctx context.Context, cred userCredential) error
	cookiesync commandRunner
	api        func(cred userCredential) slackAPI
}

func mcpUserSession() userSession {
	return userSession{read: readUserCredential, save: saveUserCredential, cookiesync: execCommand, api: cookieAPI}
}

type cookieClient struct {
	cookie string
	http   *http.Client
}

func (c cookieClient) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Cookie", slackSessionCookie+"="+c.cookie)
	return c.http.Do(req) //nolint:gosec // G704: slack-go builds the request to slack.com
}

func cookieAPI(cred userCredential) slackAPI {
	client := cookieClient{cookie: cred.Cookie, http: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	return newSlackClient(cred.Token, slack.APIURL, client)
}

func readUserCredential(ctx context.Context) (userCredential, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return userCredential{}, err
	}
	var cred userCredential
	raw, err := os.ReadFile(filepath.Join(home, mcpTokenFile))
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &cred); err != nil {
			return userCredential{}, fmt.Errorf("read ~/%s: %w", mcpTokenFile, err)
		}
	case errors.Is(err, os.ErrNotExist):
		cred.Token, cred.Cookie = keychainSecret(ctx, "token"), keychainSecret(ctx, "cookie")
	default:
		return userCredential{}, err
	}
	if cred.Token == "" || cred.Cookie == "" {
		return userCredential{}, errNoUserCredential
	}
	return cred, nil
}

func saveUserCredential(ctx context.Context, cred userCredential) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, mcpTokenFile)
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		return rewriteTokenFile(path, raw, cred)
	case errors.Is(err, os.ErrNotExist):
		return saveKeychainSecrets(ctx, cred)
	default:
		return err
	}
}

func rewriteTokenFile(path string, raw []byte, cred userCredential) error {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("read ~/%s: %w", mcpTokenFile, err)
	}
	doc["SLACK_TOKEN"], doc["SLACK_COOKIE"], doc["updated_at"] = cred.Token, cred.Cookie, time.Now().UTC().Format(time.RFC3339)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), mcpTokenFile+".*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func saveKeychainSecrets(ctx context.Context, cred userCredential) error {
	oldToken := keychainSecret(ctx, "token")
	if err := saveKeychainSecret(ctx, "token", cred.Token); err != nil {
		return err
	}
	if err := saveKeychainSecret(ctx, "cookie", cred.Cookie); err != nil {
		return errors.Join(err, saveKeychainSecret(context.WithoutCancel(ctx), "token", oldToken))
	}
	return nil
}

func saveKeychainSecret(ctx context.Context, account, secret string) error {
	save := exec.CommandContext(ctx, "/usr/bin/security", "-i")
	save.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n", mcpKeychainService, account, strconv.Quote(secret)))
	if out, err := save.CombinedOutput(); err != nil {
		return fmt.Errorf("save %s in the %s Keychain item: %w: %s", account, mcpKeychainService, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func keychainSecret(ctx context.Context, account string) string {
	find := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", mcpKeychainService, "-a", account, "-w") //nolint:gosec // G204: argv to the security binary, never a shell
	out, err := find.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

type chromeStorageState struct {
	Cookies []struct {
		Name   string `json:"name"`
		Domain string `json:"domain"`
		Value  string `json:"value"`
	} `json:"cookies"`
	Origins []struct {
		Origin       string `json:"origin"`
		LocalStorage []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"localStorage"`
	} `json:"origins"`
}

func chromeUserCredential(ctx context.Context, cookiesync commandRunner, teamID string) (userCredential, error) {
	out, err := cookiesync(ctx, cookiesyncBinary, "cookies", slackWebOrigin, "--browser", "chrome", "--format", "playwright")
	if err != nil {
		return userCredential{}, fmt.Errorf("the user's Slack session expired, and reading Chrome's through cookiesync failed: %w", err)
	}
	var state chromeStorageState
	if err := json.Unmarshal(out, &state); err != nil {
		return userCredential{}, fmt.Errorf("parse cookiesync's storage state: %w", err)
	}
	var cred userCredential
	for _, c := range state.Cookies {
		if c.Name == slackSessionCookie && c.Domain == slackCookieDomain {
			cred.Cookie = c.Value
		}
	}
	for _, o := range state.Origins {
		for _, e := range o.LocalStorage {
			if o.Origin != slackWebOrigin || e.Name != slackLocalConfig {
				continue
			}
			var config struct {
				Teams map[string]struct {
					Token string `json:"token"`
				} `json:"teams"`
			}
			if err := json.Unmarshal([]byte(e.Value), &config); err != nil {
				return userCredential{}, fmt.Errorf("parse Slack's %s: %w", slackLocalConfig, err)
			}
			cred.Token = config.Teams[teamID].Token
		}
	}
	if cred.Token == "" || cred.Cookie == "" {
		return userCredential{}, errChromeSignedOut
	}
	return cred, nil
}
