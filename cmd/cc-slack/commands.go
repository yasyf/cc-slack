package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	_ "time/tzdata"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/cmd"
	"github.com/yasyf/cc-interact/daemon"
)

const (
	tokenPlaceholder      = "unset"
	captHookBinary        = "capt-hook"
	captHookReinstallWait = 30 * time.Second
	captHookReinstallPoll = 250 * time.Millisecond
	pacificLayout         = "2006-01-02 15:04"
)

var pacific = func() *time.Location {
	zone, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return zone
}()

type sessionFlags struct {
	session string
	cwd     string
}

func (f *sessionFlags) bind(c *cobra.Command) {
	c.Flags().StringVar(&f.session, "session", os.Getenv("CLAUDE_CODE_SESSION_ID"), "Claude session id (defaults to $CLAUDE_CODE_SESSION_ID)")
	c.Flags().StringVar(&f.cwd, "cwd", "", "working directory (defaults to the current directory)")
}

func runOp(c *cobra.Command, d cmd.Deps, f sessionFlags, op daemon.Op, body map[string]any) error {
	reply, err := callOp(c, d, f, op, body)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(c.OutOrStdout(), string(reply))
	return err
}

func callOp(c *cobra.Command, d cmd.Deps, f sessionFlags, op daemon.Op, body map[string]any) (json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	reply, err := invoke(c.Context(), d, f.session, cwdOr(f.cwd), op, raw)
	return reply.Body, err
}

func renderOp[T any](c *cobra.Command, d cmd.Deps, f sessionFlags, op daemon.Op, body map[string]any, asJSON bool, render func(io.Writer, T)) error {
	if asJSON {
		return runOp(c, d, f, op, body)
	}
	raw, err := callOp(c, d, f, op, body)
	if err != nil {
		return err
	}
	var res T
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	w := tabwriter.NewWriter(c.OutOrStdout(), 0, 0, 2, ' ', 0)
	render(w, res)
	return w.Flush()
}

func sessionStartCmd(d cmd.Deps) *cobra.Command {
	return &cobra.Command{
		Use:    "session-start",
		Hidden: true,
		Short:  "SessionStart hook: create this window's cc-slack subject",
		Args:   cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			in, err := readHookInput(c)
			if err != nil {
				return err
			}
			reply, err := invoke(c.Context(), d, in.SessionID, cwdOr(in.Cwd), opSessionStart, mustJSON(sessionStartBody{Source: in.Source}))
			if err != nil {
				return err
			}
			var res sessionStartResult
			if err := json.Unmarshal(reply.Body, &res); err != nil {
				return fmt.Errorf("session-start reply: %w", err)
			}
			return writeWatchesBack(c.OutOrStdout(), res)
		},
	}
}

type hookInput struct {
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
	Trigger        string `json:"trigger"`
	Source         string `json:"source"`
}

func readHookInput(c *cobra.Command) (hookInput, error) {
	var in hookInput
	raw, err := io.ReadAll(c.InOrStdin())
	if err != nil || len(raw) == 0 {
		return in, err
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, fmt.Errorf("%s hook input: %w", c.Name(), err)
	}
	return in, nil
}

func writeWatchesBack(w io.Writer, res sessionStartResult) error {
	if res.WatchesBack == 0 {
		return nil
	}
	return json.NewEncoder(w).Encode(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "SessionStart",
		"additionalContext": fmt.Sprintf("cc-slack brought back %d of this session's Slack watches after the restart, and anything sent while it was down arrives as usual. "+
			"Nothing already delivered comes again. There is nothing to re-arm by hand.", res.WatchesBack),
	}})
}

func loginCmd(d cmd.Deps) *cobra.Command {
	var (
		f          sessionFlags
		email      string
		repo       string
		configFlag string
		postOnly   bool
	)
	c := &cobra.Command{
		Use:   "login",
		Short: "Read the shared bot token and bind this machine to your Slack user, this checkout, and the cc-slack config",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			ctx := c.Context()
			if email == "" {
				resolved, err := gitEmail(ctx)
				if err != nil {
					return err
				}
				email = resolved
			}
			captHook, err := captHookPath()
			if err != nil {
				return err
			}
			root, cfg, err := readConfig(ctx, f, repo, configFlag)
			if err != nil {
				return err
			}
			params, err := readParameters(ctx, cfg)
			if err != nil {
				return err
			}
			if err := archiveStaleState(ctx); err != nil {
				return err
			}
			if postOnly {
				params.socketURL = ""
			}
			cfg.Pictures.KeyID, cfg.Pictures.Secret = params.pictureKeyID, params.pictureSecret
			return runOp(c, d, f, opLogin, map[string]any{
				"token": params.token, "email": email, "socket_url": params.socketURL,
				"avatar_key_id": params.avatarKeyID, "avatar_secret": params.avatarSecret,
				"repo_root": root, "orca_path": orcaPath(), "capt_hook_path": captHook, "config": cfg,
			})
		},
	}
	f.bind(c)
	c.Flags().StringVar(&email, "email", "", "your Slack email (defaults to git config user.email)")
	c.Flags().StringVar(&repo, "repo", "", "checkout the message shortcut opens orca workspaces in (defaults to the main checkout of the current directory's repo)")
	c.Flags().BoolVar(&postOnly, "post-only", runtime.GOOS != "darwin", "never open the Slack socket, so this host posts and reads but takes no button taps (defaults on everywhere but macOS)")
	c.Flags().StringVar(&configFlag, "config", "", "cc-slack config file (defaults to $"+configEnv+", then the checkout's "+repoConfig+" over ~/.config/cc-slack/config.toml, then that file alone)")
	return c
}

