package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
)

const (
	consentPath       = "/cc-slack/oauth"
	consentAddr       = "127.0.0.1:38427"
	slackAuthorizeURL = "https://slack.com/oauth/v2/authorize"
	slackOAuthAccess  = "https://slack.com/api/oauth.v2.access"
	userTokenRenewal  = 5 * time.Minute
)

var (
	consentRedirect = "http://localhost:38427" + consentPath
	userScopes      = []string{"channels:history", "files:read", "groups:history", "groups:read", "groups:write", "im:history", "im:read", "mpim:history", "mpim:read"}
	errNoUserToken  = errors.New("cc-slack holds no Slack user token on this machine")
	errNoClientID   = errors.New("the cc-slack config names no [oauth] client_id, the Slack app's client ID its consent link carries")
)

func userTokenPath() string { return filepath.Join(appPaths().StateDir(), "user-token.json") }

type userToken struct {
	UserID       string `json:"user_id"`
	TeamID       string `json:"team_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
}

func (t userToken) due(now time.Time) bool {
	return t.ExpiresAt != 0 && now.Add(userTokenRenewal).Unix() >= t.ExpiresAt
}

type oauthToken struct {
	ID           string `json:"id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (o oauthToken) userToken(userID, teamID string, now time.Time) userToken {
	tok := userToken{UserID: userID, TeamID: teamID, AccessToken: o.AccessToken, RefreshToken: o.RefreshToken}
	if o.ExpiresIn > 0 {
		tok.ExpiresAt = now.Unix() + o.ExpiresIn
	}
	return tok
}

type oauthAccess struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Team  struct {
		ID string `json:"id"`
	} `json:"team"`
	AuthedUser oauthToken `json:"authed_user"`
	oauthToken
}

type userSession struct {
	mu      sync.Mutex
	path    string
	oauth   func(ctx context.Context, form url.Values) (oauthAccess, error)
	api     func(token string) slackAPI
	listen  func(ctx context.Context, h http.Handler) error
	serving bool
	pending *pendingConsent
}

type pendingConsent struct {
	state    string
	verifier string
	id       identity
	ts       string
}

func newUserSession() *userSession {
	client := &http.Client{Timeout: 30 * time.Second}
	return &userSession{path: userTokenPath(), oauth: slackOAuth(client), api: newSlackAPI, listen: listenConsent}
}

