package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.toml")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigFilesPreferTheFlagThenTheEnvThenTheCheckoutOverHomeThenHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := t.TempDir()
	t.Setenv(configEnv, "")
	user := configFile{filepath.Join(home, ".config", "cc-slack", "config.toml"), false}

	if files, err := configFiles("", repo); err != nil || !slices.Equal(files, []configFile{user}) {
		t.Fatalf("configFiles with nothing set = %+v, %v; want the optional home config", files, err)
	}
	checkout := writeConfig(t, filepath.Join(repo, ".claude"), "")
	if err := os.Rename(checkout, filepath.Join(repo, repoConfig)); err != nil {
		t.Fatal(err)
	}
	if files, _ := configFiles("", repo); !slices.Equal(files, []configFile{{filepath.Join(repo, repoConfig), true}, user}) {
		t.Fatalf("configFiles with a checkout config = %+v; want the checkout's %s over the home config", files, repoConfig)
	}
	t.Setenv(configEnv, "/env/cc-slack.toml")
	if files, _ := configFiles("", repo); !slices.Equal(files, []configFile{{"/env/cc-slack.toml", true}}) {
		t.Fatalf("configFiles with $%s = %+v; want the env path alone", configEnv, files)
	}
	if files, _ := configFiles("/flag.toml", repo); !slices.Equal(files, []configFile{{"/flag.toml", true}}) {
		t.Fatalf("configFiles with --config = %+v; want the flag alone", files)
	}
}

func TestALoginFromACheckoutKeepsEveryHomeSettingTheCheckoutLeavesOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(configEnv, "")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "draw-card"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	writeConfig(t, filepath.Join(home, ".config", "cc-slack"), `[ssm]
profile = "mine"
region = "us-east-1"
bot_token = "/mine/bot-token"
socket_url = "/mine/socket-url"
picture_key_id = "/mine/picture-key"
picture_secret = "/mine/picture-secret"

[pictures]
endpoint = "https://storage.example.com"
bucket = "pictures"

[links]
tailnet = "example.ts.net"

[pr_links]
repos = ["me/mine"]
card_command = ["draw-card"]

[[permissions.clis]]
program = "my-post"
verbs = { send = "post" }
flags = ["loud"]
`)
	repo := t.TempDir()
	checkout := writeConfig(t, filepath.Join(repo, ".claude"), `[ssm]
region = "us-west-2"
bot_token = "/team/bot-token"
socket_url = "/team/socket-url"

[pr_links]
repos = ["acme/app", "acme/lib"]

[[permissions.clis]]
program = "team-post"
verbs = { edit = "edit" }
`)
	if err := os.Rename(checkout, filepath.Join(repo, repoConfig)); err != nil {
		t.Fatal(err)
	}

	_, c, err := readConfig(context.Background(), sessionFlags{}, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	wantSSM := ssmSource{Profile: "mine", Region: "us-west-2", BotToken: "/team/bot-token", SocketURL: "/team/socket-url", PictureKeyID: "/mine/picture-key", PictureSecret: "/mine/picture-secret"}
	if c.SSM == nil || *c.SSM != wantSSM {
		t.Fatalf("[ssm] = %+v, want the checkout's keys over the home file's %+v", c.SSM, wantSSM)
	}
	if !slices.Equal(c.PRLinks.CardCommand, []string{filepath.Join(bin, "draw-card")}) || !slices.Equal(c.PRLinks.Repos, []string{"acme/app", "acme/lib"}) {
		t.Fatalf("[pr_links] = %+v, want the home card_command beside the checkout's repos", c.PRLinks)
	}
	if c.Pictures.Bucket != "pictures" || c.Links.Tailnet != "example.ts.net" {
		t.Fatalf("pictures %+v, links %+v; want the home file's sections the checkout leaves out", c.Pictures, c.Links)
	}
	if len(c.Permissions.CLIs) != 1 || c.Permissions.CLIs[0].Program != "team-post" || len(c.Permissions.CLIs[0].Verbs) != 1 || c.Permissions.CLIs[0].Flags != nil {
		t.Fatalf("[[permissions.clis]] = %+v, want the checkout's array whole", c.Permissions.CLIs)
	}

	_, c, err = readConfig(context.Background(), sessionFlags{}, repo, filepath.Join(repo, repoConfig))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.PRLinks.CardCommand) != 0 || c.Pictures.enabled() || c.SSM.Profile != "" {
		t.Fatalf("config named by --config = %+v, want that file alone", c)
	}

	if err := os.WriteFile(filepath.Join(home, ".config", "cc-slack", "config.toml"), []byte("[pictures]\nendpoint = \"https://s\"\nbucket = \"p\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readConfig(context.Background(), sessionFlags{}, repo, ""); err == nil || !strings.Contains(err.Error(), filepath.Join(repo, repoConfig)+" over ") || !strings.Contains(err.Error(), "picture_key_id") {
		t.Fatalf("an invalid layered config = %v, want it refused naming both files and the missing keys", err)
	}
}

