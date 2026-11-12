package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

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

func filePath(f *slack.File) string { return filepath.Join(filesDir(), f.ID+filepath.Ext(f.Name)) }

func attachedFile(ctx context.Context, api slackAPI, f *slack.File) messageFile {
	out := messageFile{ID: f.ID, Name: f.Name, Mimetype: f.Mimetype, Permalink: f.Permalink}
	if !strings.HasPrefix(f.Mimetype, "image/") {
		return out
	}
	path := filePath(f)
	if info, err := os.Stat(path); err == nil && info.Size() == int64(f.Size) {
		out.Path = path
		return out
	}
	if _, err := saveFile(ctx, api, f, path); err != nil {
		out.PathError = fallback(actionFile, err)
		return out
	}
	out.Path = path
	return out
}

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
	path := cmp.Or(b.Out, filePath(f))
	size, err := saveFile(hc.Ctx, api, f, path)
	if err != nil {
		a.poller.log.Printf("file %s: download: %v", b.FileID, err)
		return fail(fallback(actionFile, err))
	}
	return ok(fileResult{FileID: f.ID, Name: f.Name, Mimetype: f.Mimetype, Size: size, Path: path})
}

func saveFile(ctx context.Context, api slackAPI, f *slack.File, path string) (int, error) {
	if f.URLPrivateDownload == "" {
		return 0, fmt.Errorf("%s has no download url", f.ID)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := api.GetFileContext(ctx, f.URLPrivateDownload, tmp); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	info, err := tmp.Stat()
	if err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	return int(info.Size()), os.Rename(tmp.Name(), path)
}

// GetFileContext downloads the file itself because slack-go follows Slack's redirect of a download the bot cannot read to the workspace's sign-in page and saves that page as the file.
func (c *apiClient) GetFileContext(ctx context.Context, downloadURL string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req) //nolint:gosec // G704: the request goes to the Slack download url files.info names
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Request.URL.Host != req.URL.Host {
		return fmt.Errorf("slack redirected the download to %s, so the bot cannot read it", resp.Request.URL.Host)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		retry, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return &slack.RateLimitedError{RetryAfter: time.Duration(retry) * time.Second}
	}
	if resp.StatusCode != http.StatusOK {
		return slack.StatusCodeError{Code: resp.StatusCode, Status: resp.Status}
	}
	_, err = io.Copy(w, resp.Body)
	return err
}