func listenConsent(ctx context.Context, h http.Handler) error {
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", consentAddr)
	if err != nil {
		return fmt.Errorf("serve the consent redirect on %s: %w", consentAddr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return nil
}

func slackOAuth(client *http.Client) func(ctx context.Context, form url.Values) (oauthAccess, error) {
	return func(ctx context.Context, form url.Values) (oauthAccess, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackOAuthAccess, strings.NewReader(form.Encode()))
		if err != nil {
			return oauthAccess{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req) //nolint:gosec // G704: a fixed slack.com URL
		if err != nil {
			return oauthAccess{}, fmt.Errorf("oauth.v2.access: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out oauthAccess
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return oauthAccess{}, fmt.Errorf("oauth.v2.access answered %s: %w", resp.Status, err)
		}
		if !out.OK {
			return oauthAccess{}, slackCallError{method: "oauth.v2.access", err: slack.SlackErrorResponse{Err: out.Error}}
		}
		return out, nil
	}
}

func (u *userSession) read() (userToken, error) {
	raw, err := os.ReadFile(u.path)
	if errors.Is(err, os.ErrNotExist) {
		return userToken{}, errNoUserToken
	}
	if err != nil {
		return userToken{}, err
	}
	var tok userToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		return userToken{}, fmt.Errorf("read %s: %w", u.path, err)
	}
	return tok, nil
}

func (u *userSession) save(tok userToken) error {
	tmp := u.path + ".tmp"
	if err := os.WriteFile(tmp, mustJSON(tok), 0o600); err != nil {
		return fmt.Errorf("write the Slack user token: %w", err)
	}
	return os.Rename(tmp, u.path)
}

func (u *userSession) forget() error {
	if err := os.Remove(u.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type userConsentError struct{ permalink string }

func (e userConsentError) Error() string {
	return fmt.Sprintf("cc-slack reads as you only with your one-time consent: tap Allow on the card it posted to your DM with the app (%s) on the Mac that runs cc-slack, then retry", e.permalink)
}

func (a *app) renewUserToken(ctx context.Context, id identity, tok userToken) (userToken, error) {
	got, err := a.user.oauth(ctx, url.Values{"client_id": {id.Config.OAuth.ClientID}, "grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}})
	if err != nil && errorCode(err) == "invalid_refresh_token" {
		if err := a.user.forget(); err != nil {
			return userToken{}, err
		}
		return userToken{}, a.askConsent(ctx, id)
	}
	if err != nil {
		return userToken{}, fmt.Errorf("renew the Slack user token: %w", err)
	}
	renewed := got.userToken(id.UserID, id.TeamID, a.now())
	if err := a.user.save(renewed); err != nil {
		return userToken{}, err
	}
	return renewed, nil
}

func readAsUser(id identity, err error) bool {
	return id.Config.OAuth.enabled() && err != nil && (errorCode(err) == "channel_not_found" || errorCode(err) == "not_in_channel")
}

func staleSession(err error) bool {
	return errorCode(err) == "invalid_auth" || errorCode(err) == "token_expired" || errorCode(err) == "token_revoked"
}

func (a *app) askConsent(ctx context.Context, id identity) error {
	if c := a.user.pending; c != nil {
		return userConsentError{permalink: id.permalink(id.DMChannelID, c.ts, "")}
	}
	if !id.Config.OAuth.enabled() {
		return errNoClientID
	}
	if !a.user.serving {
		if err := a.user.listen(ctx, http.HandlerFunc(a.serveConsent)); err != nil {
			return err
		}
		a.user.serving = true
	}
	c := &pendingConsent{state: randomToken(), verifier: randomToken(), id: id}
	challenge := sha256.Sum256([]byte(c.verifier))
	link := slackAuthorizeURL + "?" + url.Values{
		"client_id":             {id.Config.OAuth.ClientID},
		"user_scope":            {strings.Join(userScopes, ",")},
		"redirect_uri":          {consentRedirect},
		"state":                 {c.state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"team":                  {id.TeamID},
	}.Encode()
	text := fmt.Sprintf("*cc-slack needs your one-time consent to act in Slack as you*\nIt reads conversations the app is not in, such as your DMs, "+
		"and invites the app to private channels you are in. Open this link on the Mac that runs cc-slack, since Slack hands your answer to it at localhost: "+
		"<%s|Allow cc-slack>", link)
	_, ts, err := a.newAPI(id.Token).PostMessageContext(ctx, id.DMChannelID, id.postOptions(text, "")...)
	if err != nil {
		return fmt.Errorf("post the consent card: %w", err)
	}
	c.ts = ts
	a.user.pending = c
	return userConsentError{permalink: id.permalink(id.DMChannelID, ts, "")}
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *app) serveConsent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	a.user.mu.Lock()
	defer a.user.mu.Unlock()
	c := a.user.pending
	if r.URL.Path != consentPath || c == nil || q.Get("state") != c.state {
		http.Error(w, "cc-slack holds no consent for this link; retry the read for a fresh one", http.StatusBadRequest)
		return
	}
	a.user.pending = nil
	outcome, status := a.finishConsent(r.Context(), c, q)
	if _, _, _, err := a.newAPI(c.id.Token).UpdateMessageContext(r.Context(), c.id.DMChannelID, c.ts, c.id.postOptions(outcome, "")...); err != nil {
		a.poller.log.Printf("edit the consent card %s: %v", c.ts, err)
	}
	http.Error(w, outcome, status)
}

func (a *app) finishConsent(ctx context.Context, c *pendingConsent, q url.Values) (string, int) {
	if e := q.Get("error"); e != "" {
		return fmt.Sprintf("Not allowed: Slack answered %s, so cc-slack still cannot read as you", e), http.StatusForbidden
	}
	got, err := a.user.oauth(ctx, url.Values{"client_id": {c.id.Config.OAuth.ClientID}, "code": {q.Get("code")}, "code_verifier": {c.verifier}, "redirect_uri": {consentRedirect}})
	if err != nil {
		return fmt.Sprintf("Not allowed: trading Slack's code for a token failed (%v)", err), http.StatusBadGateway
	}
	if got.AuthedUser.ID != c.id.UserID || got.Team.ID != c.id.TeamID {
		return fmt.Sprintf("Not allowed: this consent is %s in %s, not %s in %s", got.AuthedUser.ID, got.Team.ID, c.id.UserID, c.id.TeamID), http.StatusForbidden
	}
	if err := a.user.save(got.AuthedUser.userToken(c.id.UserID, c.id.TeamID, a.now())); err != nil {
		return fmt.Sprintf("Not allowed: saving the token failed (%v)", err), http.StatusInternalServerError
	}
	a.poller.log.Printf("%s allowed cc-slack to read Slack as them", c.id.UserID)
	return "Allowed: cc-slack now reads Slack as you on this Mac. You can close this tab.", http.StatusOK
}
