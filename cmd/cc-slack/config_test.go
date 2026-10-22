package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestConfigPathPrefersTheFlagThenTheEnvThenTheCheckoutThenHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := t.TempDir()
	t.Setenv(configEnv, "")

	if path, required, err := configPath("", repo); err != nil || required || path != filepath.Join(home, ".config", "cc-slack", "config.toml") {
		t.Fatalf("configPath with nothing set = %q, %v, %v; want the optional home config", path, required, err)
	}
	checkout := writeConfig(t, filepath.Join(repo, ".claude"), "")
	if err := os.Rename(checkout, filepath.Join(repo, repoConfig)); err != nil {
		t.Fatal(err)
	}
	if path, required, _ := configPath("", repo); !required || path != filepath.Join(repo, repoConfig) {
		t.Fatalf("configPath with a checkout config = %q, %v; want the checkout's %s", path, required, repoConfig)
	}
	t.Setenv(configEnv, "/env/cc-slack.toml")
	if path, required, _ := configPath("", repo); !required || path != "/env/cc-slack.toml" {
		t.Fatalf("configPath with $%s = %q, %v; want the env path", configEnv, path, required)
	}
	if path, required, _ := configPath("/flag.toml", repo); !required || path != "/flag.toml" {
		t.Fatalf("configPath with --config = %q, %v; want the flag", path, required)
	}
}

func TestLoadConfigTreatsAMissingDefaultAsEmptyAndAMissingNamedFileAsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.toml")
	if c, err := loadConfig(missing, false); err != nil || c.SSM != nil || c.Avatars.enabled() || c.DocCards.enabled() {
		t.Fatalf("loadConfig of a missing default = %+v, %v; want the empty config", c, err)
	}
	if _, err := loadConfig(missing, true); err == nil {
		t.Fatal("loadConfig of a missing named file succeeded, want an error")
	}
}

func TestLoadConfigReadsEverySection(t *testing.T) {
	path := writeConfig(t, t.TempDir(), `permission_hook = "slack_guard.py"

[ssm]
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
`)
	c, err := loadConfig(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.SSM == nil || c.SSM.Region != "us-east-1" || c.SSM.AvatarSecret != "/slack/avatar-secret" || c.Avatars.Bucket != "public" ||
		c.DocCards.URL != "https://cards.example.com/doc-cards" || c.permissionHook() != "slack_guard.py" {
		t.Fatalf("config = %+v (ssm %+v)", c, c.SSM)
	}
	if hook := (config{}).permissionHook(); hook != defaultPermissionHook {
		t.Fatalf("default permission hook = %q, want %q", hook, defaultPermissionHook)
	}
}

func TestLoadConfigRefusesUnknownKeysAndPartialSections(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"unknown key":      {"[avatars]\nbuckets = \"x\"\n", "unknown keys: avatars.buckets"},
		"partial avatars":  {"[avatars]\nbucket = \"x\"\n", "endpoint, bucket, and public_base together"},
		"unslashed base":   {"[avatars]\nendpoint = \"https://s\"\nbucket = \"x\"\npublic_base = \"https://p\"\n", "must end in /"},
		"partial cards":    {"[doc_cards]\nurl = \"https://c\"\n", "link and url together"},
		"partial ssm":      {"[ssm]\nregion = \"us-west-2\"\n", "region, bot_token, and socket_url"},
		"ssm avatar creds": {"[ssm]\nregion = \"r\"\nbot_token = \"/b\"\nsocket_url = \"/s\"\n[avatars]\nendpoint = \"https://s\"\nbucket = \"x\"\npublic_base = \"https://p/\"\n", "avatar_key_id and avatar_secret"},
	} {
		_, err := loadConfig(writeConfig(t, t.TempDir(), tc.body), true)
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