func reloadCmd(d cmd.Deps) *cobra.Command {
	var (
		f          sessionFlags
		repo       string
		configFlag string
	)
	c := &cobra.Command{
		Use:   "reload",
		Short: "Re-read the cc-slack config into this machine's login, keeping its bot token, socket URL, and avatar key",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			_, cfg, err := readConfig(c.Context(), f, repo, configFlag)
			if err != nil {
				return err
			}
			return runOp(c, d, f, opReload, map[string]any{"config": cfg})
		},
	}
	f.bind(c)
	c.Flags().StringVar(&repo, "repo", "", "checkout whose "+repoConfig+" to read (defaults to the main checkout of the current directory's repo)")
	c.Flags().StringVar(&configFlag, "config", "", "cc-slack config file (defaults to $"+configEnv+", then the checkout's "+repoConfig+" over ~/.config/cc-slack/config.toml, then that file alone)")
	return c
}

func readConfig(ctx context.Context, f sessionFlags, repo, configFlag string) (string, config, error) {
	root, err := repoRoot(ctx, cwdOr(f.cwd), repo)
	if err != nil {
		return "", config{}, err
	}
	files, err := configFiles(configFlag, root)
	if err != nil {
		return "", config{}, err
	}
	cfg, err := loadConfig(files...)
	if err != nil {
		return "", config{}, err
	}
	if err := cfg.resolveTokens(); err != nil {
		return "", config{}, err
	}
	return root, cfg, nil
}

func gitEmail(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "config", "user.email").Output()
	if err != nil {
		return "", fmt.Errorf("git config user.email: %w (pass --email)", err)
	}
	email := strings.TrimSpace(string(out))
	if email == "" {
		return "", errors.New("git config user.email is empty; pass --email")
	}
	return email, nil
}

func repoRoot(ctx context.Context, cwd, flag string) (string, error) {
	if flag != "" {
		return filepath.Abs(flag)
	}
	git := exec.CommandContext(ctx, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	git.Dir = cwd
	out, err := git.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-common-dir in %s: %w (run login inside the checkout, or pass --repo)", cwd, err)
	}
	common := strings.TrimSpace(string(out))
	if filepath.Base(common) != ".git" {
		return "", fmt.Errorf("%s is not a checkout's .git directory; pass --repo", common)
	}
	return filepath.Dir(common), nil
}

func orcaPath() string {
	path, err := exec.LookPath("orca")
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		panic(err)
	}
	return abs
}

func captHookPath() (string, error) {
	path, err := exec.LookPath(captHookBinary)
	if err != nil {
		return "", fmt.Errorf("every Slack write spends a captain-hook ticket, and %s is not on PATH: install captain-hook, then rerun login: %w", captHookBinary, err)
	}
	return filepath.Abs(path)
}

type parameters struct {
	token         string
	socketURL     string
	avatarKeyID   string
	avatarSecret  string
	pictureKeyID  string
	pictureSecret string
}

func readParameters(ctx context.Context, cfg config) (parameters, error) {
	if cfg.SSM == nil {
		return envParameters(cfg)
	}
	src := cfg.SSM
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(src.Region),
		awsconfig.WithCredentialsProvider(processcreds.NewProviderCommand(awsCLICredentials(src.Profile))),
	}
	if src.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(src.Profile))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return parameters{}, fmt.Errorf("load AWS config: %w", err)
	}
	client := ssm.NewFromConfig(awsCfg)
	var p parameters
	reads := map[string]*string{src.BotToken: &p.token, src.SocketURL: &p.socketURL}
	if cfg.Avatars.enabled() {
		reads[src.AvatarKeyID], reads[src.AvatarSecret] = &p.avatarKeyID, &p.avatarSecret
	}
	if cfg.Pictures.enabled() {
		reads[src.PictureKeyID], reads[src.PictureSecret] = &p.pictureKeyID, &p.pictureSecret
	}
	for name, value := range reads {
		if *value, err = readParameter(ctx, client, src.Profile, name); err != nil {
			return parameters{}, err
		}
	}
	return p, nil
}

