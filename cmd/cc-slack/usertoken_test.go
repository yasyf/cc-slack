package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUserCredentialReadsTheSlackMCPTokenFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, mcpTokenFile), []byte(`{"SLACK_TOKEN":"xoxc-1","SLACK_COOKIE":"xoxd-%2F1","updated_at":"2026-10-08"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cred, err := readUserCredential(context.Background())
	if err != nil || cred != (userCredential{Token: "xoxc-1", Cookie: "xoxd-%2F1"}) {
		t.Fatalf("credential = %+v, %v", cred, err)
	}
	if err := os.WriteFile(filepath.Join(home, mcpTokenFile), []byte(`{"SLACK_TOKEN":"xoxc-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readUserCredential(context.Background()); !errors.Is(err, errNoUserCredential) {
		t.Fatalf("credential without a cookie: %v, want errNoUserCredential", err)
	}
}

func TestCookieClientSendsTheSessionCookie(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Header.Get("Cookie") }))
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := cookieClient{cookie: "xoxd-%2F1", http: srv.Client()}.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got != "d=xoxd-%2F1" {
		t.Fatalf("Cookie = %q, want the d cookie", got)
	}
}

func TestChromeUserCredentialTakesTheTeamsTokenAndTheSessionCookie(t *testing.T) {
	t.Parallel()
	cookiesync := func(state string, err error) commandRunner {
		return func(context.Context, string, ...string) ([]byte, error) { return []byte(state), err }
	}
	for name, tc := range map[string]struct {
		state string
		err   error
		want  userCredential
		fail  string
	}{
		"signed in":                 {state: chromeStorageJSON("T1", "xoxc-t1", "xoxd-1"), want: userCredential{Token: "xoxc-t1", Cookie: "xoxd-1"}},
		"signed in to another team": {state: chromeStorageJSON("T2", "xoxc-t2", "xoxd-1"), fail: errChromeSignedOut.Error()},
		"no session cookie":         {state: chromeStorageJSON("T1", "xoxc-t1", ""), fail: errChromeSignedOut.Error()},
		"no Slack storage":          {state: `{"cookies": [], "origins": []}`, fail: errChromeSignedOut.Error()},
		"cookiesync refused":        {err: errors.New("no peer has a live session to approve consent"), fail: "reading Chrome's through cookiesync failed: no peer has a live session to approve consent"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cred, err := chromeUserCredential(context.Background(), cookiesync(tc.state, tc.err), "T1")
			if tc.fail != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fail) {
					t.Fatalf("credential = %+v, %v, want an error naming %q", cred, err, tc.fail)
				}
				return
			}
			if err != nil || cred != tc.want {
				t.Fatalf("credential = %+v, %v, want %+v", cred, err, tc.want)
			}
		})
	}
}

func TestSaveUserCredentialRewritesTheSlackMCPTokenFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, mcpTokenFile)
	if err := os.WriteFile(path, []byte(`{"SLACK_TOKEN":"xoxc-old","SLACK_COOKIE":"xoxd-old","last_auto_heal_error":"no_slack_cookie_row"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveUserCredential(context.Background(), userCredential{Token: "xoxc-new", Cookie: "xoxd-%2Fnew"}); err != nil {
		t.Fatal(err)
	}
	cred, err := readUserCredential(context.Background())
	if err != nil || cred != (userCredential{Token: "xoxc-new", Cookie: "xoxd-%2Fnew"}) {
		t.Fatalf("credential after save = %+v, %v", cred, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["last_auto_heal_error"] != "no_slack_cookie_row" || doc["updated_at"] == nil {
		t.Fatalf("token file = %s, want the Slack MCP's own fields kept and updated_at set", raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, want 0600", info.Mode())
	}
}
