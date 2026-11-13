package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSlackOAuthReadsTheUserTokenAndSlacksRefusal(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		body    string
		team    string
		user    oauthToken
		renewed oauthToken
		code    string
	}{
		"consent": {
			body: `{"ok":true,"team":{"id":"T1"},"authed_user":{"id":"U1","access_token":"xoxe.xoxp-1","refresh_token":"xoxe-1","expires_in":43200}}`,
			team: "T1", user: oauthToken{ID: "U1", AccessToken: "xoxe.xoxp-1", RefreshToken: "xoxe-1", ExpiresIn: 43200},
		},
		"renewal": {
			body:    `{"ok":true,"access_token":"xoxe.xoxp-2","refresh_token":"xoxe-2","expires_in":43200}`,
			renewed: oauthToken{AccessToken: "xoxe.xoxp-2", RefreshToken: "xoxe-2", ExpiresIn: 43200},
		},
		"spent refresh token": {body: `{"ok":false,"error":"invalid_refresh_token"}`, code: "invalid_refresh_token"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var sent url.Values
			client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				raw, _ := io.ReadAll(r.Body)
				sent, _ = url.ParseQuery(string(raw))
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
			})}
			got, err := slackOAuth(client)(context.Background(), url.Values{"code": {"c1"}})
			if tc.code != "" {
				if err == nil || errorCode(err) != tc.code {
					t.Fatalf("oauth = %+v, %v, want Slack's %s", got, err, tc.code)
				}
				return
			}
			if err != nil || got.Team.ID != tc.team || got.AuthedUser != tc.user || got.oauthToken != tc.renewed || sent.Get("code") != "c1" {
				t.Fatalf("oauth = %+v, %v after sending %v", got, err, sent)
			}
		})
	}
}

func oauthFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.id.Config.OAuth.ClientID = testClientID
	if err := saveIdentity(f.ctx, f.db(), f.id); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestAConsentLinkPostedBeforeARestartStillCompletes(t *testing.T) {
	f := oauthFixture(t)
	if err := f.app.askConsent(f.ctx, f.id); !errors.As(err, new(userConsentError)) {
		t.Fatalf("askConsent = %v, want the consent card's refusal", err)
	}
	state := consentLink(t, f.slack).Query().Get("state")

	f.app.user, f.user.consent = f.user.session(), nil
	if err := f.app.resumeConsent(f.ctx); err != nil || f.user.consent == nil {
		t.Fatalf("resumeConsent = %v, want the restarted daemon serving the redirect again", err)
	}
	if err := f.app.askConsent(f.ctx, f.id); !errors.As(err, new(userConsentError)) || len(f.slack.sentTo(f.id.DMChannelID)) != 1 {
		t.Fatalf("askConsent after the restart = %v, want the same card and no second one", err)
	}
	f.user.answer = oauthAccess{OK: true, AuthedUser: oauthToken{ID: f.id.UserID, AccessToken: "xoxe.xoxp-1"}}
	f.user.answer.Team.ID = f.id.TeamID
	if code := redirect(f.user, "code=c1&state="+state); code != http.StatusOK {
		t.Fatalf("redirect after the restart answered %d, want 200", code)
	}
	if tok, ok := f.user.stored(); !ok || tok.AccessToken != "xoxe.xoxp-1" {
		t.Fatalf("stored = %+v, want the consented token", tok)
	}
	if code := redirect(f.user, "code=c1&state="+state); code != http.StatusBadRequest {
		t.Fatalf("a second tap answered %d, want 400 once the consent is spent", code)
	}
}

func TestThePollerRenewsTheUserTokenBeforeItExpires(t *testing.T) {
	f := oauthFixture(t)
	f.user.store(userToken{UserID: f.id.UserID, TeamID: f.id.TeamID, AccessToken: storedUserToken, RefreshToken: "xoxe-1", ExpiresAt: f.now.Add(time.Minute).Unix()})
	f.user.answer = oauthAccess{OK: true, oauthToken: oauthToken{AccessToken: "xoxe.xoxp-renewed", RefreshToken: "xoxe-2", ExpiresIn: 43200}}

	want := f.now.Add(43200*time.Second - userTokenRenewal)
	if next := f.tick(); !next.Equal(want) {
		t.Fatalf("next tick = %v, want %v, 5 minutes before the renewed token expires", next, want)
	}
	if tok, _ := f.user.stored(); tok.AccessToken != "xoxe.xoxp-renewed" || tok.RefreshToken != "xoxe-2" || len(f.user.sentGrants()) != 1 {
		t.Fatalf("stored = %+v after %d grants, want one renewal holding the new refresh token", tok, len(f.user.sentGrants()))
	}
	f.tick()
	if n := len(f.user.sentGrants()); n != 1 {
		t.Fatalf("grants = %d, want no second renewal while the token is fresh", n)
	}
}
