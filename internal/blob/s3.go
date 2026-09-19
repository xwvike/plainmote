package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Config points the client at a bucket. R2 speaks the S3 API, which is the
// interface Cloudflare documents for object traffic - its own REST API is for
// management and carries a global rate limit that data would blow through.
type S3Config struct {
	Endpoint  string // e.g. https://<account>.r2.cloudflarestorage.com
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string // R2 ignores it but the signature needs one
}

func (c S3Config) validate() error {
	switch {
	case strings.TrimSpace(c.Endpoint) == "":
		return errors.New("blob endpoint must not be empty")
	case strings.TrimSpace(c.Bucket) == "":
		return errors.New("blob bucket must not be empty")
	case strings.TrimSpace(c.AccessKey) == "" || strings.TrimSpace(c.SecretKey) == "":
		return errors.New("blob credentials must not be empty")
	}
	return nil
}

// S3 stores objects in an S3-compatible bucket.
type S3 struct {
	client *s3.Client
	bucket string
}

func NewS3(cfg S3Config) (*S3, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	region := cfg.Region
	if region == "" {
		region = "auto"
	}
	client := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(strings.TrimRight(cfg.Endpoint, "/")),
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		// R2 buckets are addressed by path, not by a host prefix.
		UsePathStyle: true,
	})
	return &S3{client: client, bucket: cfg.Bucket}, nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("blob key must not be empty")
	}
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   r,
	}
	if size >= 0 {
		// Given a length the SDK can send a plain request instead of falling
		// back to chunked transfer, which some S3 implementations dislike.
		input.ContentLength = aws.Int64(size)
	}
	if _, err := s.client.PutObject(ctx, input); err != nil {
		return fmt.Errorf("write blob: %w", err)
	}
	return nil
}

func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	if strings.TrimSpace(key) == "" {
		return nil, 0, errors.New("blob key must not be empty")
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("open blob: %w", err)
	}
	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

func (s *S3) OpenRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, int64, error) {
	if strings.TrimSpace(key) == "" {
		return nil, 0, errors.New("blob key must not be empty")
	}
	if start < 0 || end < start {
		return nil, 0, errors.New("invalid blob byte range")
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", start, end)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("open blob range: %w", err)
	}
	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("blob key must not be empty")
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete blob: %w", err)
	}
	return nil
}

// isNotFound recognises a missing object across the shapes the API uses for
// it, so callers only ever see ErrNotFound.
func isNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var notFound *types.NotFound
	return errors.As(err, &notFound)
}
