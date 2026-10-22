package main

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/daemon"
)

var slackFileID = regexp.MustCompile(`^F[A-Z0-9]+$`)

type fileResult struct {
	FileID   string `json:"file_id"`
	Name     string `json:"name"`
	Mimetype string `json:"mimetype"`
	Size     int    `json:"size"`
	Path     string `json:"path"`
}

func filesDir() string { return filepath.Join(appPaths().StateDir(), "files") }

func (a *app) handleFile(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if !slackFileID.MatchString(b.FileID) {
		return fail(fmt.Sprintf("file needs a Slack file id (F…) or a Slack file url, not %q", b.FileID))
	}
	if b.Out != "" && !filepath.IsAbs(b.Out) {
		return fail(fmt.Sprintf("file needs an absolute out path, not %q", b.Out))
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionFile, err))
	}
	api := a.newAPI(id.Token)
	f, _, _, err := api.GetFileInfoContext(hc.Ctx, b.FileID, 0, 0)
	if err != nil {
		a.poller.log.Printf("file %s: files.info: %v", b.FileID, err)
		return fail(fallback(actionFile, err))
	}
	path := cmp.Or(b.Out, filepath.Join(filesDir(), f.ID+filepath.Ext(f.Name)))
	if err := saveFile(hc.Ctx, api, f, path); err != nil {
		a.poller.log.Printf("file %s: download: %v", b.FileID, err)
		return fail(fallback(actionFile, err))
	}
	return ok(fileResult{FileID: f.ID, Name: f.Name, Mimetype: f.Mimetype, Size: f.Size, Path: path})
}

func saveFile(ctx context.Context, api slackAPI, f *slack.File, path string) error {
	if f.URLPrivateDownload == "" {
		return fmt.Errorf("%s has no download url", f.ID)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := api.GetFileContext(ctx, f.URLPrivateDownload, tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	info, err := tmp.Stat()
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if info.Size() != int64(f.Size) {
		return fmt.Errorf("slack served %d bytes for %s, which has %d, so the bot cannot read it", info.Size(), f.ID, f.Size)
	}
	return os.Rename(tmp.Name(), path)
}
