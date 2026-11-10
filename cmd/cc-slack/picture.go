package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image/png"
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
	ID    string `json:"id,omitempty"`
	Key   string `json:"key,omitempty"`
	Title string `json:"title"`
}

func uploadPictures(ctx context.Context, id identity, paths []string) ([]cardImage, error) {
	if !id.Config.Pictures.enabled() {
		return nil, errors.New("a card's pictures need a [pictures] bucket in the cc-slack config; run `cc-slack login` once it has one")
	}
	return id.Config.Pictures.upload(ctx, id.TeamID, paths)
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

func (p pictureConfig) upload(ctx context.Context, teamID string, paths []string) ([]cardImage, error) {
	client := p.client()
	images := make([]cardImage, 0, len(paths))
	for _, path := range paths {
		body, err := drawablePicture(path)
		if err != nil {
			return nil, err
		}
		kind := http.DetectContentType(body)
		sum := sha256.Sum256(body)
		key := picturePrefix + teamID + "/" + hex.EncodeToString(sum[:]) + pictureExts[kind]
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(p.Bucket),
			Key:         aws.String(key),
			Body:        bytes.NewReader(body),
			ContentType: aws.String(kind),
		}); err != nil {
			return nil, fmt.Errorf("upload picture %s: %w", path, err)
		}
		images = append(images, cardImage{Key: key, Title: filepath.Base(path)})
	}
	return images, nil
}

var pictureExts = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif"}

func drawablePicture(path string) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil || http.DetectContentType(body) != "image/webp" {
		return body, err
	}
	picture, err := webp.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("decode WebP picture %s: %w", path, err)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, picture); err != nil {
		return nil, fmt.Errorf("encode %s as PNG: %w", path, err)
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
	if i.Key != "" {
		return slack.NewImageBlock(pictures.link(i.Key), i.Title, "", title)
	}
	return slack.NewImageBlockSlackFile(&slack.SlackFileObject{ID: i.ID}, i.Title, "", title)
}
