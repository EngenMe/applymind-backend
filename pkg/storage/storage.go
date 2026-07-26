// Package storage wraps the S3 operations ApplyMind needs: storing uploaded
// files and handing out short-lived presigned download URLs.
//
// NOTE: pkg/storage/storage.go already exists in the tree. If it is currently an
// empty stub, replace it with this file; if it already has content, merge the
// Client interface and the three methods below into it.
package storage

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Client is the storage behaviour the rest of the application depends on.
type Client interface {
	Upload(ctx context.Context, key string, body []byte, contentType string) error
	PresignedDownloadURL(ctx context.Context, key, downloadFilename string, ttl time.Duration) (string, time.Time, error)
	Delete(ctx context.Context, key string) error
}

// S3Client is the AWS implementation of Client.
type S3Client struct {
	api     *s3.Client
	presign *s3.PresignClient
	bucket  string
	now     func() time.Time
}

var _ Client = (*S3Client)(nil)

// NewS3Client builds a client from the ambient AWS config (IAM role on Lambda,
// profile/env locally).
func NewS3Client(ctx context.Context, bucket string) (*S3Client, error) {
	if bucket == "" {
		return nil, fmt.Errorf("storage: bucket name is required")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage: load aws config: %w", err)
	}
	api := s3.NewFromConfig(cfg)
	return &S3Client{
		api:     api,
		presign: s3.NewPresignClient(api),
		bucket:  bucket,
		now:     time.Now,
	}, nil
}

// Upload stores an object, overwriting any object already at key.
func (c *S3Client) Upload(ctx context.Context, key string, body []byte, contentType string) error {
	if key == "" {
		return fmt.Errorf("storage: key is required")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := c.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		return fmt.Errorf("storage: put object %q: %w", key, err)
	}
	return nil
}

// PresignedDownloadURL returns a temporary URL for fetching an object. When
// downloadFilename is set the browser is told to save the file under the
// original name rather than the S3 key.
func (c *S3Client) PresignedDownloadURL(ctx context.Context, key, downloadFilename string, ttl time.Duration) (string, time.Time, error) {
	if key == "" {
		return "", time.Time{}, fmt.Errorf("storage: key is required")
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}

	in := &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}
	if downloadFilename != "" {
		in.ResponseContentDisposition = aws.String(
			fmt.Sprintf("attachment; filename=%q", downloadFilename),
		)
	}

	req, err := c.presign.PresignGetObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("storage: presign get object %q: %w", key, err)
	}
	return req.URL, c.now().Add(ttl), nil
}

// Delete removes an object. Deleting a key that does not exist is not an error.
func (c *S3Client) Delete(ctx context.Context, key string) error {
	if key == "" {
		return fmt.Errorf("storage: key is required")
	}
	_, err := c.api.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("storage: delete object %q: %w", key, err)
	}
	return nil
}
