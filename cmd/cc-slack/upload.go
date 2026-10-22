package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true}

const (
	readyTries    = 40
	readyWait     = 250 * time.Millisecond
	maxBlocks     = 50
	maxBlockChars = 3000
	maxFileBytes  = 1 << 30
)

type rawBlock struct {
	json.RawMessage
	kind slack.MessageBlockType
	id   string
}

func (b rawBlock) BlockType() slack.MessageBlockType { return b.kind }

func (b rawBlock) ID() string { return b.id }

func parseBlocks(raw []json.RawMessage) ([]slack.Block, error) {
	blocks := make([]slack.Block, 0, len(raw))
	for i, block := range raw {
		var head struct {
			Type    slack.MessageBlockType `json:"type"`
			BlockID string                 `json:"block_id"`
		}
		if err := json.Unmarshal(block, &head); err != nil || head.Type == "" {
			return nil, fmt.Errorf("block %d is not a Block Kit block: blocks takes a JSON array of objects that each name a type", i)
		}
		blocks = append(blocks, rawBlock{RawMessage: block, kind: head.Type, id: head.BlockID})
	}
	return blocks, nil
}

func checkAttachments(text string, blocks []slack.Block, paths []string, broadcast bool) error {
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("attachment %q is not an absolute path", path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("attachment %s: %w", path, err)
		}
		switch {
		case !info.Mode().IsRegular():
			return fmt.Errorf("attachment %s is not a regular file", path)
		case info.Size() == 0:
			return fmt.Errorf("attachment %s is empty", path)
		case info.Size() > maxFileBytes:
			return fmt.Errorf("attachment %s is %d bytes; Slack takes files up to 1 GB", path, info.Size())
		}
	}
	images, err := allImages(paths)
	if err != nil {
		return err
	}
	if broadcast && !images {
		return errors.New("slack shares a file that is not an image without broadcast; drop broadcast or attach images only")
	}
	if len(blocks) > 0 && !images {
		return errors.New("slack shares a file that is not an image with plain text only; drop blocks or attach images only")
	}
	if n := len(messageBlocks(text, blocks)) + len(paths); images && len(paths) > 0 && n > maxBlocks {
		return fmt.Errorf("the message and %d images need %d blocks; Slack takes at most %d", len(paths), n, maxBlocks)
	}
	if len(blocks) > maxBlocks {
		return fmt.Errorf("the message has %d blocks; Slack takes at most %d", len(blocks), maxBlocks)
	}
	return nil
}

func allImages(paths []string) (bool, error) {
	for _, path := range paths {
		kind, err := sniff(path)
		if err != nil {
			return false, fmt.Errorf("attachment %s: %w", path, err)
		}
		if !imageTypes[kind] {
			return false, nil
		}
	}
	return true, nil
}

func sniff(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	return http.DetectContentType(head[:n]), nil
}

func shareFiles(ctx context.Context, api slackAPI, id identity, channelID, threadTS, text string, paths []string) (string, string, error) {
	files, err := uploadFiles(ctx, api, paths)
	if err != nil {
		return "", "", err
	}
	share := fileShare{files: files, channelID: channelID, threadTS: threadTS, text: text, username: id.username(), iconURL: id.AvatarURL}
	if err := api.ShareFilesContext(ctx, share); err != nil {
		return "", "", err
	}
	ts, err := awaitShared(ctx, api, files[0], channelID, threadTS)
	if err != nil {
		return "", "", sharedError{files[0].Title, channelID, err.Error()}
	}
	return channelID, ts, nil
}

type sharedError struct {
	title, channelID, cause string
}

func (e sharedError) Error() string {
	return fmt.Sprintf("slack shared %s into %s but cc-slack could not read the message ts (%s), so the post may be up: read the thread before sending it again", e.title, e.channelID, e.cause)
}

func awaitShared(ctx context.Context, api slackAPI, want slack.FileSummary, channelID, threadTS string) (string, error) {
	for attempt := 1; ; attempt++ {
		file, _, _, err := api.GetFileInfoContext(ctx, want.ID, 0, 0)
		if err != nil {
			return "", err
		}
		for _, share := range slices.Concat(file.Shares.Public[channelID], file.Shares.Private[channelID]) {
			if share.ThreadTs == threadTS || threadTS == "" && share.ThreadTs == share.Ts {
				return share.Ts, nil
			}
		}
		if attempt == readyTries {
			return "", fmt.Errorf("no share of %s (%s) in that thread after %d tries", want.Title, want.ID, readyTries)
		}
		if err := pause(ctx); err != nil {
			return "", err
		}
	}
}

