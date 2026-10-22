package main

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	configEnv             = "CC_SLACK_CONFIG"
	repoConfig            = ".claude/cc-slack.toml"
	defaultPermissionHook = "slack_writes_need_permission.py"
	botTokenEnv           = "CC_SLACK_BOT_TOKEN"
	socketURLEnv          = "CC_SLACK_SOCKET_URL"
	avatarKeyIDEnv        = "CC_SLACK_AVATAR_KEY_ID"
	avatarSecretEnv       = "CC_SLACK_AVATAR_SECRET"
)

type config struct {
	SSM            *ssmSource     `toml:"ssm" json:"-"`
	Avatars        avatarConfig   `toml:"avatars" json:"avatars"`
	DocCards       docCardsConfig `toml:"doc_cards" json:"doc_cards"`
	PermissionHook string         `toml:"permission_hook" json:"permission_hook,omitempty"`
}

type ssmSource struct {
	Region       string `toml:"region"`
	BotToken     string `toml:"bot_token"`
	SocketURL    string `toml:"socket_url"`
	AvatarKeyID  string `toml:"avatar_key_id"`
	AvatarSecret string `toml:"avatar_secret"`
}

type avatarConfig struct {
	Endpoint   string `toml:"endpoint" json:"endpoint,omitempty"`
	Bucket     string `toml:"bucket" json:"bucket,omitempty"`
	PublicBase string `toml:"public_base" json:"public_base,omitempty"`
}

type docCardsConfig struct {
	Link string `toml:"link" json:"link,omitempty"`
	URL  string `toml:"url" json:"url,omitempty"`
}

func (a avatarConfig) enabled() bool { return a.Bucket != "" }

func (d docCardsConfig) enabled() bool { return d.URL != "" }

func (c config) permissionHook() string { return cmp.Or(c.PermissionHook, defaultPermissionHook) }

func configPath(flag, repo string) (path string, required bool, err error) {
	if flag != "" {
		return flag, true, nil
	}
	if env := os.Getenv(configEnv); env != "" {
		return env, true, nil
	}
	if checkout := filepath.Join(repo, repoConfig); exists(checkout) {
		return checkout, true, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("resolve the home directory for the cc-slack config: %w", err)
	}
	return filepath.Join(home, ".config", "cc-slack", "config.toml"), false, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func loadConfig(path string, required bool) (config, error) {
	var c config
	meta, err := toml.DecodeFile(path, &c)
	if errors.Is(err, fs.ErrNotExist) && !required {
		return config{}, nil
	}
	if err != nil {
		return config{}, fmt.Errorf("read the cc-slack config %s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return config{}, fmt.Errorf("the cc-slack config %s has unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if err := c.validate(); err != nil {
		return config{}, fmt.Errorf("the cc-slack config %s: %w", path, err)
	}
	return c, nil
}

func (c config) validate() error {
	a := c.Avatars
	if (a.Endpoint != "" || a.Bucket != "" || a.PublicBase != "") && (a.Endpoint == "" || a.Bucket == "" || a.PublicBase == "") {
		return errors.New("[avatars] needs endpoint, bucket, and public_base together")
	}
	if a.PublicBase != "" && !strings.HasSuffix(a.PublicBase, "/") {
		return fmt.Errorf("[avatars] public_base %q must end in /", a.PublicBase)
	}
	if (c.DocCards.Link == "") != (c.DocCards.URL == "") {
		return errors.New("[doc_cards] needs link and url together")
	}
	if s := c.SSM; s != nil {
		if s.Region == "" || s.BotToken == "" || s.SocketURL == "" {
			return errors.New("[ssm] needs region, bot_token, and socket_url")
		}
		if a.enabled() && (s.AvatarKeyID == "" || s.AvatarSecret == "") {
			return errors.New("[ssm] needs avatar_key_id and avatar_secret when [avatars] is set")
		}
	}
	return nil
}
