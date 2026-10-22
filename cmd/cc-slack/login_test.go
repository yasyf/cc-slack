package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/store"
)

const (
	cliAccessKey = "AKIDFROMAWSCLI"
	ssoProfile   = "readonly"
	ssoConfig    = `[profile readonly]
sso_session = acme
sso_account_id = 111111111111
sso_role_name = ReadOnlyAccess
region = us-west-2

[sso-session acme]
sso_start_url = https://acme.awsapps.com/start
sso_region = us-west-2
sso_registration_scopes = sso:account:access
`
	awsExportsCredentials = `#!/bin/sh
printf '%s' "$*" > "$0.args"
printf '%s' "$AWS_PROFILE" > "$0.profile"
printf '{"Version":1,"AccessKeyId":"` + cliAccessKey + `","SecretAccessKey":"secret","SessionToken":"session"}'
`
	awsRefreshFails = `#!/bin/sh
echo "Error when retrieving token from sso: Token has expired and refresh failed" >&2
exit 255
`
	testSSMConfig = `[ssm]
region = "us-west-2"
bot_token = "/test/bot-token"
socket_url = "/test/socket-url"
avatar_key_id = "/test/avatar-key-id"
avatar_secret = "/test/avatar-secret"

[avatars]
endpoint = "https://storage.example.com"
bucket = "avatars"
public_base = "https://public.example.com/"
`
)

type fakeSSM struct {
	mu       sync.Mutex
	accessed []string
}

func (f *fakeSSM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.accessed = append(f.accessed, in.Name+" "+r.Header.Get("Authorization"))
	f.mu.Unlock()
	values := map[string]string{
		"/test/bot-token": "xoxb-test", "/test/socket-url": "ws://127.0.0.1:1/live", "/test/avatar-key-id": "r2-key", "/test/avatar-secret": "r2-secret",
	}
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	_ = json.NewEncoder(w).Encode(map[string]any{"Parameter": map[string]string{"Name": in.Name, "Value": values[in.Name]}})
}

func (f *fakeSSM) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.accessed...)
}

