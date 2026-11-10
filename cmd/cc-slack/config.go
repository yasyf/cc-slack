package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	pictureKeyIDEnv  = "CC_SLACK_PICTURE_KEY_ID"
	pictureSecretEnv = "CC_SLACK_PICTURE_SECRET"
)

type config struct {
	SSM         *ssmSource     `toml:"ssm" json:"-"`
	Avatars     avatarConfig   `toml:"avatars" json:"avatars"`
	Pictures    pictureConfig  `toml:"pictures" json:"pictures"`
	DocCards    docCardsConfig `toml:"doc_cards" json:"doc_cards"`
	Links       linksConfig    `toml:"links" json:"links"`
	GitHub      *tokenSource   `toml:"github" json:"github,omitempty"`
	Buildkite   *tokenSource   `toml:"buildkite" json:"buildkite,omitempty"`
	PRLinks     prLinksConfig  `toml:"pr_links" json:"pr_links"`
	Permissions permissions    `toml:"permissions" json:"-"`
}

type prLinksConfig struct {
	Repos          []string `toml:"repos" json:"repos,omitempty"`
	CardChannels   []string `toml:"card_channels" json:"card_channels,omitempty"`
	CardCommand    []string `toml:"card_command" json:"card_command,omitempty"`
	PreviewCommand []string `toml:"preview_command" json:"preview_command,omitempty"`
}