func envParameters(cfg config) (parameters, error) {
	p := parameters{
		token: os.Getenv(botEnv), socketURL: os.Getenv(socketURLEnv),
		avatarKeyID: os.Getenv(avatarKeyIDEnv), avatarSecret: os.Getenv(avatarSigningEnv),
		pictureKeyID: os.Getenv(pictureKeyIDEnv), pictureSecret: os.Getenv(pictureSecretEnv),
	}
	if p.token == "" || p.socketURL == "" {
		return parameters{}, fmt.Errorf("login needs %s and %s, or an [ssm] section in the cc-slack config", botEnv, socketURLEnv)
	}
	if cfg.Avatars.enabled() && (p.avatarKeyID == "" || p.avatarSecret == "") {
		return parameters{}, fmt.Errorf("[avatars] needs %s and %s, or an [ssm] section in the cc-slack config", avatarKeyIDEnv, avatarSigningEnv)
	}
	if cfg.Pictures.enabled() && (p.pictureKeyID == "" || p.pictureSecret == "") {
		return parameters{}, fmt.Errorf("[pictures] needs %s and %s, or an [ssm] section in the cc-slack config", pictureKeyIDEnv, pictureSecretEnv)
	}
	return p, nil
}

func awsCLICredentials(profile string) processcreds.NewCommandBuilderFunc {
	return func(ctx context.Context) (*exec.Cmd, error) {
		args := []string{"configure", "export-credentials", "--format", "process"}
		if profile != "" {
			args = append(args, "--profile", profile)
		}
		c := exec.CommandContext(ctx, "aws", args...)
		c.Stderr = os.Stderr
		return c, nil
	}
}

func readParameter(ctx context.Context, client *ssm.Client, profile, name string) (string, error) {
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name), WithDecryption: aws.Bool(true)})
	if err != nil {
		return "", fmt.Errorf("read SSM parameter %s with %s (sign in to AWS with a role that can read it, or set profile under [ssm] in the cc-slack config to an AWS profile that can): %w", name, awsProfile(profile), err)
	}
	value := aws.ToString(out.Parameter.Value)
	if value == tokenPlaceholder {
		return "", fmt.Errorf("%s still holds its placeholder: it has not been written yet", name)
	}
	return value, nil
}

func awsProfile(profile string) string {
	if profile == "" {
		return "the shell's AWS profile"
	}
	return "AWS profile " + profile
}

func whoamiCmd(d cmd.Deps) *cobra.Command {
	var f sessionFlags
	c := &cobra.Command{
		Use:   "whoami",
		Short: "Print the Slack identity cc-slack posts as",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return runOp(c, d, f, opWhoami, nil) },
	}
	f.bind(c)
	return c
}

type messageFlags struct {
	sessionFlags
	channelID string
	user      string
	threadTS  string
	ts        string
	url       string
}

func (f *messageFlags) bind(c *cobra.Command) {
	f.sessionFlags.bind(c)
	c.Flags().StringVar(&f.channelID, "channel", "", "Slack channel id, or a user id: yours for your DM with the app, anyone else's for the group DM of that person, you and the app")
	c.Flags().StringVar(&f.user, "user", "", "Slack user id, in place of --channel: yours for your DM with the app, anyone else's for the group DM of that person, you and the app")
	c.MarkFlagsMutuallyExclusive("channel", "user")
	c.Flags().StringVar(&f.threadTS, "thread", "", "thread parent ts")
	c.Flags().StringVar(&f.url, "url", "", "Slack permalink (fills channel, ts, and thread)")
}

func (f messageFlags) body() map[string]any {
	return map[string]any{"channel_id": cmp.Or(f.channelID, f.user), "thread_ts": f.threadTS, "ts": f.ts, "url": f.url}
}

func postCmd(d cmd.Deps, use, short string, op daemon.Op) *cobra.Command {
	var (
		f         messageFlags
		text      string
		textFile  string
		blocks    string
		files     []string
		agentName string
		broadcast bool
		noWatch   bool
		spend     ticketSpend
	)
	c := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body := f.body()
			if err := putText(body, f.cwd, text, textFile); err != nil {
				return err
			}
			body["files"] = absPaths(cwdOr(f.cwd), files)
			body["broadcast"] = broadcast
			body["no_watch"] = noWatch
			body["agent"] = agentName
			if err := putBlocks(body, f.cwd, blocks); err != nil {
				return err
			}
			spend.fill(body)
			return runOp(c, d, f.sessionFlags, op, body)
		},
	}
	f.bind(c)
	bindText(c, &text, &textFile, "message text")
	c.Flags().BoolVar(&broadcast, "broadcast", false, "also send the thread reply to the channel")
	c.Flags().BoolVar(&noWatch, "no-watch", false, "post without arming a watch on the message's thread")
	bindAgent(c, &agentName)
	bindTicket(c, &spend)
	bindFiles(c, &files)
	bindBlocks(c, &blocks)
	return c
}