func awsSSOShell(t *testing.T, awsScript string) (string, *fakeSSM) {
	t.Helper()
	bin := t.TempDir()
	for name, script := range map[string]string{"aws": awsScript, captHookBinary: "#!/bin/sh\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	awsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(awsDir, "config"), []byte(ssoConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(awsDir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(awsDir, "credentials"))
	t.Setenv("AWS_PROFILE", ssoProfile)
	ssm := &fakeSSM{}
	srv := httptest.NewServer(ssm)
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ENDPOINT_URL_SSM", srv.URL)
	cfg := filepath.Join(awsDir, "cc-slack.toml")
	if err := os.WriteFile(cfg, []byte(testSSMConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(configEnv, cfg)
	return filepath.Join(bin, "aws"), ssm
}

func staleState(t *testing.T) []byte {
	t.Helper()
	st, err := store.Open(context.Background(), statePath(), store.Schema{DDL: `CREATE TABLE identity (id INTEGER PRIMARY KEY CHECK (id = 1), user_id TEXT NOT NULL);`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO identity (id, user_id) VALUES (1, 'UOLD')`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return readState(t, statePath())
}

func readState(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func archives(t *testing.T) []string {
	t.Helper()
	found, err := filepath.Glob(statePath() + ".*.bak")
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func runLogin(t *testing.T, ensure func(context.Context) error) (string, error) {
	t.Helper()
	d := deps()
	d.EnsureCurrent = requireCurrentState(ensure)
	d.ClaudePID = func() int { return testPID }
	c := loginCmd(d)
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--email", "ym@example.com", "--repo", "/repo", "--session", testSession, "--cwd", "/any"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := c.ExecuteContext(ctx)
	return out.String(), err
}

func TestLoginReadsParametersWithTheAWSCLICredentialsAndArchivesTheStaleState(t *testing.T) {
	shortHome(t)
	aws, ssm := awsSSOShell(t, awsExportsCredentials)
	stale := staleState(t)

	out, err := runLogin(t, func(context.Context) error {
		l := launchDaemon(t)
		l.slack.setUser(&slack.User{ID: testUser, Name: "ym", Profile: slack.UserProfile{Email: "ym@example.com", DisplayName: "yasyf"}})
		return nil
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	var who whoami
	if err := json.Unmarshal([]byte(out), &who); err != nil || who.Username != "yasyf (agent)" {
		t.Fatalf("login printed %q (%v), want whoami as yasyf", out, err)
	}
	if args := string(readState(t, aws+".args")); args != "configure export-credentials --format process" {
		t.Fatalf("aws argv = %q", args)
	}
	if profile := string(readState(t, aws+".profile")); profile != ssoProfile {
		t.Fatalf("aws ran with AWS_PROFILE=%q, want %q", profile, ssoProfile)
	}
	requests := ssm.requests()
	if len(requests) != 4 {
		t.Fatalf("SSM requests = %q, want the bot token, the socket url, and the avatar bucket key", requests)
	}
	for _, r := range requests {
		if !strings.Contains(r, "Credential="+cliAccessKey+"/") {
			t.Fatalf("SSM request %q is not signed with the aws CLI's credentials", r)
		}
	}
	backups := archives(t)
	if len(backups) != 1 || !bytes.Equal(readState(t, backups[0]), stale) {
		t.Fatalf("archives = %q, want exactly the stale state.db", backups)
	}
}

func TestFailedLoginLeavesTheStaleStateInPlace(t *testing.T) {
	shortHome(t)
	_, ssm := awsSSOShell(t, awsRefreshFails)
	stale := staleState(t)

	_, err := runLogin(t, func(context.Context) error {
		t.Fatal("login reached the daemon without AWS credentials")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "credential_process") {
		t.Fatalf("login = %v, want the aws CLI's credential failure", err)
	}
	if requests := ssm.requests(); len(requests) != 0 {
		t.Fatalf("SSM requests = %q, want none without credentials", requests)
	}
	if !bytes.Equal(readState(t, statePath()), stale) || len(archives(t)) != 0 {
		t.Fatalf("state.db changed or was archived (archives %q) by a failed login", archives(t))
	}
}

func startedAt(t *testing.T, at time.Time) {
	t.Helper()
	prev := processStarted
	processStarted = at
	t.Cleanup(func() { processStarted = prev })
}

func TestStaleStateAsksForLoginWithoutStartingTheDaemon(t *testing.T) {
	shortHome(t)
	stale := staleState(t)
	startedAt(t, time.Now())
	started := false
	ensure := requireCurrentState(func(context.Context) error {
		started = true
		return nil
	})

	err := ensure(context.Background())
	if !errors.Is(err, errNoIdentity) || started {
		t.Fatalf("ensure on a stale state.db = %v (started %v), want a login prompt before any daemon starts", err, started)
	}
	refusal := fallback(actionWrite, err)
	for _, want := range []string{"run `cc-slack login`", statePath(), "cc-slack " + appVersion} {
		if !strings.Contains(refusal, want) {
			t.Errorf("fallback = %q, want it to name %q", refusal, want)
		}
	}
	if !bytes.Equal(readState(t, statePath()), stale) || len(archives(t)) != 0 {
		t.Fatal("checking the state.db changed or archived it")
	}

	if err := archiveStaleState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ensure(context.Background()); err != nil || !started {
		t.Fatalf("ensure after the archive = %v (started %v), want the daemon started", err, started)
	}
}

func TestStateCreatedAfterTheProcessStartedAsksForARestartNotALogin(t *testing.T) {
	shortHome(t)
	startedAt(t, time.Now().Add(-time.Minute))
	stale := staleState(t)
	started := false
	ensure := requireCurrentState(func(context.Context) error {
		started = true
		return nil
	})

	err := ensure(context.Background())
	if !errors.Is(err, errStaleProcess) || errors.Is(err, errNoIdentity) || started {
		t.Fatalf("ensure on a state.db created after this process started = %v (started %v), want errStaleProcess before any daemon starts", err, started)
	}
	got := fallback(actionWrite, err)
	for _, want := range []string{"older than its state", "restart the session", mcpSend, statePath(), "cc-slack " + appVersion} {
		if !strings.Contains(got, want) {
			t.Errorf("fallback = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "cc-slack login") {
		t.Errorf("fallback = %q, want no login prompt: login archives the newer state", got)
	}
	if !bytes.Equal(readState(t, statePath()), stale) || len(archives(t)) != 0 {
		t.Fatal("checking the state.db changed or archived it")
	}
}

func TestDaemonExitsCleanlyOnAStaleStateWithoutArchivingIt(t *testing.T) {
	shortHome(t)
	stale := staleState(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := serve(ctx); err != nil {
		t.Fatalf("serve on a stale state.db = %v, want a clean exit so launchd does not relaunch it", err)
	}
	if ctx.Err() != nil {
		t.Fatal("serve ran until the deadline instead of refusing the stale state.db")
	}
	if !bytes.Equal(readState(t, statePath()), stale) || len(archives(t)) != 0 {
		t.Fatal("the daemon changed or archived the stale state.db")
	}
}