func uploadFiles(ctx context.Context, api slackAPI, paths []string) ([]slack.FileSummary, error) {
	files := make([]slack.FileSummary, 0, len(paths))
	for _, path := range paths {
		file, err := uploadFile(ctx, api, path)
		if err != nil {
			return nil, fmt.Errorf("upload %s: %w", path, err)
		}
		files = append(files, file)
	}
	return files, nil
}

func postImages(ctx context.Context, api slackAPI, id identity, channelID, threadTS, text string, blocks []slack.Block, paths []string, extra ...slack.MsgOption) (string, string, error) {
	files, err := uploadFiles(ctx, api, paths)
	if err != nil {
		return "", "", err
	}
	if _, err := api.CompleteUploadExternalContext(ctx, slack.CompleteUploadExternalParameters{Files: files}); err != nil {
		return "", "", err
	}
	opts := append(id.postOptions(fallbackText(text, files), threadTS, extra...), slack.MsgOptionBlocks(imageMessage(text, blocks, files)...))
	for attempt := 1; ; attempt++ {
		if err := awaitProcessed(ctx, api, files); err != nil {
			return "", "", err
		}
		ch, ts, err := api.PostMessageContext(ctx, channelID, opts...)
		if err == nil || errorCode(err) != "invalid_blocks" || attempt == readyTries {
			return ch, ts, err
		}
		if err := pause(ctx); err != nil {
			return "", "", err
		}
	}
}

func uploadFile(ctx context.Context, api slackAPI, path string) (slack.FileSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return slack.FileSummary{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return slack.FileSummary{}, err
	}
	name := filepath.Base(path)
	target, err := api.GetUploadURLExternalContext(ctx, slack.GetUploadURLExternalParameters{FileName: name, FileSize: int(info.Size())})
	if err != nil {
		return slack.FileSummary{}, err
	}
	if err := api.UploadToURL(ctx, slack.UploadToURLParameters{UploadURL: target.UploadURL, Reader: f, Filename: name}); err != nil {
		return slack.FileSummary{}, err
	}
	return slack.FileSummary{ID: target.FileID, Title: name}, nil
}

func awaitProcessed(ctx context.Context, api slackAPI, files []slack.FileSummary) error {
	for _, want := range files {
		for attempt := 1; ; attempt++ {
			file, _, _, err := api.GetFileInfoContext(ctx, want.ID, 0, 0)
			if err != nil {
				return err
			}
			if file.OriginalW > 0 {
				break
			}
			if attempt == readyTries {
				return fmt.Errorf("slack never finished processing %s (%s)", want.Title, want.ID)
			}
			if err := pause(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(readyWait):
		return nil
	}
}

func imageMessage(text string, body []slack.Block, files []slack.FileSummary) []slack.Block {
	blocks := messageBlocks(text, body)
	for _, file := range files {
		blocks = append(blocks, slack.NewImageBlockSlackFile(&slack.SlackFileObject{ID: file.ID}, file.Title, "",
			slack.NewTextBlockObject(slack.PlainTextType, file.Title, false, false)))
	}
	return blocks
}

func messageBlocks(text string, blocks []slack.Block) []slack.Block {
	if len(blocks) > 0 {
		return slices.Clone(blocks)
	}
	return textBlocks(text)
}

func textBlocks(text string) []slack.Block {
	var blocks []slack.Block
	for rest := []rune(text); len(rest) > 0; {
		n := min(len(rest), maxBlockChars)
		for i := n - 1; n < len(rest) && i > 0; i-- {
			if rest[i] == '\n' {
				n = i + 1
				break
			}
		}
		blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, string(rest[:n]), false, false), nil, nil))
		rest = rest[n:]
	}
	return blocks
}

func fallbackText(text string, files []slack.FileSummary) string {
	if text != "" {
		return text
	}
	names := make([]string, 0, len(files))
	for _, file := range files {
		names = append(names, file.Title)
	}
	return strings.Join(names, ", ")
}