func TestLoadConfigTreatsAMissingDefaultAsEmptyAndAMissingNamedFileAsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.toml")
	if c, err := loadConfig(configFile{missing, false}); err != nil || c.SSM != nil || c.Avatars.enabled() || c.DocCards.enabled() {
		t.Fatalf("loadConfig of a missing default = %+v, %v; want the empty config", c, err)
	}
	if _, err := loadConfig(configFile{missing, true}); err == nil {
		t.Fatal("loadConfig of a missing named file succeeded, want an error")
	}
}

func TestLoadConfigReadsEverySection(t *testing.T) {
	path := writeConfig(t, t.TempDir(), `[ssm]
region = "us-east-1"
bot_token = "/slack/bot-token"
socket_url = "/slack/socket-url"
avatar_key_id = "/slack/avatar-key"
avatar_secret = "/slack/avatar-secret"

[avatars]
endpoint = "https://storage.example.com"
bucket = "public"
public_base = "https://public.example.com/"

[doc_cards]
link = "https://docs.example.com/"
url = "https://cards.example.com/doc-cards"

[links]
tailnet = "example.ts.net"

[permissions]
enabled = true

[[permissions.clis]]
program = "team-post"
verbs = { send = "post" }
options = ["channel", "text"]
flags = ["broadcast"]
`)
	c, err := loadConfig(configFile{path, true})
	if err != nil {
		t.Fatal(err)
	}
	if c.SSM == nil || c.SSM.Region != "us-east-1" || c.SSM.AvatarSecret != "/slack/avatar-secret" || c.Avatars.Bucket != "public" ||
		c.DocCards.URL != "https://cards.example.com/doc-cards" || c.Links.Tailnet != "example.ts.net" || !c.Permissions.Enabled ||
		len(c.Permissions.CLIs) != 1 || c.Permissions.CLIs[0].Verbs["send"] != "post" {
		t.Fatalf("config = %+v (ssm %+v)", c, c.SSM)
	}
}

func TestLoadConfigAcceptsProofChannels(t *testing.T) {
	path := writeConfig(t, t.TempDir(), "[permissions]\nenabled = true\nproof_channels = [\"acc-*\", \"outage*\"]\n")
	if _, err := loadConfig(configFile{path, true}); err != nil {
		t.Fatalf("loadConfig with proof_channels: %v", err)
	}
}

func TestLoadConfigRefusesUnknownKeysAndPartialSections(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"unknown key":       {"[avatars]\nbuckets = \"x\"\n", "unknown keys: avatars.buckets"},
		"partial avatars":   {"[avatars]\nbucket = \"x\"\n", "endpoint, bucket, and public_base together"},
		"unslashed base":    {"[avatars]\nendpoint = \"https://s\"\nbucket = \"x\"\npublic_base = \"https://p\"\n", "must end in /"},
		"partial cards":     {"[doc_cards]\nurl = \"https://c\"\n", "link and url together"},
		"not a tailnet":     {"[links]\ntailnet = \"example.com\"\n", "must be a tailnet's DNS name"},
		"bare tailnet tld":  {"[links]\ntailnet = \".ts.net\"\n", "must be a tailnet's DNS name"},
		"partial ssm":       {"[ssm]\nregion = \"us-west-2\"\n", "region, bot_token, and socket_url"},
		"cli without verbs": {"[[permissions.clis]]\nprogram = \"team-post\"\n", "needs program and verbs"},
		"unknown cli verb":  {"[[permissions.clis]]\nprogram = \"team-post\"\nverbs = { send = \"shout\" }\n", "not one of post"},
		"ssm avatar creds":  {"[ssm]\nregion = \"r\"\nbot_token = \"/b\"\nsocket_url = \"/s\"\n[avatars]\nendpoint = \"https://s\"\nbucket = \"x\"\npublic_base = \"https://p/\"\n", "avatar_key_id and avatar_secret"},
		"two token sources": {"[github]\ntoken_env = \"GH_TOKEN\"\ntoken_command = [\"gh\", \"auth\", \"token\"]\n", "exactly one of token_env and token_command"},
		"no token source":   {"[buildkite]\n", "[buildkite] needs exactly one"},
	} {
		_, err := loadConfig(configFile{writeConfig(t, t.TempDir(), tc.body), true})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: loadConfig = %v, want an error naming %q", name, err, tc.want)
		}
	}
}

