package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/slack-go/slack"
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

func sharedImages(files []slack.FileSummary) []cardImage {
	images := make([]cardImage, len(files))
	for i, f := range files {
		images[i] = cardImage{ID: f.ID, Title: f.Title}
	}
	return images
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
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		key := picturePrefix + teamID + "/" + hex.EncodeToString(sum[:]) + filepath.Ext(path)
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(p.Bucket),
			Key:         aws.String(key),
			Body:        bytes.NewReader(body),
			ContentType: aws.String(http.DetectContentType(body)),
		}); err != nil {
			return nil, fmt.Errorf("upload picture %s: %w", path, err)
		}
		images = append(images, cardImage{Key: key, Title: filepath.Base(path)})
	}
	return images, nil
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