type permissions struct {
	Enabled       bool       `toml:"enabled"`
	CLIs          []extraCLI `toml:"clis"`
	ProofChannels []string   `toml:"proof_channels"`
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
	Profile       string `toml:"profile"`
	Region        string `toml:"region"`
	BotToken      string `toml:"bot_token"`
	SocketURL     string `toml:"socket_url"`
	AvatarKeyID   string `toml:"avatar_key_id"`
	AvatarSecret  string `toml:"avatar_secret"`
	PictureKeyID  string `toml:"picture_key_id"`
	PictureSecret string `toml:"picture_secret"`
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

type linksConfig struct {
	Tailnet string `toml:"tailnet" json:"tailnet,omitempty"`
}

type pictureConfig struct {
	Endpoint string `toml:"endpoint" json:"endpoint,omitempty"`
	Bucket   string `toml:"bucket" json:"bucket,omitempty"`
	KeyID    string `toml:"-" json:"key_id,omitempty"`
	Secret   string `toml:"-" json:"secret,omitempty"`
}

func (a avatarConfig) enabled() bool { return a.Bucket != "" }

func (p pictureConfig) enabled() bool { return p.Bucket != "" }

func (l linksConfig) validate() error {
	if t := l.Tailnet; t != "" && (strings.HasPrefix(t, ".") || !strings.HasSuffix(t, ".ts.net")) {
		return fmt.Errorf("[links] tailnet %q must be a tailnet's DNS name, such as example.ts.net", t)
	}
	return nil
}

func (p permissions) validate() error {
	for _, cli := range p.CLIs {
		if cli.Program == "" || len(cli.Verbs) == 0 {
			return errors.New("[[permissions.clis]] needs program and verbs")
		}
		for verb, write := range cli.Verbs {
			if !slices.Contains(writeVerbs, write) {
				return fmt.Errorf("[[permissions.clis]] %s maps %s to %q, which is not one of %s", cli.Program, verb, write, strings.Join(writeVerbs, ", "))
			}
		}
	}
	return nil
}

func (d docCardsConfig) enabled() bool { return d.URL != "" }

func (p prLinksConfig) validate() error {
	for _, repo := range p.Repos {
		if _, ok := parseRepo(repo); !ok {
			return fmt.Errorf("[pr_links] repos entry %q is not owner/name", repo)
		}
	}
	return nil
}

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

func installed(bin string) string {
	if _, err := os.Stat(bin); errors.Is(err, fs.ErrNotExist) {
		return filepath.Base(bin)
	}
	return bin
}

func (c *config) resolveTokens() error {
	if err := c.GitHub.resolve("github"); err != nil {
		return err
	}
	if err := c.Buildkite.resolve("buildkite"); err != nil {
		return err
	}
	var err error
	if c.PRLinks.CardCommand, err = resolveCommand("card_command", c.PRLinks.CardCommand); err != nil {
		return err
	}
	c.PRLinks.PreviewCommand, err = resolveCommand("preview_command", c.PRLinks.PreviewCommand)
	return err
}

func resolveCommand(name string, command []string) ([]string, error) {
	if len(command) == 0 {
		return nil, nil
	}
	bin, err := exec.LookPath(command[0])
	if errors.Is(err, exec.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("[pr_links] %s: %w", name, err)
	}
	command[0] = bin
	return command, nil
}

type configFile struct {
	path     string
	required bool
}

func configFiles(flag, repo string) ([]configFile, error) {
	if flag != "" {
		return []configFile{{flag, true}}, nil
	}
	if env := os.Getenv(configEnv); env != "" {
		return []configFile{{env, true}}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve the home directory for the cc-slack config: %w", err)
	}
	user := configFile{filepath.Join(home, ".config", "cc-slack", "config.toml"), false}
	checkout := filepath.Join(repo, repoConfig)
	switch _, err := os.Stat(checkout); {
	case err == nil:
		return []configFile{{checkout, true}, user}, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("read the checkout's cc-slack config: %w", err)
	}
	return []configFile{user}, nil
}

func loadConfig(files ...configFile) (config, error) {
	var c config
	var read []string
	for _, f := range slices.Backward(files) {
		var layer config
		meta, err := toml.DecodeFile(f.path, &layer)
		if errors.Is(err, fs.ErrNotExist) && !f.required {
			continue
		}
		if err != nil {
			return config{}, fmt.Errorf("read the cc-slack config %s: %w", f.path, err)
		}
		if undecoded := meta.Undecoded(); len(undecoded) > 0 {
			keys := make([]string, len(undecoded))
			for i, k := range undecoded {
				keys[i] = k.String()
			}
			return config{}, fmt.Errorf("the cc-slack config %s has unknown keys: %s", f.path, strings.Join(keys, ", "))
		}
		fillUnset(reflect.ValueOf(&layer).Elem(), reflect.ValueOf(c), meta, nil)
		c, read = layer, append([]string{f.path}, read...)
	}
	if err := c.validate(); err != nil {
		return config{}, fmt.Errorf("the cc-slack config %s: %w", strings.Join(read, " over "), err)
	}
	return c, nil
}

func fillUnset(dst, src reflect.Value, set toml.MetaData, key toml.Key) {
	for i := range dst.NumField() {
		name, _, _ := strings.Cut(dst.Type().Field(i).Tag.Get("toml"), ",")
		if name == "-" {
			continue
		}
		field := append(slices.Clip(key), name)
		d, s := dst.Field(i), src.Field(i)
		switch {
		case !set.IsDefined(field...):
			d.Set(s)
		case d.Kind() == reflect.Struct:
			fillUnset(d, s, set, field)
		case d.Kind() == reflect.Pointer && !s.IsNil():
			fillUnset(d.Elem(), s.Elem(), set, field)
		}
	}
}

func (c config) validate() error {
	a := c.Avatars
	if (a.Endpoint != "" || a.Bucket != "" || a.PublicBase != "") && (a.Endpoint == "" || a.Bucket == "" || a.PublicBase == "") {
		return errors.New("[avatars] needs endpoint, bucket, and public_base together")
	}
	if a.PublicBase != "" && !strings.HasSuffix(a.PublicBase, "/") {
		return fmt.Errorf("[avatars] public_base %q must end in /", a.PublicBase)
	}
	if (c.Pictures.Endpoint == "") != (c.Pictures.Bucket == "") {
		return errors.New("[pictures] needs endpoint and bucket together")
	}
	if (c.DocCards.Link == "") != (c.DocCards.URL == "") {
		return errors.New("[doc_cards] needs link and url together")
	}
	if err := errors.Join(c.Links.validate(), c.Permissions.validate(), c.PRLinks.validate(), c.GitHub.validate("github"), c.Buildkite.validate("buildkite")); err != nil {
		return err
	}
	return c.SSM.validate(c)
}

func (s *ssmSource) validate(c config) error {
	switch {
	case s == nil:
		return nil
	case s.Region == "" || s.BotToken == "" || s.SocketURL == "":
		return errors.New("[ssm] needs region, bot_token, and socket_url")
	case c.Avatars.enabled() && (s.AvatarKeyID == "" || s.AvatarSecret == ""):
		return errors.New("[ssm] needs avatar_key_id and avatar_secret when [avatars] is set")
	case c.Pictures.enabled() && (s.PictureKeyID == "" || s.PictureSecret == ""):
		return errors.New("[ssm] needs picture_key_id and picture_secret when [pictures] is set")
	}
	return nil
}