func channelCmd(d cmd.Deps, op daemon.Op, action, short string) *cobra.Command {
	var (
		f                           sessionFlags
		channelID, url, name, topic string
		users                       []string
		private                     bool
		spend                       ticketSpend
	)
	c := &cobra.Command{
		Use:   string(op),
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body := map[string]any{"channel_id": channelID, "url": url, "name": name, "users": users, "topic": topic, "private": private}
			spend.fill(body)
			return runOp(c, d, f, op, body)
		},
	}
	f.bind(c)
	if action == approveCreate {
		c.Flags().StringVar(&name, "name", "", "name of the channel to create")
		c.Flags().BoolVar(&private, "private", false, "make it a private channel holding only you and the app, one of your conversations with it as your DM is, where dm-status --channel posts; needs no ticket, and creating the same name again returns it")
	} else {
		c.Flags().StringVar(&channelID, "channel", "", "Slack channel id")
		c.Flags().StringVar(&url, "url", "", "Slack channel or message permalink (fills channel)")
	}
	switch action {
	case approveInvite:
		c.Flags().StringArrayVar(&users, "user", nil, "Slack user id to invite (repeatable)")
	case approveTopic:
		c.Flags().StringVar(&topic, "topic", "", "the channel's new topic")
	}
	bindTicket(c, &spend)
	return c
}

func revokeCmd(d cmd.Deps) *cobra.Command {
	var f sessionFlags
	c := &cobra.Command{
		Use:   "revoke <ticket_id>",
		Short: "Revoke a captain-hook ticket",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runOp(c, d, f, opRevoke, map[string]any{"ticket_id": args[0]})
		},
	}
	f.bind(c)
	return c
}

func editCmd(d cmd.Deps) *cobra.Command {
	var f messageFlags
	var text, textFile, blocks string
	var spend ticketSpend
	c := &cobra.Command{
		Use:   "edit",
		Short: "Replace the text of a message the bot posted",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body := f.body()
			if err := putText(body, f.cwd, text, textFile); err != nil {
				return err
			}
			if err := putBlocks(body, f.cwd, blocks); err != nil {
				return err
			}
			spend.fill(body)
			return runOp(c, d, f.sessionFlags, opEdit, body)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&f.ts, "ts", "", "ts of the message to edit")
	bindText(c, &text, &textFile, "new message text")
	bindTicket(c, &spend)
	bindBlocks(c, &blocks)
	return c
}

func deleteCmd(d cmd.Deps) *cobra.Command {
	var f messageFlags
	var spend ticketSpend
	c := &cobra.Command{
		Use:   "delete",
		Short: "Delete a message the bot posted",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body := f.body()
			spend.fill(body)
			return runOp(c, d, f.sessionFlags, opDelete, body)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&f.ts, "ts", "", "ts of the message to delete")
	bindTicket(c, &spend)
	return c
}

func bindTicket(c *cobra.Command, s *ticketSpend) {
	c.Flags().StringVar(&s.TicketID, "ticket", "", "id of the captain-hook ticket covering this write; the Slack permission hook sets it and the hidden --ticket-* flags")
	for name, dest := range map[string]*string{"ticket-tree": &s.Tree, "ticket-agent": &s.Agent, "ticket-call": &s.Call, "ticket-fingerprint": &s.Fingerprint} {
		c.Flags().StringVar(dest, name, "", "set by the Slack permission hook")
		if err := c.Flags().MarkHidden(name); err != nil {
			panic(err)
		}
	}
}

func (s ticketSpend) fill(body map[string]any) {
	body["ticket_id"], body["ticket_tree"], body["ticket_agent"], body["ticket_call"], body["ticket_fingerprint"] = s.TicketID, s.Tree, s.Agent, s.Call, s.Fingerprint
}

func bindFiles(c *cobra.Command, files *[]string) {
	c.Flags().StringArrayVar(files, "file", nil, "file to attach, up to 1 GB (repeatable)")
}

func bindText(c *cobra.Command, text, textFile *string, usage string) {
	c.Flags().StringVar(text, "text", "", usage)
	c.Flags().StringVar(textFile, "text-file", "", "path to a file holding the "+usage+", in place of --text")
	c.MarkFlagsMutuallyExclusive("text", "text-file")
}

