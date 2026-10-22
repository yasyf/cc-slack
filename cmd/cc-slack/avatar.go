package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"math"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	xdraw "golang.org/x/image/draw"
)

const (
	avatarPrefix = "cc-slack/avatars/"
	avatarStyle  = "-badge"

	avatarSize   = 512
	badgeRadius  = 84
	badgeRing    = 10
	badgeInset   = 4
	badgeOuter   = badgeRadius + badgeRing
	badgeCenterX = avatarSize - badgeInset - badgeOuter
)

//go:embed agent-badge.png
var agentBadgePNG []byte

type avatarPublisher func(ctx context.Context, id identity) (string, error)

type avatarStore struct {
	http *http.Client
}

func newAvatarStore(client *http.Client) *avatarStore {
	return &avatarStore{http: client}
}

func (s *avatarStore) publish(ctx context.Context, id identity) (string, error) {
	bucket := id.Config.Avatars
	if !bucket.enabled() {
		return id.Image512, nil
	}
	photo, err := s.fetchPhoto(ctx, id.Image512)
	if err != nil {
		return "", err
	}
	badge, _, err := image.Decode(bytes.NewReader(agentBadgePNG))
	if err != nil {
		return "", fmt.Errorf("decode agent badge: %w", err)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, composeAvatar(photo, badge)); err != nil {
		return "", fmt.Errorf("encode avatar: %w", err)
	}
	sum := sha256.Sum256(out.Bytes())
	key := fmt.Sprintf("%s%s/%s/%s%s.png", avatarPrefix, id.TeamID, id.UserID, hex.EncodeToString(sum[:8]), avatarStyle)
	client := s3.New(s3.Options{
		Region:                     "auto",
		BaseEndpoint:               aws.String(bucket.Endpoint),
		Credentials:                credentials.NewStaticCredentialsProvider(id.AvatarKeyID, id.AvatarSecret, ""),
		HTTPClient:                 s.http,
		UsePathStyle:               true,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(bucket.Bucket),
		Key:          aws.String(key),
		Body:         bytes.NewReader(out.Bytes()),
		ContentType:  aws.String("image/png"),
		CacheControl: aws.String("public, max-age=31536000, immutable"),
	}); err != nil {
		return "", fmt.Errorf("upload avatar %s: %w", key, err)
	}
	return bucket.PublicBase + key, nil
}

func (s *avatarStore) fetchPhoto(ctx context.Context, photoURL string) (image.Image, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, photoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("profile photo %s: %w", photoURL, err)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch profile photo %s: %w", photoURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch profile photo %s: HTTP %d", photoURL, resp.StatusCode)
	}
	photo, _, err := image.Decode(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("decode profile photo %s: %w", photoURL, err)
	}
	return photo, nil
}

func composeAvatar(photo, badge image.Image) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, avatarSize, avatarSize))
	xdraw.CatmullRom.Scale(out, out.Bounds(), photo, photo.Bounds(), draw.Src, nil)
	ring := image.Rect(badgeCenterX-badgeOuter, badgeCenterX-badgeOuter, badgeCenterX+badgeOuter, badgeCenterX+badgeOuter)
	draw.DrawMask(out, ring, image.White, image.Point{}, disc(badgeOuter), image.Point{}, draw.Over)
	scaled := image.NewRGBA(image.Rect(0, 0, 2*badgeRadius, 2*badgeRadius))
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), badge, badge.Bounds(), draw.Src, nil)
	draw.DrawMask(out, ring.Inset(badgeRing), scaled, image.Point{}, disc(badgeRadius), image.Point{}, draw.Over)
	return out
}

type disc int

func (d disc) ColorModel() color.Model { return color.AlphaModel }

func (d disc) Bounds() image.Rectangle { return image.Rect(0, 0, 2*int(d), 2*int(d)) }

func (d disc) At(x, y int) color.Color {
	r := float64(d)
	dist := math.Hypot(float64(x)+0.5-r, float64(y)+0.5-r)
	return color.Alpha{A: uint8(255 * math.Max(0, math.Min(1, r-dist+0.5)))}
}
