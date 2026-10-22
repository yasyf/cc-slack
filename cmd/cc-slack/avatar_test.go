package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

var (
	red  = color.RGBA{R: 255, A: 255}
	blue = color.RGBA{B: 255, A: 255}
)

func solid(size int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			img.Set(x, y, c)
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestComposeAvatarBadgesTheBottomRightCorner(t *testing.T) {
	t.Parallel()
	out := composeAvatar(solid(192, red), solid(512, blue))
	if b := out.Bounds(); b.Dx() != avatarSize || b.Dy() != avatarSize {
		t.Fatalf("avatar bounds = %v, want %dx%d", b, avatarSize, avatarSize)
	}
	for _, tc := range []struct {
		name string
		x, y int
		want color.RGBA
	}{
		{"photo", 20, 20, red},
		{"corner outside the badge", avatarSize - 1, avatarSize - 1, red},
		{"ring", badgeCenterX, badgeCenterX - badgeRadius - badgeRing/2, color.RGBA{R: 255, G: 255, B: 255, A: 255}},
		{"badge", badgeCenterX, badgeCenterX, blue},
	} {
		if got := out.RGBAAt(tc.x, tc.y); got != tc.want {
			t.Errorf("%s pixel (%d,%d) = %v, want %v", tc.name, tc.x, tc.y, got, tc.want)
		}
	}
}

func TestAvatarStoreUploadsTheBadgedPhotoUnderItsHash(t *testing.T) {
	t.Parallel()
	photo := encodePNG(t, solid(512, red))
	var put struct {
		path, contentType, auth string
		body                    []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write(photo)
		case http.MethodPut:
			put.path, put.contentType, put.auth = r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization")
			put.body, _ = io.ReadAll(r.Body)
		}
	}))
	t.Cleanup(srv.Close)
	store := newAvatarStore(srv.Client())
	id := identity{
		TeamID: "T1", UserID: "U1", Image512: srv.URL + "/photo.png", AvatarKeyID: "r2-key", AvatarSecret: "r2-secret",
		Config: config{Avatars: avatarConfig{Endpoint: srv.URL, Bucket: "avatars", PublicBase: "https://public.test/"}},
	}

	got, err := store.publish(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := strings.CutPrefix(put.path, "/avatars/")
	if !ok || !strings.HasPrefix(key, avatarPrefix+"T1/U1/") || !strings.HasSuffix(key, avatarStyle+".png") || got != "https://public.test/"+key {
		t.Fatalf("published %q after a PUT to %q, want the public url of a key under %sT1/U1/", got, put.path, avatarPrefix)
	}
	if put.contentType != "image/png" || !strings.Contains(put.auth, "Credential=r2-key/") {
		t.Fatalf("PUT content-type %q, authorization %q, want image/png signed with r2-key", put.contentType, put.auth)
	}
	if img, err := png.Decode(bytes.NewReader(put.body)); err != nil || img.Bounds().Dx() != avatarSize {
		t.Fatalf("uploaded body decodes to %v (%v), want a %dpx png", img, err, avatarSize)
	}
	if again, err := store.publish(context.Background(), id); err != nil || again != got {
		t.Fatalf("second publish = %q (%v), want the same content-hashed url %q", again, err, got)
	}
}

func TestAFailedAvatarRebuildKeepsTheOldAvatarAndRetriesNextRead(t *testing.T) {
	f := newFixture(t)
	failing := true
	f.app.avatar = func(ctx context.Context, id identity) (string, error) {
		if failing {
			return "", errors.New("r2 is down")
		}
		return agentAvatar(ctx, id)
	}
	f.slack.setUser(&slack.User{ID: testUser, Name: "ym", Profile: slack.UserProfile{DisplayName: "yasyf", Image512: "https://avatars/new.png"}})
	post := func(text string) fakePost {
		t.Helper()
		id, err := loadIdentity(f.ctx, f.db())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.app.post(f.ctx, f.db(), id, testChannel, "", text, nil, nil); err != nil {
			t.Fatal(err)
		}
		sent := f.slack.sent()
		return sent[len(sent)-1]
	}

	f.now = f.now.Add(profileTTL)
	if got := post("r2 down"); got.iconURL != "https://avatars/ym.png?agent-badge" {
		t.Fatalf("post icon_url = %q while the rebuild fails, want the old agent avatar", got.iconURL)
	}
	failing = false
	f.now = f.now.Add(profileTTL)
	if got := post("r2 back"); got.iconURL != "https://avatars/new.png?agent-badge" {
		t.Fatalf("post icon_url = %q on the next profile read, want the rebuilt avatar", got.iconURL)
	}
}

func TestAvatarStoreWithoutABucketPostsTheProfilePhoto(t *testing.T) {
	t.Parallel()
	id := identity{TeamID: "T1", UserID: "U1", Image512: "https://avatars/ym.png"}
	got, err := newAvatarStore(nil).publish(context.Background(), id)
	if err != nil || got != id.Image512 {
		t.Fatalf("publish without [avatars] = %q (%v), want the Slack profile photo %q", got, err, id.Image512)
	}
}

func TestLoginRefusesWithoutAnAgentAvatar(t *testing.T) {
	l := startDaemon(t)
	l.app.avatar = func(context.Context, identity) (string, error) { return "", errors.New("r2 is down") }
	l.slack.setUser(&slack.User{ID: testUser, Name: "ym", Profile: slack.UserProfile{Email: "ym@example.com", Image512: "https://avatars/ym.png"}})
	l.refused(opLogin, map[string]string{
		"token": "xoxb-test", "email": "ym@example.com", "socket_url": "ws://127.0.0.1:1/live", "repo_root": "/repo",
		"capt_hook_path": testCaptHook, "avatar_key_id": "r2-key", "avatar_secret": "r2-secret",
	}, "agent avatar: r2 is down")
	l.refused(opWhoami, map[string]string{}, "no Slack identity")
	l.refused(opWhoami, map[string]string{}, "cc-slack "+appVersion+" read no identity from "+statePath())
}