func putText(body map[string]any, cwd, text, textFile string) error {
	if textFile == "" {
		body["text"] = text
		return nil
	}
	raw, err := os.ReadFile(absPaths(cwdOr(cwd), []string{textFile})[0])
	if err != nil {
		return fmt.Errorf("read --text-file: %w", err)
	}
	body["text"] = strings.TrimRight(string(raw), "\n")
	return nil
}

func bindBlocks(c *cobra.Command, blocks *string) {
	c.Flags().StringVar(blocks, "blocks", "", "Block Kit blocks as a JSON array, or @path to a file holding one; --text stays the notification text")
}

func putBlocks(body map[string]any, cwd, value string) error {
	if value == "" {
		return nil
	}
	raw := []byte(value)
	if path, ok := strings.CutPrefix(value, "@"); ok {
		var err error
		if raw, err = os.ReadFile(absPaths(cwdOr(cwd), []string{path})[0]); err != nil {
			return fmt.Errorf("read --blocks: %w", err)
		}
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return fmt.Errorf("--blocks takes a JSON array of Block Kit blocks: %w", err)
	}
	body["blocks"] = blocks
	return nil
}

func absPaths(cwd string, paths []string) []string {
	dir, err := filepath.Abs(cwd)
	if err != nil {
		panic(err)
	}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		out = append(out, path)
	}
	return out
}

func reactionCmd(d cmd.Deps, use, short string, op daemon.Op) *cobra.Command {
	var f messageFlags
	var name string
	c := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body := f.body()
			body["name"] = name
			return runOp(c, d, f.sessionFlags, op, body)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&f.ts, "ts", "", "message ts")
	c.Flags().StringVar(&name, "name", "", "emoji name")
	return c
}

func threadCmd(d cmd.Deps) *cobra.Command {
	var f messageFlags
	var limit int
	var raw bool
	var since string
	c := &cobra.Command{
		Use:   "thread",
		Short: "Print a Slack thread's messages",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body := f.body()
			body["limit"] = limit
			body["raw"] = raw
			body["since_ts"] = since
			return runOp(c, d, f.sessionFlags, opThread, body)
		},
	}
	f.bind(c)
	c.Flags().IntVar(&limit, "limit", defaultThreadLimit, "messages to print: the oldest newer than --since-ts, else the most recent")
	c.Flags().StringVar(&since, "since-ts", "", "print only messages newer than this Slack ts, or whole epoch seconds")
	c.Flags().BoolVar(&raw, "raw", false, "also print each message as Slack sends it, with its blocks, attachments and metadata")
	return c
}

func historyCmd(d cmd.Deps) *cobra.Command {
	var (
		f              sessionFlags
		channelID, url string
		limit          int
		asJSON, raw    bool
	)
	c := &cobra.Command{
		Use:   "history",
		Short: "Print a Slack channel's most recent top-level messages, newest first",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body := map[string]any{"channel_id": channelID, "url": url, "limit": limit, "raw": raw}
			return renderOp(c, d, f, opHistory, body, asJSON || raw, renderHistory)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&channelID, "channel", "", "Slack channel id, or a user id: yours for your DM with the app, anyone else's for the group DM of that person, you and the app")
	c.Flags().StringVar(&url, "url", "", "Slack channel or message permalink (fills channel)")
	c.Flags().BoolVar(&raw, "raw", false, "print JSON with each message as Slack sends it, with its blocks, attachments and metadata")
	c.Flags().IntVar(&limit, "limit", defaultHistoryLimit, "most recent top-level messages to print")
	c.Flags().BoolVar(&asJSON, "json", false, "print the daemon's JSON result")
	return c
}

func renderHistory(w io.Writer, res historyResult) {
	defer func() {
		if res.Partial {
			_, _ = fmt.Fprintf(w, "partial: ran out of time after the newest %d messages; ask for fewer\n", len(res.Messages))
		}
	}()
	for _, m := range res.Messages {
		replies := ""
		if m.ReplyCount > 0 {
			replies = fmt.Sprintf("%d replies", m.ReplyCount)
		}
		text, _, _ := strings.Cut(m.Text, "\n")
		sec, _ := splitTS(m.TS)
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", time.Unix(sec, 0).In(pacific).Format(pacificLayout), m.TS, m.UserName, replies, text)
	}
}