func TestEnvParametersNeedTheBotTokenAndSocketURL(t *testing.T) {
	t.Setenv(botEnv, "")
	t.Setenv(socketURLEnv, "")
	if _, err := envParameters(config{}); err == nil || !strings.Contains(err.Error(), botEnv) {
		t.Fatalf("envParameters without env = %v, want it to name %s", err, botEnv)
	}
	t.Setenv(botEnv, "xoxb-env")
	t.Setenv(socketURLEnv, "wss://socket.example.com")
	p, err := envParameters(config{})
	if err != nil || p.token != "xoxb-env" || p.socketURL != "wss://socket.example.com" {
		t.Fatalf("envParameters = %+v, %v", p, err)
	}
	t.Setenv(avatarKeyIDEnv, "")
	if _, err := envParameters(config{Avatars: avatarConfig{Endpoint: "https://s", Bucket: "b", PublicBase: "https://p/"}}); err == nil ||
		!strings.Contains(err.Error(), avatarKeyIDEnv) {
		t.Fatalf("envParameters with [avatars] and no avatar keys = %v, want it to name %s", err, avatarKeyIDEnv)
	}
}

func TestACardCommandThisMachineLacksLeavesCardsOffAndLoginWorking(t *testing.T) {
	c := config{PRLinks: prLinksConfig{CardCommand: []string{"cc-slack-test-no-such-program", "card"}}}
	if err := c.resolveTokens(); err != nil {
		t.Fatalf("a missing card_command program refused login: %v", err)
	}
	if len(c.PRLinks.CardCommand) != 0 {
		t.Fatalf("card_command = %v, want cards off on this machine", c.PRLinks.CardCommand)
	}
}

func TestATokenCommandPinnedAtLoginStillRunsAfterItsToolMoves(t *testing.T) {
	tok := refToken{source: &tokenSource{Command: []string{filepath.Join(t.TempDir(), "sh"), "-c", "echo fresh"}}}
	got, err := tok.get(context.Background(), time.Now())
	if err != nil || got != "fresh" {
		t.Fatalf("token = %q, %v, want the daemon's own sh to answer once the pinned path is gone", got, err)
	}
}

func TestTokenSourcesResolveAtLogin(t *testing.T) {
	t.Setenv("CC_SLACK_TEST_GH", "ghp-test")
	t.Setenv("CC_SLACK_TEST_EMPTY", "")
	c, err := loadConfig(configFile{writeConfig(t, t.TempDir(), "[github]\ntoken_env = \"CC_SLACK_TEST_GH\"\n[buildkite]\ntoken_command = [\"sh\", \"-c\", \"echo bk\"]\n"), true})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.resolveTokens(); err != nil {
		t.Fatal(err)
	}
	if c.GitHub.Token != "ghp-test" || c.GitHub.Env != "" || !filepath.IsAbs(c.Buildkite.Command[0]) {
		t.Fatalf("resolved sources = %+v, %+v; want the env value captured and the command made absolute", c.GitHub, c.Buildkite)
	}
	if err := c.validate(); err != nil {
		t.Fatalf("a resolved config fails validation: %v", err)
	}
	empty := config{GitHub: &tokenSource{Env: "CC_SLACK_TEST_EMPTY"}}
	if err := empty.resolveTokens(); err == nil || !strings.Contains(err.Error(), "CC_SLACK_TEST_EMPTY") {
		t.Fatalf("an empty token_env resolved: %v", err)
	}
}
