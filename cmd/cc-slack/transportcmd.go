package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"

	"github.com/spf13/cobra"
)

func transportCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "transport",
		Short: "Write to Slack as a program that is not a Claude session, through the daemon's transport socket",
		Long: `Each verb sends one request to the daemon's transport socket and prints its
JSON answer. It passes through no session, ticket, hook, voice or card
placement. --dry-run writes nothing and prints the target, the sender and the
final text and blocks.`,
	}
	for _, op := range []string{"send", "reply", "edit", "delete", "react", "status"} {
		c.AddCommand(transportOpCmd(op))
	}
	return c
}

func transportOpCmd(op string) *cobra.Command {
	var (
		req    transportRequest
		user   string
		blocks string
	)
	c := &cobra.Command{
		Use:  op,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			req.Op = op
			if user != "" {
				req.Channel = user
			}
			if blocks != "" {
				if err := json.Unmarshal([]byte(blocks), &req.Blocks); err != nil {
					return fmt.Errorf("--blocks takes a JSON array of Block Kit blocks: %w", err)
				}
			}
			reply, err := callTransport(c.Context(), req)
			if err != nil {
				return err
			}
			if !reply.OK {
				return errors.New(reply.Error)
			}
			return json.NewEncoder(c.OutOrStdout()).Encode(reply)
		},
	}
	f := c.Flags()
	f.StringVar(&req.URL, "url", "", "the Slack message url to reply under, edit, delete, react to or set the status of")
	f.StringVar(&req.Channel, "channel", "", "the conversation id")
	f.StringVar(&user, "user", "", "a person's id: posts in the app's DM with them")
	f.StringVar(&req.ThreadTS, "thread", "", "the thread's root ts")
	f.StringVar(&req.TS, "ts", "", "the message's ts")
	f.StringVar(&req.Text, "text", "", "the message's text, shown in notifications")
	f.StringVar(&blocks, "blocks", "", "Block Kit blocks as a JSON array, posted as given; an image block whose image_url is an absolute path is uploaded to the [pictures] bucket")
	f.StringVar(&req.Sender, "sender", "", "the display name the post goes out under")
	f.StringVar(&req.Key, "request-key", "", "a key that makes a repeated send answer the first post instead of posting again")
	f.StringVar(&req.Reaction, "name", "", "the emoji name for react")
	f.BoolVar(&req.Remove, "remove", false, "remove the reaction instead of adding it")
	f.StringVar(&req.Status, "status", "", "the loader line's words for status")
	f.BoolVar(&req.DryRun, "dry-run", false, "write nothing; print what would be sent")
	return c
}

func callTransport(ctx context.Context, req transportRequest) (transportReply, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", transportSocket())
	if err != nil {
		return transportReply{}, fmt.Errorf("reach the daemon's transport socket: %w", err)
	}
	defer func() { _ = conn.Close() }()
	line, err := json.Marshal(req)
	if err != nil {
		return transportReply{}, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return transportReply{}, err
	}
	answer, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return transportReply{}, fmt.Errorf("read the transport's answer: %w", err)
	}
	var reply transportReply
	if err := json.Unmarshal(answer, &reply); err != nil {
		return transportReply{}, err
	}
	return reply, nil
}