func fileCmd(d cmd.Deps) *cobra.Command {
	var (
		f      sessionFlags
		out    string
		asJSON bool
	)
	c := &cobra.Command{
		Use:   "file <permalink | file id>",
		Short: "Download a Slack file attachment through the bot and print its path, mimetype, and size",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			body := map[string]any{"file_id": args[0]}
			if strings.HasPrefix(args[0], "https://") {
				body = map[string]any{"url": args[0]}
			}
			if out != "" {
				body["out"] = absPaths(cwdOr(f.cwd), []string{out})[0]
			}
			return renderOp(c, d, f, opFile, body, asJSON, renderFile)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&out, "out", "", "path to write the file to (defaults to the daemon's files directory)")
	c.Flags().BoolVar(&asJSON, "json", false, "print the daemon's JSON result")
	return c
}

func renderFile(w io.Writer, res fileResult) {
	_, _ = fmt.Fprintf(w, "%s\t%s\t%d\n", res.Path, res.Mimetype, res.Size)
}

func channelsCmd(d cmd.Deps) *cobra.Command {
	var (
		f      sessionFlags
		prefix string
		asJSON bool
	)
	c := &cobra.Command{
		Use:   "channels",
		Short: "List the unarchived public Slack channels and the private ones the bot is in",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return renderOp(c, d, f, opChannels, map[string]any{"prefix": prefix}, asJSON, renderChannels)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&prefix, "prefix", "", "only channels whose name starts with this prefix")
	c.Flags().BoolVar(&asJSON, "json", false, "print the daemon's JSON result")
	return c
}

func renderChannels(w io.Writer, res channelsResult) {
	for _, ch := range res.Channels {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", ch.ID, ch.Name, time.Unix(ch.Created, 0).In(pacific).Format(pacificLayout))
	}
}

func usersCmd(d cmd.Deps) *cobra.Command {
	var (
		f      sessionFlags
		asJSON bool
	)
	c := &cobra.Command{
		Use:   "users <query>",
		Short: "List the active Slack users whose handle, display name, real name, or email contains the query, ignoring case, or the one a user id or exact email names",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return renderOp(c, d, f, opUsers, map[string]any{"query": args[0]}, asJSON, renderUsers)
		},
	}
	f.bind(c)
	c.Flags().BoolVar(&asJSON, "json", false, "print the daemon's JSON result")
	return c
}

func renderUsers(w io.Writer, res usersResult) {
	for _, u := range res.Users {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", u.ID, u.Name, u.RealName, u.Email)
	}
}

func dmPeerCmd(d cmd.Deps) *cobra.Command {
	var (
		f              sessionFlags
		channelID, url string
	)
	c := &cobra.Command{
		Use:   "dm-peer",
		Short: "Print the one person a Slack DM reaches besides the bot and you, or an empty peer for any other conversation",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runOp(c, d, f, opDMPeer, map[string]any{"channel_id": channelID, "url": url})
		},
	}
	f.bind(c)
	c.Flags().StringVar(&channelID, "channel", "", "Slack conversation id, or a user id")
	c.Flags().StringVar(&url, "url", "", "Slack channel or message permalink (fills channel)")
	return c
}

func channelInfoCmd(d cmd.Deps) *cobra.Command {
	var (
		f         sessionFlags
		channelID string
	)
	c := &cobra.Command{
		Use:   "channel-info",
		Short: "Print one Slack channel's name and earlier names, read once and held by the daemon",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runOp(c, d, f, opChannelInfo, map[string]any{"channel_id": channelID})
		},
	}
	f.bind(c)
	c.Flags().StringVar(&channelID, "channel", "", "Slack channel id")
	return c
}

func redrawPRCardsCmd(d cmd.Deps) *cobra.Command {
	var f sessionFlags
	var pr string
	c := &cobra.Command{
		Use:   "redraw-pr-cards",
		Short: "Redraw every open PR card in place from the card command, with no new posts; with --pr, every card listing that PR and its copies too, and its withdrawn cards, which come back in place when drawn under the waits id they were withdrawn from",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runOp(c, d, f, opRedrawPRCards, map[string]any{"pr": pr})
		},
	}
	f.bind(c)
	c.Flags().StringVar(&pr, "pr", "", "pull request URL: redraw only that PR's cards")
	return c
}

func withdrawPRCardCmd(d cmd.Deps) *cobra.Command {
	var f sessionFlags
	var pr, reason string
	c := &cobra.Command{
		Use:   "withdraw-pr-card",
		Short: "Withdraw every open tap card that carries a pull request, a stack's included, in place: Withdrawn and the reason replace its buttons, and a late tap records nothing",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runOp(c, d, f, opWithdrawPRCard, map[string]any{"pr": pr, "reason": reason})
		},
	}
	f.bind(c)
	c.Flags().StringVar(&pr, "pr", "", "pull request URL: every open card carrying it is withdrawn")
	c.Flags().StringVar(&reason, "reason", "", "why the card no longer stands, shown on it after Withdrawn")
	_ = c.MarkFlagRequired("pr")
	_ = c.MarkFlagRequired("reason")
	return c
}

