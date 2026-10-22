package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	configEnv        = "CC_SLACK_CONFIG"
	repoConfig       = ".claude/cc-slack.toml"
	botEnv           = "CC_SLACK_BOT_TOKEN"
	socketURLEnv     = "CC_SLACK_SOCKET_URL"
	avatarKeyIDEnv   = "CC_SLACK_AVATAR_KEY_ID"
	avatarSigningEnv = "CC_SLACK_AVATAR_SECRET"
)

type config struct {
	SSM         *ssmSource     `toml:"ssm" json:"-"`
	Avatars     avatarConfig   `toml:"avatars" json:"avatars"`
	DocCards    docCardsConfig `toml:"doc_cards" json:"doc_cards"`
	GitHub      *tokenSource   `toml:"github" json:"github,omitempty"`
	Buildkite   *tokenSource   `toml:"buildkite" json:"buildkite,omitempty"`
	Permissions permissions    `toml:"permissions" json:"-"`
}

type permissions struct {
	Enabled bool       `toml:"enabled"`
	CLIs    []extraCLI `toml:"clis"`
}

type extraCLI struct {
	Program string            `toml:"program"`
	Verbs   map[string]string `toml:"verbs"`
	Options []string          `toml:"options"`
	Flags   []string          `toml:"flags"`
}

var writeVerbs = []string{"post", "edit", "delete", "create", "archive", "invite", "topic", "join"}

type tokenSource struct {
	Env     string   `toml:"token_env" json:"-"`
	Command []string `toml:"token_command" json:"command,omitempty"`
	Token   string   `toml:"-" json:"token,omitempty"`
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

func (t *tokenSource) validate(section string) error {
	if t == nil {
		return nil
	}
	set := 0
	for _, on := range []bool{t.Env != "", len(t.Command) > 0, t.Token != ""} {
		if on {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("[%s] needs exactly one of token_env and token_command", section)
	}
	return nil
}

func (t *tokenSource) redacted() *tokenSource {
	if t == nil {
		return nil
	}
	return &tokenSource{Command: t.Command}
}

func (t *tokenSource) resolve(section string) error {
	switch {
	case t == nil:
		return nil
	case t.Env != "":
		t.Token = os.Getenv(t.Env)
		if t.Token == "" {
			return fmt.Errorf("[%s] token_env names $%s, which is empty", section, t.Env)
		}
		t.Env = ""
	case len(t.Command) > 0:
		bin, err := exec.LookPath(t.Command[0])
		if err != nil {
			return fmt.Errorf("[%s] token_command: %w", section, err)
		}
		t.Command[0] = bin
	}
	return nil
}

func (c *config) resolveTokens() error {
	if err := c.GitHub.resolve("github"); err != nil {
		return err
	}
	return c.Buildkite.resolve("buildkite")
}

func configPath(flag, repo string) (path string, required bool, err error) {
	if flag != "" {
		return flag, true, nil
	}
	if env := os.Getenv(configEnv); env != "" {
		return env, true, nil
	}
	checkout := filepath.Join(repo, repoConfig)
	switch _, err := os.Stat(checkout); {
	case err == nil:
		return checkout, true, nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", false, fmt.Errorf("read the checkout's cc-slack config: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("resolve the home directory for the cc-slack config: %w", err)
	}
	return filepath.Join(home, ".config", "cc-slack", "config.toml"), false, nil
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
	for _, cli := range c.Permissions.CLIs {
		if cli.Program == "" || len(cli.Verbs) == 0 {
			return errors.New("[[permissions.clis]] needs program and verbs")
		}
		for verb, write := range cli.Verbs {
			if !slices.Contains(writeVerbs, write) {
				return fmt.Errorf("[[permissions.clis]] %s maps %s to %q, which is not one of %s", cli.Program, verb, write, strings.Join(writeVerbs, ", "))
			}
		}
	}
	if err := c.GitHub.validate("github"); err != nil {
		return err
	}
	if err := c.Buildkite.validate("buildkite"); err != nil {
		return err
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
