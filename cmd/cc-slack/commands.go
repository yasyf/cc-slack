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
	tokenPlaceholder = "unset"
	captHookBinary   = "capt-hook"
	pacificLayout    = "2006-01-02 15:04"
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
			var in struct {
				SessionID string `json:"session_id"`
				Cwd       string `json:"cwd"`
			}
			raw, err := io.ReadAll(c.InOrStdin())
			if err != nil {
				return err
			}
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &in); err != nil {
					return fmt.Errorf("session-start hook input: %w", err)
				}
			}
			_, err = invoke(c.Context(), d, in.SessionID, cwdOr(in.Cwd), opSessionStart, nil)
			return err
		},
	}
}

func loginCmd(d cmd.Deps) *cobra.Command {
	var (
		f          sessionFlags
		email      string
		repo       string
		configFlag string
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
			root, err := repoRoot(ctx, cwdOr(f.cwd), repo)
			if err != nil {
				return err
			}
			captHook, err := captHookPath()
			if err != nil {
				return err
			}
			path, required, err := configPath(configFlag, root)
			if err != nil {
				return err
			}
			cfg, err := loadConfig(path, required)
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
	c.Flags().StringVar(&configFlag, "config", "", "cc-slack config file (defaults to $"+configEnv+", then the checkout's "+repoConfig+", then ~/.config/cc-slack/config.toml)")
	return c
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
	token        string
	socketURL    string
	avatarKeyID  string
	avatarSecret string
}

func readParameters(ctx context.Context, cfg config) (parameters, error) {
	if cfg.SSM == nil {
		return envParameters(cfg)
	}
	src := cfg.SSM
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(src.Region),
		awsconfig.WithCredentialsProvider(processcreds.NewProviderCommand(processcreds.NewCommandBuilderFunc(awsCLICredentials))))
	if err != nil {
		return parameters{}, fmt.Errorf("load AWS config: %w", err)
	}
	client := ssm.NewFromConfig(awsCfg)
	names := []string{src.BotToken, src.SocketURL}
	if cfg.Avatars.enabled() {
		names = append(names, src.AvatarKeyID, src.AvatarSecret)
	}
	values := make([]string, 4)
	for i, name := range names {
		if values[i], err = readParameter(ctx, client, name); err != nil {
			return parameters{}, err
		}
	}
	return parameters{token: values[0], socketURL: values[1], avatarKeyID: values[2], avatarSecret: values[3]}, nil
}

func envParameters(cfg config) (parameters, error) {
	p := parameters{
		token: os.Getenv(botEnv), socketURL: os.Getenv(socketURLEnv),
		avatarKeyID: os.Getenv(avatarKeyIDEnv), avatarSecret: os.Getenv(avatarSigningEnv),
	}
	if p.token == "" || p.socketURL == "" {
		return parameters{}, fmt.Errorf("login needs %s and %s, or an [ssm] section in the cc-slack config", botEnv, socketURLEnv)
	}
	if cfg.Avatars.enabled() && (p.avatarKeyID == "" || p.avatarSecret == "") {
		return parameters{}, fmt.Errorf("[avatars] needs %s and %s, or an [ssm] section in the cc-slack config", avatarKeyIDEnv, avatarSigningEnv)
	}
	return p, nil
}

func awsCLICredentials(ctx context.Context) (*exec.Cmd, error) {
	c := exec.CommandContext(ctx, "aws", "configure", "export-credentials", "--format", "process")
	c.Stderr = os.Stderr
	return c, nil
}

func readParameter(ctx context.Context, client *ssm.Client, name string) (string, error) {
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name), WithDecryption: aws.Bool(true)})
	if err != nil {
		return "", fmt.Errorf("read SSM parameter %s (sign in to AWS with a role that can read it): %w", name, err)
	}
	value := aws.ToString(out.Parameter.Value)
	if value == tokenPlaceholder {
		return "", fmt.Errorf("%s still holds its placeholder: it has not been written yet", name)
	}
	return value, nil
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
	c.Flags().StringVar(&f.channelID, "channel", "", "Slack channel id, or a user id for the bot's DM with that user")
	c.Flags().StringVar(&f.user, "user", "", "Slack user id for the bot's DM with that user, in place of --channel")
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
		spend                       ticketSpend
	)
	c := &cobra.Command{
		Use:   string(op),
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body := map[string]any{"channel_id": channelID, "url": url, "name": name, "users": users, "topic": topic}
			spend.fill(body)
			return runOp(c, d, f, op, body)
		},
	}
	f.bind(c)
	if action == approveCreate {
		c.Flags().StringVar(&name, "name", "", "name of the channel to create")
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
	c := &cobra.Command{
		Use:   "thread",
		Short: "Print a Slack thread's messages",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body := f.body()
			body["limit"] = limit
			return runOp(c, d, f.sessionFlags, opThread, body)
		},
	}
	f.bind(c)
	c.Flags().IntVar(&limit, "limit", defaultThreadLimit, "most recent messages to print")
	return c
}

func historyCmd(d cmd.Deps) *cobra.Command {
	var (
		f              sessionFlags
		channelID, url string
		limit          int
		asJSON         bool
	)
	c := &cobra.Command{
		Use:   "history",
		Short: "Print a Slack channel's most recent top-level messages, newest first",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body := map[string]any{"channel_id": channelID, "url": url, "limit": limit}
			return renderOp(c, d, f, opHistory, body, asJSON, renderHistory)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&channelID, "channel", "", "Slack channel id, or a user id for the bot's DM with that user")
	c.Flags().StringVar(&url, "url", "", "Slack channel or message permalink (fills channel)")
	c.Flags().IntVar(&limit, "limit", defaultHistoryLimit, "most recent top-level messages to print")
	c.Flags().BoolVar(&asJSON, "json", false, "print the daemon's JSON result")
	return c
}

func renderHistory(w io.Writer, res historyResult) {
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
		Short: "List the active Slack users whose handle, display name, real name, or email contains the query, ignoring case",
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

func unfollowCmd(d cmd.Deps) *cobra.Command {
	var f sessionFlags
	c := &cobra.Command{
		Use:   "unfollow <watch_id>",
		Short: "Stop a Slack watch",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			watchID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("watch id %q: %w", args[0], err)
			}
			return runOp(c, d, f, opUnfollow, map[string]any{"watch_id": watchID})
		},
	}
	f.bind(c)
	return c
}

func dmStatusCmd(d cmd.Deps) *cobra.Command {
	var (
		f         sessionFlags
		thread    string
		blocks    string
		card      string
		agentName string
		files     []string
	)
	c := &cobra.Command{
		Use:   "dm-status [text]",
		Short: "Post status to your DM with the app, in this session's thread",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			body := map[string]any{"thread_ts": thread, "files": absPaths(cwdOr(f.cwd), files), "agent": agentName}
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
	c.Flags().StringVar(&thread, "thread", "", "ts of a DM message to post under and bind this session's status thread to")
	bindFiles(c, &files)
	bindBlocks(c, &blocks)
	c.Flags().StringVar(&card, "card", "", "status card as a JSON object, or @path to a file holding one, in place of text")
	bindAgent(c, &agentName)
	return c
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