func dmOwedCmd(d cmd.Deps) *cobra.Command {
	var (
		f         sessionFlags
		channelID string
	)
	c := &cobra.Command{
		Use:   "dm-owed",
		Short: "Print your DM replies that no bot or agent has answered in their thread yet, from stored state",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runOp(c, d, f, opDMOwed, map[string]any{"channel_id": channelID})
		},
	}
	f.bind(c)
	bindConversation(c, &channelID)
	return c
}

func followCmd(d cmd.Deps) *cobra.Command {
	var f messageFlags
	var owner string
	c := &cobra.Command{
		Use:   "follow",
		Short: "Watch a Slack thread or channel for new messages",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body := f.body()
			body["owner"] = owner
			return runOp(c, d, f.sessionFlags, opFollow, body)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&owner, "owner", "", "agent id that owns the watch (empty for the top-level session)")
	return c
}

func subscribeCmd(d cmd.Deps) *cobra.Command {
	var (
		f     messageFlags
		owner string
		kinds []string
		match string
	)
	c := &cobra.Command{
		Use:   "subscribe",
		Short: "Subscribe to a Slack channel's alerts, releases or posts as one-line events",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body := f.body()
			body["owner"], body["kinds"], body["match"] = owner, kinds, match
			return runOp(c, d, f.sessionFlags, opFollow, body)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&owner, "owner", "", "agent id that owns the subscription (empty for the top-level session)")
	c.Flags().StringSliceVar(&kinds, "kind", nil, "alerts, releases or posts; repeat or comma-separate")
	c.Flags().StringVar(&match, "match", "", "keep only lines containing this, such as a monitor id or a word of its name")
	_ = c.MarkFlagRequired("kind")
	return c
}

func unfollowCmd(d cmd.Deps) *cobra.Command {
	var f messageFlags
	c := &cobra.Command{
		Use:   "unfollow [watch_id]",
		Short: "Stop a Slack watch, by its id or by --channel and --thread or --url",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			body := f.body()
			if len(args) == 1 {
				watchID, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil {
					return fmt.Errorf("watch id %q: %w", args[0], err)
				}
				body["watch_id"] = watchID
			}
			return runOp(c, d, f.sessionFlags, opUnfollow, body)
		},
	}
	f.bind(c)
	return c
}

func followsCmd(d cmd.Deps) *cobra.Command {
	var (
		f      sessionFlags
		asJSON bool
	)
	c := &cobra.Command{
		Use:   "follows",
		Short: "List a session's active Slack watches, channel follows first, from stored state",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return renderOp(c, d, f, opFollows, map[string]any{}, asJSON, renderFollows)
		},
	}
	f.bind(c)
	c.Flags().BoolVar(&asJSON, "json", false, "print the daemon's JSON result")
	return c
}

func renderFollows(w io.Writer, res followsResult) {
	_, _ = fmt.Fprintf(w, "session %s: %d active watches\n", res.Session, len(res.Watches))
	for _, f := range res.Watches {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", f.WatchID, f.Kind, f.ChannelID, f.ThreadTS, f.Owner, time.Unix(f.Created, 0).In(pacific).Format(pacificLayout))
	}
}

func dmStatusCmd(d cmd.Deps) *cobra.Command {
	var (
		f         sessionFlags
		thread    string
		blocks    string
		card      string
		agentName string
		files     []string
		rootOnly  bool
		mirrored  bool
		about     string
		drive     string
		tapPRs    []string
		channelID string
	)
	c := &cobra.Command{
		Use:   "dm-status [text]",
		Short: "Post status to your DM with the app, as a top-level message with its own thread",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			body := map[string]any{"channel_id": channelID, "thread_ts": thread, "files": absPaths(cwdOr(f.cwd), files), "agent": agentName, "root_only": rootOnly, "mirrored": mirrored, "about": about, "drive": drive, "tap_prs": tapPRs}
			if len(args) > 0 {
				body["text"] = args[0]
			}
			if err := putBlocks(body, f.cwd, blocks); err != nil {
				return err
			}
			if err := putCard(body, f.cwd, card); err != nil {
				return err
			}
			return runOp(c, d, f, opDMStatus, body)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&thread, "thread", "", "ts of a DM message to post under, card or text; its replies reach this session")
	bindFiles(c, &files)
	bindBlocks(c, &blocks)
	c.Flags().StringVar(&card, "card", "", "status card as a JSON object, or @path to a file holding one, in place of text")
	c.Flags().BoolVar(&rootOnly, "root-only", false, "redraw this session's one root card with --card, posting it at the DM root the first time")
	c.Flags().BoolVar(&mirrored, "mirrored", false, "the waiting card already sits on a cc-present board; post it without telling the board, with any --file images on the card")
	c.Flags().StringVar(&about, "about", "", "waits id of the waiting card this text reply is about; the reply lands in that card's thread")
	c.Flags().StringVar(&drive, "drive", "", "the drive this session works for; while it hears the DM, a DM holder of that drive leaves replies under its posts to it")
	c.Flags().StringArrayVar(&tapPRs, "tap-pr", nil, "URL of a pull request waiting on the user's tap, to draw its one card with buttons; repeatable. A link in the text or card draws none")
	bindConversation(c, &channelID)
	bindAgent(c, &agentName)
	return c
}

