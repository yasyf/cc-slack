package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
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
