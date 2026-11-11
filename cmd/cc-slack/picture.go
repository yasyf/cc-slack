package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/slack-go/slack"
	"golang.org/x/image/webp"
)

const (
	picturePrefix = "cc-slack/pictures/"
	pictureLife   = 7 * 24 * time.Hour
)

type cardImage struct {
	ID     string `json:"id,omitempty"`
	Key    string `json:"key,omitempty"`
	Source string `json:"source,omitempty"`
	Title  string `json:"title"`
}

type loadedPicture struct {
	cardImage
	body []byte
}

func uploadPictures(ctx context.Context, id identity, paths []string, lines ...loadedPicture) ([]cardImage, error) {
	if !id.Config.Pictures.enabled() {
		return nil, errors.New("a card's pictures need a [pictures] bucket in the cc-slack config; run `cc-slack login` once it has one")
	}
	pictures := make([]loadedPicture, 0, len(paths)+len(lines))
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if body, err = drawablePicture(body, path); err != nil {
			return nil, err
		}
		pictures = append(pictures, loadedPicture{cardImage{Title: filepath.Base(path)}, body})
	}
	return id.Config.Pictures.upload(ctx, id.TeamID, append(pictures, lines...))
}

func loadPictures(ctx context.Context, lines []cardImage) ([]loadedPicture, error) {
	pictures := make([]loadedPicture, 0, len(lines))
	for _, line := range lines {
		body, err := fetchPicture(ctx, line.Source)
		if err != nil {
			return nil, fmt.Errorf("read the card's picture %s: %w", line.Source, err)
		}
		if !imageTypes[http.DetectContentType(body)] {
			return nil, fmt.Errorf("the card's picture %s is no PNG, JPEG, GIF, or WebP image", line.Source)
		}
		if body, err = drawablePicture(body, line.Source); err != nil {
			return nil, err
		}
		pictures = append(pictures, loadedPicture{line, body})
	}
	return pictures, nil
}

func fetchPicture(ctx context.Context, source string) ([]byte, error) {
	if filepath.IsAbs(source) {
		return os.ReadFile(source)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, http.NoBody)
	if err != nil {
		return nil, err
	}
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("its server answered %s", res.Status)
	}
	return io.ReadAll(res.Body)
}

func r2Client(endpoint, keyID, secret string, client *http.Client) *s3.Client {
	return s3.New(s3.Options{
		Region:                     "auto",
		BaseEndpoint:               aws.String(endpoint),
		Credentials:                credentials.NewStaticCredentialsProvider(keyID, secret, ""),
		HTTPClient:                 client,
		UsePathStyle:               true,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
}

func (p pictureConfig) client() *s3.Client {
	return r2Client(p.Endpoint, p.KeyID, p.Secret, &http.Client{Timeout: 30 * time.Second})
}

func (p pictureConfig) upload(ctx context.Context, teamID string, pictures []loadedPicture) ([]cardImage, error) {
	client := p.client()
	images := make([]cardImage, 0, len(pictures))
	for _, picture := range pictures {
		kind := http.DetectContentType(picture.body)
		sum := sha256.Sum256(picture.body)
		picture.Key = picturePrefix + teamID + "/" + hex.EncodeToString(sum[:]) + pictureExts[kind]
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(p.Bucket),
			Key:         aws.String(picture.Key),
			Body:        bytes.NewReader(picture.body),
			ContentType: aws.String(kind),
		}); err != nil {
			return nil, fmt.Errorf("upload picture %s: %w", cmp.Or(picture.Source, picture.Title), err)
		}
		images = append(images, picture.cardImage)
	}
	return images, nil
}

var pictureExts = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif"}

func drawablePicture(body []byte, name string) ([]byte, error) {
	if http.DetectContentType(body) != "image/webp" {
		return body, nil
	}
	picture, err := webp.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("decode WebP picture %s: %w", name, err)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, picture); err != nil {
		return nil, fmt.Errorf("encode %s as PNG: %w", name, err)
	}
	return out.Bytes(), nil
}

func (p pictureConfig) link(key string) string {
	signed, err := s3.NewPresignClient(p.client()).PresignGetObject(context.Background(),
		&s3.GetObjectInput{Bucket: aws.String(p.Bucket), Key: aws.String(key)}, s3.WithPresignExpires(pictureLife))
	if err != nil {
		panic(fmt.Sprintf("sign picture %s: %v", key, err))
	}
	return signed.URL
}

func (i cardImage) block(pictures pictureConfig) slack.Block {
	title := slack.NewTextBlockObject(slack.PlainTextType, i.Title, false, false)
	switch {
	case i.Key != "":
		return slack.NewImageBlock(pictures.link(i.Key), i.Title, "", title)
	case i.ID != "":
		return slack.NewImageBlockSlackFile(&slack.SlackFileObject{ID: i.ID}, i.Title, "", title)
	}
	return slack.NewImageBlock(i.Source, i.Title, "", title)
}