func cardAnswerCmd(d cmd.Deps) *cobra.Command {
	var (
		f                                sessionFlags
		waits, card, label, via, surface string
		other, settled, gone             bool
	)
	c := &cobra.Command{
		Use:   "card-answer",
		Short: "Answer a waiting status card from another surface, such as a cc-present board",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runOp(c, d, f, opCardAnswer, map[string]any{"waits": waits, "url": card, "label": label, "other": other, "settled": settled, "withdrawn": gone, "via": via, "surface": surface})
		},
	}
	f.bind(c)
	c.Flags().StringVar(&waits, "waits", "", "the id you know the card by, such as its waits id or a board's id; --card finds the card")
	c.Flags().StringVar(&card, "card", "", "the waiting card's Slack permalink, which finds the card whichever session posted it")
	c.Flags().StringVar(&label, "label", "", "the answer: one of the card's options, or the written-in text with --other")
	c.Flags().BoolVar(&other, "other", false, "the label is a written-in answer, not one of the card's options")
	c.Flags().StringVar(&via, "via", "", "url of the surface the answer came from; the card and the capture name it")
	c.Flags().StringVar(&surface, "surface", "", "dashboard when the answer came from a drive dashboard; empty for a cc-present board")
	c.Flags().BoolVar(&settled, "settled", false, "the question was settled by what happened, such as the pull request landing: the card shows the written-in label and no session hears a tap")
	c.Flags().BoolVar(&gone, "withdrawn", false, "the question was withdrawn: the card shows the label as the reason and no session hears a tap")
	return c
}

func cardReopenCmd(d cmd.Deps) *cobra.Command {
	var (
		f           sessionFlags
		card, reply string
	)
	c := &cobra.Command{
		Use:   "card-reopen",
		Short: "Reopen an answered card so it waits again: a button answer while its Undo shows, or one taken from the user's reply in its thread",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runOp(c, d, f, opCardReopen, map[string]any{"url": card, "via": reply})
		},
	}
	f.bind(c)
	c.Flags().StringVar(&card, "card", "", "the answered card's Slack permalink")
	c.Flags().StringVar(&reply, "reply", "", "permalink of the user's reply in the card's thread whose words the card shows as its answer; without it, the card's button answer is undone while its Undo shows")
	return c
}

func dmRelayCmd(d cmd.Deps) *cobra.Command {
	var (
		f                     sessionFlags
		text, card, channelID string
	)
	c := &cobra.Command{
		Use:   "dm-relay",
		Short: "Post words typed on a drive dashboard in your DM, marked as yours, and route them as your DM",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runOp(c, d, f, opDMRelay, map[string]any{"text": text, "url": card, "channel_id": channelID})
		},
	}
	f.bind(c)
	c.Flags().StringVar(&text, "text", "", "the words, as typed")
	c.Flags().StringVar(&card, "under", "", "permalink of the DM message to reply under; empty starts a new message")
	bindConversation(c, &channelID)
	return c
}

func bindConversation(c *cobra.Command, channelID *string) {
	c.Flags().StringVar(channelID, "channel", "", "a private channel `cc-slack channel-create --private` made, one of your conversations with the app, in place of your DM")
}

func bindAgent(c *cobra.Command, name *string) {
	c.Flags().StringVar(name, "agent", "", "agent writing the post, shown after your name as <display name> (<agent>): claude, codex, cursor (default agent)")
}

func putCard(body map[string]any, cwd, value string) error {
	if value == "" {
		return nil
	}
	raw := []byte(value)
	if path, ok := strings.CutPrefix(value, "@"); ok {
		var err error
		if raw, err = os.ReadFile(absPaths(cwdOr(cwd), []string{path})[0]); err != nil {
			return fmt.Errorf("read --card: %w", err)
		}
	}
	var card statusCard
	if err := json.Unmarshal(raw, &card); err != nil {
		return fmt.Errorf("--card takes a JSON status card: %w", err)
	}
	body["card"] = card
	return nil
}
